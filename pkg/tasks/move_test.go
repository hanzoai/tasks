// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/tasks"
	"github.com/hanzoai/tasks/pkg/tasks/internal/dbtest"
	luxlog "github.com/luxfi/log"
)

// An engine that kept its rows in shard files moves to a shared database: what had
// finished is still there, what was in flight when it stopped is taken up by the engine
// that opens the moved rows, and the move is done once.
func TestMoveFiles_InFlightWorkAndHistoryReachTheSharedEngine(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		moveFiles(t, open)
	})
}

func moveFiles(t *testing.T, open dbtest.Open) {
	ctx := context.Background()
	dir := t.TempDir()
	resetHandover()
	register := func(w worker.Worker) { registerHandover(w); registerQuick(w) }

	// The engine of one process, on its files: one workflow finishes, one is in the
	// middle of its activity, and a schedule is declared.
	port := freePort(t)
	files, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", port), DataDir: dir})
	if err != nil {
		t.Fatalf("embed on files: %v", err)
	}
	cli := dialLocal(t, port)
	w := worker.New(cli, "default", worker.Options{Logger: luxlog.New()})
	register(w)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	done, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "finished-wf", TaskQueue: "default"}, "QuickWorkflow", "finished-wf")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitStatus(t, ctx, cli, done.GetID(), done.GetRunID(), client.WorkflowStatusCompleted, 10*time.Second); got != client.WorkflowStatusCompleted {
		t.Fatalf("the first workflow did not finish: %d", got)
	}
	flight, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "in-flight-wf", TaskQueue: "default"}, "HandoverWorkflow")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-handoverIn:
	case <-time.After(10 * time.Second):
		t.Fatal("the activity never started")
	}
	if err := files.View(tasks.Principal{}).CreateSchedule(tasks.Schedule{
		ScheduleId: "kept", Namespace: "default",
		Spec:   tasks.ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: tasks.ScheduleAction{WorkflowType: tasks.TypeRef{Name: "QuickWorkflow"}, TaskQueue: "default", Input: []any{"kept"}},
	}); err != nil {
		t.Fatal(err)
	}
	// The process stops, mid-activity.
	cli.Close()
	if err := files.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { close(handoverBlock); w.Stop() })

	if !tasks.FilesIn(dir) {
		t.Fatal("the engine left no shard files to move")
	}
	db := open()
	if moved, err := tasks.Moved(ctx, db); err != nil || moved {
		t.Fatalf("a database nothing was moved into says it was: %v %v", moved, err)
	}
	rep, err := tasks.MoveFiles(ctx, db, dir, nil)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if rep.Already || rep.Rows < 10 || rep.Tenants != 1 || rep.Shards != 1 {
		t.Fatalf("the move copied too little: %+v", rep)
	}
	again, err := tasks.MoveFiles(ctx, db, dir, nil)
	if err != nil || !again.Already || again.Rows != rep.Rows {
		t.Fatalf("a second move must find the first: %+v %v", again, err)
	}
	if moved, _ := tasks.Moved(ctx, db); !moved {
		t.Fatal("moved rows are not said to be moved")
	}

	// The engine that opens the moved rows is a replica of ONE engine on them.
	bport := freePort(t)
	shared, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", bport), Database: db, NodeID: "moved"})
	if err != nil {
		t.Fatalf("embed on the moved rows: %v", err)
	}
	bcli := dialLocal(t, bport)
	bw := worker.New(bcli, "default", worker.Options{Logger: luxlog.New()})
	register(bw)
	if err := bw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bw.Stop(); bcli.Close(); _ = shared.Stop(ctx) })

	if got := waitStatus(t, ctx, bcli, flight.GetID(), flight.GetRunID(), client.WorkflowStatusCompleted, 20*time.Second); got != client.WorkflowStatusCompleted {
		t.Fatalf("the workflow in flight when the files were moved did not complete: %d", got)
	}
	if n := handoverDone.Load(); n != 1 {
		t.Fatalf("its activity was answered %d times, want 1", n)
	}
	info, err := bcli.DescribeWorkflow(ctx, done.GetID(), done.GetRunID())
	if err != nil || info.Status != client.WorkflowStatusCompleted {
		t.Fatalf("the workflow that finished before the move: %v %v", info, err)
	}
	scheds, err := shared.View(tasks.Principal{}).ListSchedules("default")
	if err != nil || len(scheds) != 1 || scheds[0].ScheduleId != "kept" {
		t.Fatalf("schedules after the move: %v %v", scheds, err)
	}
}

// A database that already holds the rows of an engine that did not come from the files
// is not moved into; and a move that stopped halfway is begun again over its own remains.
func TestMoveFiles_RefusesForeignRowsAndRestartsItsOwn(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkMoveRefusesForeignRows(t, open)
	})
}

func checkMoveRefusesForeignRows(t *testing.T, open dbtest.Open) {
	ctx := context.Background()
	dir := t.TempDir()
	port := freePort(t)
	files, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", port), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := files.View(tasks.Principal{}).CreateSchedule(tasks.Schedule{
		ScheduleId: "s", Namespace: "default",
		Spec:   tasks.ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: tasks.ScheduleAction{WorkflowType: tasks.TypeRef{Name: "W"}, TaskQueue: "q"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := files.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// An engine born on the database, with a schedule of its own.
	db := open()
	born, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", freePort(t)), Database: db, NodeID: "born"})
	if err != nil {
		t.Fatal(err)
	}
	if err := born.View(tasks.Principal{}).CreateSchedule(tasks.Schedule{
		ScheduleId: "born", Namespace: "default",
		Spec:   tasks.ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: tasks.ScheduleAction{WorkflowType: tasks.TypeRef{Name: "W"}, TaskQueue: "q"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := born.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tasks.MoveFiles(ctx, db, dir, nil); !errors.Is(err, tasks.ErrForeign) {
		t.Fatalf("files were moved over the rows of an engine born on the database: %v", err)
	}
	if moving, _ := tasks.Moving(ctx, db); moving {
		t.Fatal("a move that was refused is said to be under way")
	}
}
