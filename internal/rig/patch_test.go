package rig

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Two bugs an FT-857D found on the air, both in ApplyPatch and neither specific
// to that radio. The first is about a VFO the request names and the radio
// cannot address; the second about a mode-and-data-mode pairing the radio has
// no code for. See applyModePair and the VFO check at the top of ApplyPatch.

// A named VFO must be refused by a radio that cannot address one even when the
// request carries nothing else. The VFO is a selector rather than a field, so
// such a request looks empty — and the empty short-circuit used to answer it
// 200 with the current state, telling the client a selection it never made had
// been accepted. On an FT-857D, whose one VFO command is a blind toggle,
// {"vfo": "B"} came back OK having done nothing at all.
//
// The session is deliberately unstarted: this is a validation failure, and it
// must be reported on its merits rather than as "radio unreachable".
func TestPatchNamedVFORefusedWhenNothingElseIsSet(t *testing.T) {
	h := newHarness(t, nil)
	if h.s.Connected() {
		t.Fatal("session should not be connected before Start")
	}

	for _, vfo := range []radio.VFO{radio.VFOA, radio.VFOB, radio.VFOMain, radio.VFOSub} {
		t.Run(vfo.String(), func(t *testing.T) {
			_, err := h.s.ApplyPatch(context.Background(), PatchRequest{VFO: vfo})
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("ApplyPatch{VFO: %s} err = %v, want ErrUnsupported", vfo, err)
			}
			if errors.Is(err, ErrDisconnected) {
				t.Error("an unsupported VFO reported as a connection failure")
			}
		})
	}

	// The zero value addresses whichever VFO the radio is on, which every radio
	// has, so a request carrying only that is still the no-op it always was.
	t.Run("current is still an empty request", func(t *testing.T) {
		if _, err := h.s.ApplyPatch(context.Background(), PatchRequest{VFO: radio.VFOCurrent}); err != nil {
			t.Fatalf("ApplyPatch{VFO: current} err = %v, want nil", err)
		}
	})
}

// dualRig is a radio that really can address a VFO by name — which is the case
// the check above is not enough for on its own. It advertises current, A and B,
// exactly as an IC-9700 does, and can exchange its two receivers.
type dualRig struct {
	*fakeRig
	addressed []radio.VFO // every VFO that reached a dual-VFO command
	exchanges int
}

func newDualRig() *dualRig {
	r := &dualRig{fakeRig: newFakeRig()}
	r.caps.VFOs = []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}
	r.caps.BandExchange = true
	return r
}

func (r *dualRig) ExchangeBands(context.Context, backend.Conn) error {
	r.exchanges++
	r.record("exchange_bands")
	return nil
}

func (r *dualRig) ReadVFOs(context.Context, backend.Conn) error { return nil }
func (r *dualRig) SetSplit(context.Context, backend.Conn, bool) error {
	return nil
}
func (r *dualRig) SetDualWatch(context.Context, backend.Conn, bool) error { return nil }

func (r *dualRig) SetVFOFrequency(_ context.Context, _ backend.Conn, vfo radio.VFO, _ uint64) error {
	r.addressed = append(r.addressed, vfo)
	return nil
}

func (r *dualRig) SetVFOMode(_ context.Context, _ backend.Conn, vfo radio.VFO, _ radio.Mode, _ bool, _ int) error {
	r.addressed = append(r.addressed, vfo)
	return nil
}

// A VFO the radio does not advertise must be refused even where the backend has
// the commands to address one — which is the half an IC-9700 found. That radio
// has a second receiver with its own VFO A and B, `25`/`26` reach neither, and
// Caps.VFOs says so by listing only current, A and B. Asking it for "sub" was
// accepted and applied to VFO B of the *main* band: a 200, and a different
// receiver moved from the one the client named.
func TestPatchVFONotInCapsIsRefusedEvenWhenAddressable(t *testing.T) {
	r := newDualRig()
	h := newHarnessRig(t, r, nil)
	ctx := context.Background()
	hz := uint64(432500000)

	for _, vfo := range []radio.VFO{radio.VFOSub, radio.VFOMain} {
		t.Run(vfo.String(), func(t *testing.T) {
			_, err := h.s.ApplyPatch(ctx, PatchRequest{VFO: vfo, Frequency: &hz})
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("ApplyPatch{VFO: %s} err = %v, want ErrUnsupported", vfo, err)
			}
		})
	}
	if len(r.addressed) != 0 {
		t.Errorf("an unadvertised VFO reached the radio: %v", r.addressed)
	}

	// And the ones it does advertise still work.
	t.Run("B is advertised and goes through", func(t *testing.T) {
		h.start(t)
		if _, err := h.s.ApplyPatch(ctx, PatchRequest{VFO: radio.VFOB, Frequency: &hz}); err != nil {
			t.Fatalf("ApplyPatch{VFO: B} err = %v, want nil", err)
		}
		if len(r.addressed) != 1 || r.addressed[0] != radio.VFOB {
			t.Errorf("addressed = %v, want [B]", r.addressed)
		}
	})
}

// Exchanging the two receivers is what reaches a band an IC-9700 will not put
// its main receiver on while the sub one is there. It is an action: true only,
// gated on the capability, and applied before everything else in the request —
// because after it, every other field in that request is about a different
// band.
func TestPatchExchangeBands(t *testing.T) {
	ctx := context.Background()
	yes, no := true, false

	t.Run("refused where the radio has no such command", func(t *testing.T) {
		h := newHarness(t, nil) // the plain fake: no BandExchange
		_, err := h.s.ApplyPatch(ctx, PatchRequest{ExchangeBands: &yes})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
	})

	t.Run("false is refused, since there is nothing to swap back", func(t *testing.T) {
		r := newDualRig()
		h := newHarnessRig(t, r, nil)
		_, err := h.s.ApplyPatch(ctx, PatchRequest{ExchangeBands: &no})
		if !errors.Is(err, ErrUnsupported) {
			t.Fatalf("err = %v, want ErrUnsupported", err)
		}
		if r.exchanges != 0 {
			t.Error("a false request still exchanged the receivers")
		}
	})

	t.Run("applied before the rest of the request", func(t *testing.T) {
		r := newDualRig()
		h := newHarnessRig(t, r, nil).start(t)
		hz, mode := uint64(144300000), radio.ModeUSB

		// "Bring the other band over and tune it", which is the whole reason an
		// operator reaches for this. The exchange has to happen first or the
		// frequency lands on the band being swapped away.
		if _, err := h.s.ApplyPatch(ctx, PatchRequest{
			ExchangeBands: &yes, Frequency: &hz, Mode: &mode,
		}); err != nil {
			t.Fatalf("ApplyPatch: %v", err)
		}
		if r.exchanges != 1 {
			t.Fatalf("exchanges = %d, want 1", r.exchanges)
		}
		log := r.setLog()
		if len(log) == 0 || log[0] != "exchange_bands" {
			t.Fatalf("set log = %v, want the exchange first", log)
		}
	})
}

// A mode change that says nothing about data mode carries the rig's current
// flag forward, which is right when both spellings exist and wrong when the
// target mode has no data code: the operator asked for CW and got a refusal
// naming a data mode they never mentioned. On an FT-857D left in PKT — that
// radio's FM-with-data — every bare mode change was refused, including the one
// to DIG, its other data mode, so there was no way out of PKT at all.
func TestPatchCarriedDataModeFallsBackToPlainMode(t *testing.T) {
	h := newHarness(t, nil).start(t)
	ctx := context.Background()

	usb, yes := radio.ModeUSB, true
	if _, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &usb, DataMode: &yes}); err != nil {
		t.Fatalf("into USB with data mode: %v", err)
	}
	if st := h.s.State(); !st.DataMode {
		t.Fatalf("data mode = false after asking for it, state %+v", st)
	}

	h.rig.setNoDataMode(radio.ModeCW)

	cw := radio.ModeCW
	st, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &cw})
	if err != nil {
		t.Fatalf("bare mode change out of a data mode: %v", err)
	}
	if st.Mode != radio.ModeCW {
		t.Errorf("mode = %s, want CW", st.Mode)
	}
	if st.DataMode {
		t.Error("data mode still on after falling back to the plain mode")
	}

	// Both halves should be on the wire log, in order: the carried-forward
	// attempt, then the retry that dropped the flag nobody asked for.
	want := []string{"mode=CW data=true", "mode=CW data=false"}
	got := h.rig.setLog()
	if len(got) < len(want) {
		t.Fatalf("set log = %v, want it to end with %v", got, want)
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("set log tail = %v, want %v", tail, want)
		}
	}
}

// The fallback covers a flag that was carried forward, never one that was
// asked for. A client that spells out data_mode: true on a mode with no data
// code gets the refusal, because that request really was made — and it must not
// be quietly turned into the plain mode.
func TestPatchExplicitDataModeIsStillRefused(t *testing.T) {
	h := newHarness(t, nil).start(t)
	ctx := context.Background()

	h.rig.setNoDataMode(radio.ModeCW)

	before := len(h.rig.setLog())
	cw, yes := radio.ModeCW, true
	if _, err := h.s.ApplyPatch(ctx, PatchRequest{Mode: &cw, DataMode: &yes}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if n := len(h.rig.setLog()) - before; n != 1 {
		t.Errorf("%d set commands issued, want 1: an explicit request must not be retried", n)
	}
}
