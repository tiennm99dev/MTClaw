package cli

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	yaml "github.com/goccy/go-yaml"
	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/config"
	"github.com/tiennm99/MTClaw/internal/provider/openai"
	"github.com/tiennm99/MTClaw/internal/tools"
)

//go:embed prompts/AGENTS.md
var starterAgentsMD string

// onboardVerifyTimeout bounds every network call onboard makes to verify a
// value the user just entered (the OpenAI key/model, the Telegram token).
const onboardVerifyTimeout = 15 * time.Second

// telegramCaptureWindow is how long onboard's Telegram ID capture step
// waits for a message, per phase-09-hardening-and-release.md step 5.
const telegramCaptureWindow = 60 * time.Second

// newOnboardCmd builds `mtclaw onboard`: interactive first-run setup that
// writes a working, valid config without the user reading source. It is
// exempt from the normal config load (see skipsConfigLoad in root.go)
// because its entire purpose is to run on a machine that has no config yet.
func newOnboardCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "onboard",
		Short: "Interactive first-run setup: writes a working config and runs doctor",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := newStdioPrompter(cmd.InOrStdin(), cmd.OutOrStdout(), int(os.Stdin.Fd()))
			return runOnboard(cmd.Context(), cmd.OutOrStdout(), s.configPath, p, realTelegramCapturer{})
		},
	}
}

// runOnboard is onboard's testable core: p supplies every interactive
// answer and capturer stands in for a live Telegram long-poll, so
// onboard_test.go can drive the entire sequence - including the
// refuse-to-clobber path and the Telegram ID capture - with fakes, never a
// TTY or a real network call.
func runOnboard(ctx context.Context, out io.Writer, configPath string, p prompter, capturer telegramCapturer) error {
	if _, err := os.Stat(configPath); err == nil {
		return showWouldNotOverwrite(configPath, out)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config file %s: %w", configPath, err)
	}

	cfg := config.Default()

	// Step 1: OS-appropriate shell/deny-list. No prompt - these are the
	// starting-point choices onboard makes for the user, not a question.
	if runtime.GOOS == "windows" {
		cfg.Tools.Exec.Deny = append([]string(nil), tools.DefaultDenyWindows...)
	} else {
		cfg.Tools.Exec.Deny = append([]string(nil), tools.DefaultDenyPOSIX...)
	}
	// exec.mode is always "approval" and never offered as a choice here:
	// "auto" is a deliberate, documented config edit, not an onboarding
	// option (phase 5's Security Model, restated in docs/security.md).
	cfg.Tools.Exec.Mode = "approval"

	// Steps 2+3: OpenAI key and model, verified together by listing models
	// (a successful list call already proves the key works and is
	// reachable, so onboard does not additionally call Probe).
	if err := onboardOpenAI(ctx, p, cfg); err != nil {
		return err
	}

	// Steps 4+5: Telegram token, getMe, and the interactive ID capture.
	if err := onboardTelegram(ctx, p, capturer, cfg); err != nil {
		return err
	}
	// config.Validate refuses to load telegram.enabled: true with an empty
	// allowlist (fail-closed by design), and the capture step can
	// legitimately end with nothing confirmed (no sender arrived, multiple
	// senders forced a manual choice the user declined to make, ...).
	// Writing that combination would hand back a config onboard itself
	// cannot reload, so disable the channel here rather than ship an
	// invalid file - the user re-enables it once they have a real id.
	if cfg.Channels.Telegram.Enabled && !telegramHasAnyAllowlist(cfg) {
		p.Printf("\nno Telegram user id was confirmed, so channels.telegram.enabled is being written as false. Add your id to channels.telegram.allow_from and set enabled: true once you have it (get the id with /whoami), or re-run `mtclaw onboard`.\n")
		cfg.Channels.Telegram.Enabled = false
	}

	// Step 6: workspace directory.
	workspaceAnswer, err := p.Text("Workspace directory (the agent's filesystem root)", cfg.Agent.Workspace)
	if err != nil {
		return err
	}
	workspacePath, err := config.ExpandPath(workspaceAnswer, filepath.Dir(configPath))
	if err != nil {
		return fmt.Errorf("expand workspace path: %w", err)
	}
	if err := os.MkdirAll(workspacePath, 0o755); err != nil {
		return fmt.Errorf("create workspace %s: %w", workspacePath, err)
	}
	cfg.Agent.Workspace = workspaceAnswer
	cfg.Tools.Filesystem.Roots = []string{workspaceAnswer}
	cfg.Tools.Exec.CWD = workspaceAnswer

	// Step 8: starter AGENTS.md, written before the config so the config
	// this run writes already references a file that exists.
	agentsPath, err := writeStarterAgentsFile()
	if err != nil {
		return err
	}
	cfg.Agent.SystemPromptFiles = []string{agentsPath}

	// Step 7: write the config. 0600 on POSIX; a documented best-effort
	// no-op on Windows (see internal/config/load.go's warnIfWorldReadable
	// for the same call made about *_file secrets).
	if err := writeOnboardConfig(configPath, cfg); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nWrote %s\n", configPath)

	// Step 9: run doctor and print next steps.
	fmt.Fprintln(out, "\n== doctor ==")
	rows := runDoctor(ctx, configPath)
	if err := printDoctorReport(out, rows, false); err != nil {
		return err
	}
	// A FAIL row here is not onboard failing: it is normal for a freshly
	// exported env var not to be visible yet in the *current* shell
	// session, and the whole point of ending on doctor is to hand the user
	// that actionable list, not to make onboard itself exit non-zero over
	// something only a shell restart (or `mtclaw doctor` run again later)
	// can resolve.
	printOnboardNextSteps(out, cfg)
	return nil
}

// showWouldNotOverwrite is onboard's refuse-to-clobber path: it never
// overwrites an existing config, and instead prints the existing config
// (redacted) next to what a fresh onboard run would write by default, so
// the user can see what would change without either file being touched.
func showWouldNotOverwrite(configPath string, out io.Writer) error {
	fmt.Fprintf(out, "A config file already exists at %s; onboard refuses to overwrite it.\n\n", configPath)

	fmt.Fprintln(out, "=== existing config (secrets redacted) ===")
	if existing, err := config.LoadFile(configPath); err == nil {
		redacted, err := config.MarshalRedacted(existing)
		if err != nil {
			return fmt.Errorf("render existing config: %w", err)
		}
		if _, err := out.Write(redacted); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(out, "(could not load it to render redacted: %v)\n", err)
	}

	fmt.Fprintln(out, "\n=== what a fresh `onboard` run would write (built-in defaults; your answers would fill in the rest) ===")
	fresh, err := yaml.Marshal(config.Default())
	if err != nil {
		return fmt.Errorf("render default config: %w", err)
	}
	if _, err := out.Write(fresh); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nDelete or rename the existing file (or pass --config pointing elsewhere) to onboard a fresh one.")
	return nil
}

// onboardOpenAI prompts for the OpenAI key's env var name and (optionally)
// its value, prints the export/setx line the user must add themselves, and
// - only ever using the value in memory - verifies both the key and the
// chosen model in one call to ListModels. The key is never written to cfg;
// only apiKeyEnv is.
func onboardOpenAI(ctx context.Context, p prompter, cfg *config.Config) error {
	p.Printf("\n== OpenAI ==\n")

	envName, err := p.Text("Environment variable holding your OpenAI API key", cfg.OpenAI.APIKeyEnv)
	if err != nil {
		return err
	}
	cfg.OpenAI.APIKeyEnv = envName

	typed, err := p.Secret("Paste your OpenAI API key to verify it now (never written to the config; leave blank to skip verification)")
	if err != nil {
		return err
	}

	p.Printf("Add this to your shell profile (mtclaw reads it at startup; never commit it):\n  %s\n", exportLine(envName))

	key := typed
	if key == "" {
		key = os.Getenv(envName)
	}

	var model string
	for model == "" {
		model, err = p.Text("Model (e.g. gpt-4o, gpt-4o-mini); there is no built-in default", "")
		if err != nil {
			return err
		}
		if model == "" {
			p.Printf("a model is required.\n")
		}
	}
	cfg.Agent.Model = model

	if key == "" {
		p.Printf("no API key available in the prompt or in %s; skipping verification - run `mtclaw doctor` once it is set.\n", envName)
		return nil
	}

	client, err := openai.NewWithAPIKey(key, cfg.OpenAI.BaseURL, onboardVerifyTimeout)
	if err != nil {
		p.Printf("could not build a verification client: %v\n", err)
		return nil
	}
	vctx, cancel := context.WithTimeout(ctx, onboardVerifyTimeout)
	defer cancel()
	models, err := client.ListModels(vctx)
	if err != nil {
		p.Printf("could not verify the key/model against %s: %v - double check the key and try `mtclaw doctor` later.\n", cfg.OpenAI.BaseURL, err)
		return nil
	}
	found := false
	for _, id := range models {
		if id == model {
			found = true
			break
		}
	}
	if found {
		p.Printf("verified: the key works and %q is in the models list.\n", model)
	} else {
		p.Printf("the key works, but %q was not in the models list (new models can lag the list; double check the name).\n", model)
	}
	return nil
}

// telegramHasAnyAllowlist reports whether cfg's Telegram config would
// actually let anyone through: a non-empty channel-level allow_from, or a
// non-empty allow_from on any group.
func telegramHasAnyAllowlist(cfg *config.Config) bool {
	if len(cfg.Channels.Telegram.AllowFrom) > 0 {
		return true
	}
	for _, g := range cfg.Channels.Telegram.Groups {
		if len(g.AllowFrom) > 0 {
			return true
		}
	}
	return false
}

// exportLine renders the shell command the user must run themselves to set
// envName persistently, OS-appropriate.
func exportLine(envName string) string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`setx %s "<your value>"`, envName)
	}
	return fmt.Sprintf(`export %s=<your value>`, envName)
}

// onboardTelegram runs onboard steps 4 and 5: token env-var indirection,
// getMe, and - only if the channel is enabled - the interactive Telegram ID
// capture window.
func onboardTelegram(ctx context.Context, p prompter, capturer telegramCapturer, cfg *config.Config) error {
	p.Printf("\n== Telegram ==\n")

	enable, err := p.Confirm("Enable the Telegram channel?", true)
	if err != nil {
		return err
	}
	cfg.Channels.Telegram.Enabled = enable
	if !enable {
		cfg.Channels.Telegram.AllowFrom = nil
		return nil
	}

	envName, err := p.Text("Environment variable holding your Telegram bot token", cfg.Channels.Telegram.TokenEnv)
	if err != nil {
		return err
	}
	cfg.Channels.Telegram.TokenEnv = envName

	typed, err := p.Secret("Paste your Telegram bot token to verify it now (never written to the config; leave blank to skip verification)")
	if err != nil {
		return err
	}
	p.Printf("Add this to your shell profile (never commit it):\n  %s\n", exportLine(envName))

	token := typed
	if token == "" {
		token = os.Getenv(envName)
	}
	if token == "" {
		p.Printf("no token available in the prompt or in %s; skipping getMe and the ID capture - run `mtclaw onboard` again, or `mtclaw doctor`, once it is set.\n", envName)
		return nil
	}

	gctx, cancel := context.WithTimeout(ctx, onboardVerifyTimeout)
	username, err := capturer.GetMe(gctx, token)
	cancel()
	if err != nil {
		p.Printf("getMe failed: %v - double check the token; skipping the ID capture.\n", err)
		return nil
	}
	p.Printf("bot is @%s.\n", username)

	return captureAllowFrom(ctx, p, capturer, token, cfg)
}

// captureAllowFrom is onboard step 5: it opens a temporary long poll for
// telegramCaptureWindow, collecting every distinct sender rather than
// returning on the first one, and never writes an ID the user has not
// explicitly confirmed on screen. More than one distinct sender writes
// nothing automatically and forces a manual choice, because a second
// sender means someone else found the bot during the capture window.
func captureAllowFrom(ctx context.Context, p prompter, capturer telegramCapturer, token string, cfg *config.Config) error {
	p.Printf("\nSend any message to your bot now - you have %s. Collecting every sender who writes in, not just the first.\n", telegramCaptureWindow)

	senders, err := capturer.Capture(ctx, token, telegramCaptureWindow)
	if err != nil {
		p.Printf("capture failed: %v\n", err)
		senders = nil
	}

	switch len(senders) {
	case 0:
		p.Printf("no message arrived in %s.\n", telegramCaptureWindow)
		return manualAllowFromEntry(p, cfg)

	case 1:
		s := senders[0]
		p.Printf("message received from @%s (id %d).\n", s.Username, s.UserID)
		ok, err := p.Confirm(fmt.Sprintf("add %d to channels.telegram.allow_from?", s.UserID), true)
		if err != nil {
			return err
		}
		if ok {
			cfg.Channels.Telegram.AllowFrom = []int64{s.UserID}
			return nil
		}
		return manualAllowFromEntry(p, cfg)

	default:
		p.Printf("%d distinct senders messaged the bot during the capture window - onboard will not guess which one is you:\n", len(senders))
		for _, s := range senders {
			p.Printf("  @%s (id %d)\n", s.Username, s.UserID)
		}
		return manualAllowFromEntry(p, cfg)
	}
}

// manualAllowFromEntry is the fallback for captureAllowFrom: nothing
// arrived, the single sender was declined, or more than one sender showed
// up. It also serves as the `/whoami` pointer the phase file requires.
func manualAllowFromEntry(p prompter, cfg *config.Config) error {
	answer, err := p.Text("Enter your numeric Telegram user id manually (message the bot and run /whoami once it is live, or leave blank to set channels.telegram.allow_from later)", "")
	if err != nil {
		return err
	}
	if answer == "" {
		p.Printf("leaving channels.telegram.allow_from empty for now; the bot will refuse every message until it is set.\n")
		return nil
	}
	id, err := strconv.ParseInt(answer, 10, 64)
	if err != nil {
		p.Printf("could not parse %q as a numeric id; leaving channels.telegram.allow_from empty - edit the config manually.\n", answer)
		return nil
	}
	cfg.Channels.Telegram.AllowFrom = []int64{id}
	return nil
}

// writeStarterAgentsFile writes the embedded starter AGENTS.md to
// ~/.mtclaw/prompts/AGENTS.md, returning the path written so the caller can
// reference it from agent.system_prompt_files.
func writeStarterAgentsFile() (string, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state directory: %w", err)
	}
	promptsDir := filepath.Join(stateDir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", promptsDir, err)
	}
	agentsPath := filepath.Join(promptsDir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(starterAgentsMD), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", agentsPath, err)
	}
	return agentsPath, nil
}

// writeOnboardConfig marshals cfg to YAML and writes it to path at mode
// 0600. It deliberately uses yaml.Marshal directly rather than
// config.MarshalRedacted: MarshalRedacted always renders a placeholder
// string ("<unset>", "<set:env:...>") into OpenAI.APIKeyInline/
// Channels.Telegram.TokenInline for `config show`'s benefit, and writing
// that placeholder into the real config file would itself be a non-empty
// literal api_key/token value - exactly what config.Validate rejects on
// the next load. cfg here never has APIKeyInline/TokenInline set (onboard
// never touches those fields), so plain yaml.Marshal omits them via their
// `omitempty` tag, and the file contains only the env/file indirection
// keys - never a secret.
func writeOnboardConfig(path string, cfg *config.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("render config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

// printOnboardNextSteps prints what to do after onboard finishes.
func printOnboardNextSteps(out io.Writer, cfg *config.Config) {
	fmt.Fprintln(out, "\n== next steps ==")
	fmt.Fprintln(out, "1. Add the export/setx lines printed above to your shell profile (or set them for this session).")
	fmt.Fprintln(out, "2. Run `mtclaw doctor` again any time to re-check the setup.")
	if cfg.Channels.Telegram.Enabled {
		fmt.Fprintln(out, "3. Run `mtclaw gateway` and message your bot on Telegram.")
	} else {
		fmt.Fprintln(out, "3. Run `mtclaw prompt \"hello\"` to try the agent from the terminal (Telegram is disabled).")
	}
}
