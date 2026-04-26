package temporalite

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanzoai/tasks/common/config"
	sqliteplugin "github.com/hanzoai/tasks/common/persistence/sql/sqlplugin/sqlite"
	schemasqlite "github.com/hanzoai/tasks/schema/sqlite"
)

// TestSqliteNeedsSchema covers the three states LiteServer must handle on
// boot: missing file, zero-byte file (PV mount leftover), and a fully
// provisioned database. The regression that triggered this helper was a
// fresh pod crashing on "no such table: cluster_metadata_info" when the
// PV had left an empty file behind.
func TestSqliteNeedsSchema(t *testing.T) {
	dir := t.TempDir()

	// 1. Missing file -> needs schema.
	missing := filepath.Join(dir, "missing.db")
	need, err := sqliteNeedsSchema(missing)
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	if !need {
		t.Fatal("missing file should require schema setup")
	}

	// 2. Zero-byte file (PV touch) -> needs schema. This is the specific
	//    production failure mode: the old code skipped setup here.
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("create empty: %v", err)
	}
	need, err = sqliteNeedsSchema(empty)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if !need {
		t.Fatal("zero-byte file should require schema setup")
	}

	// 3. Provisioned DB -> schema already installed.
	prov := filepath.Join(dir, "provisioned.db")
	cfg := &config.SQL{
		PluginName:        sqliteplugin.PluginName,
		DatabaseName:      prov,
		ConnectAttributes: map[string]string{"mode": "rwc"},
	}
	if err := schemasqlite.SetupSchema(cfg); err != nil {
		t.Fatalf("setup schema: %v", err)
	}
	need, err = sqliteNeedsSchema(prov)
	if err != nil {
		t.Fatalf("provisioned: %v", err)
	}
	if need {
		t.Fatal("provisioned database should not require schema setup")
	}
}
