package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/lock"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// The fakes here are the API's own, deliberately: this package's job is the
// HTTP contract, and its tests must fail for HTTP reasons only. The wire
// protocol below is a caricature of Kenwood CAT — two-letter commands, ';'
// terminated — chosen because it reads clearly in a failure message.
//
//	FA00014025000  frequency     MD3   mode        DA1    data mode
//	PT1            PTT           PC050 power pct   SM0012 s-meter
//	FW0500         passband      FL2   filter slot

const (
	testUser = "oh2xyz"
	testPass = "hunter2"

	connectedRadio    = "ic7610"  // started, inside a 20 m band limit
	disconnectedRadio = "ts590sg" // never started, so every write is a 503
)

// testHash is computed once: bcrypt is the whole point of the auth package and
// nothing here needs it paid for per test.
var testHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(testPass, 4)
	if err != nil {
		panic(err)
	}
	return h
})

// --- transport ---------------------------------------------------------

type fakeDevice struct {
	mu       sync.Mutex
	freq     uint64
	mode     int
	dataMode bool
	ptt      bool
	power    int
	passband int
	slot     int
	smeter   int
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{
		freq: 14025000, mode: int(radio.ModeCW), power: 50,
		passband: 500, slot: 1, smeter: 12,
	}
}

// handle applies one command and returns the reply, if the rig answers it.
func (d *fakeDevice) handle(cmd string) (string, bool) {
	if len(cmd) < 2 {
		return "", false
	}
	key, arg := cmd[:2], cmd[2:]

	d.mu.Lock()
	defer d.mu.Unlock()

	switch key {
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
		return fmt.Sprintf("DA%d", btoi(d.dataMode)), true
	case "PT":
		if arg != "" {
			// Set is silent, like Kenwood TX;/RX;.
			d.ptt = arg == "1"
			return "", false
		}
		return fmt.Sprintf("PT%d", btoi(d.ptt)), true
	case "PC":
		if arg != "" {
			v, _ := strconv.Atoi(arg)
			d.power = v
		}
		return fmt.Sprintf("PC%03d", d.power), true
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

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

type fakePort struct {
	dev    *fakeDevice
	in     chan []byte
	closed chan struct{}
	once   sync.Once

	pending []byte // reader goroutine only
}

func newFakePort(dev *fakeDevice) *fakePort {
	return &fakePort{dev: dev, in: make(chan []byte, 64), closed: make(chan struct{})}
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
	for _, cmd := range strings.Split(string(b), ";") {
		if cmd == "" {
			continue
		}
		reply, ok := p.dev.handle(cmd)
		if !ok {
			continue
		}
		select {
		case p.in <- []byte(reply + ";"):
		case <-p.closed:
			return 0, transport.ErrDisconnected
		}
	}
	return len(b), nil
}

func (p *fakePort) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}

type fakeDialer struct{ dev *fakeDevice }

func (d *fakeDialer) Dial(context.Context) (transport.Transport, error) {
	return newFakePort(d.dev), nil
}

func (d *fakeDialer) Describe() string { return "fake://rig" }

// --- backend -----------------------------------------------------------

// fakeRig is a backend.Rig with an error injection hook, so that the API's
// mapping of every rig sentinel onto a status code can be exercised without
// unplugging anything.
type fakeRig struct {
	caps radio.Caps

	mu   sync.Mutex
	errs map[string]error
}

func newFakeRig() *fakeRig {
	return &fakeRig{
		caps: radio.Caps{
			Modes:        []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR, radio.ModeFSK},
			VFOs:         []radio.VFO{radio.VFOA, radio.VFOB},
			PTTControl:   true,
			PowerControl: true,
			FilterWidth:  true,
			FilterSlots:  3,
			SMeterScale:  30,
			MaxPowerW:    100,
			CWMethod:     radio.CWViaCAT,
			CWCharset:    "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 /?.,",
			CWMinWPM:     6,
			CWMaxWPM:     60,
		},
		errs: map[string]error{},
	}
}

func (r *fakeRig) failOn(op string, err error) {
	r.mu.Lock()
	r.errs[op] = err
	r.mu.Unlock()
}

func (r *fakeRig) failure(op string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.errs[op]
}

func (r *fakeRig) Caps() radio.Caps { return r.caps }

func (r *fakeRig) Init(ctx context.Context, c backend.Conn) error {
	_, err := c.Do(ctx, []byte("FA;"), "FA")
	return err
}

func (r *fakeRig) Split(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, ';'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), nil, nil
	}
	return 0, nil, nil
}

func (r *fakeRig) Decode(frame []byte) (backend.Update, error) {
	up := backend.Update{OK: true, Raw: frame}
	s := string(frame)
	if len(s) < 2 {
		return up, nil
	}
	key, arg := s[:2], s[2:]
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
	if err := r.failure("poll"); err != nil {
		return err
	}
	reqs := [][2]string{{"PC;", "PC"}, {"FW;", "FW"}, {"FL;", "FL"}}
	if tier == backend.PollFast {
		reqs = [][2]string{{"FA;", "FA"}, {"MD;", "MD"}, {"DA;", "DA"}, {"PT;", "PT"}, {"SM;", "SM"}}
	}
	for _, q := range reqs {
		if _, err := c.Do(ctx, []byte(q[0]), backend.Key(q[1])); err != nil {
			return err
		}
	}
	return nil
}

func (r *fakeRig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if err := r.failure("frequency"); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FA%011d;", hz)), "FA")
	return err
}

func (r *fakeRig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	if err := r.failure("mode"); err != nil {
		return err
	}
	if _, err := c.Do(ctx, []byte(fmt.Sprintf("MD%d;", int(m))), "MD"); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("DA%d;", btoi(dataMode))), "DA")
	return err
}

func (r *fakeRig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	if err := r.failure("power"); err != nil {
		return err
	}
	pct := 0.0
	switch {
	case p.Pct != nil:
		pct = *p.Pct
	case p.Watts != nil:
		pct = *p.Watts // the fake rig is 100 W full scale, so watts == percent
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("PC%03d;", int(pct))), "PC")
	return err
}

func (r *fakeRig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	if err := r.failure("ptt"); err != nil {
		return err
	}
	return c.Send(ctx, []byte(fmt.Sprintf("PT%d;", btoi(on))))
}

func (r *fakeRig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	if err := r.failure("width"); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FW%04d;", hz)), "FW")
	return err
}

func (r *fakeRig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	if err := r.failure("slot"); err != nil {
		return err
	}
	_, err := c.Do(ctx, []byte(fmt.Sprintf("FL%d;", slot)), "FL")
	return err
}

// --- CW ----------------------------------------------------------------

type fakeCW struct {
	mu         sync.Mutex
	status     radio.CWStatus
	enqueueErr error
	speedErr   error
	sent       []string
	aborts     int
}

func newFakeCW() *fakeCW {
	return &fakeCW{status: radio.CWStatus{WPM: 25}}
}

func (c *fakeCW) Enqueue(text string, mode cw.Mode) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enqueueErr != nil {
		return 0, c.enqueueErr
	}
	if mode == cw.Replace {
		c.status.Queued = 0
	}
	n := len([]rune(text))
	c.status.Queued += n
	c.sent = append(c.sent, text)
	return n, nil
}

func (c *fakeCW) Abort() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.aborts++
	c.status.Queued = 0
	c.status.Busy = false
}

func (c *fakeCW) Status() radio.CWStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *fakeCW) SetSpeed(wpm int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.speedErr != nil {
		return c.speedErr
	}
	c.status.WPM = wpm
	return nil
}

func (c *fakeCW) Charset() string            { return "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 /?.," }
func (c *fakeCW) Method() radio.CWMethod     { return radio.CWViaCAT }
func (c *fakeCW) WPMRange() (int, int, bool) { return 0, 0, false }

func (c *fakeCW) abortCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.aborts
}

func (c *fakeCW) fail(err error) {
	c.mu.Lock()
	c.enqueueErr = err
	c.mu.Unlock()
}

// --- environment -------------------------------------------------------

// logBuffer collects log output. slog handlers are called from several
// goroutines, so it has to be safe under -race.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t     *testing.T
	cfg   *config.Config
	mgr   *rig.Manager
	locks *lock.Manager
	srv   *server
	h     http.Handler
	logs  *logBuffer

	rigs map[string]*fakeRig
	cw   *fakeCW
	now  func() time.Time
}

type envOpts struct {
	lockEnabled bool
	allowSteal  bool
	now         func() time.Time
	noCW        bool
}

func newEnv(t *testing.T, mutate ...func(*envOpts)) *env {
	t.Helper()

	o := envOpts{lockEnabled: true}
	for _, m := range mutate {
		m(&o)
	}

	cfg := &config.Config{
		Server: config.Server{BasePath: config.DefaultBasePath},
		Lock: config.Lock{
			Enabled:    o.lockEnabled,
			TTL:        config.Duration(30 * time.Second),
			AllowSteal: o.allowSteal,
		},
		Radios: []config.Radio{
			{
				ID: connectedRadio, Name: "Icom IC-7610", Backend: config.BackendCIV,
				Poll: config.Poll{
					Interval:     config.Duration(50 * time.Millisecond),
					SlowInterval: config.Duration(200 * time.Millisecond),
				},
				Limits: config.Limits{
					MaxPowerPct: 80,
					TXTimeout:   config.Duration(120 * time.Second),
					Bands:       []config.Band{{LowHz: 14_000_000, HighHz: 14_350_000}},
				},
			},
			{
				ID: disconnectedRadio, Name: "Kenwood TS-590SG", Backend: config.BackendKenwood,
				Poll: config.Poll{
					Interval:     config.Duration(50 * time.Millisecond),
					SlowInterval: config.Duration(200 * time.Millisecond),
				},
			},
		},
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	logs := &logBuffer{}

	e := &env{
		t: t, cfg: cfg, logs: logs,
		rigs: map[string]*fakeRig{},
		now:  o.now,
	}

	var sessions []*rig.Session
	for i := range cfg.Radios {
		rc := cfg.Radios[i]
		fr := newFakeRig()
		e.rigs[rc.ID] = fr
		s, err := rig.NewSession(rc, fr, &fakeDialer{dev: newFakeDevice()},
			rig.WithLogger(quiet), rig.WithCommandTimeout(200*time.Millisecond))
		if err != nil {
			t.Fatalf("NewSession(%s): %v", rc.ID, err)
		}
		sessions = append(sessions, s)
	}

	mgr, err := rig.NewManagerWithSessions(sessions...)
	if err != nil {
		t.Fatalf("NewManagerWithSessions: %v", err)
	}
	e.mgr = mgr

	// Only the first radio is started. The second stands in for a rig that is
	// switched off or unplugged, which is what makes 503 reachable without
	// tearing a connection down mid-test.
	ctx, cancel := context.WithCancel(context.Background())
	connected, _ := mgr.Get(connectedRadio)
	connected.Start(ctx)
	waitFor(t, "radio to connect", connected.Connected)

	if !o.noCW {
		e.cw = newFakeCW()
		connected.SetCWSender(e.cw)
	}

	e.locks = lock.NewManager(cfg.Lock, lock.WithLogger(quiet))

	a, err := auth.New(config.Auth{
		Realm:      "remoses",
		BcryptCost: 4,
		CacheTTL:   config.Duration(time.Minute),
		Users:      []config.User{{Username: testUser, PasswordBcrypt: testHash()}},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}

	opts := []Option{WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))}
	if o.now != nil {
		opts = append(opts, WithNow(o.now))
	}
	// newServer rather than New, so that the tests can enumerate the route
	// table they are asserting against instead of restating it.
	e.srv = newServer(cfg, mgr, e.locks, a, stubHandler("ws"), stubHandler("ws-ticket"), opts...)
	e.h = e.srv.handler()

	t.Cleanup(func() {
		cancel()
		_ = mgr.Close()
		e.locks.Close()
	})
	return e
}

func stubHandler(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"stub":"`+name+`"}`)
	})
}

// req builds an authenticated request. body may be nil, a string, or anything
// json.Marshal accepts.
func (e *env) req(method, path string, body any) *http.Request {
	e.t.Helper()

	var rdr io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatalf("marshalling request body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}

	r := httptest.NewRequest(method, config.DefaultBasePath+path, rdr)
	r.SetBasicAuth(testUser, testPass)
	if rdr != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	return r
}

func (e *env) send(r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, r)
	return rr
}

// do is the common case: an authenticated request holding the lock, if one is
// needed.
func (e *env) do(method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(e.req(method, path, body))
}

// withLock returns a request carrying a freshly acquired token for radioID.
func (e *env) acquire(radioID string) string {
	e.t.Helper()
	rr := e.do(http.MethodPost, "/radios/"+radioID+"/lock", nil)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("acquiring lock: status %d, body %s", rr.Code, rr.Body.String())
	}
	var l lockDTO
	e.decode(rr, &l)
	return l.Token
}

// stateBody is the wire shape of a state response, spelled out rather than
// reusing the server's own types: a test that decoded into stateDTO would
// agree with the handler about a field name even when both were wrong.
type stateBody struct {
	Frequency  uint64 `json:"frequency"`
	Mode       string `json:"mode"`
	DataMode   bool   `json:"data_mode"`
	PassbandHz int    `json:"passband_hz"`
	FilterSlot int    `json:"filter_slot"`
	Power      struct {
		Watts  *float64 `json:"watts"`
		Pct    float64  `json:"pct"`
		Native int      `json:"native"`
	} `json:"power"`
	PTT    bool `json:"ptt"`
	SMeter struct {
		Raw   int `json:"raw"`
		Scale int `json:"scale"`
	} `json:"s_meter"`
	CW struct {
		Busy   bool `json:"busy"`
		Queued int  `json:"queued"`
		WPM    int  `json:"wpm"`
	} `json:"cw"`
	Connected bool   `json:"connected"`
	Seq       uint64 `json:"seq"`
	UpdatedAt string `json:"updated_at"`
	AgeMS     int64  `json:"age_ms"`
	Stale     bool   `json:"stale"`
}

// doLocked issues a request carrying a lock token in the header.
func (e *env) doLocked(method, path string, body any, token string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := e.req(method, path, body)
	if token != "" {
		r.Header.Set(lockHeader, token)
	}
	return e.send(r)
}

func (e *env) decode(rr *httptest.ResponseRecorder, dst any) {
	e.t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), dst); err != nil {
		e.t.Fatalf("decoding response %q: %v", rr.Body.String(), err)
	}
}

// problemOf decodes a problem document, asserting the media type and status.
func (e *env) problemOf(rr *httptest.ResponseRecorder, want int) map[string]any {
	e.t.Helper()
	if rr.Code != want {
		e.t.Fatalf("status = %d, want %d (body %s)", rr.Code, want, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		e.t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	var doc map[string]any
	e.decode(rr, &doc)
	if got := doc["status"]; got != float64(want) {
		e.t.Errorf("problem status member = %v, want %d", got, want)
	}
	if doc["title"] == "" || doc["title"] == nil {
		e.t.Error("problem document has no title")
	}
	return doc
}

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
