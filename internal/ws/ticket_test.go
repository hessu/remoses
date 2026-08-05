package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// issueTicket calls POST /ws-ticket the way a browser would, and returns the
// decoded body.
func (h *harness) issueTicket(path string, withAuth bool) (*http.Response, ticketResponse) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url(path), nil)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if withAuth {
		req.SetBasicAuth(testUser, testPass)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	var body ticketResponse
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			h.t.Fatalf("decode ticket: %v", err)
		}
	}
	return resp, body
}

// TestTicketIssueUseReplay is the whole point of the ticket: a browser cannot
// send Authorization on a WebSocket handshake, so it trades one authenticated
// HTTP request for one socket — and only one.
func TestTicketIssueUseReplay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	resp, body := h.issueTicket("/ws-ticket", true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if body.Ticket == "" {
		t.Fatal("empty ticket")
	}
	if len(body.Ticket) < 20 {
		t.Errorf("ticket %q is shorter than 128 bits of base64url", body.Ticket)
	}
	if want := h.clock.Now().Add(ticketTTL); !body.ExpiresAt.Equal(want) {
		t.Errorf("expires_at = %s, want %s", body.ExpiresAt, want)
	}

	// Use it. No Authorization header anywhere, exactly as in a browser.
	c := h.dial("/ws?ticket="+url.QueryEscape(body.Ticket), nil)
	hello := readMsg(t, c)
	if str(hello, "type") != "hello" {
		t.Fatalf("first message = %v, want hello", hello)
	}
	if h.hub.tickets.count() != 0 {
		t.Errorf("ticket survived being used: %d left", h.hub.tickets.count())
	}

	// Replay must fail.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	replay, resp2, err := websocket.Dial(ctx, h.url("/ws?ticket="+url.QueryEscape(body.Ticket)), nil)
	if err == nil {
		_ = replay.CloseNow()
		t.Fatal("a used ticket was accepted a second time")
	}
	if resp2 == nil || resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status = %v, want 401", resp2)
	}
}

// TestTicketExpires covers the 30 second life.
func TestTicketExpires(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	_, body := h.issueTicket("/ws-ticket", true)
	if body.Ticket == "" {
		t.Fatal("empty ticket")
	}

	h.clock.advance(ticketTTL + time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, h.url("/ws?ticket="+url.QueryEscape(body.Ticket)), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("an expired ticket was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}

	// Expired tickets are swept rather than accumulating.
	if _, err := h.hub.tickets.issue("someone"); err != nil {
		t.Fatalf("issue: %v", err)
	}
	if got := h.hub.tickets.count(); got != 1 {
		t.Errorf("%d tickets held, want the expired one swept", got)
	}
}

// TestTicketRejectsGarbage covers the redeem path against values that were
// never issued.
func TestTicketRejectsGarbage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	_, body := h.issueTicket("/ws-ticket", true)

	for name, value := range map[string]string{
		"empty":    "",
		"random":   "AAAAAAAAAAAAAAAAAAAAAA",
		"prefix":   body.Ticket[:len(body.Ticket)-1],
		"extended": body.Ticket + "A",
	} {
		if user, ok := h.hub.tickets.redeem(value); ok {
			t.Errorf("%s ticket %q was redeemed as %q", name, value, user)
		}
	}

	// The real one must still work: a failed attempt does not consume it.
	if user, ok := h.hub.tickets.redeem(body.Ticket); !ok || user != testUser {
		t.Fatalf("redeem = %q, %v; want %q, true", user, ok, testUser)
	}
}

// TestTicketRequiresAuth covers both mounts: behind the middleware, and bare.
func TestTicketRequiresAuth(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	// Behind the middleware, the middleware rejects it.
	resp, _ := h.issueTicket("/ws-ticket", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}

	// Mounted bare, the handler rejects it itself, with the same challenge.
	resp, _ = h.issueTicket("/ws-ticket-open", false)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("no Basic challenge on the bare mount")
	}

	// And accepts a valid header without the middleware's help.
	resp, body := h.issueTicket("/ws-ticket-open", true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if body.Ticket == "" {
		t.Fatal("empty ticket")
	}
}

// TestTicketMethodNotAllowed keeps a ticket from being minted by a GET, which
// would put a bearer credential in a URL that anything might follow.
func TestTicketMethodNotAllowed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	req, err := http.NewRequest(http.MethodGet, h.url("/ws-ticket-open"), nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.SetBasicAuth(testUser, testPass)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

// TestTicketBoundToIssuingUser proves a ticket carries the identity it was
// issued with rather than being an anonymous pass.
func TestTicketBoundToIssuingUser(t *testing.T) {
	t.Parallel()
	h := newHarness(t, harnessOpts{})

	tk, err := h.hub.tickets.issue("someone-else")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	user, ok := h.hub.tickets.redeem(tk.value)
	if !ok || user != "someone-else" {
		t.Fatalf("redeem = %q, %v; want someone-else, true", user, ok)
	}
}
