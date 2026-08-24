package rig

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// txAudioRig is a radio that can work its transmit audio chain, and records the
// order the session wrote in — which is the half of this feature that a unit
// test can hold and a radio on the bench cannot be relied on to show.
type txAudioRig struct {
	*fakeRig
}

func newTXAudioRig() *txAudioRig {
	r := &txAudioRig{fakeRig: newFakeRig()}
	r.caps.TXAudioGainControl = true
	r.caps.ProcControl = true
	r.caps.ProcLevelControl = true
	return r
}

func (r *txAudioRig) SetTXAudioGain(_ context.Context, _ backend.Conn, pct float64) error {
	r.record("tx_audio_gain")
	_ = pct
	return nil
}

func (r *txAudioRig) SetProc(_ context.Context, _ backend.Conn, on bool) error {
	r.record("proc")
	_ = on
	return nil
}

func (r *txAudioRig) SetProcLevel(_ context.Context, _ backend.Conn, pct float64) error {
	r.record("proc_level")
	_ = pct
	return nil
}

// The processor's switch must reach the radio before its level.
//
// This is the same trap the noise blanker already carries: a Kenwood refuses NL
// while the blanker is off, "an error occurs" in as many words, and the
// processors behave the same way on the sets that document it. A client
// switching the processor on and setting its level in one request is the
// obvious thing to send, and sending the level first would fail a request that
// is perfectly sensible as a whole.
func TestProcSwitchIsWrittenBeforeItsLevel(t *testing.T) {
	r := newTXAudioRig()
	h := newHarnessRig(t, r, nil)
	h.start(t)

	on, level := true, 60.0
	if _, err := h.s.ApplyPatch(context.Background(),
		PatchRequest{Proc: &on, ProcLevel: &level}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	log := r.setLog()
	proc := slices.Index(log, "proc")
	procLevel := slices.Index(log, "proc_level")
	switch {
	case proc < 0:
		t.Fatalf("the processor switch never reached the radio: %v", log)
	case procLevel < 0:
		t.Fatalf("the processor level never reached the radio: %v", log)
	case proc > procLevel:
		t.Errorf("the level was written before the switch: %v", log)
	}
}

// Every field is refused on a radio whose capabilities do not carry it, and
// refused as unsupported rather than as a link failure — the session is
// deliberately unstarted, because this is a decision remoses makes about the
// radio and not something the radio gets asked.
func TestTXAudioRefusedWithoutCaps(t *testing.T) {
	pct, on := 50.0, true
	for _, tc := range []struct {
		name string
		req  PatchRequest
	}{
		{"tx_audio_gain", PatchRequest{TXAudioGain: &pct}},
		{"proc", PatchRequest{Proc: &on}},
		{"proc_level", PatchRequest{ProcLevel: &pct}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A rig with the commands but no capability for them, so the refusal
			// can only be coming from the capability check.
			r := newTXAudioRig()
			r.caps.TXAudioGainControl = false
			r.caps.ProcControl = false
			r.caps.ProcLevelControl = false
			h := newHarnessRig(t, r, nil)

			_, err := h.s.ApplyPatch(context.Background(), tc.req)
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
			if errors.Is(err, ErrDisconnected) {
				t.Error("an unsupported control reported as a connection failure")
			}
			if log := r.setLog(); len(log) != 0 {
				t.Errorf("a refused control still reached the radio: %v", log)
			}
		})
	}
}

// A backend that does not implement the interface at all must refuse too, even
// when the capabilities say otherwise. The two can disagree — capabilities are
// built per model and the interface is satisfied per backend — and the type
// assertion is the only thing standing between that disagreement and a nil
// dereference.
func TestTXAudioRefusedWhenTheBackendCannot(t *testing.T) {
	r := newFakeRig() // no TXAudioController methods
	r.caps.TXAudioGainControl = true
	r.caps.ProcControl = true
	r.caps.ProcLevelControl = true
	h := newHarnessRig(t, r, nil)

	pct := 50.0
	_, err := h.s.ApplyPatch(context.Background(), PatchRequest{TXAudioGain: &pct})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}

// Percentages are checked before anything reaches the wire, because the radios
// scale them into their own counts and a number outside the range would arrive
// as a silently clamped or wrapped value rather than an error.
func TestTXAudioPercentagesAreRangeChecked(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(*float64) PatchRequest
	}{
		{"tx_audio_gain", func(v *float64) PatchRequest { return PatchRequest{TXAudioGain: v} }},
		{"proc_level", func(v *float64) PatchRequest { return PatchRequest{ProcLevel: v} }},
	} {
		for _, bad := range []float64{-1, 101} {
			t.Run(tc.name, func(t *testing.T) {
				r := newTXAudioRig()
				h := newHarnessRig(t, r, nil)
				v := bad
				if _, err := h.s.ApplyPatch(context.Background(), tc.req(&v)); err == nil {
					t.Fatalf("%s %g was accepted", tc.name, bad)
				}
				if log := r.setLog(); len(log) != 0 {
					t.Errorf("an out-of-range value reached the radio: %v", log)
				}
			})
		}
	}
}

// The state a radio reports must survive a patch, so that a client redrawing
// the control sees what the radio actually holds.
func TestTXAudioStateRoundTrips(t *testing.T) {
	gain, level := 42.0, 63.0
	on := true
	st := radio.State{}.Apply(radio.Patch{TXAudioGain: &gain, Proc: &on, ProcLevel: &level})

	if st.TXAudioGain == nil || *st.TXAudioGain != gain {
		t.Errorf("mic gain = %v, want %g", st.TXAudioGain, gain)
	}
	if st.Proc == nil || !*st.Proc {
		t.Errorf("proc = %v, want true", st.Proc)
	}
	if st.ProcLevel == nil || *st.ProcLevel != level {
		t.Errorf("proc level = %v, want %g", st.ProcLevel, level)
	}
}
