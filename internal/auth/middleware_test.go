package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
)

func echoUser(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := UserFrom(r.Context())
		if !ok {
			t.Error("handler reached with no username in context")
		}
		_, _ = w.Write([]byte(username))
	})
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name     string
		setAuth  bool
		username string
		password string
		wantCode int
		wantBody string
	}{
		{"valid", true, "op", "secret", http.StatusOK, "op"},
		{"wrong password", true, "op", "nope", http.StatusUnauthorized, ""},
		{"unknown user", true, "nobody", "secret", http.StatusUnauthorized, ""},
		{"no credentials", false, "", "", http.StatusUnauthorized, ""},
	}

	a := testAuth(t)
	h := a.Middleware(echoUser(t))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/radios", nil)
			if tt.setAuth {
				r.SetBasicAuth(tt.username, tt.password)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantCode)
			}
			if tt.wantCode == http.StatusOK {
				if got := w.Body.String(); got != tt.wantBody {
					t.Errorf("body = %q, want %q", got, tt.wantBody)
				}
				return
			}
			if got := w.Header().Get("WWW-Authenticate"); got != `Basic realm="remoses", charset="UTF-8"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q", got)
			}
		})
	}
}

func TestMiddlewareDoesNotLeakCredentialsIntoContext(t *testing.T) {
	a := testAuth(t)
	reached := false
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/radios", nil)
	r.SetBasicAuth("op", "wrong")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if reached {
		t.Error("next handler ran despite a failed verification")
	}
}

func TestUserFromWithoutMiddleware(t *testing.T) {
	if username, ok := UserFrom(t.Context()); ok {
		t.Errorf("UserFrom on a bare context returned %q", username)
	}
}

// A realm is operator-supplied, so it must not be able to break out of the
// header's quoted-string.
func TestRealmIsEscaped(t *testing.T) {
	a, err := New(config.Auth{
		Realm:      "he said \"hi\"\r\nX-Injected: 1",
		BcryptCost: config.DefaultBcryptCost,
		CacheTTL:   config.Duration(time.Minute),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	a.Unauthorized(w)

	got := w.Header().Get("WWW-Authenticate")
	want := `Basic realm="he said \"hi\"X-Injected: 1", charset="UTF-8"`
	if got != want {
		t.Errorf("WWW-Authenticate =\n%q\nwant\n%q", got, want)
	}
}
