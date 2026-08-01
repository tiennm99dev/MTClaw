package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/config"
)

// Status is one Check's outcome. OK/WARN/FAIL mirror doctor's table; INFO is
// for facts worth surfacing that are not problems (an already-running
// gateway holding the instance lock, most notably).
type Status string

const (
	StatusOK   Status = "OK"
	StatusWarn Status = "WARN"
	StatusFail Status = "FAIL"
	StatusInfo Status = "INFO"
)

// Result is one Check's report: a Status plus an actionable message. A FAIL
// message must contain the fix, not just the symptom - see doctor_checks.go.
type Result struct {
	Status  Status
	Message string
}

// Check is one named doctor diagnostic. Run receives the already-loaded,
// already-validated config; it is never called when config loading itself
// failed (see runDoctor), since nothing below that point can be checked
// meaningfully without a config to check it against. `onboard` (step 9 of
// its sequence) runs the same registry after writing a fresh config, so a
// check written once here backs both commands and both sets of tests.
type Check struct {
	Name string
	Run  func(ctx context.Context, cfg *config.Config) Result
}

// Row is one printed/JSON-encoded line of doctor's report: a Check's Name
// plus the Result it produced.
type Row struct {
	Check   string `json:"check"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// runDoctor loads configPath itself (bypassing the normal
// PersistentPreRunE, since doctor's whole point is to diagnose a config
// that may not load at all) and runs every Check in order. When the config
// fails to load or validate, that failure becomes the sole row: every other
// check needs a *config.Config to check anything, so running them against a
// nil config would just repeat the same failure in different words.
func runDoctor(ctx context.Context, configPath string) []Row {
	cfg, err := config.LoadFile(configPath)
	if err != nil {
		return []Row{{
			Check:  "Config found and valid",
			Status: string(StatusFail),
			Message: fmt.Sprintf(
				"%s - fix the error(s) above (run `mtclaw config validate` for the same report), or run `mtclaw onboard` to write a fresh config",
				err.Error(),
			),
		}}
	}

	rows := []Row{{Check: "Config found and valid", Status: string(StatusOK), Message: fmt.Sprintf("loaded %s", configPath)}}
	for _, c := range doctorChecks(configPath) {
		res := c.Run(ctx, cfg)
		rows = append(rows, Row{Check: c.Name, Status: string(res.Status), Message: res.Message})
	}
	return rows
}

// anyFailed reports whether rows contains a FAIL, the condition that makes
// `doctor` exit non-zero.
func anyFailed(rows []Row) bool {
	for _, r := range rows {
		if r.Status == string(StatusFail) {
			return true
		}
	}
	return false
}

// printDoctorReport renders rows either as a table (default) or as JSON
// (--json), for scripting.
func printDoctorReport(w io.Writer, rows []Row, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tMESSAGE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Status, r.Check, r.Message)
	}
	return tw.Flush()
}

// newDoctorCmd builds `mtclaw doctor`. It is exempt from the normal config
// load (see skipsConfigLoad in root.go) precisely so a missing or broken
// config file is a diagnosable FAIL row instead of a command that cannot
// even start.
func newDoctorCmd(s *state) *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local install and configuration",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			rows := runDoctor(cmd.Context(), s.configPath)
			if err := printDoctorReport(cmd.OutOrStdout(), rows, jsonOut); err != nil {
				return err
			}
			if anyFailed(rows) {
				return fmt.Errorf("doctor: one or more checks failed")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "print machine-readable JSON instead of a table")
	return cmd
}
