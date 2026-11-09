// Copyright © 2026 Hanzo AI. MIT License.

package store_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks/internal/dbtest"
	"github.com/hanzoai/tasks/pkg/tasks/store"
)

func newShared(t *testing.T, open dbtest.Open) *store.Shared {
	t.Helper()
	sh, err := store.OpenShared(context.Background(), open())
	if err != nil {
		t.Fatalf("OpenShared: %v", err)
	}
	return sh
}

func TestShared_PutGetDel(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedPutGetDel(t, open)
	})
}

func checkSharedPutGetDel(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	p := store.Org("org-a")
	if err := sh.Put(ctx, p, "wf/ns1/a/1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := sh.Get(ctx, p, "wf/ns1/a/1")
	if err != nil || !ok || string(v) != "hello" {
		t.Fatalf("get: ok=%v v=%q err=%v", ok, v, err)
	}
	if err := sh.Put(ctx, p, "wf/ns1/a/1", []byte("again")); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := sh.Get(ctx, p, "wf/ns1/a/1"); string(v) != "again" {
		t.Fatalf("a second put must replace: %q", v)
	}
	if err := sh.Del(ctx, p, "wf/ns1/a/1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := sh.Get(ctx, p, "wf/ns1/a/1"); ok {
		t.Fatal("expected delete to remove key")
	}
	if err := sh.Del(ctx, p, "wf/ns1/a/1"); err != nil {
		t.Fatalf("deleting a missing key is not an error: %v", err)
	}
}

func TestShared_TenantIsolation(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedTenantIsolation(t, open)
	})
}

func checkSharedTenantIsolation(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	a, b := store.Org("org-a"), store.Org("org-b")
	_ = sh.Put(ctx, a, "wf/default/k/1", []byte("A"))
	_ = sh.Put(ctx, b, "wf/default/k/1", []byte("B"))
	if v, _, _ := sh.Get(ctx, a, "wf/default/k/1"); string(v) != "A" {
		t.Fatalf("a sees %q", v)
	}
	if v, _, _ := sh.Get(ctx, b, "wf/default/k/1"); string(v) != "B" {
		t.Fatalf("b sees %q", v)
	}
	var n int
	_ = sh.List(ctx, store.Principal{}, "wf/default/", func(string, []byte) error { n++; return nil })
	if n != 0 {
		t.Fatalf("the root tenant sees %d rows of the orgs", n)
	}
}

func TestShared_KeyRefusals(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedKeyRefusals(t, open)
	})
}

func checkSharedKeyRefusals(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	for _, key := range []string{"", "nokind", "wf/ns/\x00", "wf/ns/\xff\xfe"} {
		if err := sh.Put(ctx, store.Principal{}, key, []byte("x")); err == nil {
			t.Errorf("Put(%q) must be refused: PostgreSQL text holds neither NUL nor invalid UTF-8", key)
		}
	}
}

func collect(t *testing.T, fn func(func(string, []byte) error) error) []string {
	t.Helper()
	var keys []string
	if err := fn(func(k string, _ []byte) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatal(err)
	}
	return keys
}

// A prefix scan is by the bytes of the key, in key order, and stops where the prefix
// does; a page resumes after the last key of the one before.
func TestShared_ScanIsByteOrderedAndPages(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedScanIsByteOrderedAndPages(t, open)
	})
}

func checkSharedScanIsByteOrderedAndPages(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	p := store.Org("org-a")
	var want []string
	for i := 0; i < 25; i++ {
		k := fmt.Sprintf("wfh/ns/wf/run/%020d", i)
		want = append(want, k)
		if err := sh.Put(ctx, p, k, []byte("e")); err != nil {
			t.Fatal(err)
		}
	}
	// neighbours that must not appear: the prefix's upper bound and a sibling run
	for _, k := range []string{"wfh/ns/wf/run0/1", "wfh/ns/wf/ruN/1", "wfh/ns/wf/run", "wf/ns/wf/run/1", "wfh/ns/wf/run/é"} {
		if err := sh.Put(ctx, p, k, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	want = append(want, "wfh/ns/wf/run/é") // after the digits in byte order, and under the prefix
	got := collect(t, func(fn func(string, []byte) error) error { return sh.List(ctx, p, "wfh/ns/wf/run/", fn) })
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("list:\n got %v\nwant %v", got, want)
	}
	var paged []string
	after := ""
	for {
		page := collect(t, func(fn func(string, []byte) error) error { return sh.Scan(ctx, p, "wfh/ns/wf/run/", after, 7, fn) })
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		after = page[len(page)-1]
	}
	if strings.Join(paged, "\n") != strings.Join(want, "\n") {
		t.Fatalf("paged scan:\n got %v\nwant %v", paged, want)
	}
}

// A callback may ask the store for more while a scan is handing it rows: the scan has
// read its page before the first call, so it does not wait on its own connection.
func TestShared_ScanCallbackMayQuery(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedScanCallbackMayQuery(t, open)
	})
}

func checkSharedScanCallbackMayQuery(t *testing.T, open dbtest.Open) {
	db := open()
	db.SetMaxOpenConns(1)
	sh, err := store.OpenShared(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p := store.Principal{}
	for i := 0; i < 3; i++ {
		_ = sh.Put(ctx, p, fmt.Sprintf("sc/ns/%d", i), []byte("s"))
	}
	err = sh.List(ctx, p, "sc/ns/", func(k string, _ []byte) error {
		_, _, err := sh.Get(ctx, p, k)
		return err
	})
	if err != nil {
		t.Fatalf("a callback that queries on a one-connection handle: %v", err)
	}
}

// ScanAll is the sweeper's view: one kind, every tenant, the tenant beside each row.
func TestShared_ScanAllCrossesTenants(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedScanAllCrossesTenants(t, open)
	})
}

func checkSharedScanAllCrossesTenants(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	tenants := []store.Principal{{}, store.Org("acme"), store.OrgProject("acme", "web")}
	for _, p := range tenants {
		_ = sh.Put(ctx, p, "sc/ns/a", []byte("s"))
		_ = sh.Put(ctx, p, "wf/ns/a/1", []byte("w"))
		_ = sh.Put(ctx, p, "ns/ns", []byte("n"))
	}
	var seen []string
	err := sh.ScanAll(ctx, "sc/", func(p store.Principal, key string, _ []byte) error {
		seen = append(seen, p.String()+" "+key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)
	want := []string{"_/_/_ sc/ns/a", "acme/_/_ sc/ns/a", "acme/web/_ sc/ns/a"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("scan all: got %v want %v", seen, want)
	}
	got, err := sh.Principals(ctx)
	if err != nil || len(got) != 3 {
		t.Fatalf("principals: %v %v", got, err)
	}
}

// Two replicas open one database at the same moment, as a deployment starts: the
// migration is safe to race.
func TestShared_OpenIsSafeToRace(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedOpenIsSafeToRace(t, open)
	})
}

func checkSharedOpenIsSafeToRace(t *testing.T, open dbtest.Open) {
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		db := open()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.OpenShared(context.Background(), db)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a replica that opened beside others: %v", err)
		}
	}
}

func TestShared_LeaseIsExclusiveAndExpires(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedLeaseIsExclusiveAndExpires(t, open)
	})
}

func checkSharedLeaseIsExclusiveAndExpires(t *testing.T, open dbtest.Open) {
	a, err := store.OpenShared(context.Background(), open())
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.OpenShared(context.Background(), open())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	held, err := a.Claim(ctx, "lead/sweep", "a", 2*time.Second)
	if err != nil || !held {
		t.Fatalf("a claims a free lease: %v %v", held, err)
	}
	if held, _ := b.Claim(ctx, "lead/sweep", "b", 2*time.Second); held {
		t.Fatal("b took a lease a holds")
	}
	if held, _ := a.Claim(ctx, "lead/sweep", "a", 2*time.Second); !held {
		t.Fatal("a renews its own lease")
	}
	// Released: the next asker has it at once.
	if err := a.Release(ctx, "lead/sweep", "a"); err != nil {
		t.Fatal(err)
	}
	if held, _ := b.Claim(ctx, "lead/sweep", "b", 1*time.Second); !held {
		t.Fatal("b takes a lease that was released")
	}
	// A holder that stops renewing loses it at the end of its term, in the database's clock.
	time.Sleep(2200 * time.Millisecond)
	if held, _ := a.Claim(ctx, "lead/sweep", "a", 2*time.Second); !held {
		t.Fatal("a takes a lease whose term is over")
	}
	// Releasing a lease the holder no longer has leaves it to the new holder.
	if err := b.Release(ctx, "lead/sweep", "b"); err != nil {
		t.Fatal(err)
	}
	if held, _ := b.Claim(ctx, "lead/sweep", "b", 2*time.Second); held {
		t.Fatal("b's late release took a's lease")
	}
}

func TestShared_MembersLapseWhenExpired(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedMembersLapseWhenExpired(t, open)
	})
}

func checkSharedMembersLapseWhenExpired(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if held, err := sh.Claim(ctx, "member/"+id, id, time.Minute); !held || err != nil {
			t.Fatalf("join %s: %v %v", id, held, err)
		}
	}
	if err := sh.Expire(ctx, "member/b", "b"); err != nil {
		t.Fatal(err)
	}
	held, _ := sh.Held(ctx, "member/")
	lapsed, _ := sh.Lapsed(ctx, "member/")
	if strings.Join(held, ",") != "member/a,member/c" || strings.Join(lapsed, ",") != "member/b" {
		t.Fatalf("held %v lapsed %v", held, lapsed)
	}
	if err := sh.Release(ctx, "member/b", "b"); err != nil {
		t.Fatal(err)
	}
	if lapsed, _ := sh.Lapsed(ctx, "member/"); len(lapsed) != 0 {
		t.Fatalf("a retired member still lapsed: %v", lapsed)
	}
}

func TestShared_Meta(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkSharedMeta(t, open)
	})
}

func checkSharedMeta(t *testing.T, open dbtest.Open) {
	sh := newShared(t, open)
	ctx := context.Background()
	if _, ok, _ := sh.Meta(ctx, "moved"); ok {
		t.Fatal("nothing moved yet")
	}
	if err := sh.SetMeta(ctx, "moved", "1"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := sh.Meta(ctx, "moved"); !ok || v != "1" {
		t.Fatalf("meta: %q %v", v, ok)
	}
}
