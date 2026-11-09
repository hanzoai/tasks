// Copyright © 2026 Hanzo AI. MIT License.

// Package dbtest runs a test on both engines the engine's shared database can be on.
//
// SQLite always runs. PostgreSQL runs when TASKS_TEST_SQL_DSN names a server whose
// role may create databases (cloud's scripts/postgres.sh starts one, and exports it as
// CLOUD_TEST_SQL_DSN); without it the postgres subtest is a visible skip.
//
// The body is handed a function that opens a handle on the test's database. Each call
// is a pool of its own onto the same rows, which is what a replica is: two pods do not
// share a pool, they share a server.
package dbtest

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	hanzosqlite "github.com/hanzoai/sqlite"
)

// Env names the PostgreSQL server the postgres leg runs on.
const Env = "TASKS_TEST_SQL_DSN"

// Open opens a handle on the database of the test, closed when the test ends.
type Open func() *sql.DB

// Engines runs body once per engine, each as a subtest, on a database of its own.
func Engines(t *testing.T, body func(t *testing.T, open Open)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tasks.db")
		body(t, func() *sql.DB {
			db, err := hanzosqlite.OpenPragma(hanzosqlite.PragmaDSN(path, hanzosqlite.DefaultPragmas), hanzosqlite.DefaultPragmas)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv(Env)
		if dsn == "" {
			t.Skip(Env + " is not set; cloud's scripts/postgres.sh starts a server for a command")
		}
		name := throwaway(t, dsn)
		cfg, err := pgx.ParseConfig(name)
		if err != nil {
			t.Fatal(err)
		}
		body(t, func() *sql.DB {
			db := stdlib.OpenDB(*cfg.Copy())
			db.SetMaxOpenConns(8)
			t.Cleanup(func() { _ = db.Close() })
			return db
		})
	})
}

// throwaway makes a database on the server dsn names and drops it when the test ends,
// whatever still holds a connection to it. It answers the DSN of that database.
func throwaway(t *testing.T, dsn string) string {
	t.Helper()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	var token [6]byte
	_, _ = rand.Read(token[:])
	name := "t_" + hex.EncodeToString(token[:])
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create the test database on %s: %v", Env, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP DATABASE ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop the test database: %v", err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("%s: %v", Env, err)
	}
	u.Path = "/" + name
	return u.String()
}
