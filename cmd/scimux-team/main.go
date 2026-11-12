// Command scimux-team is the scimux team server.
//
// This executable is deliberately thin: it calls internal/team and holds
// the linker-stamped version. Product behavior lives in internal/team.
package main

import (
	"fmt"
	"os"

	"github.com/scimux/scimux-team/internal/team"
)

// version is stamped at link time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)"
var version = "dev"

func main() {
	// coverage-exempt: main exits the process, so tests cannot call it
	if err := team.Run(os.Args[1:], version, os.Stdout, os.Stderr); err != nil {
		// coverage-exempt: main exits the process, so tests cannot call it
		fmt.Fprintln(os.Stderr, "scimux-team:", err)
		os.Exit(team.ExitCode(err))
	}
}
