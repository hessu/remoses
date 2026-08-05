package rigctld

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// sampleDumpState is a \dump_state body.
//
// It is hand-constructed, not captured from a running daemon: no Hamlib
// installation was available here. Every line follows the printf formats in
// declare_proto_rig(dump_state) in tests/rigctl_parse.c literally — including
// the detail that the frequency bounds come out as "%lf" and therefore carry
// six decimal places, while the sentinel rows are written separately as plain
// integers — and the field order matches the one Hamlib's own netrigctl client
// reads back in rigs/dummy/netrigctl.c.
//
// The values describe a plausible HF transceiver: an Icom IC-7300 (Hamlib model
// 3073) with an S-meter, a settable RF power level and a CW keyer speed.
const sampleDumpState = `1
3073
0
30000.000000 74800000.000000 0x1dbf -1 -1 0x3 0xf
108000.000000 174000000.000000 0x1dbf -1 -1 0x3 0xf
0 0 0 0 0 0 0
1800000.000000 1999999.000000 0x1dbf 2000 100000 0x3 0x1
3500000.000000 3999999.000000 0x1dbf 2000 100000 0x3 0x1
0 0 0 0 0 0 0
0x1dbf 1
0x1dbf 10
0x1dbf 100
0 0
0x82 2400
0x82 1800
0x2 500
0x2 250
0 0
9999
9999
0
0
10 20
12
0x40000c1b
0x40000c1b
0x40005000
0x5000
0x0
0x0`

// sampleDumpStateProto1 adds the "key=value" tail that a daemon emits once some
// client has run \chk_vfo. remoses never runs it — see dumpState.settings — so
// this is the case where another program on the same daemon has.
const sampleDumpStateProto1 = sampleDumpState + `
vfo_ops=0x8fb
ptt_type=0x1
targetable_vfo=0x7
has_set_vfo=1
has_get_vfo=1
has_set_freq=1
has_get_freq=1
timeout=1000
rig_model=3073
rigctld_version=Hamlib 4.6
level_gran=0=0,0,0;12=0,1,0.001;14=4,48,1;30=0,0,0;
parm_gran=0=0,0,0;
rig_model=3073
hamlib_version=Hamlib 4.6
done`

// sampleDumpCaps is an excerpt of a \dump_caps body, cut down to the lines this
// backend reads plus enough of its neighbours to prove the rest is skipped. The
// labels and the tab separator are from tests/dumpcaps.c.
const sampleDumpCaps = `Caps dump for model:	3073
Model name:	IC-7300
Mfg name:	Icom
Backend version:	20231029.0
Backend copyright:	LGPL
Rig type:	Transceiver
PTT type:	Rig capable
Port type:	RS-232
	1800000 Hz - 1999999 Hz
		VFO list: VFOA VFOB
		Mode list: AM CW USB LSB RTTY FM
Get level: RFPOWER(0..1) KEYSPD(4..48) STRENGTH(0..0)
Set level: RFPOWER(0..1) KEYSPD(4..48)
Can set Frequency:	Y
Can get Frequency:	Y
Can set Mode:	Y
Can get Mode:	Y
Can set PTT:	Y
Can get PTT:	Y
Can send DTMF:	Y
Can recv DTMF:	N
Can send Morse:	Y
Can stop Morse:	Y
Can wait Morse:	Y
Can Scan:	Y

Overall backend warnings: 0  `

func lines(s string) []string { return strings.Split(s, "\n") }

func TestParseDumpState(t *testing.T) {
	st := parseDumpState(lines(sampleDumpState))

	if !st.complete {
		t.Fatal("the positional block did not parse to the end")
	}
	if st.protocol != 1 {
		t.Errorf("protocol = %d, want 1", st.protocol)
	}
	if st.model != 3073 {
		t.Errorf("model = %d, want 3073", st.model)
	}
	if len(st.rxRanges) != 2 || len(st.txRanges) != 2 {
		t.Fatalf("ranges = %d rx, %d tx; want 2 and 2", len(st.rxRanges), len(st.txRanges))
	}
	if st.rxRanges[0].startHz != 30000 || st.rxRanges[0].endHz != 74800000 {
		t.Errorf("first rx range = %v..%v Hz", st.rxRanges[0].startHz, st.rxRanges[0].endHz)
	}
	if st.txRanges[0].highPowerMW != 100000 {
		t.Errorf("first tx range high power = %d mW, want 100000", st.txRanges[0].highPowerMW)
	}
	if len(st.filters) != 4 {
		t.Errorf("filters = %d, want 4", len(st.filters))
	}
	if st.getLevel != 0x40005000 || st.setLevel != 0x5000 {
		t.Errorf("level masks = %#x / %#x, want 0x40005000 / 0x5000", st.getLevel, st.setLevel)
	}

	for _, tc := range []struct {
		name     string
		get, set bool
	}{
		{levelRFPOWER, true, true},
		{levelKEYSPD, true, true},
		{levelSTRENGTH, true, false},
	} {
		if got := st.hasGetLevel(tc.name); got != tc.get {
			t.Errorf("hasGetLevel(%s) = %v, want %v", tc.name, got, tc.get)
		}
		if got := st.hasSetLevel(tc.name); got != tc.set {
			t.Errorf("hasSetLevel(%s) = %v, want %v", tc.name, got, tc.set)
		}
	}

	want := "AM CW USB LSB RTTY FM CWR RTTYR PKTLSB PKTUSB PKTFM"
	if got := strings.Join(st.modeTokens(), " "); got != want {
		t.Errorf("modes = %q, want %q", got, want)
	}

	// Without the \chk_vfo tail there is no level_gran to read.
	if _, _, ok := st.keyerRange(); ok {
		t.Error("keyerRange found a range in a dump with no protocol-1 tail")
	}
}

func TestParseDumpStateProtocol1Tail(t *testing.T) {
	st := parseDumpState(lines(sampleDumpStateProto1))
	if !st.complete {
		t.Fatal("the positional block did not parse to the end")
	}
	if got := st.settings["rigctld_version"]; got != "Hamlib 4.6" {
		t.Errorf("rigctld_version = %q", got)
	}
	// "done" terminates the tail and is not itself a setting.
	if _, ok := st.settings["done"]; ok {
		t.Error("the terminator was stored as a setting")
	}

	lo, hi, ok := st.keyerRange()
	if !ok || lo != 4 || hi != 48 {
		t.Errorf("keyerRange = (%d, %d, %v), want (4, 48, true)", lo, hi, ok)
	}
}

func TestParseDumpStateKeyerRangeEdges(t *testing.T) {
	tests := []struct {
		name string
		gran string
		ok   bool
	}{
		// Plenty of Hamlib backends leave level_gran zeroed, which means
		// "not stated" and must not be taken as a 0..0 wpm keyer.
		{name: "zeroed entry", gran: "14=0,0,0;", ok: false},
		{name: "no KEYSPD entry", gran: "12=0,1,0.001;", ok: false},
		{name: "inverted range", gran: "14=48,4,1;", ok: false},
		{name: "unparsable", gran: "14=x,y,z;", ok: false},
		{name: "usable", gran: "14=5,55,1;", ok: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := parseDumpState(lines(sampleDumpState + "\nlevel_gran=" + tc.gran + "\ndone"))
			if _, _, ok := st.keyerRange(); ok != tc.ok {
				t.Errorf("keyerRange ok = %v, want %v", ok, tc.ok)
			}
		})
	}
}

// TestParseDumpStateTruncated proves a dump that stops early keeps what it read
// and reports the rest as absent, rather than throwing away the mode list.
func TestParseDumpStateTruncated(t *testing.T) {
	all := lines(sampleDumpState)
	st := parseDumpState(all[:9]) // through the tx range sentinel

	if st.complete {
		t.Error("complete is set on a truncated dump")
	}
	if st.model != 3073 {
		t.Errorf("model = %d, want 3073", st.model)
	}
	if len(st.modeTokens()) == 0 {
		t.Error("the modes read before the break were discarded")
	}
	// The level masks were never reached, so nothing may be claimed for them.
	if st.hasGetLevel(levelSTRENGTH) {
		t.Error("a level was claimed from a dump that never reached the masks")
	}
}

// TestParseDumpStateGarbage walks a break through every stage of the positional
// block. Each case must stop cleanly rather than panic or claim a complete
// parse: this backend talks to daemons of unknown vintage.
func TestParseDumpStateGarbage(t *testing.T) {
	full := lines(sampleDumpState)
	corrupt := func(n int, with string) []string {
		out := append([]string(nil), full...)
		out[n] = with
		return out
	}

	tests := []struct {
		name string
		body []string
	}{
		{name: "nothing at all", body: nil},
		{name: "one empty line", body: []string{""}},
		{name: "no protocol version", body: []string{"not a number"}},
		{name: "truncated before the model", body: full[:1]},
		{name: "a short rx range row", body: corrupt(3, "30000 74800000")},
		{name: "an unparsable rx range bound", body: corrupt(3, "x y 0x1 0 0 0 0")},
		{name: "an unparsable tx range bound", body: corrupt(6, "x y 0x1 0 0 0 0")},
		{name: "a short tuning step row", body: corrupt(9, "0x1dbf")},
		{name: "an unparsable tuning step", body: corrupt(9, "0xzz nope")},
		{name: "an unparsable filter width", body: corrupt(13, "0x82 wide")},
		{name: "a missing max_rit", body: corrupt(17, "not a number")},
		{name: "an unparsable level mask", body: corrupt(24, "not hex")},
		{name: "truncated in the middle of the masks", body: full[:23]},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := parseDumpState(tc.body)
			if st.complete {
				t.Errorf("claimed a complete parse of %q", tc.body)
			}
		})
	}
}

func TestParseDumpCaps(t *testing.T) {
	dc := parseDumpCaps(lines(sampleDumpCaps))

	if !dc.present {
		t.Fatal("present is false after a successful parse")
	}
	if dc.modelLabel() != "Icom IC-7300" {
		t.Errorf("modelLabel = %q, want %q", dc.modelLabel(), "Icom IC-7300")
	}
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"sendMorse", dc.sendMorse, true},
		{"stopMorse", dc.stopMorse, true},
		{"setMode", dc.setMode, true},
		{"getMode", dc.getMode, true},
		{"setPTT", dc.setPTT, true},
		{"getPTT", dc.getPTT, true},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

func TestParseDumpCapsMorseAbsent(t *testing.T) {
	body := lines(strings.ReplaceAll(sampleDumpCaps, "Can send Morse:\tY", "Can send Morse:\tN"))
	if dc := parseDumpCaps(body); dc.sendMorse {
		t.Error("sendMorse is true for a rig whose dump says N")
	}
}

// TestParseDumpCapsEmulated proves 'E' counts as available: from the client's
// side, a capability Hamlib emulates out of other calls is a capability.
func TestParseDumpCapsEmulated(t *testing.T) {
	dc := parseDumpCaps([]string{"Can set Mode:\tE", "Can get Mode:\tN"})
	if !dc.setMode || dc.getMode {
		t.Errorf("setMode/getMode = %v/%v, want true/false", dc.setMode, dc.getMode)
	}
}

func TestBuildCaps(t *testing.T) {
	st := parseDumpState(lines(sampleDumpStateProto1))
	dc := parseDumpCaps(lines(sampleDumpCaps))
	c := buildCaps(&st, &dc, 4, 48)

	wantModes := []radio.Mode{
		radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
		radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
	}
	if len(c.Modes) != len(wantModes) {
		t.Fatalf("modes = %v, want %v", c.Modes, wantModes)
	}
	for i := range wantModes {
		if c.Modes[i] != wantModes[i] {
			t.Fatalf("modes = %v, want %v", c.Modes, wantModes)
		}
	}

	// rigctld addresses the current VFO only, because remoses does not run it
	// with -o.
	if len(c.VFOs) != 1 || c.VFOs[0] != radio.VFOCurrent {
		t.Errorf("VFOs = %v, want [current]", c.VFOs)
	}

	// RFPOWER is a fraction, whatever the tx range list says about milliwatts.
	if c.PowerWattAccurate || c.MaxPowerW != 0 {
		t.Errorf("power claimed to be watt-accurate: %v / %v W", c.PowerWattAccurate, c.MaxPowerW)
	}
	if !c.FilterWidth {
		t.Error("FilterWidth is false for a rig with set_mode")
	}
	if c.FilterSlots != 0 {
		t.Errorf("FilterSlots = %d; Hamlib has no filter-slot command", c.FilterSlots)
	}
	if c.SMeterScale != sMeterScale {
		t.Errorf("SMeterScale = %d, want %d", c.SMeterScale, sMeterScale)
	}
	if c.SubReceiver {
		t.Error("SubReceiver is true; addressing one would need VFO mode")
	}
	if c.CWMethod != radio.CWViaCAT || c.CWCharset != Charset {
		t.Errorf("CW = %q / %q, want cat and the backend charset", c.CWMethod, c.CWCharset)
	}
	if c.CWMinWPM != 4 || c.CWMaxWPM != 48 {
		t.Errorf("wpm range = %d..%d, want 4..48", c.CWMinWPM, c.CWMaxWPM)
	}
}

func TestBuildCapsHonesty(t *testing.T) {
	full := parseDumpState(lines(sampleDumpState))

	t.Run("no dump_caps means no CW and no filter width", func(t *testing.T) {
		// The dump was never fetched, so nothing is known about send_morse or
		// set_mode, and nothing may be claimed for them.
		c := buildCaps(&full, &dumpCaps{}, 5, 60)
		if c.CWMethod != radio.CWNone {
			t.Errorf("CWMethod = %q, want none", c.CWMethod)
		}
		if c.CWCharset != "" {
			t.Errorf("CWCharset = %q, want empty", c.CWCharset)
		}
		if c.FilterWidth {
			t.Error("FilterWidth is true without a dump saying set_mode exists")
		}
		// The mode list comes from dump_state and survives.
		if len(c.Modes) == 0 {
			t.Error("modes were lost with dump_caps")
		}
	})

	t.Run("no send_morse means no CW", func(t *testing.T) {
		c := buildCaps(&full, &dumpCaps{present: true, setMode: true}, 5, 60)
		if c.CWMethod != radio.CWNone {
			t.Errorf("CWMethod = %q, want none", c.CWMethod)
		}
	})

	t.Run("no STRENGTH means no S-meter scale", func(t *testing.T) {
		st := full
		st.getLevel &^= 1 << bitSTRENGTH
		c := buildCaps(&st, &dumpCaps{present: true}, 5, 60)
		if c.SMeterScale != 0 {
			t.Errorf("SMeterScale = %d for a rig with no STRENGTH level", c.SMeterScale)
		}
	})

	t.Run("morse without a settable KEYSPD reports no speed range", func(t *testing.T) {
		st := full
		st.setLevel &^= 1 << bitKEYSPD
		c := buildCaps(&st, &dumpCaps{present: true, sendMorse: true}, 5, 60)
		if c.CWMethod != radio.CWViaCAT {
			t.Errorf("CWMethod = %q, want cat", c.CWMethod)
		}
		if c.CWMinWPM != 0 || c.CWMaxWPM != 0 {
			t.Errorf("wpm range = %d..%d, want 0..0 for a rig whose speed cannot be set",
				c.CWMinWPM, c.CWMaxWPM)
		}
	})

	t.Run("an empty dump_state claims nothing", func(t *testing.T) {
		var empty dumpState
		c := buildCaps(&empty, &dumpCaps{}, 5, 60)
		if len(c.Modes) != 0 || c.SMeterScale != 0 || c.CWMethod != radio.CWNone || c.FilterWidth {
			t.Errorf("an empty dump produced %+v", c)
		}
	})
}
