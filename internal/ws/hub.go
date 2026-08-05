// Package ws implements the WebSocket state stream: one connection carries
// every state change for every radio, to many concurrent clients.
//
// One invariant carries this package, and everything in it exists to protect
// that invariant: A SLOW CLIENT MUST NEVER SLOW A RIG SESSION. A serial port
// with a transmitter on the end of it cannot be made to wait for someone's
// laptop to wake up. So nothing here ever blocks a publisher:
//
//   - The hub holds ONE subscription to the rig manager and fans out to clients
//     with non-blocking sends into per-client queues. One subscription rather
//     than one per client also keeps the manager's forwarding goroutines
//     proportional to the number of radios, not to the number of browsers.
//   - Each client has two lanes. State is last-value-wins, so the state lane
//     coalesces per radio — a client that is not reading accumulates exactly one
//     pending update per radio no matter how long it stalls — and is then rate
//     limited to one message per radio per ws.min_interval. Spinning a VFO knob
//     with Transceive on produces hundreds of updates a second and none of them
//     is worth queueing.
//   - cw and conn events are discrete: "the queue emptied" and "the queue
//     filled again" are different facts, not two versions of one, so they go
//     into a bounded FIFO that is never merged. When it overflows, the lane is
//     dropped and the client is told to resync rather than being handed a
//     truncated history or silently disconnected.
//
// The stream is read-only and needs no lock. All control stays on REST, so the
// socket has no authorisation surface at all: the only client messages are ping
// and subscribe, and anything else is ignored rather than acted on.
//
// # The lock envelope
//
// The API contract lists a "lock" frame, and this package does not emit one.
// internal/lock has no event feed: acquisition, renewal and expiry are visible
// only to whoever called them, and the only way to publish them from here would
// be to poll the lock manager on a timer. A timer that reports a 30 second lease
// is either too slow to be useful or too fast to be free, and inventing one to
// fill a gap in the frame list would be worse than the gap. When the lock
// manager grows a subscription the frame belongs here; until then clients read
// lock state from GET /radios and GET /radios/{id}/lock.
package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// defaultVersion is reported in hello when main does not supply the build
// version. It tracks the API version in api/openapi.yaml, because that is what
// a client is actually asking about.
const defaultVersion = "0.1.0"

// readLimit bounds a single client message. The whole client vocabulary is a
// type and a list of radio ids, so anything larger is a mistake or an attack.
const readLimit = 16 << 10

// writeTimeout bounds one message write. It is generous: a stalled client is
// meant to be absorbed by coalescing, not killed. It exists only so that a
// connection black-holed by the network cannot pin a goroutine forever; the
// keepalive is what actually notices a dead peer.
const writeTimeout = 30 * time.Second

// maxMissedPongs is how many keepalive pings may go unanswered before the
// connection is closed. This is not cosmetic: dropping a dead client promptly
// is what lets its lock expire, which is what drops PTT.
const maxMissedPongs = 2

// Option configures a Hub.
type Option func(*Hub)

// WithLogger sets the logger. The hub adds connection attributes itself.
func WithLogger(l *slog.Logger) Option {
	return func(h *Hub) {
		if l != nil {
			h.log = l
		}
	}
}

// WithVersion sets the version string reported in hello. main passes the build
// version; the default is the API version.
func WithVersion(v string) Option {
	return func(h *Hub) {
		if v != "" {
			h.version = v
		}
	}
}

// WithOriginPatterns authorises browser origins other than the request host.
//
// The default is host-only, which is right when the UI is served by remoses
// itself. A UI served from somewhere else needs its origin listed here, and
// listing one is a deliberate CSRF decision rather than something to do by
// reflex.
func WithOriginPatterns(patterns ...string) Option {
	return func(h *Hub) { h.origins = patterns }
}

// Hub owns the fan-out from the rig manager to every connected client.
type Hub struct {
	mgr     *rig.Manager
	log     *slog.Logger
	version string
	origins []string
	tickets *ticketStore

	minInterval  time.Duration
	pingInterval time.Duration
	queueCap     int

	mu      sync.Mutex
	clients map[*client]struct{}
	closed  bool

	done      chan struct{}
	runOnce   sync.Once
	closeOnce sync.Once
}

// NewHub builds a hub over the manager's aggregate event stream. It performs no
// I/O and starts no goroutines; call Run.
func NewHub(mgr *rig.Manager, cfg config.WS, opts ...Option) *Hub {
	h := &Hub{
		mgr:          mgr,
		log:          slog.Default(),
		version:      defaultVersion,
		tickets:      newTicketStore(ticketTTL),
		minInterval:  orDefault(cfg.MinInterval.D(), config.DefaultWSMinInterval),
		pingInterval: orDefault(cfg.PingInterval.D(), config.DefaultWSPingInterval),
		queueCap:     cfg.SendQueue,
		clients:      map[*client]struct{}{},
		done:         make(chan struct{}),
	}
	if h.queueCap <= 0 {
		h.queueCap = config.DefaultWSSendQueue
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Run consumes the manager's event stream and fans it out. It blocks until ctx
// is cancelled, Close is called, or the manager shuts the stream down, so the
// server runs it on its own goroutine. Calling it twice is a no-op.
//
// This loop is the one place that must always be ready to receive: it holds the
// only subscription, and every step it takes is a non-blocking hand-off into a
// client's queue, so it can never be the reason a session stalls.
func (h *Hub) Run(ctx context.Context) {
	h.runOnce.Do(func() { h.run(ctx) })
}

func (h *Hub) run(ctx context.Context) {
	events, unsubscribe := h.mgr.Subscribe()
	defer unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.done:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			h.dispatch(ev)
		}
	}
}

// dispatch offers one event to every client. Each enqueue is non-blocking.
func (h *Hub) dispatch(ev rig.Event) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		c.enqueue(ev)
	}
}

// Close disconnects every client and stops Run.
func (h *Hub) Close() {
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		clients := make([]*client, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.mu.Unlock()

		close(h.done)
		for _, c := range clients {
			c.stop()
		}
	})
}

// Handler serves GET /ws.
//
// Two authentication paths reach it. A programmatic client sends Basic auth on
// the upgrade and arrives with a username already in the context, put there by
// the auth middleware. A browser cannot set headers on a WebSocket handshake at
// all, so it presents a single-use ticket from /ws-ticket instead; that path has
// to work whether or not the middleware let the request through, which is why
// the ticket is checked here rather than upstream.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := auth.UserFrom(r.Context())
		if !ok {
			user, ok = h.tickets.redeem(r.URL.Query().Get("ticket"))
		}
		if !ok {
			problem(w, http.StatusUnauthorized, "Unauthorized",
				"HTTP Basic credentials or a ticket from /ws-ticket are required")
			return
		}

		h.mu.Lock()
		closed := h.closed
		h.mu.Unlock()
		if closed {
			problem(w, http.StatusServiceUnavailable, "Service Unavailable",
				"the server is shutting down")
			return
		}

		radios := h.resolveRadios(r.URL.Query().Get("radios"))

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: h.origins,
		})
		if err != nil {
			// Accept has already written a response.
			h.log.Debug("websocket upgrade failed", "user", user, "err", err)
			return
		}

		h.serve(r.Context(), conn, user, radios)
	})
}

// serve runs one connection to completion on the HTTP handler's goroutine, so
// that net/http keeps ownership of the hijacked connection until we are done
// with it.
func (h *Hub) serve(ctx context.Context, conn *websocket.Conn, user string, radios []string) {
	// Detached from the request context: it is documented as unsafe to use
	// after a hijack, and the connection outlives the handler's notion of a
	// request anyway. Only the values are kept, for logging.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()

	c := newClient(h, conn, user, radios, cancel)

	if !h.add(c) {
		conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer h.remove(c)

	c.run(ctx)
}

func (h *Hub) add(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *Hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

// clientCount reports the number of connected clients. Used by tests.
func (h *Hub) clientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// resolveRadios turns the ?radios= filter into an ordered list of ids that
// actually exist, in configuration order.
//
// Unknown ids are dropped rather than rejected: hello reports the subscription
// the connection really got, so a client that asked for a radio this instance
// does not have finds out from the stream instead of from an error it would
// have to handle during a handshake it cannot see the body of.
func (h *Hub) resolveRadios(filter string) []string {
	sessions := h.mgr.List()

	var want map[string]struct{}
	if strings.TrimSpace(filter) != "" {
		want = map[string]struct{}{}
		for _, id := range strings.Split(filter, ",") {
			if id = strings.TrimSpace(id); id != "" {
				want[id] = struct{}{}
			}
		}
	}

	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if want != nil {
			if _, ok := want[s.ID()]; !ok {
				continue
			}
		}
		ids = append(ids, s.ID())
	}
	return ids
}

// state returns the current snapshot for a radio, straight from the session's
// atomic cache. It never blocks on a serial port, which is what makes a
// snapshot cheap enough to send on every connect and every subscribe.
func (h *Hub) state(id string) (radio.State, bool) {
	s, ok := h.mgr.Get(id)
	if !ok {
		return radio.State{}, false
	}
	return s.State(), true
}

// problem writes an RFC 9457 error, matching the format the REST API uses so a
// client sees one error shape everywhere.
func problem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
		"detail": detail,
	})
}
