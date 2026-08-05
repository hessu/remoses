// Package lock implements the per-radio advisory locks that gate every
// state-changing API call.
//
// Exclusive control is per radio, not per instance: one operator can work the
// IC-7610 while another works the TS-590SG. The locks are advisory in the sense
// that they gate the API rather than the serial port.
//
// Two properties are load-bearing:
//
//   - The TTL slides. Any accepted command renews the lease, so a client that
//     is actively operating never has to think about the lock; only an idle one
//     loses it.
//   - Expiry is proactive, driven by a timer per lock rather than discovered on
//     the next request. That is the whole point. A client that crashes
//     mid-transmission produces no next request, so a lazily expired lock would
//     never fire the callback that drops PTT and the radio would keep
//     transmitting until someone walked into the shack.
package lock

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hessu/remoses/internal/config"
)

// Errors are distinct sentinels because the API maps them to different status
// codes, and a remote operator needs to know which of "someone else has it" and
// "your lock is gone" applies.
var (
	// ErrHeldByOther means another user holds the lock. Maps to 409.
	ErrHeldByOther = errors.New("lock: held by another user")
	// ErrNoLock means no lock exists for this radio. Maps to 423.
	ErrNoLock = errors.New("lock: no lock held")
	// ErrBadToken means the presented token is not the holder's. Maps to 423.
	ErrBadToken = errors.New("lock: invalid lock token")
	// ErrExpired means the lock's lease ran out. Maps to 423.
	ErrExpired = errors.New("lock: lock expired")
)

// defaultTTL applies when the configuration leaves lock.ttl at zero.
const defaultTTL = 30 * time.Second

// Lock is one held lease.
type Lock struct {
	RadioID string `json:"radio_id"`
	// Token is opaque and secret. It identifies the holder, so it must not be
	// returned to anyone but the client that acquired it: use IsHolder to
	// answer "is this mine" instead of publishing this field.
	Token     string    `json:"-"`
	User      string    `json:"user"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Held reports whether the lease is still in the future.
func (l Lock) Held(now time.Time) bool { return now.Before(l.ExpiresAt) }

type entry struct {
	lock Lock
	// gen distinguishes one lease from the next. A timer that fires while a
	// renewal is waiting on the mutex would otherwise expire the lease that
	// just replaced it — dropping PTT under an operator who is actively
	// transmitting, which is the exact opposite of what this is for.
	gen   uint64
	timer *time.Timer
}

// Manager holds the locks for every radio.
type Manager struct {
	enabled    bool
	allowSteal bool
	ttl        time.Duration
	log        *slog.Logger

	mu    sync.Mutex
	locks map[string]*entry
	gen   uint64

	expireMu sync.RWMutex
	onExpire func(radioID string)
}

// NewManager builds a lock manager from the lock section of the configuration.
func NewManager(cfg config.Lock, opts ...Option) *Manager {
	m := &Manager{
		enabled:    cfg.Enabled,
		allowSteal: cfg.AllowSteal,
		ttl:        cfg.TTL.D(),
		log:        slog.Default(),
		locks:      map[string]*entry{},
	}
	if m.ttl <= 0 {
		m.ttl = defaultTTL
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(m *Manager) {
		if l != nil {
			m.log = l
		}
	}
}

// SetOnExpire installs the callback fired when a lock expires by itself. main
// wires it to Session.ForceRX, so a lease that runs out mid-transmission drops
// PTT and flushes the CW queue.
//
// The callback runs on its own goroutine, so it may block on the radio.
func (m *Manager) SetOnExpire(f func(radioID string)) {
	m.expireMu.Lock()
	m.onExpire = f
	m.expireMu.Unlock()
}

// Enabled reports whether locking gates commands at all.
func (m *Manager) Enabled() bool { return m.enabled }

// TTL is the configured lease duration.
func (m *Manager) TTL() time.Duration { return m.ttl }

// Acquire takes the lock for user.
//
// A user who already holds the lock may acquire it again; that issues a fresh
// token and invalidates the old one, which is what a client reconnecting after
// losing its token needs. Anyone else gets ErrHeldByOther unless force is set
// and lock.allow_steal permits stealing.
func (m *Manager) Acquire(radioID, user string, force bool) (Lock, error) {
	tok, err := newToken()
	if err != nil {
		return Lock{}, err
	}

	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	if cur, ok := m.locks[radioID]; ok && cur.lock.Held(now) && cur.lock.User != user {
		if !force {
			return cur.lock.public(), fmt.Errorf("%w: %s holds %s until %s",
				ErrHeldByOther, cur.lock.User, radioID, cur.lock.ExpiresAt.Format(time.RFC3339))
		}
		if !m.allowSteal {
			return cur.lock.public(), fmt.Errorf("%w: stealing is disabled by configuration (holder %s)",
				ErrHeldByOther, cur.lock.User)
		}
		m.log.Warn("lock stolen", "radio", radioID, "from", cur.lock.User, "by", user)
	}

	l := Lock{RadioID: radioID, Token: tok, User: user, ExpiresAt: now.Add(m.ttl)}
	m.installLocked(l)
	m.log.Info("lock acquired", "radio", radioID, "user", user, "expires_at", l.ExpiresAt)
	return l, nil
}

// Renew is the heartbeat for a client holding a radio while its operator
// thinks, without issuing a command.
func (m *Manager) Renew(radioID, token string) (Lock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.renewLocked(radioID, token)
}

// Release drops the lock immediately.
//
// The caller is expected to treat this as a safety event exactly like expiry:
// a radio released mid-transmission must still be forced back to receive.
func (m *Manager) Release(radioID, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.locks[radioID]
	if !ok {
		return fmt.Errorf("%w for radio %s", ErrNoLock, radioID)
	}
	if !tokenEqual(e.lock.Token, token) {
		return fmt.Errorf("%w for radio %s", ErrBadToken, radioID)
	}
	m.removeLocked(radioID)
	m.log.Info("lock released", "radio", radioID, "user", e.lock.User)
	return nil
}

// Check gates a state-changing call, and renews the lease when it passes. That
// sliding renewal is how "any accepted command keeps the lock alive" works: an
// operator who is actively tuning never has to send a heartbeat.
//
// When locking is disabled in the configuration, every check succeeds.
func (m *Manager) Check(radioID, token string) error {
	if !m.enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	e, ok := m.locks[radioID]
	if !ok {
		return fmt.Errorf("%w for radio %s", ErrNoLock, radioID)
	}
	if !e.lock.Held(time.Now()) {
		// The timer has not run yet. Treat it as gone rather than honouring a
		// lease that has already lapsed.
		m.removeLocked(radioID)
		return fmt.Errorf("%w for radio %s", ErrExpired, radioID)
	}
	if token == "" {
		return fmt.Errorf("%w for radio %s (held by %s)", ErrNoLock, radioID, e.lock.User)
	}
	if !tokenEqual(e.lock.Token, token) {
		return fmt.Errorf("%w: %s holds %s", ErrHeldByOther, e.lock.User, radioID)
	}
	_, err := m.renewLocked(radioID, token)
	return err
}

// IsHolder reports whether token is the current holder's, without renewing the
// lease. It exists so the API can answer "is_mine" on a GET without a read
// having the side effect of extending someone's lock.
func (m *Manager) IsHolder(radioID, token string) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.locks[radioID]
	if !ok || !e.lock.Held(time.Now()) {
		return false
	}
	return tokenEqual(e.lock.Token, token)
}

// Inspect returns the current lock for a radio, with the token redacted. The
// second result is false when no lock is held.
func (m *Manager) Inspect(radioID string) (Lock, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.locks[radioID]
	if !ok {
		return Lock{}, false
	}
	if !e.lock.Held(time.Now()) {
		m.removeLocked(radioID)
		return Lock{}, false
	}
	return e.lock.public(), true
}

// Close stops every expiry timer. Callbacks already in flight are not waited
// for; they are running on the radio, which has its own shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.locks {
		e.timer.Stop()
		delete(m.locks, id)
	}
}

// renewLocked extends the lease. Callers hold m.mu.
func (m *Manager) renewLocked(radioID, token string) (Lock, error) {
	e, ok := m.locks[radioID]
	if !ok {
		return Lock{}, fmt.Errorf("%w for radio %s", ErrNoLock, radioID)
	}
	if !e.lock.Held(time.Now()) {
		m.removeLocked(radioID)
		return Lock{}, fmt.Errorf("%w for radio %s", ErrExpired, radioID)
	}
	if !tokenEqual(e.lock.Token, token) {
		return Lock{}, fmt.Errorf("%w for radio %s", ErrBadToken, radioID)
	}
	l := e.lock
	l.ExpiresAt = time.Now().Add(m.ttl)
	m.installLocked(l)
	return l, nil
}

// installLocked stores a lock and arms its expiry timer. Callers hold m.mu.
func (m *Manager) installLocked(l Lock) {
	if old, ok := m.locks[l.RadioID]; ok {
		old.timer.Stop()
	}
	m.gen++
	gen := m.gen
	e := &entry{lock: l, gen: gen}
	e.timer = time.AfterFunc(time.Until(l.ExpiresAt), func() { m.expire(l.RadioID, gen) })
	m.locks[l.RadioID] = e
}

// removeLocked deletes a lock without firing the expiry callback. Callers hold
// m.mu.
func (m *Manager) removeLocked(radioID string) {
	if e, ok := m.locks[radioID]; ok {
		e.timer.Stop()
		delete(m.locks, radioID)
	}
}

// expire runs from the lease timer.
func (m *Manager) expire(radioID string, gen uint64) {
	m.mu.Lock()
	e, ok := m.locks[radioID]
	if !ok || e.gen != gen {
		// Superseded by a renewal or a steal between the timer firing and this
		// callback acquiring the mutex.
		m.mu.Unlock()
		return
	}
	user := e.lock.User
	e.timer.Stop()
	delete(m.locks, radioID)
	m.mu.Unlock()

	m.log.Warn("lock expired", "radio", radioID, "user", user)

	m.expireMu.RLock()
	f := m.onExpire
	m.expireMu.RUnlock()
	if f == nil {
		return
	}
	// On its own goroutine: the callback forces the radio back to receive, which
	// talks to the rig and may block for a command timeout or two. Nothing here
	// should wait for that.
	go f(radioID)
}

// public returns the lock with the token removed, for anything that leaves the
// process.
func (l Lock) public() Lock {
	l.Token = ""
	return l
}

// newToken returns 128 bits of randomness, base64url encoded.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("lock: generating token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// tokenEqual compares in constant time, so a client cannot discover a valid
// token one byte at a time by measuring how long a rejection takes.
//
// The empty guard matters: ConstantTimeCompare answers 1 for two empty inputs,
// and an absent token must never authenticate anything.
func tokenEqual(want, got string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}
