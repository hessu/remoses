package auth

import (
	"context"
	"net/http"
	"strings"
)

// unauthorizedBody matches the RFC 9457 problem+json shape the rest of the API
// uses, so a client sees one error format everywhere.
const unauthorizedBody = `{"type":"about:blank","title":"Unauthorized","status":401,` +
	`"detail":"valid HTTP Basic credentials are required"}`

// Middleware wraps next with HTTP Basic authentication, storing the
// authenticated username in the request context.
//
// It is not applied to anything here; the server decides what it covers.
// /healthz is deliberately unauthenticated, and the WebSocket upgrade may
// authenticate by ticket instead (browsers cannot set headers on a WebSocket).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || !a.Verify(username, password) {
			a.Unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), username)))
	})
}

// Unauthorized writes a 401 carrying the Basic challenge. Exported so that
// handlers authenticating outside the middleware — the WebSocket ticket
// endpoint — reject with the same response.
func (a *Authenticator) Unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", a.challenge)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(unauthorizedBody))
}

// quoteRealm makes an operator-supplied realm safe inside the quoted-string of
// a WWW-Authenticate header.
func quoteRealm(realm string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\r", "",
		"\n", "",
	).Replace(realm)
}

type ctxKey struct{}

// WithUser returns ctx carrying an authenticated username.
func WithUser(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, ctxKey{}, username)
}

// UserFrom returns the username the request authenticated as.
func UserFrom(ctx context.Context) (string, bool) {
	username, ok := ctx.Value(ctxKey{}).(string)
	return username, ok
}
