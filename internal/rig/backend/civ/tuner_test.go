package civ

import (
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Which radios have 1C 01 as an antenna tuner, and which must never be told
// they do.
//
// This started as a default of true for every modern Icom and was caught on an
// IC-9700: it advertised a tuner it does not have, answered NG to the poll
// every slow tick, and would have shown an operator a Tune button that could
// only fail.
func TestTunerIsPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
		why   string
	}{
		{"ic-7610", true, "its table has 1C 01 Antenna tuner; exercised on the air"},
		{"ic-703", true, "1C 01 set/read antenna tuner condition"},
		// Every other HF set whose reference has been read.
		{"ic-7300", true, "1C 01 00=off, 01=on, 02=to tuning"},
		{"ic-7300mk2", true, "1C 01 tuning status of the internal antenna tuner"},
		{"ic-7600", true, "1C 01 antenna tuner OFF (through) / ON / Tuning"},
		{"ic-7700", true, "1C 01 antenna tuner OFF (through) / ON / Tuning"},
		{"ic-7760", true, "1C 01 antenna tuner setting 00=OFF, 01=ON, 02=Tune"},
		{"ic-7850", true, "1C 01 antenna tuner OFF (through) / ON / to tuning"},
		{"ic-9100", true, "1C 01, with 02 worded Manual tuning selection"},
		// A VHF/UHF set with nothing to match: its 1C table is 00, 02, 03 and
		// omits 01 entirely. Confirmed against the radio.
		{"ic-9700", false, "no 1C 01 row at all"},
		{"ic-905", false, "no 1C 01 row; 144 MHz to 10 GHz"},
		{"ic-910h", false, "its table ends at 1C 00"},
		// The dangerous one: there 1C 01 is PTT.
		{"ic-718", false, "1C 01 is the transmitter on this radio"},
		// No 1C whatsoever.
		{"ic-706mkiig", false, "no transmitter or tuner command in its table"},
		// Not transcribed, so not claimed.
		{"generic", false, "an unidentified radio claims nothing"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			cfg := &config.CIV{Model: tt.model}
			if tt.model == "generic" {
				cfg.RigAddress = 0x94
			}
			r, err := New(&config.Radio{CIV: cfg})
			if err != nil {
				t.Fatal(err)
			}
			c := r.Caps()
			if c.TunerControl != tt.want || c.TunerTune != tt.want {
				t.Errorf("tuner caps = {control %v, tune %v}, want %v — %s",
					c.TunerControl, c.TunerTune, tt.want, tt.why)
			}
		})
	}
}

// A radio without one refuses both halves, and nothing reaches the wire. The
// refusal has to be legible: an operator seeing it should learn that the radio
// has no tuner, not read a frame dump.
func TestTunerRefusedWithoutOne(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-9700"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &captureConn{}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"SetTuner", func() error { return r.SetTuner(t.Context(), c, true) }},
		{"StartTune", func() error { return r.StartTune(t.Context(), c) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, backend.ErrUnsupported) {
				t.Errorf("error = %v, want ErrUnsupported so the API answers 422", err)
			}
		})
	}
	if len(c.sent) != 0 {
		t.Errorf("sent %d frames to a radio with no tuner", len(c.sent))
	}
}

// And it is not polled either, or every slow tick would carry a rejection for a
// value that can never arrive.
func TestTunerNotPolledWithoutOne(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-9700"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &pollConn{}
	if err := r.Poll(t.Context(), c, backend.PollSlow); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	for _, f := range c.sent {
		if len(f) >= 6 && f[4] == cmdTransceiver && f[5] == subTuner {
			t.Fatal("polled the antenna tuner on a radio that has none")
		}
	}
}

// On a radio that does have one, 1C 01 is the tuner and 1C 00 is still PTT.
// The two readings of one command stay apart because the model says which
// applies — the same mechanism that keeps the IC-718's PTT out of the tuner.
func TestTunerAndPTTShareOneCommand(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
	if err != nil {
		t.Fatal(err)
	}

	tuner := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, subTuner, tunerOn, 0xFD}
	u, err := r.Decode(tuner)
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.Tuner == nil || *u.Patch.Tuner != radio.TunerOn {
		t.Errorf("1C 01 decoded to tuner %v, want on", u.Patch.Tuner)
	}
	if u.Patch.PTT != nil {
		t.Error("1C 01 was also decoded as PTT")
	}

	ptt := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x00, 0x01, 0xFD}
	u, err = r.Decode(ptt)
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.PTT == nil || !*u.Patch.PTT {
		t.Errorf("1C 00 decoded to PTT %v, want true", u.Patch.PTT)
	}
	if u.Patch.Tuner != nil {
		t.Error("1C 00 was also decoded as the tuner")
	}
}

// The IC-718 is the one where getting this wrong keys a transmitter: there
// 1C 01 is PTT, so a tuner frame must decode as PTT and a tune must be refused.
func TestIC718TunerFrameIsPTT(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-718"}})
	if err != nil {
		t.Fatal(err)
	}
	f := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x01, 0x01, 0xFD}
	u, err := r.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.PTT == nil || !*u.Patch.PTT {
		t.Errorf("1C 01 on an IC-718 decoded to PTT %v, want true", u.Patch.PTT)
	}
	if u.Patch.Tuner != nil {
		t.Errorf("1C 01 on an IC-718 decoded as an antenna tuner: %v", *u.Patch.Tuner)
	}
}
