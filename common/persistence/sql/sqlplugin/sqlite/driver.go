package sqlite

import (
	"regexp"

	// hanzosqlite is the ONE Hanzo SQLite driver (registers "sqlite" under both
	// build tags; !cgo backend IS modernc, cgo is mattn+SQLCipher). Its
	// backend-neutral constraint classifiers replace the direct
	// modernc.org/sqlite/lib error-code checks so no file imports modernc.
	hanzosqlite "github.com/hanzoai/sqlite"
)

const (
	goSQLDriverName       = "sqlite"
	sqlTableExistsPattern = "SQL logic error: table .* already exists \\(1\\)"
)

var sqlTableExistsRegex = regexp.MustCompile(sqlTableExistsPattern)

// IsDupEntryError reports a duplicate-key insert — a UNIQUE or PRIMARY KEY
// constraint violation — via the driver's backend-neutral classifier.
func (*db) IsDupEntryError(err error) bool {
	return hanzosqlite.IsConstraintUnique(err) || hanzosqlite.IsConstraintPrimaryKey(err)
}

func isTableExistsError(err error) bool {
	return err != nil && sqlTableExistsRegex.MatchString(err.Error())
}
