package main

import (
	"os"

	"github.com/hanzoai/tasks/tools/cassandra"
)

func main() {
	if err := cassandra.RunTool(os.Args); err != nil {
		os.Exit(1)
	}
}
