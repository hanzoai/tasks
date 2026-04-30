// Copyright © 2026 Hanzo AI. MIT License.

package replication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/luxfi/consensus"
)

// Transport abstracts the cluster wire so tests can swap a memory bus
// for ZAP without dragging the network layer into unit tests. The
// quasar driver uses it to broadcast Block payloads and votes; in
// production Embedded wires this to the per-process zap.Node.
type Transport interface {
	// Broadcast sends payload to every other validator. Implementations
	// SHOULD return promptly; durability is the consensus engine's job.
	Broadcast(ctx context.Context, kind string, payload []byte) error
	// OnReceive registers a handler for inbound traffic; returns nil if
	// the transport is local-only (single node).
	OnReceive(kind string, h func(payload []byte) error)
	// Validators returns the current validator set, leader-first.
	Validators() []string
	// LocalID returns this node's stable id.
	LocalID() string
}

// QuasarConfig pins the post-quantum engine to a validator set.
type QuasarConfig struct {
	NodeID       string
	Validators   []string // host:port strings; LocalID must appear here
	Transport    Transport
	ProposeWait  time.Duration // default 2s
	WitnessAfter time.Duration // default 100ms — when to escalate to commit vote
}

// QuasarReplicator drives consensus.NewPQ with WAL frames as block
// payloads. On a single-node validator set the engine accepts every
// frame after a self-vote; multi-node operation requires Transport to
// fan votes between peers — the engine treats them as native PQ votes.
type QuasarReplicator struct {
	cfg      QuasarConfig
	engine   consensus.Engine
	mu       sync.RWMutex
	handlers []Handler
	pending  map[consensus.ID]*proposal
	height   uint64
	parent   consensus.ID
	closed   chan struct{}
	stats    struct {
		accepted, rejected, timeouts uint64
	}
}

type proposal struct {
	frame Frame
	done  chan Decision
}

// NewQuasar wires consensus.NewPQ to the supplied transport and starts
// the engine. Returns once the genesis block is in place.
func NewQuasar(ctx context.Context, cfg QuasarConfig) (*QuasarReplicator, error) {
	if cfg.NodeID == "" {
		return nil, fmt.Errorf("quasar: NodeID required")
	}
	if cfg.Transport == nil {
		return nil, fmt.Errorf("quasar: Transport required")
	}
	if cfg.ProposeWait == 0 {
		cfg.ProposeWait = 2 * time.Second
	}
	if cfg.WitnessAfter == 0 {
		cfg.WitnessAfter = 100 * time.Millisecond
	}
	params := consensus.GetConfig(len(cfg.Validators))
	pqCfg := consensus.DefaultConfig()
	pqCfg.Alpha = params.AlphaConfidence
	pqCfg.K = params.K
	pqCfg.QuantumResistant = true
	if pqCfg.Alpha < 1 {
		pqCfg.Alpha = 1
	}
	eng := consensus.NewPQ(pqCfg)
	if err := eng.Start(ctx); err != nil {
		return nil, fmt.Errorf("quasar: start engine: %w", err)
	}
	r := &QuasarReplicator{
		cfg:     cfg,
		engine:  eng,
		pending: make(map[consensus.ID]*proposal),
		parent:  consensus.GenesisID,
		closed:  make(chan struct{}),
	}
	cfg.Transport.OnReceive("tasks.frame", r.handleFrame)
	cfg.Transport.OnReceive("tasks.vote", r.handleVote)
	return r, nil
}

// Propose builds a Block whose payload is the JSON-encoded frame, adds
// it to the engine, broadcasts it to peers, and waits for the engine to
// mark it accepted. Self-vote happens immediately so single-node
// configurations terminate without external traffic.
func (r *QuasarReplicator) Propose(ctx context.Context, f Frame) (Decision, error) {
	if f.Seq == 0 {
		r.mu.Lock()
		r.height++
		f.Seq = r.height
		r.mu.Unlock()
	}
	body, err := json.Marshal(f)
	if err != nil {
		return DecisionReject, fmt.Errorf("quasar: marshal frame: %w", err)
	}
	id := frameID(f, body)
	block := consensus.NewBlock(id, r.parent, f.Seq, body)
	block.Time = time.Now()
	if err := r.engine.Add(ctx, block); err != nil {
		return DecisionReject, fmt.Errorf("quasar: engine.Add: %w", err)
	}
	p := &proposal{frame: f, done: make(chan Decision, 1)}
	r.mu.Lock()
	r.pending[id] = p
	r.mu.Unlock()

	// Self vote — required to reach Alpha when Alpha=1.
	if err := r.engine.RecordVote(ctx, consensus.NewVote(id, consensus.VoteCommit, consensus.NodeID{})); err != nil {
		// Ignore "already voted"-style failures; the engine still accepts.
	}
	// Tell peers about the block; their replicators apply on accept.
	_ = r.cfg.Transport.Broadcast(ctx, "tasks.frame", body)

	timeout := r.cfg.ProposeWait
	deadline, ok := ctx.Deadline()
	if ok {
		if d := time.Until(deadline); d > 0 && d < timeout {
			timeout = d
		}
	}
	// Engine.IsAccepted is polled because the chain engine doesn't
	// expose a per-block channel. The poll interval is small enough
	// to keep e2e latency in the low-ms range for single-node.
	deadlineT := time.Now().Add(timeout)
	for time.Now().Before(deadlineT) {
		if r.engine.IsAccepted(id) {
			r.applyAccepted(ctx, f)
			r.mu.Lock()
			r.parent = id
			r.stats.accepted++
			delete(r.pending, id)
			r.mu.Unlock()
			return DecisionAccept, nil
		}
		select {
		case <-ctx.Done():
			r.mu.Lock()
			r.stats.timeouts++
			delete(r.pending, id)
			r.mu.Unlock()
			return DecisionTimeout, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	r.mu.Lock()
	r.stats.timeouts++
	delete(r.pending, id)
	r.mu.Unlock()
	return DecisionTimeout, ErrTimeout
}

// Subscribe registers an applier; called both for self-proposed frames
// (after engine accept) and for frames received from peers.
func (r *QuasarReplicator) Subscribe(h Handler) {
	r.mu.Lock()
	r.handlers = append(r.handlers, h)
	r.mu.Unlock()
}

// Close stops the engine.
func (r *QuasarReplicator) Close() error {
	select {
	case <-r.closed:
		return nil
	default:
		close(r.closed)
	}
	return r.engine.Stop()
}

// Stats returns counters used by /v1/tasks/cluster.
func (r *QuasarReplicator) Stats() (accepted, rejected, timeouts uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.stats.accepted, r.stats.rejected, r.stats.timeouts
}

// applyAccepted invokes registered handlers under the read lock.
func (r *QuasarReplicator) applyAccepted(ctx context.Context, f Frame) {
	r.mu.RLock()
	hs := append([]Handler(nil), r.handlers...)
	r.mu.RUnlock()
	for _, h := range hs {
		_ = h(ctx, f)
	}
}

// handleFrame receives a peer-proposed frame. We add it to our engine
// and self-vote so quorum forms; once IsAccepted, applyAccepted runs.
func (r *QuasarReplicator) handleFrame(payload []byte) error {
	var f Frame
	if err := json.Unmarshal(payload, &f); err != nil {
		return fmt.Errorf("quasar.handleFrame: %w", err)
	}
	id := frameID(f, payload)
	ctx, cancel := context.WithTimeout(context.Background(), r.cfg.ProposeWait)
	defer cancel()
	block := consensus.NewBlock(id, r.parent, f.Seq, payload)
	block.Time = time.Now()
	_ = r.engine.Add(ctx, block)
	_ = r.engine.RecordVote(ctx, consensus.NewVote(id, consensus.VoteCommit, consensus.NodeID{}))
	deadline := time.Now().Add(r.cfg.ProposeWait)
	for time.Now().Before(deadline) {
		if r.engine.IsAccepted(id) {
			r.applyAccepted(ctx, f)
			r.mu.Lock()
			r.parent = id
			r.stats.accepted++
			r.mu.Unlock()
			return nil
		}
		time.Sleep(2 * time.Millisecond)
	}
	return ErrTimeout
}

// handleVote forwards an inbound peer vote to the engine. The vote
// envelope is {block_id[32], voter[20], type[1], sig[N]}.
func (r *QuasarReplicator) handleVote(payload []byte) error {
	if len(payload) < 32+20+1 {
		return fmt.Errorf("quasar.handleVote: short payload")
	}
	var blockID consensus.ID
	copy(blockID[:], payload[:32])
	var voter consensus.NodeID
	copy(voter[:], payload[32:52])
	vt := consensus.VoteType(payload[52])
	sig := payload[53:]
	v := consensus.NewVote(blockID, vt, voter)
	v.Signature = append([]byte(nil), sig...)
	return r.engine.RecordVote(context.Background(), v)
}

// frameID is a deterministic SHA-256 over (seq||key||value||op) so the
// same frame produces the same Block ID across nodes.
func frameID(f Frame, body []byte) consensus.ID {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], f.Seq)
	h.Write(buf[:])
	h.Write(body)
	var out consensus.ID
	copy(out[:], h.Sum(nil))
	return out
}

// MemoryTransport is a deterministic in-process Transport used by
// tests and by single-node embedded mode. It implements broadcast as
// a fan-out across peers registered against the same hub.
type MemoryTransport struct {
	mu       sync.Mutex
	id       string
	hub      *MemoryHub
	handlers map[string]func([]byte) error
}

// MemoryHub coordinates a set of MemoryTransport peers.
type MemoryHub struct {
	mu    sync.Mutex
	peers []*MemoryTransport
}

// NewMemoryHub returns a hub for in-process replicator tests.
func NewMemoryHub() *MemoryHub { return &MemoryHub{} }

// Connect attaches a node to the hub; the returned Transport
// participates in broadcasts originating from any other peer.
func (h *MemoryHub) Connect(id string) *MemoryTransport {
	t := &MemoryTransport{id: id, hub: h, handlers: map[string]func([]byte) error{}}
	h.mu.Lock()
	h.peers = append(h.peers, t)
	h.mu.Unlock()
	return t
}

// Broadcast delivers payload to every peer except this one.
func (t *MemoryTransport) Broadcast(_ context.Context, kind string, payload []byte) error {
	t.hub.mu.Lock()
	peers := append([]*MemoryTransport(nil), t.hub.peers...)
	t.hub.mu.Unlock()
	for _, p := range peers {
		if p == t {
			continue
		}
		p.mu.Lock()
		h := p.handlers[kind]
		p.mu.Unlock()
		if h != nil {
			_ = h(append([]byte(nil), payload...))
		}
	}
	return nil
}

// OnReceive registers handlers per kind.
func (t *MemoryTransport) OnReceive(kind string, h func([]byte) error) {
	t.mu.Lock()
	t.handlers[kind] = h
	t.mu.Unlock()
}

// Validators returns every registered peer ID, this node first.
func (t *MemoryTransport) Validators() []string {
	t.hub.mu.Lock()
	defer t.hub.mu.Unlock()
	out := []string{t.id}
	for _, p := range t.hub.peers {
		if p.id == t.id {
			continue
		}
		out = append(out, p.id)
	}
	return out
}

// LocalID returns this node's id.
func (t *MemoryTransport) LocalID() string { return t.id }

// frameKey is exported for /v1/tasks/cluster diagnostics.
func (f Frame) FrameKey() []byte {
	var buf bytes.Buffer
	buf.WriteString(f.OrgID)
	buf.WriteByte('/')
	buf.WriteString(f.Namespace)
	buf.WriteByte('/')
	buf.WriteString(f.Key)
	return buf.Bytes()
}
