package rig

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

func TestReconnectAfterDisconnectReRunsInit(t *testing.T) {
	h := startedHarness(t, nil)

	if got := h.rig.inits.Load(); got != 1 {
		t.Fatalf("Init ran %d times before the cable was pulled, want 1", got)
	}
	freqBefore := h.s.State().Frequency

	// Pull the cable: the port returns transport.ErrDisconnected.
	h.dl.lastPort().Close()

	waitFor(t, "the session to notice the disconnect", func() bool {
		return !h.s.Connected()
	})
	waitFor(t, "a redial", func() bool { return h.dl.dialCount() >= 2 })
	waitFor(t, "the radio to come back", h.s.Connected)

	if got := h.rig.inits.Load(); got < 2 {
		t.Fatalf("Init ran %d times after reconnect, want at least 2: push updates would stay off", got)
	}
	if got := h.s.State().Frequency; got != freqBefore {
		t.Errorf("frequency after reconnect = %d, want %d", got, freqBefore)
	}
	if !h.s.State().Connected {
		t.Error("state still reports disconnected")
	}
}

func TestDisconnectPublishesConnEventAndClearsPTT(t *testing.T) {
	h := startedHarness(t, nil)
	sender := newFakeCW()
	h.s.SetCWSender(sender)

	if _, err := h.s.SetPTT(context.Background(), true); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	waitFor(t, "state to show transmit", func() bool { return h.s.State().PTT })

	ch, unsub := h.s.Subscribe()
	defer unsub()

	h.dl.lastPort().Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == EventConn && !ev.State.Connected {
				if ev.State.PTT {
					t.Error("PTT still true in the disconnect event: a radio that is gone cannot be transmitting")
				}
				if ev.Err == "" {
					t.Error("disconnect event carries no reason")
				}
				if sender.abortCount() == 0 {
					t.Error("CW queue was not flushed on disconnect")
				}
				return
			}
		case <-deadline:
			t.Fatal("no disconnect event published")
		}
	}
}

func TestDialFailuresAreRetried(t *testing.T) {
	h := newHarness(t, nil)
	h.dl.failFor = 3
	h.start(t)

	if h.dl.dialCount() < 4 {
		t.Errorf("dial count = %d, want at least 4 (3 failures then success)", h.dl.dialCount())
	}
}

func TestInitFailureRetriesWithoutMarkingConnected(t *testing.T) {
	h := newHarness(t, nil)
	h.rig.setInitErr(errors.New("rig did not answer AI"))

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = h.s.Close()
	}()
	h.s.Start(ctx)

	waitFor(t, "several init attempts", func() bool { return h.rig.inits.Load() >= 3 })
	if h.s.Connected() {
		t.Error("session reports connected despite Init failing")
	}
	if st := h.s.State(); st.Connected {
		t.Error("state reports connected despite Init failing")
	}

	// Once the rig answers, the supervisor gets in without any intervention.
	h.rig.setInitErr(nil)
	waitFor(t, "recovery", h.s.Connected)
}

func TestUnansweringRigIsTornDownByThePoller(t *testing.T) {
	h := newHarness(t, func(rc *config.Radio) {
		rc.Poll.Interval = config.Duration(5 * time.Millisecond)
	})
	h.s.cmdTimeout = 20 * time.Millisecond
	h.start(t)

	dialsBefore := h.dl.dialCount()

	// The port stays open but the rig stops answering — switched off at the
	// front panel with the USB adapter still enumerated. Nothing arrives on the
	// port, so only the poller can notice.
	h.dev.setSilent("FA", true)
	waitFor(t, "the poller to give up on a silent rig", func() bool {
		return h.dl.dialCount() > dialsBefore
	})
	h.dev.setSilent("FA", false)
	waitFor(t, "recovery", h.s.Connected)
}

func TestBackoffDoublesAndIsBounded(t *testing.T) {
	minD, maxD := 100*time.Millisecond, 30*time.Second

	d := minD
	var steps []time.Duration
	for range 20 {
		steps = append(steps, d)
		d = nextBackoff(d, minD, maxD)
	}
	if steps[0] != minD {
		t.Errorf("first backoff = %s, want %s", steps[0], minD)
	}
	if steps[1] != 200*time.Millisecond {
		t.Errorf("second backoff = %s, want 200ms", steps[1])
	}
	for i, s := range steps {
		if s > maxD {
			t.Fatalf("backoff step %d = %s exceeds the %s cap", i, s, maxD)
		}
	}
	if steps[len(steps)-1] != maxD {
		t.Errorf("backoff did not reach the cap: %s", steps[len(steps)-1])
	}
	if got := nextBackoff(0, minD, maxD); got != minD {
		t.Errorf("nextBackoff(0) = %s, want %s", got, minD)
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	const base = time.Second
	var sawLow, sawHigh bool
	for range 200 {
		d := jitter(base)
		if d < 750*time.Millisecond || d > 1250*time.Millisecond {
			t.Fatalf("jitter(%s) = %s, outside +/-25%%", base, d)
		}
		if d < base {
			sawLow = true
		}
		if d > base {
			sawHigh = true
		}
	}
	if !sawLow || !sawHigh {
		t.Error("jitter does not actually spread")
	}
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %s, want 0", got)
	}
}

func TestCloseIsIdempotentAndStopsEverything(t *testing.T) {
	h := startedHarness(t, nil)
	if err := h.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if h.s.Connected() {
		t.Error("session still connected after Close")
	}
	if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 14000000); err == nil {
		t.Error("command accepted after Close")
	}
}

// TestDefaultBackoffCeiling pins the number an operator actually experiences:
// the ceiling is how long a radio that has been plugged back in can go
// unnoticed, because the supervisor is asleep between dials.
//
// TestBackoffDoublesAndIsBounded above tests the function against bounds it is
// handed; this tests the default the daemon ships with, which is the one that
// was measured on hardware and found wanting at 30 s.
func TestDefaultBackoffCeiling(t *testing.T) {
	if defaultBackoffMax > 5*time.Second {
		t.Errorf("default backoff ceiling is %s; a replugged radio can go unnoticed that long, "+
			"and a failed dial costs one open() on a missing path", defaultBackoffMax)
	}

	// And the ceiling is actually reached from the floor, rather than the floor
	// being so large the schedule skips it.
	d := defaultBackoffMin
	for range 20 {
		d = nextBackoff(d, defaultBackoffMin, defaultBackoffMax)
	}
	if d != defaultBackoffMax {
		t.Errorf("schedule settles at %s, want the ceiling %s", d, defaultBackoffMax)
	}
}
