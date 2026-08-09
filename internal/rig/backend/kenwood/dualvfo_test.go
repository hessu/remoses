package kenwood

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Caps used to advertise VFO A and B on every model here while nothing could
// address either, so a client that believed the list got a 422 for every
// request naming one. These tests pin both halves: what the list says, and that
// the commands behind it exist.

func TestVFOCapsMatchTheCommandSet(t *testing.T) {
	tests := []struct {
		model     string
		wantVFOs  []radio.VFO
		wantSplit bool
		// wantAddressing is "" where there is nothing to describe.
		wantAddressing string
	}{
		{"ts480", []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}, true, "named"},
		{"ts590s", []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}, true, "named"},
		{"ts590sg", []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}, true, "named"},
		{"ts890s", []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}, true, "named"},
		{"generic", []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}, true, "named"},
		// The TS-990S's FA and FB are the Main and Sub bands, not VFO A and B.
		{"ts990s", []radio.VFO{radio.VFOCurrent}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := k.Caps()
			if len(c.VFOs) != len(tt.wantVFOs) {
				t.Fatalf("VFOs = %v, want %v", c.VFOs, tt.wantVFOs)
			}
			for i, v := range tt.wantVFOs {
				if c.VFOs[i] != v {
					t.Fatalf("VFOs = %v, want %v", c.VFOs, tt.wantVFOs)
				}
			}
			if c.Split != tt.wantSplit {
				t.Errorf("Split = %v, want %v", c.Split, tt.wantSplit)
			}
			if c.VFOAddressing != tt.wantAddressing {
				t.Errorf("VFOAddressing = %q, want %q", c.VFOAddressing, tt.wantAddressing)
			}
			// No model here has either of these, and saying otherwise would be
			// the same class of promise this test exists to prevent.
			if c.PerVFOMode {
				t.Error("PerVFOMode true: MD applies to the selected VFO only")
			}
			if c.DualWatch {
				t.Error("DualWatch true: these radios receive on one VFO at a time")
			}
		})
	}
}

// A radio that advertises A and B must implement the interface the session
// reaches for, or every request naming a VFO is refused before it reaches the
// wire.
func TestAdvertisedVFOsAreAddressable(t *testing.T) {
	for _, name := range []string{"ts480", "ts590s", "ts590sg", "ts890s", "generic"} {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			if _, ok := backend.Rig(k).(backend.DualVFO); !ok {
				t.Fatal("advertises VFO A and B but does not implement backend.DualVFO")
			}
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.SetVFOFrequency(context.Background(), c, radio.VFOB, 7_050_000); err != nil {
				t.Fatalf("SetVFOFrequency(B): %v", err)
			}
			c.wantSent(t, "FB00007050000;", "FB;")
		})
	}
}

// And one that does not advertise them must refuse, rather than sending FB and
// moving the Sub band of a radio the client thinks has two VFOs.
func TestTS990RefusesVFOAddressing(t *testing.T) {
	k := newModelRig(t, "ts990s")
	c := newTestConn(t, k, answersFor(k.profile))
	err := k.SetVFOFrequency(context.Background(), c, radio.VFOB, 7_050_000)
	if err == nil {
		t.Fatal("set VFO B on a radio whose FB is the Sub band")
	}
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("error = %v, want backend.ErrUnsupported", err)
	}
	c.wantSent(t)
}

// Split is not a flag on this protocol: FR selects the receive VFO and forces
// simplex, FT selects the transmit VFO and forces split.
func TestSetSplit(t *testing.T) {
	tests := []struct {
		name string
		rx   radio.VFO
		on   bool
		want []string
	}{
		{"on from VFO A transmits on B", radio.VFOA, true, []string{"FT1;", "FR;", "FT;"}},
		{"on from VFO B transmits on A", radio.VFOB, true, []string{"FT0;", "FR;", "FT;"}},
		// Off names the receive VFO, since FR is what returns it to simplex.
		{"off on VFO A", radio.VFOA, false, []string{"FR0;", "FR;", "FT;"}},
		{"off on VFO B", radio.VFOB, false, []string{"FR1;", "FR;", "FT;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, "ts590sg")
			k.receiveVFO.Store(tt.rx)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.SetSplit(context.Background(), c, tt.on); err != nil {
				t.Fatalf("SetSplit: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

// Guessing which VFO transmits would put RF on the wrong frequency, so an
// unknown receive VFO is read rather than assumed.
func TestSetSplitReadsTheReceiveVFOWhenUnknown(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	c := newTestConn(t, k, answersFor(k.profile)) // FR; answers FR0
	if err := k.SetSplit(context.Background(), c, true); err != nil {
		t.Fatalf("SetSplit: %v", err)
	}
	c.wantSent(t, "FR;", "FT1;", "FR;", "FT;")
}

func TestSetVFOModeIsRefused(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	c := newTestConn(t, k, answersFor(k.profile))
	err := k.SetVFOMode(context.Background(), c, radio.VFOB, radio.ModeUSB, false, 0)
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported: MD addresses the selected VFO only", err)
	}
	c.wantSent(t)
}

func TestSetDualWatchIsRefused(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	c := newTestConn(t, k, answersFor(k.profile))
	err := k.SetDualWatch(context.Background(), c, true)
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("error = %v, want ErrUnsupported", err)
	}
	c.wantSent(t)
}

// Each frequency lands in its own VFO's slot, and State.Frequency follows only
// the VFO being received — otherwise polling the parked VFO would overwrite the
// operating frequency with one nobody is listening to.
func TestVFOFrequencyDecode(t *testing.T) {
	tests := []struct {
		name  string
		rx    radio.VFO
		frame string
		// wantCurrent is whether State.Frequency should be updated.
		wantCurrent bool
		wantA       bool
		wantB       bool
	}{
		{"FA while receiving A", radio.VFOA, "FA00014025000", true, true, false},
		{"FB while receiving A", radio.VFOA, "FB00007050000", false, false, true},
		{"FA while receiving B", radio.VFOB, "FA00014025000", false, true, false},
		{"FB while receiving B", radio.VFOB, "FB00007050000", true, false, true},
		// Before anything has said which VFO is selected, FA is taken as the
		// operating one, which is what this backend always assumed.
		{"FA before FR has answered", radio.VFOCurrent, "FA00014025000", true, true, false},
		{"FB before FR has answered", radio.VFOCurrent, "FB00007050000", false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, "ts590sg")
			if tt.rx != radio.VFOCurrent {
				k.receiveVFO.Store(tt.rx)
			}
			u, err := k.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := u.Patch.Frequency != nil; got != tt.wantCurrent {
				t.Errorf("State.Frequency updated = %v, want %v", got, tt.wantCurrent)
			}
			if got := u.Patch.VFOA != nil; got != tt.wantA {
				t.Errorf("VFOA updated = %v, want %v", got, tt.wantA)
			}
			if got := u.Patch.VFOB != nil; got != tt.wantB {
				t.Errorf("VFOB updated = %v, want %v", got, tt.wantB)
			}
		})
	}
}

// On the TS-990S the same frames must not be published as VFO A and B.
func TestTS990DoesNotPublishBandsAsVFOs(t *testing.T) {
	k := newModelRig(t, "ts990s")
	for _, f := range []string{"FA00014025000", "FB00007050000"} {
		u, err := k.Decode([]byte(f))
		if err != nil {
			t.Fatalf("Decode(%q): %v", f, err)
		}
		if u.Patch.VFOA != nil || u.Patch.VFOB != nil {
			t.Errorf("Decode(%q) published a VFO on a radio whose FA/FB are bands", f)
		}
	}
}

// Split is the relationship between FR and FT, so neither answer alone settles
// it and both have to be seen.
func TestSplitFromFRAndFT(t *testing.T) {
	tests := []struct {
		name      string
		frames    []string
		wantKnown bool
		wantSplit bool
	}{
		{"FR alone says nothing", []string{"FR0"}, false, false},
		{"FR then FT, same VFO, is simplex", []string{"FR0", "FT0"}, true, false},
		{"FR then FT, different VFOs, is split", []string{"FR0", "FT1"}, true, true},
		{"receiving B and transmitting B is simplex", []string{"FR1", "FT1"}, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, "ts590sg")
			var last backend.Update
			for _, f := range tt.frames {
				u, err := k.Decode([]byte(f))
				if err != nil {
					t.Fatalf("Decode(%q): %v", f, err)
				}
				last = u
			}
			if got := last.Patch.Split != nil; got != tt.wantKnown {
				t.Fatalf("split published = %v, want %v", got, tt.wantKnown)
			}
			if tt.wantKnown && *last.Patch.Split != tt.wantSplit {
				t.Errorf("split = %v, want %v", *last.Patch.Split, tt.wantSplit)
			}
		})
	}
}

// An FR answer also says which VFO the radio is on, which is what makes
// State.VFO meaningful and what SetSplit needs.
func TestFRPublishesTheSelectedVFO(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	u, err := k.Decode([]byte("FR1"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.VFO == nil || *u.Patch.VFO != radio.VFOB {
		t.Fatalf("VFO = %v, want B", u.Patch.VFO)
	}
	if k.rxVFO() != radio.VFOB {
		t.Errorf("rxVFO() = %v, want B", k.rxVFO())
	}

	// Memory is neither VFO. Claiming the radio is on one it has left would be
	// worse than leaving the last known reading in place.
	u, err = k.Decode([]byte("FR2"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.VFO != nil {
		t.Errorf("published VFO %v for a radio on a memory channel", *u.Patch.VFO)
	}
	if k.rxVFO() != radio.VFOB {
		t.Errorf("rxVFO() = %v, want the last VFO reading to survive memory mode", k.rxVFO())
	}
}

// IF carries both fields on every fast poll, on the models that have IF at all.
// Neither was read before, which is why split was never published on a radio
// that reports it twice a second.
func TestIFCarriesSelectedVFOAndSplit(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	// P10 is the FR/FT function and P12 is simplex/split; build the frame from
	// the sample by replacing those two fields.
	f := []byte(sampleIF)
	f[ifFunc] = '1'  // receiving on VFO B
	f[ifSplit] = '1' // and transmitting on the other one

	u, err := k.Decode(f)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.VFO == nil || *u.Patch.VFO != radio.VFOB {
		t.Fatalf("VFO = %v, want B", u.Patch.VFO)
	}
	if u.Patch.Split == nil || !*u.Patch.Split {
		t.Fatalf("Split = %v, want true", u.Patch.Split)
	}
	// The transmit VFO is recorded to match, so that a later FR or FT answer
	// does not contradict what IF just said.
	if tx, _ := k.transmitVFO.Load().(radio.VFO); tx != radio.VFOA {
		t.Errorf("transmit VFO = %v, want A (the other one)", tx)
	}
}

// The discrete fast poll has to follow the operator onto VFO B, or it keeps
// republishing the frequency of the VFO they are not listening to.
func TestFastPollFollowsTheReceiveVFO(t *testing.T) {
	k := newRig(t, 2, false) // bulk poll off, so the discrete path is used
	k.receiveVFO.Store(radio.VFOB)
	c := newTestConn(t, k, initAnswers())
	if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	c.wantSent(t, "FB;", "MD;", "SM0;")
}
