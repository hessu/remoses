// Package rig owns every piece of concurrency between the HTTP layer and a
// radio's serial port.
//
// Two invariants carry the design, and everything here exists to protect them:
//
//  1. Exactly one goroutine owns each transport. It is the reader. Commands are
//     submitted through a Conn, which serialises them; nothing else touches the
//     port.
//  2. The reader never performs a request/response. It splits frames, decodes
//     them, folds every patch into the state cache, and only then tries to hand
//     the frame to a waiting request.
//
// The second invariant is what makes push updates free. Icom Transceive and
// Kenwood AI frames arrive unsolicited, match no pending request, and still
// update the cache — a front-panel knob movement appears without polling. It
// also handles the awkward cases correctly for nothing: CI-V bus echo, and a rig
// that answers after its request has already timed out.
//
// Reads never block. State is published through an atomic pointer, so the API
// and new WebSocket subscribers are served from a snapshot even while the port
// is wedged.
package rig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// Errors the API layer maps onto status codes. They are distinct sentinels
// rather than strings because the mapping is part of the contract: a remote
// operator needs to know whether to retry or go and check a cable.
var (
	// ErrTimeout means the rig did not answer in time. Maps to 504.
	ErrTimeout = errors.New("rig: timed out waiting for reply")
	// ErrDisconnected means there is no live connection to the radio. Maps to
	// 503. It aliases the transport sentinel so a caller needs only one check.
	ErrDisconnected = transport.ErrDisconnected
	// ErrOutOfBand means the requested frequency is outside limits.bands. Maps
	// to 422.
	ErrOutOfBand = errors.New("rig: frequency outside configured band limits")
	// ErrUnsupported means this radio cannot do what was asked. Maps to 422.
	ErrUnsupported = errors.New("rig: unsupported by this radio")
	// ErrNAK means the rig rejected the command.
	ErrNAK = errors.New("rig: command rejected by radio")
	// ErrNoKeys is a backend bug: Do was called with no reply keys. Send is the
	// call for commands that are not answered.
	ErrNoKeys = errors.New("rig: Do requires at least one reply key")
)

// Defaults applied when the corresponding configuration value is zero.
const (
	defaultCmdTimeout  = time.Second
	defaultPollFast    = 500 * time.Millisecond
	defaultPollSlow    = 5 * time.Second
	defaultBackoffMin  = 100 * time.Millisecond
	defaultBackoffMax  = 30 * time.Second
	maxPollFailures    = 5
	forceRXTimeoutMult = 2
)

// Option configures a Session or a Manager. Options that do not apply to the
// target are ignored, so one type covers both constructors.
type Option func(*options)

type options struct {
	log       *slog.Logger
	timeout   time.Duration
	queue     int
	dialerFor DialerFactory
}

// WithLogger sets the logger. The session adds a "radio" attribute itself.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.log = l
		}
	}
}

// WithCommandTimeout sets how long a single transaction waits for its reply.
// A rig that has not answered in a second is not going to.
func WithCommandTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithEventQueue sets the per-subscriber buffer depth.
func WithEventQueue(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.queue = n
		}
	}
}

func resolve(opts []Option) options {
	o := options{
		log:     slog.Default(),
		timeout: defaultCmdTimeout,
		queue:   defaultQueue,
	}
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Session is one radio: its connection supervisor, reader, poller, state cache
// and event fan-out. Every exported method is safe for concurrent use.
type Session struct {
	id      string
	name    string
	backend string
	cfg     config.Radio
	rig     backend.Rig
	dialer  transport.Dialer
	log     *slog.Logger

	cmdTimeout time.Duration
	pollFast   time.Duration
	pollSlow   time.Duration
	backoffMin time.Duration
	backoffMax time.Duration

	// state is read by the API and the WebSocket layer without ever touching a
	// mutex; stateMu serialises only the read-modify-write of publishers, so
	// that Seq stays monotonic and no update is lost.
	state   atomic.Pointer[radio.State]
	stateMu sync.Mutex

	caps atomic.Pointer[radio.Caps]
	conn atomic.Pointer[conn]
	// connected mirrors State.Connected for cheap checks on the write path. It
	// is set only after Init has completed, so a half-initialised rig is never
	// exposed to callers.
	connected atomic.Bool

	subs *subscribers

	cwMu     sync.RWMutex
	cwSender cw.Sender

	// deadman forces RX when a transmission outlives limits.tx_timeout, whether
	// or not the client that started it still exists.
	deadmanMu sync.Mutex
	deadman   *time.Timer

	startOnce sync.Once
	closeOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewSession builds a session for one configured radio. It performs no I/O;
// the connection is made by Start.
func NewSession(rc config.Radio, r backend.Rig, d transport.Dialer, opts ...Option) (*Session, error) {
	if r == nil {
		return nil, fmt.Errorf("rig: radio %q has no backend", rc.ID)
	}
	if d == nil {
		return nil, fmt.Errorf("rig: radio %q has no dialer", rc.ID)
	}
	o := resolve(opts)

	s := &Session{
		id:         rc.ID,
		name:       rc.Name,
		backend:    rc.Backend,
		cfg:        rc,
		rig:        r,
		dialer:     d,
		log:        o.log.With("radio", rc.ID),
		cmdTimeout: o.timeout,
		pollFast:   orDefault(rc.Poll.Interval.D(), defaultPollFast),
		pollSlow:   orDefault(rc.Poll.SlowInterval.D(), defaultPollSlow),
		backoffMin: defaultBackoffMin,
		backoffMax: defaultBackoffMax,
		subs:       newSubscribers(o.queue),
	}
	st := radio.State{UpdatedAt: time.Now()}
	s.state.Store(&st)
	caps := r.Caps()
	s.caps.Store(&caps)
	return s, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// ID is the stable, URL-safe identifier from the configuration.
func (s *Session) ID() string { return s.id }

// Name is the human-readable name.
func (s *Session) Name() string { return s.name }

// Backend is the configured backend name, for the radio descriptor.
func (s *Session) Backend() string { return s.backend }

// Limits reports the configured safety interlocks, so the API can publish them.
func (s *Session) Limits() config.Limits { return s.cfg.Limits }

// Caps describes what this radio supports. It is refreshed after Init, since a
// backend may learn more from the rig than it knew from the configuration.
func (s *Session) Caps() radio.Caps { return *s.caps.Load() }

// State returns a snapshot. It never blocks on the serial port.
func (s *Session) State() radio.State { return *s.state.Load() }

// Connected reports whether the radio is currently usable.
func (s *Session) Connected() bool { return s.connected.Load() }

// Subscribe returns a channel of events and a function that unsubscribes and
// closes it. The channel is buffered and sends to it are non-blocking: a
// subscriber that stops reading loses events (reported through Event.Dropped)
// but can never slow the session down.
func (s *Session) Subscribe() (<-chan Event, func()) { return s.subs.add() }

// SetCWSender installs the Morse sender. Wiring is done by main, because the
// sender needs the backend and the session's Conn, and the session must not
// know which of the two CW methods is in use.
func (s *Session) SetCWSender(snd cw.Sender) {
	s.cwMu.Lock()
	s.cwSender = snd
	s.cwMu.Unlock()
}

// CW returns the installed sender, or nil.
func (s *Session) CW() cw.Sender {
	s.cwMu.RLock()
	defer s.cwMu.RUnlock()
	return s.cwSender
}

// Do implements backend.Conn against whichever connection is current, so a
// long-lived holder — the CW sender, in particular — keeps working across a
// reconnect without being re-wired.
func (s *Session) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	c := s.conn.Load()
	if c == nil {
		return backend.Update{}, fmt.Errorf("radio %s: %w", s.id, ErrDisconnected)
	}
	return c.Do(ctx, req, want...)
}

// Send implements backend.Conn. See Do.
func (s *Session) Send(ctx context.Context, req []byte) error {
	c := s.conn.Load()
	if c == nil {
		return fmt.Errorf("radio %s: %w", s.id, ErrDisconnected)
	}
	return c.Send(ctx, req)
}

// applyUpdate folds a decoded frame into the state cache. Called from the reader
// goroutine for every frame, solicited or not.
func (s *Session) applyUpdate(up backend.Update) {
	if up.Patch.Empty() {
		return
	}
	s.apply(up.Patch, EventState, "")
}

// apply is the single writer path into the state cache.
//
// Side effects (event publication, dead-man arming) deliberately happen after
// stateMu is released: they take other locks, and nesting them under the state
// mutex would make lock ordering a thing anyone had to think about.
func (s *Session) apply(p radio.Patch, kind EventKind, reason string) {
	if p.Empty() {
		return
	}
	s.stateMu.Lock()
	old := *s.state.Load()
	next := old.Apply(p)
	next.UpdatedAt = time.Now()
	changed := old.Diff(next)
	if !changed.Empty() {
		// Seq advances only on a real change, so a WebSocket client can use it
		// for gap detection without seeing churn from unchanged poll replies.
		next.Seq = old.Seq + 1
	}
	s.state.Store(&next)
	s.stateMu.Unlock()

	if changed.Empty() {
		return
	}

	if changed.PTT != nil {
		if *changed.PTT {
			s.armDeadman()
		} else {
			s.disarmDeadman()
		}
	}

	s.subs.publish(Event{
		Kind:    kind,
		RadioID: s.id,
		Seq:     next.Seq,
		At:      next.UpdatedAt,
		State:   next,
		Patch:   changed,
		Err:     reason,
	})
}

// refreshCW copies the sender's queue status into the state cache. The CW queue
// lives in the sender rather than in a Patch, so it gets its own small path
// instead of being forced through Apply.
func (s *Session) refreshCW() {
	snd := s.CW()
	if snd == nil {
		return
	}
	st := snd.Status()

	s.stateMu.Lock()
	old := *s.state.Load()
	if old.CW == st {
		s.stateMu.Unlock()
		return
	}
	next := old
	next.CW = st
	next.UpdatedAt = time.Now()
	next.Seq = old.Seq + 1
	s.state.Store(&next)
	s.stateMu.Unlock()

	s.subs.publish(Event{
		Kind:    EventCW,
		RadioID: s.id,
		Seq:     next.Seq,
		At:      next.UpdatedAt,
		State:   next,
	})
}

// requireConnected is the guard on every write path.
func (s *Session) requireConnected() error {
	if !s.connected.Load() || s.conn.Load() == nil {
		return fmt.Errorf("radio %s: %w", s.id, ErrDisconnected)
	}
	return nil
}

// SetFrequency tunes the radio, rejecting anything outside limits.bands.
func (s *Session) SetFrequency(ctx context.Context, vfo radio.VFO, hz uint64) (radio.State, error) {
	// Validate before checking the link. A request that is illegal for this
	// station is illegal whether or not the radio happens to be reachable, and
	// reporting 503 for an out-of-band frequency would tell the operator to
	// retry something that must never succeed.
	if !s.cfg.Limits.AllowsFrequency(hz) {
		return s.State(), fmt.Errorf("radio %s: %d Hz: %w", s.id, hz, ErrOutOfBand)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := s.rig.SetFrequency(ctx, s, vfo, hz); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollFast)
}

// SetMode selects the operating mode. DataMode is orthogonal on Kenwood (MD +
// DA), so it is carried separately rather than folded into Mode.
func (s *Session) SetMode(ctx context.Context, m radio.Mode, dataMode bool) (radio.State, error) {
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if caps := s.Caps(); len(caps.Modes) > 0 && !caps.SupportsMode(m) {
		return s.State(), fmt.Errorf("radio %s: mode %s: %w", s.id, m, ErrUnsupported)
	}
	if err := s.rig.SetMode(ctx, s, m, dataMode); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollFast, backend.PollSlow)
}

// SetPower sets transmit power, clamped to the configured maximum. The returned
// state carries what the rig actually did, which is not necessarily what was
// asked for: rigs clamp and round silently.
func (s *Session) SetPower(ctx context.Context, p radio.PowerSet) (radio.State, error) {
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	clamped, err := s.clampPower(p)
	if err != nil {
		return s.State(), err
	}
	if err := s.rig.SetPower(ctx, s, clamped); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetPTT keys or unkeys the transmitter. Keying arms the dead-man timer, which
// happens in the state-apply path so that PTT asserted from the front panel is
// covered too.
func (s *Session) SetPTT(ctx context.Context, on bool) (radio.State, error) {
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := s.rig.SetPTT(ctx, s, on); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollFast)
}

// SetFilterWidth sets the IF passband in Hz.
func (s *Session) SetFilterWidth(ctx context.Context, hz int) (radio.State, error) {
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if !s.Caps().FilterWidth {
		return s.State(), fmt.Errorf("radio %s: filter width: %w", s.id, ErrUnsupported)
	}
	if err := s.rig.SetFilterWidth(ctx, s, hz); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetFilterSlot selects an IF filter: FIL1-3 on Icom, IF Filter A/B on Kenwood.
func (s *Session) SetFilterSlot(ctx context.Context, slot int) (radio.State, error) {
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if n := s.Caps().FilterSlots; n <= 0 || slot < 1 || slot > n {
		return s.State(), fmt.Errorf("radio %s: filter slot %d: %w", s.id, slot, ErrUnsupported)
	}
	if err := s.rig.SetFilterSlot(ctx, s, slot); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// PatchRequest is a partial, atomic state change: the request body of
// PATCH /state. A nil field means "leave this alone".
type PatchRequest struct {
	Mode     *radio.Mode
	DataMode *bool
	// VFO selects which VFO Frequency addresses; the zero value is the current
	// one.
	VFO           radio.VFO
	Frequency     *uint64
	FilterSlot    *int
	FilterWidthHz *int
	Power         *radio.PowerSet
	PTT           *bool
}

// Empty reports whether the request would change nothing.
func (r PatchRequest) Empty() bool {
	return r.Mode == nil && r.DataMode == nil && r.Frequency == nil &&
		r.FilterSlot == nil && r.FilterWidthHz == nil && r.Power == nil && r.PTT == nil
}

// ApplyPatch performs a multi-field change as one ordered transaction.
//
// The order is the point of the endpoint existing at all. Mode goes first,
// because on both target rigs selecting a mode can change the filter and the
// carrier offset, which would silently undo a frequency or filter set applied
// before it. PTT goes last, so the radio is fully configured before it keys.
//
// Every field is validated against the configured limits BEFORE anything is
// written, so a rejected request does not leave the rig half-changed.
func (s *Session) ApplyPatch(ctx context.Context, req PatchRequest) (radio.State, error) {
	if req.Empty() {
		return s.State(), nil
	}

	// Validation precedes the connection check: a request that is illegal for
	// this station is illegal whether or not the radio is reachable, and
	// answering 503 would invite the operator to retry something that must
	// never succeed.
	caps := s.Caps()
	if req.Mode != nil && len(caps.Modes) > 0 && !caps.SupportsMode(*req.Mode) {
		return s.State(), fmt.Errorf("radio %s: mode %s: %w", s.id, *req.Mode, ErrUnsupported)
	}
	if req.Frequency != nil && !s.cfg.Limits.AllowsFrequency(*req.Frequency) {
		return s.State(), fmt.Errorf("radio %s: %d Hz: %w", s.id, *req.Frequency, ErrOutOfBand)
	}
	if req.FilterWidthHz != nil && !caps.FilterWidth {
		return s.State(), fmt.Errorf("radio %s: filter width: %w", s.id, ErrUnsupported)
	}
	if req.FilterSlot != nil {
		if n := caps.FilterSlots; n <= 0 || *req.FilterSlot < 1 || *req.FilterSlot > n {
			return s.State(), fmt.Errorf("radio %s: filter slot %d: %w", s.id, *req.FilterSlot, ErrUnsupported)
		}
	}
	var power radio.PowerSet
	if req.Power != nil {
		p, err := s.clampPower(*req.Power)
		if err != nil {
			return s.State(), err
		}
		power = p
	}

	// Everything the station's own rules can reject has now been rejected, so
	// from here on a failure really is a link problem.
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}

	slow := req.Power != nil || req.FilterSlot != nil || req.FilterWidthHz != nil

	// Data mode is orthogonal to mode, so a request that changes only one of
	// the two has to resend the other as the rig currently has it.
	if req.Mode != nil || req.DataMode != nil {
		cur := s.State()
		m, dm := cur.Mode, cur.DataMode
		if req.Mode != nil {
			m = *req.Mode
		}
		if req.DataMode != nil {
			dm = *req.DataMode
		}
		if err := s.rig.SetMode(ctx, s, m, dm); err != nil {
			return s.State(), err
		}
		slow = true // mode selection moves the filter on both target rigs
	}
	if req.Frequency != nil {
		if err := s.rig.SetFrequency(ctx, s, req.VFO, *req.Frequency); err != nil {
			return s.State(), err
		}
	}
	if req.FilterSlot != nil {
		if err := s.rig.SetFilterSlot(ctx, s, *req.FilterSlot); err != nil {
			return s.State(), err
		}
	}
	if req.FilterWidthHz != nil {
		if err := s.rig.SetFilterWidth(ctx, s, *req.FilterWidthHz); err != nil {
			return s.State(), err
		}
	}
	if req.Power != nil {
		if err := s.rig.SetPower(ctx, s, power); err != nil {
			return s.State(), err
		}
	}
	if req.PTT != nil {
		if err := s.rig.SetPTT(ctx, s, *req.PTT); err != nil {
			return s.State(), err
		}
	}

	if slow {
		return s.readback(ctx, backend.PollSlow, backend.PollFast)
	}
	return s.readback(ctx, backend.PollFast)
}

// readback re-reads the radio after a write, so callers see reality rather than
// intent. Rigs clamp power per band, refuse filter slots that the current mode
// does not have, and round frequencies to their tuning step, all without saying
// so.
func (s *Session) readback(ctx context.Context, tiers ...backend.PollTier) (radio.State, error) {
	for _, t := range tiers {
		err := s.rig.Poll(ctx, s, t)
		if err == nil {
			continue
		}
		// The write already succeeded; this is only the confirming read. If the
		// rig declined one of the optional commands in the tier — an Icom
		// acknowledging an unimplemented command with FB, a Kenwood answering
		// ?; — failing the request would report a change that did happen as an
		// error, and tempt the operator into repeating it.
		if !isFatalPollErr(err) {
			s.log.Debug("partial read-back after write", "radio", s.id, "err", err)
			continue
		}
		return s.State(), fmt.Errorf("radio %s: read-back after write: %w", s.id, err)
	}
	return s.State(), nil
}

// clampPower enforces limits.max_power_* server-side. A request for 100 gets 80
// and a response saying 80 — the operator is told what the radio is actually
// doing, not what they asked for.
func (s *Session) clampPower(p radio.PowerSet) (radio.PowerSet, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	lim := s.cfg.Limits
	caps := s.Caps()

	if p.Pct != nil {
		v := *p.Pct
		v = min(max(v, 0), 100)
		if lim.MaxPowerPct > 0 {
			v = min(v, lim.MaxPowerPct)
		}
		// A watt limit on a rig with a known full-scale wattage still binds a
		// percentage request; without a scale there is nothing to compare.
		if lim.MaxPowerW > 0 && caps.MaxPowerW > 0 {
			v = min(v, lim.MaxPowerW/caps.MaxPowerW*100)
		}
		return radio.PowerSet{Pct: &v}, nil
	}

	if !caps.PowerWattAccurate && caps.MaxPowerW <= 0 {
		return p, fmt.Errorf("radio %s: power in watts: %w", s.id, ErrUnsupported)
	}
	v := max(*p.Watts, 0)
	if lim.MaxPowerW > 0 {
		v = min(v, lim.MaxPowerW)
	}
	if lim.MaxPowerPct > 0 && caps.MaxPowerW > 0 {
		v = min(v, lim.MaxPowerPct/100*caps.MaxPowerW)
	}
	return radio.PowerSet{Watts: &v}, nil
}

// ForceRX is the safety path: drop PTT and stop CW, right now, regardless of
// what any client thinks. It is called by the TX dead-man timer and by lock
// expiry, and it never returns an error, because there is nobody left to tell.
//
// It may block for up to a couple of command timeouts, so callers on a timer
// goroutine should invoke it from their own goroutine.
func (s *Session) ForceRX(reason string) {
	s.log.Warn("forcing receive", "reason", reason)
	s.disarmDeadman()

	if snd := s.CW(); snd != nil {
		// Both halves matter: the local queue, and whatever is already inside
		// the rig's own buffer.
		snd.Abort()
	}

	if s.conn.Load() == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), forceRXTimeoutMult*s.cmdTimeout)
	defer cancel()
	if err := s.rig.SetPTT(ctx, s, false); err != nil {
		s.log.Error("force receive failed to drop PTT", "reason", reason, "err", err)
		return
	}
	if err := s.rig.Poll(ctx, s, backend.PollFast); err != nil {
		s.log.Debug("force receive: read-back failed", "err", err)
	}
	s.refreshCW()
}

// armDeadman starts the TX timeout. It fires even if the HTTP client that keyed
// the radio has vanished, which is the entire point: a crashed client must not
// leave a carrier up.
func (s *Session) armDeadman() {
	d := s.cfg.Limits.TXTimeout.D()
	if d <= 0 {
		return
	}
	s.deadmanMu.Lock()
	defer s.deadmanMu.Unlock()
	if s.deadman != nil {
		s.deadman.Stop()
	}
	s.deadman = time.AfterFunc(d, func() {
		s.log.Warn("TX timeout expired, forcing receive", "timeout", d)
		s.ForceRX(fmt.Sprintf("tx_timeout %s expired", d))
	})
}

func (s *Session) disarmDeadman() {
	s.deadmanMu.Lock()
	defer s.deadmanMu.Unlock()
	if s.deadman != nil {
		s.deadman.Stop()
		s.deadman = nil
	}
}

// Start launches the connection supervisor. It returns immediately; the radio
// may not be connected yet, and the API is expected to serve state from the
// cache regardless.
func (s *Session) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.supervise(ctx)
		}()
	})
}

// Close stops the supervisor and waits for every goroutine to finish.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
	s.wg.Wait()
	s.disarmDeadman()
	s.subs.closeAll()
	return nil
}
