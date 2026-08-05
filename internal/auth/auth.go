// Package auth implements HTTP Basic authentication against the bcrypt hashes
// held in the configuration file.
//
// Authorisation is per instance: any authenticated user may use any radio.
// There are no scopes and no read-only role, because this is one small station
// with a handful of trusted operators rather than a multi-tenant service.
//
// Two properties shape the implementation. Basic auth means every request —
// including a client polling state at 2 Hz — carries a password that must be
// verified, so a naive bcrypt-per-request would spend most of the CPU on the
// KDF; hence the TTL verify-cache. And a rejection must not reveal whether the
// username exists, so the unknown-user path does exactly the same work as the
// wrong-password path.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/hessu/remoses/internal/config"
)

// dummyCost is used for the unknown-user hash when the configured cost is out
// of range. The dummy exists only to burn a comparable amount of CPU, so any
// sane cost will do rather than refusing to start.
const dummyCost = 8

// cacheSweepAt is the size at which an insert also walks the cache dropping
// expired entries. Nothing sweeps on a timer, so eviction rides along with the
// writes that grow the map.
const cacheSweepAt = 64

// Authenticator verifies credentials for one remoses instance.
//
// The zero value is not usable; call New.
type Authenticator struct {
	realm     string
	challenge string
	users     []user
	// dummy is a hash of a random secret, compared against when the username is
	// unknown so that the response time does not enumerate accounts.
	dummy []byte
	ttl   time.Duration

	mu    sync.Mutex
	cache map[cacheKey]time.Time

	// now is swapped by tests to drive cache expiry without sleeping.
	now func() time.Time
	// bcryptCalls counts KDF comparisons. Tests assert that the unknown-user
	// path runs one, which is the property that makes timing uninformative.
	bcryptCalls atomic.Int64
}

type user struct {
	name []byte
	hash []byte
}

// cacheKey is sha256(username || 0x00 || password). The separator stops
// ("ab", "c") and ("a", "bc") sharing an entry.
type cacheKey [sha256.Size]byte

// New builds an Authenticator from a validated config.Auth.
func New(cfg config.Auth) (*Authenticator, error) {
	a := &Authenticator{
		realm: cfg.Realm,
		ttl:   cfg.CacheTTL.D(),
		cache: make(map[cacheKey]time.Time),
		now:   time.Now,
	}
	if a.realm == "" {
		a.realm = config.DefaultRealm
	}
	a.challenge = fmt.Sprintf(`Basic realm="%s", charset="UTF-8"`, quoteRealm(a.realm))

	seen := make(map[string]bool, len(cfg.Users))
	for _, u := range cfg.Users {
		if u.Username == "" {
			return nil, fmt.Errorf("auth: user with an empty username")
		}
		if seen[u.Username] {
			return nil, fmt.Errorf("auth: duplicate username %q", u.Username)
		}
		seen[u.Username] = true
		if _, err := bcrypt.Cost([]byte(u.PasswordBcrypt)); err != nil {
			return nil, fmt.Errorf("auth: user %q: password_bcrypt is not a bcrypt hash: %w",
				u.Username, err)
		}
		a.users = append(a.users, user{
			name: []byte(u.Username),
			hash: []byte(u.PasswordBcrypt),
		})
	}

	dummy, err := newDummyHash(cfg.BcryptCost)
	if err != nil {
		return nil, err
	}
	a.dummy = dummy

	return a, nil
}

// Verify reports whether the credentials are valid.
//
// It is safe for concurrent use, and takes the same time for an unknown user as
// for a known user with the wrong password.
func (a *Authenticator) Verify(username, password string) bool {
	key := newCacheKey(username, password)
	if a.cacheHit(key) {
		return true
	}

	// Scan every user rather than looking one up: a map probe branches on the
	// name, and the whole point here is not to leak which names exist.
	hash := a.dummy
	match := 0
	name := []byte(username)
	for i := range a.users {
		if subtle.ConstantTimeCompare(a.users[i].name, name) == 1 {
			hash = a.users[i].hash
			match = 1
		}
	}

	a.bcryptCalls.Add(1)
	err := bcrypt.CompareHashAndPassword(hash, []byte(password))
	ok := match == 1 && err == nil

	if ok {
		a.cacheStore(key)
	}
	return ok
}

func (a *Authenticator) cacheHit(key cacheKey) bool {
	if a.ttl <= 0 {
		return false
	}
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	expires, ok := a.cache[key]
	if !ok {
		return false
	}
	if !now.Before(expires) {
		delete(a.cache, key)
		return false
	}
	return true
}

func (a *Authenticator) cacheStore(key cacheKey) {
	if a.ttl <= 0 {
		return
	}
	now := a.now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if len(a.cache) >= cacheSweepAt {
		for k, expires := range a.cache {
			if !now.Before(expires) {
				delete(a.cache, k)
			}
		}
	}
	a.cache[key] = now.Add(a.ttl)
}

func newCacheKey(username, password string) cacheKey {
	h := sha256.New()
	h.Write([]byte(username))
	h.Write([]byte{0})
	h.Write([]byte(password))

	var k cacheKey
	h.Sum(k[:0])
	return k
}

func newDummyHash(cost int) ([]byte, error) {
	if cost < bcrypt.MinCost || cost > bcrypt.MaxCost {
		cost = dummyCost
	}
	var secret [16]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, fmt.Errorf("auth: generating dummy secret: %w", err)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret[:])), cost)
	if err != nil {
		return nil, fmt.Errorf("auth: generating dummy hash: %w", err)
	}
	return h, nil
}

// HashPassword produces a bcrypt hash for the config file. It backs the
// "remoses passwd" subcommand.
func HashPassword(password string, cost int) (string, error) {
	if password == "" {
		return "", fmt.Errorf("auth: refusing to hash an empty password")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), cost)
	if err != nil {
		return "", fmt.Errorf("auth: hashing password: %w", err)
	}
	return string(h), nil
}
