package rig

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// sleepingRig refuses everything with an NG, which is what an IC-7610 switched
// off over CAT actually does: its CI-V circuit stays alive and answers, so the
// link is perfect and the radio is asleep. wake makes it start answering again.
type sleepingRig struct {
	*fakeRig
	awake chan struct{}
}

func newSleepingRig() *sleepingRig {
	return &sleepingRig{fakeRig: newFakeRig(), awake: make(chan struct{})}
}

func (r *sleepingRig) asleep() bool {
	select {
	case <-r.awake:
		return false
	default:
		return true
	}
}

func (r *sleepingRig) Init(ctx context.Context, c backend.Conn) error {
	if r.asleep() {
		return ErrNAK
	}
	return r.fakeRig.Init(ctx, c)
}

func (r *sleepingRig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	if r.asleep() {
		return ErrNAK
	}
	return r.fakeRig.Poll(ctx, c, tier)
}

// PowerOn is what the supervisor or the session sends; it wakes this fake.
func (r *sleepingRig) PowerOn(ctx context.Context, c backend.Conn) error {
	r.record("power=on")
	select {
	case <-r.awake:
	default:
		close(r.awake)
	}
	return nil
}

func newSleepingSession(t *testing.T, r *sleepingRig) *Session {
	t.Helper()
	s, err := NewSession(config.Radio{
		ID:      "sleepy",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(10 * time.Millisecond),
		},
	}, r, newFakeDialer(newFakeDevice()), WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond), WithEventQueue(256))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	s.Start(ctx)
	return s
}

// A radio that answers but refuses everything is a third state, and neither of
// the other two describes it: the link is fine, so "disconnected" would send
// somebody to check a cable that is perfectly seated.
func TestStandbyIsConnectedButNotUsable(t *testing.T) {
	s := newSleepingSession(t, newSleepingRig())

	waitFor(t, "the radio to be recognised as switched off", func() bool {
		return s.State().Standby
	})
	if !s.State().Connected {
		t.Error("standby reported as disconnected; the link is fine and the radio is asleep")
	}

	// And the internal flag agrees with the published one. Letting those two
	// disagree produced exactly the bug it looks like it would: the API
	// reporting the radio connected while every command was refused with "not
	// currently connected".
	if err := s.requireConnected(); !errors.Is(err, ErrStandby) {
		t.Errorf("requireConnected = %v, want ErrStandby so the client is told the remedy", err)
	}
}

// Waking has to work from standby, which is the state it is for. The session is
// parked holding a perfectly good port, so waiting for a reconnection would
// wait forever — that loop exists to prevent one.
func TestPowerOnWakesFromStandby(t *testing.T) {
	r := newSleepingRig()
	s := newSleepingSession(t, r)

	waitFor(t, "the radio to be recognised as switched off", func() bool {
		return s.State().Standby
	})

	if _, err := s.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	waitFor(t, "the radio to wake", func() bool {
		st := s.State()
		return st.Connected && !st.Standby
	})

	// Sent on the open port rather than armed for a reconnection that is not
	// coming.
	if s.wakeWanted.Load() {
		t.Error("the wake was armed for later while a port was open")
	}
}

// silentRig does not answer at all, which is what a pulled cable or a wrong bus
// address looks like.
type silentRig struct{ *fakeRig }

func (r *silentRig) Init(context.Context, backend.Conn) error { return ErrTimeout }

// Standby is specifically "answering, and refusing". A radio that says nothing
// is unreachable, and reporting that as switched off would tell an operator to
// press a power button when the real fault is a cable or a bus address.
func TestStandbyNeedsAnAnsweringRadio(t *testing.T) {
	s, err := NewSession(config.Radio{
		ID:      "silent",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(10 * time.Millisecond),
		},
	}, &silentRig{newFakeRig()}, newFakeDialer(newFakeDevice()),
		WithLogger(testLogger()), WithCommandTimeout(50*time.Millisecond), WithEventQueue(256))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	s.Start(ctx)

	// Long enough that a standby misdiagnosis would have shown.
	time.Sleep(200 * time.Millisecond)
	if s.State().Standby {
		t.Error("a radio that does not answer at all was reported as switched off")
	}
	if s.State().Connected {
		t.Error("a radio that does not answer at all was reported as connected")
	}
}
