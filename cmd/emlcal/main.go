// Command emlcal is a local, offline-first mail and calendar archive with an
// agent-friendly CLI. See DESIGN.md.
package main

import (
	"os"

	"github.com/lennert/emlcal/internal/cli"
)

func main() {
	app := cli.NewApp()
	os.Exit(cli.Execute(os.Args[1:], app))
}
