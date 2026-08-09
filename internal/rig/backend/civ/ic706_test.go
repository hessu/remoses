package civ

import (
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The IC-706 family is the oldest here and the first to have no transmitter
// command and no power level at all. Those two absences are new capabilities
// rather than new commands, so they are pinned per model.
func TestIC706FamilyCaps(t *testing.T) {
	tests := []struct {
		model      string
		addr       byte
		wantMeter  int
		wantBreak  bool
		wantCWMode radio.CWMethod
	}{
		{"ic-706", 0x48, 0, false, radio.CWNone},
		{"ic-706mkii", 0x4E, 0, false, radio.CWNone},
		// The MKIIG is the only one of the three with a readable meter and a
		// break-in command.
		{"ic-706mkiig", 0x58, sMeterScale, true, radio.CWNone},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			r, err := New(&config.Radio{CIV: &config.CIV{Model: tt.model}})
			if err != nil {
				t.Fatal(err)
			}
			if r.rigAddr != tt.addr {
				t.Errorf("rig address = %#x, want %#x", r.rigAddr, tt.addr)
			}
			c := r.Caps()

			// No 1C at any sub-command, and no 14. A client reads these rather
			// than offering a PTT button that cannot key.
			if c.PTTControl {
				t.Error("claims PTT control; this family has no transmitter command")
			}
			if c.PowerControl {
				t.Error("claims power control; this family has no 14 command")
			}
			if c.SMeterScale != tt.wantMeter {
				t.Errorf("SMeterScale = %d, want %d", c.SMeterScale, tt.wantMeter)
			}
			if c.BreakInControl != tt.wantBreak {
				t.Errorf("BreakInControl = %v, want %v", c.BreakInControl, tt.wantBreak)
			}
			if c.CWMethod != tt.wantCWMode {
				t.Errorf("CWMethod = %q, want %q", c.CWMethod, tt.wantCWMode)
			}
			// No 1A at all: neither a filter width nor a data mode.
			if c.FilterWidth {
				t.Error("claims a filter width; this family has no 1A 03")
			}
			// Three filter selections, and split is not claimed for the same
			// reason as the IC-703's: 0F has no documented read form.
			if c.FilterSlots != 3 {
				t.Errorf("FilterSlots = %d, want 3", c.FilterSlots)
			}
			if c.Split {
				t.Error("claims split; only the two set forms of 0F are documented")
			}
		})
	}
}

// Refusing at the backend as well as the session, because the session is not
// the only caller: nothing should be able to send a command this radio has
// never had.
func TestIC706FamilyRefusesPTTAndPower(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-706mkiig"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &captureConn{}

	err = r.SetPTT(t.Context(), c, true)
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("SetPTT = %v, want ErrUnsupported", err)
	}
	pct := 50.0
	err = r.SetPower(t.Context(), c, radio.PowerSet{Pct: &pct})
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("SetPower = %v, want ErrUnsupported", err)
	}
	// And neither reached the wire.
	if len(c.sent) != 0 {
		t.Errorf("sent %d frames for commands this radio does not have", len(c.sent))
	}
}

// Command 06 runs 00 to 06 here: WFM is added and CW-R and RTTY-R are absent,
// so the whole table is overridden. A byte that means WFM here means nothing on
// the rest of the family, and 07 would decode as CW-R against the family table.
func TestIC706FamilyModeCodes(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-706"}})
	if err != nil {
		t.Fatal(err)
	}

	for b, want := range map[byte]radio.Mode{
		0x00: radio.ModeLSB,
		0x01: radio.ModeUSB,
		0x02: radio.ModeAM,
		0x03: radio.ModeCW,
		0x04: radio.ModeFSK,
		0x05: radio.ModeFM,
		0x06: radio.ModeWFM,
	} {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, b, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatalf("Decode(%#x): %v", b, err)
		}
		if u.Patch.Mode == nil || *u.Patch.Mode != want {
			t.Errorf("mode byte %#x decoded to %v, want %s", b, u.Patch.Mode, want)
		}
	}

	// 07 is CW-R on the rest of the family and nothing here, so it must not be
	// decoded as a mode this radio does not have.
	frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, 0x07, 0xFD}
	u, err := r.Decode(frame)
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.Mode != nil {
		t.Errorf("decoded mode byte 07 as %s; this radio's table stops at 06", *u.Patch.Mode)
	}

	if got := r.Caps().Modes; len(got) != 7 {
		t.Errorf("Modes = %v, want the seven this radio has", got)
	}
}

// The filter byte counts from zero here — 00 wide, 01 normal, 02 narrow — where
// the modern family numbers FIL1..FIL3 from one. Being wrong selects the
// neighbouring filter on every mode change, which the rig accepts without
// complaint.
func TestIC706FamilyFilterByteIsZeroBased(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-706"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("encode", func(t *testing.T) {
		for slot, want := range map[int]byte{1: 0x00, 2: 0x01, 3: 0x02} {
			if got := r.model.filterByte(slot); got != want {
				t.Errorf("filterByte(%d) = %#x, want %#x", slot, got, want)
			}
		}
	})

	t.Run("decode", func(t *testing.T) {
		for b, want := range map[byte]int{0x00: 1, 0x01: 2, 0x02: 3} {
			got, ok := r.model.filterSlot(b)
			if !ok || got != want {
				t.Errorf("filterSlot(%#x) = %d,%v, want %d,true", b, got, ok, want)
			}
		}
		// One past the end: with three slots counted from zero, 03 is not one.
		if _, ok := r.model.filterSlot(0x03); ok {
			t.Error("filterSlot(0x03) accepted; this radio has three slots, 00 to 02")
		}
	})

	// The whole point: a mode answer carrying 00 is FIL1, not slot 0.
	t.Run("a mode answer publishes the slot", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, 0x03, 0x02, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.FilterSlot == nil || *u.Patch.FilterSlot != 3 {
			t.Errorf("filter slot = %v, want 3 (narrow, wire 02)", u.Patch.FilterSlot)
		}
	})
}

// The modern family must keep counting from one. This is the half of the
// change that could quietly move every other radio's filter.
func TestModernFilterByteIsStillOneBased(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}
	for slot, want := range map[int]byte{1: 0x01, 2: 0x02, 3: 0x03} {
		if got := r.model.filterByte(slot); got != want {
			t.Errorf("filterByte(%d) = %#x, want %#x", slot, got, want)
		}
	}
	if got, ok := r.model.filterSlot(0x02); !ok || got != 2 {
		t.Errorf("filterSlot(0x02) = %d,%v, want 2,true", got, ok)
	}
	// Zero is not a slot on these radios.
	if _, ok := r.model.filterSlot(0x00); ok {
		t.Error("filterSlot(0x00) accepted on a radio whose filters are FIL1..FIL3")
	}
}

// Neither PTT nor power nor (before the MKIIG) the meter may be asked for, or
// every connect and every poll would carry rejections for values that can never
// arrive.
func TestIC706FamilyPollsOnlyWhatItHas(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-706"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &captureConn{}
	// Poll errors because the fake answers nothing; what matters is what it
	// tried to send before giving up.
	_ = r.Poll(t.Context(), c, backend.PollFast)

	for _, f := range c.sent {
		if len(f) < 5 {
			continue
		}
		switch f[4] {
		case cmdTransceiver:
			t.Error("polled PTT on a radio with no transmitter command")
		case cmdMeter:
			t.Error("polled the S-meter on a radio with no meter")
		case cmdLevel:
			t.Error("polled power on a radio with no power command")
		}
	}
}
