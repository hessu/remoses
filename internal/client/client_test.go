package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/wire"
)

const (
	testUser = "oh2xyz"
	testPass = "hunter2"
	testBase = "/api/v1"
)

// stateJSON is the example from DESIGN.md §8, so this test fails if the wire
// shape the client expects and the shape the contract documents ever part
// company.
const stateJSON = `{
  "frequency": 14025000, "mode": "CW", "data_mode": false,
  "passband_hz": 500, "filter_slot": 2,
  "power": { "pct": 40.0, "watts": null, "native": 102 },
  "ptt": false,
  "s_meter": { "raw": 78, "scale": 255, "s": 5.5 },
  "cw": { "busy": false, "queued": 0, "wpm": 28 },
  "connected": true,
  "seq": 4471, "updated_at": "2026-08-04T20:11:04Z", "age_ms": 120, "stale": false
}`

const radioJSON = `{
  "id": "ic7610", "name": "IC-7610", "backend": "civ", "connected": true,
  "caps": { "modes": ["LSB","USB","CW"], "vfos": ["A","B"],
            "power_watt_accurate": false, "filter_width": true, "filter_slots": 3,
            "s_meter_scale": 255, "cw_method": "cat", "cw_max_wpm": 48 },
  "limits": { "max_power_pct": 50, "tx_timeout_s": 120, "bands": ["14.000000-14.350000MHz"] },
  "lock": { "held": true, "holder": "oh2abc", "is_mine": false }
}`

// recorder is a test server that remembers every method and path it was asked
// for, which is how "this client only ever issues GET" is checked rather than
// asserted.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req.Method+" "+req.URL.Path)
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func newServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		user, pass, ok := r.BasicAuth()
		if !ok || user != testUser || pass != testPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="remoses"`)
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"about:blank","title":"Unauthorized","status":401,` +
				`"detail":"missing or invalid credentials"}`))
			return
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL+testBase, testUser, testPass)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func serveJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func TestStateDecodesTheDocumentedShape(t *testing.T) {
	srv, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != testBase+"/radios/ic7610/state" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		serveJSON(w, stateJSON)
	})

	st, err := newClient(t, srv).State(context.Background(), "ic7610")
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if st.Frequency != 14025000 {
		t.Errorf("frequency = %d", st.Frequency)
	}
	if st.Mode != wire.ModeCW {
		t.Errorf("mode = %v", st.Mode)
	}
	if st.PassbandHz != 500 || st.FilterSlot != 2 {
		t.Errorf("filter = %d Hz slot %d", st.PassbandHz, st.FilterSlot)
	}
	// watts is null in the fixture: a rig with no watt-accurate scale. Null is
	// not the same as absent, and the generated type keeps them apart, so this
	// asserts the reading is unavailable rather than merely unmentioned.
	if st.Power.Pct != 40 || st.Power.Native != 102 || !st.Power.Watts.IsNull() {
		t.Errorf("power = %+v", st.Power)
	}
	if s, err := st.SMeter.S.Get(); err != nil || s != 5.5 {
		t.Errorf("s_meter = %+v (s: %v)", st.SMeter, err)
	}
	if st.SMeter.Raw != 78 || st.SMeter.Scale != 255 {
		t.Errorf("s_meter = %+v", st.SMeter)
	}
	if st.CW.WPM != 28 {
		t.Errorf("cw = %+v", st.CW)
	}
	if !st.Connected || IsStale(st) {
		t.Errorf("connected = %t, stale = %t", st.Connected, IsStale(st))
	}
	if st.Seq != 4471 {
		t.Errorf("seq = %d", st.Seq)
	}
	if got, want := Age(st), 120*time.Millisecond; got != want {
		t.Errorf("Age = %v, want %v", got, want)
	}

	if calls := rec.snapshot(); len(calls) != 1 || !strings.HasPrefix(calls[0], "GET ") {
		t.Errorf("calls = %v, want one GET", calls)
	}
}

// A disconnected radio is a normal state served with 200, not an error. The
// last snapshot is still the most useful thing the client can show.
func TestStateOfADisconnectedRadioIsNotAnError(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, `{"frequency":14025000,"mode":"CW","connected":false,
			"s_meter":{"raw":0,"scale":255},"cw":{"busy":false,"queued":0,"wpm":25},
			"power":{"pct":0,"native":0,"watts":null},
			"seq":9,"updated_at":"2026-08-04T20:11:04Z","age_ms":134000,"stale":true}`)
	})

	st, err := newClient(t, srv).State(context.Background(), "ic7610")
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Connected {
		t.Error("expected connected = false")
	}
	if !IsStale(st) {
		t.Error("expected stale = true")
	}
	if st.Frequency != 14025000 {
		t.Errorf("the stale snapshot lost its frequency: %d", st.Frequency)
	}
}

func TestRadioDescriptor(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, radioJSON)
	})

	rd, err := newClient(t, srv).Radio(context.Background(), "ic7610")
	if err != nil {
		t.Fatalf("Radio: %v", err)
	}
	if rd.Name != "IC-7610" || rd.Backend != "civ" || !rd.Connected {
		t.Errorf("descriptor = %+v", rd)
	}
	if !rd.Lock.Held || rd.Lock.Holder == nil || *rd.Lock.Holder != "oh2abc" || rd.Lock.IsMine {
		t.Errorf("lock = %+v", rd.Lock)
	}
	if rd.Caps.CWMethod != wire.CWMethodCat || rd.Caps.SMeterScale != 255 {
		t.Errorf("caps = %+v", rd.Caps)
	}
	if rd.Limits.Bands == nil || len(*rd.Limits.Bands) != 1 {
		t.Errorf("limits = %+v", rd.Limits)
	}
}

func TestBadCredentialsAreReportedAsSuch(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, stateJSON)
	})

	c, err := New(srv.URL+testBase, testUser, "wrong")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.State(context.Background(), "ic7610")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error")
		}
		if !IsUnauthorized(err) {
			t.Fatalf("IsUnauthorized = false for %v", err)
		}
		if !Fatal(err) {
			t.Error("a rejected password must not be retried")
		}
		if !strings.Contains(err.Error(), "missing or invalid credentials") {
			t.Errorf("problem detail is missing from %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a rejected password hung instead of returning an error")
	}
}

func TestUnknownRadioIsNotFound(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"title":"Not Found","status":404,` +
			`"detail":"no radio with that id is configured","radio_id":"nope"}`))
	})

	_, err := newClient(t, srv).State(context.Background(), "nope")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound = false for %v", err)
	}
	if !Fatal(err) {
		t.Error("a radio that does not exist will not appear by retrying")
	}
}

// A server that answers with something other than a problem document still has
// to produce a usable error rather than a decode failure.
func TestNonProblemErrorBody(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy is unhappy</html>"))
	})

	_, err := newClient(t, srv).State(context.Background(), "ic7610")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error does not carry the status: %v", err)
	}
	if Fatal(err) {
		t.Error("a gateway error is worth retrying")
	}
}

// Read-only is the point of this package, so it is checked rather than trusted:
// exercising everything the client can do must leave a server that saw nothing
// but GETs, and nothing addressed to a lock.
func TestClientOnlyIssuesGets(t *testing.T) {
	srv, rec := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/state") {
			serveJSON(w, stateJSON)
			return
		}
		serveJSON(w, radioJSON)
	})

	c := newClient(t, srv)
	if _, err := c.Radio(context.Background(), "ic7610"); err != nil {
		t.Fatalf("Radio: %v", err)
	}
	if _, err := c.State(context.Background(), "ic7610"); err != nil {
		t.Fatalf("State: %v", err)
	}

	calls := rec.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("non-GET request %q", call)
		}
		if strings.Contains(call, "/lock") {
			t.Errorf("monitor touched a lock: %q", call)
		}
	}
}

// A radio id goes into a URL path unescaped — the generated request builder
// interpolates it — so an id that is not the daemon's own shape must be refused
// here rather than turned into a request to whatever path it happens to spell.
func TestUnusableRadioIDsAreRefusedBeforeAnyRequest(t *testing.T) {
	srv, rec := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		serveJSON(w, stateJSON)
	})
	c := newClient(t, srv)

	for _, id := range []string{"../lock", "ic7610/state", "IC7610", "", "ic7610?x=1"} {
		if _, err := c.State(context.Background(), id); err == nil {
			t.Errorf("State(%q): expected an error", id)
		}
		if _, err := c.Radio(context.Background(), id); err == nil {
			t.Errorf("Radio(%q): expected an error", id)
		}
		if _, err := c.Stream(context.Background(), id); err == nil {
			t.Errorf("Stream(%q): expected an error", id)
		}
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Errorf("requests were sent for unusable ids: %v", calls)
	}
}

func TestNewRejectsUnusableURLs(t *testing.T) {
	for _, in := range []string{"ftp://host/api", "http://", "://nonsense"} {
		if _, err := New(in, testUser, testPass); err == nil {
			t.Errorf("New(%q): expected an error", in)
		}
	}
}
