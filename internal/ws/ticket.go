package ws

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/hessu/remoses/internal/auth"
)

// ticketTTL is how long an issued ticket stays usable.
//
// Short by design. A ticket is a bearer credential that travels in a query
// string, which means it lands in proxy logs and browser history, so its useful
// life is exactly "long enough to open one socket".
const ticketTTL = 30 * time.Second

// ticketBytes is the entropy per ticket: 128 bits, so guessing is not a threat
// model that needs thinking about.
const ticketBytes = 16

// maxTickets bounds the store. Every ticket is issued to an already
// authenticated user, so this is not an anti-abuse control; it stops a client
// that requests tickets it never uses from growing the map without limit.
const maxTickets = 1024

// ticket is one issued, unused credential.
type ticket struct {
	value   string
	user    string
	expires time.Time
}

// ticketStore holds unredeemed tickets.
//
// Tickets are kept in a slice and matched by scanning it with a constant-time
// comparison rather than by map lookup: a map probe branches on the secret, and
// the cost of scanning a list that expires itself every 30 seconds is nothing.
type ticketStore struct {
	ttl time.Duration
	// now is swapped by tests to drive expiry without sleeping.
	now func() time.Time

	mu      sync.Mutex
	tickets []ticket
}

func newTicketStore(ttl time.Duration) *ticketStore {
	if ttl <= 0 {
		ttl = ticketTTL
	}
	return &ticketStore{ttl: ttl, now: time.Now}
}

// issue mints a ticket bound to user.
func (s *ticketStore) issue(user string) (ticket, error) {
	var b [ticketBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ticket{}, err
	}
	now := s.clock()
	t := ticket{
		value:   base64.RawURLEncoding.EncodeToString(b[:]),
		user:    user,
		expires: now.Add(s.ttl),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)
	if len(s.tickets) >= maxTickets {
		// Oldest first: it is the closest to expiring anyway.
		s.tickets = s.tickets[1:]
	}
	s.tickets = append(s.tickets, t)
	return t, nil
}

// redeem consumes a ticket and returns the user it was issued to.
//
// Single use is the point: a ticket that survived its first use would be a
// password with a shorter lifetime, and query strings are not a place to keep
// passwords.
func (s *ticketStore) redeem(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	now := s.clock()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked(now)

	want := []byte(value)
	for i := range s.tickets {
		if subtle.ConstantTimeCompare([]byte(s.tickets[i].value), want) != 1 {
			continue
		}
		user := s.tickets[i].user
		s.tickets = append(s.tickets[:i], s.tickets[i+1:]...)
		return user, true
	}
	return "", false
}

// sweepLocked drops expired tickets. Nothing runs on a timer: eviction rides
// along with the calls that touch the store, which for a 30 second TTL is often
// enough that the slice never grows.
func (s *ticketStore) sweepLocked(now time.Time) {
	live := s.tickets[:0]
	for _, t := range s.tickets {
		if now.Before(t.expires) {
			live = append(live, t)
		}
	}
	// Clear the tail so redeemed and expired ticket values do not linger in the
	// backing array.
	for i := len(live); i < len(s.tickets); i++ {
		s.tickets[i] = ticket{}
	}
	s.tickets = live
}

func (s *ticketStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// count reports the number of live tickets. Used by tests.
func (s *ticketStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tickets)
}

// ticketResponse is the body of POST /ws-ticket.
type ticketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TicketHandler serves POST /ws-ticket.
//
// Browsers cannot set headers on a WebSocket handshake, which is the entire
// reason this endpoint exists: the browser authenticates once over plain HTTP,
// where it can send Authorization, and carries the result to the upgrade as a
// query parameter.
//
// It is meant to be mounted behind the Basic auth middleware, and uses the
// authenticated username from the request context when one is there. It also
// verifies an Authorization header itself, so that mounting it unwrapped is
// merely redundant rather than a hole.
func (h *Hub) TicketHandler(a *auth.Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			problem(w, http.StatusMethodNotAllowed, "Method Not Allowed",
				"ws-ticket accepts POST only")
			return
		}

		user, ok := auth.UserFrom(r.Context())
		if !ok {
			username, password, hasAuth := r.BasicAuth()
			if a == nil || !hasAuth || !a.Verify(username, password) {
				if a != nil {
					a.Unauthorized(w)
					return
				}
				problem(w, http.StatusUnauthorized, "Unauthorized",
					"valid HTTP Basic credentials are required")
				return
			}
			user = username
		}

		t, err := h.tickets.issue(user)
		if err != nil {
			h.log.Error("issuing websocket ticket", "err", err)
			problem(w, http.StatusInternalServerError, "Internal Server Error",
				"could not issue a ticket")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ticketResponse{
			Ticket:    t.value,
			ExpiresAt: t.expires,
		})
	})
}
