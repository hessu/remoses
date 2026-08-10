package civ

import (
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// One command, two vocabularies. Most of the family reads 16 47 as
// "00=OFF, 01=semi, 02=full"; the IC-910H's table says only "0=OFF; 1=ON".
func TestBreakInStylePerModel(t *testing.T) {
	for _, tt := range []struct {
		model string
		want  BreakInStyle
	}{
		{"ic-7610", BreakInSemiFull},
		{"ic-9700", BreakInSemiFull},
		{"ic-703", BreakInSemiFull},
		{"ic-706mkiig", BreakInSemiFull},
		{"ic-910h", BreakInOnOff},
		// No 16 47 at all.
		{"ic-718", BreakInNone},
		{"ic-706", BreakInNone},
	} {
		t.Run(tt.model, func(t *testing.T) {
			r, err := New(&config.Radio{CIV: &config.CIV{Model: tt.model}})
			if err != nil {
				t.Fatal(err)
			}
			if r.model.BreakIn != tt.want {
				t.Errorf("break-in style = %v, want %v", r.model.BreakIn, tt.want)
			}
			if got := r.Caps().BreakInControl; got != (tt.want != BreakInNone) {
				t.Errorf("BreakInControl = %v with style %v", got, tt.want)
			}
		})
	}
}

// A radio that says only "on" publishes "on". Reading its 01 as semi would
// invent a distinction it declined to make, and the difference is audible.
func TestTwoValueBreakInDecode(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-910h"}})
	if err != nil {
		t.Fatal(err)
	}
	for b, want := range map[byte]radio.BreakIn{
		0x00: radio.BreakInOff,
		0x01: radio.BreakInOn,
	} {
		f := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdFunc, subBreakIn, b, 0xFD}
		u, err := r.Decode(f)
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.BreakIn == nil || *u.Patch.BreakIn != want {
			t.Errorf("16 47 %#x decoded to %v, want %s", b, u.Patch.BreakIn, want)
		}
	}

	// 02 is not a value this radio has. Publishing it as "full" would report a
	// state the rig cannot be in.
	f := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdFunc, subBreakIn, 0x02, 0xFD}
	u, err := r.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.BreakIn != nil {
		t.Errorf("16 47 02 decoded to %v on a radio whose table stops at 01", *u.Patch.BreakIn)
	}
}

// Setting semi or full on such a radio is not an error: both mean "on" there,
// so both are honoured with its single on. Sending 02 would be an out-of-range
// parameter for a distinction the radio does not have.
func TestTwoValueBreakInSet(t *testing.T) {
	for _, tt := range []struct {
		v    radio.BreakIn
		want byte
	}{
		{radio.BreakInOff, 0x00},
		{radio.BreakInOn, 0x01},
		{radio.BreakInSemi, 0x01},
		{radio.BreakInFull, 0x01},
	} {
		t.Run(string(tt.v), func(t *testing.T) {
			r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-910h"}})
			if err != nil {
				t.Fatal(err)
			}
			c := &captureConn{}
			if err := r.SetBreakIn(t.Context(), c, tt.v); err != nil {
				t.Fatalf("SetBreakIn(%s): %v", tt.v, err)
			}
			f := c.last()
			if len(f) != 8 {
				t.Fatalf("frame % X has unexpected length", f)
			}
			if f[4] != cmdFunc || f[5] != subBreakIn || f[6] != tt.want {
				t.Errorf("sent % X, want 16 47 %02X", f, tt.want)
			}
		})
	}
}

// The three-value radios keep all three, and a bare "on" becomes semi there —
// the radio distinguishes them and the caller did not, so the quieter one is
// chosen, as cw.break_in's default does.
func TestThreeValueBreakInSet(t *testing.T) {
	for _, tt := range []struct {
		v    radio.BreakIn
		want byte
	}{
		{radio.BreakInOff, 0x00},
		{radio.BreakInSemi, 0x01},
		{radio.BreakInFull, 0x02},
		{radio.BreakInOn, 0x01},
	} {
		t.Run(string(tt.v), func(t *testing.T) {
			r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
			if err != nil {
				t.Fatal(err)
			}
			c := &captureConn{}
			if err := r.SetBreakIn(t.Context(), c, tt.v); err != nil {
				t.Fatalf("SetBreakIn(%s): %v", tt.v, err)
			}
			if f := c.last(); f[6] != tt.want {
				t.Errorf("sent % X, want data %02X", f, tt.want)
			}
		})
	}
}

// A radio with no 16 47 refuses instead; see TestBreakInRefusedWithoutTheCommand
// in breakin_test.go.

// "on" transmits, which is the only thing the CW guard asks — so a message
// queued on an IC-910H with break-in on is not refused.
func TestBreakInOnTransmits(t *testing.T) {
	if !radio.BreakInOn.Transmits() {
		t.Error("BreakInOn does not transmit; CW would be refused on an IC-910H")
	}
}
