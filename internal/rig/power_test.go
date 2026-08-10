package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// Switching a radio off is the one command whose success looks exactly like a
// failure: the radio stops answering. These pin the two halves of coping with
// that — remembering that it was deliberate, and being able to undo it from the
// state where there is no link to undo it on.

func TestPowerOffRecordsTheIntent(t *testing.T) {
	h := newHarness(t, nil).start(t)

	if _, err := h.s.PowerOff(context.Background(), false); err != nil {
		t.Fatalf("PowerOff: %v", err)
	}
	if !h.s.poweredOff.Load() {
		t.Error("a power-off was not recorded, so the disconnection that follows " +
			"would be logged as a fault")
	}
	if got := h.rig.setLog(); len(got) == 0 || got[len(got)-1] != "power=off" {
		t.Errorf("set log = %v, want a power-off last", got)
	}
}

// deep is passed through rather than swallowed, and is not the default: the
// shallow off is the one a remote station can wake from.
func TestPowerOffDeepIsOptIn(t *testing.T) {
	for _, tt := range []struct {
		deep bool
		want string
	}{
		{false, "power=off"},
		{true, "power=off_deep"},
	} {
		h := newHarness(t, nil).start(t)
		if _, err := h.s.PowerOff(context.Background(), tt.deep); err != nil {
			t.Fatalf("PowerOff(%v): %v", tt.deep, err)
		}
		got := h.rig.setLog()
		if len(got) == 0 || got[len(got)-1] != tt.want {
			t.Errorf("PowerOff(deep=%v) logged %v, want %q last", tt.deep, got, tt.want)
		}
	}
}

// A failed power-off must not leave the session believing the radio is off:
// that would suppress the reconnect logging for a link that really is broken.
func TestPowerOffFailureClearsTheIntent(t *testing.T) {
	boom := errors.New("rig said no")
	h := newHarness(t, nil).start(t)
	h.rig.setPowerErr(boom)

	if _, err := h.s.PowerOff(context.Background(), false); !errors.Is(err, boom) {
		t.Fatalf("PowerOff = %v, want the radio's own failure", err)
	}
	if h.s.poweredOff.Load() {
		t.Error("a failed power-off still recorded the radio as switched off")
	}
}

// Waking works where it is needed: with no link. It arms a request rather than
// racing the supervisor for an exclusive serial port.
func TestPowerOnWhileDisconnectedArmsAWake(t *testing.T) {
	h := newHarness(t, nil) // never started, so never connected

	if _, err := h.s.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if !h.s.wakeWanted.Load() {
		t.Fatal("no wake was armed, so a radio that is off could never be woken")
	}
	// The supervisor consumes it exactly once. Repeating it on every attempt
	// would hold a radio somebody switched off at the panel permanently on.
	if !h.s.takeWakeRequest() {
		t.Error("takeWakeRequest did not see the armed wake")
	}
	if h.s.takeWakeRequest() {
		t.Error("the wake request survived being taken; it would repeat forever")
	}
}

// On a live link there is a port in hand, so the wake goes out immediately —
// useful when the radio is awake but for a front panel somebody switched.
func TestPowerOnWhileConnectedSendsImmediately(t *testing.T) {
	h := newHarness(t, nil).start(t)

	if _, err := h.s.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if h.s.wakeWanted.Load() {
		t.Error("armed a wake for later while the link was up")
	}
	if got := h.rig.setLog(); len(got) == 0 || got[len(got)-1] != "power=on" {
		t.Errorf("set log = %v, want a power-on last", got)
	}
}

// Waking clears the record of having switched it off, so a wake that is
// requested and then fails does not leave reconnects logged as expected.
func TestPowerOnClearsTheOffIntent(t *testing.T) {
	h := newHarness(t, nil).start(t)
	h.s.poweredOff.Store(true)

	if _, err := h.s.PowerOn(context.Background()); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}
	if h.s.poweredOff.Load() {
		t.Error("a wake left the radio recorded as switched off")
	}
}

// A radio without the command refuses both directions rather than pretending.
func TestPowerRefusedWithoutTheCapability(t *testing.T) {
	h := newHarness(t, func(rc *config.Radio) {}).start(t)
	caps := h.rig.caps
	caps.PowerSwitch = false
	h.rig.caps = caps
	h.s.publishCaps()

	if _, err := h.s.PowerOff(context.Background(), false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("PowerOff = %v, want ErrUnsupported", err)
	}
	if _, err := h.s.PowerOn(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("PowerOn = %v, want ErrUnsupported", err)
	}
}

var _ = radio.Caps{}
