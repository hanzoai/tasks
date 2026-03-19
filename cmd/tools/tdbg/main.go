package main

import (
	"os"

	"github.com/hanzoai/tasks/tools/tdbg"
)

func main() {
	app := tdbg.NewCliApp()
	if err := app.Run(os.Args); err != nil {
		os.Exit(1)
	}
}
