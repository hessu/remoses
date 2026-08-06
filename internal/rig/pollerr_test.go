package rig

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

func TestIsFatalPollErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"disconnected", fmt.Errorf("read: %w", ErrDisconnected), true},
		{"timeout", fmt.Errorf("poll: %w", ErrTimeout), true},
		{"cancelled", context.Canceled, true},
		{"deadline", context.DeadlineExceeded, true},

		// Everything below is the rig answering something the backend did not
		// expect — which proves it is still there.
		{"icom acks an unimplemented command", errors.New("civ: read 1A/03: unexpected reply FB"), false},
		{"kenwood refuses", errors.New(`kenwood: read FW: rejected with "?;"`), false},
		{"nak", fmt.Errorf("set: %w", ErrNAK), false},
		{"unsupported", fmt.Errorf("set: %w", ErrUnsupported), false},
		// Busy is the strongest evidence of all that the rig is alive: it
		// answered. Reconnecting over it would drop a working radio because it
		// was momentarily occupied.
		{"busy", fmt.Errorf("yaesu: IF;: the rig answered ?: %w", ErrBusy), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFatalPollErr(tc.err); got != tc.want {
				t.Errorf("isFatalPollErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// pickyRig answers the fast tier but declines the slow one, the way a rig that
// does not implement an optional command does.
type pickyRig struct {
	*fakeRig
	slowErr error
}

func (r *pickyRig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	if tier == backend.PollSlow {
		r.pollSlow.Add(1)
		return r.slowErr
	}
	return r.fakeRig.Poll(ctx, c, tier)
}

// A radio whose frequency, mode, PTT and meters all work must not be held in a
// permanent reconnect loop because it declines one optional command. Rigs
// differ in which of those they answer, and this is the difference between
// working on one model and working on a family.
func TestSlowPollRefusalStillConnects(t *testing.T) {
	dev := newFakeDevice()
	dl := newFakeDialer(dev)
	r := &pickyRig{
		fakeRig: newFakeRig(),
		slowErr: errors.New("civ: read 1A/03: unexpected reply FB"),
	}

	s, err := NewSession(config.Radio{
		ID:      "picky",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(30 * time.Millisecond),
		},
	}, r, dl, WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond), WithEventQueue(256))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	s.Start(ctx)

	waitFor(t, "radio to connect despite the slow poll refusal", s.Connected)

	// It must also stay up rather than flapping once the slow ticker fires.
	inits := r.inits.Load()
	time.Sleep(120 * time.Millisecond)
	if !s.Connected() {
		t.Fatal("session dropped the connection after a slow-poll refusal")
	}
	if got := r.inits.Load(); got != inits {
		t.Errorf("reconnected %d time(s) after a non-fatal slow poll refusal", got-inits)
	}

	// And a write must still succeed, even though its read-back touches the
	// tier the rig declines.
	if _, err := s.SetPower(context.Background(), radio.PowerSet{Pct: ptrTo(50.0)}); err != nil {
		t.Errorf("SetPower: %v, want success despite the partial read-back", err)
	}
}

// A busy answer is transient by definition, so the session must do nothing
// durable about it: no reconnect, and no dropping the tier that reported it.
// The next tick asking again is the entire recovery mechanism, and a rig that
// stopped being polled would never take it.
func TestBusyPollNeitherReconnectsNorStopsPolling(t *testing.T) {
	dev := newFakeDevice()
	dl := newFakeDialer(dev)
	r := &pickyRig{
		fakeRig: newFakeRig(),
		slowErr: fmt.Errorf("yaesu: PC;: the rig answered ?: %w", ErrBusy),
	}

	s, err := NewSession(config.Radio{
		ID:      "busy",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(30 * time.Millisecond),
		},
	}, r, dl, WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond), WithEventQueue(256))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.Close() })
	s.Start(ctx)

	waitFor(t, "radio to connect despite a busy slow poll", s.Connected)

	inits, polls := r.inits.Load(), r.pollSlow.Load()
	time.Sleep(120 * time.Millisecond)

	if !s.Connected() {
		t.Fatal("session dropped the connection over a busy answer")
	}
	if got := r.inits.Load(); got != inits {
		t.Errorf("reconnected %d time(s) because the rig said it was busy", got-inits)
	}
	if got := r.pollSlow.Load(); got <= polls {
		t.Error("the slow tier stopped being polled; a busy answer must not disable anything")
	}

	// And a write still succeeds: its read-back touches the busy tier, which is
	// not a reason to report a change that did happen as a failure.
	if _, err := s.SetPower(context.Background(), radio.PowerSet{Pct: ptrTo(50.0)}); err != nil {
		t.Errorf("SetPower: %v, want success despite the busy read-back", err)
	}
}
