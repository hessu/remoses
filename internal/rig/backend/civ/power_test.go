package civ

import (
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The wake-up preamble is a duration expressed in bytes, so it is per baud
// rate. Sent short, the radio misses the frame and stays off — which looks
// exactly like a radio that is not there.
func TestWakePreamble(t *testing.T) {
	// The reference's own table.
	for baud, want := range map[int]int{
		115200: 150,
		57600:  75,
		38400:  50,
		19200:  25,
		9600:   13,
		4800:   7,
	} {
		if got := wakePreamble(baud); got != want {
			t.Errorf("wakePreamble(%d) = %d, want %d", baud, got, want)
		}
	}

	// A rate between two tabulated ones rounds UP to the longer preamble: too
	// many FEs cost milliseconds, too few cost a radio that does not wake.
	if got, want := wakePreamble(14400), 13; got != want {
		t.Errorf("wakePreamble(14400) = %d, want %d — the next longer count", got, want)
	}
	// Above the table, the longest.
	if got := wakePreamble(230400); got != 150 {
		t.Errorf("wakePreamble(230400) = %d, want 150", got)
	}
}

func TestPowerOnSendsPreambleThenCommand(t *testing.T) {
	r, err := New(&config.Radio{
		CIV:  &config.CIV{Model: "ic-7610"},
		Port: config.Port{Baud: 38400},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &captureConn{}
	if err := r.PowerOn(t.Context(), c); err != nil {
		t.Fatalf("PowerOn: %v", err)
	}

	f := c.last()
	// 50 FEs for 38400, then FE FE <to> <from> 18 01 FD.
	want := wakePreamble(38400)
	n := 0
	for n < len(f) && f[n] == preamble {
		n++
	}
	// The frame's own two opening FEs are part of that run, so the count seen
	// is the preamble plus two.
	if n != want+2 {
		t.Errorf("leading FE run = %d, want %d (a %d-byte preamble plus the frame's two)",
			n, want+2, want)
	}
	tail := f[len(f)-5:]
	if tail[2] != cmdPower || tail[3] != subPowerOn || tail[4] != 0xFD {
		t.Errorf("frame ends % X, want ... 18 01 FD", tail)
	}
}

func TestPowerOffSendsTheCommand(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, deep := range []bool{false, true} {
		c := &captureConn{}
		if err := r.PowerOff(t.Context(), c, deep); err != nil {
			t.Fatalf("PowerOff(deep=%v): %v", deep, err)
		}
		f := c.last()
		// This family documents one off, so deep changes nothing rather than
		// being refused: 18 00 is both.
		if len(f) != 7 || f[4] != cmdPower || f[5] != subPowerOff {
			t.Errorf("PowerOff(deep=%v) sent % X, want 18 00", deep, f)
		}
	}
}

// A radio with no command 18 refuses, and sends nothing.
func TestPowerRefusedWithoutTheCommand(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-703"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &captureConn{}
	if err := r.PowerOn(t.Context(), c); !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("PowerOn = %v, want ErrUnsupported", err)
	}
	if err := r.PowerOff(t.Context(), c, false); !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("PowerOff = %v, want ErrUnsupported", err)
	}
	if len(c.sent) != 0 {
		t.Error("sent a power command to a radio that has none")
	}
	if r.Caps().PowerSwitch {
		t.Error("Caps claims a power switch on a radio with no command 18")
	}
}
