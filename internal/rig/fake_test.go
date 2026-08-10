package rig

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// The fakes here deliberately do not use the civ or kenwood backends: this
// package's job is concurrency, and its tests must fail for concurrency reasons
// only. The wire protocol below is a caricature of Kenwood CAT — two-letter
// commands, ';' terminated — chosen because it is trivial to read in a failure
// message.
//
// Frames:
//
//	FA00014025000  frequency          MD3  mode        DA1  data mode
//	PT1            PTT                PC050 power pct  SM0015 s-meter
//	FW0500         passband           FL2  filter slot AI1  init ack
//	NKFA           negative ack for the FA command
//	UF00007000000  UNSOLICITED frequency report, answering no request

// fakePort is one connection's worth of transport.
//
// Its Write is synchronous with the reader by design: it pushes the rig's reply
// and then waits until the session's reader goroutine has consumed it AND come
// back for more data. That makes the register-before-write ordering a hard
// requirement rather than a race — a Do that registered its waiter after
// writing would provably have missed the reply.
type fakePort struct {
	dev *fakeDevice

	in     chan []byte
	hungry chan struct{} // signalled whenever the reader blocks for more data
	closed chan struct{}

	closeOnce sync.Once
	pending   []byte // reader goroutine only

	// inFlight proves command serialisation: because Write does not return
	// until its reply has been read, two overlapping transactions would show up
	// here immediately.
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func newFakePort(dev *fakeDevice) *fakePort {
	return &fakePort{
		dev:    dev,
		in:     make(chan []byte, 16),
		hungry: make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

func (p *fakePort) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
		select {
		case p.hungry <- struct{}{}:
		default:
		}
		select {
		case data := <-p.in:
			p.pending = data
		case <-p.closed:
			return 0, transport.ErrDisconnected
		}
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *fakePort) Write(b []byte) (int, error) {
	n := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		m := p.maxInFlight.Load()
		if n <= m || p.maxInFlight.CompareAndSwap(m, n) {
			break
		}
	}

	select {
	case <-p.closed:
		return 0, transport.ErrDisconnected
	default:
	}
	for _, cmd := range splitFrames(b) {
		reply, ok := p.dev.handle(cmd)
		if !ok {
			continue
		}
		// Drain any stale "reader is waiting" signal first, so the wait below
		// observes the reader coming back after THIS reply.
		select {
		case <-p.hungry:
		default:
		}
		select {
		case p.in <- []byte(reply + ";"):
		case <-p.closed:
			return 0, transport.ErrDisconnected
		}
		select {
		case <-p.hungry:
		case <-p.closed:
		case <-time.After(2 * time.Second):
			return 0, fmt.Errorf("fakePort: reader never consumed reply to %q", cmd)
		}
	}
	return len(b), nil
}

// push injects an unsolicited frame, as a rig in Transceive/AI mode would.
func (p *fakePort) push(frame string) {
	select {
	case p.in <- []byte(frame + ";"):
	case <-p.closed:
	}
}

func (p *fakePort) Close() error {
	p.closeOnce.Do(func() { close(p.closed) })
	return nil
}

func splitFrames(b []byte) []string {
	var out []string
	for _, f := range bytes.Split(b, []byte(";")) {
		if len(f) > 0 {
			out = append(out, string(f))
		}
	}
	return out
}

// fakeDevice is the radio behind the port. It survives reconnects, exactly as a
// real rig does.
type fakeDevice struct {
	mu       sync.Mutex
	freq     uint64
	mode     int
	dataMode bool
	ptt      bool
	powerPct int
	passband int
	slot     int
	smeter   int

	silent map[string]bool // commands the rig never answers
	nak    map[string]bool // commands the rig rejects
	seen   []string

	inits atomic.Int64
	port  atomic.Pointer[fakePort]
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{
		freq: 14025000, mode: int(radio.ModeCW), powerPct: 50,
		passband: 500, slot: 1, smeter: 12,
		silent: map[string]bool{},
		nak:    map[string]bool{},
	}
}

func (d *fakeDevice) setSilent(cmd string, on bool) {
	d.mu.Lock()
	d.silent[cmd] = on
	d.mu.Unlock()
}

func (d *fakeDevice) setNAK(cmd string, on bool) {
	d.mu.Lock()
	d.nak[cmd] = on
	d.mu.Unlock()
}

func (d *fakeDevice) snapshot() (freq uint64, mode int, ptt bool, power int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.freq, d.mode, d.ptt, d.powerPct
}

func (d *fakeDevice) commands() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// handle applies one command and returns the reply, if the rig answers it.
func (d *fakeDevice) handle(cmd string) (string, bool) {
	if len(cmd) < 2 {
		return "", false
	}
	key, arg := cmd[:2], cmd[2:]

	d.mu.Lock()
	defer d.mu.Unlock()
	d.seen = append(d.seen, cmd)

	if d.silent[key] {
		return "", false
	}
	if d.nak[key] {
		return "NK" + key, true
	}

	switch key {
	case "AI":
		d.inits.Add(1)
		return "AI1", true
	case "FA":
		if arg != "" {
			v, _ := strconv.ParseUint(arg, 10, 64)
			d.freq = v
		}
		return fmt.Sprintf("FA%011d", d.freq), true
	case "MD":
		if arg != "" {
			v, _ := strconv.Atoi(arg)
			d.mode = v
		}
		return fmt.Sprintf("MD%d", d.mode), true
	case "DA":
		if arg != "" {
			d.dataMode = arg == "1"
		}
		return fmt.Sprintf("DA%d", b2i(d.dataMode)), true
	case "PT":
		// Set is silent, like Kenwood TX;/RX;. Query answers.
		if arg != "" {
			d.ptt = arg == "1"
			return "", false
		}
		return fmt.Sprintf("PT%d", b2i(d.ptt)), true
	case "PC":
		if arg != "" {
			v, _ := strconv.Atoi(arg)
			d.powerPct = v
		}
		return fmt.Sprintf("PC%03d", d.powerPct), true
	case "SM":
		return fmt.Sprintf("SM%04d", d.smeter), true
	case "FW":
		if arg != "" {
			v, _ := strconv.Atoi(arg)
			d.passband = v
		}
		return fmt.Sprintf("FW%04d", d.passband), true
	case "FL":
		if arg != "" {
			v, _ := strconv.Atoi(arg)
			d.slot = v
		}
		return fmt.Sprintf("FL%d", d.slot), true
	}
	return "", false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fakeDialer hands out a new port per dial, all backed by the same device.
type fakeDialer struct {
	dev *fakeDevice

	mu      sync.Mutex
	dials   int
	failFor int   // fail this many dials before succeeding
	failErr error // error to return while failing
	ports   []*fakePort
}

func newFakeDialer(dev *fakeDevice) *fakeDialer {
	return &fakeDialer{dev: dev, failErr: io.ErrUnexpectedEOF}
}

func (d *fakeDialer) Dial(ctx context.Context) (transport.Transport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.failFor > 0 {
		d.failFor--
		return nil, d.failErr
	}
	p := newFakePort(d.dev)
	d.ports = append(d.ports, p)
	d.dev.port.Store(p)
	return p, nil
}

func (d *fakeDialer) Describe() string { return "fake://rig" }

func (d *fakeDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *fakeDialer) lastPort() *fakePort {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ports) == 0 {
		return nil
	}
	return d.ports[len(d.ports)-1]
}

// fakeRig is a backend.Rig: pure protocol, no goroutines, no locks of its own.
type fakeRig struct {
	caps radio.Caps

	inits    atomic.Int64
	pollFast atomic.Int64
	pollSlow atomic.Int64

	// breakIn holds a radio.BreakIn, read by the CW send guard without a round
	// trip, exactly as a real backend's does.
	breakIn atomic.Value

	mu         sync.Mutex
	initErr    error
	breakInErr error
	powerErr   error
	sets       []string // ordered log of the set commands the session issued
	// noDataMode names modes this rig has no data-mode spelling of, so that
	// SetMode refuses the pairing the way a backend's own mode table does.
	noDataMode map[radio.Mode]bool
}

func (r *fakeRig) setInitErr(err error) {
	r.mu.Lock()
	r.initErr = err
	r.mu.Unlock()
}

func (r *fakeRig) initError() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initErr
}

func newFakeRig() *fakeRig {
	return &fakeRig{caps: radio.Caps{
		Modes:        []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR, radio.ModeFSK},
		VFOs:         []radio.VFO{radio.VFOA, radio.VFOB},
		PTTControl:   true,
		PowerControl: true,
		PowerSwitch:  true,
		FilterWidth:  true,
		FilterSlots:  3,
		SMeterScale:  30,
		MaxPowerW:    100,

		// The rig's own keyer, as a backend would report it. Deliberately
		// narrower than internal/cw's local clamp of 5-60, so that a test can
		// tell "the backend's range" from "the local keyer's" rather than
		// comparing zero with zero.
		CWMethod:  radio.CWViaCAT,
		CWCharset: "ABC",
		CWMinWPM:  6,
		CWMaxWPM:  48,
	}}
}

func (r *fakeRig) Caps() radio.Caps { return r.caps }

func (r *fakeRig) record(s string) {
	r.mu.Lock()
	r.sets = append(r.sets, s)
	r.mu.Unlock()
}

func (r *fakeRig) setLog() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sets...)
}

func (r *fakeRig) Init(ctx context.Context, c backend.Conn) error {
	r.inits.Add(1)
	if err := r.initError(); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte("AI1;"), "AI")
	return err
}

func (r *fakeRig) Split(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, ';'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), nil, nil // resynchronise on trailing garbage
	}
	return 0, nil, nil
}

func (r *fakeRig) Decode(frame []byte) (backend.Update, error) {
	up := backend.Update{OK: true, Raw: frame}
	s := string(frame)
	if len(s) < 2 {
		return up, nil // unknown traffic is ignored quietly
	}
	key, arg := s[:2], s[2:]

	switch key {
	case "NK":
		up.Key = backend.Key(arg)
		up.OK = false
		return up, nil
	case "UF": // unsolicited: matches no request, still updates state
		v, _ := strconv.ParseUint(arg, 10, 64)
		up.Patch.Frequency = &v
		return up, nil
	}

	up.Key = backend.Key(key)
	switch key {
	case "FA":
		v, _ := strconv.ParseUint(arg, 10, 64)
		up.Patch.Frequency = &v
	case "MD":
		v, _ := strconv.Atoi(arg)
		m := radio.Mode(v)
		up.Patch.Mode = &m
	case "DA":
		v := arg == "1"
		up.Patch.DataMode = &v
	case "PT":
		v := arg == "1"
		up.Patch.PTT = &v
	case "PC":
		v, _ := strconv.Atoi(arg)
		up.Patch.Power = &radio.Power{Pct: float64(v), Native: v}
	case "SM":
		v, _ := strconv.Atoi(arg)
		up.Patch.SMeter = &radio.Meter{Raw: v, Scale: 30}
	case "FW":
		v, _ := strconv.Atoi(arg)
		up.Patch.PassbandHz = &v
	case "FL":
		v, _ := strconv.Atoi(arg)
		up.Patch.FilterSlot = &v
	}
	return up, nil
}

func (r *fakeRig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	var reqs [][2]string
	switch tier {
	case backend.PollFast:
		r.pollFast.Add(1)
		reqs = [][2]string{{"FA;", "FA"}, {"MD;", "MD"}, {"DA;", "DA"}, {"PT;", "PT"}, {"SM;", "SM"}}
	case backend.PollSlow:
		r.pollSlow.Add(1)
		reqs = [][2]string{{"PC;", "PC"}, {"FW;", "FW"}, {"FL;", "FL"}}
	}
	for _, q := range reqs {
		if _, err := c.Do(ctx, []byte(q[0]), backend.Key(q[1])); err != nil {
			return err
		}
	}
	return nil
}

func (r *fakeRig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	r.record(fmt.Sprintf("freq=%d", hz))
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FA%011d;", hz)), "FA")
	return err
}

func (r *fakeRig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	// Recorded before the refusal, so a test can see both halves of a retry.
	r.record(fmt.Sprintf("mode=%s data=%v", m, dataMode))
	if dataMode && r.refusesDataMode(m) {
		return fmt.Errorf("fake: no data mode code for %s: %w", m, backend.ErrUnsupported)
	}
	if _, err := c.Do(ctx, []byte(fmt.Sprintf("MD%d;", int(m))), "MD"); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("DA%d;", b2i(dataMode))), "DA")
	return err
}

func (r *fakeRig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	pct := 0.0
	switch {
	case p.Pct != nil:
		pct = *p.Pct
	case p.Watts != nil:
		pct = *p.Watts // fake rig is 100 W full scale, so watts == percent
	}
	r.record(fmt.Sprintf("power=%.0f", pct))
	_, err := c.Do(ctx, []byte(fmt.Sprintf("PC%03d;", int(pct))), "PC")
	return err
}

func (r *fakeRig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	r.record(fmt.Sprintf("ptt=%v", on))
	// Unanswered, like Kenwood TX;/RX;.
	return c.Send(ctx, []byte(fmt.Sprintf("PT%d;", b2i(on))))
}

func (r *fakeRig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	r.record(fmt.Sprintf("width=%d", hz))
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FW%04d;", hz)), "FW")
	return err
}

func (r *fakeRig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	r.record(fmt.Sprintf("slot=%d", slot))
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FL%d;", slot)), "FL")
	return err
}

// Break-in, so the CW send guard can be exercised. A real backend reads this
// from the rig; here it is whatever a test last stored, plus an optional error
// to stand in for a radio that refuses the command.
func (r *fakeRig) SetBreakIn(ctx context.Context, c backend.Conn, v radio.BreakIn) error {
	r.record("break_in=" + string(v))
	r.mu.Lock()
	err := r.breakInErr
	r.mu.Unlock()
	if err != nil {
		return err
	}
	r.breakIn.Store(v)
	return nil
}

func (r *fakeRig) BreakIn() radio.BreakIn {
	v, _ := r.breakIn.Load().(radio.BreakIn)
	return v
}

// The power switch, recorded rather than acted on. The session's own handling
// of a radio that stops answering is what these tests are about.
func (r *fakeRig) PowerOn(ctx context.Context, c backend.Conn) error {
	r.record("power=on")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.powerErr
}

func (r *fakeRig) PowerOff(ctx context.Context, c backend.Conn, deep bool) error {
	what := "power=off"
	if deep {
		what = "power=off_deep"
	}
	r.record(what)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.powerErr
}

func (r *fakeRig) setPowerErr(err error) {
	r.mu.Lock()
	r.powerErr = err
	r.mu.Unlock()
}

// setNoDataMode makes the rig refuse the data-mode variant of m, as a radio
// whose mode table has no code for the pairing does. An FT-857D is the real
// example: its data modes are DIG and PKT, so there is no CW-with-data to ask
// for and the backend says so before anything reaches the wire.
func (r *fakeRig) setNoDataMode(m radio.Mode) {
	r.mu.Lock()
	if r.noDataMode == nil {
		r.noDataMode = map[radio.Mode]bool{}
	}
	r.noDataMode[m] = true
	r.mu.Unlock()
}

func (r *fakeRig) refusesDataMode(m radio.Mode) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.noDataMode[m]
}

func (r *fakeRig) setBreakInState(v radio.BreakIn) { r.breakIn.Store(v) }

func (r *fakeRig) setBreakInErr(err error) {
	r.mu.Lock()
	r.breakInErr = err
	r.mu.Unlock()
}

// fakeCW is a cw.Sender that records aborts, so the safety paths can be
// asserted without pulling in the real sender.
type fakeCW struct {
	mu      sync.Mutex
	aborts  int
	status  radio.CWStatus
	aborted chan struct{}

	// How this sender describes its own keying, which the session folds into
	// the published capabilities. Defaults to the CAT shape: a rig keyer, whose
	// speed range belongs to the backend rather than to the sender.
	method radio.CWMethod
	wpmLo  int
	wpmHi  int
	wpmOK  bool
}

func newFakeCW() *fakeCW {
	return &fakeCW{aborted: make(chan struct{}, 16), method: radio.CWViaCAT}
}

// newFakeSerialCW is a locally keyed sender: it names its own method, charset
// and speed range, all of which must win over whatever the backend claims.
func newFakeSerialCW() *fakeCW {
	c := newFakeCW()
	c.method = radio.CWViaSerial
	c.wpmLo, c.wpmHi, c.wpmOK = 5, 60, true
	return c
}

func (c *fakeCW) Enqueue(text string, mode cw.Mode) (int, error) { return len(text), nil }

func (c *fakeCW) Abort() {
	c.mu.Lock()
	c.aborts++
	c.status.Busy = false
	c.status.Queued = 0
	c.mu.Unlock()
	select {
	case c.aborted <- struct{}{}:
	default:
	}
}

func (c *fakeCW) Status() radio.CWStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *fakeCW) setStatus(s radio.CWStatus) {
	c.mu.Lock()
	c.status = s
	c.mu.Unlock()
}

func (c *fakeCW) SetSpeed(wpm int) error     { return nil }
func (c *fakeCW) Charset() string            { return "ABC" }
func (c *fakeCW) Method() radio.CWMethod     { return c.method }
func (c *fakeCW) WPMRange() (int, int, bool) { return c.wpmLo, c.wpmHi, c.wpmOK }

func (c *fakeCW) abortCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aborts
}

// waitFor polls cond until it holds or the deadline passes. Used instead of
// sleeps so the tests stay fast and do not depend on timing margins.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
