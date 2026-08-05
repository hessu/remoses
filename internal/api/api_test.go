package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
)

// pathFor fills the {radioId} placeholder in a route table path.
func pathFor(p, radioID string) string {
	return strings.ReplaceAll(p, "{radioId}", radioID)
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	e := newEnv(t)

	r := httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/healthz", nil)
	rr := e.send(r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got healthDTO
	e.decode(rr, &got)
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got.RadiosTotal != 2 || got.RadiosConnected != 1 {
		t.Errorf("radios = %d/%d connected, want 1/2", got.RadiosConnected, got.RadiosTotal)
	}
}

// A radio that is not connected must not make the daemon look dead: the
// process is fine, the USB cable is not.
func TestHealthzReportsDisconnectedRadiosWithoutFailing(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodGet, "/healthz", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
}

func TestEveryRouteButHealthzAndWSRequiresAuthentication(t *testing.T) {
	e := newEnv(t)

	for _, rt := range e.srv.routes() {
		if rt.public {
			continue
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			r := httptest.NewRequest(rt.method,
				config.DefaultBasePath+pathFor(rt.path, connectedRadio), nil)
			rr := e.send(r)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rr.Code, rr.Body.String())
			}
			if rr.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 without a WWW-Authenticate challenge")
			}
		})
	}
}

// The WebSocket upgrade cannot sit behind the Basic middleware, because a
// browser cannot put credentials on a handshake; it authenticates by ticket
// inside the stream handler instead.
func TestWebSocketUpgradeIsNotBehindBasicAuth(t *testing.T) {
	e := newEnv(t)

	r := httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/ws", nil)
	rr := e.send(r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ws"`) {
		t.Fatalf("status = %d, body %s; want the stream handler to be reached",
			rr.Code, rr.Body.String())
	}
}

func TestWebSocketTicketIsAuthenticatedAndDelegated(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodPost, "/ws-ticket", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ws-ticket"`) {
		t.Fatalf("status = %d, body %s; want the ticket handler to be reached",
			rr.Code, rr.Body.String())
	}
}

// A stream that is not wired up must not look like a client typing the wrong
// path, so the route stays registered and says so.
func TestWebSocketRoutesAnswer503WhenNotWired(t *testing.T) {
	e := newEnv(t)
	e.srv.ws, e.srv.wsTicket = nil, nil

	rr := e.send(httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/ws", nil))
	e.problemOf(rr, http.StatusServiceUnavailable)

	rr = e.do(http.MethodPost, "/ws-ticket", nil)
	e.problemOf(rr, http.StatusServiceUnavailable)
}

func TestNewMountsUnderTheConfiguredBasePath(t *testing.T) {
	e := newEnv(t)
	h := New(e.cfg, e.mgr, e.locks, nil, nil, nil)

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz under the base path: status = %d, want 200", rr.Code)
	}

	// ... and nowhere else.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("healthz outside the base path: status = %d, want 404", rr.Code)
	}
}

func TestNormalizeBase(t *testing.T) {
	// A base path without the leading slash would be read by ServeMux as a
	// host pattern and match nothing, which is a silently broken daemon.
	for in, want := range map[string]string{
		"/api/v1":  "/api/v1",
		"api/v1":   "/api/v1",
		"/api/v1/": "/api/v1",
		"/":        "",
		"":         "",
	} {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnknownPathIsAProblemDocument(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodGet, "/nope", nil)
	e.problemOf(rr, http.StatusNotFound)
}

func TestUnknownRadioIs404OnEveryRadioRoute(t *testing.T) {
	e := newEnv(t)

	for _, rt := range e.srv.routes() {
		if !strings.Contains(rt.path, "{radioId}") {
			continue
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			rr := e.do(rt.method, pathFor(rt.path, "nosuchrig"), map[string]any{})
			doc := e.problemOf(rr, http.StatusNotFound)
			if doc["radio_id"] != "nosuchrig" {
				t.Errorf("radio_id = %v, want nosuchrig", doc["radio_id"])
			}
		})
	}
}

func TestListRadiosReportsBothRadiosInConfigurationOrder(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodGet, "/radios", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var radios []radioDTO
	e.decode(rr, &radios)
	if len(radios) != 2 {
		t.Fatalf("got %d radios, want 2", len(radios))
	}
	if radios[0].ID != connectedRadio || radios[1].ID != disconnectedRadio {
		t.Errorf("order = %s, %s; want configuration order", radios[0].ID, radios[1].ID)
	}
	if !radios[0].Connected || radios[1].Connected {
		t.Errorf("connected = %v, %v; want true, false", radios[0].Connected, radios[1].Connected)
	}
	if radios[0].Backend != config.BackendCIV {
		t.Errorf("backend = %q, want %q", radios[0].Backend, config.BackendCIV)
	}
}

func TestGetRadioPublishesCapabilitiesAndLimits(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodGet, "/radios/"+connectedRadio, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got radioDTO
	e.decode(rr, &got)
	if got.Caps.FilterSlots != 3 || got.Caps.SMeterScale != 30 {
		t.Errorf("caps = %+v, want the session's", got.Caps)
	}
	if got.Limits.MaxPowerPct != 80 {
		t.Errorf("limits.max_power_pct = %v, want 80", got.Limits.MaxPowerPct)
	}
	if got.Limits.TXTimeoutS != 120 {
		t.Errorf("limits.tx_timeout_s = %v, want 120", got.Limits.TXTimeoutS)
	}
	if len(got.Limits.Bands) != 1 || !strings.HasPrefix(got.Limits.Bands[0], "14.") {
		t.Errorf("limits.bands = %v, want one 20 m band", got.Limits.Bands)
	}
}

// A cached descriptor would show a radio as connected long after its cable
// came out, so nothing here may be stored.
func TestResponsesAreNotCacheable(t *testing.T) {
	e := newEnv(t)

	for _, path := range []string{"/radios", "/radios/" + connectedRadio,
		"/radios/" + connectedRadio + "/state", "/radios/" + connectedRadio + "/cw"} {
		rr := e.do(http.MethodGet, path, nil)
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, got)
		}
	}
}
