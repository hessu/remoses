package rig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// readBufMax is the largest frame the reader will assemble. Rig frames are tens
// of bytes; this exists only so a rig spewing garbage cannot grow the buffer
// without bound. bufio's default is the same value, set explicitly here because
// it is a protocol decision rather than an accident.
const readBufMax = 64 * 1024

// conn is one live connection to a radio: the transport, its reader goroutine,
// and the table of requests waiting for a reply.
//
// A conn is created per successful dial and thrown away on any error. That is
// what makes the failure path simple — nothing has to be reset, because nothing
// is reused. Session holds the current one in an atomic pointer.
type conn struct {
	s   *Session
	t   transport.Transport
	log *slog.Logger

	// wire enables the CAT byte trace for this radio. Copied at construction
	// rather than read from the session on every frame: it is checked once per
	// frame in the reader goroutine, which is in the CW timing path.
	wire bool

	// framer is the backend, when its inbound framing depends on which command
	// is in flight. Nil for the three protocols that delimit their own frames.
	// Resolved once here rather than asserted per write. See
	// backend.ReplyFramer.
	framer backend.ReplyFramer

	timeout time.Duration

	// cmdMu serialises transactions. A backend may assume its own request and
	// the reply it is waiting for are not interleaved with another command's.
	cmdMu sync.Mutex

	mu      sync.Mutex
	waiters []*waiter
	err     error
	closing bool

	done     chan struct{} // closed when the reader goroutine has exited
	readDone sync.WaitGroup
}

// waiter is one pending request. keys is the set of reply keys that satisfy it;
// most commands want exactly one.
type waiter struct {
	keys []backend.Key
	ch   chan backend.Update // buffered, so the reader never blocks on delivery
}

func (w *waiter) wants(k backend.Key) bool {
	for _, want := range w.keys {
		if want == k {
			return true
		}
	}
	return false
}

func newConn(s *Session, t transport.Transport) *conn {
	c := &conn{
		s:       s,
		t:       t,
		log:     s.log,
		wire:    s.wireDebug,
		timeout: s.cmdTimeout,
		done:    make(chan struct{}),
	}
	c.framer, _ = s.rig.(backend.ReplyFramer)
	return c
}

// start launches the reader goroutine. Exactly one goroutine ever reads the
// transport, and it never performs a transaction of its own.
func (c *conn) start() {
	c.readDone.Add(1)
	go func() {
		defer c.readDone.Done()
		c.readLoop()
	}()
}

// readLoop splits the inbound stream into frames, decodes each one and folds it
// into the state cache — solicited or not. Applying every patch unconditionally
// is what makes Icom Transceive and Kenwood AI push updates work for free: an
// unsolicited frequency report goes through exactly the same path as the reply
// to a poll.
func (c *conn) readLoop() {
	defer close(c.done)

	sc := bufio.NewScanner(c.t)
	sc.Buffer(make([]byte, 0, 4096), readBufMax)
	sc.Split(c.s.rig.Split)

	for sc.Scan() {
		// The scanner reuses its buffer, so copy before handing the bytes to a
		// backend that may retain them in Update.Raw.
		frame := make([]byte, len(sc.Bytes()))
		copy(frame, sc.Bytes())

		up, err := c.s.rig.Decode(frame)
		if err != nil {
			// A backend is required not to error on unknown frames, so this is
			// a real protocol fault. Log and resynchronise rather than dropping
			// the connection: a rig powering up emits noise.
			if c.wire {
				c.logWire(wireFromRig, frame, "err", err)
			}
			c.log.Debug("undecodable frame", "frame", fmt.Sprintf("%q", frame), "err", err)
			continue
		}

		// Traced before the patch is applied and before a waiter is looked for,
		// so the log reads in arrival order and no lock is held while it is
		// written. Solicited or not: an unsolicited frame is the one a trace is
		// most often opened for.
		if c.wire {
			c.logWire(wireFromRig, frame, "key", wireKey(up.Key), "ok", up.OK)
		}

		c.s.applyUpdate(up)

		if up.Key != backend.KeyUnsolicited {
			c.deliver(up)
		}
	}

	err := sc.Err()
	if err == nil {
		// A clean EOF on a serial port means the device went away.
		err = transport.ErrDisconnected
	}
	c.fail(err)
	c.wakeWaiters()
}

// deliver hands an update to the oldest waiter that asked for its key. An
// update nobody is waiting for is dropped after its patch has been applied —
// that covers CI-V bus echo, and a rig answering after its request timed out.
func (c *conn) deliver(up backend.Update) {
	c.mu.Lock()
	var w *waiter
	for i, cand := range c.waiters {
		if cand.wants(up.Key) {
			w = cand
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
	if w == nil {
		return
	}
	select {
	case w.ch <- up:
	default: // buffered size 1 and a waiter is only matched once; cannot happen
	}
}

// fail records the first error seen on this connection and closes the transport
// so the reader unblocks. Later errors are ignored: the first one is the cause,
// the rest are consequences.
func (c *conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	already := c.closing
	c.closing = true
	c.mu.Unlock()
	if !already {
		_ = c.t.Close()
	}
}

// wakeWaiters is not strictly required — every waiter also selects on c.done —
// but draining the table keeps the failure path free of stale entries.
func (c *conn) wakeWaiters() {
	c.mu.Lock()
	c.waiters = nil
	c.mu.Unlock()
}

// close tears the connection down and waits for the reader to exit, so the
// caller can be sure nothing is still touching the transport.
func (c *conn) close(err error) {
	c.fail(err)
	c.readDone.Wait()
}

func (c *conn) lastErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	return transport.ErrDisconnected
}

func (c *conn) register(w *waiter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		if c.err != nil {
			return c.err
		}
		return transport.ErrDisconnected
	}
	c.waiters = append(c.waiters, w)
	return nil
}

func (c *conn) unregister(w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, cand := range c.waiters {
		if cand == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return
		}
	}
}

// Do writes req and waits for a frame carrying one of want.
//
// The waiter is registered BEFORE the write. A rig on a fast USB link can put
// its reply on the wire before the writing goroutine gets scheduled again, and
// registering afterwards would lose that reply and force a timeout.
func (c *conn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("rig %s: %w", c.s.id, ErrNoKeys)
	}

	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	if err := ctx.Err(); err != nil {
		return backend.Update{}, err
	}

	w := &waiter{keys: want, ch: make(chan backend.Update, 1)}
	if err := c.register(w); err != nil {
		return backend.Update{}, err
	}
	defer c.unregister(w)

	if err := c.writeLocked(req); err != nil {
		return backend.Update{}, err
	}

	timer := time.NewTimer(c.timeout)
	defer timer.Stop()

	select {
	case up := <-w.ch:
		return checkUpdate(c.s.id, req, up)
	case <-ctx.Done():
		return backend.Update{}, ctx.Err()
	case <-timer.C:
		return backend.Update{}, fmt.Errorf("rig %s: no reply to %q within %s: %w",
			c.s.id, req, c.timeout, ErrTimeout)
	case <-c.done:
		// A reply may have landed just before the reader exited; prefer it.
		select {
		case up := <-w.ch:
			return checkUpdate(c.s.id, req, up)
		default:
		}
		return backend.Update{}, c.lastErr()
	}
}

// checkUpdate turns a negative acknowledgement into an error. Icom answers FA
// and Kenwood answers "?" when it rejects a command, and a backend that got a
// NAK must not treat it as success.
func checkUpdate(id string, req []byte, up backend.Update) (backend.Update, error) {
	if !up.OK {
		return up, fmt.Errorf("rig %s: rejected %q: %w", id, req, ErrNAK)
	}
	return up, nil
}

// Send writes req and returns. Used for commands the rig does not answer:
// Kenwood TX;/RX; are silent unless AI happens to be on, so waiting for an ack
// would stall until the timeout on a correctly configured rig.
func (c *conn) Send(ctx context.Context, req []byte) error {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.writeLocked(req)
}

// writeLocked must be called with cmdMu held, which is what keeps two commands
// from interleaving half-frames on the wire.
func (c *conn) writeLocked(req []byte) error {
	c.mu.Lock()
	closing, err := c.closing, c.err
	c.mu.Unlock()
	if closing {
		if err != nil {
			return err
		}
		return transport.ErrDisconnected
	}

	// Told before the write, never after: on a protocol whose answers are not
	// self-delimiting the reply can be back before this goroutine is scheduled
	// again, and a reader that has not yet been told how long it will be would
	// frame it wrongly. Under cmdMu, so the backend sees commands in the order
	// the radio does. See backend.ReplyFramer.
	if c.framer != nil {
		c.framer.Expect(req)
	}

	if _, err := c.t.Write(req); err != nil {
		if !errors.Is(err, transport.ErrDisconnected) {
			err = fmt.Errorf("rig %s: write: %w", c.s.id, err)
		}
		// A failed write means the port is gone; tear the connection down so the
		// supervisor reconnects rather than retrying on a dead handle.
		c.fail(err)
		return err
	}
	// Traced after the write, so the line means "these bytes are on the wire"
	// rather than "these bytes were about to be". The guard is what keeps the
	// unconditional cost at zero: slog evaluates its arguments eagerly, so
	// formatting here without it would run on every command of every radio
	// whatever the log level.
	if c.wire {
		c.logWire(wireToRig, req)
	}
	return nil
}
