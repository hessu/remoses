package civ

import (
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

func TestDecode(t *testing.T) {
	r := testRig(t)

	tests := []struct {
		name    string
		frame   []byte
		wantKey backend.Key
		wantOK  bool
		check   func(t *testing.T, p radio.Patch)
	}{
		{
			name:    "ok acknowledgement",
			frame:   fromRig(codeOK),
			wantKey: KeyAck,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "negative acknowledgement",
			frame:   fromRig(codeNG),
			wantKey: KeyAck,
			wantOK:  false,
			check:   wantEmpty,
		},
		{
			name:    "frequency",
			frame:   fromRig(cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00),
			wantKey: KeyFrequency,
			wantOK:  true,
			check:   wantFrequency(14_025_000),
		},
		{
			name:    "frequency above 100 MHz",
			frame:   fromRig(cmdReadFreq, 0x00, 0x00, 0x50, 0x45, 0x01),
			wantKey: KeyFrequency,
			wantOK:  true,
			check:   wantFrequency(145_500_000),
		},
		{
			name: "frequency transceive broadcast",
			// Command 00 is the rig volunteering the same value command 03
			// answers. It must not resolve a pending read, hence the empty key.
			frame:   broadcast(cmdXcvFreq, 0x00, 0x50, 0x02, 0x14, 0x00),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantFrequency(14_025_000),
		},
		{
			name:    "frequency with a bad BCD digit",
			frame:   fromRig(cmdReadFreq, 0x0F, 0x50, 0x02, 0x14, 0x00),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "frequency of the wrong length",
			frame:   fromRig(cmdReadFreq, 0x00, 0x50, 0x02),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "mode with filter",
			frame:   fromRig(cmdReadMode, 0x03, 0x02),
			wantKey: KeyMode,
			wantOK:  true,
			check:   wantMode(radio.ModeCW, 2),
		},
		{
			name:    "mode without filter",
			frame:   fromRig(cmdReadMode, 0x01),
			wantKey: KeyMode,
			wantOK:  true,
			check:   wantMode(radio.ModeUSB, 0),
		},
		{
			name:    "mode transceive broadcast",
			frame:   broadcast(cmdXcvMode, 0x07, 0x01),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantMode(radio.ModeCWR, 1),
		},
		{
			name:    "unknown mode byte",
			frame:   fromRig(cmdReadMode, 0x0B),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "out of range filter byte is ignored",
			frame:   fromRig(cmdReadMode, 0x03, 0x07),
			wantKey: KeyMode,
			wantOK:  true,
			check:   wantMode(radio.ModeCW, 0),
		},
		{
			name:    "rf power",
			frame:   fromRig(cmdLevel, subRFPower, 0x01, 0x28),
			wantKey: KeyPower,
			wantOK:  true,
			check:   wantPower(128),
		},
		{
			name:    "rf power maximum",
			frame:   fromRig(cmdLevel, subRFPower, 0x02, 0x55),
			wantKey: KeyPower,
			wantOK:  true,
			check:   wantPower(255),
		},
		{
			name:    "keyer speed resolves the request but patches nothing",
			frame:   fromRig(cmdLevel, subKeyerSpeed, 0x01, 0x28),
			wantKey: KeyKeyerSpeed,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "unhandled level sub-command",
			frame:   fromRig(cmdLevel, 0x0B, 0x01, 0x28),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "s-meter",
			frame:   fromRig(cmdMeter, subSMeter, 0x01, 0x20),
			wantKey: KeySMeter,
			wantOK:  true,
			check:   wantSMeter(120),
		},
		{
			name:    "squelch status is not a meter",
			frame:   fromRig(cmdMeter, 0x01, 0x01),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		// 1A 03 is deliberately absent from this table. Its answer is an index
		// whose width depends on the mode, so what it decodes to depends on the
		// mode frames decoded before it — and every case here shares one Rig.
		// TestFilterWidthNeedsTheMode covers it against an explicit mode.
		{
			name:    "data mode on with filter",
			frame:   fromRig(cmdMisc, subDataMode, 0x01, 0x02),
			wantKey: KeyDataMode,
			wantOK:  true,
			check:   wantDataMode(true, 2),
		},
		{
			name:    "data mode off",
			frame:   fromRig(cmdMisc, subDataMode, 0x00, 0x00),
			wantKey: KeyDataMode,
			wantOK:  true,
			check:   wantDataMode(false, 0),
		},
		{
			name:    "ptt receiving",
			frame:   fromRig(cmdTransceiver, subPTT, 0x00),
			wantKey: KeyPTT,
			wantOK:  true,
			check:   wantPTT(false),
		},
		{
			name:    "ptt transmitting",
			frame:   fromRig(cmdTransceiver, subPTT, 0x01),
			wantKey: KeyPTT,
			wantOK:  true,
			check:   wantPTT(true),
		},
		{
			name:    "ptt with a nonsense value",
			frame:   fromRig(cmdTransceiver, subPTT, 0x02),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "antenna tuner status is not implemented",
			frame:   fromRig(cmdTransceiver, 0x01, 0x01),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "unimplemented command",
			frame:   fromRig(0x27, 0x00, 0x01, 0x02),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "our own frame echoed back",
			frame:   r.frame(cmdReadFreq),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "echoed set frequency carries no state",
			frame:   r.frame(cmdSetFreq, 0x00, 0x50, 0x02, 0x14, 0x00),
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "another controller on the bus",
			frame:   []byte{preamble, preamble, 0xE1, DefaultRigAddress, cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00, eom},
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "another rig on the bus",
			frame:   []byte{preamble, preamble, DefaultControllerAddress, 0x94, cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00, eom},
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "malformed frame",
			frame:   []byte{0x01, 0x02, 0x03},
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
		{
			name:    "empty frame",
			frame:   nil,
			wantKey: backend.KeyUnsolicited,
			wantOK:  true,
			check:   wantEmpty,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := r.Decode(tc.frame)
			if err != nil {
				t.Fatalf("Decode returned an error: %v", err)
			}
			if u.Key != tc.wantKey {
				t.Errorf("key = %q, want %q", u.Key, tc.wantKey)
			}
			if u.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v", u.OK, tc.wantOK)
			}
			if string(u.Raw) != string(tc.frame) {
				t.Errorf("Raw = % X, want % X", u.Raw, tc.frame)
			}
			tc.check(t, u.Patch)
		})
	}
}

// TestDecodeNeverErrors is the contract the session relies on: an unknown frame
// must be ignored quietly rather than taken as a protocol failure.
func TestDecodeNeverErrors(t *testing.T) {
	r := testRig(t)
	for cmd := 0; cmd <= 0xFF; cmd++ {
		for _, body := range [][]byte{nil, {0x00}, {0x00, 0x01}, {0xFF, 0xFF, 0xFF}} {
			f := fromRig(byte(cmd), body...)
			u, err := r.Decode(f)
			if err != nil {
				t.Fatalf("Decode(% X): %v", f, err)
			}
			if u.Key == KeyAck && byte(cmd) == codeNG && u.OK {
				t.Errorf("NG frame % X decoded as OK", f)
			}
		}
	}
}

func TestDecodeWithNonDefaultAddresses(t *testing.T) {
	r := &Rig{rigAddr: 0x94, ctrlAddr: 0xE1}
	// A frame from the configured rig to the configured controller decodes.
	u, _ := r.Decode([]byte{preamble, preamble, 0xE1, 0x94, cmdTransceiver, subPTT, 0x01, eom})
	if u.Key != KeyPTT || u.Patch.PTT == nil || !*u.Patch.PTT {
		t.Errorf("addressed frame decoded as %+v", u)
	}
	// The default addresses are now somebody else's traffic.
	u, _ = r.Decode(fromRig(cmdTransceiver, subPTT, 0x01))
	if u.Key != backend.KeyUnsolicited || !u.Patch.Empty() {
		t.Errorf("foreign frame decoded as %+v", u)
	}
}

func wantEmpty(t *testing.T, p radio.Patch) {
	t.Helper()
	if !p.Empty() {
		t.Errorf("patch = %+v, want empty", p)
	}
}

func wantFrequency(hz uint64) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.Frequency == nil {
			t.Fatalf("patch has no frequency: %+v", p)
		}
		if *p.Frequency != hz {
			t.Errorf("frequency = %d, want %d", *p.Frequency, hz)
		}
	}
}

// wantMode checks the mode and, when slot is non-zero, the filter slot. A zero
// slot means the patch must not mention one.
func wantMode(m radio.Mode, slot int) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.Mode == nil {
			t.Fatalf("patch has no mode: %+v", p)
		}
		if *p.Mode != m {
			t.Errorf("mode = %s, want %s", *p.Mode, m)
		}
		switch {
		case slot == 0 && p.FilterSlot != nil:
			t.Errorf("filter slot = %d, want none", *p.FilterSlot)
		case slot != 0 && p.FilterSlot == nil:
			t.Errorf("filter slot missing, want %d", slot)
		case slot != 0 && *p.FilterSlot != slot:
			t.Errorf("filter slot = %d, want %d", *p.FilterSlot, slot)
		}
	}
}

func wantPower(native int) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.Power == nil {
			t.Fatalf("patch has no power: %+v", p)
		}
		if p.Power.Native != native {
			t.Errorf("native power = %d, want %d", p.Power.Native, native)
		}
		if p.Power.Watts != nil {
			t.Errorf("power reported %v watts; the CI-V scale has no watt meaning", *p.Power.Watts)
		}
		wantPct := float64(native) / levelMax * 100
		if p.Power.Pct != wantPct {
			t.Errorf("power pct = %v, want %v", p.Power.Pct, wantPct)
		}
	}
}

func wantSMeter(raw int) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.SMeter == nil {
			t.Fatalf("patch has no s-meter: %+v", p)
		}
		if p.SMeter.Raw != raw || p.SMeter.Scale != sMeterScale {
			t.Errorf("s-meter = %+v, want raw %d scale %d", *p.SMeter, raw, sMeterScale)
		}
		if p.SMeter.S != nil {
			t.Errorf("s-meter reported S %v; no calibration table exists", *p.SMeter.S)
		}
	}
}

func wantDataMode(on bool, slot int) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.DataMode == nil {
			t.Fatalf("patch has no data mode: %+v", p)
		}
		if *p.DataMode != on {
			t.Errorf("data mode = %v, want %v", *p.DataMode, on)
		}
		switch {
		case slot == 0 && p.FilterSlot != nil:
			t.Errorf("filter slot = %d, want none", *p.FilterSlot)
		case slot != 0 && (p.FilterSlot == nil || *p.FilterSlot != slot):
			t.Errorf("filter slot = %v, want %d", p.FilterSlot, slot)
		}
	}
}

func wantPTT(on bool) func(*testing.T, radio.Patch) {
	return func(t *testing.T, p radio.Patch) {
		t.Helper()
		if p.PTT == nil {
			t.Fatalf("patch has no PTT: %+v", p)
		}
		if *p.PTT != on {
			t.Errorf("PTT = %v, want %v", *p.PTT, on)
		}
	}
}

// TestFilterWidthNeedsTheMode covers the whole point of keeping a mode hint: a
// 1A 03 answer is an index into one of four tables, and only the mode says
// which.
//
// The CW case is not invented. An IC-7610 on 18.073 MHz answered 1A 03 with
// exactly this byte, and its front panel agreed with the width below.
func TestFilterWidthNeedsTheMode(t *testing.T) {
	cases := []struct {
		name string
		mode radio.Mode
		idx  byte // BCD, as it arrives
		want int  // 0 means "publish nothing"
	}{
		{"CW index 16 is 1200 Hz", radio.ModeCW, 0x16, 1200},
		{"SSB index 31 is 2700 Hz", radio.ModeUSB, 0x31, 2700},
		{"CW index 0 is 50 Hz", radio.ModeCW, 0x00, 50},
		{"CW index 9 is 500 Hz", radio.ModeCW, 0x09, 500},
		{"CW index 10 is 600 Hz", radio.ModeCW, 0x10, 600},
		{"CW index 40 is 3600 Hz", radio.ModeCW, 0x40, 3600},
		{"AM index 0 is 200 Hz", radio.ModeAM, 0x00, 200},
		{"AM index 49 is 10 kHz", radio.ModeAM, 0x49, 10000},
		{"RTTY index 31 is 2700 Hz", radio.ModeFSK, 0x31, 2700},

		// Off the end of the mode's own column: the premise is wrong, so
		// nothing is published rather than a number that would look real.
		{"RTTY stops at index 31", radio.ModeFSK, 0x32, 0},
		{"SSB stops at index 40", radio.ModeUSB, 0x41, 0},
		// Modes with fixed filters have no row in the table at all.
		{"FM has no width table", radio.ModeFM, 0x16, 0},
		{"an unknown mode has no width table", radio.ModeUnknown, 0x16, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := testRig(t)
			// What decodeMode would have stored from the last 04 reply.
			r.mode.Store(uint32(tc.mode))

			u, err := r.Decode(fromRig(cmdMisc, subFilterWidth, tc.idx))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if u.Key != KeyFilterWidth {
				t.Fatalf("key = %q, want %q", u.Key, KeyFilterWidth)
			}
			switch {
			case tc.want == 0:
				if u.Patch.PassbandHz != nil {
					t.Errorf("passband = %d Hz, want none published", *u.Patch.PassbandHz)
				}
			case u.Patch.PassbandHz == nil:
				t.Errorf("no passband published, want %d Hz", tc.want)
			case *u.Patch.PassbandHz != tc.want:
				t.Errorf("passband = %d Hz, want %d", *u.Patch.PassbandHz, tc.want)
			}
		})
	}
}

// TestFilterWidthTablesAgree is the reason the inverse was written beside the
// forward table rather than anywhere else. Two transcriptions of one table that
// disagreed would report a passband the rig is not using.
func TestFilterWidthTablesAgree(t *testing.T) {
	modes := []radio.Mode{radio.ModeUSB, radio.ModeCW, radio.ModeFSK, radio.ModeAM}
	for _, m := range modes {
		for idx := range 50 {
			hz, ok := filterWidthHz(m, idx)
			if !ok {
				continue // off this mode's column
			}
			back, err := filterWidthIndex(m, hz)
			if err != nil {
				t.Errorf("%s: index %d is %d Hz, which does not convert back: %v", m, idx, hz, err)
				continue
			}
			if back != idx {
				t.Errorf("%s: index %d is %d Hz, which converts back to index %d", m, idx, hz, back)
			}
		}
	}
}

// TestModeHintIsStoredByDecode proves the hint is fed by the ordinary mode read
// rather than needing a caller to remember to set it.
func TestModeHintIsStoredByDecode(t *testing.T) {
	r := testRig(t)
	if _, err := r.Decode(fromRig(cmdReadMode, 0x03, 0x01)); err != nil { // CW, FIL1
		t.Fatalf("Decode: %v", err)
	}
	if got := radio.Mode(r.mode.Load()); got != radio.ModeCW {
		t.Fatalf("mode hint = %v after a CW report, want CW", got)
	}
	u, err := r.Decode(fromRig(cmdMisc, subFilterWidth, 0x16))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Patch.PassbandHz == nil || *u.Patch.PassbandHz != 1200 {
		t.Fatalf("passband = %v, want 1200 Hz", u.Patch.PassbandHz)
	}
}
