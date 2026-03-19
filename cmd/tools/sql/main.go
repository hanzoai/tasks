package main

import (
	"os"

	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/mysql"      // needed to load mysql plugin
	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/postgresql" // needed to load postgresql plugin
	_ "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/sqlite"     // needed to load sqlite plugin
	"github.com/hanzoai/tasks/tools/sql"
)

func main() {
	if err := sql.RunTool(os.Args); err != nil {
		os.Exit(1)
	}
}
