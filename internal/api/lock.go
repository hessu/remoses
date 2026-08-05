package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/lock"
)

// lockHeader carries the token on every state-changing request.
const lockHeader = "X-Remoses-Lock"

// lockCookiePrefix names the per-radio fallback cookie. It is per radio
// because locks are per radio: one browser may hold the IC-7610 and not the
// TS-590SG, and a single cookie could not express that.
const lockCookiePrefix = "remoses_lock_"

// lockToken reads the lock token for one radio.
//
// The header wins. The cookie exists only because a browser cannot easily put
// a header on every request it makes, and a stale cookie left over from an
// earlier session must never override what the client explicitly sent.
func lockToken(r *http.Request, radioID string) string {
	if v := r.Header.Get(lockHeader); v != "" {
		return v
	}
	if radioID == "" {
		return ""
	}
	if c, err := r.Cookie(lockCookiePrefix + radioID); err == nil {
		return c.Value
	}
	return ""
}

// requireLock gates a state-changing route.
//
// Check does the gating and, on success, slides the lease: that is how "any
// accepted command keeps the lock alive" works, so an operator who is actively
// tuning never has to send a heartbeat. When locking is disabled in the
// configuration every request passes straight through.
//
// The radio is resolved first, so a request against a radio that does not
// exist is a 404 rather than a confusing 423 about a lock that could never be
// held.
func (s *server) requireLock(action string, conflictStatus int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("radioId")
		if _, ok := s.mgr.Get(id); !ok {
			s.fail(w, r, id, action, errNoSuchRadio)
			return
		}
		if s.locks.Enabled() {
			if err := s.locks.Check(id, lockToken(r, id)); err != nil {
				s.failWith(w, r, id, action, err, conflictStatus)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type lockStateDTO struct {
	Held      bool   `json:"held"`
	Holder    string `json:"holder,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	IsMine    bool   `json:"is_mine"`
}

type lockDTO struct {
	Token      string `json:"token"`
	ExpiresAt  string `json:"expires_at"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type lockRequestBody struct {
	Force bool `json:"force"`
}

// lockState describes who holds a radio.
//
// is_mine is answered with IsHolder rather than Check, because Check renews:
// listing the radios must not slide somebody's lease as a side effect of a
// client redrawing its screen.
func (s *server) lockState(radioID, token string) lockStateDTO {
	l, ok := s.locks.Inspect(radioID)
	if !ok {
		return lockStateDTO{}
	}
	return lockStateDTO{
		Held:      true,
		Holder:    l.User,
		ExpiresAt: rfc3339(l.ExpiresAt),
		IsMine:    s.locks.IsHolder(radioID, token),
	}
}

func (s *server) lockResponse(l lock.Lock) lockDTO {
	return lockDTO{
		Token:      l.Token,
		ExpiresAt:  rfc3339(l.ExpiresAt),
		TTLSeconds: int(s.locks.TTL().Seconds()),
	}
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *server) getLock(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.session(w, r, "")
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.lockState(id, lockToken(r, id)))
}

func (s *server) acquireLock(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.session(w, r, actionAcquireLock)
	if !ok {
		return
	}

	var body lockRequestBody
	if err := decodeJSON(w, r, &body, false); err != nil {
		s.fail(w, r, id, actionAcquireLock, err)
		return
	}
	user, _ := auth.UserFrom(r.Context())

	l, err := s.locks.Acquire(id, user, body.Force)
	if err != nil {
		// A refused steal and a plain conflict come back as the same sentinel,
		// so the configuration is what separates them: force was asked for and
		// the operator has disabled stealing, which is a 403 the client cannot
		// retry its way out of.
		if body.Force && !s.cfg.Lock.AllowSteal && errors.Is(err, lock.ErrHeldByOther) {
			holder, expires := s.holder(id)
			s.audit(r, actionAcquireLock, id, http.StatusForbidden, err, "force", true)
			problem(w, http.StatusForbidden, "Forbidden",
				"stealing a held lock is disabled by configuration",
				kv("radio_id", id), kv("locked_by", holder), kv("expires_at", expires),
				kv("instance", r.URL.Path))
			return
		}
		s.fail(w, r, id, actionAcquireLock, err)
		return
	}

	s.audit(r, actionAcquireLock, id, http.StatusCreated, nil, "force", body.Force)
	s.writeJSON(w, http.StatusCreated, s.lockResponse(l))
}

// releaseLock drops the lock and forces the radio back to receive.
//
// The force-to-receive is not optional and not asynchronous: DESIGN.md §7 makes
// release a safety event exactly like expiry, so a client that releases while
// transmitting must not be able to walk away leaving a carrier up. The 204
// therefore means "released, and the transmitter is down".
func (s *server) releaseLock(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, actionReleaseLock)
	if !ok {
		return
	}

	if s.locks.Enabled() {
		if err := s.locks.Release(id, lockToken(r, id)); err != nil {
			s.failWith(w, r, id, actionReleaseLock, err, http.StatusLocked)
			return
		}
		// Only on a real release. With locking disabled there is no lease to
		// give up, and knocking a sharing station off the air on a request that
		// released nothing would be a surprise, not a safety measure.
		sess.ForceRX("lock released by " + userOf(r))
	}

	s.audit(r, actionReleaseLock, id, http.StatusNoContent, nil)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) renewLock(w http.ResponseWriter, r *http.Request) {
	_, id, ok := s.session(w, r, actionRenewLock)
	if !ok {
		return
	}

	if !s.locks.Enabled() {
		// Nothing to renew, but a client written for a locking instance should
		// not have to special-case this one; hand it a lease it can heartbeat
		// against harmlessly.
		s.audit(r, actionRenewLock, id, http.StatusOK, nil)
		s.writeJSON(w, http.StatusOK, lockDTO{
			ExpiresAt:  rfc3339(s.now().Add(s.locks.TTL())),
			TTLSeconds: int(s.locks.TTL().Seconds()),
		})
		return
	}

	l, err := s.locks.Renew(id, lockToken(r, id))
	if err != nil {
		s.fail(w, r, id, actionRenewLock, err)
		return
	}
	s.audit(r, actionRenewLock, id, http.StatusOK, nil)
	s.writeJSON(w, http.StatusOK, s.lockResponse(l))
}

func userOf(r *http.Request) string {
	if u, ok := auth.UserFrom(r.Context()); ok {
		return u
	}
	return "unknown"
}
