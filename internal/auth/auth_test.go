package auth

import (
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/hessu/remoses/internal/config"
)

// Hashes at the configured default cost. Generating them once keeps the whole
// suite to a couple of KDF runs.
var (
	opHash    = mustHash("secret")
	guestHash = mustHash("guest-pw")
)

func mustHash(pw string) string {
	h, err := HashPassword(pw, config.DefaultBcryptCost)
	if err != nil {
		panic(err)
	}
	return h
}

func testAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := New(config.Auth{
		Realm:      "remoses",
		BcryptCost: config.DefaultBcryptCost,
		CacheTTL:   config.Duration(time.Minute),
		Users: []config.User{
			{Username: "op", PasswordBcrypt: opHash},
			{Username: "guest", PasswordBcrypt: guestHash},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestVerify(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		want     bool
	}{
		{"correct", "op", "secret", true},
		{"second user", "guest", "guest-pw", true},
		{"wrong password", "op", "Secret", false},
		{"other user's password", "op", "guest-pw", false},
		{"unknown user", "nobody", "secret", false},
		{"empty username", "", "secret", false},
		{"empty password", "op", "", false},
		{"both empty", "", "", false},
		{"username is a prefix", "o", "secret", false},
		{"username has a suffix", "op ", "secret", false},
	}

	a := testAuth(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.Verify(tt.username, tt.password); got != tt.want {
				t.Errorf("Verify(%q, %q) = %v, want %v", tt.username, tt.password, got, tt.want)
			}
		})
	}
}

// The property that makes response timing uninformative is that an unknown
// username still costs a bcrypt comparison.
func TestVerifyAlwaysRunsBcrypt(t *testing.T) {
	a := testAuth(t)
	before := a.bcryptCalls.Load()
	a.Verify("nobody-at-all", "whatever")
	if got := a.bcryptCalls.Load() - before; got != 1 {
		t.Errorf("unknown user ran %d bcrypt comparisons, want 1", got)
	}
}

// Belt and braces on top of the counter: an unknown user must not answer
// noticeably faster than a known one with the wrong password.
func TestVerifyUnknownUserIsNotFaster(t *testing.T) {
	a := testAuth(t)
	measure := func(username, password string) time.Duration {
		best := time.Duration(1<<63 - 1)
		for range 3 {
			start := time.Now()
			a.Verify(username, password)
			if d := time.Since(start); d < best {
				best = d
			}
		}
		return best
	}

	known := measure("op", "wrong-password")
	unknown := measure("nobody", "wrong-password")
	if unknown*2 < known {
		t.Errorf("unknown user took %v, known-user rejection took %v: "+
			"the gap is large enough to enumerate accounts", unknown, known)
	}
}

func TestVerifyCache(t *testing.T) {
	a := testAuth(t)

	now := time.Now()
	a.now = func() time.Time { return now }

	if !a.Verify("op", "secret") {
		t.Fatal("first Verify failed")
	}
	after := a.bcryptCalls.Load()

	for range 5 {
		if !a.Verify("op", "secret") {
			t.Fatal("cached Verify failed")
		}
	}
	if got := a.bcryptCalls.Load(); got != after {
		t.Errorf("cached verifications ran %d extra bcrypt comparisons, want 0", got-after)
	}

	// A wrong password must not ride in on the cached entry for the right one.
	if a.Verify("op", "wrong") {
		t.Error("wrong password accepted")
	}

	now = now.Add(time.Minute)
	if !a.Verify("op", "secret") {
		t.Fatal("Verify after expiry failed")
	}
	if a.bcryptCalls.Load() <= after+1 {
		t.Error("expired cache entry was reused")
	}
}

func TestVerifyFailureIsNotCached(t *testing.T) {
	a := testAuth(t)
	for range 3 {
		if a.Verify("op", "wrong") {
			t.Fatal("wrong password accepted")
		}
	}
	if got := a.bcryptCalls.Load(); got != 3 {
		t.Errorf("ran %d bcrypt comparisons for 3 failures, want 3", got)
	}
}

func TestVerifyWithCacheDisabled(t *testing.T) {
	a, err := New(config.Auth{
		BcryptCost: config.DefaultBcryptCost,
		CacheTTL:   0,
		Users:      []config.User{{Username: "op", PasswordBcrypt: opHash}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 3 {
		if !a.Verify("op", "secret") {
			t.Fatal("Verify failed")
		}
	}
	if got := a.bcryptCalls.Load(); got != 3 {
		t.Errorf("ran %d bcrypt comparisons with the cache off, want 3", got)
	}
	if n := len(a.cache); n != 0 {
		t.Errorf("cache holds %d entries with ttl 0", n)
	}
}

func TestCacheEviction(t *testing.T) {
	a := testAuth(t)
	now := time.Now()
	a.now = func() time.Time { return now }

	// Fill past the sweep threshold with entries that all expire together.
	a.mu.Lock()
	for i := range cacheSweepAt {
		a.cache[newCacheKey("filler", string(rune(i)))] = now.Add(time.Second)
	}
	a.mu.Unlock()

	now = now.Add(time.Minute)
	if !a.Verify("op", "secret") {
		t.Fatal("Verify failed")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if n := len(a.cache); n != 1 {
		t.Errorf("cache holds %d entries after a sweep, want 1", n)
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Auth
		wantErr string
	}{
		{
			name: "ok",
			cfg: config.Auth{
				BcryptCost: 8,
				Users:      []config.User{{Username: "op", PasswordBcrypt: opHash}},
			},
		},
		{
			name: "empty username",
			cfg: config.Auth{
				BcryptCost: 8,
				Users:      []config.User{{Username: "", PasswordBcrypt: opHash}},
			},
			wantErr: "empty username",
		},
		{
			name: "duplicate username",
			cfg: config.Auth{
				BcryptCost: 8,
				Users: []config.User{
					{Username: "op", PasswordBcrypt: opHash},
					{Username: "op", PasswordBcrypt: guestHash},
				},
			},
			wantErr: `duplicate username "op"`,
		},
		{
			name: "plaintext password in config",
			cfg: config.Auth{
				BcryptCost: 8,
				Users:      []config.User{{Username: "op", PasswordBcrypt: "hunter2"}},
			},
			wantErr: "not a bcrypt hash",
		},
		{
			name: "out-of-range cost still starts",
			cfg: config.Auth{
				BcryptCost: 0,
				Users:      []config.User{{Username: "op", PasswordBcrypt: opHash}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("New: %v", err)
			case tt.wantErr == "":
				if a.realm != config.DefaultRealm {
					t.Errorf("realm = %q, want %q", a.realm, config.DefaultRealm)
				}
			case err == nil:
				t.Fatalf("New succeeded, want error containing %q", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("New error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewWithNoUsersVerifiesNothing(t *testing.T) {
	a, err := New(config.Auth{BcryptCost: 8})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Verify("op", "secret") {
		t.Error("Verify succeeded with no users configured")
	}
}

func TestHashPassword(t *testing.T) {
	h, err := HashPassword("secret", 6)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cost, err := bcrypt.Cost([]byte(h))
	if err != nil {
		t.Fatalf("result is not a bcrypt hash: %v", err)
	}
	if cost != 6 {
		t.Errorf("cost = %d, want 6", cost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte("secret")); err != nil {
		t.Errorf("hash does not verify: %v", err)
	}

	if _, err := HashPassword("", 6); err == nil {
		t.Error("HashPassword accepted an empty password")
	}
	if _, err := HashPassword("secret", 99); err == nil {
		t.Error("HashPassword accepted cost 99")
	}
}

func TestCacheKeySeparatesFields(t *testing.T) {
	// Without the separator byte, ("ab", "c") and ("a", "bc") would collide and
	// one user's session would authenticate another.
	if newCacheKey("ab", "c") == newCacheKey("a", "bc") {
		t.Error("cache keys collide across the username/password boundary")
	}
}
