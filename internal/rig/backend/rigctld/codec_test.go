package rigctld

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// scanAll runs the real split function over a stream and returns every frame it
// produced, which is what the session's reader does.
func scanAll(t *testing.T, stream string) []string {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(stream))
	sc.Buffer(make([]byte, 0, 128), 64*1024)
	sc.Split(splitBlocks)
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
	return out
}

func TestSplitBlocks(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []string
	}{
		{
			name:   "one block",
			stream: "get_freq:\nFrequency: 14074000\nRPRT 0\n",
			want:   []string{"get_freq:\nFrequency: 14074000\nRPRT 0"},
		},
		{
			name:   "several blocks in one read",
			stream: "get_freq:\nFrequency: 1\nRPRT 0\nget_ptt:\nPTT: 0\nRPRT 0\n",
			want: []string{
				"get_freq:\nFrequency: 1\nRPRT 0",
				"get_ptt:\nPTT: 0\nRPRT 0",
			},
		},
		{
			name:   "set command with no values",
			stream: "set_ptt: 1\nRPRT 0\n",
			want:   []string{"set_ptt: 1\nRPRT 0"},
		},
		{
			name:   "error block",
			stream: "get_level: STRENGTH\nRPRT -11\n",
			want:   []string{"get_level: STRENGTH\nRPRT -11"},
		},
		{
			// A daemon that has just been connected to, or a stream joined
			// mid-response, puts bytes in front of the first whole block.
			name:   "leading garbage joins the first block",
			stream: "\x00\x01junk\nget_freq:\nFrequency: 7\nRPRT 0\n",
			want:   []string{"\x00\x01junk\nget_freq:\nFrequency: 7\nRPRT 0"},
		},
		{
			name:   "CRLF line endings",
			stream: "get_freq:\r\nFrequency: 14074000\r\nRPRT 0\r\n",
			want:   []string{"get_freq:\r\nFrequency: 14074000\r\nRPRT 0"},
		},
		{
			// The Morse text is echoed back, so a block must not be terminated
			// by an RPRT that is merely quoted inside one.
			name:   "RPRT inside an echoed argument does not terminate",
			stream: "send_morse: RPRT 0\nRPRT 0\n",
			want:   []string{"send_morse: RPRT 0\nRPRT 0"},
		},
		{
			name:   "empty lines inside a block are kept",
			stream: "dump_state:\n1\n\n2\nRPRT 0\n",
			want:   []string{"dump_state:\n1\n\n2\nRPRT 0"},
		},
		{
			// The daemon went away mid-response. There is nothing to report.
			name:   "unterminated tail at EOF is dropped",
			stream: "get_freq:\nFrequency: 14074000\n",
			want:   nil,
		},
		{
			name:   "empty stream",
			stream: "",
			want:   nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := scanAll(t, tc.stream)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d frames %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("frame %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSplitPartialReads feeds the stream one byte at a time, which is what a
// slow socket looks like: every intermediate call must ask for more rather than
// invent a frame.
func TestSplitPartialReads(t *testing.T) {
	stream := "get_mode:\nMode: USB\nPassband: 2400\nRPRT 0\n"
	data := []byte(stream)

	for i := range len(data) - 1 {
		adv, tok, err := splitBlocks(data[:i], false)
		if err != nil {
			t.Fatalf("prefix of %d bytes: %v", i, err)
		}
		if tok != nil || adv != 0 {
			t.Fatalf("prefix of %d bytes produced a frame (%d, %q)", i, adv, tok)
		}
	}

	adv, tok, err := splitBlocks(data, false)
	if err != nil {
		t.Fatalf("full stream: %v", err)
	}
	if adv != len(data) {
		t.Errorf("advance = %d, want %d", adv, len(data))
	}
	if string(tok) != strings.TrimSuffix(stream, "\n") {
		t.Errorf("frame = %q", tok)
	}
}

// TestSplitOversizedBlock proves the size guard. The session's scanner drops
// the connection on a token above 64 kB, so a block that never terminates has
// to be surrendered before it gets there.
func TestSplitOversizedBlock(t *testing.T) {
	data := bytes.Repeat([]byte("x\n"), maxBlock)
	adv, tok, err := splitBlocks(data, false)
	if err != nil {
		t.Fatalf("splitBlocks: %v", err)
	}
	if adv != len(data) || len(tok) != len(data) {
		t.Fatalf("advance = %d, token = %d bytes; want both %d", adv, len(tok), len(data))
	}

	// Below the guard it still waits for more.
	small := bytes.Repeat([]byte("x\n"), 8)
	if adv, tok, _ := splitBlocks(small, false); adv != 0 || tok != nil {
		t.Fatalf("small buffer produced (%d, %q), want (0, nil)", adv, tok)
	}
}

// TestSplitMethod proves the interface method is the same function the tests
// above exercise directly.
func TestSplitMethod(t *testing.T) {
	g := newRig(t)
	data := []byte("get_freq:\nFrequency: 7\nRPRT 0\n")
	adv, tok, err := g.Split(data, false)
	if err != nil || adv != len(data) || string(tok) != "get_freq:\nFrequency: 7\nRPRT 0" {
		t.Fatalf("Split = (%d, %q, %v)", adv, tok, err)
	}
}

func TestDecode(t *testing.T) {
	u64 := func(v uint64) *uint64 { return &v }
	i := func(v int) *int { return &v }
	b := func(v bool) *bool { return &v }
	m := func(v radio.Mode) *radio.Mode { return &v }

	tests := []struct {
		name  string
		frame string
		key   backend.Key
		ok    bool
		patch radio.Patch
	}{
		{
			name:  "get_freq",
			frame: resp("get_freq:", "Frequency: 14074000", "RPRT 0"),
			key:   keyGetFreq,
			ok:    true,
			patch: radio.Patch{Frequency: u64(14074000)},
		},
		{
			name:  "get_mode with passband",
			frame: resp("get_mode:", "Mode: USB", "Passband: 2400", "RPRT 0"),
			key:   keyGetMode,
			ok:    true,
			patch: radio.Patch{Mode: m(radio.ModeUSB), DataMode: b(false), PassbandHz: i(2400)},
		},
		{
			// PKTUSB is USB with the data flag, since remoses keeps data mode
			// orthogonal to the mode.
			name:  "get_mode splits PKTUSB into mode plus data",
			frame: resp("get_mode:", "Mode: PKTUSB", "Passband: 3000", "RPRT 0"),
			key:   keyGetMode,
			ok:    true,
			patch: radio.Patch{Mode: m(radio.ModeUSB), DataMode: b(true), PassbandHz: i(3000)},
		},
		{
			// RIG_PASSBAND_NORMAL is a request for the default width, not a
			// measurement of one.
			name:  "get_mode with a zero passband reports no width",
			frame: resp("get_mode:", "Mode: CW", "Passband: 0", "RPRT 0"),
			key:   keyGetMode,
			ok:    true,
			patch: radio.Patch{Mode: m(radio.ModeCW), DataMode: b(false)},
		},
		{
			// A mode remoses has no word for still completes the transaction.
			name:  "get_mode with an unmappable mode",
			frame: resp("get_mode:", "Mode: WFM", "Passband: 180000", "RPRT 0"),
			key:   keyGetMode,
			ok:    true,
			patch: radio.Patch{PassbandHz: i(180000)},
		},
		{
			name:  "get_ptt off",
			frame: resp("get_ptt:", "PTT: 0", "RPRT 0"),
			key:   keyGetPTT,
			ok:    true,
			patch: radio.Patch{PTT: b(false)},
		},
		{
			// ptt_t 3 is RIG_PTT_ON_DATA: still transmitting.
			name:  "get_ptt on the data input",
			frame: resp("get_ptt:", "PTT: 3", "RPRT 0"),
			key:   keyGetPTT,
			ok:    true,
			patch: radio.Patch{PTT: b(true)},
		},
		{
			// get_level answers with a bare number: the echo is the only thing
			// that says which level it belongs to.
			name:  "get_level RFPOWER, unlabelled value",
			frame: resp("get_level: RFPOWER", "0.5", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelRFPOWER),
			ok:    true,
			patch: radio.Patch{Power: &radio.Power{Pct: 50, Native: 50}},
		},
		{
			name:  "get_level STRENGTH",
			frame: resp("get_level: STRENGTH", "-12", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelSTRENGTH),
			ok:    true,
			patch: radio.Patch{SMeter: ptrMeter(meterFromStrength(-12))},
		},
		{
			// The keying speed has no home in radio.Patch; the CW layer owns it.
			name:  "get_level KEYSPD carries no patch",
			frame: resp("get_level: KEYSPD", "25", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelKEYSPD),
			ok:    true,
		},
		{
			// The echo is written before the command runs, so a successful RPRT
			// is what makes it safe to believe.
			name:  "set_freq patches State from its own echo",
			frame: resp("set_freq: 7100000", "RPRT 0"),
			key:   keySetFreq,
			ok:    true,
			patch: radio.Patch{Frequency: u64(7100000)},
		},
		{
			name:  "a rejected set_freq patches nothing",
			frame: resp("set_freq: 7100000", "RPRT -1"),
			key:   keySetFreq,
			ok:    false,
		},
		{
			name:  "set_ptt patches State from its own echo",
			frame: resp("set_ptt: 1", "RPRT 0"),
			key:   keySetPTT,
			ok:    true,
			patch: radio.Patch{PTT: b(true)},
		},
		{
			name:  "set_mode patches mode and data mode",
			frame: resp("set_mode: PKTLSB 3000", "RPRT 0"),
			key:   keySetMode,
			ok:    true,
			patch: radio.Patch{Mode: m(radio.ModeLSB), DataMode: b(true), PassbandHz: i(3000)},
		},
		{
			name:  "set_level RFPOWER",
			frame: resp("set_level: RFPOWER 0.250", "RPRT 0"),
			key:   levelKey(cmdSetLevel, levelRFPOWER),
			ok:    true,
			patch: radio.Patch{Power: &radio.Power{Pct: 25, Native: 25}},
		},
		{
			name:  "set_level KEYSPD carries no patch",
			frame: resp("set_level: KEYSPD 28", "RPRT 0"),
			key:   levelKey(cmdSetLevel, levelKEYSPD),
			ok:    true,
		},
		{
			name:  "send_morse",
			frame: resp("send_morse: CQ TEST", "RPRT 0"),
			key:   keySendMorse,
			ok:    true,
		},
		{
			name:  "stop_morse",
			frame: resp("stop_morse:", "RPRT 0"),
			key:   keyStopMorse,
			ok:    true,
		},
		{
			// -11 is RIG_ENAVAIL: the rig's Hamlib backend has no such function.
			name:  "a rejection keys the transaction so it fails fast",
			frame: resp("get_level: STRENGTH", "RPRT -11"),
			key:   levelKey(cmdGetLevel, levelSTRENGTH),
			ok:    false,
		},
		{
			name:  "an unrecognised block is unsolicited and harmless",
			frame: resp("something_else: 1", "RPRT 0"),
			key:   backend.KeyUnsolicited,
			ok:    true,
		},
		{
			name:  "pure garbage is unsolicited",
			frame: "\x00\xff not a protocol at all",
			key:   backend.KeyUnsolicited,
			ok:    false,
		},
		{
			// The size guard hands over a block with no terminator. Reporting it
			// as a failure completes the transaction instead of stalling it.
			name:  "a block with no RPRT is not OK",
			frame: resp("get_freq:", "Frequency: 14074000"),
			key:   keyGetFreq,
			ok:    false,
			patch: radio.Patch{Frequency: u64(14074000)},
		},
		{
			name:  "an unparsable value completes the transaction with nothing to say",
			frame: resp("get_freq:", "Frequency: not-a-number", "RPRT 0"),
			key:   keyGetFreq,
			ok:    true,
		},
		{
			name:  "an unparsable PTT value",
			frame: resp("get_ptt:", "PTT: yes", "RPRT 0"),
			key:   keyGetPTT,
			ok:    true,
		},
		{
			name:  "an unparsable power level",
			frame: resp("get_level: RFPOWER", "quite a lot", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelRFPOWER),
			ok:    true,
		},
		{
			name:  "an unparsable S-meter reading",
			frame: resp("get_level: STRENGTH", "loud", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelSTRENGTH),
			ok:    true,
		},
		{
			// A level this backend does not model completes its transaction and
			// says nothing about State. SWR used to be the example here and is
			// modelled now, so this uses one that is not.
			name:  "a level with no home in State",
			frame: resp("get_level: COMP", "0.4", "RPRT 0"),
			key:   levelKey(cmdGetLevel, "COMP"),
			ok:    true,
		},
		{
			// SWR arrives as a ratio rather than a deflection, so it fills both
			// the bar and the exact figure.
			name:  "SWR is a ratio",
			frame: resp("get_level: SWR", "1.2", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelSWR),
			ok:    true,
			patch: radio.Patch{
				SWR:      ptrMeter(radio.Meter{Raw: 12, Scale: swrBarTop}),
				SWRRatio: ptrFloat(1.2),
			},
		},
		{
			name:  "forward power arrives as a fraction of maximum",
			frame: resp("get_level: RFPOWER_METER", "0.75", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelRFPOWERMETER),
			ok:    true,
			patch: radio.Patch{
				PowerMeter: ptrMeter(radio.Meter{Raw: 75, Scale: fractionScale}),
			},
		},
		{
			name:  "a get_level answer with no value at all",
			frame: resp("get_level: RFPOWER", "RPRT 0"),
			key:   levelKey(cmdGetLevel, levelRFPOWER),
			ok:    true,
		},
		{
			name:  "empty frame",
			frame: "",
			key:   backend.KeyUnsolicited,
			ok:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t)
			u, err := g.Decode([]byte(tc.frame))
			if err != nil {
				t.Fatalf("Decode must never error, got %v", err)
			}
			if u.Key != tc.key {
				t.Errorf("key = %q, want %q", u.Key, tc.key)
			}
			if u.OK != tc.ok {
				t.Errorf("OK = %v, want %v", u.OK, tc.ok)
			}
			if diff := patchDiff(u.Patch, tc.patch); diff != "" {
				t.Errorf("patch mismatch: %s", diff)
			}
		})
	}
}

// TestDecodeOwnsItsBytes proves Raw does not alias the scanner's buffer, which
// the session reuses between frames.
func TestDecodeOwnsItsBytes(t *testing.T) {
	g := newRig(t)
	buf := []byte(resp("get_freq:", "Frequency: 14074000", "RPRT 0"))
	u, _ := g.Decode(buf)
	copy(buf, bytes.Repeat([]byte("z"), len(buf)))
	if !strings.HasPrefix(string(u.Raw), "get_freq:") {
		t.Fatalf("Raw aliased the caller's buffer: %q", u.Raw)
	}
}

// TestDecodeRemembersModeToken proves the backend keeps the token verbatim,
// including one it cannot map, because SetFilterWidth has to hand it back.
func TestDecodeRemembersModeToken(t *testing.T) {
	g := newRig(t)
	for _, tc := range []struct{ frame, want string }{
		{resp("get_mode:", "Mode: PKTUSB", "Passband: 3000", "RPRT 0"), "PKTUSB"},
		{resp("get_mode:", "Mode: WFM", "Passband: 180000", "RPRT 0"), "WFM"},
	} {
		if _, err := g.Decode([]byte(tc.frame)); err != nil {
			t.Fatal(err)
		}
		got := g.modeName.Load()
		if got == nil || *got != tc.want {
			t.Errorf("mode token = %v, want %q", got, tc.want)
		}
	}
}

func TestParseRPRT(t *testing.T) {
	tests := []struct {
		line string
		code int
		ok   bool
	}{
		{"RPRT 0", 0, true},
		{"RPRT -11", -11, true},
		{"RPRT  -5", -5, true},
		{"RPRT", 0, false},     // no code: the block ends, but the outcome is unknown
		{"RPRT abc", 0, false}, // ditto
		{"RPRTX 0", 0, false},  // not the terminator at all
		{"Frequency: 1", 0, false},
	}
	for _, tc := range tests {
		code, ok := parseRPRT(tc.line)
		if ok != tc.ok || code != tc.code {
			t.Errorf("parseRPRT(%q) = (%d, %v), want (%d, %v)", tc.line, code, ok, tc.code, tc.ok)
		}
	}
}

func ptrMeter(m radio.Meter) *radio.Meter { return &m }

func ptrFloat(v float64) *float64 { return &v }
