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
// Activity results are also pushed: when an activity completes, the
// dispatcher Sends OpcodeDeliverActivityResult to the peer that is
// running the parent workflow, unblocking the ExecuteActivity call.
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

	// activities[activityID] → ledger entry. Populated by
	// ScheduleActivity, completed by RespondActivity{Completed,Failed}.
	activities map[string]*pendingActivity

	// workflowPeer[ns|wfID|runID] = peerID of the worker currently
	// running the workflow execution. Activity results are pushed
	// back to this peer.
	workflowPeer map[string]string

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
	ns               string
	queue            string
	activityID       string
	activityType     string
	input            []byte
	workflowID       string
	runID            string
	workerPeer       string // peer that scheduled it (parent workflow)
	startToCloseMs   int64
	heartbeatMs      int64
	scheduledAt      time.Time
	dispatchedToPeer string // worker handling the activity (may differ from workerPeer)
}

type pendingActivity struct {
	activityID   string
	ns           string
	workflowID   string
	runID        string
	workflowPeer string // workflow's worker — receives the result push
	completed    bool
	result       []byte
	failure      []byte
}

func newDispatcher() *dispatcher {
	d := &dispatcher{
		subs:         make(map[subKey][]*subscription),
		byPeer:       make(map[string][]*subscription),
		rrIdx:        make(map[subKey]int),
		pendingWF:    make(map[subKey][]*pendingWorkflowTask),
		pendingAct:   make(map[subKey][]*pendingActivityTask),
		wfByToken:    make(map[string]*pendingWorkflowTask),
		actByToken:   make(map[string]*pendingActivityTask),
		activities:   make(map[string]*pendingActivity),
		workflowPeer: make(map[string]string),
		queries:      make(map[string]*pendingQuery),
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

// drainLocked delivers any tasks queued under sub.key to sub. Caller
// holds d.mu.
func (d *dispatcher) drainLocked(sub *subscription) {
	if sub.key.kind == kindWorkflow {
		queue := d.pendingWF[sub.key]
		if len(queue) == 0 {
			return
		}
		// Pop the head and deliver.
		t := queue[0]
		d.pendingWF[sub.key] = queue[1:]
		t.workerPeer = sub.peerID
		d.workflowPeer[wfKey(t.ns, t.workflowID, t.runID)] = sub.peerID
		body := encodeWorkflowTaskDelivery(t)
		if d.send != nil {
			_ = d.send(sub.peerID, OpcodeDeliverWorkflowTask, body)
		}
	} else {
		queue := d.pendingAct[sub.key]
		if len(queue) == 0 {
			return
		}
		t := queue[0]
		d.pendingAct[sub.key] = queue[1:]
		t.dispatchedToPeer = sub.peerID
		body := encodeActivityTaskDelivery(t)
		if d.send != nil {
			_ = d.send(sub.peerID, OpcodeDeliverActivityTask, body)
		}
	}
}

// ── task ingress ────────────────────────────────────────────────────

// EnqueueWorkflowTask creates a workflow task, mints its token, and
// either delivers it to a subscribed peer or queues it for later.
func (d *dispatcher) EnqueueWorkflowTask(ns, queue, workflowID, runID, workflowType string, input []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	t := &pendingWorkflowTask{
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
	if sub := d.pickLocked(key); sub != nil {
		t.workerPeer = sub.peerID
		d.workflowPeer[wfKey(ns, workflowID, runID)] = sub.peerID
		body := encodeWorkflowTaskDelivery(t)
		if d.send != nil {
			err := d.send(sub.peerID, OpcodeDeliverWorkflowTask, body)
			if dispatcherTrace {
				fmt.Fprintf(os.Stderr, "DISPATCH delivered_workflow peer=%s err=%v\n", sub.peerID, err)
			}
		}
		return
	}
	d.pendingWF[key] = append(d.pendingWF[key], t)
}

// ScheduleActivity records an activity for the running workflow on
// peerID, mints an activityID + token, and delivers the activity task
// to a subscribed activity peer (or queues it). The activityID is
// returned to the workflow so the worker can match the eventual
// DeliverActivityResult push.
func (d *dispatcher) ScheduleActivity(workflowPeer, ns, queue, workflowID, runID, activityType string, input []byte, startToCloseMs, heartbeatMs int64) (activityID string, token []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()

	activityID = "act-" + newRandID()
	t := &pendingActivityTask{
		ns:             ns,
		queue:          queue,
		activityID:     activityID,
		activityType:   activityType,
		input:          input,
		workflowID:     workflowID,
		runID:          runID,
		workerPeer:     workflowPeer,
		startToCloseMs: startToCloseMs,
		heartbeatMs:    heartbeatMs,
		scheduledAt:    time.Now(),
	}
	t.token = d.mintToken("act", activityID)
	t.tokenStr = string(t.token)
	d.actByToken[t.tokenStr] = t

	d.activities[activityID] = &pendingActivity{
		activityID:   activityID,
		ns:           ns,
		workflowID:   workflowID,
		runID:        runID,
		workflowPeer: workflowPeer,
	}

	key := subKey{ns, queue, kindActivity}
	if sub := d.pickLocked(key); sub != nil {
		t.dispatchedToPeer = sub.peerID
		body := encodeActivityTaskDelivery(t)
		if d.send != nil {
			_ = d.send(sub.peerID, OpcodeDeliverActivityTask, body)
		}
	} else {
		d.pendingAct[key] = append(d.pendingAct[key], t)
	}
	return activityID, t.token
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
	delete(d.workflowPeer, wfKey(t.ns, t.workflowID, t.runID))
	return t, true
}

// CompleteActivityTask consumes an activity-task token, records the
// result on the activity ledger, and pushes DeliverActivityResult to
// the peer running the parent workflow.
func (d *dispatcher) CompleteActivityTask(token []byte, result, failure []byte) (*pendingActivityTask, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.actByToken[string(token)]
	if !ok {
		return nil, false
	}
	delete(d.actByToken, string(token))

	a, exists := d.activities[t.activityID]
	if exists {
		a.completed = true
		a.result = result
		a.failure = failure
	}
	wfPeer := ""
	if exists {
		wfPeer = a.workflowPeer
	}
	if wfPeer != "" && d.send != nil {
		body := encodeActivityResultDelivery(t.activityID, result, failure)
		_ = d.send(wfPeer, OpcodeDeliverActivityResult, body)
	}
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
	Input            string `json:"input,omitempty"`
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

type activityResultDeliveryJSON struct {
	ActivityID string `json:"activity_id"`
	Result     string `json:"result,omitempty"`
	Failure    string `json:"failure,omitempty"`
}

func encodeWorkflowTaskDelivery(t *pendingWorkflowTask) []byte {
	b, _ := json.Marshal(workflowTaskDeliveryJSON{
		TaskToken:        string(t.token),
		WorkflowID:       t.workflowID,
		RunID:            t.runID,
		WorkflowTypeName: t.workflowType,
		Input:            string(t.input),
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

func encodeActivityResultDelivery(activityID string, result, failure []byte) []byte {
	b, _ := json.Marshal(activityResultDeliveryJSON{
		ActivityID: activityID,
		Result:     string(result),
		Failure:    string(failure),
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

	// Server → worker (Send).
	OpcodeDeliverWorkflowTask   uint16 = 0x00B0
	OpcodeDeliverActivityTask   uint16 = 0x00B1
	OpcodeDeliverActivityResult uint16 = 0x00B2
	OpcodeDeliverCancelRequest  uint16 = 0x00B3
	OpcodeDeliverQuery          uint16 = 0x00B4

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
}

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
	token := newRandID()
	pq := &pendingQuery{resCh: make(chan queryResponse, 1)}
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
