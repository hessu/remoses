package civ

import (
	"bytes"
	"context"
	"math"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// pollConn records what a poll sends and satisfies every read, so that a test
// sees the whole request list rather than stopping at the first one. The shared
// captureConn answers everything with an ACK, which ends a poll immediately.
type pollConn struct{ sent [][]byte }

func (c *pollConn) Do(_ context.Context, req []byte, keys ...backend.Key) (backend.Update, error) {
	c.sent = append(c.sent, bytes.Clone(req))
	key := backend.Key("")
	if len(keys) > 0 {
		key = keys[0]
	}
	return backend.Update{Key: key, OK: true}, nil
}

func (c *pollConn) Send(_ context.Context, req []byte) error {
	c.sent = append(c.sent, bytes.Clone(req))
	return nil
}

// meterFrame builds a 15 xx answer carrying a 0000-0255 BCD value.
func meterFrame(r *Rig, sub byte, v int) []byte {
	b := encodeBCD2(v)
	return []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdMeter, sub, b[0], b[1], 0xFD}
}

func TestDecodeTXMeters(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("forward power against the model's own full scale", func(t *testing.T) {
		u, err := r.Decode(meterFrame(r, subPOMeter, 143))
		if err != nil {
			t.Fatal(err)
		}
		if u.Key != KeyPOMeter {
			t.Errorf("key = %q, want %q", u.Key, KeyPOMeter)
		}
		if u.Patch.PowerMeter == nil {
			t.Fatal("no power meter published")
		}
		// The reference calls 143 the 50% point on this radio.
		if got := u.Patch.PowerMeter; got.Raw != 143 || got.Scale != 255 {
			t.Errorf("power meter = %+v, want raw 143 of 255", got)
		}
	})

	// "0000=Minimum to 0120=Maximum": publishing ALC against 255 would show a
	// meter at full deflection as 47%.
	t.Run("ALC runs to 120, not 255", func(t *testing.T) {
		u, err := r.Decode(meterFrame(r, subALCMeter, 120))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.ALC == nil || u.Patch.ALC.Scale != alcScale {
			t.Fatalf("ALC = %+v, want scale %d", u.Patch.ALC, alcScale)
		}
		if f := u.Patch.ALC.Fraction(); f != 1 {
			t.Errorf("ALC at maximum reads %v of full scale, want 1", f)
		}
	})

	t.Run("SWR carries both the bar and the ratio", func(t *testing.T) {
		u, err := r.Decode(meterFrame(r, subSWRMeter, 80))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.SWR == nil || u.Patch.SWR.Raw != 80 {
			t.Fatalf("SWR = %+v, want raw 80", u.Patch.SWR)
		}
		// 0080 is SWR 2.0 in both references.
		if u.Patch.SWRRatio == nil || math.Abs(*u.Patch.SWRRatio-2.0) > 0.001 {
			t.Errorf("SWR ratio = %v, want 2.0", u.Patch.SWRRatio)
		}
	})
}

// The IC-9700's PO meter reaches 100% at 213, not the IC-7610's 255. Against
// the wrong scale a radio at full power reads 84%.
func TestPOScaleIsPerModel(t *testing.T) {
	for _, tc := range []struct {
		model string
		want  int
	}{
		{"ic-7610", 255},
		{"ic-9700", 213},
	} {
		r, err := New(&config.Radio{CIV: &config.CIV{Model: tc.model}})
		if err != nil {
			t.Fatal(err)
		}
		u, err := r.Decode(meterFrame(r, subPOMeter, 100))
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.PowerMeter == nil || u.Patch.PowerMeter.Scale != tc.want {
			t.Errorf("%s power meter = %+v, want scale %d", tc.model, u.Patch.PowerMeter, tc.want)
		}
	}
}

// The IC-703's table names the three meters and calibrates none of them, so it
// publishes deflections and no ratio. Deriving one from another radio's
// calibration would be remoses inventing a figure about somebody's antenna.
func TestSWRRatioOnlyWhereCalibrated(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-703"}})
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.Decode(meterFrame(r, subSWRMeter, 80))
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.SWR == nil {
		t.Error("no SWR bar published; the meter itself is in this radio's table")
	}
	if u.Patch.SWRRatio != nil {
		t.Errorf("SWR ratio = %v, want none: this radio's reference calibrates nothing",
			*u.Patch.SWRRatio)
	}
}

func TestSWRRatioInterpolation(t *testing.T) {
	tests := []struct {
		raw  int
		want float64
		ok   bool
	}{
		{0, 1.0, true},   // documented
		{48, 1.5, true},  // documented
		{80, 2.0, true},  // documented
		{120, 3.0, true}, // documented
		{24, 1.25, true}, // halfway between the first two
		{64, 1.75, true}, // halfway between the second and third
		// Beyond the last documented point the curve is unknown. An SWR that
		// high is a fault either way, and "7.4:1" would be a precise-looking
		// number remoses made up.
		{121, 0, false},
		{255, 0, false},
	}
	for _, tt := range tests {
		got, ok := swrRatio(tt.raw)
		if ok != tt.ok {
			t.Errorf("swrRatio(%d) ok = %v, want %v", tt.raw, ok, tt.ok)
			continue
		}
		if ok && math.Abs(got-tt.want) > 0.001 {
			t.Errorf("swrRatio(%d) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

// In receive the three meters read zero and mean nothing, so asking for them
// would spend three transactions a tick publishing zeroes a client could not
// tell from a real reading.
func TestTXMetersPolledOnlyWhileTransmitting(t *testing.T) {
	txSubs := map[byte]bool{subPOMeter: true, subSWRMeter: true, subALCMeter: true}

	askedFor := func(t *testing.T, transmitting bool) bool {
		t.Helper()
		r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
		if err != nil {
			t.Fatal(err)
		}
		r.transmitting.Store(transmitting)
		c := &pollConn{}
		if err := r.Poll(t.Context(), c, backend.PollFast); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		for _, f := range c.sent {
			if len(f) >= 6 && f[4] == cmdMeter && txSubs[f[5]] {
				return true
			}
		}
		return false
	}

	if askedFor(t, false) {
		t.Error("polled the transmit meters in receive")
	}
	if !askedFor(t, true) {
		t.Error("did not poll the transmit meters while transmitting")
	}
}

// The flag comes from decoding PTT, so a transmission started at the radio's
// own switch is picked up as readily as one remoses keyed.
func TestTransmittingFollowsDecodedPTT(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}
	on := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x00, 0x01, 0xFD}
	if _, err := r.Decode(on); err != nil {
		t.Fatal(err)
	}
	if !r.transmitting.Load() {
		t.Error("a PTT-on frame did not set the transmitting hint")
	}
	off := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x00, 0x00, 0xFD}
	if _, err := r.Decode(off); err != nil {
		t.Fatal(err)
	}
	if r.transmitting.Load() {
		t.Error("a PTT-off frame did not clear the transmitting hint")
	}
}
