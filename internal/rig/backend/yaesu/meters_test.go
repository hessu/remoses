package yaesu

import (
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// RM answers are not the same length on both generations — the newer one
// appends three fixed digits — so the decoder reads the meter and the three
// digits after it and ignores the rest. Getting that wrong would silently drop
// every reading on one generation or the other.
func TestDecodeRMBothAnswerLengths(t *testing.T) {
	tests := []struct {
		name  string
		frame string
		want  func(radio.Patch) *radio.Meter
		raw   int
	}{
		// The FT-950 generation: RM<meter><nnn>;
		{"power, short form", "RM5100", func(p radio.Patch) *radio.Meter { return p.PowerMeter }, 100},
		{"SWR, short form", "RM6032", func(p radio.Patch) *radio.Meter { return p.SWR }, 32},
		{"ALC, short form", "RM4200", func(p radio.Patch) *radio.Meter { return p.ALC }, 200},
		// The FT-710 generation, with the fixed P3 field after the value.
		{"power, long form", "RM5100000", func(p radio.Patch) *radio.Meter { return p.PowerMeter }, 100},
		{"SWR, long form", "RM6032000", func(p radio.Patch) *radio.Meter { return p.SWR }, 32},
		{"ALC, long form", "RM4200000", func(p radio.Patch) *radio.Meter { return p.ALC }, 200},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, "ft-710")
			u, err := y.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatal(err)
			}
			if u.Key != keyRM {
				t.Errorf("key = %q, want %q", u.Key, keyRM)
			}
			m := tt.want(u.Patch)
			if m == nil {
				t.Fatalf("nothing published for %q", tt.frame)
			}
			if m.Raw != tt.raw || m.Scale != rmScale {
				t.Errorf("meter = %+v, want raw %d of %d", m, tt.raw, rmScale)
			}
			// No Yaesu reference calibrates the SWR meter, so no ratio is
			// derived from a deflection.
			if u.Patch.SWRRatio != nil {
				t.Errorf("SWR ratio = %v, want none", *u.Patch.SWRRatio)
			}
		})
	}
}

// The S-meter already arrives on SM every fast poll. Publishing RM1 as well
// would let two commands disagree about one value.
func TestRMSMeterIsNotRepublished(t *testing.T) {
	y := newModelRig(t, "ft-710")
	u, err := y.Decode([]byte("RM1100"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Key != keyRM {
		t.Errorf("key = %q, want %q so the read completes", u.Key, keyRM)
	}
	if !u.Patch.Empty() {
		t.Errorf("patch = %+v, want nothing: SM is where the S-meter comes from", u.Patch)
	}
}

// In receive the three meters read zero, so they are not asked for at all.
func TestYaesuTXMeterReadsOnlyWhileTransmitting(t *testing.T) {
	y := newModelRig(t, "ft-710")

	if got := y.txMeterReads(); len(got) != 0 {
		t.Errorf("txMeterReads() = %v in receive, want none", got)
	}

	y.transmitting.Store(true)
	got := y.txMeterReads()
	if len(got) != 3 {
		t.Fatalf("txMeterReads() = %v, want power, SWR and ALC", got)
	}
	want := []string{reqRMPower, reqRMSWR, reqRMALC}
	for i, r := range got {
		if r.req != want[i] {
			t.Errorf("read %d = %q, want %q", i, r.req, want[i])
		}
	}
}

// TestFTdx9000HasNoTransmitMeters. RM is the only command any radio here has
// for forward power, SWR and ALC, and the FTdx9000's command list has no row for
// it — nor for MS, the front-panel meter selector, so there is nothing to read
// instead. Its SM reads the S-meter and stops there.
//
// The cost of getting this wrong is three transactions per fast tick for the
// whole of every transmission, each answered with silence and each burning the
// session's full per-command timeout, in exchange for three bars that would
// never move.
func TestFTdx9000HasNoTransmitMeters(t *testing.T) {
	y := newModelRig(t, "ftdx9000")
	y.transmitting.Store(true)
	if got := y.txMeterReads(); len(got) != 0 {
		t.Errorf("txMeterReads() = %v while transmitting, want none: the FTdx9000 has no RM", got)
	}
	// And a frame arriving under those letters is not turned into a reading.
	u := mustDecode(t, y, "RM5100")
	if !u.Patch.Empty() {
		t.Errorf("RM5100 decoded to %+v on a radio with no RM command", u.Patch)
	}
	// Every other model reads all three.
	for _, name := range ModelNames() {
		if name == "ftdx9000" {
			continue
		}
		other := newModelRig(t, name)
		other.transmitting.Store(true)
		if got := other.txMeterReads(); len(got) != 3 {
			t.Errorf("%s: txMeterReads() = %v, want power, SWR and ALC", name, got)
		}
	}
}

// The flag follows the TX answer, so a transmission started with a foot switch
// or MOX is metered exactly like one remoses keyed.
func TestYaesuTransmittingFollowsTX(t *testing.T) {
	y := newModelRig(t, "ft-710")
	for _, tc := range []struct {
		frame string
		want  bool
	}{
		{"TX0", false},
		{"TX1", true}, // keyed by CAT
		{"TX2", true}, // keyed at the rig
	} {
		if _, err := y.Decode([]byte(tc.frame)); err != nil {
			t.Fatal(err)
		}
		if got := y.transmitting.Load(); got != tc.want {
			t.Errorf("%s left transmitting = %v, want %v", tc.frame, got, tc.want)
		}
	}
}
