package lock

import (
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestManager(t *testing.T, ttl time.Duration, enabled, steal bool) *Manager {
	t.Helper()
	m := NewManager(config.Lock{
		Enabled:    enabled,
		TTL:        config.Duration(ttl),
		AllowSteal: steal,
	}, WithLogger(testLogger()))
	t.Cleanup(m.Close)
	return m
}

func TestAcquireIssuesADistinctToken(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)

	a, err := m.Acquire("ic7610", "n0call", false)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if a.Token == "" {
		t.Fatal("no token issued")
	}
	// 128 bits, base64url, unpadded.
	if len(a.Token) != 22 {
		t.Errorf("token %q is %d chars, want 22 (128 bits base64url)", a.Token, len(a.Token))
	}
	if a.User != "n0call" || a.RadioID != "ic7610" {
		t.Errorf("lock = %+v", a)
	}
	if !a.ExpiresAt.After(time.Now()) {
		t.Error("lock is already expired")
	}

	// A second radio is independent: one operator on the IC-7610 while another
	// works the TS-590SG is the whole point of per-radio locking.
	b, err := m.Acquire("ts590sg", "oh2xyz", false)
	if err != nil {
		t.Fatalf("Acquire second radio: %v", err)
	}
	if b.Token == a.Token {
		t.Error("two locks share a token")
	}
	if err := m.Check("ts590sg", b.Token); err != nil {
		t.Errorf("Check on the second radio: %v", err)
	}
}

func TestAcquireHeldByAnotherUser(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)

	if _, err := m.Acquire("ic7610", "n0call", false); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l, err := m.Acquire("ic7610", "oh2xyz", false)
	if !errors.Is(err, ErrHeldByOther) {
		t.Fatalf("second Acquire = %v, want ErrHeldByOther", err)
	}
	if l.User != "n0call" {
		t.Errorf("rejection did not name the holder: %+v", l)
	}
	if l.Token != "" {
		t.Error("rejection leaked the holder's token")
	}
}

func TestAcquireBySameUserReissues(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)

	a, _ := m.Acquire("ic7610", "n0call", false)
	b, err := m.Acquire("ic7610", "n0call", false)
	if err != nil {
		t.Fatalf("re-acquire by the holder: %v", err)
	}
	if b.Token == a.Token {
		t.Error("re-acquire returned the same token")
	}
	if err := m.Check("ic7610", a.Token); err == nil {
		t.Error("the superseded token still works")
	}
	if err := m.Check("ic7610", b.Token); err != nil {
		t.Errorf("the new token does not work: %v", err)
	}
}

func TestStealRequiresConfiguration(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)
	a, _ := m.Acquire("ic7610", "n0call", false)

	if _, err := m.Acquire("ic7610", "oh2xyz", true); !errors.Is(err, ErrHeldByOther) {
		t.Fatalf("forced Acquire with stealing disabled = %v, want ErrHeldByOther", err)
	}
	if err := m.Check("ic7610", a.Token); err != nil {
		t.Errorf("holder lost the lock to a refused steal: %v", err)
	}

	steal := newTestManager(t, time.Minute, true, true)
	a, _ = steal.Acquire("ic7610", "n0call", false)
	b, err := steal.Acquire("ic7610", "oh2xyz", true)
	if err != nil {
		t.Fatalf("permitted steal: %v", err)
	}
	if err := steal.Check("ic7610", a.Token); !errors.Is(err, ErrHeldByOther) {
		t.Errorf("stolen-from token = %v, want ErrHeldByOther", err)
	}
	if err := steal.Check("ic7610", b.Token); err != nil {
		t.Errorf("thief cannot use the lock: %v", err)
	}
}

func TestCheckSlidesTheTTL(t *testing.T) {
	const ttl = 200 * time.Millisecond
	m := newTestManager(t, ttl, true, false)

	l, _ := m.Acquire("ic7610", "n0call", false)
	first := l.ExpiresAt

	// Keep issuing commands for well over one TTL. A sliding lease must survive.
	for range 6 {
		time.Sleep(ttl / 4)
		if err := m.Check("ic7610", l.Token); err != nil {
			t.Fatalf("Check during continuous use: %v", err)
		}
	}
	cur, ok := m.Inspect("ic7610")
	if !ok {
		t.Fatal("lock lapsed despite continuous use")
	}
	if !cur.ExpiresAt.After(first) {
		t.Errorf("expiry did not slide: %s vs %s", cur.ExpiresAt, first)
	}
}

func TestRenewAndRelease(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)
	l, _ := m.Acquire("ic7610", "n0call", false)

	r, err := m.Renew("ic7610", l.Token)
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if !r.ExpiresAt.After(l.ExpiresAt) {
		t.Error("Renew did not extend the lease")
	}
	if _, err := m.Renew("ic7610", "wrong-token-entirely"); !errors.Is(err, ErrBadToken) {
		t.Errorf("Renew with a bad token = %v, want ErrBadToken", err)
	}
	if _, err := m.Renew("ts590sg", l.Token); !errors.Is(err, ErrNoLock) {
		t.Errorf("Renew on an unlocked radio = %v, want ErrNoLock", err)
	}

	if err := m.Release("ic7610", "wrong"); !errors.Is(err, ErrBadToken) {
		t.Errorf("Release with a bad token = %v, want ErrBadToken", err)
	}
	if err := m.Release("ic7610", l.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, ok := m.Inspect("ic7610"); ok {
		t.Error("lock survived Release")
	}
	if err := m.Release("ic7610", l.Token); !errors.Is(err, ErrNoLock) {
		t.Errorf("double Release = %v, want ErrNoLock", err)
	}
}

func TestCheckErrors(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)

	if err := m.Check("ic7610", "anything"); !errors.Is(err, ErrNoLock) {
		t.Errorf("Check with no lock held = %v, want ErrNoLock", err)
	}
	l, _ := m.Acquire("ic7610", "n0call", false)
	if err := m.Check("ic7610", ""); !errors.Is(err, ErrNoLock) {
		t.Errorf("Check with no token = %v, want ErrNoLock", err)
	}
	if err := m.Check("ic7610", "not-the-right-token"); !errors.Is(err, ErrHeldByOther) {
		t.Errorf("Check with someone else's radio = %v, want ErrHeldByOther", err)
	}
	if err := m.Check("ic7610", l.Token); err != nil {
		t.Errorf("Check by the holder: %v", err)
	}
}

// A wrong token of the same length must take the constant-time path rather than
// short-circuiting on the first differing byte. This asserts the behaviour that
// path must have: a same-length near-miss is rejected exactly like any other.
func TestTokenComparisonIsConstantTime(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)
	l, _ := m.Acquire("ic7610", "n0call", false)

	nearMiss := []byte(l.Token)
	nearMiss[len(nearMiss)-1] ^= 1 // differs only in the last byte
	if err := m.Check("ic7610", string(nearMiss)); !errors.Is(err, ErrHeldByOther) {
		t.Errorf("near-miss token accepted or misclassified: %v", err)
	}
	if !tokenEqual(l.Token, l.Token) {
		t.Error("tokenEqual rejected an identical token")
	}
	if tokenEqual(l.Token, l.Token[:len(l.Token)-1]) {
		t.Error("tokenEqual accepted a prefix")
	}
	if tokenEqual("", "") {
		// subtle.ConstantTimeCompare returns 1 for two empty slices; an empty
		// token must never authenticate anything, so guard against that.
		t.Error("tokenEqual accepted two empty tokens")
	}
}

func TestExpiryIsProactiveAndFiresTheCallback(t *testing.T) {
	m := newTestManager(t, 50*time.Millisecond, true, false)

	fired := make(chan string, 4)
	m.SetOnExpire(func(radioID string) { fired <- radioID })

	if _, err := m.Acquire("ic7610", "n0call", false); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Nobody calls anything: this is the crashed-client case, and the timer is
	// the only thing that can drop PTT.
	select {
	case id := <-fired:
		if id != "ic7610" {
			t.Errorf("expiry fired for %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expiry callback never fired: a crashed client would keep transmitting")
	}
	if _, ok := m.Inspect("ic7610"); ok {
		t.Error("expired lock still held")
	}
}

func TestRenewalPreventsTheExpiryCallback(t *testing.T) {
	m := newTestManager(t, 80*time.Millisecond, true, false)
	fired := make(chan string, 4)
	m.SetOnExpire(func(radioID string) { fired <- radioID })

	l, _ := m.Acquire("ic7610", "n0call", false)
	for range 5 {
		time.Sleep(20 * time.Millisecond)
		if err := m.Check("ic7610", l.Token); err != nil {
			t.Fatalf("Check: %v", err)
		}
	}
	select {
	case id := <-fired:
		t.Fatalf("expiry fired for %q despite continuous renewal", id)
	default:
	}

	// And it does fire once use stops.
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("expiry never fired after use stopped")
	}
}

func TestReleaseDoesNotFireExpiry(t *testing.T) {
	m := newTestManager(t, 60*time.Millisecond, true, false)
	fired := make(chan string, 4)
	m.SetOnExpire(func(radioID string) { fired <- radioID })

	l, _ := m.Acquire("ic7610", "n0call", false)
	if err := m.Release("ic7610", l.Token); err != nil {
		t.Fatalf("Release: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case id := <-fired:
		t.Errorf("expiry fired for %q after an explicit release", id)
	default:
	}
}

func TestStealCancelsTheVictimsExpiryTimer(t *testing.T) {
	m := newTestManager(t, 100*time.Millisecond, true, true)
	fired := make(chan string, 4)
	m.SetOnExpire(func(radioID string) { fired <- radioID })

	if _, err := m.Acquire("ic7610", "n0call", false); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	b, err := m.Acquire("ic7610", "oh2xyz", true)
	if err != nil {
		t.Fatalf("steal: %v", err)
	}

	// The victim's timer must not expire the thief's fresh lock.
	time.Sleep(70 * time.Millisecond)
	select {
	case <-fired:
		t.Fatal("the stolen-from lock's timer expired the new holder's lock")
	default:
	}
	if err := m.Check("ic7610", b.Token); err != nil {
		t.Errorf("thief's lock died early: %v", err)
	}
}

func TestDisabledLockingPassesEveryCheck(t *testing.T) {
	m := newTestManager(t, time.Minute, false, false)

	if err := m.Check("ic7610", ""); err != nil {
		t.Errorf("Check with locking disabled = %v, want nil", err)
	}
	if _, err := m.Acquire("ic7610", "n0call", false); err != nil {
		t.Fatalf("Acquire with locking disabled: %v", err)
	}
	// Even against a held lock, and with somebody else's token.
	if err := m.Check("ic7610", "garbage"); err != nil {
		t.Errorf("Check by a non-holder with locking disabled = %v, want nil", err)
	}
	if m.Enabled() {
		t.Error("Enabled() true for a disabled manager")
	}
}

func TestInspectRedactsTheToken(t *testing.T) {
	m := newTestManager(t, time.Minute, true, false)
	l, _ := m.Acquire("ic7610", "n0call", false)

	got, ok := m.Inspect("ic7610")
	if !ok {
		t.Fatal("Inspect found no lock")
	}
	if got.Token != "" {
		t.Error("Inspect leaked the token; a GET would hand control to anyone who can read it")
	}
	if got.User != "n0call" || got.RadioID != "ic7610" {
		t.Errorf("Inspect = %+v", got)
	}
	if !m.IsHolder("ic7610", l.Token) {
		t.Error("IsHolder said no to the holder")
	}
	if m.IsHolder("ic7610", "someone-else") || m.IsHolder("ic7610", "") {
		t.Error("IsHolder said yes to a non-holder")
	}
	if _, ok := m.Inspect("ts590sg"); ok {
		t.Error("Inspect invented a lock")
	}
}

func TestIsHolderDoesNotRenew(t *testing.T) {
	m := newTestManager(t, 300*time.Millisecond, true, false)
	l, _ := m.Acquire("ic7610", "n0call", false)
	before, _ := m.Inspect("ic7610")

	time.Sleep(20 * time.Millisecond)
	if !m.IsHolder("ic7610", l.Token) {
		t.Fatal("IsHolder said no")
	}
	after, _ := m.Inspect("ic7610")
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Error("IsHolder slid the lease; a read must not extend someone's exclusive control")
	}
}

func TestDefaultTTL(t *testing.T) {
	m := NewManager(config.Lock{Enabled: true}, WithLogger(testLogger()))
	defer m.Close()
	if m.TTL() != defaultTTL {
		t.Errorf("TTL = %s, want the %s default", m.TTL(), defaultTTL)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	m := newTestManager(t, 100*time.Millisecond, true, true)
	m.SetOnExpire(func(string) {})

	var wg sync.WaitGroup
	stop := time.Now().Add(300 * time.Millisecond)
	for i := range 8 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			user := string(rune('a' + i))
			for time.Now().Before(stop) {
				l, err := m.Acquire("ic7610", user, true)
				if err != nil {
					continue
				}
				_ = m.Check("ic7610", l.Token)
				_, _ = m.Renew("ic7610", l.Token)
				_, _ = m.Inspect("ic7610")
				_ = m.IsHolder("ic7610", l.Token)
				_ = m.Release("ic7610", l.Token)
			}
		}(i)
	}
	wg.Wait()
}
