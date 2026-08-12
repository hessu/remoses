package selftest

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
)

// wireMsg is the message every CAT trace line carries. It is duplicated from
// internal/rig rather than exported from there because it is deliberately a
// stable part of the log format — people put it in grep scripts — and a
// constant two packages agree on by contract is honest about that.
const wireMsg = "cat wire"

// capture is an slog.Handler that keeps CAT frames for whichever step is
// running, and passes everything through to the handler underneath.
//
// Attaching the trace to the step that provoked it is what makes a submitted
// log diagnosable. A flat file of frames and a flat file of results, with the
// reader left to line them up by timestamp, is what this replaces — and the
// lining up is exactly the fiddly part, because a poll tick lands in the middle
// of every set.
type capture struct {
	next  slog.Handler
	mu    *sync.Mutex
	frame *[]WireFrame
	// on is whether frames are being kept at all. Between steps they are not:
	// the poller runs continuously, and its traffic belongs to no step.
	on *bool
}

func newCapture(next slog.Handler) *capture {
	return &capture{
		next:  next,
		mu:    &sync.Mutex{},
		frame: new([]WireFrame),
		on:    new(bool),
	}
}

func (c *capture) Enabled(ctx context.Context, l slog.Level) bool {
	// Debug always, whatever the underlying handler wants: the wire trace is
	// emitted at debug level and this run exists to collect it. Without this a
	// user running at the default log level would send back a file with no
	// frames in it, which is most of its value gone.
	if l == slog.LevelDebug {
		return true
	}
	return c.next.Enabled(ctx, l)
}

func (c *capture) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == wireMsg {
		c.record(r)
		// Not passed on. The trace is in the file; repeating it on the terminal
		// would bury the progress the operator is watching.
		return nil
	}
	if !c.next.Enabled(ctx, r.Level) {
		return nil
	}
	return c.next.Handle(ctx, r)
}

func (c *capture) record(r slog.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !*c.on {
		return
	}
	var f WireFrame
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "dir":
			f.Dir = a.Value.String()
		case "hex":
			f.Hex = a.Value.String()
		case "text":
			f.Text = a.Value.String()
		case "key":
			f.Key = a.Value.String()
		case "ok":
			v := a.Value.Bool()
			f.OK = &v
		}
		return true
	})
	if f.Hex != "" {
		*c.frame = append(*c.frame, f)
	}
}

// start begins keeping frames for a step, discarding anything left over.
func (c *capture) start() {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.frame = nil
	*c.on = true
}

// take ends the step and returns what was collected.
func (c *capture) take() []WireFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	*c.on = false
	out := *c.frame
	*c.frame = nil
	return out
}

// WithAttrs and WithGroup keep the pass-through handler correct. The captured
// state is shared through pointers on purpose: slog clones a handler for every
// logger derived from it, and a copy with its own buffer would collect frames
// nobody ever reads.
func (c *capture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &capture{next: c.next.WithAttrs(attrs), mu: c.mu, frame: c.frame, on: c.on}
}

func (c *capture) WithGroup(name string) slog.Handler {
	return &capture{next: c.next.WithGroup(name), mu: c.mu, frame: c.frame, on: c.on}
}

// quoteHz renders a frequency the way the rest of this package writes numbers
// into the log: plain, unpunctuated, and parseable.
func quoteHz(hz uint64) string { return strconv.FormatUint(hz, 10) }
