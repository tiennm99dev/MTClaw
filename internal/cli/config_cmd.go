package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tiennm99/MTClaw/internal/config"
)

func newConfigCmd(s *state) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the mtclaw configuration",
	}
	cmd.AddCommand(newConfigPathCmd(s))
	cmd.AddCommand(newConfigShowCmd(s))
	cmd.AddCommand(newConfigValidateCmd(s))
	return cmd
}

// newConfigPathCmd prints the resolved config path and where it came from.
// It is exempt from config loading (see skipsConfigLoad) so it works even
// when the file is missing or invalid.
func newConfigPathCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the resolved config file path and its source",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "%s (source: %s)\n", s.configPath, s.configSource)
			return err
		},
	}
}

// newConfigShowCmd prints the loaded config back as YAML with every secret
// replaced by a placeholder. Reaching RunE implies the config already
// loaded and validated successfully, since PersistentPreRunE runs first.
func newConfigShowCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the resolved config with secrets redacted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := config.MarshalRedacted(s.cfg)
			if err != nil {
				return fmt.Errorf("render config: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
}

// newConfigValidateCmd validates the config file. All the actual work
// happens in PersistentPreRunE: if the config is invalid, cobra reports the
// full multi-error and this RunE never runs; reaching it means every rule
// passed.
func newConfigValidateCmd(s *state) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate the config file and report every error at once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "OK: %s\n", s.configPath)
			return err
		},
	}
}
