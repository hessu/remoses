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
	// ErrStandby means the radio is reachable but switched off. Maps to 503.
	//
	// Distinct from ErrDisconnected because the remedy is different and a
	// client should offer it: nothing is wrong with the link, and the radio can
	// be woken over the same connection that just refused the command. It
	// wraps ErrDisconnected so that anything reasoning about "no usable radio
	// right now" still matches with one check.
	ErrStandby = fmt.Errorf("rig: the radio is switched off: %w", transport.ErrDisconnected)
	// ErrOutOfBand means the requested frequency is outside limits.bands. Maps
	// to 422.
	ErrOutOfBand = errors.New("rig: frequency outside configured band limits")
	// ErrUnsupported means this radio cannot do what was asked. Maps to 422.
	// It aliases the backend sentinel, the way ErrBusy does, so that a backend
	// refusing a control it does not have — which it cannot report as
	// rig.ErrUnsupported, being unable to import this package — reaches the
	// client as 422 with its own explanation rather than as a bare 500.
	ErrUnsupported = backend.ErrUnsupported
	// ErrNAK means the rig rejected the command.
	ErrNAK = errors.New("rig: command rejected by radio")
	// ErrBusy means the rig said "not now" rather than refusing outright —
	// Yaesu's ?;. Maps to 503 and never to 422: the request was well formed and
	// repeating it is the recovery, so it must not be confused with ErrNAK. It
	// aliases the backend sentinel, the way ErrDisconnected aliases the
	// transport one, so a caller needs only one check.
	ErrBusy = backend.ErrBusy
	// ErrNoKeys is a backend bug: Do was called with no reply keys. Send is the
	// call for commands that are not answered.
	ErrNoKeys = errors.New("rig: Do requires at least one reply key")
)

// Defaults applied when the corresponding configuration value is zero.
const (
	defaultCmdTimeout = time.Second
	defaultPollFast   = 500 * time.Millisecond
	defaultPollSlow   = 5 * time.Second
	defaultBackoffMin = 100 * time.Millisecond
	// defaultBackoffMax bounds how long a replugged radio can go unnoticed,
	// which is the number an operator actually experiences.
	//
	// It was 30 s, on the reasoning that an absent radio should not have the
	// daemon enumerating ports forever. Measured on hardware, that reasoning
	// bought very little and cost a lot: a USB cable pulled and reseated took
	// 56 s to come back, of which about 35 s was the supervisor asleep at the
	// ceiling. A rig that takes half a minute to return after you reseat a
	// cable reads as broken.
	//
	// What a failed dial actually costs decides the trade. With port.device it
	// is one open() on a path that is not there — microseconds, and free even
	// once a second. With port.match it is a USB enumeration, which is heavier
	// but still nothing at this interval. Five seconds is the compromise:
	// six-fold better worst-case latency for a syscall every five seconds on a
	// radio that is switched off anyway.
	defaultBackoffMax  = 5 * time.Second
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

	// wireDebug traces every frame to and from this radio. It belongs to the
	// session rather than to a backend because the session is the only layer
	// that sees the bytes of all four of them — backends never touch the
	// transport. See wirelog.go.
	wireDebug bool

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

	// standby is a radio that is reachable but switched off: answering, and
	// refusing everything. The link is up, so commands are refused with a
	// message that says so rather than with "not connected".
	standby atomic.Bool
	// poweredOff records that remoses switched the radio off, so the
	// disconnection that follows is logged as expected rather than as a fault.
	// Cleared by any successful connection, since a radio that is talking is
	// evidently not off any more.
	poweredOff atomic.Bool
	// wakeWanted is a power-on waiting for a port. The supervisor consumes it
	// on the next freshly opened port, before Init, which is the only moment a
	// sleeping radio can be reached.
	wakeWanted atomic.Bool

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
		wireDebug:  rc.DebugWire,
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
//
// The published capabilities are refreshed here, because installing a sender
// can change them: see publishCaps.
func (s *Session) SetCWSender(snd cw.Sender) {
	s.cwMu.Lock()
	s.cwSender = snd
	s.cwMu.Unlock()
	s.publishCaps()
}

// publishCaps stores the capabilities clients see: what the backend says about
// the radio, corrected by what the installed CW sender says about keying.
//
// The correction is needed because those two can disagree, and did. A backend
// reports the radio's own keyer — civ says cw_method: cat, because an IC-7610
// has a CAT buffer — but cw.method: serial_key on that same radio installs a
// local keyer that drives a DTR line and never sends command 17. Publishing the
// backend's answer told clients the radio keyed over CAT, and handed them the
// rig keyer's charset and speed range for a keyer that was not in use.
//
// It must be called wherever caps are stored, not once at startup: every
// reconnect re-reads them from the backend (a backend may learn more from the
// rig than it knew from the configuration) and would otherwise put the wrong
// answer back.
func (s *Session) publishCaps() {
	caps := s.rig.Caps()
	if snd := s.CW(); snd != nil {
		caps.CWMethod = snd.Method()
		caps.CWCharset = snd.Charset()
		if lo, hi, ok := snd.WPMRange(); ok {
			caps.CWMinWPM, caps.CWMaxWPM = lo, hi
		}
	}
	s.caps.Store(&caps)
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
	// Reachable but switched off. Saying so beats letting the command through
	// to be refused by the radio, which answers NG to everything in standby and
	// so produces a rejection that says nothing about why.
	//
	// Wrapped as ErrDisconnected so it answers 503: this is a temporary
	// condition with an obvious remedy, not a request the station will never
	// accept. The message carries the remedy.
	if s.standby.Load() {
		return fmt.Errorf("radio %s: %w", s.id, ErrStandby)
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

// dualVFO reports the backend's dual-VFO interface, or nil where the radio can
// only reach the VFO it is on.
//
// Asked of the backend rather than of Caps, because this is the type assertion
// the write paths need; Caps is the same fact published for clients, and
// TestCapsAgreeWithTheInterface holds the two together.
func (s *Session) dualVFO() backend.DualVFO {
	d, _ := s.rig.(backend.DualVFO)
	return d
}

// breakInController reports the backend's CW break-in control, or nil.
func (s *Session) breakInController() backend.BreakInController {
	b, _ := s.rig.(backend.BreakInController)
	return b
}

// SetBreakIn changes the CW break-in setting.
func (s *Session) SetBreakIn(ctx context.Context, v radio.BreakIn) (radio.State, error) {
	b := s.breakInController()
	if b == nil || !s.Caps().BreakInControl {
		return s.State(), fmt.Errorf("radio %s: break-in: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := b.SetBreakIn(ctx, s, v); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// powerSwitch reports the backend's power control, or nil.
func (s *Session) powerSwitch() backend.PowerSwitch {
	p, _ := s.rig.(backend.PowerSwitch)
	return p
}

// PowerOff switches the radio off. deep asks for the lowest standby current the
// radio offers, where it has a choice.
//
// The success of this command looks exactly like its failure: the radio stops
// answering, the next poll times out, and the session tears the link down. So
// the intent is recorded before the command goes out, and the supervisor treats
// the disconnection that follows as expected rather than as a fault — otherwise
// switching a radio off would fill the log with errors about a radio doing
// precisely what it was told.
func (s *Session) PowerOff(ctx context.Context, deep bool) (radio.State, error) {
	p := s.powerSwitch()
	if p == nil || !s.Caps().PowerSwitch {
		return s.State(), fmt.Errorf("radio %s: switching the radio off: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	// Before the command, not after: the link may not survive long enough for
	// anything after it to run.
	s.poweredOff.Store(true)
	if err := p.PowerOff(ctx, s, deep); err != nil {
		s.poweredOff.Store(false)
		return s.State(), err
	}
	s.log.Info("radio switched off over CAT", "deep", deep)
	// No read-back. There is nothing left to read from, and asking would spend
	// a command timeout confirming what the operator just asked for.
	return s.State(), nil
}

// PowerOn wakes the radio.
//
// It works in the state where it is needed, which is the whole difficulty: a
// radio that is off is disconnected, so there is no live link to send on. What
// there usually IS is an openable port — an external CI-V interface stays
// powered, and a radio whose USB survives its own power switch presents one too
// — and the supervisor is already looping on it, dialling and failing to Init.
//
// So this arms a request rather than sending anything itself. The supervisor
// picks it up on its next attempt and sends the wake on the freshly opened port
// BEFORE Init, which is the one moment a sleeping radio can be reached. On a
// link that is already up it is sent immediately instead, since the port is in
// hand and the radio may be awake but for a front panel somebody switched.
//
// Racing the supervisor for the port would be the obvious alternative and is
// the wrong one: these are exclusive devices, and two dialers would produce a
// wake that fails because the port was busy.
func (s *Session) PowerOn(ctx context.Context) (radio.State, error) {
	p := s.powerSwitch()
	if p == nil || !s.Caps().PowerSwitch {
		return s.State(), fmt.Errorf("radio %s: switching the radio on: %w", s.id, ErrUnsupported)
	}
	s.poweredOff.Store(false)

	// An open port is the whole requirement, and "connected" is not the same
	// question: a radio in standby is answering NG to everything, so the
	// session is parked in awaitWake holding a perfectly good port open. Waiting
	// for a reconnection there would wait forever, because that loop exists to
	// prevent one.
	if s.conn.Load() != nil {
		if err := p.PowerOn(ctx, s); err != nil {
			return s.State(), err
		}
		s.log.Info("radio sent a wake-up on the open port")
		return s.State(), nil
	}

	// Not connected: hand it to the supervisor, which owns the port.
	s.wakeWanted.Store(true)
	s.log.Info("radio wake-up armed; it will be sent on the next connection attempt")
	return s.State(), nil
}

// takeWakeRequest consumes a pending wake, if there is one. The supervisor
// calls it on a freshly opened port.
func (s *Session) takeWakeRequest() bool { return s.wakeWanted.Swap(false) }

// tunerController reports the backend's antenna tuner, or nil.
func (s *Session) tunerController() backend.TunerController {
	t, _ := s.rig.(backend.TunerController)
	return t
}

// SetTuner puts the antenna tuner in line or bypasses it.
//
// An ordinary setting: it changes what the matching network is doing but does
// not transmit. Starting a tuning cycle does, and lives in StartTune.
func (s *Session) SetTuner(ctx context.Context, on bool) (radio.State, error) {
	t := s.tunerController()
	if t == nil || !s.Caps().TunerControl {
		return s.State(), fmt.Errorf("radio %s: antenna tuner: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := t.SetTuner(ctx, s, on); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// StartTune runs a tuning cycle, which transmits.
//
// It is gated like any other transmit path rather than like a setting. The
// radio keys itself for a second or two with nobody holding a switch, on
// whatever frequency the rig is sitting on, so:
//
//   - the frequency is checked against limits.bands, because a tune is a
//     transmission and a station that may not transmit on a band may not tune
//     into it either;
//   - the dead-man timer is armed, so a cycle that never ends — a tuner hunting
//     into an antenna that cannot be matched — is caught by the same interlock
//     that catches a stuck PTT.
//
// The cycle is not waited on. The rig decides how long it takes, reports
// progress as radio.TunerTuning, and the poller follows it back to on or off;
// blocking here would hold a lock and a request open for the duration and tell
// the caller nothing it could not see in the state.
func (s *Session) StartTune(ctx context.Context) (radio.State, error) {
	t := s.tunerController()
	if t == nil || !s.Caps().TunerTune {
		return s.State(), fmt.Errorf("radio %s: starting a tuning cycle: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if hz := s.State().Frequency; hz > 0 && !s.cfg.Limits.AllowsFrequency(hz) {
		return s.State(), fmt.Errorf("radio %s: tuning transmits on %d Hz: %w",
			s.id, hz, ErrOutOfBand)
	}
	if err := t.StartTune(ctx, s); err != nil {
		return s.State(), err
	}
	s.armDeadman()
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// frontEnd reports the backend's receive front-end controls, or nil.
func (s *Session) frontEnd() backend.FrontEndController {
	f, _ := s.rig.(backend.FrontEndController)
	return f
}

// preselect reports the backend's IP+ and DIGI-SEL controls, or nil.
func (s *Session) preselect() backend.PreselectController {
	p, _ := s.rig.(backend.PreselectController)
	return p
}

// validateFrontEnd rejects every front-end field the radio cannot honour,
// before any of them reaches the wire.
//
// All of it up front, like the rest of ApplyPatch's validation, so that a patch
// asking for a preamp the radio has and an attenuator step it does not leaves
// the receiver as it was rather than half-changed.
func (s *Session) validateFrontEnd(req PatchRequest, caps radio.Caps) error {
	if !req.frontEndRequested() {
		return nil
	}
	if req.Preamp != nil || req.AttenuatorDB != nil || req.RFGain != nil || req.AGC != nil {
		if s.frontEnd() == nil {
			return fmt.Errorf("radio %s: receive front end: %w", s.id, ErrUnsupported)
		}
	}
	if req.IPPlus != nil || req.DigiSel != nil || req.DigiSelShift != nil {
		if s.preselect() == nil {
			return fmt.Errorf("radio %s: preselector: %w", s.id, ErrUnsupported)
		}
	}
	if req.Preamp != nil {
		if caps.PreampLevels == 0 {
			return fmt.Errorf("radio %s: preamplifier: %w", s.id, ErrUnsupported)
		}
		if *req.Preamp < 0 || *req.Preamp > caps.PreampLevels {
			return fmt.Errorf("radio %s: preamplifier %d, want 0 to %d: %w",
				s.id, *req.Preamp, caps.PreampLevels, ErrUnsupported)
		}
	}
	if req.AttenuatorDB != nil {
		if !caps.AttenuatorControl() {
			return fmt.Errorf("radio %s: attenuator: %w", s.id, ErrUnsupported)
		}
		if !caps.SupportsAttenuation(*req.AttenuatorDB) {
			return fmt.Errorf("radio %s: no %d dB attenuator setting, only 0 and %v: %w",
				s.id, *req.AttenuatorDB, caps.AttenuatorDB, ErrUnsupported)
		}
	}
	if req.RFGain != nil {
		if !caps.RFGainControl {
			return fmt.Errorf("radio %s: RF gain: %w", s.id, ErrUnsupported)
		}
		if err := percentInRange("RF gain", *req.RFGain); err != nil {
			return fmt.Errorf("radio %s: %w", s.id, err)
		}
	}
	if req.AGC != nil {
		if !caps.AGCControl() {
			return fmt.Errorf("radio %s: AGC: %w", s.id, ErrUnsupported)
		}
		// Two different mistakes, worth two different messages. Asking for a
		// speed this radio has not got is one; echoing back an auto-resolved
		// reading — "auto-mid", which a Yaesu reports and will not accept — is
		// the other, and a client that reads a state and writes it back will
		// make exactly that one.
		if !req.AGC.Settable() {
			return fmt.Errorf("radio %s: AGC %q is a reading rather than a setting; "+
				"ask for %q and the radio will choose: %w",
				s.id, *req.AGC, radio.AGCAuto, ErrUnsupported)
		}
		if !caps.SupportsAGC(*req.AGC) {
			return fmt.Errorf("radio %s: no AGC setting %q, only %v: %w",
				s.id, *req.AGC, caps.AGCSettings, ErrUnsupported)
		}
	}
	if req.IPPlus != nil && !caps.IPPlusControl {
		return fmt.Errorf("radio %s: IP+: %w", s.id, ErrUnsupported)
	}
	if req.DigiSel != nil && !caps.DigiSelControl {
		return fmt.Errorf("radio %s: DIGI-SEL preselector: %w", s.id, ErrUnsupported)
	}
	if req.DigiSelShift != nil {
		if !caps.DigiSelShiftControl {
			return fmt.Errorf("radio %s: DIGI-SEL shift: %w", s.id, ErrUnsupported)
		}
		if err := percentInRange("DIGI-SEL shift", *req.DigiSelShift); err != nil {
			return fmt.Errorf("radio %s: %w", s.id, err)
		}
	}
	return nil
}

// applyFrontEnd writes the front-end fields a request carries.
//
// The order is the signal path: the preamplifier and the attenuator, which sit
// ahead of the first mixer, then the gain controls behind them. It matters only
// in one direction — winding an attenuator in before switching a preamplifier
// off is the quiet way round, and the reverse can put a loud band through a
// preamplifier for the length of one CAT transaction.
func (s *Session) applyFrontEnd(ctx context.Context, req PatchRequest) error {
	if !req.frontEndRequested() {
		return nil
	}
	if req.AttenuatorDB != nil {
		if err := s.frontEnd().SetAttenuator(ctx, s, *req.AttenuatorDB); err != nil {
			return err
		}
	}
	if req.Preamp != nil {
		if err := s.frontEnd().SetPreamp(ctx, s, *req.Preamp); err != nil {
			return err
		}
	}
	// The preselector goes with the stages it sits among, and its shift after
	// it: switching DIGI-SEL in and then placing it is the order a client would
	// write, and the radio keeps the shift either way.
	if req.IPPlus != nil {
		if err := s.preselect().SetIPPlus(ctx, s, *req.IPPlus); err != nil {
			return err
		}
	}
	if req.DigiSel != nil {
		if err := s.preselect().SetDigiSel(ctx, s, *req.DigiSel); err != nil {
			return err
		}
	}
	if req.DigiSelShift != nil {
		if err := s.preselect().SetDigiSelShift(ctx, s, *req.DigiSelShift); err != nil {
			return err
		}
	}
	if req.AGC != nil {
		if err := s.frontEnd().SetAGC(ctx, s, *req.AGC); err != nil {
			return err
		}
	}
	if req.RFGain != nil {
		if err := s.frontEnd().SetRFGain(ctx, s, *req.RFGain); err != nil {
			return err
		}
	}
	return nil
}

// percentInRange is the shared 0-100 check for the front end's two level
// controls.
func percentInRange(what string, pct float64) error {
	if pct < 0 || pct > 100 {
		return fmt.Errorf("%s %.1f%%, want 0 to 100: %w", what, pct, ErrUnsupported)
	}
	return nil
}

// EnsureCWWillTransmit makes sure Morse queued now would actually reach the
// air, switching break-in on if configuration allows it.
//
// It exists because the failure it prevents is invisible: with break-in off and
// nothing keying by hand, the rig accepts the CW command, drains its buffer on
// schedule and transmits nothing. Every signal remoses has says success — the
// queue empties, no error comes back — and the operator hears silence. That has
// now happened twice on real radios, an IC-9700 and a TS-590S, and both times
// it was the operator noticing, not remoses.
//
// cw.break_in decides what is done about it. Under "semi" or "full" the setting
// is written, because a remote operator has no way to reach the rig's front
// panel and a knob they cannot turn is not a safety feature. Under "manual"
// nothing is written and a rig known to have break-in off is refused, which is
// for the station that sequences its own transmit path.
//
// The asymmetry between known-off and unknown is deliberate throughout: a
// refusal on an unknown is how a safety check turns into an outage. Where the
// backend cannot report break-in at all, this says nothing.
//
// PTT already being up is accepted, because that is one of the conditions the
// references name, and an operator holding the transmitter down is entitled to
// send into it.
func (s *Session) EnsureCWWillTransmit(ctx context.Context) error {
	b := s.breakInController()
	if b == nil {
		return nil
	}
	v := b.BreakIn()
	if v.Transmits() || s.State().PTT {
		return nil
	}

	want, manage := configuredBreakIn(s.cfg.CW.BreakIn)
	if !manage {
		if v == radio.BreakInUnknown {
			return nil
		}
		return fmt.Errorf("radio %s: break-in is off, so CW sent over CAT would be "+
			"accepted and never transmitted; set break_in to semi or full, or key the "+
			"transmitter another way first (cw.break_in is manual, so remoses will not "+
			"set it): %w", s.id, ErrUnsupported)
	}

	if err := b.SetBreakIn(ctx, s, want); err != nil {
		if v == radio.BreakInUnknown {
			// Never read one, and setting it did not work either. The radio may
			// simply not be in a state where the command applies, which is not
			// evidence that the Morse will go nowhere.
			s.log.Debug("cw: could not set break-in before sending, and its state is unknown",
				"want", want, "err", err)
			return nil
		}
		return fmt.Errorf("radio %s: break-in is off and setting it to %s failed, so CW "+
			"sent over CAT would be accepted and never transmitted: %w", s.id, want, err)
	}
	s.log.Info("cw: enabled break-in so the message will be transmitted", "break_in", want)
	return nil
}

// configuredBreakIn maps cw.break_in onto the value to write. The second result
// is false for "manual", the setting that tells remoses to keep its hands off.
func configuredBreakIn(s string) (radio.BreakIn, bool) {
	switch s {
	case "full":
		return radio.BreakInFull, true
	case "manual":
		return radio.BreakInUnknown, false
	default:
		// Semi, and also the default for an empty value: a Session built in a
		// test does not go through the config layer's defaulting.
		return radio.BreakInSemi, true
	}
}

// vfoModeSelector reports the backend's way out of memory mode, or nil.
func (s *Session) vfoModeSelector() backend.VFOModeSelector {
	v, _ := s.rig.(backend.VFOModeSelector)
	return v
}

// SelectVFOMode returns the radio to VFO operation, out of memory mode.
//
// Deliberately not gated on requireConnected before validating, like the other
// setters: an operator reaching for this is usually doing so because the radio
// is behaving oddly, and "this radio cannot do that" is more useful then than
// "not connected".
func (s *Session) SelectVFOMode(ctx context.Context, vfo radio.VFO) (radio.State, error) {
	v := s.vfoModeSelector()
	if v == nil {
		return s.State(), fmt.Errorf("radio %s: leaving memory mode: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := v.SelectVFOMode(ctx, s, vfo); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetVFOFrequency tunes one named VFO, which is a different operation from
// SetFrequency: that one moves whatever the radio is on.
//
// The band limits apply to both. A frequency this station may not transmit on
// is one it may not put in a VFO either, because split makes the other VFO a
// transmit frequency without any further command.
func (s *Session) SetVFOFrequency(ctx context.Context, vfo radio.VFO, hz uint64) (radio.State, error) {
	if !s.cfg.Limits.AllowsFrequency(hz) {
		return s.State(), fmt.Errorf("radio %s: %d Hz: %w", s.id, hz, ErrOutOfBand)
	}
	d := s.dualVFO()
	if d == nil {
		return s.State(), fmt.Errorf("radio %s: addressing VFO %s: %w", s.id, vfo, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := d.SetVFOFrequency(ctx, s, vfo, hz); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetVFOMode sets mode, data mode and filter on one named VFO. slot 0 keeps
// whatever filter that VFO has.
func (s *Session) SetVFOMode(ctx context.Context, vfo radio.VFO, m radio.Mode, dataMode bool, slot int) (radio.State, error) {
	caps := s.Caps()
	if len(caps.Modes) > 0 && !caps.SupportsMode(m) {
		return s.State(), fmt.Errorf("radio %s: mode %s: %w", s.id, m, ErrUnsupported)
	}
	d := s.dualVFO()
	if d == nil {
		return s.State(), fmt.Errorf("radio %s: addressing VFO %s: %w", s.id, vfo, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := d.SetVFOMode(ctx, s, vfo, m, dataMode, slot); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetSplit moves transmit to the other VFO, or back.
//
// It is read back before returning, like every other write here, and for a
// sharper reason than most: this is the setting that decides where the
// transmitter lands, and an operator who thinks split is off when it is on
// transmits on somebody else's frequency.
func (s *Session) SetSplit(ctx context.Context, on bool) (radio.State, error) {
	d := s.dualVFO()
	if d == nil || !s.Caps().Split {
		return s.State(), fmt.Errorf("radio %s: split: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := d.SetSplit(ctx, s, on); err != nil {
		return s.State(), err
	}
	return s.readback(ctx, backend.PollSlow, backend.PollFast)
}

// SetDualWatch turns receiving on both VFOs on or off.
func (s *Session) SetDualWatch(ctx context.Context, on bool) (radio.State, error) {
	d := s.dualVFO()
	if d == nil || !s.Caps().DualWatch {
		return s.State(), fmt.Errorf("radio %s: dual watch: %w", s.id, ErrUnsupported)
	}
	if err := s.requireConnected(); err != nil {
		return s.State(), err
	}
	if err := d.SetDualWatch(ctx, s, on); err != nil {
		return s.State(), err
	}
	// Slow first: it carries the dual-watch flag itself, and the fast tier's
	// decision to poll the second receiver's meter depends on it.
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

	// Split and DualWatch are radio-wide rather than per-VFO, which is why
	// they sit beside VFO rather than inside it.
	Split     *bool
	DualWatch *bool

	// VFOMode returns the radio to VFO operation, out of memory mode. Write
	// only, and true only: remoses models no memory mode to switch back into,
	// so there is nothing false could mean. Applied before everything else,
	// since a rig on a memory channel refuses most of what follows.
	VFOMode *bool

	// BreakIn is the CW break-in setting, which decides whether Morse sent
	// over CAT reaches the air.
	BreakIn *radio.BreakIn

	// Tuner switches the antenna tuner in or out of line. Only off and on are
	// accepted: "tuning" is a state to observe, not one to ask for.
	Tuner *radio.Tuner
	// TunerTune starts a tuning cycle, which TRANSMITS. Write only, and true
	// only, like VFOMode.
	//
	// It is a separate field from Tuner rather than a third value of it so that
	// a client which reads the state and writes it back — a perfectly ordinary
	// thing to do — can never key a transmitter by echoing "tuning" at the
	// radio it just read it from.
	TunerTune *bool

	// The receive front end. They belong in a patch rather than in setters of
	// their own because an operator works them together — preamp off, pad in,
	// RF gain back — and one request that either applies all of it or none of
	// it is better than three that can half-succeed.
	Preamp       *int
	AttenuatorDB *int
	RFGain       *float64
	AGC          *radio.AGC
	IPPlus       *bool
	DigiSel      *bool
	DigiSelShift *float64
}

// Empty reports whether the request would change nothing.
func (r PatchRequest) Empty() bool {
	return r.Mode == nil && r.DataMode == nil && r.Frequency == nil &&
		r.FilterSlot == nil && r.FilterWidthHz == nil && r.Power == nil && r.PTT == nil &&
		r.Split == nil && r.DualWatch == nil && r.VFOMode == nil && r.BreakIn == nil &&
		r.Tuner == nil && r.TunerTune == nil &&
		r.Preamp == nil && r.AttenuatorDB == nil && r.RFGain == nil &&
		r.AGC == nil && r.IPPlus == nil && r.DigiSel == nil && r.DigiSelShift == nil
}

// frontEndRequested reports whether this request touches the receive front end.
func (r PatchRequest) frontEndRequested() bool {
	return r.Preamp != nil || r.AttenuatorDB != nil || r.RFGain != nil ||
		r.AGC != nil || r.IPPlus != nil || r.DigiSel != nil || r.DigiSelShift != nil
}

// vfoState is the cached state of one named VFO, for filling in the half of a
// mode change a request did not specify.
func (s *Session) vfoState(vfo radio.VFO) radio.VFOState {
	st := s.State()
	if vfo == radio.VFOB || vfo == radio.VFOSub {
		return st.VFOB
	}
	return st.VFOA
}

// namesAVFO reports whether this request addresses a particular VFO rather than
// whichever one the radio is on.
func (r PatchRequest) namesAVFO() bool {
	return r.VFO == radio.VFOA || r.VFO == radio.VFOB ||
		r.VFO == radio.VFOMain || r.VFO == radio.VFOSub
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
	if req.BreakIn != nil && !caps.BreakInControl {
		return s.State(), fmt.Errorf("radio %s: break-in: %w", s.id, ErrUnsupported)
	}
	// A radio may have no transmitter command and no power level at all — the
	// IC-706 family has neither. Refusing here rather than at the backend keeps
	// the whole patch from being half-applied, and gives the same 422 whichever
	// field was the impossible one.
	if req.PTT != nil && !caps.PTTControl {
		return s.State(), fmt.Errorf("radio %s: this radio has no transmitter command; "+
			"key it with a footswitch, the microphone or a serial control line: %w",
			s.id, ErrUnsupported)
	}
	if req.Power != nil && !caps.PowerControl {
		return s.State(), fmt.Errorf("radio %s: this radio has no RF power command; "+
			"its output is set on the radio: %w", s.id, ErrUnsupported)
	}
	if req.Tuner != nil {
		if !(*req.Tuner).Valid() {
			return s.State(), fmt.Errorf("radio %s: tuner %q, want off or on; "+
				"start a tuning cycle with tuner_tune: %w", s.id, *req.Tuner, ErrUnsupported)
		}
		if !caps.TunerControl {
			return s.State(), fmt.Errorf("radio %s: antenna tuner: %w", s.id, ErrUnsupported)
		}
	}
	if req.TunerTune != nil {
		if !*req.TunerTune {
			return s.State(), fmt.Errorf("radio %s: tuner_tune can only be set true; "+
				"it starts a tuning cycle and there is nothing false would stop: %w",
				s.id, ErrUnsupported)
		}
		if !caps.TunerTune {
			return s.State(), fmt.Errorf("radio %s: starting a tuning cycle: %w",
				s.id, ErrUnsupported)
		}
		// A tuning cycle transmits, so it answers to the band limits like any
		// other transmission rather than to the tuning ones.
		if hz := s.State().Frequency; hz > 0 && !s.cfg.Limits.AllowsFrequency(hz) {
			return s.State(), fmt.Errorf("radio %s: tuning transmits on %d Hz: %w",
				s.id, hz, ErrOutOfBand)
		}
	}
	if req.VFOMode != nil {
		if !*req.VFOMode {
			return s.State(), fmt.Errorf("radio %s: vfo_mode can only be set true; "+
				"remoses does not model memory mode and has nothing to switch back into: %w",
				s.id, ErrUnsupported)
		}
		if s.vfoModeSelector() == nil {
			return s.State(), fmt.Errorf("radio %s: leaving memory mode: %w", s.id, ErrUnsupported)
		}
	}
	// The dual-VFO controls, validated here with the rest so that a request
	// naming VFO B on a radio that cannot address one is refused before
	// anything reaches the wire.
	if req.namesAVFO() && s.dualVFO() == nil {
		return s.State(), fmt.Errorf("radio %s: addressing VFO %s: %w", s.id, req.VFO, ErrUnsupported)
	}
	if req.Split != nil && !caps.Split {
		return s.State(), fmt.Errorf("radio %s: split: %w", s.id, ErrUnsupported)
	}
	if req.DualWatch != nil && !caps.DualWatch {
		return s.State(), fmt.Errorf("radio %s: dual watch: %w", s.id, ErrUnsupported)
	}
	if err := s.validateFrontEnd(req, caps); err != nil {
		return s.State(), err
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

	// Which tier the read-back needs. Everything here is read on the slow tier,
	// so a request touching any of it must re-read that tier or the response —
	// and the cache behind it — reports the value from before the write.
	//
	// Break-in is the one that showed why this list has to be kept in step: it
	// was missing, so setting break-in answered with the old value and the CW
	// path went on refusing to send.
	slow := req.Power != nil || req.FilterSlot != nil || req.FilterWidthHz != nil ||
		req.Split != nil || req.DualWatch != nil || req.namesAVFO() ||
		req.VFOMode != nil || req.BreakIn != nil ||
		req.Tuner != nil || req.TunerTune != nil ||
		req.frontEndRequested()

	// First of everything: a radio on a memory channel refuses several of the
	// commands below, so a request that says "get back on a VFO and tune there"
	// has to do those in that order to mean anything.
	if req.VFOMode != nil {
		if err := s.vfoModeSelector().SelectVFOMode(ctx, s, req.VFO); err != nil {
			return s.State(), err
		}
	}

	// A request naming a VFO takes the whole mode/frequency path through the
	// dual-VFO commands instead. On an Icom that is strictly better even for
	// the operating VFO — command 26 carries mode, data mode and filter in one
	// frame, where the single-VFO path needs 06 then 1A 06 and those two
	// overwrite each other — but it is used only when a VFO was named, because
	// on any other radio there is no such command and the ordinary path is the
	// only one there is.
	if named := req.namesAVFO(); named {
		d := s.dualVFO()
		if req.Mode != nil || req.DataMode != nil {
			cur := s.vfoState(req.VFO)
			m, dm := cur.Mode, cur.DataMode
			if req.Mode != nil {
				m = *req.Mode
			}
			if req.DataMode != nil {
				dm = *req.DataMode
			}
			// Slot 0 keeps the filter this VFO has, unless the request names
			// one — and when it does, it is applied here rather than by the
			// separate filter step below, because command 26 sets all three at
			// once and a later SetFilterSlot would be a second write.
			slot := 0
			if req.FilterSlot != nil {
				slot = *req.FilterSlot
			}
			if err := d.SetVFOMode(ctx, s, req.VFO, m, dm, slot); err != nil {
				return s.State(), err
			}
			req.FilterSlot = nil
		}
		if req.Frequency != nil {
			if err := d.SetVFOFrequency(ctx, s, req.VFO, *req.Frequency); err != nil {
				return s.State(), err
			}
			req.Frequency = nil
		}
	}

	// Data mode is orthogonal to mode, so a request that changes only one of
	// the two has to resend the other as the rig currently has it.
	if !req.namesAVFO() && (req.Mode != nil || req.DataMode != nil) {
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
	// The receive front end, in the order an operator would work it: gain
	// stages first, then the gain controls behind them. Nothing here keys the
	// transmitter, so all of it goes before PTT.
	if err := s.applyFrontEnd(ctx, req); err != nil {
		return s.State(), err
	}
	// Break-in before PTT and before anything that might key: a request that
	// turns break-in on and then sends is the ordinary way to make CW audible,
	// and doing it the other way round would send the first message into
	// silence.
	if req.BreakIn != nil {
		if err := s.breakInController().SetBreakIn(ctx, s, *req.BreakIn); err != nil {
			return s.State(), err
		}
	}
	// Split and dual watch go after everything that shapes a VFO and before
	// PTT, so that a request which sets up the other VFO and then enables split
	// cannot key before the transmit VFO is where the operator asked for it.
	if req.Split != nil {
		if err := s.dualVFO().SetSplit(ctx, s, *req.Split); err != nil {
			return s.State(), err
		}
	}
	if req.DualWatch != nil {
		if err := s.dualVFO().SetDualWatch(ctx, s, *req.DualWatch); err != nil {
			return s.State(), err
		}
	}
	// The tuner goes in before PTT and before the tuning cycle: switching the
	// matching network in or out mid-transmission is not something to do, and a
	// cycle started with the tuner bypassed either does nothing or means
	// something else — on a Kenwood the reference says outright that "AT Tuning
	// will not begin when using the TX THRU status".
	if req.Tuner != nil {
		if err := s.tunerController().SetTuner(ctx, s, *req.Tuner == radio.TunerOn); err != nil {
			return s.State(), err
		}
	}
	if req.PTT != nil {
		if err := s.rig.SetPTT(ctx, s, *req.PTT); err != nil {
			return s.State(), err
		}
	}
	// Last, because it transmits, for the same reason PTT is last: the radio is
	// fully configured before anything keys it. The dead-man timer is armed as
	// it is for any other transmission, so a cycle that never ends is caught by
	// the interlock that catches a stuck PTT.
	if req.TunerTune != nil {
		if err := s.tunerController().StartTune(ctx, s); err != nil {
			return s.State(), err
		}
		s.armDeadman()
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
	// Aborting the CW above is the whole of the safety path on a radio with no
	// transmitter command. Sending one anyway would fail every time this fires,
	// and log an error about a control the radio has never had — on such a rig
	// PTT is a footswitch or a control line, and neither is remoses' to drop.
	if !s.Caps().PTTControl {
		s.refreshCW()
		return
	}
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
