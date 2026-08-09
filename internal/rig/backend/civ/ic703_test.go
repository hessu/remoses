package civ

import (
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// The IC-703's command table is the older shape, and three of its differences
// are commands this backend would otherwise send blind. Each is pinned here
// against section 11 of its instruction manual, the only CI-V documentation it
// has.
func TestIC703Differences(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-703"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("factory address is 0x68", func(t *testing.T) {
		if r.rigAddr != 0x68 {
			t.Errorf("rig address = %#x, want 0x68", r.rigAddr)
		}
	})

	// 1A 03 exists on this radio and is NOT the filter width: it takes a
	// two-byte Set-mode item index, so 1A 0301 is the confirmation beep and
	// 1A 0305 the CW carrier point. Reading it as a passband would ask the
	// radio about its beeper; writing one would change it.
	t.Run("no filter width, because 1A 03 is the Set-mode menu", func(t *testing.T) {
		c := r.Caps()
		if c.FilterWidth {
			t.Error("advertises a filter width; 1A 03 is a Set-mode item index here")
		}
		if c.FilterSlots != 0 {
			t.Errorf("FilterSlots = %d, want 0: the mode command carries no filter byte", c.FilterSlots)
		}
	})

	// No command 17 at all, so Morse has to be keyed on a control line.
	t.Run("no CAT CW buffer", func(t *testing.T) {
		if got := r.Caps().CWMethod; got != radio.CWNone {
			t.Errorf("CWMethod = %q, want %q: its table has no command 17", got, radio.CWNone)
		}
	})

	// 14 0C is the keyer speed and the Quick set menu gives 6 to 60 wpm, not
	// the 6-48 of the modern family.
	t.Run("keyer runs to 60 wpm", func(t *testing.T) {
		if got := r.Caps().CWMaxWPM; got != 60 {
			t.Errorf("CWMaxWPM = %d, want 60", got)
		}
	})

	// 16 47 is in its table, and an operator keying a control line still needs
	// the rig in break-in to hear themselves through it.
	t.Run("break-in is controllable", func(t *testing.T) {
		if !r.Caps().BreakInControl {
			t.Error("no break-in control; 16 47 is in this radio's table")
		}
	})

	// The table lists 0F 00 and 0F 01 to turn split off and on and shows no
	// read form, while writing "Set/read" against 1C 00 in the same table. A
	// setting remoses could write and never read back is the failure this
	// backend keeps hitting, so it is not claimed.
	t.Run("no split, because no read form is documented", func(t *testing.T) {
		if r.Caps().Split {
			t.Error("advertises split; its table documents only the two set forms of 0F")
		}
	})

	// No 25/26, so no VFO addressing — but 07 00 and 07 01 do select A and B.
	t.Run("one VFO addressable", func(t *testing.T) {
		c := r.Caps()
		if len(c.VFOs) != 1 || c.VFOs[0] != radio.VFOCurrent {
			t.Errorf("VFOs = %v, want [current]: it has no 25 or 26", c.VFOs)
		}
	})

	t.Run("modes are the family set with no PSK or D-STAR", func(t *testing.T) {
		want := []radio.Mode{
			radio.ModeLSB, radio.ModeUSB, radio.ModeAM, radio.ModeCW,
			radio.ModeFSK, radio.ModeFM, radio.ModeCWR, radio.ModeFSKR,
		}
		got := r.Caps().Modes
		if len(got) != len(want) {
			t.Fatalf("Modes = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Modes = %v, want %v", got, want)
			}
		}
	})
}

// Data mode is on 1A 04 here, not the 1A 06 the rest of the family uses, and is
// the on/off flag alone. Sending the modern two-byte form would hand its parser
// a parameter it is not expecting.
func TestIC703DataModeIsOn1A04(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-703"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("set sends 1A 04 with one data byte", func(t *testing.T) {
		c := &captureConn{}
		if err := r.SetMode(t.Context(), c, radio.ModeUSB, true); err != nil {
			t.Fatalf("SetMode: %v", err)
		}
		var found []byte
		for _, f := range c.sent {
			if len(f) > 5 && f[4] == cmdMisc {
				found = f
			}
		}
		if found == nil {
			t.Fatal("no 1A frame sent for the data-mode change")
		}
		// FE FE to from 1A 04 <on> FD
		if len(found) != 8 {
			t.Fatalf("frame % X has unexpected length; want the one-byte form", found)
		}
		if found[5] != 0x04 {
			t.Errorf("sub-command %#x, want 0x04; 1A 06 is not in this radio's table", found[5])
		}
		if found[6] != 0x01 {
			t.Errorf("data byte %#x, want 0x01", found[6])
		}
	})

	t.Run("decodes 1A 04", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdMisc, 0x04, 0x01, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if u.Key != KeyDataMode || u.Patch.DataMode == nil || !*u.Patch.DataMode {
			t.Errorf("decode of % X gave key %q patch %+v", frame, u.Key, u.Patch)
		}
	})

	// And must not read the modern sub-command as data mode: 1A 06 is not in
	// this radio's table, so a frame carrying it means something else.
	t.Run("1A 06 is not data mode here", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdMisc, subDataMode, 0x01, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.DataMode != nil {
			t.Errorf("decoded 1A 06 as data mode on a radio whose data mode is 1A 04")
		}
	})
}

// The modern family keeps the two-byte form, filter byte and all. This is the
// half of the change that could regress every other radio.
func TestModernDataModeStillCarriesTheFilter(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}
	f := r.dataModeFrame(0x01, 2)
	// FE FE to from 1A 06 <on> <filter> FD
	if len(f) != 9 {
		t.Fatalf("frame % X has unexpected length; want the two-byte form", f)
	}
	if f[5] != subDataMode || f[6] != 0x01 || f[7] != 0x02 {
		t.Errorf("frame % X, want 1A 06 01 02", f)
	}
}
