package ws

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// The fakes here are this package's own. The wire protocol is a caricature of
// Kenwood CAT — two-letter commands, ';' terminated — because a failure message
// containing "FA00014025000" is readable, and because this package's tests must
// fail for WebSocket reasons only, never for CI-V or Kenwood parsing reasons.
//
//	FA00014025000  frequency     MD3  mode      PT1  PTT
//	PC050 power    SM0015 s-meter FW0500 passband FL2 filter slot
//	AI1            init ack
//	UF00007000000  UNSOLICITED frequency report, answering no request

// fakePort is one connection's worth of transport, backed by a fakeDevice.
type fakePort struct {
	dev *fakeDevice

	in     chan []byte
	closed chan struct{}

	closeOnce sync.Once
	pending   []byte // reader goroutine only
}

func newFakePort(dev *fakeDevice) *fakePort {
	return &fakePort{
		dev:    dev,
		in:     make(chan []byte, 8192),
		closed: make(chan struct{}),
	}
}

func (p *fakePort) Read(b []byte) (int, error) {
	if len(p.pending) == 0 {
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
	select {
	case <-p.closed:
		return 0, transport.ErrDisconnected
	default:
	}
	for _, frame := range bytes.Split(b, []byte(";")) {
		if len(frame) == 0 {
			continue
		}
		reply, ok := p.dev.handle(string(frame))
		if !ok {
			continue
		}
		p.push(reply)
	}
	return len(b), nil
}

// push injects a frame, as a rig in Transceive/AI mode would.
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

// fakeDevice is the radio behind the port. It survives reconnects, as a real
// rig does.
type fakeDevice struct {
	mu       sync.Mutex
	freq     uint64
	mode     int
	ptt      bool
	powerPct int
	passband int
	slot     int
	smeter   int

	port atomic.Pointer[fakePort]
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{
		freq: 14025000, mode: int(radio.ModeCW), powerPct: 50,
		passband: 500, slot: 1, smeter: 12,
	}
}

// tune moves the radio and reports it unsolicited, exactly as a front-panel VFO
// knob does with Transceive on. This is the flood generator.
func (d *fakeDevice) tune(hz uint64) {
	d.mu.Lock()
	d.freq = hz
	d.mu.Unlock()
	if p := d.port.Load(); p != nil {
		p.push(fmt.Sprintf("UF%011d", hz))
	}
}

// drop kills the current connection, as pulling the USB cable would.
func (d *fakeDevice) drop() {
	if p := d.port.Load(); p != nil {
		_ = p.Close()
	}
}

func (d *fakeDevice) handle(cmd string) (string, bool) {
	if len(cmd) < 2 {
		return "", false
	}
	key, arg := cmd[:2], cmd[2:]

	d.mu.Lock()
	defer d.mu.Unlock()

	switch key {
	case "AI":
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
	case "PT":
		if arg != "" {
			d.ptt = arg == "1"
			return "", false
		}
		return fmt.Sprintf("PT%d", boolToInt(d.ptt)), true
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

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fakeDialer hands out a new port per dial, all backed by the same device.
type fakeDialer struct{ dev *fakeDevice }

func (d *fakeDialer) Dial(context.Context) (transport.Transport, error) {
	p := newFakePort(d.dev)
	d.dev.port.Store(p)
	return p, nil
}

func (d *fakeDialer) Describe() string { return "fake://rig" }

// fakeRig is a backend.Rig: pure protocol, no goroutines, no locks of its own.
type fakeRig struct{}

func (fakeRig) Caps() radio.Caps {
	return radio.Caps{
		Modes:       []radio.Mode{radio.ModeCW, radio.ModeUSB, radio.ModeLSB},
		VFOs:        []radio.VFO{radio.VFOA, radio.VFOB},
		FilterWidth: true,
		FilterSlots: 3,
		SMeterScale: 30,
		MaxPowerW:   100,
	}
}

func (fakeRig) Init(ctx context.Context, c backend.Conn) error {
	_, err := c.Do(ctx, []byte("AI1;"), "AI")
	return err
}

func (fakeRig) Split(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, ';'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func (fakeRig) Decode(frame []byte) (backend.Update, error) {
	up := backend.Update{OK: true, Raw: frame}
	s := string(frame)
	if len(s) < 2 {
		return up, nil
	}
	key, arg := s[:2], s[2:]

	if key == "UF" { // unsolicited: matches no request, still updates state
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

func (fakeRig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	var reqs [][2]string
	switch tier {
	case backend.PollFast:
		reqs = [][2]string{{"FA;", "FA"}, {"MD;", "MD"}, {"PT;", "PT"}, {"SM;", "SM"}}
	case backend.PollSlow:
		reqs = [][2]string{{"PC;", "PC"}, {"FW;", "FW"}, {"FL;", "FL"}}
	}
	for _, q := range reqs {
		if _, err := c.Do(ctx, []byte(q[0]), backend.Key(q[1])); err != nil {
			return err
		}
	}
	return nil
}

func (fakeRig) SetFrequency(ctx context.Context, c backend.Conn, _ radio.VFO, hz uint64) error {
	_, err := c.Do(ctx, fmt.Appendf(nil, "FA%011d;", hz), "FA")
	return err
}

func (fakeRig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, _ bool) error {
	_, err := c.Do(ctx, fmt.Appendf(nil, "MD%d;", int(m)), "MD")
	return err
}

func (fakeRig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	pct := 0.0
	if p.Pct != nil {
		pct = *p.Pct
	}
	_, err := c.Do(ctx, fmt.Appendf(nil, "PC%03d;", int(pct)), "PC")
	return err
}

func (fakeRig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	return c.Send(ctx, fmt.Appendf(nil, "PT%d;", boolToInt(on)))
}

func (fakeRig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	_, err := c.Do(ctx, fmt.Appendf(nil, "FW%04d;", hz), "FW")
	return err
}

func (fakeRig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	_, err := c.Do(ctx, fmt.Appendf(nil, "FL%d;", slot), "FL")
	return err
}

// fakeCW is a cw.Sender. With churn enabled its status changes on every read,
// which is what makes the session publish a stream of EventCW: the only way to
// exercise the discrete lane at any volume.
type fakeCW struct {
	churn  atomic.Bool
	queued atomic.Int64
}

func (c *fakeCW) Enqueue(text string, _ cw.Mode) (int, error) { return len(text), nil }
func (c *fakeCW) Abort()                                      {}
func (c *fakeCW) SetSpeed(int) error                          { return nil }
func (c *fakeCW) Charset() string                             { return "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789" }

func (c *fakeCW) Status() radio.CWStatus {
	q := 0
	if c.churn.Load() {
		q = int(c.queued.Add(1))
	}
	return radio.CWStatus{Busy: q > 0, Queued: q, WPM: 28}
}

// discardLogger keeps test output about a wedged fake rig out of the way of the
// assertion that actually failed.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// newTestSession builds one session over a fake device.
//
// eventQueue sizes the session's fan-out buffer. Setting it to 1 makes the
// session drop events under a flood, which is how a test provokes the
// Event.Dropped that the WebSocket layer has to turn into a resync.
func newTestSession(t *testing.T, id string, poll time.Duration, eventQueue int) (*rig.Session, *fakeDevice, *fakeCW) {
	t.Helper()
	dev := newFakeDevice()
	rc := config.Radio{
		ID:      id,
		Name:    "Fake " + id,
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(poll),
			SlowInterval: config.Duration(time.Hour),
		},
	}
	opts := []rig.Option{
		rig.WithLogger(discardLogger()),
		rig.WithCommandTimeout(5 * time.Second),
	}
	if eventQueue > 0 {
		opts = append(opts, rig.WithEventQueue(eventQueue))
	}
	s, err := rig.NewSession(rc, fakeRig{}, &fakeDialer{dev: dev}, opts...)
	if err != nil {
		t.Fatalf("NewSession(%q): %v", id, err)
	}
	snd := &fakeCW{}
	s.SetCWSender(snd)
	return s, dev, snd
}

// waitFor polls cond until it holds or the deadline passes. Used instead of
// sleeps so the tests stay fast and do not depend on timing margins.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
