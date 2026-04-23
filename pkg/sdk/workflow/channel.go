package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ReceiveChannel is the read-only view of a workflow channel. Both
// signal channels (GetSignalChannel) and user channels (NewChannel,
// NewBufferedChannel) satisfy it.
//
// Mirrors go.temporal.io/sdk/workflow.ReceiveChannel exactly.
type ReceiveChannel interface {
	// Receive blocks the workflow coroutine until a value is
	// available, decodes it into valPtr, and reports whether the
	// channel is still open via the ok return. If the channel is
	// closed and drained, ok is false and valPtr is untouched.
	Receive(ctx Context, valPtr any) (ok bool)

	// ReceiveAsync is the non-blocking form: it returns false
	// immediately if no value is buffered. Useful inside a
	// Selector callback where you only want to consume if ready.
	ReceiveAsync(valPtr any) (ok bool)

	// Name returns the channel's debug identifier. Empty for
	// unnamed user channels. Signal channels carry the signal name.
	Name() string
}

// Channel is the read/write view of a workflow channel. All
// user-created channels satisfy Channel; only workflow code can
// Send on them.
type Channel interface {
	ReceiveChannel

	// Send enqueues a value. On an unbuffered channel Send blocks
	// the coroutine until a receiver pairs with it; on a buffered
	// channel it blocks only when the buffer is full.
	Send(ctx Context, val any) error

	// Close marks the channel closed. Further Sends panic;
	// Receives drain any buffered values then report ok=false.
	// Idempotent.
	Close()
}

// NewChannel returns an unbuffered workflow channel. Send blocks
// until a Receive pairs with it.
func NewChannel(ctx Context) Channel {
	if ctx == nil {
		return newChan("", 0)
	}
	return ctx.env().NewChannel("", 0)
}

// NewBufferedChannel returns a buffered workflow channel with the
// given capacity. Send blocks only when the buffer is full.
func NewBufferedChannel(ctx Context, size int) Channel {
	if size < 0 {
		size = 0
	}
	if ctx == nil {
		return newChan("", size)
	}
	return ctx.env().NewChannel("", size)
}

// NewNamedChannel returns a buffered channel with a caller-supplied
// debug name. Names are used for replay identity and log output.
func NewNamedChannel(ctx Context, name string, size int) Channel {
	if size < 0 {
		size = 0
	}
	if ctx == nil {
		return newChan(name, size)
	}
	return ctx.env().NewChannel(name, size)
}

// chanImpl is the default Channel implementation used by the stub
// env and by the worker when it doesn't need a bespoke queue. It
// stores values as encoded []byte so the Send/Receive boundary
// matches what the worker produces over the wire.
//
// Thread-safety: single-coroutine-per-workflow is the invariant, so
// the mutex here is only to keep the data race detector happy in
// the stub-env tests that drive Send/Receive from test goroutines.
type chanImpl struct {
	name string
	size int // 0 = unbuffered

	mu     sync.Mutex
	buf    [][]byte
	closed bool

	// waiting receivers: each is a channel that Send closes when a
	// value is handed to that receiver directly.
	recvWaiters []*chanRecvWaiter

	// waiting senders (only on unbuffered): each holds the payload
	// and a done channel closed once a receiver takes it.
	sendWaiters []*chanSendWaiter

	// wakers is the set of single-shot one-buffer channels an outside
	// observer (Selector fan-in) parks on to learn when this channel
	// becomes readable. Send() and Close() each signal every armed
	// waker by non-blocking send; wakers auto-remove on consume.
	wakers []chan struct{}
}

type chanRecvWaiter struct {
	done    chan struct{}
	payload []byte
	ok      bool // false iff delivered because channel closed
}

type chanSendWaiter struct {
	done    chan struct{}
	payload []byte
}

// newChan constructs a chanImpl. Exposed package-private so the
// stub env can mint them without round-tripping through a public
// constructor that requires a ctx.
func newChan(name string, size int) *chanImpl {
	return &chanImpl{name: name, size: size}
}

// NewChannelFromEnv mints a Channel without needing a Context. The
// worker env uses this so it can satisfy its CoroutineEnv.NewChannel
// contract without a chicken-and-egg dependency on a Context that
// carries the env.
func NewChannelFromEnv(name string, size int) Channel {
	if size < 0 {
		size = 0
	}
	return newChan(name, size)
}

// HasValue reports whether the channel has a value ready (buffered
// or a waiting sender). Exported for the worker env's Select fan-in;
// user code should use ReceiveAsync.
func (c *chanImpl) HasValue() bool { return c.hasValue() }

// RegisterReadyWaker attaches a single-shot notification channel. The
// channel receives a value (non-blocking) as soon as the chan becomes
// readable — i.e. a value is buffered, an unbuffered sender parks, or
// the channel is closed. The waker is auto-removed from the set once
// fired; callers pass a buffered (cap 1) channel so the signal is
// never dropped.
//
// Used by the Selector fan-in to avoid busy-polling. If the channel
// already has a value at registration time the waker fires
// immediately (synchronously, before returning).
func (c *chanImpl) RegisterReadyWaker(w chan struct{}) {
	if w == nil {
		return
	}
	c.mu.Lock()
	readable := len(c.buf) > 0 || (c.size == 0 && len(c.sendWaiters) > 0) || c.closed
	if readable {
		c.mu.Unlock()
		select {
		case w <- struct{}{}:
		default:
		}
		return
	}
	c.wakers = append(c.wakers, w)
	c.mu.Unlock()
}

// signalWakersLocked fires every registered waker (non-blocking) and
// drops them from the slice. Caller must hold c.mu.
func (c *chanImpl) signalWakersLocked() {
	for _, w := range c.wakers {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	c.wakers = nil
}

// Name implements ReceiveChannel.
func (c *chanImpl) Name() string { return c.name }

// Send implements Channel.
func (c *chanImpl) Send(ctx Context, val any) error {
	if ctx == nil {
		return errors.New("workflow.Channel.Send: nil context")
	}
	payload, err := EncodePayload(val)
	if err != nil {
		return fmt.Errorf("workflow.Channel.Send: encode: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		panic("workflow.Channel.Send: send on closed channel")
	}

	// Fast path: hand off to a waiting receiver.
	if len(c.recvWaiters) > 0 {
		r := c.recvWaiters[0]
		c.recvWaiters = c.recvWaiters[1:]
		r.payload = payload
		r.ok = true
		close(r.done)
		c.mu.Unlock()
		return nil
	}

	// Buffered channel with room: enqueue.
	if c.size > 0 && len(c.buf) < c.size {
		c.buf = append(c.buf, payload)
		c.signalWakersLocked()
		c.mu.Unlock()
		return nil
	}

	// Block: park as a send-waiter. For unbuffered channels a parked
	// sender means Receive can now satisfy, so wake observers.
	w := &chanSendWaiter{done: make(chan struct{}), payload: payload}
	c.sendWaiters = append(c.sendWaiters, w)
	if c.size == 0 {
		c.signalWakersLocked()
	}
	c.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		// Best-effort removal so we don't deliver to a dead sender.
		for i, sw := range c.sendWaiters {
			if sw == w {
				c.sendWaiters = append(c.sendWaiters[:i], c.sendWaiters[i+1:]...)
				break
			}
		}
		c.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errors.New("workflow.Channel.Send: canceled")
	}
}

// Receive implements ReceiveChannel.
func (c *chanImpl) Receive(ctx Context, valPtr any) bool {
	if ctx == nil {
		return false
	}
	for {
		c.mu.Lock()

		// Fast path: buffered value available.
		if len(c.buf) > 0 {
			payload := c.buf[0]
			c.buf = c.buf[1:]
			// Promote one waiting sender into the vacated slot.
			if len(c.sendWaiters) > 0 {
				sw := c.sendWaiters[0]
				c.sendWaiters = c.sendWaiters[1:]
				c.buf = append(c.buf, sw.payload)
				close(sw.done)
			}
			c.mu.Unlock()
			decode(payload, valPtr)
			return true
		}

		// Closed and empty: signal done.
		if c.closed {
			// Drain any still-parked senders; closed channel must
			// panic on send, but we're defensive here.
			c.mu.Unlock()
			return false
		}

		// Unbuffered: if a sender is waiting, take directly.
		if c.size == 0 && len(c.sendWaiters) > 0 {
			sw := c.sendWaiters[0]
			c.sendWaiters = c.sendWaiters[1:]
			close(sw.done)
			c.mu.Unlock()
			decode(sw.payload, valPtr)
			return true
		}

		// Park as a recv-waiter and release the lock.
		w := &chanRecvWaiter{done: make(chan struct{})}
		c.recvWaiters = append(c.recvWaiters, w)
		c.mu.Unlock()

		select {
		case <-w.done:
			if !w.ok {
				return false
			}
			decode(w.payload, valPtr)
			return true
		case <-ctx.Done():
			c.mu.Lock()
			for i, rw := range c.recvWaiters {
				if rw == w {
					c.recvWaiters = append(c.recvWaiters[:i], c.recvWaiters[i+1:]...)
					break
				}
			}
			c.mu.Unlock()
			return false
		}
	}
}

// ReceiveAsync implements ReceiveChannel.
func (c *chanImpl) ReceiveAsync(valPtr any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) > 0 {
		payload := c.buf[0]
		c.buf = c.buf[1:]
		if len(c.sendWaiters) > 0 {
			sw := c.sendWaiters[0]
			c.sendWaiters = c.sendWaiters[1:]
			c.buf = append(c.buf, sw.payload)
			close(sw.done)
		}
		decode(payload, valPtr)
		return true
	}
	if c.size == 0 && len(c.sendWaiters) > 0 {
		sw := c.sendWaiters[0]
		c.sendWaiters = c.sendWaiters[1:]
		close(sw.done)
		decode(sw.payload, valPtr)
		return true
	}
	return false
}

// Close implements Channel.
func (c *chanImpl) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	// Wake up every parked receiver with ok=false.
	for _, r := range c.recvWaiters {
		r.ok = false
		close(r.done)
	}
	c.recvWaiters = nil
	// Closed channel is readable (Receive returns ok=false). Notify
	// any Selector fan-in observers.
	c.signalWakersLocked()
	c.mu.Unlock()
}

// hasValue reports whether a Receive would return immediately. Used
// by the Selector stub env to decide which case is ready.
func (c *chanImpl) hasValue() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.buf) > 0 {
		return true
	}
	if c.size == 0 && len(c.sendWaiters) > 0 {
		return true
	}
	return c.closed
}

// decode is the shared JSON-decoder helper. Ignores nil valPtr.
func decode(payload []byte, valPtr any) {
	if valPtr == nil || len(payload) == 0 {
		return
	}
	_ = json.Unmarshal(payload, valPtr)
}
