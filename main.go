package main

import (
	"os"

	"github.com/tiennm99/MTClaw/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
