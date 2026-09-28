package main

import (
	"errors"
	"os"

	// Embeds the IANA timezone database (~450 KB) so cron.timezone loads on
	// Windows (which ships no zoneinfo files at all) and on a
	// scratch/distroless Linux image with no /usr/share/zoneinfo, not only
	// on a normal POSIX install.
	_ "time/tzdata"

	"github.com/tiennm99/MTClaw/internal/cli"
)

func main() {
	err := cli.Execute()
	if err == nil {
		return
	}
	if errors.Is(err, cli.ErrInterrupted) {
		// 128 + SIGINT(2), the conventional "killed by signal" exit code -
		// distinguishable from an ordinary command failure (1) by anything
		// scripting mtclaw and checking $?.
		os.Exit(130)
	}
	os.Exit(1)
}
