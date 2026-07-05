// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// dispatcher owns the worker subscription state, pending task queues,
// and activity registry. It is the single source of truth for who is
// currently executing what across all connected workers.
//
// Wire model: every task delivery is a server-initiated Send to the
// subscribed peer. Workers Subscribe once per (namespace, taskQueue,
// kind); the server pushes work as it arrives. There is no polling.
//
// Phase-2a: activities are event-sourced. An activity task is dispatched
// with a token bound to (ns, wf, run, seq); when the worker responds the
// token resolves back to that seq and the engine appends the terminal
// event to the workflow's history and schedules a new workflow task. There
// is no result push back to a "workflow peer" — the run advances by replay.
type dispatcher struct {
	mu sync.Mutex

	// send pushes opcode+body to peerID. Wired by Embed at boot to
	// the underlying zap.Node.Send. Returning an error removes the
	// failing subscription on the next operation.
	send func(peerID string, opcode uint16, body []byte) error

	// secret signs task tokens. Random per-process; tokens issued
	// before a restart are rejected (the worker simply re-subscribes).
	secret [32]byte

	// subs[(ns, queue, kind)] → ordered list of subscribers.
	subs map[subKey][]*subscription
	// byPeer[peerID] → subscriptions held by that peer.
	byPeer map[string][]*subscription
	// rrIdx[(ns,queue,kind)] → round-robin index across subscribers.
	rrIdx map[subKey]int

	// pending queues: task waiting to be claimed when a subscriber
	// arrives. FIFO per key.
	pendingWF  map[subKey][]*pendingWorkflowTask
	pendingAct map[subKey][]*pendingActivityTask

	// inflight tasks indexed by token (for Respond*).
	wfByToken  map[string]*pendingWorkflowTask
	actByToken map[string]*pendingActivityTask

	// queries[token] → pending query awaiting a worker response.
	queries map[string]*pendingQuery
}

type subKey struct {
	ns    string
	queue string
	kind  taskKind
}

type taskKind uint8

const (
	kindWorkflow taskKind = 1
	kindActivity taskKind = 2
)

type subscription struct {
	id     string
	peerID string
	key    subKey
}

type pendingWorkflowTask struct {
	token        []byte
	tokenStr     string
	orgID        string
	ns           string
	queue        string
	workflowID   string
	runID        string
	workflowType string
	input        []byte
	scheduledAt  time.Time
	workerPeer   string // set on delivery
}

type pendingActivityTask struct {
	token            []byte
	tokenStr         string
	orgID            string
	ns               string
	queue            string
	activityID       string
	activityType     string
	input            []byte
	workflowID       string
	runID            string
	seq              int // workflow command sequence number of this activity
	startToCloseMs   int64
	heartbeatMs      int64
	scheduledAt      time.Time
	dispatchedToPeer string // worker handling the activity
}

func newDispatcher() *dispatcher {
	d := &dispatcher{
		subs:       make(map[subKey][]*subscription),
		byPeer:     make(map[string][]*subscription),
		rrIdx:      make(map[subKey]int),
		pendingWF:  make(map[subKey][]*pendingWorkflowTask),
		pendingAct: make(map[subKey][]*pendingActivityTask),
		wfByToken:  make(map[string]*pendingWorkflowTask),
		actByToken: make(map[string]*pendingActivityTask),
		queries:    make(map[string]*pendingQuery),
	}
	if _, err := rand.Read(d.secret[:]); err != nil {
		// /dev/urandom never fails on supported platforms; if it does,
		// fall back to a constant — tokens still verify within this
		// process which is the only correctness invariant.
		copy(d.secret[:], []byte("tasks-fallback-secret-do-not-use"))
	}
	return d
}

// ── tokens ──────────────────────────────────────────────────────────

func (d *dispatcher) mintToken(prefix string, payload string) []byte {
	mac := hmac.New(sha256.New, d.secret[:])
	mac.Write([]byte(prefix))
	mac.Write([]byte{0})
	mac.Write([]byte(payload))
	sum := mac.Sum(nil)
	out := make([]byte, 0, len(prefix)+1+len(payload)+1+len(sum)*2)
	out = append(out, prefix...)
	out = append(out, '|')
	out = append(out, payload...)
	out = append(out, '|')
	out = append(out, []byte(hex.EncodeToString(sum))...)
	return out
}

// ── subscriptions ───────────────────────────────────────────────────

func (d *dispatcher) Subscribe(peerID, ns, queue string, kind taskKind) (string, error) {
	if peerID == "" || ns == "" || queue == "" {
		return "", fmt.Errorf("subscribe requires peerID, namespace, queue")
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	sub := &subscription{id: newRandID(), peerID: peerID, key: subKey{ns, queue, kind}}
	d.subs[sub.key] = append(d.subs[sub.key], sub)
	d.byPeer[peerID] = append(d.byPeer[peerID], sub)

	if dispatcherTrace {
		fmt.Fprintf(os.Stderr, "DISPATCH subscribe peer=%s ns=%s queue=%s kind=%d sub_id=%s subs_total=%d\n", peerID, ns, queue, kind, sub.id, len(d.subs[sub.key]))
	}
	// Drain any pending tasks for this key.
	d.drainLocked(sub)
	return sub.id, nil
}

// dispatcherTrace — flip to true via env DISPATCH_TRACE=1 (set in main).
var dispatcherTrace = os.Getenv("DISPATCH_TRACE") == "1"

func (d *dispatcher) Unsubscribe(subID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, list := range d.subs {
		for i, s := range list {
			if s.id == subID {
				d.subs[k] = append(list[:i], list[i+1:]...)
				if peerSubs, ok := d.byPeer[s.peerID]; ok {
					for j, ps := range peerSubs {
						if ps.id == subID {
							d.byPeer[s.peerID] = append(peerSubs[:j], peerSubs[j+1:]...)
							break
						}
					}
					if len(d.byPeer[s.peerID]) == 0 {
						delete(d.byPeer, s.peerID)
					}
				}
				return
			}
		}
	}
}

// RemovePeer drops every subscription held by peerID. Called when the
// underlying zap.Node observes a disconnect.
func (d *dispatcher) RemovePeer(peerID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	subs, ok := d.byPeer[peerID]
	if !ok {
		return
	}
	for _, s := range subs {
		list := d.subs[s.key]
		for i, x := range list {
			if x.id == s.id {
				d.subs[s.key] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(d.subs[s.key]) == 0 {
			delete(d.subs, s.key)
			delete(d.rrIdx, s.key)
		}
	}
	delete(d.byPeer, peerID)
}

// pickLocked returns the next subscriber for key in round-robin order,
// or nil if none subscribed. Caller holds d.mu.
func (d *dispatcher) pickLocked(key subKey) *subscription {
	list := d.subs[key]
	if len(list) == 0 {
		return nil
	}
	i := d.rrIdx[key] % len(list)
	d.rrIdx[key] = (i + 1) % len(list)
	return list[i]
}

// removeSubLocked drops a single subscription from subs+byPeer. Used to prune a
// subscription whose worker has disconnected: luxfi/zap exposes no disconnect
// hook, so a leaked subscription is only discovered when a server-push Send to
// its peer fails. Caller holds d.mu.
func (d *dispatcher) removeSubLocked(sub *subscription) {
	list := d.subs[sub.key]
	for i, x := range list {
		if x.id == sub.id {
			d.subs[sub.key] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(d.subs[sub.key]) == 0 {
		delete(d.subs, sub.key)
		delete(d.rrIdx, sub.key)
	}
	if ps, ok := d.byPeer[sub.peerID]; ok {
		for j, x := range ps {
			if x.id == sub.id {
				d.byPeer[sub.peerID] = append(ps[:j], ps[j+1:]...)
				break
			}
		}
		if len(d.byPeer[sub.peerID]) == 0 {
			delete(d.byPeer, sub.peerID)
		}
	}
}

// deliverWFLocked tries subscribers for key in round-robin order, pruning any
// whose peer the transport can no longer reach (a Send error means the worker
// disconnected and its subscription leaked — there is no zap disconnect hook),
// until one accepts the task. Returns true once delivered, false when no live
// subscriber remains (the caller then queues the task). Caller holds d.mu.
func (d *dispatcher) deliverWFLocked(key subKey, t *pendingWorkflowTask) bool {
	for {
		sub := d.pickLocked(key)
		if sub == nil {
			return false
		}
		t.workerPeer = sub.peerID
		if d.send == nil {
			return true // no transport (tests): treat as delivered
		}
		err := d.send(sub.peerID, OpcodeDeliverWorkflowTask, encodeWorkflowTaskDelivery(t))
		if dispatcherTrace {
			fmt.Fprintf(os.Stderr, "DISPATCH delivered_workflow peer=%s err=%v\n", sub.peerID, err)
		}
		if err == nil {
			return true
		}
		d.removeSubLocked(sub) // dead/absent peer — prune and try the next
	}
}

// deliverActLocked is the activity-task twin of deliverWFLocked.
func (d *dispatcher) deliverActLocked(key subKey, t *pendingActivityTask) bool {
	for {
		sub := d.pickLocked(key)
		if sub == nil {
			return false
		}
		t.dispatchedToPeer = sub.peerID
		if d.send == nil {
			return true
		}
		err := d.send(sub.peerID, OpcodeDeliverActivityTask, encodeActivityTaskDelivery(t))
		if err == nil {
			return true
		}
		d.removeSubLocked(sub)
	}
}

// drainLocked delivers any tasks queued under sub.key to sub. Caller
// holds d.mu.
func (d *dispatcher) drainLocked(sub *subscription) {
	key := sub.key
	if key.kind == kindWorkflow {
		for len(d.pendingWF[key]) > 0 {
			t := d.pendingWF[key][0]
			if !d.deliverWFLocked(key, t) {
				return // no live subscriber — leave the backlog queued
			}
			d.pendingWF[key] = d.pendingWF[key][1:]
		}
	} else {
		for len(d.pendingAct[key]) > 0 {
			t := d.pendingAct[key][0]
			if !d.deliverActLocked(key, t) {
				return
			}
			d.pendingAct[key] = d.pendingAct[key][1:]
		}
	}
}

// ── task ingress ────────────────────────────────────────────────────

// EnqueueWorkflowTask creates a workflow task, mints its token, and
// either delivers it to a subscribed peer or queues it for later.
func (d *dispatcher) EnqueueWorkflowTask(orgID, ns, queue, workflowID, runID, workflowType string, input []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	t := &pendingWorkflowTask{
		orgID:        orgID,
		ns:           ns,
		queue:        queue,
		workflowID:   workflowID,
		runID:        runID,
		workflowType: workflowType,
		input:        input,
		scheduledAt:  time.Now(),
	}
	t.token = d.mintToken("wf", workflowID+"|"+runID+"|"+newRandID())
	t.tokenStr = string(t.token)
	d.wfByToken[t.tokenStr] = t

	key := subKey{ns, queue, kindWorkflow}
	if dispatcherTrace {
		fmt.Fprintf(os.Stderr, "DISPATCH enqueue_workflow ns=%s queue=%s wf=%s subs_for_key=%d\n", ns, queue, workflowID, len(d.subs[key]))
	}
	if d.deliverWFLocked(key, t) {
		return
	}
	d.pendingWF[key] = append(d.pendingWF[key], t)
}

// DispatchActivity delivers the activity task for (ns, wf, run, seq) to a
// subscribed activity worker (or queues it until one arrives). It mints a
// fresh token bound to the task so the worker's Respond resolves back to
// (ns, wf, run, seq) via ResolveActivityToken. activityID is deterministic
// for (run, seq); the token is unique per dispatch so a re-dispatch (retry
// / recovery) does not collide with a stale token.
func (d *dispatcher) DispatchActivity(orgID, ns, queue, workflowID, runID string, seq int, activityID, activityType string, input []byte, startToCloseMs, heartbeatMs int64) []byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	t := &pendingActivityTask{
		orgID:          orgID,
		ns:             ns,
		queue:          queue,
		activityID:     activityID,
		activityType:   activityType,
		input:          input,
		workflowID:     workflowID,
		runID:          runID,
		seq:            seq,
		startToCloseMs: startToCloseMs,
		heartbeatMs:    heartbeatMs,
		scheduledAt:    time.Now(),
	}
	t.token = d.mintToken("act", activityID+"|"+newRandID())
	t.tokenStr = string(t.token)
	d.actByToken[t.tokenStr] = t

	key := subKey{ns, queue, kindActivity}
	if !d.deliverActLocked(key, t) {
		d.pendingAct[key] = append(d.pendingAct[key], t)
	}
	return t.token
}

// ── responses ───────────────────────────────────────────────────────

// CompleteWorkflowTask consumes a workflow-task token and returns
// whether the token was valid (drops the inflight record on success).
// Command application (CompleteWorkflow / FailWorkflow) is layered on
// top by the embed.go handler which mutates the WorkflowExecution
// status via the engine.
func (d *dispatcher) CompleteWorkflowTask(token []byte) (*pendingWorkflowTask, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.wfByToken[string(token)]
	if !ok {
		return nil, false
	}
	delete(d.wfByToken, string(token))
	return t, true
}

// ResolveActivityToken consumes an activity-task token and returns the
// task it was minted for, carrying (ns, wf, run, seq). The engine advances
// the run from there (append terminal event → schedule a workflow task);
// there is no result push back to a workflow peer.
func (d *dispatcher) ResolveActivityToken(token []byte) (*pendingActivityTask, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.actByToken[string(token)]
	if !ok {
		return nil, false
	}
	delete(d.actByToken, string(token))
	return t, true
}

// ── helpers ─────────────────────────────────────────────────────────

func wfKey(ns, wfID, runID string) string {
	return ns + "|" + wfID + "|" + runID
}

func newRandID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ── delivery body encoders ──────────────────────────────────────────

type workflowTaskDeliveryJSON struct {
	TaskToken        string `json:"task_token"`
	WorkflowID       string `json:"workflow_id"`
	RunID            string `json:"run_id"`
	WorkflowTypeName string `json:"workflow_type_name"`
	// History is the run's full event-sourced history (JSON array of
	// HistoryEvent) that the worker replays. The dispatcher ships whatever
	// bytes the engine enqueued; the engine builds it from the wfh/ log.
	History string `json:"history,omitempty"`
}

type activityTaskDeliveryJSON struct {
	TaskToken             string `json:"task_token"`
	WorkflowID            string `json:"workflow_id"`
	RunID                 string `json:"run_id"`
	ActivityID            string `json:"activity_id"`
	ActivityTypeName      string `json:"activity_type_name"`
	Input                 string `json:"input,omitempty"`
	ScheduledTimeMs       int64  `json:"scheduled_time_ms"`
	StartToCloseTimeoutMs int64  `json:"start_to_close_timeout_ms,omitempty"`
	HeartbeatTimeoutMs    int64  `json:"heartbeat_timeout_ms,omitempty"`
}

func encodeWorkflowTaskDelivery(t *pendingWorkflowTask) []byte {
	b, _ := json.Marshal(workflowTaskDeliveryJSON{
		TaskToken:        string(t.token),
		WorkflowID:       t.workflowID,
		RunID:            t.runID,
		WorkflowTypeName: t.workflowType,
		History:          string(t.input),
	})
	return b
}

func encodeActivityTaskDelivery(t *pendingActivityTask) []byte {
	b, _ := json.Marshal(activityTaskDeliveryJSON{
		TaskToken:             string(t.token),
		WorkflowID:            t.workflowID,
		RunID:                 t.runID,
		ActivityID:            t.activityID,
		ActivityTypeName:      t.activityType,
		Input:                 string(t.input),
		ScheduledTimeMs:       t.scheduledAt.UnixMilli(),
		StartToCloseTimeoutMs: t.startToCloseMs,
		HeartbeatTimeoutMs:    t.heartbeatMs,
	})
	return b
}

// ── server-push opcodes (declared here so engine + embed agree) ─────

const (
	// Worker → server (Call). Existing 0x00A2..0x00A5 declared in
	// pkg/sdk/client/transport.go remain authoritative.
	OpcodeSubscribeWorkflowTasks uint16 = 0x00A0
	OpcodeSubscribeActivityTasks uint16 = 0x00A1
	OpcodeUnsubscribeTasks       uint16 = 0x00A6

	// Server → worker (Send). 0x00B2 (DeliverActivityResult) is retired:
	// Phase-2a activities advance the run by replay, not a result push.
	OpcodeDeliverWorkflowTask  uint16 = 0x00B0
	OpcodeDeliverActivityTask  uint16 = 0x00B1
	OpcodeDeliverCancelRequest uint16 = 0x00B3
	OpcodeDeliverQuery         uint16 = 0x00B4

	// Worker → server (Call). Query response.
	OpcodeRespondQuery uint16 = 0x00C4
)

// ErrNoWorkersSubscribed is returned by QueryWorkflow when no worker is
// subscribed to the workflow's task queue. Callers must surface this as
// a 503-class condition; there is no engine-side fallback.
var ErrNoWorkersSubscribed = fmt.Errorf("no workers subscribed to task queue")

// ── cancel push ─────────────────────────────────────────────────────

type cancelRequestDeliveryJSON struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	Reason     string `json:"reason,omitempty"`
	Identity   string `json:"identity,omitempty"`
}

// PushCancelRequest pushes OpcodeDeliverCancelRequest to every worker
// subscribed to the workflow's task queue. Returns the number of peers
// notified. Caller holds no engine lock.
func (d *dispatcher) PushCancelRequest(ns, queue, workflowID, runID, reason, identity string) int {
	d.mu.Lock()
	subs := append([]*subscription(nil), d.subs[subKey{ns, queue, kindWorkflow}]...)
	send := d.send
	d.mu.Unlock()
	if send == nil || len(subs) == 0 {
		return 0
	}
	body, _ := json.Marshal(cancelRequestDeliveryJSON{
		Namespace:  ns,
		WorkflowID: workflowID,
		RunID:      runID,
		Reason:     reason,
		Identity:   identity,
	})
	n := 0
	for _, s := range subs {
		if err := send(s.peerID, OpcodeDeliverCancelRequest, body); err == nil {
			n++
		}
	}
	return n
}

// HasSubscribers reports whether any worker is subscribed to (ns, queue, kind).
func (d *dispatcher) HasSubscribers(ns, queue string, kind taskKind) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.subs[subKey{ns, queue, kind}]) > 0
}

// ── query push / respond ───────────────────────────────────────────

type queryDeliveryJSON struct {
	Token      string `json:"token"`
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
	QueryType  string `json:"query_type"`
	Args       []byte `json:"args,omitempty"`
}

type pendingQuery struct {
	resCh chan queryResponse
	at    time.Time
}

// maxPendingQueries caps the in-flight query map. Past this point a new
// PushQuery evicts the oldest (FIFO) so a misbehaving worker that never
// answers cannot grow the map without bound.
const maxPendingQueries = 4096

// pendingQueryTTL is the hard age cap for a pending query record. Both
// the per-call timeout (default 5s in QueryWorkflowCtx) and this sweep
// must trigger before the entry is reclaimed; the sweep is the safety
// net for callers that go away without canceling.
const pendingQueryTTL = 5 * time.Minute

type queryResponse struct {
	result []byte
	errMsg string
}

// PushQuery picks a subscribed worker for (ns, queue, kindWorkflow),
// mints a query token, and sends OpcodeDeliverQuery. Returns the token
// (used as map key for the response) and the response channel that
// CompleteQuery resolves. Returns ErrNoWorkersSubscribed if no peer.
func (d *dispatcher) PushQuery(ns, queue, workflowID, runID, queryType string, args []byte) (string, <-chan queryResponse, error) {
	d.mu.Lock()
	sub := d.pickLocked(subKey{ns, queue, kindWorkflow})
	if sub == nil {
		d.mu.Unlock()
		return "", nil, ErrNoWorkersSubscribed
	}
	if d.queries == nil {
		d.queries = make(map[string]*pendingQuery)
	}
	d.evictExpiredQueriesLocked()
	if len(d.queries) >= maxPendingQueries {
		d.evictOldestQueryLocked()
	}
	token := newRandID()
	pq := &pendingQuery{resCh: make(chan queryResponse, 1), at: time.Now()}
	d.queries[token] = pq
	send := d.send
	peer := sub.peerID
	d.mu.Unlock()

	if send == nil {
		d.mu.Lock()
		delete(d.queries, token)
		d.mu.Unlock()
		return "", nil, fmt.Errorf("dispatcher send not wired")
	}
	body, _ := json.Marshal(queryDeliveryJSON{
		Token:      token,
		Namespace:  ns,
		WorkflowID: workflowID,
		RunID:      runID,
		QueryType:  queryType,
		Args:       args,
	})
	if err := send(peer, OpcodeDeliverQuery, body); err != nil {
		d.mu.Lock()
		delete(d.queries, token)
		d.mu.Unlock()
		return "", nil, err
	}
	return token, pq.resCh, nil
}

// CompleteQuery resolves a pending query by token. Returns false if
// the token was not registered (already timed out / unknown).
func (d *dispatcher) CompleteQuery(token string, result []byte, errMsg string) bool {
	d.mu.Lock()
	pq, ok := d.queries[token]
	if ok {
		delete(d.queries, token)
	}
	d.mu.Unlock()
	if !ok {
		return false
	}
	pq.resCh <- queryResponse{result: result, errMsg: errMsg}
	return true
}

// CancelQuery drops the pending query without resolving (caller timeout).
func (d *dispatcher) CancelQuery(token string) {
	d.mu.Lock()
	delete(d.queries, token)
	d.mu.Unlock()
}

// evictExpiredQueriesLocked drops queries older than pendingQueryTTL.
// Caller holds d.mu.
func (d *dispatcher) evictExpiredQueriesLocked() {
	cutoff := time.Now().Add(-pendingQueryTTL)
	for tok, pq := range d.queries {
		if pq.at.Before(cutoff) {
			delete(d.queries, tok)
		}
	}
}

// evictOldestQueryLocked drops the single oldest pending query.
// Bounded-overflow safety net for the maxPendingQueries cap. O(n).
// Caller holds d.mu.
func (d *dispatcher) evictOldestQueryLocked() {
	var (
		oldestTok string
		oldestAt  time.Time
	)
	for tok, pq := range d.queries {
		if oldestTok == "" || pq.at.Before(oldestAt) {
			oldestTok = tok
			oldestAt = pq.at
		}
	}
	if oldestTok != "" {
		delete(d.queries, oldestTok)
	}
}
