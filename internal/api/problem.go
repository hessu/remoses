package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hessu/remoses/internal/auth"
	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/lock"
	"github.com/hessu/remoses/internal/rig"
)

// Errors this package raises itself. They exist so that every failure — ours
// and the lower layers' — reaches one classifier, and so that nothing is
// mapped onto a status code by matching on the text of an error.
var (
	// errNoSuchRadio is an unknown {radioId}.
	errNoSuchRadio = errors.New("no such radio")
	// errUnprocessable is a request the client could rewrite: a malformed
	// body, or one that breaks a rule of the schema.
	errUnprocessable = errors.New("unprocessable request")
)

// extra is one RFC 9457 extension member.
type extra struct {
	key   string
	value any
}

func kv(key string, value any) extra { return extra{key: key, value: value} }

// problem writes an RFC 9457 problem document.
//
// Every error response in this package goes through here, so that the media
// type, the required members and the extension members have exactly one
// implementation. Note that no internal error text reaches this function for a
// 5xx: the caller substitutes a generic detail and logs the real one.
func problem(w http.ResponseWriter, status int, title, detail string, extras ...extra) {
	doc := map[string]any{
		// about:blank is the RFC 9457 default: the status code alone carries
		// the semantics. remoses publishes no problem-type URIs, and inventing
		// unresolvable ones would be worse than saying nothing.
		"type":   "about:blank",
		"title":  title,
		"status": status,
	}
	if detail != "" {
		doc["detail"] = detail
	}
	for _, e := range extras {
		if e.key == "" || e.value == nil {
			continue
		}
		if s, ok := e.value.(string); ok && s == "" {
			continue
		}
		doc[e.key] = e.value
	}

	body, err := json.Marshal(doc)
	if err != nil {
		// Only reachable if an extension member is unmarshalable, which is a
		// programming error; answer with something rather than nothing.
		body = []byte(`{"type":"about:blank","title":"Internal Server Error","status":500}`)
		status = http.StatusInternalServerError
	}

	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// fail classifies err, writes the problem document and records the audit line.
func (s *server) fail(w http.ResponseWriter, r *http.Request, radioID, action string, err error) {
	s.failWith(w, r, radioID, action, err, 0)
}

// failWith is fail with a status override for lock.ErrHeldByOther, which one
// route reports differently; see the route table.
func (s *server) failWith(w http.ResponseWriter, r *http.Request, radioID, action string, err error, conflictStatus int) {
	status, title, detail, extras := s.classify(radioID, err)
	if conflictStatus != 0 && status == http.StatusConflict {
		status, title = conflictStatus, http.StatusText(conflictStatus)
	}
	if radioID != "" {
		extras = append(extras, kv("radio_id", radioID))
	}
	extras = append(extras, kv("instance", r.URL.Path))

	switch {
	case action != "":
		// A denied state change is still a state-changing request, and §12
		// wants it in the audit trail.
		s.audit(r, action, radioID, status, err)
	case status >= http.StatusInternalServerError:
		s.log.Error("request failed", "radio", radioID, "status", status, "err", err)
	}

	problem(w, status, title, detail, extras...)
}

// classify maps an error onto a status code, a title and any extension members
// that go with it.
//
// The mapping is part of the contract rather than a convenience: a remote
// operator has to be able to tell "someone else has the radio" from "your lock
// is gone" from "go and check a cable", and the sentinels the lower layers
// export are what make that possible without parsing error strings.
func (s *server) classify(radioID string, err error) (status int, title, detail string, extras []extra) {
	var charErr *cw.CharError

	switch {
	case errors.Is(err, errNoSuchRadio):
		return http.StatusNotFound, "Not Found", "no radio with that id is configured", nil

	case errors.Is(err, lock.ErrHeldByOther):
		holder, expires := s.holder(radioID)
		return http.StatusConflict, "Conflict", err.Error(),
			[]extra{kv("locked_by", holder), kv("expires_at", expires)}

	case errors.Is(err, lock.ErrNoLock), errors.Is(err, lock.ErrBadToken), errors.Is(err, lock.ErrExpired):
		return http.StatusLocked, "Locked", err.Error(), nil

	case errors.As(err, &charErr):
		// The client needs all three to fix the text: which character, where in
		// the string, and what this radio will actually key.
		return http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error(), []extra{
			kv("character", string(charErr.Char)),
			kv("offset", charErr.Offset),
			kv("charset", charErr.Charset),
		}

	case errors.Is(err, rig.ErrOutOfBand), errors.Is(err, rig.ErrUnsupported),
		errors.Is(err, cw.ErrNotSupported), errors.Is(err, errUnprocessable),
		// A NAK means the request was well formed but the radio refused it —
		// an out-of-range value for this model, or a command illegal in the
		// current mode. That is the client's problem to fix, not a daemon bug,
		// so it belongs with the other 422s rather than in the 500 catch-all.
		errors.Is(err, rig.ErrNAK):
		return http.StatusUnprocessableEntity, "Unprocessable Entity", err.Error(), nil

	case errors.Is(err, rig.ErrDisconnected):
		return http.StatusServiceUnavailable, "Service Unavailable",
			"the radio is not currently connected", nil

	case errors.Is(err, rig.ErrTimeout):
		return http.StatusGatewayTimeout, "Gateway Timeout",
			"the radio did not answer within the command timeout", nil
	}

	// Anything unrecognised is a bug here or in a backend. The real error is
	// logged by the caller; the client gets nothing that could describe the
	// internals of the daemon.
	return http.StatusInternalServerError, "Internal Server Error",
		"the request could not be completed", nil
}

// holder reports who owns a radio's lock, for the 409 body. It uses Inspect
// rather than Check so that reporting a conflict does not slide the lease of
// the operator who is actually using the radio.
func (s *server) holder(radioID string) (user string, expires any) {
	if radioID == "" || s.locks == nil {
		return "", nil
	}
	l, ok := s.locks.Inspect(radioID)
	if !ok {
		return "", nil
	}
	return l.User, l.ExpiresAt.UTC().Format(time.RFC3339)
}

// audit writes the one structured line per state-changing request that
// DESIGN.md §12 requires.
//
// This API keys a transmitter over a network, so "who transmitted on which
// radio, when, and did the rig accept it" has to be answerable from the log
// alone — including for the requests that were refused, since a stream of
// denied PTT attempts is exactly what an operator would want to see.
func (s *server) audit(r *http.Request, action, radioID string, status int, err error, attrs ...any) {
	user, _ := auth.UserFrom(r.Context())

	result := "ok"
	level := slog.LevelInfo
	switch {
	case status >= http.StatusInternalServerError:
		result, level = "error", slog.LevelError
	case status >= http.StatusBadRequest:
		result, level = "denied", slog.LevelWarn
	}

	args := make([]any, 0, 14+len(attrs))
	args = append(args,
		"user", user,
		"radio", radioID,
		"action", action,
		"result", result,
		"status", status,
	)
	if id := tokenID(lockToken(r, radioID)); id != "" {
		args = append(args, "lock", id)
	}
	args = append(args, attrs...)
	if err != nil {
		args = append(args, "err", err.Error())
	}
	s.log.Log(r.Context(), level, "audit", args...)
}

// tokenID is a short fingerprint of a lock token, so the audit trail can tell
// two sessions of the same operator apart. The token itself never reaches the
// log: it is a bearer credential, and a log file is a poor place to keep one.
func tokenID(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:4])
}
