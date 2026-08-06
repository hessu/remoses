package yaesu

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// sampleIF is a well-formed 28-byte IF answer, field by field, so a miscount
// fails in TestIFSamplesAreWellFormed rather than silently weakening every test
// that uses it.
const sampleIF = "IF" + // 0-1
	"000" + // 2-4    memory channel, 000 = VFO
	"014025000" + // 5-13   frequency, 14.025 MHz
	"+" + // 14     clarifier direction
	"0000" + // 15-18  clarifier offset
	"0" + // 19     RX clarifier off
	"0" + // 20     TX clarifier off
	"3" + // 21     mode CW
	"0" + // 22     VFO rather than memory
	"0" + // 23     CTCSS off
	"00" + // 24-25  fixed
	"0" // 26     simplex

// sampleIFLong is the FTX-1's 30-byte form. Its memory channel field is five
// characters, so every field after it sits two bytes further along.
const sampleIFLong = "IF" + // 0-1
	"00000" + // 2-6    memory channel
	"014025000" + // 7-15   frequency
	"+" + // 16     clarifier direction
	"0000" + // 17-20  clarifier offset
	"0" + // 21     RX clarifier off
	"0" + // 22     TX clarifier off
	"3" + // 23     mode CW
	"0" + // 24     VFO rather than memory
	"0" + // 25     CTCSS off
	"00" + // 26-27  fixed
	"0" // 28     simplex

// sampleIFOlder is the FT-950 generation's 27-byte form. Its frequency field is
// eight digits rather than nine, so every field after it sits one byte earlier
// than in sampleIF — the layout is otherwise identical, field for field.
const sampleIFOlder = "IF" + // 0-1
	"000" + // 2-4    memory channel; three spaces on the FTdx9000
	"14025000" + // 5-12   frequency, 14.025 MHz in EIGHT digits
	"+" + // 13     clarifier direction
	"0000" + // 14-17  clarifier offset
	"0" + // 18     RX clarifier off
	"0" + // 19     TX clarifier off
	"3" + // 20     mode CW
	"0" + // 21     VFO rather than memory
	"0" + // 22     CTCSS off
	"00" + // 23-24  tone number
	"0" // 25     simplex

func TestIFSamplesAreWellFormed(t *testing.T) {
	// 27, 28 and 30 on the wire, one less once Split has taken the terminator
	// off.
	for _, tt := range []struct {
		name   string
		sample string
		wire   int
		layout ifLayout
	}{
		{"the FT-950 generation", sampleIFOlder, 27, ifOlder},
		{"the FTdx101 generation", sampleIF, 28, ifShort},
		{"the FTX-1", sampleIFLong, 30, ifLong},
	} {
		if got := len(tt.sample) + 1; got != tt.wire {
			t.Errorf("%s IF answer is %d characters on the wire, its manual says %d",
				tt.name, got, tt.wire)
		}
		if len(tt.sample) != tt.layout.length {
			t.Errorf("%s sample is %d characters, its layout says %d",
				tt.name, len(tt.sample), tt.layout.length)
		}
	}

	// The FTX-1's fields are the short layout's shifted by the two characters
	// its memory channel field grew by, and the FT-950 generation's are the
	// short layout's shifted back by the one digit its frequency field lost.
	// Those are the only two reasons any of these offsets differ.
	if ifLong.freqStart-ifShort.freqStart != 2 || ifLong.mode-ifShort.mode != 2 {
		t.Errorf("the FTX-1 offsets are not the short ones shifted by 2: %+v vs %+v", ifLong, ifShort)
	}
	if ifOlder.freqStart != ifShort.freqStart {
		t.Errorf("the older frequency field does not start where the short one does: %+v vs %+v",
			ifOlder, ifShort)
	}
	if ifShort.freqEnd-ifOlder.freqEnd != 1 || ifShort.mode-ifOlder.mode != 1 {
		t.Errorf("the older offsets are not the short ones shifted back by 1: %+v vs %+v",
			ifOlder, ifShort)
	}
	// The widths the layouts imply have to be the two the FA/FB parser accepts,
	// or a frequency would decode from one and not the other.
	if got := ifOlder.freqEnd - ifOlder.freqStart; got != freqDigitsOld {
		t.Errorf("the older IF frequency field is %d digits, want %d", got, freqDigitsOld)
	}
	for _, l := range []ifLayout{ifShort, ifLong} {
		if got := l.freqEnd - l.freqStart; got != freqDigitsModern {
			t.Errorf("IF layout %+v has a %d-digit frequency field, want %d", l, got, freqDigitsModern)
		}
	}
	// The three lengths have to stay distinct, because that is the only thing
	// decodeIF dispatches on.
	seen := map[int]bool{}
	for _, n := range []int{ifOlder.length, ifShort.length, ifLong.length, ifQuirkLength} {
		if seen[n] {
			t.Errorf("two IF layouts are %d characters long; the dispatch is ambiguous", n)
		}
		seen[n] = true
	}
}

func TestSplit(t *testing.T) {
	tests := []struct {
		name  string
		reads []string
		want  []string
	}{
		{
			name:  "single frame",
			reads: []string{"FA014025000;"},
			want:  []string{"FA014025000"},
		},
		{
			name:  "several frames in one read",
			reads: []string{"FA014025000;MD03;SM0123;"},
			want:  []string{"FA014025000", "MD03", "SM0123"},
		},
		{
			name: "frame split across reads",
			// A 28-byte IF answer arriving in three chunks. Split must ask for
			// more rather than emit a partial frame.
			reads: []string{sampleIF[:8], sampleIF[8:20], sampleIF[20:] + ";"},
			want:  []string{sampleIF},
		},
		{
			name:  "trailing partial frame is withheld",
			reads: []string{"MD03;FA0140"},
			want:  []string{"MD03"},
		},
		{
			name:  "empty frames are swallowed",
			reads: []string{";;MD03;;;TX0;"},
			want:  []string{"MD03", "TX0"},
		},
		{
			name: "leading garbage resynchronises",
			// A rig powering up, or a port opened mid-frame. Every command
			// starts with a letter, so anything before one can only be noise.
			reads: []string{"\x00\xff\x1b\x00FA014025000;"},
			want:  []string{"FA014025000"},
		},
		{
			name:  "garbage between frames",
			reads: []string{"MD03;\x00\x00;\xfeTX1;"},
			want:  []string{"MD03", "TX1"},
		},
		{
			name:  "line endings are trimmed",
			reads: []string{"MD03\r\n;TX0;"},
			want:  []string{"MD03", "TX0"},
		},
		{
			// The busy answer is the one frame that does not start with a
			// command letter, so resynchronisation has to make an exception for
			// it or it would be discarded as noise and cost a full timeout.
			name:  "a busy answer survives resynchronisation",
			reads: []string{"?;"},
			want:  []string{"?"},
		},
		{
			name:  "a busy answer between frames",
			reads: []string{"MD03;?;TX0;"},
			want:  []string{"MD03", "?", "TX0"},
		},
		{
			name:  "a busy answer with a line ending",
			reads: []string{"?\r\n;"},
			want:  []string{"?"},
		},
		{
			// A '?' in front of a real answer can only be noise: the two would
			// be separate frames if both were real, so the answer behind it is
			// what matters and must not be swallowed.
			name:  "a stray ? does not swallow the frame behind it",
			reads: []string{"?FA014025000;"},
			want:  []string{"FA014025000"},
		},
		{
			name:  "nothing at all",
			reads: []string{""},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, "generic")
			sc := bufio.NewScanner(&chunkReader{chunks: tt.reads})
			sc.Split(y.Split)

			var got []string
			for sc.Scan() {
				got = append(got, sc.Text())
			}
			if err := sc.Err(); err != nil {
				t.Fatalf("scanner: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("frames %q, want %q", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("frame %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestDecodeNeverErrors is the contract Decode is held to: this path carries
// unsolicited AI traffic, so garbage has to be ignored quietly rather than
// reported.
func TestDecodeNeverErrors(t *testing.T) {
	y := newModelRig(t, "ft-991a")
	for _, f := range []string{
		"", "F", "FA", "FA1", "FA0140250001", "MD0", "MD0Z", "MD12", "SM", "SM01234",
		"TX", "TXX", "PC", "PC12", "PCxyz", "PC1xyz", "PC12345", "SH", "SH0", "NA0", "ID", "IDxyzw",
		"IF", sampleIF[:12], sampleIF + "00", "ZZ99", "123",
	} {
		u, err := y.Decode([]byte(f))
		if err != nil {
			t.Errorf("Decode(%q): %v", f, err)
		}
		if !u.OK {
			t.Errorf("Decode(%q) reported a rejection; ?; is the only one", f)
		}
	}
}

// TestDecodeBusy pins the one answer that is not a command. No manual documents
// it, but radios send it — an FT-450 to IF;, an FTdx3000 to FB; — and a
// transaction that does not recognise it sits out the session's whole
// per-command timeout instead of failing in one round trip.
func TestDecodeBusy(t *testing.T) {
	y := newModelRig(t, "ft-991a")

	u, err := y.Decode([]byte("?"))
	if err != nil {
		t.Fatalf("Decode(%q): %v", "?", err)
	}
	if u.Key != keyBusy {
		t.Errorf("Decode(%q) keyed %q, want %q", "?", u.Key, keyBusy)
	}
	if u.OK {
		t.Error("Decode(\"?\") reported OK; the rig did not take the command")
	}
	if !u.Patch.Empty() {
		t.Errorf("Decode(\"?\") published %+v; a busy answer says nothing about state", u.Patch)
	}

	// N, O and E exist too and are deliberately not handled: N is reported to
	// mean invalid data, a genuine rejection rather than a "try again", and
	// treating it as busy would retry a command remoses is spelling wrongly for
	// ever. They stay unrecognised traffic until each has a decided meaning.
	for _, f := range []string{"N", "O", "E"} {
		u, err := y.Decode([]byte(f))
		if err != nil {
			t.Errorf("Decode(%q): %v", f, err)
		}
		if u.Key != backend.KeyUnsolicited || !u.OK {
			t.Errorf("Decode(%q) = key %q, OK %v; want it ignored quietly", f, u.Key, u.OK)
		}
	}
}

func TestDecode(t *testing.T) {
	hz := func(v uint64) *uint64 { return &v }
	mode := func(m radio.Mode) *radio.Mode { return &m }
	flag := func(b bool) *bool { return &b }
	num := func(v int) *int { return &v }

	tests := []struct {
		name    string
		model   string
		frame   string
		wantKey backend.Key
		want    radio.Patch
	}{
		{
			name: "frequency", model: "generic", frame: "FA014025000",
			wantKey: keyFA, want: radio.Patch{Frequency: hz(14_025_000)},
		},
		{
			// Eight digits is the FT-950 generation. It denotes the same Hz, so
			// it is read rather than refused — and read on any profile, because
			// the field boundaries are unambiguous either way.
			name: "eight-digit frequency", model: "ft-950", frame: "FA14025000",
			wantKey: keyFA, want: radio.Patch{Frequency: hz(14_025_000)},
		},
		{
			name: "eight-digit frequency on a nine-digit profile", model: "generic",
			frame: "FA14025000", wantKey: keyFA, want: radio.Patch{Frequency: hz(14_025_000)},
		},
		{
			// Eight or nine, not Kenwood's eleven: an eleven-digit field is not
			// a tolerated variant, it is a frame from a different radio.
			name: "eleven-digit frequency is not this protocol", model: "generic",
			frame: "FA00014025000", wantKey: keyFA,
		},
		{
			name: "top of the FA field", model: "generic", frame: "FA470000000",
			wantKey: keyFA, want: radio.Patch{Frequency: hz(470_000_000)},
		},
		{
			// VFO B completes its transaction and publishes nothing: State
			// carries one frequency and this backend anchors it on VFO A.
			name: "vfo B is not published", model: "generic", frame: "FB014100000",
			wantKey: keyFB,
		},
		{
			name: "mode", model: "generic", frame: "MD03",
			wantKey: keyMD, want: radio.Patch{Mode: mode(radio.ModeCW), DataMode: flag(false)},
		},
		{
			// DATA is inside the mode code on a Yaesu, so one answer settles
			// both fields and there is no DA to follow it with.
			name: "data mode rides in the code", model: "generic", frame: "MD0C",
			wantKey: keyMD, want: radio.Patch{Mode: mode(radio.ModeUSB), DataMode: flag(true)},
		},
		{
			name: "lower case", model: "generic", frame: "md0c",
			wantKey: keyMD, want: radio.Patch{Mode: mode(radio.ModeUSB), DataMode: flag(true)},
		},
		{
			// The sub receiver, or the FTdx10's selector on a radio that has
			// none. State publishes one mode, so folding it in would let the
			// second receiver overwrite the first.
			name: "sub receiver is ignored", model: "ftdx101d", frame: "MD12",
			wantKey: keyMD,
		},
		{
			name: "power", model: "generic", frame: "PC050",
			wantKey: keyPC, want: radio.Patch{Power: &radio.Power{Pct: 50, Native: 50}},
		},
		{
			name: "power on a 200 W radio", model: "ftdx101mp", frame: "PC100",
			wantKey: keyPC, want: radio.Patch{Power: &radio.Power{Pct: 50, Native: 100}},
		},
		{
			name: "s-meter", model: "generic", frame: "SM0123",
			wantKey: keySM, want: radio.Patch{SMeter: &radio.Meter{Raw: 123, Scale: 255}},
		},
		{
			name: "s-meter full scale", model: "generic", frame: "SM0255",
			wantKey: keySM, want: radio.Patch{SMeter: &radio.Meter{Raw: 255, Scale: 255}},
		},
		{
			// The sub receiver's meter, discarded for the same reason its mode
			// is: State has one S-meter.
			name: "sub s-meter is ignored", model: "ftdx101d", frame: "SM1200",
			wantKey: keySM,
		},
		{
			// Four digits is Kenwood's SM. Reading it here would report a
			// signal an order of magnitude out.
			name: "four-digit s-meter is not this protocol", model: "generic",
			frame: "SM00015", wantKey: keySM,
		},
		{
			name: "receiving", model: "generic", frame: "TX0",
			wantKey: keyTX, want: radio.Patch{PTT: flag(false)},
		},
		{
			name: "keyed by CAT", model: "generic", frame: "TX1",
			wantKey: keyTX, want: radio.Patch{PTT: flag(true)},
		},
		{
			// TX2 is the rig keying itself — front panel, foot switch, MOX, a
			// paddle in break-in. It is transmitting, and no other backend here
			// can tell the operator that.
			name: "keyed at the rig", model: "generic", frame: "TX2",
			wantKey: keyTX, want: radio.Patch{PTT: flag(true)},
		},
		{
			// TX3 is the FTdx9000's fourth value: keyed at the rig and by CAT
			// at once. Every other manual stops at 2.
			name: "keyed at the rig and by CAT", model: "ftdx9000", frame: "TX3",
			wantKey: keyTX, want: radio.Patch{PTT: flag(true)},
		},
		{
			name: "bad TX value", model: "generic", frame: "TX9", wantKey: keyTX,
		},
		{
			// The FTdx5000's PC is an uncalibrated index, so the same three
			// digits mean a fifth of full output rather than 50 W.
			name: "power as an index", model: "ftdx5000", frame: "PC051",
			wantKey: keyPC, want: radio.Patch{Power: &radio.Power{Pct: 20, Native: 51}},
		},
		{
			name: "narrow has no patch of its own", model: "generic", frame: "NA01",
			wantKey: keyNA,
		},
		{
			name: "identity", model: "generic", frame: "ID0670", wantKey: keyID,
		},
		{
			name: "auto information", model: "generic", frame: "AI1", wantKey: keyAI,
		},
		{
			name: "bulk status", model: "generic", frame: sampleIF, wantKey: keyIF,
			want: radio.Patch{Frequency: hz(14_025_000), Mode: mode(radio.ModeCW), DataMode: flag(false)},
		},
		{
			name: "unknown command", model: "generic", frame: "ZZ9",
			wantKey: backend.KeyUnsolicited,
		},
		{
			// The FT-891 has no A code at all, so a frame carrying one reports
			// nothing rather than guessing.
			name: "code the model does not have", model: "ft-891", frame: "MD0A",
			wantKey: keyMD,
		},
		{
			name: "SH width", model: "ftdx101d", frame: "SH0010",
			wantKey: keySH, want: radio.Patch{PassbandHz: num(500)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			// The SH decode needs a mode to pick a table column with, and the
			// samples are all CW.
			y.mode.Store(uint32(radio.ModeCW))

			u := mustDecode(t, y, tt.frame)
			if u.Key != tt.wantKey {
				t.Fatalf("key = %q, want %q", u.Key, tt.wantKey)
			}
			if !u.OK {
				t.Error("OK false")
			}
			comparePatch(t, y, u.Patch, tt.want)
		})
	}
}

// TestDecodeIFEveryLength is what the two generation splits cost, in one test:
// the same radio state in three layouts, decoded from the length that arrived
// rather than from the configured model, so a misconfigured station still reads
// correctly.
func TestDecodeIFEveryLength(t *testing.T) {
	for _, name := range ModelNames() {
		for _, frame := range []string{sampleIFOlder, sampleIF, sampleIFLong} {
			t.Run(fmt.Sprintf("%s/%d", name, len(frame)), func(t *testing.T) {
				y := newModelRig(t, name)
				u := mustDecode(t, y, frame)
				if u.Key != keyIF {
					t.Fatalf("key = %q, want %q", u.Key, keyIF)
				}
				if u.Patch.Frequency == nil || *u.Patch.Frequency != 14_025_000 {
					t.Errorf("frequency = %v, want 14025000", u.Patch.Frequency)
				}
				if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeCW {
					t.Errorf("mode = %v, want CW", u.Patch.Mode)
				}
			})
		}
	}
}

// TestDecodeIFWrongOffsetsWouldShow guards the offsets themselves: a frame with
// a different frequency and mode has to be read at the right places, and a
// length that is no layout must publish nothing at all.
func TestDecodeIFWrongOffsetsWouldShow(t *testing.T) {
	y := newModelRig(t, "ftx-1")

	// 7.030 MHz in USB, in the long layout.
	f := []byte(sampleIFLong)
	copy(f[ifLong.freqStart:], "007030000")
	f[ifLong.mode] = '2'
	u := mustDecode(t, y, string(f))
	if u.Patch.Frequency == nil || *u.Patch.Frequency != 7_030_000 {
		t.Errorf("frequency = %v, want 7030000", u.Patch.Frequency)
	}
	if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeUSB {
		t.Errorf("mode = %v, want USB", u.Patch.Mode)
	}

	// The same thing in the eight-digit layout, where reading at the FTdx101
	// offsets would take the frequency from one byte too far along and land the
	// mode on the memory/VFO flag.
	f = []byte(sampleIFOlder)
	copy(f[ifOlder.freqStart:], "07030000")
	f[ifOlder.mode] = '2'
	u = mustDecode(t, newModelRig(t, "ft-950"), string(f))
	if u.Patch.Frequency == nil || *u.Patch.Frequency != 7_030_000 {
		t.Errorf("frequency = %v, want 7030000", u.Patch.Frequency)
	}
	if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeUSB {
		t.Errorf("mode = %v, want USB", u.Patch.Mode)
	}

	for _, bad := range []string{sampleIFOlder[:25], sampleIF + "0", sampleIFLong + "0"} {
		u := mustDecode(t, y, bad)
		if u.Key != keyIF {
			t.Errorf("Decode(%d bytes) key = %q, want %q so the transaction completes", len(bad), u.Key, keyIF)
		}
		if !u.Patch.Empty() {
			t.Errorf("a %d-byte IF frame published %+v; no layout is that long", len(bad), u.Patch)
		}
	}
}

// TestDecodeIFQuirkLength covers the tolerance for a field report rather than a
// transcribed fact: some FT-991 firmware is said to append thirteen bytes of
// rubbish at random. The surplus is trailing, so every field remoses reads is
// still at its usual offset and the 28-byte layout applies unchanged. Refusing
// it would drop the entire bulk poll on an affected radio, intermittently.
func TestDecodeIFQuirkLength(t *testing.T) {
	y := newModelRig(t, "ft-991a")
	frame := sampleIF + strings.Repeat("0", ifQuirkLength-len(sampleIF))
	if len(frame) != ifQuirkLength {
		t.Fatalf("the quirk frame is %d characters, want %d", len(frame), ifQuirkLength)
	}
	u := mustDecode(t, y, frame)
	if u.Patch.Frequency == nil || *u.Patch.Frequency != 14_025_000 {
		t.Errorf("frequency = %v, want 14025000", u.Patch.Frequency)
	}
	if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeCW {
		t.Errorf("mode = %v, want CW", u.Patch.Mode)
	}
}

// TestDecodePCHead covers the FTX-1's power command, whose answer names the
// amplifier chain in front of the three digits — and with it the ceiling the
// percentage is measured against.
func TestDecodePCHead(t *testing.T) {
	y := newModelRig(t, "ftx-1")

	// Head 1 is the field head alone: 10 W full scale.
	u := mustDecode(t, y, "PC1010")
	if u.Patch.Power == nil || u.Patch.Power.Native != 10 || u.Patch.Power.Pct != 100 {
		t.Fatalf("power = %+v, want 10 W at 100%% of the bare head", u.Patch.Power)
	}
	if got := y.maxPowerW(); got != ftx1HeadMaxW {
		t.Errorf("maxPowerW = %d, want %d", got, ftx1HeadMaxW)
	}

	// Head 2 is the SPA-1 amplifier, which takes it to 100 W.
	u = mustDecode(t, y, "PC2050")
	if u.Patch.Power == nil || u.Patch.Power.Native != 50 || u.Patch.Power.Pct != 50 {
		t.Fatalf("power = %+v, want 50 W at 50%% with the amplifier", u.Patch.Power)
	}
	if got := y.maxPowerW(); got != ftx1AmpMaxW {
		t.Errorf("maxPowerW = %d, want %d", got, ftx1AmpMaxW)
	}
	if got := y.Caps().MaxPowerW; got != float64(ftx1AmpMaxW) {
		t.Errorf("Caps.MaxPowerW = %v, want %v; Caps is read after Init and may refine", got, ftx1AmpMaxW)
	}

	// A head the manual does not define is not a power reading.
	if u := mustDecode(t, y, "PC3050"); u.Patch.Power != nil {
		t.Errorf("head 3 accepted as %+v", *u.Patch.Power)
	}

	// The plain three-digit form still reads, which is what an FTX-1
	// misconfigured as anything else would send.
	if u := mustDecode(t, y, "PC050"); u.Patch.Power == nil || u.Patch.Power.Native != 50 {
		t.Errorf("three-digit PC on an FTX-1 profile = %+v", u.Patch.Power)
	}
}

// TestDecodeSHPerModel covers the two halves of the filter answer: the spelling,
// which is one character shorter on the FT-991A, and the table column, which
// depends on the mode and on the narrow setting.
func TestDecodeSHPerModel(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		mode   radio.Mode
		data   bool
		narrow bool
		frame  string
		want   int // 0 means nothing published
	}{
		// The FT-991A's six-byte form. Index 10 is 500 Hz in CW either way.
		{"991A CW", "ft-991a", radio.ModeCW, false, false, "SH010", 500},
		{"991A CW narrow", "ft-991a", radio.ModeCW, false, true, "SH010", 500},
		// Narrow moves the SSB column wholesale: index 4 is 850 Hz narrow and
		// blank wide.
		{"991A SSB narrow", "ft-991a", radio.ModeUSB, false, true, "SH004", 850},
		{"991A SSB wide", "ft-991a", radio.ModeUSB, false, false, "SH004", 0},
		{"991A SSB wide 14", "ft-991a", radio.ModeUSB, false, false, "SH014", 2400},
		// The seven-byte form on a radio that reads it as six is not a width.
		{"991A rejects the long form", "ft-991a", radio.ModeCW, false, false, "SH0010", 0},
		// The FT-891 carries the narrow flag inside SH itself.
		{"891 narrow flag in the answer", "ft-891", radio.ModeUSB, false, false, "SH1104", 0},
		{"891 rejects a flag the manual has no value for", "ft-891", radio.ModeUSB, false, false, "SH0204", 0},
		// Two digits, and only digits: a letter where the index should be is
		// line noise, not index 0.
		{"non-numeric index", "ftdx101d", radio.ModeCW, false, false, "SH00x0", 0},
		{"non-numeric index second digit", "ftdx101d", radio.ModeCW, false, false, "SH001x", 0},
		{"891 narrow set by SH", "ft-891", radio.ModeUSB, false, false, "SH0104", 850},
		{"891 wide", "ft-891", radio.ModeUSB, false, false, "SH0014", 2400},
		{"891 rejects the short form", "ft-891", radio.ModeCW, false, false, "SH010", 0},
		// The newer radios have one column per group, so narrow changes nothing.
		{"dx101 CW", "ftdx101d", radio.ModeCW, false, false, "SH0010", 500},
		{"dx101 CW narrow makes no difference", "ftdx101d", radio.ModeCW, false, true, "SH0010", 500},
		{"dx101 SSB", "ftdx101d", radio.ModeUSB, false, false, "SH0010", 1950},
		// DATA is grouped with CW and RTTY, not with SSB, so the same index is
		// a different width.
		{"dx101 USB-DATA", "ftdx101d", radio.ModeUSB, true, false, "SH0010", 500},
		{"710 SSB top", "ft-710", radio.ModeUSB, false, false, "SH0023", 4000},
		{"710 CW top", "ft-710", radio.ModeCW, false, false, "SH0021", 4000},
		// Index 0 is the rig's default, which these manuals decline to state.
		{"dx101 default index is unknown", "ftdx101d", radio.ModeCW, false, false, "SH0000", 0},
		// The FT-991A does state its defaults.
		{"991A default index", "ft-991a", radio.ModeCW, false, true, "SH000", 500},
		// AM and FM have no ladder at all.
		{"no width in FM", "ftdx101d", radio.ModeFM, false, false, "SH0010", 0},
		{"no width in AM", "ft-991a", radio.ModeAM, false, false, "SH010", 0},

		// The FT-950 generation takes the same six-byte form as the FT-991A,
		// and every one of the four has its own table underneath it. Index 7 is
		// four different widths in wide CW: nothing on the FT-991A's ladder,
		// 500 Hz on the FT-950's, and nothing again on the FTdx1200's, whose
		// wide CW column starts at 10.
		{"950 CW wide 7", "ft-950", radio.ModeCW, false, false, "SH007", 500},
		{"950 CW narrow 7", "ft-950", radio.ModeCW, false, true, "SH007", 500},
		{"950 CW narrow 3", "ft-950", radio.ModeCW, false, true, "SH003", 100},
		{"950 CW narrow 1 is blank", "ft-950", radio.ModeCW, false, true, "SH001", 0},
		{"950 SSB wide 14", "ft-950", radio.ModeUSB, false, false, "SH014", 2450},
		{"950 SSB narrow 4", "ft-950", radio.ModeUSB, false, true, "SH004", 850},
		{"950 default is stated", "ft-950", radio.ModeUSB, false, true, "SH000", 1800},
		{"1200 SSB wide 14", "ftdx1200", radio.ModeUSB, false, false, "SH014", 2400},
		{"1200 CW wide 7 is blank", "ftdx1200", radio.ModeCW, false, false, "SH007", 0},
		{"1200 CW narrow 7", "ftdx1200", radio.ModeCW, false, true, "SH007", 350},
		{"3000 shares the 1200 table", "ftdx3000", radio.ModeUSB, false, false, "SH025", 4000},
		{"5000 SSB wide 12", "ftdx5000", radio.ModeUSB, false, false, "SH012", 2250},
		{"5000 SSB wide 14 is the hole", "ftdx5000", radio.ModeUSB, false, false, "SH014", 0},
		{"5000 SSB narrow past its end", "ftdx5000", radio.ModeUSB, false, true, "SH008", 0},
		{"5000 USB-DATA", "ftdx5000", radio.ModeUSB, true, true, "SH000", 300},
		// The FTdx9000's SH is the WIDTH knob's position, so nothing it can say
		// is a bandwidth — including a value that indexes every other table.
		{"9000 has no table", "ftdx9000", radio.ModeCW, false, false, "SH010", 0},
		{"9000 knob fully clockwise", "ftdx9000", radio.ModeUSB, false, false, "SH031", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			y.mode.Store(uint32(tt.mode))
			y.dataMode.Store(tt.data)
			y.narrow.Store(tt.narrow)

			u := mustDecode(t, y, tt.frame)
			if u.Key != keySH {
				t.Fatalf("key = %q, want %q", u.Key, keySH)
			}
			switch {
			case tt.want == 0:
				if u.Patch.PassbandHz != nil {
					t.Errorf("passband = %d, want nothing published", *u.Patch.PassbandHz)
				}
			case u.Patch.PassbandHz == nil:
				t.Errorf("no passband published, want %d", tt.want)
			case *u.Patch.PassbandHz != tt.want:
				t.Errorf("passband = %d, want %d", *u.Patch.PassbandHz, tt.want)
			}
		})
	}
}

// TestDecodeNAFeedsTheFilterColumn is the other half of the FT-991A's narrow
// handling: its SH answer does not carry the flag, so NA is the only source.
func TestDecodeNAFeedsTheFilterColumn(t *testing.T) {
	y := newModelRig(t, "ft-991a")
	y.mode.Store(uint32(radio.ModeUSB))

	mustDecode(t, y, "NA01")
	if !y.narrow.Load() {
		t.Fatal("NA01 did not set narrow")
	}
	if u := mustDecode(t, y, "SH004"); u.Patch.PassbandHz == nil || *u.Patch.PassbandHz != 850 {
		t.Errorf("passband = %v, want 850 from the narrow column", u.Patch.PassbandHz)
	}

	mustDecode(t, y, "NA00")
	if y.narrow.Load() {
		t.Fatal("NA00 did not clear narrow")
	}
	if u := mustDecode(t, y, "SH004"); u.Patch.PassbandHz != nil {
		t.Errorf("passband = %d, want nothing: index 4 is blank in the wide column", *u.Patch.PassbandHz)
	}

	// A sub-receiver or malformed NA changes nothing.
	mustDecode(t, y, "NA11")
	if y.narrow.Load() {
		t.Error("a sub-receiver NA frame changed the main receiver's narrow setting")
	}
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		frame   string
		wantCmd string
		wantArg string
		wantOK  bool
	}{
		{"FA014025000", "FA", "014025000", true},
		{"md03", "MD", "03", true},
		{"TX", "TX", "", true},
		// Three letters is Kenwood's shape, not Yaesu's: every command here is
		// exactly two.
		{"ZZZ1", "", "", false},
		{"F", "", "", false},
		{"?", "", "", false},
		{"0123", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.frame, func(t *testing.T) {
			cmd, arg, ok := splitCommand([]byte(tt.frame))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if cmd != tt.wantCmd || string(arg) != tt.wantArg {
				t.Errorf("= (%q, %q), want (%q, %q)", cmd, arg, tt.wantCmd, tt.wantArg)
			}
		})
	}
}

// TestDecodeRawIsOwned covers the one aliasing hazard in this file: the
// session's scanner reuses its read buffer.
func TestDecodeRawIsOwned(t *testing.T) {
	y := newModelRig(t, "generic")
	buf := []byte("FA014025000")
	u := mustDecode(t, y, string(buf))
	copy(buf, "FA007030000")
	if string(u.Raw) != "FA014025000" {
		t.Errorf("Raw = %q; it aliases the caller's buffer", u.Raw)
	}
}

// --- helpers ---------------------------------------------------------------

func mustDecode(t *testing.T, y *Rig, frame string) backend.Update {
	t.Helper()
	u, err := y.Decode([]byte(frame))
	if err != nil {
		t.Fatalf("Decode(%q): %v", frame, err)
	}
	return u
}

func comparePatch(t *testing.T, y *Rig, got, want radio.Patch) {
	t.Helper()

	cmp := func(name string, g, w any) {
		t.Helper()
		gs, ws := describe(g), describe(w)
		if gs != ws {
			t.Errorf("patch.%s = %s, want %s", name, gs, ws)
		}
	}
	cmp("Frequency", got.Frequency, want.Frequency)
	cmp("Mode", got.Mode, want.Mode)
	cmp("DataMode", got.DataMode, want.DataMode)
	cmp("PassbandHz", got.PassbandHz, want.PassbandHz)
	cmp("FilterSlot", got.FilterSlot, want.FilterSlot)
	cmp("PTT", got.PTT, want.PTT)

	compareMeter(t, "SMeter", got.SMeter, want.SMeter)
	comparePower(t, y, got.Power, want.Power)
}

func compareMeter(t *testing.T, name string, got, want *radio.Meter) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("patch.%s = %v, want %v", name, got, want)
	case *got != *want:
		t.Errorf("patch.%s = %+v, want %+v", name, *got, *want)
	}
}

func comparePower(t *testing.T, y *Rig, got, want *radio.Power) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("patch.Power = %v, want %v", got, want)
	}
	if got.Native != want.Native || got.Pct != want.Pct {
		t.Errorf("patch.Power = {native %d, pct %.2f}, want {native %d, pct %.2f}",
			got.Native, got.Pct, want.Native, want.Pct)
	}
	// PC is in real watts on ten of the twelve models, so a decoded power
	// carries them. On the FTdx5000 and FTdx9000 it is an uncalibrated index
	// and Watts must stay nil rather than repeat the index as a watt figure.
	if y.profile.PowerRaw {
		if got.Watts != nil {
			t.Errorf("patch.Power.Watts = %v on a radio whose PC is an index, want none", *got.Watts)
		}
		return
	}
	if got.Watts == nil || *got.Watts != float64(got.Native) {
		t.Errorf("patch.Power.Watts = %v, want %d", got.Watts, got.Native)
	}
}

// describe renders a possibly-nil pointer field for comparison and for the
// failure message.
func describe(v any) string {
	switch p := v.(type) {
	case *uint64:
		if p == nil {
			return "unset"
		}
		return fmt.Sprint(*p)
	case *int:
		if p == nil {
			return "unset"
		}
		return fmt.Sprint(*p)
	case *bool:
		if p == nil {
			return "unset"
		}
		return fmt.Sprint(*p)
	case *radio.Mode:
		if p == nil {
			return "unset"
		}
		return p.String()
	}
	return "unset"
}

// chunkReader hands out one scripted read at a time, which is how a serial port
// behaves and how a frame ends up split across reads.
type chunkReader struct {
	chunks []string
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for r.i < len(r.chunks) && r.chunks[r.i] == "" {
		r.i++
	}
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.chunks[r.i] = r.chunks[r.i][n:]
	return n, nil
}
