// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"testing"
)

// These run against a hanzoai/sql server's ZAP listener, where TASKS_SQL_ADDR names
// one (a test server, never production's), and drop the table they made.

func sharedManager(t *testing.T) *Manager {
	t.Helper()
	addr := os.Getenv("TASKS_SQL_ADDR")
	if addr == "" {
		t.Skip("TASKS_SQL_ADDR unset")
	}
	master := make([]byte, MasterKeyLen)
	_, _ = rand.Read(master)
	m, err := NewShared(context.Background(), addr, master)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = m.srv.exec(context.Background(), `DELETE FROM `+Table)
		_ = m.Close()
	})
	return m
}

func TestSharedShardRowsAreTheFilesRows(t *testing.T) {
	ctx := context.Background()
	shared := sharedManager(t)
	file, err := New(t.TempDir(), make([]byte, MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	p := OrgProject("acme", "web")
	keys := []string{"wf/a", "wf/a/1", "wf/b", "wf/bé", "wf/b世", "wf/c\x7f", "act/x", "wf/z"}
	for _, m := range []*Manager{shared, file} {
		s, err := m.Get(ctx, p, "default")
		if err != nil {
			t.Fatal(err)
		}
		for i, k := range keys {
			if err := s.Put(ctx, k, []byte(fmt.Sprintf("v%d", i))); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Put(ctx, "wf/a", []byte("again")); err != nil {
			t.Fatal(err)
		}
		if err := s.Del(ctx, "wf/z"); err != nil {
			t.Fatal(err)
		}
	}
	walk := func(m *Manager, prefix, after string, limit int) []string {
		s, _ := m.Get(ctx, p, "default")
		var out []string
		if err := s.Scan(ctx, prefix, after, limit, func(k string, v []byte) error {
			out = append(out, k+"="+string(v))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, c := range []struct {
		prefix, after string
		limit         int
	}{{"", "", 0}, {"wf/", "", 0}, {"wf/b", "", 0}, {"wf/", "wf/a/1", 2}, {"act/", "", 1}, {"nope/", "", 0}} {
		a, b := walk(shared, c.prefix, c.after, c.limit), walk(file, c.prefix, c.after, c.limit)
		if !slices.Equal(a, b) {
			t.Fatalf("scan %+v: the shared store walks %v, the file %v", c, a, b)
		}
	}
	s, _ := shared.Get(ctx, p, "default")
	if v, ok, err := s.Get(ctx, "wf/a"); err != nil || !ok || string(v) != "again" {
		t.Fatalf("get wf/a = %q %v %v", v, ok, err)
	}
	if _, ok, err := s.Get(ctx, "wf/z"); err != nil || ok {
		t.Fatalf("a deleted key reads back: %v %v", ok, err)
	}
	ps, err := shared.ListPrincipals(ctx)
	if err != nil || len(ps) != 1 || ps[0] != p {
		t.Fatalf("principals %v %v", ps, err)
	}
}

func TestSharedRowsAreSealed(t *testing.T) {
	ctx := context.Background()
	m := sharedManager(t)
	s, err := m.Get(ctx, Org("acme"), "default")
	if err != nil {
		t.Fatal(err)
	}
	marker := "plaintext-marker-77ab"
	if err := s.Put(ctx, "k", []byte(marker)); err != nil {
		t.Fatal(err)
	}
	rows, err := m.srv.query(ctx, `SELECT 'h' || encode(value,'hex') AS v FROM `+Table)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("no row was written")
	}
	for _, r := range rows {
		v, _ := r["v"].(string)
		b, err := unhx(v)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Fatal("the server holds a value in the clear")
		}
	}
}

func TestCopyMovesEveryShardAndProvesIt(t *testing.T) {
	ctx := context.Background()
	dst := sharedManager(t)
	src, err := New(t.TempDir(), make([]byte, MasterKeyLen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	want := 0
	for _, p := range []Principal{{}, Org("acme"), OrgProject("acme", "web"), Org("beta")} {
		for _, ns := range []string{"default", "jobs"} {
			s, err := src.Get(ctx, p, ns)
			if err != nil {
				t.Fatal(err)
			}
			for i := range 1234 {
				v := bytes.Repeat([]byte{byte(i)}, i%300)
				if err := s.Put(ctx, fmt.Sprintf("k/%05d", i), v); err != nil {
					t.Fatal(err)
				}
				want++
			}
		}
	}
	// A row only the destination holds, as an interrupted copy and a later delete leave.
	stray, _ := dst.Get(ctx, Org("acme"), "default")
	if err := stray.Put(ctx, "gone", []byte("x")); err != nil {
		t.Fatal(err)
	}
	reports, err := Copy(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for _, r := range reports {
		got += r.Rows
		if r.Digest == "" || r.Rows != r.Written {
			t.Fatalf("report %+v", r)
		}
	}
	if got != want || len(reports) != 8 {
		t.Fatalf("copied %d rows in %d shards, want %d in 8", got, len(reports), want)
	}
	if _, ok, _ := stray.Get(ctx, "gone"); ok {
		t.Fatal("a row the source does not hold survived the copy")
	}
	again, err := Copy(ctx, src, dst)
	if err != nil || len(again) != 8 {
		t.Fatalf("a second copy: %v %d", err, len(again))
	}
}
