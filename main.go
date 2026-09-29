package main

import (
	"os"

	// Embeds the IANA timezone database (~450 KB) so cron.timezone loads on
	// Windows (which ships no zoneinfo files at all) and on a
	// scratch/distroless Linux image with no /usr/share/zoneinfo, not only
	// on a normal POSIX install.
	_ "time/tzdata"

	"github.com/tiennm99/MTClaw/internal/cli"
)

func main() {
	os.Exit(cli.ExitCode(cli.Execute()))
}
