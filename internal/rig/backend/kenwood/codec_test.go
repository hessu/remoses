package kenwood

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// sampleIF is a well-formed IF answer, field by field, so a miscount fails in
// TestIFSampleIsWellFormed rather than silently weakening every test that uses
// it.
const sampleIF = "IF" + // 1-2
	"00014025000" + // 3-13   frequency, 14.025 MHz
	"     " + // 14-18  five spaces
	"+0000" + // 19-23  RIT/XIT offset
	"0" + // 24     RIT off
	"0" + // 25     XIT off
	"0" + // 26     memory channel hundreds
	"00" + // 27-28  memory channel
	"0" + // 29     RX
	"3" + // 30     mode CW
	"0" + // 31     FR/FT
	"0" + // 32     scan off
	"0" + // 33     simplex
	"0" + // 34     tone off
	"08" + // 35-36  tone frequency
	"0" // 37     always 0

func TestIFSampleIsWellFormed(t *testing.T) {
	// 38 on the wire, 37 once Split has taken the terminator off.
	if got := len(sampleIF) + 1; got != 38 {
		t.Fatalf("sample IF answer is %d characters on the wire, the reference says 38", got)
	}
	if len(sampleIF) != ifLen {
		t.Fatalf("sample IF frame is %d characters, ifLen is %d", len(sampleIF), ifLen)
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
			reads: []string{"FA00014025000;"},
			want:  []string{"FA00014025000"},
		},
		{
			name:  "several frames in one read",
			reads: []string{"FA00014025000;MD3;SM00015;"},
			want:  []string{"FA00014025000", "MD3", "SM00015"},
		},
		{
			name: "frame split across reads",
			// The classic serial case: a 38-byte IF answer arriving in three
			// chunks. Split must ask for more rather than emit a partial frame.
			reads: []string{sampleIF[:10], sampleIF[10:30], sampleIF[30:] + ";"},
			want:  []string{sampleIF},
		},
		{
			name:  "trailing partial frame is withheld",
			reads: []string{"MD3;FA000140"},
			want:  []string{"MD3"},
		},
		{
			name:  "empty frames are swallowed",
			reads: []string{";;MD3;;;FL1;"},
			want:  []string{"MD3", "FL1"},
		},
		{
			name: "leading garbage resynchronises",
			// A rig powering up, or a port opened mid-frame. Every command
			// starts with a letter, so anything before one can only be noise.
			reads: []string{"\x00\xff\x1b\x00FA00014025000;"},
			want:  []string{"FA00014025000"},
		},
		{
			name:  "garbage between frames",
			reads: []string{"MD3;\x00\x00;\xfeFL2;"},
			want:  []string{"MD3", "FL2"},
		},
		{
			name:  "line endings are trimmed",
			reads: []string{"MD3\r\n;FL1;"},
			want:  []string{"MD3", "FL1"},
		},
		{
			name:  "nothing at all",
			reads: []string{""},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			sc := bufio.NewScanner(&chunkReader{chunks: tt.reads})
			sc.Split(k.Split)

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

// chunkReader hands out one scripted chunk per Read, so a test can control
// exactly where a frame is torn in half.
type chunkReader struct{ chunks []string }

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 && r.chunks[0] == "" {
		r.chunks = r.chunks[1:]
	}
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	return n, nil
}

func TestDecode(t *testing.T) {
	hz := func(v uint64) *uint64 { return &v }
	mode := func(m radio.Mode) *radio.Mode { return &m }
	yes := func(b bool) *bool { return &b }
	num := func(v int) *int { return &v }

	tests := []struct {
		name string
		// priorMode primes the last-decoded mode, which PC needs to scale watts.
		priorMode radio.Mode
		frame     string
		wantKey   backend.Key
		wantOK    bool
		want      radio.Patch
	}{
		{
			name: "FA frequency", frame: "FA00014025000",
			wantKey: keyFA, wantOK: true,
			want: radio.Patch{Frequency: hz(14_025_000)},
		},
		{
			name: "FA at the top of the field", frame: "FA00060000000",
			wantKey: keyFA, wantOK: true,
			want: radio.Patch{Frequency: hz(60_000_000)},
		},
		{
			name: "FA with a short parameter reports nothing", frame: "FA0001402",
			wantKey: keyFA, wantOK: true,
		},
		{
			name: "FB completes but publishes no frequency", frame: "FB00007050000",
			wantKey: keyFB, wantOK: true,
		},
		{
			name: "MD CW", frame: "MD3", wantKey: keyMD, wantOK: true,
			want: radio.Patch{Mode: mode(radio.ModeCW)},
		},
		{
			name: "MD FSK-R", frame: "MD9", wantKey: keyMD, wantOK: true,
			want: radio.Patch{Mode: mode(radio.ModeFSKR)},
		},
		{
			// 0 and 8 are "None (setting failure)". Mapping them to
			// ModeUnknown would let a rejected set wipe a good cached mode.
			name: "MD0 setting failure carries no mode", frame: "MD0",
			wantKey: keyMD, wantOK: true,
		},
		{
			name: "MD8 setting failure carries no mode", frame: "MD8",
			wantKey: keyMD, wantOK: true,
		},
		{
			name: "DA on", frame: "DA1", wantKey: keyDA, wantOK: true,
			want: radio.Patch{DataMode: yes(true)},
		},
		{
			name: "DA off", frame: "DA0", wantKey: keyDA, wantOK: true,
			want: radio.Patch{DataMode: yes(false)},
		},
		{
			name: "PC in SSB", priorMode: radio.ModeUSB, frame: "PC050",
			wantKey: keyPC, wantOK: true,
			want: radio.Patch{Power: power(50, 50)},
		},
		{
			// The AM ceiling is 25 W, so the same watts are a different
			// fraction of full power.
			name: "PC in AM", priorMode: radio.ModeAM, frame: "PC010",
			wantKey: keyPC, wantOK: true,
			want: radio.Patch{Power: power(10, 40)},
		},
		{
			name: "PC with a short parameter", priorMode: radio.ModeUSB, frame: "PC05",
			wantKey: keyPC, wantOK: true,
		},
		{
			// A parameter made of letters is indistinguishable from a longer
			// command name, so it degrades to unsolicited rather than being
			// mis-keyed. No real answer looks like this.
			name: "PC with letters in the parameter", priorMode: radio.ModeUSB, frame: "PCxyz",
			wantKey: backend.KeyUnsolicited, wantOK: true,
		},
		{
			name: "SM meter dots", frame: "SM00015", wantKey: keySM, wantOK: true,
			want: radio.Patch{SMeter: &radio.Meter{Raw: 15, Scale: 30}},
		},
		{
			name: "SM full scale", frame: "SM00030", wantKey: keySM, wantOK: true,
			want: radio.Patch{SMeter: &radio.Meter{Raw: 30, Scale: 30}},
		},
		{
			name: "SM short", frame: "SM000", wantKey: keySM, wantOK: true,
		},
		{
			name: "FW width", frame: "FW0500", wantKey: keyFW, wantOK: true,
			want: radio.Patch{PassbandHz: num(500)},
		},
		{
			name: "FL filter A", frame: "FL1", wantKey: keyFL, wantOK: true,
			want: radio.Patch{FilterSlot: num(1)},
		},
		{
			name: "FL filter B", frame: "FL2", wantKey: keyFL, wantOK: true,
			want: radio.Patch{FilterSlot: num(2)},
		},
		{
			name: "FL out of range", frame: "FL7", wantKey: keyFL, wantOK: true,
		},
		{
			name: "IF full status", frame: sampleIF, wantKey: keyIF, wantOK: true,
			want: radio.Patch{
				Frequency: hz(14_025_000),
				Mode:      mode(radio.ModeCW),
				PTT:       yes(false),
			},
		},
		{
			name:    "IF while transmitting",
			frame:   replaceAt(sampleIF, ifTX, '1'),
			wantKey: keyIF, wantOK: true,
			want: radio.Patch{
				Frequency: hz(14_025_000),
				Mode:      mode(radio.ModeCW),
				PTT:       yes(true),
			},
		},
		{
			name:    "IF with a failed mode digit",
			frame:   replaceAt(sampleIF, ifMode, '0'),
			wantKey: keyIF, wantOK: true,
			want: radio.Patch{
				Frequency: hz(14_025_000),
				PTT:       yes(false),
			},
		},
		{
			// Anything but exactly 37 characters is line corruption. The key
			// still completes the pending transaction, but nothing is applied.
			name: "IF truncated", frame: sampleIF[:20],
			wantKey: keyIF, wantOK: true,
		},
		{
			name: "IF overlong", frame: sampleIF + "0",
			wantKey: keyIF, wantOK: true,
		},
		{
			name: "KY buffer available", frame: "KY0", wantKey: keyKY, wantOK: true,
		},
		{
			name: "KY buffer full", frame: "KY1", wantKey: keyKY, wantOK: true,
		},
		{
			name: "KS keyer speed", frame: "KS025", wantKey: keyKS, wantOK: true,
		},
		{
			name: "ID TS-590SG", frame: "ID023", wantKey: keyID, wantOK: true,
		},
		{
			name: "AI acknowledged", frame: "AI2", wantKey: keyAI, wantOK: true,
		},
		{
			// TX/RX arrive unsolicited under AI. They are the only way to see
			// PTT while the rig is in Data mode.
			name: "TX push", frame: "TX0", wantKey: keyTX, wantOK: true,
			want: radio.Patch{PTT: yes(true)},
		},
		{
			name: "TX tune push", frame: "TX2", wantKey: keyTX, wantOK: true,
			want: radio.Patch{PTT: yes(true)},
		},
		{
			name: "RX push", frame: "RX0", wantKey: keyRX, wantOK: true,
			want: radio.Patch{PTT: yes(false)},
		},
		{
			name: "syntax error answer", frame: "?", wantKey: keyErrSyntax, wantOK: false,
		},
		{
			name: "serial error answer", frame: "E", wantKey: keyErrComm, wantOK: false,
		},
		{
			name: "processing incomplete answer", frame: "O", wantKey: keyErrBusy, wantOK: false,
		},
		{
			// Everything unrecognised is unsolicited and harmless: this same
			// path carries AI push traffic, which is by definition whatever the
			// operator touched.
			name: "unknown command", frame: "AG0100",
			wantKey: backend.KeyUnsolicited, wantOK: true,
		},
		{
			name: "not a command at all", frame: "HELLOWORLD",
			wantKey: backend.KeyUnsolicited, wantOK: true,
		},
		{
			name: "empty frame", frame: "",
			wantKey: backend.KeyUnsolicited, wantOK: true,
		},
		{
			name: "lower case is accepted", frame: "md3", wantKey: keyMD, wantOK: true,
			want: radio.Patch{Mode: mode(radio.ModeCW)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			if tt.priorMode != radio.ModeUnknown {
				k.mode.Store(uint32(tt.priorMode))
			}

			u, err := k.Decode([]byte(tt.frame))
			if err != nil {
				t.Fatalf("Decode(%q) errored: %v — Decode must never error", tt.frame, err)
			}
			if u.Key != tt.wantKey {
				t.Errorf("key = %q, want %q", u.Key, tt.wantKey)
			}
			if u.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v", u.OK, tt.wantOK)
			}
			if string(u.Raw) != tt.frame {
				t.Errorf("Raw = %q, want %q", u.Raw, tt.frame)
			}
			comparePatch(t, u.Patch, tt.want)
		})
	}
}

// TestDecodeRawIsOwned guards the reason Decode clones its input: the session's
// scanner reuses one read buffer, so an Update that pointed into it would change
// under a logger's feet.
func TestDecodeRawIsOwned(t *testing.T) {
	k := newRig(t, 2, true)
	buf := []byte("MD3")
	u, err := k.Decode(buf)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	copy(buf, "XXX")
	if string(u.Raw) != "MD3" {
		t.Errorf("Raw = %q after the caller reused its buffer, want %q", u.Raw, "MD3")
	}
}

// TestDecodeTracksModeAndDataMode covers the two values the backend carries
// between calls.
func TestDecodeTracksModeAndDataMode(t *testing.T) {
	k := newRig(t, 2, true)

	if got := k.lastMode(); got != radio.ModeUnknown {
		t.Errorf("initial mode = %s, want unknown", got)
	}

	mustDecode(t, k, "MD5")
	if got := k.lastMode(); got != radio.ModeAM {
		t.Errorf("after MD5, mode = %s, want AM", got)
	}

	// The mode digit inside IF has to update it too, or a bulk-polled rig would
	// scale its power percentage against a stale mode forever.
	mustDecode(t, k, sampleIF)
	if got := k.lastMode(); got != radio.ModeCW {
		t.Errorf("after IF, mode = %s, want CW", got)
	}

	mustDecode(t, k, "DA1")
	if !k.dataMode.Load() {
		t.Error("after DA1, data mode not recorded")
	}
	if k.useBulkPoll() {
		t.Error("bulk poll still enabled in data mode; IF; will not answer there")
	}

	mustDecode(t, k, "DA0")
	if k.dataMode.Load() {
		t.Error("after DA0, data mode still recorded")
	}
	if !k.useBulkPoll() {
		t.Error("bulk poll not re-enabled after leaving data mode")
	}
}

// TestDecodeIFClearsBlocked covers the other half of the retry cadence: a good
// IF answer proves the rig is answering IF; again.
func TestDecodeIFClearsBlocked(t *testing.T) {
	k := newRig(t, 2, true)
	k.ifBlocked.Store(true)
	mustDecode(t, k, sampleIF)
	if k.ifBlocked.Load() {
		t.Error("a well-formed IF answer did not clear the blocked flag")
	}

	// A truncated one must not, since it is no evidence either way.
	k.ifBlocked.Store(true)
	mustDecode(t, k, sampleIF[:20])
	if !k.ifBlocked.Load() {
		t.Error("a truncated IF answer cleared the blocked flag")
	}
}

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		frame   string
		wantCmd string
		wantArg string
		wantOK  bool
	}{
		{"FA00014025000", "FA", "00014025000", true},
		{"md3", "MD", "3", true},
		{"IF" + strings.Repeat("0", 35), "IF", strings.Repeat("0", 35), true},
		{"SM00015", "SM", "00015", true},
		{"ABC1", "ABC", "1", true},
		{"A", "", "", false},          // one letter cannot be a command
		{"ABCD1", "", "", false},      // four is not a command either
		{"1234", "", "", false},       // no leading letters at all
		{"\xffFA0001", "", "", false}, // noise Split did not manage to trim
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

// --- helpers ---------------------------------------------------------------

func mustDecode(t *testing.T, k *Rig, frame string) backend.Update {
	t.Helper()
	u, err := k.Decode([]byte(frame))
	if err != nil {
		t.Fatalf("Decode(%q): %v", frame, err)
	}
	return u
}

func power(watts float64, pct float64) *radio.Power {
	return &radio.Power{Watts: &watts, Pct: pct, Native: int(watts)}
}

// replaceAt returns s with the byte at index i replaced, for building IF
// variants without restating all 37 characters.
func replaceAt(s string, i int, c byte) string {
	b := []byte(s)
	b[i] = c
	return string(b)
}

func comparePatch(t *testing.T, got, want radio.Patch) {
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
	cmp("CW", got.CW, want.CW)
	cmp("Connected", got.Connected, want.Connected)

	compareMeter(t, "SMeter", got.SMeter, want.SMeter)
	compareMeter(t, "SWR", got.SWR, want.SWR)
	compareMeter(t, "ALC", got.ALC, want.ALC)
	comparePower(t, got.Power, want.Power)
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

func comparePower(t *testing.T, got, want *radio.Power) {
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
	switch {
	case got.Watts == nil && want.Watts == nil:
	case got.Watts == nil || want.Watts == nil:
		t.Errorf("patch.Power.Watts = %v, want %v", got.Watts, want.Watts)
	case *got.Watts != *want.Watts:
		t.Errorf("patch.Power.Watts = %v, want %v", *got.Watts, *want.Watts)
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
