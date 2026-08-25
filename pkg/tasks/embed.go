// Copyright © 2026 Hanzo AI. MIT License.

// Embed runs the in-process Hanzo Tasks server. One backend, two
// transports: ZAP on :9999 (canonical, native, binary) and HTTP/JSON
// (browser-only, for the embedded UI). Both go through the engine →
// store layer so they cannot drift. No gRPC. No go.temporal.io.

package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hanzoai/authz/edge"
	"github.com/hanzoai/tasks/pkg/auth"
	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/tasks/migration"
	"github.com/hanzoai/tasks/pkg/tasks/replication"
	"github.com/hanzoai/tasks/pkg/tasks/routing"
	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
	"github.com/luxfi/zap"
)

var base64Std = base64.StdEncoding

// EmbedConfig configures the in-process Tasks server.
type EmbedConfig struct {
	DataDir string // "" → "./tasks-data" (reserved; memdb today)
	// Address is where the ZAP listener binds. A filesystem path binds a
	// unix socket — the right shape for service-to-service traffic that
	// never leaves the host, and one that needs no port. Anything else is
	// a host:port TCP address; "" → ":9999".
	Address   string
	Namespace string       // "" → "default"
	Logger    *slog.Logger // nil → slog.Default()
	// MasterKey is the 32-byte root key every shard is encrypted under.
	// Each shard file gets a data-encryption key of its own, wrapped under
	// a key the shard's tenant derives from this one, so rotating the
	// master rewraps the sidecars and leaves the ciphertext untouched.
	// nil leaves shards plaintext — the zero-config dev posture. Supply it
	// from KMS in production; hold it nowhere else.
	MasterKey []byte
	// JWTValidator validates the auth_token field on every ZAP request.
	// nil = no ZAP-side validation (dev / embedded). When non-nil and
	// RequireIdentity=true, every ZAP request must carry an auth_token
	// that validates against IAM; per-request engine is scoped
	// to claims.Owner. This mirrors the HTTP middleware trust boundary.
	JWTValidator    *edge.Verifier
	RequireIdentity bool
	// Replicator wires consensus-replication for every shard. nil →
	// LocalReplicator (single-node passthrough). cmd/tasksd builds a
	// QuasarReplicator when --replicator=quasar is set.
	Replicator replication.Replicator
	// Router selects the (org, ns, taskQueue) leader. nil → solo
	// router that returns the local node for every key.
	Router routing.Router
	// NodeID is this process's stable identifier. "" → "tasks-embed".
	NodeID string
}

// Embedded is the handle to a running in-process Tasks server.
type Embedded struct {
	cfg     EmbedConfig
	nodes   []*zap.Node
	nodesMu sync.Mutex
	engine  *engine
	stop    chan struct{}
	repl    replication.Replicator
	router  routing.Router
	migr    *migration.Coordinator
}

// Embed starts the Tasks server. Stop before exit.
// sunPath is the size of sockaddr_un.sun_path, so a socket path may be at
// most sunPath-1 bytes plus its NUL. The kernel rejects a longer one with
// EADDRINUSE's less helpful cousin — bind: invalid argument — naming
// neither the path, the limit, nor the fact that a limit was involved. A
// deployment that puts its socket under a data directory sits a few nested
// directories from that ceiling, so say it here instead of leaving it to be
// discovered.
const sunPath = 108

// checkAddress refuses an address that cannot be bound, before zap tries.
// Which addresses are sockets is zap's rule (Network), asked rather than
// restated, so the two can never disagree about what a path is.
func checkAddress(addr string) error {
	if zap.Network(addr) != "unix" || len(addr) < sunPath {
		return nil
	}
	return fmt.Errorf("tasks: socket path is %d bytes, over the %d the kernel allows (sockaddr_un.sun_path is %d including its NUL): %s",
		len(addr), sunPath-1, sunPath, addr)
}

func Embed(ctx context.Context, cfg EmbedConfig) (*Embedded, error) {
	if cfg.DataDir == "" {
		// Per-process scratch directory keeps tests / multiple
		// Embed() callers from sharing state on disk. Production
		// callers (cmd/tasksd) always pass a stable DataDir.
		dir, err := os.MkdirTemp("", "tasks-data-")
		if err != nil {
			return nil, fmt.Errorf("tasks.Embed: tempdir: %w", err)
		}
		cfg.DataDir = dir
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := checkAddress(cfg.Address); err != nil {
		return nil, err
	}

	st, err := newStoreFromEnv(cfg.DataDir, cfg.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("tasks.Embed: store: %w", err)
	}
	repl := cfg.Replicator
	if repl == nil {
		repl = replication.NewLocal()
	}
	st.mgr.WithReplicator(repl)
	router := cfg.Router
	if router == nil {
		router = routing.NewHash([]byte("tasks-default"))
		nodeID := cfg.NodeID
		if nodeID == "" {
			nodeID = "tasks-embed"
		}
		router.SetMembership(routing.NodeID(nodeID), []routing.NodeID{routing.NodeID(nodeID)})
	}
	en := newEngine(st)
	// The background sweepers have no caller to return an error to, so the
	// host's logger is the only way their failures reach anyone.
	en.log = cfg.Logger
	migr := migration.NewCoordinator(st.mgr, repl)

	// Bootstrap the root tenant's default namespace so the UI has
	// something to render on first boot. Every other namespace, in every
	// other tenant, comes into existence on first use (engine.namespace).
	if _, err := en.namespace(cfg.Namespace); err != nil {
		return nil, fmt.Errorf("tasks.Embed: namespace %q: %w", cfg.Namespace, err)
	}

	node := zap.NewNode(zap.NodeConfig{
		NodeID:      "tasks-embed",
		ServiceType: "_tasks._tcp",
		Address:     cfg.Address,
		Logger:      cfg.Logger,
		NoDiscovery: true,
	})

	// e holds the engine + every ZAP listener. Build it before wiring push so the
	// dispatcher's send closure can route delivery to whichever listener (the
	// loopback one here, or a gated one added later by ServeGated) holds the
	// subscribed worker's peer.
	e := &Embedded{cfg: cfg, engine: en, repl: repl, router: router, migr: migr, nodes: []*zap.Node{node}}

	// Wire dispatcher → node.Send for server-push delivery, routed to the listener
	// that owns the worker's subscription.
	en.disp.send = func(peerID string, opcode uint16, body []byte) error {
		msg, err := wireSend(opcode, body)
		if err != nil {
			return err
		}
		for _, nd := range e.nodeSnapshot() {
			for _, p := range nd.Peers() {
				if p == peerID {
					return nd.Send(context.Background(), peerID, msg)
				}
			}
		}
		return fmt.Errorf("tasks: no listener holds worker peer %s", peerID)
	}

	for op, h := range zapHandlers(en, cfg.Namespace, cfg.JWTValidator, cfg.RequireIdentity) {
		node.Handle(op, h)
	}

	if err := node.Start(); err != nil {
		return nil, fmt.Errorf("tasks.Embed: zap start: %w", err)
	}

	// Crash-recovery: rebuild in-flight dispatch state from the durable
	// store. Re-dispatched activities and re-enqueued workflow tasks queue
	// in the dispatcher until workers (re)subscribe, then drain. Recovery
	// is idempotent — seq-keyed dispatch + replay dedup mean no lost and no
	// double execution.
	if err := en.Recover(); err != nil {
		node.Stop()
		return nil, fmt.Errorf("tasks.Embed: recover: %w", err)
	}

	stop := make(chan struct{})
	go en.runScheduler(stop)
	e.stop = stop

	return e, nil
}

// wireSend builds a ZAP message for server-initiated push: the body is
// wrapped in the same single-field envelope used by request/response
// (status=0, error=""), with the opcode stamped in the frame's flag
// high byte so the receiver dispatches on it.
func wireSend(opcode uint16, body []byte) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + 32)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, 200)
	obj.FinishAsRoot()
	flags := uint16(opcode) << 8
	frame := b.FinishWithFlags(flags)
	return zap.Parse(frame)
}

// Address returns where the loopback ZAP listener is bound.
func (e *Embedded) Address() string {
	if e == nil {
		return ""
	}
	return e.cfg.Address
}

// View is an org-scoped, in-process control-plane handle on the embedded
// engine — the door for a HOST binary (e.g. cloud's platform cron) to manage
// namespaces and schedules in a specific org's shard without a ZAP hop or a
// written identity. In-process callers share the host's trust boundary (the
// same stance as the ungated loopback listener), so the org they pass is
// authoritative — never derive it from anything client-supplied. Workflows a
// schedule starts dispatch to whatever worker is subscribed on the schedule's
// (namespace, task queue) — worker subscription is org-agnostic; results
// route back to the owning shard via the task's org.
type View struct{ en *engine }

// Principal is the tenant a store view is scoped to: an org, optionally
// narrowed to a project and a user. The zero value is the root tenant.
type Principal = storepkg.Principal

// Org returns the principal naming an org.
func Org(org string) Principal { return storepkg.Org(org) }

// View returns the control-plane view scoped to p. The zero Principal is
// the root view.
func (e *Embedded) View(p Principal) View { return View{en: e.engine.As(p)} }

// RegisterNamespace idempotently registers ns in this org's shard — required
// before any workflow (including a schedule fire) can start in it.
func (v View) RegisterNamespace(ns Namespace) error { return v.en.RegisterNamespace(ns) }

// CreateSchedule upserts a schedule (store-keyed by namespace+id).
func (v View) CreateSchedule(s Schedule) error { return v.en.CreateSchedule(s) }

// DeleteSchedule removes a schedule.
func (v View) DeleteSchedule(ns, id string) error { return v.en.DeleteSchedule(ns, id) }

// ListSchedules returns every schedule in ns.
func (v View) ListSchedules(ns string) ([]Schedule, error) { return v.en.ListSchedules(ns) }

// DescribeSchedule loads one schedule; ok=false when absent.
func (v View) DescribeSchedule(ns, id string) (*Schedule, bool, error) {
	return v.en.DescribeSchedule(ns, id)
}

// TriggerSchedule fires the schedule's action immediately (manual run).
func (v View) TriggerSchedule(ns, id, requestID string) (*WorkflowExecution, error) {
	return v.en.TriggerSchedule(ns, id, requestID)
}

// ListWorkflows returns every workflow execution in ns (this org's shard) —
// how a host observes the runs its schedules produced.
func (v View) ListWorkflows(ns string) ([]WorkflowExecution, error) {
	return v.en.ListWorkflows(ns)
}

// StartActivity enqueues a standalone activity in ns (this org's shard) — the
// in-binary seam a host subsystem uses to put work on an org queue for a fleet
// worker to claim (fn.run, studio.render), mirroring the HTTP activities API.
func (v View) StartActivity(ns, activityID, runID string, typ TypeRef, taskQueue string, input any, retry *RetryPolicy, scheduleToClose, scheduleToStart, startToClose, heartbeat string, identity, requestID string) (*StandaloneActivity, error) {
	return v.en.StartActivity(ns, activityID, runID, typ, taskQueue, input, retry, scheduleToClose, scheduleToStart, startToClose, heartbeat, identity, requestID)
}

// DescribeActivity loads one standalone activity in ns; ok=false when absent.
func (v View) DescribeActivity(ns, activityID, runID string) (*StandaloneActivity, bool, error) {
	return v.en.DescribeActivity(ns, activityID, runID)
}

// FailureStreaks returns what is broken in ns right now, worst first: one
// durable row per activity/workflow identity that has failed every attempt
// since it last succeeded. Empty means nothing is failing.
//
// This is the read that was missing. cloud's clients/cron fired a JobWorkflow
// whose activity failed on every one of 4489 fires across 11 days while the
// engine's only output was an SSE event nobody was subscribed to; the six
// nightly backups it silently skipped were found by hand-reading SQLite. A
// host polls this instead — each row carries org, namespace, workflow and
// activity type, task queue, originating scheduleId, consecutive failures,
// how long it has been failing, the last error, and a run to go read.
func (v View) FailureStreaks(ns string) ([]FailureStreak, error) {
	return v.en.FailureStreaks(ns)
}

// nodeSnapshot returns a copy of the current listener set under the lock so the
// server-push loop never races ServeGated appending a gated listener.
func (e *Embedded) nodeSnapshot() []*zap.Node {
	e.nodesMu.Lock()
	defer e.nodesMu.Unlock()
	out := make([]*zap.Node, len(e.nodes))
	copy(out, e.nodes)
	return out
}

// ServeGated starts a SECOND ZAP listener at addr that exposes the SAME
// engine to out-of-process callers under mandatory identity gating: every request
// must carry an auth_token that validates against validator (RequireIdentity), and
// its engine view is org-scoped to the token owner (CONTRACT §6). The loopback
// listener started by Embed is unchanged — in-process callers inside the host's
// trust boundary keep dialing it ungated. Server-push to a worker is routed to the
// listener that holds its subscription, so gated and loopback workers both receive
// delivery. A nil validator is refused: exposing durable execution ungated across
// the cluster is never correct. Call once, after Embed; Stop tears down every
// listener.
func (e *Embedded) ServeGated(ctx context.Context, addr string, validator *edge.Verifier) error {
	if e == nil {
		return errors.New("tasks: ServeGated on nil Embedded")
	}
	if validator == nil {
		return errors.New("tasks: ServeGated requires a validator; refusing to expose durable execution ungated")
	}
	if err := checkAddress(addr); err != nil {
		return err
	}
	n := zap.NewNode(zap.NodeConfig{
		NodeID:      "tasks-embed-gated",
		ServiceType: "_tasks._tcp",
		Address:     addr,
		Logger:      e.cfg.Logger,
		NoDiscovery: true,
	})
	for op, h := range zapHandlers(e.engine, e.cfg.Namespace, validator, true) {
		n.Handle(op, h)
	}
	if err := n.Start(); err != nil {
		return fmt.Errorf("tasks: gated zap start on %s: %w", addr, err)
	}
	e.nodesMu.Lock()
	e.nodes = append(e.nodes, n)
	e.nodesMu.Unlock()
	_ = ctx
	return nil
}

// Stop shuts the server down. Idempotent.
func (e *Embedded) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if e.stop != nil {
		close(e.stop)
		e.stop = nil
	}
	e.nodesMu.Lock()
	nodes := e.nodes
	e.nodes = nil
	e.nodesMu.Unlock()
	for _, nd := range nodes {
		nd.Stop()
	}
	if e.engine != nil && e.engine.store != nil {
		_ = e.engine.store.close()
	}
	if e.repl != nil {
		_ = e.repl.Close()
		e.repl = nil
	}
	_ = ctx
	return nil
}

// ClusterHandler exposes /v1/tasks/cluster, /v1/tasks/cluster/health,
// /v1/tasks/namespaces/{ns}/migrate. Probe routes are unauthenticated;
// migrate accepts the same X-Org-Id-scoped identity as everything else.
func (e *Embedded) ClusterHandler() http.Handler {
	mux := http.NewServeMux()
	serve := func(fn func(rq call) answer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { fn(asked(r)).write(w) }
	}
	mux.HandleFunc("/v1/tasks/cluster", serve(e.clusterStatus))
	mux.HandleFunc("/v1/tasks/cluster/health", serve(e.clusterHealth))
	mux.HandleFunc("/v1/tasks/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/tasks/namespaces/"
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/migrate") {
			absent().write(w)
			return
		}
		ns := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/migrate")
		e.migrate(asked(r), ns).write(w)
	})
	return mux
}

func (e *Embedded) clusterStatus(rq call) answer {
	if rq.method != http.MethodGet {
		return absent()
	}
	type stats struct {
		Accepted uint64 `json:"accepted"`
		Rejected uint64 `json:"rejected"`
		Timeouts uint64 `json:"timeouts,omitempty"`
	}
	resp := map[string]any{
		"nodeId":     e.cfg.NodeID,
		"replicator": replicatorKind(e.repl),
		"shardCount": shardCount(e),
		"openShards": shardCount(e),
	}
	if e.router != nil {
		vs := e.router.Validators()
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = string(v)
		}
		resp["validators"] = out
	}
	if e.repl != nil {
		a, rj, to := e.repl.Stats()
		resp["stats"] = stats{Accepted: a, Rejected: rj, Timeouts: to}
	}
	return data(nil, resp)
}

func (e *Embedded) clusterHealth(rq call) answer {
	if rq.method != http.MethodGet {
		return absent()
	}
	// Health is "in-quorum" — for the local driver always true. For
	// quasar we treat presence of validators as proof; real
	// out-of-quorum signal would come from the engine, which we don't
	// drive a heartbeat against in this build.
	if e.repl == nil {
		return answer{
			status: http.StatusServiceUnavailable,
			ctype:  ctypeJSON,
			body:   render(map[string]any{"status": "down"}),
		}
	}
	return data(nil, map[string]any{"status": "ok"})
}

// migrate is the admin-only POST /v1/tasks/namespaces/{ns}/migrate operation.
// Body: {"toNode":"<nodeId>"}. Returns the migration job. Its refusals are
// plain text, not the engine's JSON envelope.
func (e *Embedded) migrate(rq call, ns string) answer {
	if rq.method != http.MethodPost {
		return absent()
	}
	if ns == "" {
		return plain(http.StatusBadRequest, "namespace required")
	}
	var req struct {
		ToNode string `json:"toNode"`
	}
	// A stream decode, so an EMPTY body is the error "EOF" rather than the
	// zero value every other operation on this surface accepts.
	if err := rq.stream(&req); err != nil {
		return plain(http.StatusBadRequest, err.Error())
	}
	job, err := e.migr.Migrate(rq.ctx, migration.Job{
		Principal: Org(auth.OrgID(rq.ctx)).String(),
		Namespace: ns,
		To:        req.ToNode,
	})
	if err != nil {
		return plain(http.StatusInternalServerError, err.Error())
	}
	return data(nil, job)
}

func replicatorKind(r replication.Replicator) string {
	if r == nil {
		return "none"
	}
	return r.Kind()
}

func shardCount(e *Embedded) int {
	if e == nil || e.engine == nil || e.engine.store == nil || e.engine.store.mgr == nil {
		return 0
	}
	return e.engine.store.mgr.OpenShardCount()
}

// MCPHandler returns the JSON-RPC 2.0 MCP endpoint.
func (e *Embedded) MCPHandler() http.Handler { return e.mcpHandler() }

// RegisterWorker upserts a worker into the in-memory registry. Workers
// self-register on first poll/heartbeat. Real wiring will move to the
// dispatcher Subscribe path; this is the public surface the UI reads.
func (e *Embedded) RegisterWorker(w Worker) {
	if e == nil || e.engine == nil {
		return
	}
	e.engine.workers.Register(w)
}

// ActivitiesForOrg lists the standalone activities in ns for org (org=="" ⇒
// the unscoped/embedded shard). It is the thin, org-scoped programmatic read
// the in-process host (cloud's clients/fleet) uses to render a registry from
// engine data without a second HTTP hop into its own surface. Tenancy is the
// caller's to enforce: pass the validated X-Org-Id, never client input.
func (e *Embedded) ActivitiesForOrg(org, ns string) ([]StandaloneActivity, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("tasks engine not ready")
	}
	rows, _, err := e.engine.As(Org(org)).ListActivities(ns, "", 0)
	return rows, err
}

// ActivitiesPageForOrg is the PAGINATED org-scoped read: it exposes the cursor
// ActivitiesForOrg hides, so an in-process host can walk a namespace to COMPLETION
// instead of silently seeing only the first (hash-ordered) 100 rows — the truncation
// that hides live jobs and drops online workers on a busy org. cursor "" starts the
// walk; pageSize<=0 defaults to the engine's 100. Returns the next cursor ("" at the
// end). Tenancy is the caller's to enforce: pass the validated X-Org-Id.
func (e *Embedded) ActivitiesPageForOrg(org, ns, cursor string, pageSize int) ([]StandaloneActivity, string, error) {
	if e == nil || e.engine == nil {
		return nil, "", fmt.Errorf("tasks engine not ready")
	}
	return e.engine.As(Org(org)).ListActivities(ns, cursor, pageSize)
}

// CancelActivityForOrg cancels a standalone activity in org's shard — the
// org-scoped programmatic MUTATOR that mirrors ActivitiesForOrg's read, so an
// in-process host (cloud's clients/fleet) can cancel a queued/running job it just
// listed without a second HTTP hop into its own surface. Tenancy is the caller's
// to enforce: pass the validated X-Org-Id, never client input. Rejected (error)
// if the activity is missing or already terminal, exactly like the HTTP path.
func (e *Embedded) CancelActivityForOrg(org, ns, activityID, runID, reason, identity string) error {
	if e == nil || e.engine == nil {
		return fmt.Errorf("tasks engine not ready")
	}
	return e.engine.As(Org(org)).CancelActivity(ns, activityID, runID, reason, identity)
}

// EventsHandler returns the SSE realtime stream of engine events.
func (e *Embedded) EventsHandler() http.Handler { return e.sseHandler() }

// HTTPHandler returns the browser-only JSON shim. Mirrors zapHandlers.
// Per-request engine is scoped to the X-Org-Id written by pkg/auth from
// the validated IAM JWT (Authorization: Bearer). Client-supplied
// identity headers are stripped before the handler runs. Empty org →
// legacy unscoped store (embedded/dev path only).
//
// The operations themselves are in the handleX family below and hold no
// net/http type: this mux and Surface's routes both reach them, so the two
// transports answer the same bytes by construction rather than by agreement.
func (e *Embedded) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	serve := func(fn func(rq call, en *engine) answer) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rq := asked(r)
			fn(rq, e.engine.As(Org(auth.OrgID(rq.ctx)))).write(w)
		}
	}

	// /v1/tasks/settings — UI bootstrap. Unauthenticated.
	mux.HandleFunc("/v1/tasks/settings", serve(func(rq call, _ *engine) answer {
		return settings(rq)
	}))

	// /v1/tasks/namespaces
	mux.HandleFunc("/v1/tasks/namespaces", serve(namespaces))

	// /v1/tasks/nexus — cross-namespace aggregate (read-only).
	mux.HandleFunc("/v1/tasks/nexus", serve(endpoints))

	// /v1/tasks/namespaces/{ns}[/...] — matched by path segment, which is what
	// a ServeMux can express. Surface registers the same operations as routes.
	mux.HandleFunc("/v1/tasks/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		rq := asked(r)
		en := e.engine.As(Org(auth.OrgID(rq.ctx)))
		rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/namespaces/")
		below(rq, en, strings.Split(rest, "/")).write(w)
	})

	return mux
}

// asked reads one net/http request as an operation reads it. A body that
// cannot be read is carried rather than raised, so it surfaces from decode —
// where a caller has always seen it.
func asked(r *http.Request) call {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	return call{ctx: r.Context(), method: r.Method, query: r.URL.Query(), body: body, unread: err}
}

// ── per-resource routers ───────────────────────────────────────────

// below routes one path under /v1/tasks/namespaces/, already split on "/".
// parts[0] is the namespace and the rest names a resource within it.
func below(rq call, en *engine, parts []string) answer {
	ns := parts[0]
	if ns == "" {
		return absent()
	}
	if a, ok := grammar(ns, parts[1:]); !ok {
		return a
	}
	if len(parts) == 1 {
		return namespace(rq, en, ns)
	}
	return resource(rq, en, ns, parts[1], parts[2:])
}

// grammar enforces what path-injected values must satisfy before they reach a
// constructed store key, and returns the refusal when they do not.
func grammar(ns string, rest []string) (answer, bool) {
	if !validIdent(ns) {
		return fault(400, "invalid namespace"), false
	}
	// Reject any path-traversal sentinels in the resource id slot too.
	for _, p := range rest {
		if !validPathSegment(p) {
			return fault(400, "invalid path segment"), false
		}
	}
	return answer{}, true
}

// resource dispatches one namespace's sub-resource by name. Both transports
// arrive here — the mux by splitting a path, Surface by matching a route.
func resource(rq call, en *engine, ns, kind string, sub []string) answer {
	switch kind {
	case "workflows":
		return handleWorkflows(rq, en, ns, sub)
	case "schedules":
		return handleSchedules(rq, en, ns, sub)
	case "batches":
		return handleBatches(rq, en, ns, sub)
	case "deployments":
		return handleDeployments(rq, en, ns, sub)
	case "nexus":
		return handleNexus(rq, en, ns, sub)
	case "identities":
		return handleIdentities(rq, en, ns, sub)
	case "task-queues":
		return handleTaskQueues(rq, en, ns, sub)
	case "workers":
		return handleWorkers(rq, en, ns, sub)
	case "search-attributes":
		return handleSearchAttributes(rq, en, ns, sub)
	case "metadata":
		return handleNamespaceMetadata(rq, en, ns, sub)
	case "archival":
		return handleArchival(rq, en, ns, sub)
	case "activities":
		return handleActivities(rq, en, ns, sub)
	default:
		return absent()
	}
}

// settings is the UI bootstrap read at /v1/tasks/settings.
func settings(rq call) answer {
	if rq.method != http.MethodGet {
		return absent()
	}
	return data(nil, settingsResponse())
}

// namespaces lists and registers namespaces at /v1/tasks/namespaces.
func namespaces(rq call, en *engine) answer {
	switch rq.method {
	case http.MethodGet:
		rows, err := en.ListNamespaces()
		return data(err, map[string]any{"namespaces": rows})
	case http.MethodPost:
		var req struct {
			Namespace
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		err := en.RegisterNamespace(req.Namespace)
		return data(err, req.Namespace)
	default:
		return absent()
	}
}

// endpoints is the cross-namespace nexus aggregate at /v1/tasks/nexus.
func endpoints(rq call, en *engine) answer {
	if rq.method != http.MethodGet {
		return absent()
	}
	rows, err := en.ListAllNexusEndpoints()
	return data(err, map[string]any{"endpoints": rows})
}

// namespace reads or deprecates one namespace at /v1/tasks/namespaces/{ns}.
func namespace(rq call, en *engine, ns string) answer {
	switch rq.method {
	case http.MethodGet:
		n, ok, err := en.DescribeNamespace(ns)
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "namespace not found")
		}
		return data(nil, n)
	case http.MethodDelete:
		n, err := en.DeprecateNamespace(ns)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, n)
	default:
		return absent()
	}
}

func handleWorkflows(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		query := rq.query.Get("query")
		rows, err := en.ListWorkflowExecutions(ns, query)
		return data(err, map[string]any{"executions": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req struct {
			WorkflowId   string  `json:"workflowId"`
			RunId        string  `json:"runId"`
			WorkflowType TypeRef `json:"workflowType"`
			TaskQueue    struct {
				Name string `json:"name"`
			} `json:"taskQueue"`
			Input     any    `json:"input"`
			RequestId string `json:"requestId"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		wf, err := en.StartWorkflowWithRequestID(ns, req.WorkflowId, req.RunId, req.WorkflowType, req.TaskQueue.Name, req.Input, req.RequestId)
		return data(err, wf)
	case len(sub) == 1 && sub[0] == "signal-with-start" && rq.method == http.MethodPost:
		var req struct {
			WorkflowId   string  `json:"workflowId"`
			RunId        string  `json:"runId"`
			WorkflowType TypeRef `json:"workflowType"`
			TaskQueue    struct {
				Name string `json:"name"`
			} `json:"taskQueue"`
			Input         any    `json:"input"`
			SignalName    string `json:"signalName"`
			SignalPayload any    `json:"signalPayload"`
			RequestId     string `json:"requestId"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		if req.SignalName == "" {
			return fault(400, "signalName required")
		}
		wf, err := en.SignalWithStartWorkflow(ns, req.WorkflowId, req.RunId, req.WorkflowType, req.TaskQueue.Name, req.Input, req.SignalName, req.SignalPayload, req.RequestId)
		return data(err, wf)
	case len(sub) == 1 && rq.method == http.MethodGet:
		runId := rq.query.Get("execution.runId")
		if runId == "" {
			runId = rq.query.Get("runId")
		}
		wf, ok, err := en.DescribeWorkflow(ns, sub[0], runId)
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "workflow not found")
		}
		return data(nil, map[string]any{
			"workflowExecutionInfo": wf,
			"executionConfig": map[string]any{
				"taskQueue": map[string]string{"name": wf.TaskQueue},
			},
		})
	case len(sub) == 2 && sub[1] == "cancel" && rq.method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		wf, err := en.CancelWorkflowWithReason(ns, sub[0], rq.query.Get("runId"), req.Reason, req.Identity)
		return data(err, wf)
	case len(sub) == 2 && sub[1] == "terminate" && rq.method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		wf, err := en.TerminateWorkflowWithReason(ns, sub[0], rq.query.Get("runId"), req.Reason, req.Identity)
		return data(err, wf)
	case len(sub) == 2 && sub[1] == "signal" && rq.method == http.MethodPost:
		var req struct {
			Name    string `json:"name"`
			Payload any    `json:"payload"`
		}
		_ = rq.decode(&req)
		err := en.SignalWorkflow(ns, sub[0], rq.query.Get("runId"), req.Name, req.Payload)
		return data(err, map[string]string{"status": "signaled"})
	case len(sub) == 2 && sub[1] == "history" && rq.method == http.MethodGet:
		runId := rq.query.Get("runId")
		afterID := parseInt64(rq.query.Get("after"))
		pageSize := int(parseInt64(rq.query.Get("pageSize")))
		reverse := rq.query.Get("reverse") == "true"
		events, next, err := en.GetWorkflowHistory(ns, sub[0], runId, afterID, pageSize, reverse)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, map[string]any{"events": events, "nextCursor": next})
	case len(sub) == 2 && sub[1] == "query" && rq.method == http.MethodPost:
		var req struct {
			QueryType string `json:"queryType"`
			Args      any    `json:"args"`
		}
		_ = rq.decode(&req)
		runId := rq.query.Get("runId")
		out, err := en.QueryWorkflowCtx(rq.ctx, ns, sub[0], runId, req.QueryType, req.Args)
		if err == ErrNoWorkersSubscribed {
			return fault(503, err.Error())
		}
		if err != nil && strings.Contains(err.Error(), "timeout") {
			return fault(504, err.Error())
		}
		return data(err, map[string]any{"queryResult": out})
	case len(sub) == 2 && sub[1] == "metadata" && rq.method == http.MethodPost:
		var req WorkflowUserMetadata
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		runId := rq.query.Get("runId")
		wf, err := en.UpdateWorkflowMetadata(ns, sub[0], runId, req)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, wf)
	case len(sub) == 2 && sub[1] == "executions" && rq.method == http.MethodGet:
		rows, err := en.ListWorkflowChain(ns, sub[0])
		return data(err, map[string]any{"executions": rows})
	case len(sub) == 2 && sub[1] == "reset" && rq.method == http.MethodPost:
		var req struct {
			RunId    string `json:"runId"`
			EventId  int64  `json:"eventId"`
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		wf, err := en.ResetWorkflow(ns, sub[0], req.RunId, req.EventId, req.Reason, req.Identity)
		return data(err, wf)
	default:
		return absent()
	}
}

// validIdent enforces the namespace / id grammar for path-injected
// values. Rejects empty, length > 64, and anything outside
// [A-Za-z0-9_.-]. Used as the path-traversal trust boundary so
// constructed store keys (e.g. "wf/<ns>/<wfid>/<runid>") cannot escape
// their key family. Reserved sentinels ("." / "..") are rejected.
func validIdent(s string) bool {
	if s == "" || len(s) > 64 || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			continue
		}
		if c == '_' || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// validPathSegment is a permissive segment check that still bars the
// classic traversal characters (NUL, '/', '..'). Used for non-leading
// path slots whose grammar isn't strictly an ident (signal names etc).
func validPathSegment(s string) bool {
	if s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0 || c == '/' {
			return false
		}
	}
	return true
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// handleTaskQueues derives queues from listed workflows. Honest: there
// is no separate "task queue" object in storage yet; queues live as
// strings on workflows. Aggregating them here is cheap and matches what
// the upstream UI shows on first paint.
func handleTaskQueues(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListWorkflows(ns)
		if err != nil {
			return fault(500, err.Error())
		}
		return data(nil, map[string]any{"taskQueues": aggregateTaskQueues(rows)})
	case len(sub) == 1 && rq.method == http.MethodGet:
		rows, err := en.ListWorkflows(ns)
		if err != nil {
			return fault(500, err.Error())
		}
		return data(nil, taskQueueDetail(rows, sub[0]))
	case len(sub) == 2 && sub[1] == "workers" && rq.method == http.MethodGet:
		// Filter the registry by task queue name (sub[0]).
		all := en.workers.List(ns)
		out := make([]Worker, 0, len(all))
		for _, w := range all {
			if w.TaskQueue == sub[0] {
				out = append(out, w)
			}
		}
		return data(nil, map[string]any{"workers": out})
	case len(sub) == 2 && sub[1] == "partitions" && rq.method == http.MethodGet:
		// Placeholder for the real partition manager. The engine does
		// not shard task queues yet, so every queue reports as a single
		// partition with zero backlog.
		return data(nil, map[string]any{
			"partitions": []map[string]any{{
				"key":         0,
				"backlogAge":  "0s",
				"backlogSize": 0,
			}},
		})
	default:
		return absent()
	}
}

// handleWorkers — namespace-wide worker listing. Stub registry: workers
// self-register via Embedded.RegisterWorker on first poll/heartbeat.
func handleWorkers(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		return data(nil, map[string]any{"workers": en.workers.List(ns)})
	case len(sub) == 1 && rq.method == http.MethodGet:
		wk, ok := en.workers.Get(ns, sub[0])
		if !ok {
			return fault(404, "worker not found")
		}
		return data(nil, wk)
	default:
		return absent()
	}
}

// handleArchival — archival is disabled. UI renders the documented
// "disabled" body. When archival lands, switch to engine.QueryArchival
// with cursor pagination.
func handleArchival(rq call, en *engine, ns string, sub []string) answer {
	if len(sub) != 0 || rq.method != http.MethodGet {
		return absent()
	}
	body, err := en.QueryArchival(ns, rq.query.Get("query"), rq.query.Get("nextPageToken"))
	return data(err, body)
}

// settingsResponse builds the UI bootstrap response.
func settingsResponse() map[string]any {
	return map[string]any{
		"version":                   tasksVersion(),
		"namespaceWriteDisabled":    envBool("TASKSD_NAMESPACE_WRITE_DISABLED", false),
		"archivalEnabled":           envBool("TASKSD_ARCHIVAL_ENABLED", false),
		"visibilityArchivalEnabled": envBool("TASKSD_VISIBILITY_ARCHIVAL_ENABLED", false),
		"advancedVisibilityEnabled": envBool("TASKSD_ADVANCED_VISIBILITY_ENABLED", true),
		"workerHeartbeatsEnabled":   envBool("TASKSD_WORKER_HEARTBEATS_ENABLED", true),
		"codecEndpointsEnabled":     envBool("TASKSD_CODEC_ENDPOINTS_ENABLED", false),
		"multiClusterEnabled":       envBool("TASKSD_MULTI_CLUSTER_ENABLED", false),
		"capabilities": map[string]any{
			"signalAndQueryHeader":            true,
			"internalErrorDifferentiation":    true,
			"activityFailureIncludeHeartbeat": true,
			"supportsSchedules":               true,
			"encodedFailureAttributes":        true,
			"upsertMemo":                      true,
			"eagerWorkflowStart":              false,
			"sdkMetadata":                     true,
			"countGroupByExecutionStatus":     true,
		},
	}
}

func tasksVersion() string {
	if v := os.Getenv("TASKSD_VERSION"); v != "" {
		return v
	}
	return "v3.5.0"
}

func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

// parseTimeParam returns the first non-empty RFC3339 value, falling back
// to def. Empty / unparseable inputs return def.
func parseTimeParam(a, b string, def time.Time) time.Time {
	for _, s := range []string{a, b} {
		if s == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return def
}

type taskQueueSummary struct {
	Name        string `json:"name"`
	Workflows   int    `json:"workflows"`
	Running     int    `json:"running"`
	LatestStart string `json:"latestStart,omitempty"`
}

func aggregateTaskQueues(rows []WorkflowExecution) []taskQueueSummary {
	by := map[string]*taskQueueSummary{}
	for i := range rows {
		wf := &rows[i]
		q := wf.TaskQueue
		if q == "" {
			q = "default"
		}
		s, ok := by[q]
		if !ok {
			s = &taskQueueSummary{Name: q}
			by[q] = s
		}
		s.Workflows++
		if wf.Status == "WORKFLOW_EXECUTION_STATUS_RUNNING" {
			s.Running++
		}
		if wf.StartTime > s.LatestStart {
			s.LatestStart = wf.StartTime
		}
	}
	out := make([]taskQueueSummary, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	return out
}

func taskQueueDetail(rows []WorkflowExecution, queue string) map[string]any {
	matches := make([]WorkflowExecution, 0, len(rows))
	for i := range rows {
		q := rows[i].TaskQueue
		if q == "" {
			q = "default"
		}
		if q == queue {
			matches = append(matches, rows[i])
		}
	}
	running := 0
	for i := range matches {
		if matches[i].Status == "WORKFLOW_EXECUTION_STATUS_RUNNING" {
			running++
		}
	}
	return map[string]any{
		"name":      queue,
		"workflows": matches,
		"running":   running,
		"total":     len(matches),
	}
}

func handleSchedules(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListSchedules(ns)
		return data(err, map[string]any{"schedules": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req Schedule
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		req.Namespace = ns
		err := en.CreateSchedule(req)
		return data(err, req)
	case len(sub) == 1 && rq.method == http.MethodGet:
		s, ok, err := en.DescribeSchedule(ns, sub[0])
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "schedule not found")
		}
		return data(nil, s)
	case len(sub) == 1 && rq.method == http.MethodPost:
		var req Schedule
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		out, err := en.UpdateSchedule(ns, sub[0], req)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, out)
	case len(sub) == 1 && rq.method == http.MethodDelete:
		err := en.DeleteSchedule(ns, sub[0])
		return data(err, map[string]string{"status": "deleted"})
	case len(sub) == 2 && sub[1] == "trigger" && rq.method == http.MethodPost:
		var req struct {
			RequestId     string `json:"requestId"`
			OverlapPolicy string `json:"overlapPolicy"`
		}
		_ = rq.decode(&req)
		wf, err := en.TriggerSchedule(ns, sub[0], req.RequestId)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, map[string]any{"status": "triggered", "execution": wf})
	case len(sub) == 2 && sub[1] == "matching-times" && rq.method == http.MethodGet:
		from := parseTimeParam(rq.query.Get("from"), rq.query.Get("start"), time.Now().UTC())
		to := parseTimeParam(rq.query.Get("to"), rq.query.Get("end"), from.Add(24*time.Hour))
		times, err := en.ScheduleMatchingTimes(ns, sub[0], from, to)
		if err != nil {
			return fault(404, err.Error())
		}
		out := make([]string, len(times))
		for i, t := range times {
			out[i] = t.UTC().Format(time.RFC3339)
		}
		return data(nil, map[string]any{"matchingTimes": out})
	case len(sub) == 2 && sub[1] == "pause" && rq.method == http.MethodPost:
		var req struct {
			Note string `json:"note"`
		}
		_ = rq.decode(&req)
		err := en.PauseSchedule(ns, sub[0], true, req.Note)
		return data(err, map[string]string{"status": "paused"})
	case len(sub) == 2 && sub[1] == "unpause" && rq.method == http.MethodPost:
		err := en.PauseSchedule(ns, sub[0], false, "")
		return data(err, map[string]string{"status": "running"})
	default:
		return absent()
	}
}

func handleBatches(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListBatches(ns)
		return data(err, map[string]any{"batches": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req BatchOperation
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		req.Namespace = ns
		b, err := en.StartBatch(req)
		return data(err, b)
	case len(sub) == 1 && rq.method == http.MethodGet:
		b, ok, err := en.DescribeBatch(ns, sub[0])
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "batch not found")
		}
		return data(nil, b)
	case len(sub) == 2 && sub[1] == "terminate" && rq.method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		b, err := en.TerminateBatch(ns, sub[0], req.Reason, req.Identity)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, b)
	default:
		return absent()
	}
}

// handleSearchAttributes — POST adds, GET lists, DELETE removes by name.
func handleSearchAttributes(rq call, en *engine, ns string, sub []string) answer {
	if len(sub) == 1 && rq.method == http.MethodDelete {
		if err := en.RemoveSearchAttribute(ns, sub[0]); err != nil {
			if strings.Contains(err.Error(), "not registered") {
				return fault(404, err.Error())
			}
			return fault(400, err.Error())
		}
		return data(nil, map[string]string{"status": "removed"})
	}
	if len(sub) != 0 {
		return absent()
	}
	switch rq.method {
	case http.MethodGet:
		rows, err := en.ListSearchAttributes(ns)
		return data(err, map[string]any{"searchAttributes": rows})
	case http.MethodPost:
		var req SearchAttribute
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		if err := en.AddSearchAttribute(ns, req); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				return fault(409, err.Error())
			}
			if strings.Contains(err.Error(), "not registered") {
				return fault(404, err.Error())
			}
			return fault(400, err.Error())
		}
		return data(nil, req)
	default:
		return absent()
	}
}

// handleNamespaceMetadata — POST patches namespace metadata.
func handleNamespaceMetadata(rq call, en *engine, ns string, sub []string) answer {
	if len(sub) != 0 || rq.method != http.MethodPost {
		return absent()
	}
	var req NamespaceMetadataPatch
	if err := rq.decode(&req); err != nil {
		return fault(400, err.Error())
	}
	n, err := en.UpdateNamespaceMetadata(ns, req)
	if err != nil {
		if strings.Contains(err.Error(), "not registered") {
			return fault(404, err.Error())
		}
		return fault(400, err.Error())
	}
	return data(nil, n)
}

func handleDeployments(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListDeployments(ns)
		return data(err, map[string]any{"deployments": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req struct {
			Name           string `json:"name"`
			Description    string `json:"description"`
			OwnerEmail     string `json:"ownerEmail"`
			DefaultCompute string `json:"defaultCompute"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		d, err := en.CreateDeployment(ns, req.Name, req.Description, req.OwnerEmail, req.DefaultCompute)
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				return fault(409, err.Error())
			}
			return fault(400, err.Error())
		}
		return data(nil, d)
	case len(sub) == 1 && rq.method == http.MethodGet:
		d, ok, err := en.DescribeDeployment(ns, sub[0])
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "deployment not found")
		}
		return data(nil, d)
	case len(sub) == 1 && rq.method == http.MethodPost:
		var req DeploymentPatch
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		d, err := en.UpdateDeployment(ns, sub[0], req)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, d)
	case len(sub) == 1 && rq.method == http.MethodDelete:
		force := rq.query.Get("force") == "true"
		err := en.DeleteDeployment(ns, sub[0], force)
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return fault(404, err.Error())
			}
			return fault(409, err.Error())
		}
		return data(nil, map[string]string{"status": "deleted"})
	case len(sub) == 2 && sub[1] == "set-current" && rq.method == http.MethodPost:
		var req struct {
			BuildId string `json:"buildId"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		d, err := en.SetCurrentDeploymentVersion(ns, sub[0], req.BuildId)
		if err != nil {
			if strings.Contains(err.Error(), "not in deployment") {
				return fault(400, err.Error())
			}
			return fault(404, err.Error())
		}
		return data(nil, d)
	case len(sub) == 2 && sub[1] == "versions" && rq.method == http.MethodPost:
		var req struct {
			BuildId     string            `json:"buildId"`
			Description string            `json:"description"`
			Compute     string            `json:"compute"`
			Image       string            `json:"image"`
			Env         map[string]string `json:"env"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		v, err := en.CreateVersion(ns, sub[0], req.BuildId, req.Description, req.Compute, req.Image, req.Env)
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				return fault(409, err.Error())
			}
			if strings.Contains(err.Error(), "not found") {
				return fault(404, err.Error())
			}
			return fault(400, err.Error())
		}
		return data(nil, v)
	case len(sub) == 3 && sub[1] == "versions" && rq.method == http.MethodPost:
		var req DeploymentVersionPatch
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		v, err := en.UpdateVersion(ns, sub[0], sub[2], req)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, v)
	case len(sub) == 3 && sub[1] == "versions" && rq.method == http.MethodDelete:
		d, err := en.DeleteDeploymentVersion(ns, sub[0], sub[2])
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, d)
	case len(sub) == 4 && sub[1] == "versions" && sub[3] == "validate" && rq.method == http.MethodPost:
		res, err := en.ValidateVersion(ns, sub[0], sub[2])
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, res)
	default:
		return absent()
	}
}

// handleActivities — standalone activities engine.
func handleActivities(rq call, en *engine, ns string, sub []string) answer {
	for _, s := range sub {
		if s == "" {
			return absent()
		}
	}
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		cursor := rq.query.Get("cursor")
		pageSize := int(parseInt64(rq.query.Get("pageSize")))
		rows, next, err := en.ListActivities(ns, cursor, pageSize)
		return data(err, map[string]any{"activities": rows, "nextCursor": next})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req struct {
			ActivityId             string       `json:"activityId"`
			RunId                  string       `json:"runId"`
			ActivityType           TypeRef      `json:"activityType"`
			TaskQueue              string       `json:"taskQueue"`
			Input                  any          `json:"input"`
			RetryPolicy            *RetryPolicy `json:"retryPolicy"`
			ScheduleToCloseTimeout string       `json:"scheduleToCloseTimeout"`
			ScheduleToStartTimeout string       `json:"scheduleToStartTimeout"`
			StartToCloseTimeout    string       `json:"startToCloseTimeout"`
			HeartbeatTimeout       string       `json:"heartbeatTimeout"`
			Identity               string       `json:"identity"`
			RequestId              string       `json:"requestId"`
		}
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		a, err := en.StartActivity(ns, req.ActivityId, req.RunId, req.ActivityType, req.TaskQueue, req.Input, req.RetryPolicy, req.ScheduleToCloseTimeout, req.ScheduleToStartTimeout, req.StartToCloseTimeout, req.HeartbeatTimeout, req.Identity, req.RequestId)
		if err != nil {
			return fault(400, err.Error())
		}
		return data(nil, a)
	case len(sub) == 1 && sub[0] == "claim" && rq.method == http.MethodPost:
		var req struct {
			TaskQueue    string `json:"taskQueue"`
			Identity     string `json:"identity"`
			LeaseSeconds int    `json:"leaseSeconds"`
		}
		_ = rq.decode(&req)
		a, ok, err := en.ClaimNextActivity(ns, req.TaskQueue, req.Identity, time.Duration(req.LeaseSeconds)*time.Second)
		if err != nil {
			return fault(400, err.Error())
		}
		if !ok {
			return empty(http.StatusNoContent)
		}
		return data(nil, a)
	case len(sub) == 2 && rq.method == http.MethodGet:
		a, ok, err := en.DescribeActivity(ns, sub[0], sub[1])
		if err != nil {
			return fault(500, err.Error())
		}
		if !ok {
			return fault(404, "activity not found")
		}
		return data(nil, a)
	case len(sub) == 3 && sub[2] == "cancel" && rq.method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		err := en.CancelActivity(ns, sub[0], sub[1], req.Reason, req.Identity)
		return activityResult(en, ns, sub[0], sub[1], err)
	case len(sub) == 3 && sub[2] == "complete" && rq.method == http.MethodPost:
		var req struct {
			Result   any    `json:"result"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		err := en.CompleteActivity(ns, sub[0], sub[1], req.Result, req.Identity)
		return activityResult(en, ns, sub[0], sub[1], err)
	case len(sub) == 3 && sub[2] == "fail" && rq.method == http.MethodPost:
		var req struct {
			Cause    string `json:"cause"`
			Identity string `json:"identity"`
		}
		_ = rq.decode(&req)
		err := en.FailActivity(ns, sub[0], sub[1], req.Cause, req.Identity)
		return activityResult(en, ns, sub[0], sub[1], err)
	case len(sub) == 3 && sub[2] == "heartbeat" && rq.method == http.MethodPost:
		var req struct {
			Details any `json:"details"`
		}
		_ = rq.decode(&req)
		err := en.HeartbeatActivity(ns, sub[0], sub[1], req.Details)
		return activityResult(en, ns, sub[0], sub[1], err)
	case len(sub) == 3 && sub[2] == "history" && rq.method == http.MethodGet:
		afterID := parseInt64(rq.query.Get("after"))
		pageSize := int(parseInt64(rq.query.Get("pageSize")))
		reverse := rq.query.Get("reverse") == "true"
		events, next, err := en.GetActivityHistory(ns, sub[0], sub[1], afterID, pageSize, reverse)
		if err != nil {
			return fault(404, err.Error())
		}
		return data(nil, map[string]any{"events": events, "nextCursor": next})
	default:
		return absent()
	}
}

// activityResult maps engine errors onto statuses for the activity lifecycle
// operations and answers with the refreshed activity.
func activityResult(en *engine, ns, activityID, runID string, err error) answer {
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return fault(404, err.Error())
		}
		if strings.Contains(err.Error(), "terminal") {
			return fault(409, err.Error())
		}
		return fault(400, err.Error())
	}
	a, _, _ := en.DescribeActivity(ns, activityID, runID)
	return data(nil, a)
}

func handleNexus(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListNexusEndpoints(ns)
		return data(err, map[string]any{"endpoints": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req NexusEndpoint
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		req.Namespace = ns
		err := en.CreateNexusEndpoint(req)
		return data(err, req)
	case len(sub) == 1 && rq.method == http.MethodDelete:
		err := en.DeleteNexusEndpoint(ns, sub[0])
		return data(err, map[string]string{"status": "deleted"})
	default:
		return absent()
	}
}

func handleIdentities(rq call, en *engine, ns string, sub []string) answer {
	switch {
	case len(sub) == 0 && rq.method == http.MethodGet:
		rows, err := en.ListIdentities(ns)
		return data(err, map[string]any{"identities": rows})
	case len(sub) == 0 && rq.method == http.MethodPost:
		var req Identity
		if err := rq.decode(&req); err != nil {
			return fault(400, err.Error())
		}
		req.Namespace = ns
		err := en.GrantIdentity(req)
		return data(err, req)
	case len(sub) == 1 && rq.method == http.MethodDelete:
		err := en.RevokeIdentity(ns, sub[0])
		return data(err, map[string]string{"status": "revoked"})
	default:
		return absent()
	}
}

// ── ZAP handler dispatch ────────────────────────────────────────────

const (
	opStartWorkflow     uint16 = 0x0060
	opSignalWorkflow    uint16 = 0x0061
	opCancelWorkflow    uint16 = 0x0062
	opTerminateWorkflow uint16 = 0x0063
	opDescribeWorkflow  uint16 = 0x0064
	opListWorkflows     uint16 = 0x0065
	// SignalWithStart is 0x0066 on the canonical wire (pkg/sdk/client +
	// TS opcodes + schema/tasks.zap). The server previously registered it
	// at 0x0069 and squatted 0x0066 with GetWorkflowHistory, so every
	// client's signalWithStart 404'd — social's digest/sendEmail/poke all
	// depend on it. GetWorkflowHistory has no ZAP client (HTTP-served), so
	// it takes the now-free 0x0069.
	opSignalWithStartWorkflow uint16 = 0x0066
	opQueryWorkflow           uint16 = 0x0067
	opResetWorkflow           uint16 = 0x0068
	opGetWorkflowHistory      uint16 = 0x0069
	opCreateSchedule          uint16 = 0x0070
	opDeleteSchedule          uint16 = 0x0071
	opListSchedules           uint16 = 0x0072
	opPauseSchedule           uint16 = 0x0073
	opUnpauseSchedule         uint16 = 0x0074
	opDescribeSchedule        uint16 = 0x0076
	opRegisterNamespace       uint16 = 0x0080
	opDescribeNamespace       uint16 = 0x0081
	opListNamespaces          uint16 = 0x0082
	opHealth                  uint16 = 0x0090
)

func zapHandlers(rootEn *engine, defaultNS string, validator *edge.Verifier, requireID bool) map[uint16]zap.Handler {
	envBody := func(v any) ([]byte, error) { return json.Marshal(v) }
	// scope validates the request's auth_token and returns the
	// org-scoped engine view. orgErr non-empty → caller authoritatively
	// failed auth and wrap should return a 401 envelope without dispatching.
	scope := func(ctx context.Context, req map[string]any) (en *engine, status uint32, errMsg string) {
		if validator == nil {
			// Dev / embedded path. No ZAP-side validation.
			return rootEn, 0, ""
		}
		tok, _ := req["auth_token"].(string)
		if tok == "" {
			if requireID {
				return nil, 401, "auth_token required"
			}
			return rootEn, 0, ""
		}
		claims, err := validator.VerifyRaw(strings.TrimPrefix(tok, "Bearer "))
		// EffectiveOrg, not the raw `owner` claim: it is the ONE function that answers
		// "which org does this request act in", and the ZAP path must answer it the same
		// way the HTTP one does. With no selection to honour — this transport carries no
		// org-switch — it resolves the home org, which is what `owner` meant here.
		var org string
		if err == nil && claims != nil {
			org, _ = claims.EffectiveOrg("")
		}
		if org == "" {
			if requireID {
				return nil, 401, "invalid auth_token"
			}
			return rootEn, 0, ""
		}
		return rootEn.As(Org(org)), 0, ""
	}
	wrap := func(fn func(en *engine, req map[string]any) (any, uint32, string)) zap.Handler {
		return func(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
			req := map[string]any{}
			if msg != nil {
				root := msg.Root()
				if !root.IsNull() {
					if rb := root.Bytes(envelopeBody); len(rb) > 0 {
						_ = json.Unmarshal(rb, &req)
					}
				}
			}
			en, st, em := scope(ctx, req)
			if em != "" {
				return envelope(nil, st, em)
			}
			out, status, errMsg := fn(en, req)
			body, _ := envBody(out)
			return envelope(body, status, errMsg)
		}
	}
	// wrapPeer exposes the caller's peerID to the handler so subscription
	// state can be keyed off it. Used by Subscribe / Schedule / Respond.
	wrapPeer := func(fn func(en *engine, from string, req map[string]any) (any, uint32, string)) zap.Handler {
		return func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
			req := map[string]any{}
			if msg != nil {
				root := msg.Root()
				if !root.IsNull() {
					if rb := root.Bytes(envelopeBody); len(rb) > 0 {
						_ = json.Unmarshal(rb, &req)
					}
				}
			}
			en, st, em := scope(ctx, req)
			if em != "" {
				return envelope(nil, st, em)
			}
			out, status, errMsg := fn(en, from, req)
			body, _ := envBody(out)
			return envelope(body, status, errMsg)
		}
	}
	str := func(req map[string]any, k string) string {
		if v, ok := req[k].(string); ok {
			return v
		}
		return ""
	}
	strOr := func(req map[string]any, k, def string) string {
		if v, ok := req[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	// scheduleSDK marshals an engine Schedule into the SDK shape.
	scheduleSDK := func(s *Schedule) map[string]any {
		return map[string]any{
			"id": s.ScheduleId,
			"spec": map[string]any{
				"cron": s.Spec.CronString,
			},
			"action": map[string]any{
				"workflow_type": s.Action.WorkflowType.Name,
				"task_queue":    s.Action.TaskQueue,
			},
			"paused": s.State.Paused,
		}
	}
	_ = scheduleSDK
	// wfInfoSDK marshals an engine WorkflowExecution into the
	// snake_case SDK shape (WorkflowExecutionInfo) expected over ZAP.
	wfInfoSDK := func(wf *WorkflowExecution) map[string]any {
		statusInt := 0
		switch wf.Status {
		case "WORKFLOW_EXECUTION_STATUS_RUNNING":
			statusInt = 1
		case "WORKFLOW_EXECUTION_STATUS_COMPLETED":
			statusInt = 2
		case "WORKFLOW_EXECUTION_STATUS_FAILED":
			statusInt = 3
		case "WORKFLOW_EXECUTION_STATUS_CANCELED":
			statusInt = 4
		case "WORKFLOW_EXECUTION_STATUS_TERMINATED":
			statusInt = 5
		case "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW":
			statusInt = 6
		}
		out := map[string]any{
			"workflow_id":    wf.Execution.WorkflowId,
			"run_id":         wf.Execution.RunId,
			"workflow_type":  wf.Type.Name,
			"status":         statusInt,
			"history_length": wf.HistoryLen,
			"task_queue":     wf.TaskQueue,
		}
		// Empty time strings won't parse with omitempty time.Time on the
		// SDK side; only include when present.
		if wf.StartTime != "" {
			out["start_time"] = wf.StartTime
		}
		if wf.CloseTime != "" {
			out["close_time"] = wf.CloseTime
		}
		return out
	}
	return map[uint16]zap.Handler{
		opHealth: wrap(func(_ *engine, _ map[string]any) (any, uint32, string) {
			return map[string]any{"service": "tasks", "status": "ok", "namespace": defaultNS}, 200, ""
		}),
		opListNamespaces: wrap(func(en *engine, _ map[string]any) (any, uint32, string) {
			rows, err := en.ListNamespaces()
			if err != nil {
				return nil, 500, err.Error()
			}
			return map[string]any{"namespaces": rows}, 200, ""
		}),
		opDescribeNamespace: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			n, ok, err := en.DescribeNamespace(str(req, "namespace"))
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "namespace not found"
			}
			return n, 200, ""
		}),
		opRegisterNamespace: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			var ns Namespace
			if b, _ := json.Marshal(req); b != nil {
				_ = json.Unmarshal(b, &ns)
			}
			// The SDK client sends the flat registerNamespaceWire shape
			// ({"name","description","owner_email"}); map it onto NamespaceInfo
			// when the nested {"namespaceInfo":{...}} form is absent so an
			// org-scoped caller can register the namespace ExecuteWorkflow requires.
			if ns.NamespaceInfo.Name == "" {
				ns.NamespaceInfo.Name = strOr(req, "name", "")
				ns.NamespaceInfo.Description = strOr(req, "description", "")
				ns.NamespaceInfo.OwnerEmail = strOr(req, "owner_email", "")
			}
			if err := en.RegisterNamespace(ns); err != nil {
				return nil, 400, err.Error()
			}
			return ns, 200, ""
		}),
		opStartWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			typeName := strOr(req, "workflow_type", "")
			if typeName == "" {
				if t, ok := req["workflowType"].(map[string]any); ok {
					typeName, _ = t["name"].(string)
				}
			}
			tq := strOr(req, "task_queue", "")
			if tq == "" {
				if q, ok := req["taskQueue"].(map[string]any); ok {
					tq, _ = q["name"].(string)
				}
			}
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			reqID := strOr(req, "request_id", "")
			sa, _ := req["search_attributes"].(map[string]any)
			policy := strOr(req, "workflow_id_conflict_policy", str(req, "workflowIdConflictPolicy"))
			wf, err := en.startWorkflowFull(ns, wfID, runID, TypeRef{Name: typeName}, tq, req["input"], reqID, sa, req["memo"], policy)
			if err != nil {
				return nil, 400, err.Error()
			}
			// SDK expects {run_id} response shape.
			return map[string]any{"run_id": wf.Execution.RunId, "workflow_id": wf.Execution.WorkflowId}, 200, ""
		}),
		opListWorkflows: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			rows, err := en.ListWorkflows(ns)
			if err != nil {
				return nil, 500, err.Error()
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				out = append(out, wfInfoSDK(&rows[i]))
			}
			return map[string]any{"executions": out}, 200, ""
		}),
		opDescribeWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, ok, err := en.DescribeWorkflow(ns, wfID, runID)
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "workflow not found"
			}
			return map[string]any{"info": wfInfoSDK(wf)}, 200, ""
		}),
		opSignalWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			sigName := strOr(req, "signal_name", str(req, "signalName"))
			err := en.SignalWorkflow(ns, wfID, runID, sigName, req["input"])
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "signaled"}, 200, ""
		}),
		opCancelWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, err := en.CancelWorkflow(ns, wfID, runID)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opTerminateWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			reason := strOr(req, "reason", "")
			identity := strOr(req, "identity", "")
			wf, err := en.TerminateWorkflowWithReason(ns, wfID, runID, reason, identity)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opGetWorkflowHistory: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			after := int64Field(req, "after_event_id")
			page := int(int64Field(req, "page_size"))
			reverse := false
			if v, ok := req["reverse"].(bool); ok {
				reverse = v
			}
			events, next, err := en.GetWorkflowHistory(ns, wfID, runID, after, page, reverse)
			if err != nil {
				return nil, 404, err.Error()
			}
			return map[string]any{"events": events, "next_cursor": next}, 200, ""
		}),
		opQueryWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			qType := strOr(req, "query_type", str(req, "queryType"))
			out, err := en.QueryWorkflow(ns, wfID, runID, qType, req["args"])
			if err != nil {
				return nil, 404, err.Error()
			}
			return map[string]any{"query_result": out}, 200, ""
		}),
		opResetWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			eventID := int64Field(req, "event_id")
			reason := strOr(req, "reason", "")
			identity := strOr(req, "identity", "")
			wf, err := en.ResetWorkflow(ns, wfID, runID, eventID, reason, identity)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opSignalWithStartWorkflow: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			typeName := strOr(req, "workflow_type", "")
			tq := strOr(req, "task_queue", "")
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			sigName := strOr(req, "signal_name", str(req, "signalName"))
			reqID := strOr(req, "request_id", "")
			if sigName == "" {
				return nil, 400, "signal_name required"
			}
			sa, _ := req["search_attributes"].(map[string]any)
			wf, err := en.signalWithStartFull(ns, wfID, runID, TypeRef{Name: typeName}, tq, req["input"], sigName, req["signal_input"], reqID, sa, req["memo"])
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]any{"workflow_id": wf.Execution.WorkflowId, "run_id": wf.Execution.RunId}, 200, ""
		}),
		opListSchedules: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			rows, err := en.ListSchedules(ns)
			if err != nil {
				return nil, 500, err.Error()
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				out = append(out, scheduleSDK(&rows[i]))
			}
			return map[string]any{"schedules": out}, 200, ""
		}),
		opCreateSchedule: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			s := Schedule{ScheduleId: id, Namespace: ns}
			// Re-marshal the "schedule" field and let the SDK shape's
			// custom UnmarshalJSON handle it via a side struct.
			if sched, ok := req["schedule"].(map[string]any); ok {
				if spec, ok := sched["spec"].(map[string]any); ok {
					if cron, ok := spec["cron"].([]any); ok {
						for _, c := range cron {
							if cs, ok := c.(string); ok {
								s.Spec.CronString = append(s.Spec.CronString, cs)
							}
						}
					}
					if iv, ok := spec["interval"]; ok {
						// The engine fires interval schedules (engine.go
						// scheduleNext); decode straight into the spec's
						// interval entries via their json tags.
						raw, _ := json.Marshal(iv)
						_ = json.Unmarshal(raw, &s.Spec.Interval)
					}
				}
				if action, ok := sched["action"].(map[string]any); ok {
					s.Action.WorkflowType.Name, _ = action["workflow_type"].(string)
					s.Action.TaskQueue, _ = action["task_queue"].(string)
				}
				if p, ok := sched["paused"].(bool); ok {
					s.State.Paused = p
				}
			}
			if s.ScheduleId == "" {
				return nil, 400, "schedule_id required"
			}
			if err := en.CreateSchedule(s); err != nil {
				return nil, 400, err.Error()
			}
			return scheduleSDK(&s), 200, ""
		}),
		opDescribeSchedule: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			s, ok, err := en.DescribeSchedule(ns, id)
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "schedule not found"
			}
			return scheduleSDK(s), 200, ""
		}),
		opDeleteSchedule: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			if err := en.DeleteSchedule(ns, id); err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "deleted"}, 200, ""
		}),
		opPauseSchedule: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			paused := true
			if p, ok := req["paused"].(bool); ok {
				paused = p
			}
			if err := en.PauseSchedule(ns, id, paused, str(req, "note")); err != nil {
				return nil, 400, err.Error()
			}
			out := "paused"
			if !paused {
				out = "running"
			}
			return map[string]string{"status": out}, 200, ""
		}),
		opUnpauseSchedule: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			if err := en.PauseSchedule(ns, id, false, ""); err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "running"}, 200, ""
		}),

		// ── subscribe / deliver task plumbing ─────────────────────
		OpcodeSubscribeWorkflowTasks: wrapPeer(func(en *engine, from string, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			q := strOr(req, "task_queue", str(req, "taskQueue"))
			id, err := en.disp.Subscribe(from, ns, q, kindWorkflow)
			if err != nil {
				return nil, 400, err.Error()
			}
			en.workers.Register(Worker{
				Identity: strOr(req, "identity", from), Namespace: ns, TaskQueue: q,
				SDKName: str(req, "sdk_name"), SDKVersion: str(req, "sdk_version"),
			})
			return map[string]string{"subscription_id": id}, 200, ""
		}),
		OpcodeSubscribeActivityTasks: wrapPeer(func(en *engine, from string, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			q := strOr(req, "task_queue", str(req, "taskQueue"))
			id, err := en.disp.Subscribe(from, ns, q, kindActivity)
			if err != nil {
				return nil, 400, err.Error()
			}
			en.workers.Register(Worker{
				Identity: strOr(req, "identity", from), Namespace: ns, TaskQueue: q,
				SDKName: str(req, "sdk_name"), SDKVersion: str(req, "sdk_version"),
			})
			return map[string]string{"subscription_id": id}, 200, ""
		}),
		OpcodeUnsubscribeTasks: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			id := strOr(req, "subscription_id", str(req, "subscriptionId"))
			en.disp.Unsubscribe(id)
			return map[string]bool{"ok": true}, 200, ""
		}),

		// ── responses (object-field frames, see client/worker_transport.go) ──
		// respondWorkflow / respondActivity bypass scope — the caller's
		// task_token is HMAC-signed by the dispatcher and only resolves
		// for tasks we issued in-process. Token integrity is the trust
		// boundary here, not the auth_token.
		client.OpcodeRespondWorkflowTaskCompleted: respondWorkflowHandler(rootEn),
		client.OpcodeRespondActivityTaskCompleted: respondActivityCompletedHandler(rootEn),
		client.OpcodeRespondActivityTaskFailed:    respondActivityFailedHandler(rootEn),
		client.OpcodeRecordActivityTaskHeartbeat:  heartbeatHandler(),

		// Phase-2a: workflow-driven activities are event-sourced — the
		// decider emits a ScheduleActivity command on
		// RespondWorkflowTaskCompleted (applied by respondWorkflowHandler),
		// so there is no mid-episode ScheduleActivity RPC (0x006B retired).
		//
		// 0x006D — child workflows ship in a follow-up.
		client.OpcodeStartChildWorkflow: wrap(func(_ *engine, _ map[string]any) (any, uint32, string) {
			return nil, 501, "start_child_workflow: not yet implemented"
		}),

		// 0x00C4 — worker → server query response.
		OpcodeRespondQuery: wrap(func(en *engine, req map[string]any) (any, uint32, string) {
			token := strOr(req, "token", "")
			if token == "" {
				return nil, 400, "token required"
			}
			result, _ := decodeBytesField(req, "result")
			errMsg := strOr(req, "error", "")
			if !en.disp.CompleteQuery(token, result, errMsg) {
				return nil, 404, "query token not found"
			}
			return map[string]bool{"ok": true}, 200, ""
		}),
	}
}

// ── object-field handlers for worker Respond / Heartbeat ──────────────

// respondWorkflowHandler decodes the object-field frame the worker
// produces in encodeRespondWorkflowCompleted, completes the workflow
// task, and applies the worker's command list to the engine state.
func respondWorkflowHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, commands := decodeRespondFrame(msg, client.FieldCommandsBytes)
		t, ok := en.disp.CompleteWorkflowTask(token)
		if !ok {
			return objectAck(0, "task token not found", 404)
		}
		// Apply on the org that owns the run: org-scoped runs live in a prefixed
		// store partition rootEn cannot see. The root principal (unscoped /
		// loopback) makes As a no-op, so the in-process path is unchanged.
		oe := en.As(t.principal)
		// Apply the decider's command batch. kind=2 (scheduleActivity)
		// carries the deterministic seq + activity spec — this is where a
		// workflow-driven activity enters the durable, event-sourced path.
		var env struct {
			Version  int8 `json:"v"`
			Commands []struct {
				Kind            int8                    `json:"kind"`
				Result          []byte                  `json:"result,omitempty"`
				Failure         []byte                  `json:"failure,omitempty"`
				Seq             int                     `json:"seq,omitempty"`
				ActivityType    string                  `json:"activityType,omitempty"`
				WorkflowType    string                  `json:"workflowType,omitempty"`
				ChildWorkflowId string                  `json:"childWorkflowId,omitempty"`
				Input           []byte                  `json:"input,omitempty"`
				TaskQueue       string                  `json:"taskQueue,omitempty"`
				SearchAttrs     map[string]any          `json:"searchAttrs,omitempty"`
				StartToCloseMs  int64                   `json:"startToCloseMs,omitempty"`
				HeartbeatMs     int64                   `json:"heartbeatMs,omitempty"`
				RetryPolicy     *client.RetryPolicyJSON `json:"retryPolicy,omitempty"`
			} `json:"cmds"`
		}
		if len(commands) > 0 {
			_ = json.Unmarshal(commands, &env)
		}
		unlock := oe.lockRun(t.ns, t.workflowID, t.runID)
		defer unlock()
		for _, c := range env.Commands {
			switch c.Kind {
			case 0: // complete
				_, _ = oe.terminalTransition(t.ns, t.workflowID, t.runID, "WORKFLOW_EXECUTION_STATUS_COMPLETED", "workflow.completed", "WORKFLOW_EXECUTION_COMPLETED", map[string]any{"result": string(c.Result)})
			case 1: // fail
				_, _ = oe.terminalTransition(t.ns, t.workflowID, t.runID, "WORKFLOW_EXECUTION_STATUS_FAILED", "workflow.failed", "WORKFLOW_EXECUTION_FAILED", map[string]any{"failure": string(c.Failure)})
			case 2: // scheduleActivity — idempotent per (run, seq)
				_ = oe.applyScheduleActivity(t.ns, t.workflowID, t.runID, c.Seq, c.ActivityType, c.Input, c.TaskQueue, c.StartToCloseMs, c.HeartbeatMs, c.RetryPolicy)
			case 3: // canceled — worker ack of a CANCELING handshake
				_, _ = oe.AckCanceled(t.ns, t.workflowID, t.runID, string(c.Failure), "")
			case 4: // continueAsNew — close this run, start a successor
				_ = oe.applyContinueAsNew(t.ns, t.workflowID, t.runID, c.Input, c.WorkflowType, c.TaskQueue)
			case 5: // startChildWorkflow — detached (ABANDON), idempotent per (run, seq)
				_ = oe.applyStartChild(t.ns, t.workflowID, t.runID, c.Seq, c.ChildWorkflowId, c.WorkflowType, c.TaskQueue, c.Input, c.SearchAttrs)
			}
		}
		return objectAck(0, "", 200)
	}
}

func respondActivityCompletedHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, result := decodeRespondFrame(msg, client.FieldResultBytes)
		pt, ok := en.disp.ResolveActivityToken(token)
		if !ok {
			return objectAck(0, "task token not found", 404)
		}
		if err := en.As(pt.principal).completeWorkflowActivity(pt.ns, pt.workflowID, pt.runID, pt.seq, result, nil); err != nil {
			return objectAck(0, err.Error(), 500)
		}
		return objectAck(0, "", 200)
	}
}

func respondActivityFailedHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, failure := decodeRespondFrame(msg, client.FieldFailureBytes)
		pt, ok := en.disp.ResolveActivityToken(token)
		if !ok {
			return objectAck(0, "task token not found", 404)
		}
		if err := en.As(pt.principal).completeWorkflowActivity(pt.ns, pt.workflowID, pt.runID, pt.seq, nil, failure); err != nil {
			return objectAck(0, err.Error(), 500)
		}
		return objectAck(0, "", 200)
	}
}

func heartbeatHandler() zap.Handler {
	return func(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
		// v1: never request cancel.
		return objectAck(0, "", 200)
	}
}

// decodeRespondFrame extracts the task_token and the secondary bytes
// field (commands / result / failure / details) from a worker-encoded
// object frame.
func decodeRespondFrame(msg *zap.Message, secondaryField int) (token, secondary []byte) {
	if msg == nil {
		return nil, nil
	}
	root := msg.Root()
	if root.IsNull() {
		return nil, nil
	}
	t := root.Bytes(client.FieldTaskToken)
	s := root.Bytes(secondaryField)
	out1 := make([]byte, len(t))
	copy(out1, t)
	out2 := make([]byte, len(s))
	copy(out2, s)
	return out1, out2
}

// objectAck builds a minimal object-field response for worker Respond
// ops. cancelRequested defaults to 0 (false). status/error are folded
// into a tiny envelope-compatible shape so callers that decode either
// the heartbeat object form or the envelope shape both succeed.
func objectAck(cancelRequested uint8, errMsg string, status uint32) (*zap.Message, error) {
	b := zap.NewBuilder(64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetUint32(envelopeStatus, status)
	if cancelRequested != 0 {
		obj.SetBytes(client.FieldRespCancelRequested, []byte{cancelRequested})
	}
	if errMsg != "" {
		obj.SetBytes(envelopeError, []byte(errMsg))
	}
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// decodeBytesField pulls a base64-encoded []byte field out of the
// generic map[string]any decode. Go's default JSON unmarshal yields a
// string for []byte fields; we round-trip via base64 to recover the
// original bytes. Empty / missing → nil, nil.
func decodeBytesField(req map[string]any, key string) ([]byte, error) {
	v, ok := req[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		// Numbers / objects come through as the marshaled form — re-encode.
		return json.Marshal(v)
	}
	if s == "" {
		return nil, nil
	}
	// Encoded as base64 (Go json default for []byte).
	dec, err := base64DecodeStd(s)
	if err == nil {
		return dec, nil
	}
	// Some callers pass the raw string; fall back to its bytes.
	return []byte(s), nil
}

func base64DecodeStd(s string) ([]byte, error) {
	return base64Std.DecodeString(s)
}

func int64Field(req map[string]any, key string) int64 {
	v, ok := req[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}

// ── envelope (ZAP single-field JSON shape) ─────────────────────────

const (
	envelopeBody       = 0
	envelopeStatus     = 8
	envelopeError      = 12
	envelopeObjectSize = 24
)

func envelope(body []byte, status uint32, errMsg string) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + len(errMsg) + 64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	obj.SetBytes(envelopeError, []byte(errMsg))
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}
