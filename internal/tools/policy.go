package tools

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/tiennm99/MTClaw/internal/config"
)

// Verdict is the outcome of Policy.Evaluate.
type Verdict int

const (
	// VerdictRun means the command may execute with no prompt.
	VerdictRun Verdict = iota
	// VerdictRefuse means the command is permanently blocked: never
	// prompted, never overridable by approval.
	VerdictRefuse
	// VerdictAsk means an Approver must decide.
	VerdictAsk
)

// classifierTimeout bounds the auto-mode classifier call. It is an
// additional bound derived from the caller's context, not a replacement for
// it: turn cancellation still cancels the classifier call immediately.
const classifierTimeout = 10 * time.Second

// Decision is Policy.Evaluate's result.
type Decision struct {
	Verdict Verdict
	Audit   string // denied_rule | allowed_rule | approval | auto_allowed
	Rule    string // the matching deny/allow regex source, when any
	Reason  string // shown to the user in the approval prompt when Verdict is VerdictAsk
}

// Policy implements the fixed deny-list -> allow-list -> mode pipeline. It
// is otherwise a pure function of the command string; classifier calls in
// auto mode are the one side effect, and Classifier is an interface
// specifically so tests can inject a fake instead of exercising a real
// provider call.
type Policy struct {
	deny  []compiledRule
	allow []compiledRule

	mode       string // approval | auto (never "off": that mode does not register the tool)
	classifier Classifier
	confirmOn  map[string]bool
	cwd        string
	shell      []string
}

type compiledRule struct {
	re     *regexp.Regexp
	source string
}

// NewPolicy compiles cfg's deny and allow patterns and returns a ready
// Policy. classifier may be nil unless cfg.Mode == "auto", in which case a
// nil classifier is a caller bug (New in registry.go always supplies one
// for auto mode) and Evaluate fails closed with VerdictAsk rather than
// panicking.
func NewPolicy(cfg config.ExecConfig, classifier Classifier) (*Policy, error) {
	deny, err := compileRules(cfg.Deny, true)
	if err != nil {
		return nil, fmt.Errorf("tools: compile tools.exec.deny: %w", err)
	}
	allow, err := compileRules(cfg.Allow, false)
	if err != nil {
		return nil, fmt.Errorf("tools: compile tools.exec.allow: %w", err)
	}

	confirmOn := make(map[string]bool, len(cfg.Auto.ConfirmOn))
	for _, c := range cfg.Auto.ConfirmOn {
		confirmOn[c] = true
	}

	return &Policy{
		deny:       deny,
		allow:      allow,
		mode:       cfg.Mode,
		classifier: classifier,
		confirmOn:  confirmOn,
		cwd:        cfg.CWD,
		shell:      resolveShell(cfg.Shell),
	}, nil
}

// compileRules compiles patterns as-is for the allow-list, but prefixes
// deny patterns with (?s) so "." also matches a newline: a deny pattern
// like ".*" written against a single-line example command must still match
// when the same command arrives with a line continuation (a
// backslash-newline pasted from a multi-line shell snippet). Applying (?s)
// to allow as well would do the opposite of what an allow rule is for: an
// anchored pattern like "^npm run .+$" is meant to permit exactly one tight
// command, and (?s) would let ".+" swallow a newline plus an unrelated
// second command appended after it, auto-running that second command with
// no prompt.
func compileRules(patterns []string, deny bool) ([]compiledRule, error) {
	rules := make([]compiledRule, 0, len(patterns))
	for _, p := range patterns {
		src := p
		if deny {
			src = "(?s)" + p
		}
		re, err := regexp.Compile(src)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", p, err)
		}
		rules = append(rules, compiledRule{re: re, source: p})
	}
	return rules, nil
}

// denyCommandWordQuote matches a command word at the very start of cmd, or
// immediately after a `;`, `&`, or `|` segment separator, that is wrapped in
// a single quote, a double quote, or escaped with a single leading
// backslash - the shapes `'rm' -rf /`, `"rm" -rf /`, and `\rm -rf /` use to
// dodge a plain "rm" pattern without changing what the shell actually runs.
// It is deliberately anchored to the command-word position only: a quoted
// *argument* elsewhere in the command (`grep "rm -rf" file`, `cat "my 'rm
// -rf' notes.txt"`) must keep its quotes, because those quotes are what
// keep "rm -rf" inert text instead of a command - stripping them there
// would turn an ordinary read-only command into a permanent, non-overridable
// refusal.
var denyCommandWordQuote = regexp.MustCompile(`(^|[;&|]\s*)(?:'([^'\s]+)'|"([^"\s]+)"|\\(\w))`)

// normalizeForDeny rewrites only each segment's leading command word,
// unquoting or unescaping it, so a deny rule also catches a command whose
// command word is trivially quoted or escaped without weakening the
// pattern itself or touching quoting anywhere else in cmd. It is evaluated
// in addition to, not instead of, the raw command - see Evaluate - so this
// covers only "one character of rephrasing" at the command-word position,
// not interpreter wrappers like `sh -c '...'` or `eval`, which remain a
// documented gap (see docs/security.md).
func normalizeForDeny(cmd string) string {
	return denyCommandWordQuote.ReplaceAllString(cmd, "${1}${2}${3}${4}")
}

// Evaluate decides cmd in the fixed order deny -> allow -> mode. Deny is
// checked first and, on a match, wins unconditionally: neither the
// allow-list nor a human approver nor the classifier ever sees the command
// again. cmd is matched as the raw command string (not tokenized), exactly
// as config.Validate already required each pattern to compile against.
func (p *Policy) Evaluate(ctx context.Context, cmd string) Decision {
	normalized := normalizeForDeny(cmd)
	for _, rule := range p.deny {
		if rule.re.MatchString(cmd) || rule.re.MatchString(normalized) {
			return Decision{
				Verdict: VerdictRefuse,
				Audit:   "denied_rule",
				Rule:    rule.source,
				Reason:  "matches a deny rule; refused permanently and not overridable by approval",
			}
		}
	}

	for _, rule := range p.allow {
		if rule.re.MatchString(cmd) {
			return Decision{Verdict: VerdictRun, Audit: "allowed_rule", Rule: rule.source}
		}
	}

	switch p.mode {
	case "approval":
		return Decision{Verdict: VerdictAsk, Audit: "approval", Reason: "no deny or allow rule matched; tools.exec.mode is \"approval\""}
	case "auto":
		return p.evaluateAuto(ctx, cmd)
	default:
		// "off" never reaches here (the exec tool is not registered), and
		// nothing else is a valid config.Validate value; fail closed rather
		// than assume anything about an unrecognized mode.
		return Decision{Verdict: VerdictAsk, Audit: "approval", Reason: fmt.Sprintf("unrecognized tools.exec.mode %q; failing closed", p.mode)}
	}
}

// evaluateAuto asks the classifier for cmd's risk, on p.cwd/p.shell only -
// never on any other context the model may have, so a fetched web page or
// forwarded message cannot address the classifier directly. Any classifier
// error, timeout, or nil classifier fails closed to VerdictAsk; the
// classifier's own Classify implementation is additionally required to
// never return a risk that skips this ask/run decision on a parse failure.
func (p *Policy) evaluateAuto(ctx context.Context, cmd string) Decision {
	if p.classifier == nil {
		return Decision{Verdict: VerdictAsk, Audit: "approval", Reason: "auto mode has no risk classifier configured; failing closed"}
	}

	cctx, cancel := context.WithTimeout(ctx, classifierTimeout)
	defer cancel()

	result, err := p.classifier.Classify(cctx, cmd, p.cwd, p.shell)
	if err != nil {
		return Decision{Verdict: VerdictAsk, Audit: "approval", Reason: "risk classification unavailable"}
	}

	ask := result.Risk == "high"
	for _, category := range result.Categories {
		if p.confirmOn[category] {
			ask = true
			break
		}
	}

	if ask {
		reason := result.Reason
		if reason == "" {
			reason = fmt.Sprintf("classifier flagged this command (risk=%s, categories=%v)", result.Risk, result.Categories)
		}
		return Decision{Verdict: VerdictAsk, Audit: "approval", Reason: reason}
	}
	return Decision{Verdict: VerdictRun, Audit: "auto_allowed", Reason: result.Reason}
}
