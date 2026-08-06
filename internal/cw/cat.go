package cw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/morse"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// Speed limits. These bound what remoses will ask of any keyer; a backend may
// clamp further to what its rig actually accepts (the IC-7610 starts at 6 wpm,
// the TS-590 tops out at 60), and the achieved speed is always readable back
// through Status.
const (
	MinWPM = 5
	MaxWPM = 60
	// DefaultWPM is used when the configuration names no speed.
	DefaultWPM = 20
)

const (
	// minPollInterval and maxPollInterval bound the closed-loop buffer poll.
	// The interval tracks one dit time so a 40 wpm buffer refills promptly, but
	// the floor keeps a fast rig from hammering the serial port, and the
	// ceiling keeps a slow one from leaving audible gaps.
	minPollInterval = 20 * time.Millisecond
	maxPollInterval = 250 * time.Millisecond

	// commandTimeout bounds the transactions Abort and SetSpeed start. They are
	// called from an API request rather than from the pacing loop, so they
	// cannot borrow its cancellable context.
	commandTimeout = 2 * time.Second
)

// ErrClosed is returned by a sender that has been shut down with Close.
var ErrClosed = errors.New("cw: sender closed")

// Both senders can be shut down, which Sender itself does not express; the
// session closes them through an io.Closer assertion when a radio goes away.
var (
	_ Sender    = (*catSender)(nil)
	_ Sender    = (*serialSender)(nil)
	_ io.Closer = (*catSender)(nil)
	_ io.Closer = (*serialSender)(nil)
)

// ClampWPM brings a requested speed inside the range remoses will key. Zero
// means "not specified" and yields the default rather than the minimum, so an
// unset configuration field does not quietly become 5 wpm.
func ClampWPM(wpm int) int {
	if wpm <= 0 {
		return DefaultWPM
	}
	return min(max(wpm, MinWPM), MaxWPM)
}

// flight is a chunk that has been handed to the rig and is, by our reckoning,
// still sounding.
type flight struct {
	chars  int
	doneAt time.Time
}

// catSender feeds a rig's own CW buffer.
//
// One goroutine owns the pacing loop; everything else is queue manipulation
// under the mutex. The loop cannot be a simple write-and-forget, because both
// target rigs have buffers of a couple of dozen characters and neither
// tolerates being overrun — Kenwood answers a write to a full buffer with an
// error, and Icom silently drops what does not fit.
type catSender struct {
	ms   backend.MorseSender
	conn backend.Conn

	maxChunk       int
	chunksInFlight int

	mu       sync.Mutex
	cond     *sync.Cond
	queue    []*chunk
	inflight []flight
	// drainAt is when the rig is expected to fall silent, given everything we
	// have already handed it. It is the whole of the open-loop model.
	drainAt time.Time
	sending bool
	wpm     int
	nextID  uint64
	closed  bool
	// ctx is cancelled by Abort so that a pacing loop parked in a buffer poll
	// gives up at once instead of finishing its wait.
	ctx    context.Context
	cancel context.CancelFunc

	done chan struct{}
}

// NewCAT builds a sender that keys through the rig's own CW buffer.
//
// Neither target rig needs PTT asserting first: the transceiver keys itself
// from its buffer, so this sender never touches PTT. A station that needs an
// amplifier lead-in arranges it elsewhere.
func NewCAT(ms backend.MorseSender, c backend.Conn, cfg config.CW) (Sender, error) {
	if ms == nil || c == nil {
		return nil, errors.New("cw: NewCAT needs both a MorseSender and a Conn")
	}
	if ms.Charset() == "" {
		return nil, errors.New("cw: backend reports an empty CW charset")
	}
	maxChunk := ms.MaxChunk()
	if maxChunk < minMaxChunk {
		return nil, fmt.Errorf("cw: backend reports MaxChunk %d, which is too small to carry a prosign", maxChunk)
	}
	inFlight := cfg.ChunksInFlight
	if inFlight < 1 {
		inFlight = 1
	}

	s := &catSender{
		ms:             ms,
		conn:           c,
		maxChunk:       maxChunk,
		chunksInFlight: inFlight,
		wpm:            ClampWPM(cfg.DefaultWPM),
		done:           make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	go s.run()
	return s, nil
}

func (s *catSender) Charset() string { return s.ms.Charset() }

// Method reports CAT keying: this sender drives the rig's own buffer.
func (s *catSender) Method() radio.CWMethod { return radio.CWViaCAT }

// WPMRange declines to answer. The speed here is the rig's own keyer speed —
// set with a backend command, clamped by the rig — and the range is per model,
// which the backend's Caps already carries. Overriding it with this package's
// wider clamp would advertise speeds the radio will not key.
func (s *catSender) WPMRange() (int, int, bool) { return 0, 0, false }

// Enqueue validates, translates and chunks text, then leaves it for the pacing
// loop. It never blocks on the radio.
func (s *catSender) Enqueue(text string, mode Mode) (int, error) {
	up := asciiUpper(text)
	if err := validate(up, s.ms.Charset()); err != nil {
		return 0, err
	}
	chunks, err := buildChunks(up, s.maxChunk, s.ms.EncodeProsigns)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if mode == Replace {
		s.queue = nil
	}
	for i := range chunks {
		s.nextID++
		chunks[i].id = s.nextID
		s.queue = append(s.queue, &chunks[i])
	}
	s.cond.Broadcast()
	return utf8.RuneCountInString(text), nil
}

// Abort drops the queue and tells the rig to stop. Both halves are needed: a
// full buffer's worth of text may already be inside the radio, and it will
// keep transmitting it whatever we do locally.
func (s *catSender) Abort() {
	s.mu.Lock()
	s.queue = nil
	s.inflight = nil
	s.drainAt = time.Time{}
	cancel := s.cancel
	if !s.closed {
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	cancel()

	ctx, done := context.WithTimeout(context.Background(), commandTimeout)
	defer done()
	if err := s.ms.Abort(ctx, s.conn); err != nil {
		// A disconnected radio is not sending, so failing to tell it to stop is
		// not a problem worth an error line. Abort also runs on every session
		// teardown, including each reconnect attempt, so logging this loudly
		// turns one unplugged cable into a stream of alarming noise.
		if errors.Is(err, transport.ErrDisconnected) {
			slog.Debug("cw: no link to send the CW abort to", "err", err)
			return
		}
		slog.Error("cw: the radio did not acknowledge the CW abort", "err", err)
	}
}

func (s *catSender) Status() radio.CWStatus {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)

	st := radio.CWStatus{WPM: s.wpm}
	var remaining time.Duration
	if s.drainAt.After(now) {
		remaining = s.drainAt.Sub(now)
	}
	for _, f := range s.inflight {
		st.Queued += f.chars
	}
	for _, c := range s.queue {
		st.Queued += c.chars
		remaining += morse.Estimate(c.canon, s.wpm, morse.NeutralWeight)
	}
	st.EstRemainingMS = int(remaining.Milliseconds())
	st.Busy = st.Queued > 0 || s.sending || remaining > 0
	return st
}

// SetSpeed pushes the speed to the rig's keyer, which is what actually times
// the elements on this path.
func (s *catSender) SetSpeed(wpm int) error {
	wpm = ClampWPM(wpm)
	ctx, done := context.WithTimeout(context.Background(), commandTimeout)
	defer done()
	if err := s.ms.SetSpeed(ctx, s.conn, wpm); err != nil {
		return err
	}
	s.mu.Lock()
	s.wpm = wpm
	s.mu.Unlock()
	return nil
}

// Close stops the pacing loop. It is not part of Sender; the session calls it
// through an io.Closer assertion when a radio goes away.
func (s *catSender) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.queue = nil
	cancel := s.cancel
	s.cond.Broadcast()
	s.mu.Unlock()

	cancel()
	<-s.done
	return nil
}

func (s *catSender) run() {
	defer close(s.done)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			s.cond.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		c := s.queue[0]
		ctx := s.ctx
		s.mu.Unlock()

		if err := s.waitForRoom(ctx, c); err != nil {
			if !aborted(ctx, err) {
				s.fail("cw: could not pace the radio's CW buffer", err)
			}
			continue
		}

		// The head may have moved while we waited for room: Abort and a replace
		// both drop the queue, and neither should have what it discarded sent
		// after the fact.
		s.mu.Lock()
		if s.closed || s.ctx != ctx || len(s.queue) == 0 || s.queue[0].id != c.id {
			s.mu.Unlock()
			continue
		}
		s.queue = s.queue[1:]
		s.sending = true
		s.mu.Unlock()

		err := s.ms.SendChunk(ctx, s.conn, c.encoded)

		s.mu.Lock()
		s.sending = false
		if err == nil {
			s.recordSentLocked(c, time.Now())
		}
		s.mu.Unlock()

		if err != nil && !aborted(ctx, err) {
			s.fail("cw: could not send to the radio's CW buffer", err)
		}
	}
}

// waitForRoom blocks until the rig can take the chunk, by whichever of the two
// loops the backend supports.
func (s *catSender) waitForRoom(ctx context.Context, c *chunk) error {
	for {
		free, ok, err := s.ms.BufferFree(ctx, s.conn)
		if err != nil {
			return err
		}
		if !ok {
			// The rig cannot be asked how full it is, so pace on the estimate.
			return s.waitForDrain(ctx)
		}
		if free >= len(c.encoded) {
			return nil
		}
		if err := s.sleep(ctx, s.pollInterval()); err != nil {
			return err
		}
	}
}

// waitForDrain is the open-loop half: hold ChunksInFlight chunks in the rig
// beyond the one it is sounding, and wait for the estimate to say the oldest
// has finished before adding another. Too few chunks leaves audible gaps
// between them; too many lengthens abort latency, since everything already in
// the rig has to be stopped by the rig.
func (s *catSender) waitForDrain(ctx context.Context) error {
	for {
		s.mu.Lock()
		s.pruneLocked(time.Now())
		var until time.Time
		if len(s.inflight) > s.chunksInFlight {
			until = s.inflight[0].doneAt
		}
		s.mu.Unlock()

		if until.IsZero() {
			return nil
		}
		if err := s.sleepUntil(ctx, until); err != nil {
			return err
		}
	}
}

// recordSentLocked advances the drain model. A chunk starts sounding when the
// one before it finished, not when it was written, which is the whole point of
// keeping a buffer full.
func (s *catSender) recordSentLocked(c *chunk, now time.Time) {
	start := now
	if s.drainAt.After(now) {
		start = s.drainAt
	}
	s.drainAt = start.Add(morse.Estimate(c.canon, s.wpm, morse.NeutralWeight))
	s.inflight = append(s.inflight, flight{chars: c.chars, doneAt: s.drainAt})
}

func (s *catSender) pruneLocked(now time.Time) {
	i := 0
	for i < len(s.inflight) && !s.inflight[i].doneAt.After(now) {
		i++
	}
	s.inflight = s.inflight[i:]
}

func (s *catSender) pollInterval() time.Duration {
	s.mu.Lock()
	wpm := s.wpm
	s.mu.Unlock()
	return min(max(morse.UnitDuration(wpm), minPollInterval), maxPollInterval)
}

func (s *catSender) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *catSender) sleepUntil(ctx context.Context, at time.Time) error {
	return s.sleep(ctx, time.Until(at))
}

// fail gives up on the queue. A CAT error here means the port or the rig is in
// trouble; keeping the rest of the text and sending it once the session
// reconnects would put half a message on the air minutes later.
func (s *catSender) fail(msg string, err error) {
	s.mu.Lock()
	s.queue = nil
	s.mu.Unlock()
	slog.Error(msg, "err", err)
}

// aborted reports whether an error is just the pacing loop being cancelled.
func aborted(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}
