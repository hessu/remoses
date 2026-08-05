package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
)

// The contract offers programmatic clients the ordinary Basic header on the
// upgrade, and says the ticket exists only because a browser cannot set one.
// The route is registered public, so nothing upstream verifies that header —
// which meant a correct Basic upgrade reached the stream handler with no
// authenticated user in the context and was answered with 401.
func TestWebSocketUpgradeAcceptsBasicAuth(t *testing.T) {
	e := newEnv(t)

	// The stream handler is a stub here, so what is asserted is that the
	// request arrives authenticated.
	var gotUser string
	var haveUser bool
	e.srv.ws = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, haveUser = auth.UserFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/ws", nil)
	r.SetBasicAuth(testUser, testPass)

	rr := e.send(r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if !haveUser || gotUser != testUser {
		t.Errorf("stream handler saw user %q (present %t), want %q", gotUser, haveUser, testUser)
	}
}

// A wrong password on the upgrade is a wrong password, not an invitation to go
// and fetch a ticket.
func TestWebSocketUpgradeRejectsBadBasicAuth(t *testing.T) {
	e := newEnv(t)
	e.srv.ws = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the stream handler was reached with invalid credentials")
	})

	r := httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/ws", nil)
	r.SetBasicAuth(testUser, "wrong")

	rr := e.send(r)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("WWW-Authenticate") == "" {
		t.Error("401 without a Basic challenge")
	}
}

// The browser path is unchanged: no Authorization header still reaches the
// stream handler, which redeems a ticket instead.
func TestWebSocketUpgradeWithoutCredentialsStillReachesTheTicketPath(t *testing.T) {
	e := newEnv(t)

	r := httptest.NewRequest(http.MethodGet, config.DefaultBasePath+"/ws", nil)
	rr := e.send(r)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"ws"`) {
		t.Fatalf("status = %d, body %s; want the stream handler to be reached",
			rr.Code, rr.Body.String())
	}
}
