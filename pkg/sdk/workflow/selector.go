package workflow

// Selector is the multi-way choice primitive. Build one with
// NewSelector(ctx), attach cases with AddFuture / AddReceive /
// AddDefault, then block on Select(ctx). Exactly one case fires per
// Select call.
//
// Mirrors go.temporal.io/sdk/workflow.Selector exactly.
type Selector interface {
	AddFuture(f Future, fn func(f Future)) Selector
	AddReceive(ch ReceiveChannel, fn func(ch ReceiveChannel, more bool)) Selector
	AddDefault(fn func()) Selector
	Select(ctx Context)
}

// NewSelector returns a Selector that blocks on Select until one of
// the attached cases fires. The ctx is captured only for the env
// handle; Select takes its own ctx so callers can pair a Selector
// with a cancel-scoped ctx for one iteration of a loop.
func NewSelector(ctx Context) Selector {
	return &selector{
		ctx: ctx,
	}
}

type selectorCase struct {
	kind    int // 0 = future, 1 = receive, 2 = default
	future  Future
	futCB   func(Future)
	ch      ReceiveChannel
	chCB    func(ReceiveChannel, bool)
	defCB   func()
}

const (
	caseFuture  = 0
	caseReceive = 1
	caseDefault = 2
)

type selector struct {
	ctx   Context
	cases []selectorCase
	def   *selectorCase
}

func (s *selector) AddFuture(f Future, fn func(f Future)) Selector {
	s.cases = append(s.cases, selectorCase{kind: caseFuture, future: f, futCB: fn})
	return s
}

func (s *selector) AddReceive(ch ReceiveChannel, fn func(ch ReceiveChannel, more bool)) Selector {
	s.cases = append(s.cases, selectorCase{kind: caseReceive, ch: ch, chCB: fn})
	return s
}

func (s *selector) AddDefault(fn func()) Selector {
	s.def = &selectorCase{kind: caseDefault, defCB: fn}
	return s
}

// Select blocks until one case fires, then invokes its callback.
// Inside the callback the caller may still call f.Get(ctx, &v) for
// the ready future or ch.Receive(ctx, &v) for the ready channel,
// exactly as users expect.
//
// When a default is attached, Select returns immediately with the
// default callback if no other case is ready.
func (s *selector) Select(ctx Context) {
	if len(s.cases) == 0 {
		if s.def != nil {
			s.def.defCB()
		}
		return
	}

	// Build the env SelectCase list. Default is handled separately.
	envCases := make([]SelectCase, 0, len(s.cases))
	for _, c := range s.cases {
		switch c.kind {
		case caseFuture:
			envCases = append(envCases, SelectCase{Future: c.future})
		case caseReceive:
			envCases = append(envCases, SelectCase{Channel: c.ch})
		}
	}

	// Fast path: if a default is attached, poll once and only
	// fall through to the env's blocking Select if no case is
	// ready. This is what Temporal does too.
	if s.def != nil {
		if idx := readyIndex(envCases); idx >= 0 {
			s.fire(s.cases[idx])
			return
		}
		s.def.defCB()
		return
	}

	idx := ctx.env().Select(envCases)
	if idx < 0 || idx >= len(s.cases) {
		return
	}
	s.fire(s.cases[idx])
}

// fire invokes the matched callback. For a receive case, it
// reports whether the channel is still open (the `more` argument
// that chan-close semantics require).
func (s *selector) fire(c selectorCase) {
	switch c.kind {
	case caseFuture:
		if c.futCB != nil {
			c.futCB(c.future)
		}
	case caseReceive:
		more := true
		// Phase 1 heuristic: if the channel is an unbuffered impl,
		// it knows its closed state. For external channels we pass
		// `more=true` and rely on the user calling Receive in the
		// callback to discover the close.
		if ch, ok := c.ch.(*chanImpl); ok {
			ch.mu.Lock()
			// Mirror the Receive rule: "drained and closed" means no more.
			drainedClosed := ch.closed && len(ch.buf) == 0 && len(ch.sendWaiters) == 0
			ch.mu.Unlock()
			more = !drainedClosed
		}
		if c.chCB != nil {
			c.chCB(c.ch, more)
		}
	}
}

// readyIndex scans cases for one that is immediately ready. Returns
// the first ready index or -1. Used for the AddDefault fast path.
func readyIndex(cases []SelectCase) int {
	for i, c := range cases {
		if c.Future != nil && c.Future.IsReady() {
			return i
		}
		if ch, ok := c.Channel.(*chanImpl); ok && ch.hasValue() {
			return i
		}
	}
	return -1
}
