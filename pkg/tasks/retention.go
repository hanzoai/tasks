// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// Retention.
//
// The purge enforces each namespace's WorkflowExecutionRetentionTtl: it walks
// the namespace's run rows a page per pass, from a cursor kept at
// purge/<ns>, and removes every terminal run whose close time plus the
// retention has passed, together with every key that belongs to it. A run's
// row is deleted last, so a pass cut short leaves the row to find the rest
// by on the next walk.

const (
	// purgeEvery is how often the purge runs.
	purgeEvery = 5 * time.Second
	// purgeBatch is how many run rows one pass reads from one shard. An
	// expired run is about ten single-row deletes, each its own statement, so
	// engine work on the shard interleaves with a pass instead of waiting it
	// out.
	purgeBatch = 512
)

const msgPurgeFailed = "tasks: retention purge failed"

func purgeKey(ns string) string { return "purge/" + ns }

// runPrefixes is the order the purge walks a namespace's run rows in:
// workflow runs, then standalone activities.
func runPrefixes(ns string) []string { return []string{"wf/" + ns + "/", "act/" + ns + "/"} }

// closedAt is when a run closed: its close time, else its start time. A row
// with neither readable reads as the zero time, which every retention has
// passed.
func closedAt(closeTime, startTime string) time.Time {
	s := closeTime
	if s == "" {
		s = startTime
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// defaultRetention is how long a namespace keeps a terminal run unless its
// registry row says otherwise.
const defaultRetention = "720h"

// retentionOf reads how long n keeps a terminal run. A namespace with no
// retention keeps the default. One that does not parse, or is not positive,
// is an error rather than a guess: nothing is purged under a retention that
// cannot be read.
func retentionOf(n Namespace) (time.Duration, error) {
	s := n.Config.WorkflowExecutionRetentionTtl
	if s == "" {
		s = defaultRetention
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("namespace %q: retention %q is not a positive duration", n.NamespaceInfo.Name, s)
	}
	return d, nil
}

// runPurge enforces every namespace's retention until stop closes.
func (e *engine) runPurge(stop <-chan struct{}) {
	t := time.NewTicker(purgeEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			e.purgeOnce(time.Now())
		}
	}
}

// purgeOnce runs one pass over every tenant's namespaces, the same set the
// cron sweeper fires schedules across. A namespace whose pass fails is
// reported, throttled, and the rest still run.
func (e *engine) purgeOnce(now time.Time) {
	type target struct {
		p Principal
		n Namespace
	}
	var targets []target
	err := e.store.listEveryTenant("ns/", func(p Principal, _ string, body []byte) error {
		var n Namespace
		if json.Unmarshal(body, &n) == nil && n.NamespaceInfo.Name != "" {
			targets = append(targets, target{p, n})
		}
		return nil
	})
	e.reportPurge(Principal{}, "", err)
	for _, t := range targets {
		_, err := e.As(t.p).purge(t.n, now, purgeBatch)
		e.reportPurge(t.p, t.n.NamespaceInfo.Name, err)
	}
}

// reportPurge logs a failed pass under the package's one throttle rule; a nil
// err ends the streak. An empty ns stands for the listing a pass starts with.
func (e *engine) reportPurge(p Principal, ns string, err error) {
	key := "purge|" + p.String() + "|" + ns
	if err == nil || errors.Is(err, storepkg.ErrClosed) { // a pass cut off by Stop
		e.fails.ok(key)
		return
	}
	if n, report := e.fails.fail(key); report {
		e.log.Error(msgPurgeFailed, "org", p.Org, "namespace", ns, "consecutiveFailures", n, "error", err)
	}
}

// purge reads up to limit of n's run rows from where the last pass stopped,
// removes every terminal run among them whose close time plus n's retention
// is at or before now, and returns how many it removed. When the walk reaches
// the end of the rows it starts over on the next pass, and the shard, now as
// small as it will be until the next walk, gives its free pages back.
//
// An open run is never touched: each run is checked against its own row,
// under the run's lock for a workflow, before anything of it is deleted.
func (e *engine) purge(n Namespace, now time.Time, limit int) (int, error) {
	ns := n.NamespaceInfo.Name
	keep, err := retentionOf(n)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-keep)
	var at string // the last row key read; "" starts a walk
	if _, err := e.store.get(purgeKey(ns), &at); err != nil {
		return 0, err
	}
	from := at
	prefixes := runPrefixes(ns)
	phase := 0
	for i, p := range prefixes {
		if strings.HasPrefix(at, p) {
			phase = i
		}
	}
	purged, left, walked := 0, limit, false
	// A run that cannot be purged is passed over, not waited on: it is
	// reported and met again on the next walk, and the runs behind it still go.
	var first error
	for left > 0 && !walked {
		rows, last, read, err := e.page(prefixes[phase], at, left)
		if err != nil {
			return purged, err
		}
		for _, r := range rows {
			terminal := isTerminal(r.Status)
			if phase == 1 {
				terminal = isActivityTerminal(r.Status)
			}
			if !terminal || closedAt(r.CloseTime, r.StartTime).After(cutoff) {
				continue
			}
			var gone bool
			if phase == 0 {
				gone, err = e.purgeWorkflowRun(ns, r.Execution, cutoff)
			} else {
				gone, err = e.purgeActivityRun(ns, r.Execution, cutoff)
			}
			if err != nil && first == nil {
				first = fmt.Errorf("purge %s/%s: %w", r.Execution.WorkflowId, r.Execution.RunId, err)
			}
			if gone {
				purged++
			}
		}
		left -= read
		at = last
		if left > 0 { // this prefix is exhausted
			if phase++; phase == len(prefixes) {
				at, walked = "", true
			} else {
				at = ""
			}
		}
	}
	if at != from {
		if err := e.store.put(purgeKey(ns), at); err != nil {
			return purged, err
		}
	}
	if err := e.store.reclaim(ns, walked); err != nil && first == nil {
		first = err
	}
	return purged, first
}

// purgeWorkflowRun removes workflow run ref and every key of it, if its row
// is terminal and closed at or before cutoff. The row is re-read under the
// run's lock, so a run that is open, or was reopened since the page was
// read, is left alone.
func (e *engine) purgeWorkflowRun(ns string, ref ExecutionRef, cutoff time.Time) (bool, error) {
	wfID, run := ref.WorkflowId, ref.RunId
	unlock := e.lockRun(ns, wfID, run)
	defer unlock()
	wf, ok, err := e.DescribeWorkflow(ns, wfID, run)
	if err != nil || !ok || !isTerminal(wf.Status) || closedAt(wf.CloseTime, wf.StartTime).After(cutoff) {
		return false, err
	}
	var keys []string
	collect := func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	}
	for _, prefix := range []string{
		fmt.Sprintf("wfh/%s/%s/%s/", ns, wfID, run),
		fmt.Sprintf("wfact/%s/%s/%s/", ns, wfID, run),
	} {
		if err := e.store.list(prefix, collect); err != nil {
			return false, err
		}
	}
	// idem/<ns>/<workflowId>/<requestId> holds the runId a request started.
	if err := e.store.list(fmt.Sprintf("idem/%s/%s/", ns, wfID), func(key string, body []byte) error {
		var r string
		if json.Unmarshal(body, &r) == nil && r == run {
			keys = append(keys, key)
		}
		return nil
	}); err != nil {
		return false, err
	}
	// sctrig/<ns>/<scheduleId>/<requestId> holds the run a manual trigger
	// started; TriggerSchedule stamps the scheduleId on that run.
	if sid, _ := wf.SearchAttrs[searchAttrScheduleID].(string); sid != "" {
		if err := e.store.list(fmt.Sprintf("sctrig/%s/%s/", ns, sid), func(key string, body []byte) error {
			var r ExecutionRef
			if json.Unmarshal(body, &r) == nil && r.WorkflowId == wfID && r.RunId == run {
				keys = append(keys, key)
			}
			return nil
		}); err != nil {
			return false, err
		}
	}
	return true, e.delAll(append(keys, openKey(ns, wfID, run), runKey(ns, wfID, run)))
}

// purgeActivityRun removes standalone activity ref and every key of it, if
// its row is terminal and closed at or before cutoff.
func (e *engine) purgeActivityRun(ns string, ref ExecutionRef, cutoff time.Time) (bool, error) {
	id, run := ref.WorkflowId, ref.RunId
	a, ok, err := e.DescribeActivity(ns, id, run)
	if err != nil || !ok || !isActivityTerminal(a.Status) || closedAt(a.CloseTime, a.StartTime).After(cutoff) {
		return false, err
	}
	var keys []string
	if err := e.store.list(fmt.Sprintf("ahist/%s/%s/%s/", ns, id, run), func(key string, _ []byte) error {
		keys = append(keys, key)
		return nil
	}); err != nil {
		return false, err
	}
	// aidem/<ns>/<activityId>/<requestId> holds the runId a request started.
	if err := e.store.list(fmt.Sprintf("aidem/%s/%s/", ns, id), func(key string, body []byte) error {
		var r activityIdempotency
		if json.Unmarshal(body, &r) == nil && r.RunId == run {
			keys = append(keys, key)
		}
		return nil
	}); err != nil {
		return false, err
	}
	return true, e.delAll(append(keys, actKey(ns, id, run)))
}

// delAll deletes keys in order through the replicated delete path.
func (e *engine) delAll(keys []string) error {
	for _, k := range keys {
		if err := e.store.del(k); err != nil {
			return err
		}
	}
	return nil
}
