// Package api is the HTTP surface of remoses: it maps api/openapi.yaml onto
// the session manager, the lock manager and the authenticator.
//
// The package holds no state of its own beyond that wiring, and that is
// deliberate. Reads are served from the session's state cache and writes are a
// single call on a Session, so nothing here can block on a serial port; the
// no-blocking property belongs to internal/rig and this layer is careful not
// to undo it by introducing locks or caches of its own.
//
// The route table in routes is the one place that knows which operations
// exist. It is spelled with the same paths as the OpenAPI document so a test
// can compare the two directly, which is what keeps handler and contract from
// drifting apart without a code generator in the build.
package api

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/lock"
	"github.com/hessu/remoses/internal/rig"
)

// Audited actions. They name the operation in the audit log and in problem
// documents, and are constants because the route table and the handler that
// reports the result must agree on the spelling.
const (
	actionPatchState  = "patch_state"
	actionSendCW      = "send_cw"
	actionAbortCW     = "abort_cw"
	actionAcquireLock = "acquire_lock"
	actionReleaseLock = "release_lock"
	actionRenewLock   = "renew_lock"
)

// server carries the dependencies every handler needs.
type server struct {
	cfg   *config.Config
	mgr   *rig.Manager
	locks *lock.Manager
	auth  *auth.Authenticator
	log   *slog.Logger
	now   func() time.Time

	ws       http.Handler
	wsTicket http.Handler
}

// Option configures the API server.
type Option func(*server)

// WithLogger sets the logger used for the audit trail and for internal errors.
func WithLogger(l *slog.Logger) Option {
	return func(s *server) {
		if l != nil {
			s.log = l
		}
	}
}

// WithNow overrides the clock. Only the snapshot age and staleness depend on
// it; tests use it to age a snapshot without sleeping.
func WithNow(f func() time.Time) Option {
	return func(s *server) {
		if f != nil {
			s.now = f
		}
	}
}

// New builds the HTTP handler for the whole REST API, mounted under
// cfg.Server.BasePath.
//
// wsHandler and wsTicketHandler are taken as plain http.Handler values rather
// than as an import of internal/ws, so the stream implementation and this
// package can change independently. Either may be nil, in which case the route
// stays registered and answers 503: a missing stream must not turn into a
// silent 404 that looks like a client using the wrong path.
//
// mgr, locks and a must all be non-nil.
func New(cfg *config.Config, mgr *rig.Manager, locks *lock.Manager, a *auth.Authenticator, wsHandler, wsTicketHandler http.Handler, opts ...Option) http.Handler {
	return newServer(cfg, mgr, locks, a, wsHandler, wsTicketHandler, opts...).handler()
}

func newServer(cfg *config.Config, mgr *rig.Manager, locks *lock.Manager, a *auth.Authenticator, wsHandler, wsTicketHandler http.Handler, opts ...Option) *server {
	if cfg == nil {
		cfg = &config.Config{}
	}
	s := &server{
		cfg:      cfg,
		mgr:      mgr,
		locks:    locks,
		auth:     a,
		log:      slog.Default(),
		now:      time.Now,
		ws:       wsHandler,
		wsTicket: wsTicketHandler,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// route is one operation from api/openapi.yaml.
type route struct {
	method string
	// path is relative to server.base_path and spelled exactly as in the
	// OpenAPI document, including the {radioId} placeholder, so that the
	// conformance test can compare the two sets without translating between
	// them.
	path    string
	handler http.HandlerFunc

	// public routes skip the authentication middleware. Only /healthz (a
	// liveness probe that must answer before credentials are configured) and
	// the WebSocket upgrade (which authenticates by ticket, because a browser
	// cannot set headers on a handshake) are public.
	public bool

	// lockAction, when set, puts the route behind the lock guard and names the
	// action in the audit log.
	lockAction string

	// conflictStatus overrides the status for lock.ErrHeldByOther on this
	// route. Zero means the normal 409.
	conflictStatus int
}

// routes is the whole contract, in one place.
func (s *server) routes() []route {
	return []route{
		{method: http.MethodGet, path: "/radios", handler: s.listRadios},
		{method: http.MethodGet, path: "/radios/{radioId}", handler: s.getRadio},

		{method: http.MethodGet, path: "/radios/{radioId}/state", handler: s.getState},
		{method: http.MethodPatch, path: "/radios/{radioId}/state", handler: s.patchState, lockAction: actionPatchState},

		{method: http.MethodGet, path: "/radios/{radioId}/cw", handler: s.getCW},
		{method: http.MethodPost, path: "/radios/{radioId}/cw", handler: s.sendCW, lockAction: actionSendCW},
		{method: http.MethodDelete, path: "/radios/{radioId}/cw", handler: s.abortCW, lockAction: actionAbortCW},

		{method: http.MethodGet, path: "/radios/{radioId}/lock", handler: s.getLock},
		{method: http.MethodPost, path: "/radios/{radioId}/lock", handler: s.acquireLock},
		// The spec documents 423 and no 409 for a release, so a token that
		// belongs to somebody else is reported the same way as one that has
		// expired: from this client's point of view the token is simply not
		// valid, and it has nothing to retry.
		{method: http.MethodDelete, path: "/radios/{radioId}/lock", handler: s.releaseLock,
			lockAction: actionReleaseLock, conflictStatus: http.StatusLocked},
		{method: http.MethodPost, path: "/radios/{radioId}/lock/renew", handler: s.renewLock, lockAction: actionRenewLock},

		{method: http.MethodPost, path: "/ws-ticket", handler: s.wsTicketUpgrade},
		{method: http.MethodGet, path: "/ws", handler: s.wsUpgrade, public: true},

		{method: http.MethodGet, path: "/healthz", handler: s.health, public: true},
	}
}

// handler registers every route on a ServeMux, wrapping each one in the
// middleware it asks for. Order matters: authentication runs outermost, so an
// anonymous request is rejected before it can learn whether a radio exists or
// who holds its lock.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	base := normalizeBase(s.cfg.Server.BasePath)

	for _, rt := range s.routes() {
		h := http.Handler(rt.handler)
		if rt.lockAction != "" {
			h = s.requireLock(rt.lockAction, rt.conflictStatus, h)
		}
		if !rt.public {
			h = s.auth.Middleware(h)
		}
		mux.Handle(rt.method+" "+base+rt.path, h)
	}

	// A catch-all so that a mistyped path gets the same problem+json shape as
	// everything else rather than net/http's plain-text 404. It is
	// unauthenticated on purpose: demanding credentials to be told a route does
	// not exist helps nobody.
	mux.Handle("/", http.HandlerFunc(s.notFound))

	return mux
}

// normalizeBase makes an operator-supplied base path safe to concatenate into
// a ServeMux pattern. Without the leading slash the mux would read the first
// segment as a host name and silently match nothing.
func normalizeBase(base string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "/") {
		base = "/" + base
	}
	return base
}

// health is the unauthenticated liveness probe. It reports how many radios are
// connected but never fails on their account: the daemon is alive even when
// every rig is unplugged, and a probe that flapped with the USB cable would
// restart a perfectly healthy process.
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	sessions := s.mgr.List()
	connected := 0
	for _, sess := range sessions {
		if sess.Connected() {
			connected++
		}
	}
	s.writeJSON(w, http.StatusOK, healthDTO{
		Status:          "ok",
		RadiosConnected: connected,
		RadiosTotal:     len(sessions),
	})
}

type healthDTO struct {
	Status          string `json:"status"`
	RadiosConnected int    `json:"radios_connected"`
	RadiosTotal     int    `json:"radios_total"`
}

// wsUpgrade hands the connection to the stream implementation. The handler
// authenticates itself: a browser cannot put Basic credentials on a WebSocket
// handshake, so this route accepts a ticket instead and cannot sit behind the
// normal middleware.
//
// A programmatic client does send the header, though, and both the OpenAPI
// document and DESIGN.md §9 say it is the path for one. The middleware being
// skipped is what makes the check belong here: credentials that are present are
// verified, and a request carrying none still falls through to the ticket, so
// the browser path is untouched. Verifying rather than passing through also
// means a wrong password is answered with a 401 and a Basic challenge instead
// of the stream handler's "or a ticket" message, which is not advice a
// programmatic client can act on.
func (s *server) wsUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.ws == nil {
		problem(w, http.StatusServiceUnavailable, "Service Unavailable",
			"the WebSocket stream is not enabled on this instance",
			kv("instance", r.URL.Path))
		return
	}
	if user, pass, ok := r.BasicAuth(); ok && s.auth != nil {
		if !s.auth.Verify(user, pass) {
			s.auth.Unauthorized(w)
			return
		}
		r = r.WithContext(auth.WithUser(r.Context(), user))
	}
	s.ws.ServeHTTP(w, r)
}

// wsTicketUpgrade mints a ticket for browser clients. Unlike /ws it runs
// behind the Basic auth middleware, so the handler can read the authenticated
// username from the request context.
func (s *server) wsTicketUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.wsTicket == nil {
		problem(w, http.StatusServiceUnavailable, "Service Unavailable",
			"the WebSocket stream is not enabled on this instance",
			kv("instance", r.URL.Path))
		return
	}
	s.wsTicket.ServeHTTP(w, r)
}

func (s *server) notFound(w http.ResponseWriter, r *http.Request) {
	problem(w, http.StatusNotFound, "Not Found",
		"no such endpoint", kv("instance", r.URL.Path))
}

// session resolves the {radioId} path value, answering 404 when there is no
// such radio. The bool result says whether the caller should carry on.
func (s *server) session(w http.ResponseWriter, r *http.Request, action string) (*rig.Session, string, bool) {
	id := r.PathValue("radioId")
	sess, ok := s.mgr.Get(id)
	if !ok {
		s.fail(w, r, id, action, errNoSuchRadio)
		return nil, id, false
	}
	return sess, id, true
}
