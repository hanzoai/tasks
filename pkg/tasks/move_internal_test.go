// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/internal/dbtest"
	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// A move that stopped halfway left rows and a marker saying they are its own: the next
// attempt clears them and copies from the files, so a row the files no longer hold does
// not come back, and the count and checksum it ends on are the files'.
func TestMoveFiles_RestartsOverTheRemainsOfItsOwn(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		ctx := context.Background()
		dir := t.TempDir()
		files, err := Embed(ctx, EmbedConfig{Address: t.TempDir() + "/f.sock", DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if err := files.View(Principal{}).CreateSchedule(Schedule{
			ScheduleId: "kept", Namespace: "default",
			Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
			Action: ScheduleAction{WorkflowType: TypeRef{Name: "W"}, TaskQueue: "q"},
		}); err != nil {
			t.Fatal(err)
		}
		if err := files.Stop(ctx); err != nil {
			t.Fatal(err)
		}

		db := open()
		sh, err := storepkg.OpenShared(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		// What the stopped attempt wrote: a row the files do not hold, and its marker.
		if err := sh.Put(ctx, storepkg.Principal{}, "sc/default/deleted-since", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := sh.SetMeta(ctx, movingKey, "then"); err != nil {
			t.Fatal(err)
		}
		if moving, _ := Moving(ctx, db); !moving {
			t.Fatal("a move under way is not said to be")
		}

		rep, err := MoveFiles(ctx, db, dir, nil)
		if err != nil {
			t.Fatalf("the move over its own remains: %v", err)
		}
		if rep.Rows < 2 {
			t.Fatalf("the move copied %d rows", rep.Rows)
		}
		if _, ok, _ := sh.Get(ctx, storepkg.Principal{}, "sc/default/deleted-since"); ok {
			t.Fatal("a row the files no longer hold came back")
		}
		if _, ok, _ := sh.Get(ctx, storepkg.Principal{}, "sc/default/kept"); !ok {
			t.Fatal("the files' schedule was not copied")
		}
		if moving, _ := Moving(ctx, db); moving {
			t.Fatal("a finished move is said to be under way")
		}
	})
}
