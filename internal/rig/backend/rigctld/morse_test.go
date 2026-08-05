package rigctld

import (
	"context"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// morseRig returns a backend that has been through Init against a rig whose
// dump says it can send Morse.
func morseRig(t *testing.T) (*Rig, *testConn) {
	t.Helper()
	g := newRig(t)
	c := newTestConn(t, g, initAnswers())
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.sent = nil
	return g, c
}

func TestMorseShape(t *testing.T) {
	g := newRig(t)
	if g.MaxChunk() != MaxChunk {
		t.Errorf("MaxChunk = %d, want %d", g.MaxChunk(), MaxChunk)
	}
	if g.Charset() != Charset {
		t.Errorf("Charset = %q", g.Charset())
	}
	// The shared pacing layer refuses a backend that cannot carry a prosign.
	if MaxChunk < 8 {
		t.Errorf("MaxChunk %d is below the pacing layer's floor", MaxChunk)
	}
	// A line break in the text would truncate the command and leave the rest to
	// be parsed as new commands, so it must not be sendable.
	for _, r := range Charset {
		if r == '\n' || r == '\r' {
			t.Errorf("the charset contains %q, which would break the command line", r)
		}
	}
}

// TestBufferFree pins the open-loop contract: Hamlib has no buffer query, so the
// pacing layer must fall back to its timing estimate.
func TestBufferFree(t *testing.T) {
	g, c := morseRig(t)
	free, ok, err := g.BufferFree(context.Background(), c)
	if err != nil {
		t.Fatalf("BufferFree: %v", err)
	}
	if ok {
		t.Errorf("BufferFree reported %d characters as knowable; Hamlib has no such query", free)
	}
	if len(c.sent) != 0 {
		t.Errorf("BufferFree went to the wire: %q", c.sent)
	}
}

func TestSendChunk(t *testing.T) {
	g, c := morseRig(t)
	c.answers["+b CQ CQ DE OH2XYZ\n"] = resp("send_morse: CQ CQ DE OH2XYZ", "RPRT 0")

	if err := g.SendChunk(context.Background(), c, "CQ CQ DE OH2XYZ"); err != nil {
		t.Fatalf("SendChunk: %v", err)
	}
	c.wantSent(t, "+b CQ CQ DE OH2XYZ\n")
}

// TestSendChunkNeedsSendMorse proves the capability is enforced where the
// interface cannot express it: MorseSender is satisfied statically, so the
// check has to live in the method.
func TestSendChunkNeedsSendMorse(t *testing.T) {
	g := newRig(t)
	answers := initAnswers()
	answers[reqDumpCaps] = resp(append([]string{"dump_caps:"},
		append(lines(strings.ReplaceAll(sampleDumpCaps, "Can send Morse:\tY", "Can send Morse:\tN")), "RPRT 0")...)...)
	c := newTestConn(t, g, answers)
	if err := g.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}

	c.sent = nil
	err := g.SendChunk(context.Background(), c, "TEST")
	if err == nil {
		t.Fatal("SendChunk keyed a rig whose dump says it cannot send Morse")
	}
	if !strings.Contains(err.Error(), "Can send Morse") {
		t.Errorf("error = %v, want it to quote the dump", err)
	}
	if len(c.sent) != 0 {
		t.Errorf("the command went to the wire anyway: %q", c.sent)
	}
}

// TestSendChunkValidates proves bad text is caught before it reaches the wire.
// Hamlib forwards the string unexamined, so this is the only place it can be.
func TestSendChunkValidates(t *testing.T) {
	g, c := morseRig(t)
	if err := g.SendChunk(context.Background(), c, "CQ\nDE"); err == nil {
		t.Fatal("SendChunk accepted text with a line break in it")
	}
	if len(c.sent) != 0 {
		t.Errorf("it went to the wire anyway: %q", c.sent)
	}
}

func TestSendChunkBeforeInit(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, nil)
	err := g.SendChunk(context.Background(), c, "TEST")
	if err == nil || !strings.Contains(err.Error(), "before its capabilities") {
		t.Fatalf("error = %v, want a refusal naming the missing capability read", err)
	}
}

func TestValidateChunk(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantErr string
	}{
		{name: "letters and digits", text: "CQ DE OH2XYZ 599"},
		{name: "the accepted punctuation", text: "A/B?C.D,E-F=G+"},
		{name: "empty", text: ""},
		{name: "exactly MaxChunk", text: strings.Repeat("A", MaxChunk)},

		{name: "too long", text: strings.Repeat("A", MaxChunk+1), wantErr: "at most"},
		{name: "a newline", text: "CQ\nDE", wantErr: "line break"},
		{name: "a carriage return", text: "CQ\rDE", wantErr: "line break"},
		// Lower case is folded by the CW layer before it gets here, so anything
		// still lower case at this point is a caller bypassing that.
		{name: "lower case", text: "cq", wantErr: "will not send"},
		{name: "an exotic character", text: "CQä", wantErr: "will not send"},
		{name: "a semicolon", text: "CQ;", wantErr: "will not send"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateChunk(tc.text)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateChunk(%q): %v", tc.text, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateChunk(%q) accepted it, want an error mentioning %q", tc.text, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestAbort(t *testing.T) {
	g, c := morseRig(t)
	c.answers[reqStopMorse] = resp("stop_morse:", "RPRT 0")

	if err := g.Abort(context.Background(), c); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// stop_morse has no printable command letter, so the long name is the only
	// way to reach it.
	c.wantSent(t, "+\\stop_morse\n")
}

func TestSetSpeed(t *testing.T) {
	// The sample rig's level_gran reports a 4..48 wpm keyer.
	tests := []struct {
		name string
		wpm  int
		want int
	}{
		{name: "inside the rig's range", wpm: 25, want: 25},
		{name: "below it is clamped", wpm: 1, want: 4},
		{name: "above it is clamped", wpm: 90, want: 48},
		{name: "at the floor", wpm: 4, want: 4},
		{name: "at the ceiling", wpm: 48, want: 48},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g, c := morseRig(t)
			req := "+L KEYSPD " + itoa(tc.want) + "\n"
			c.answers[req] = resp("set_level: KEYSPD "+itoa(tc.want), "RPRT 0")

			if err := g.SetSpeed(context.Background(), c, tc.wpm); err != nil {
				t.Fatalf("SetSpeed: %v", err)
			}
			c.wantSent(t, req)
		})
	}
}

// TestSetSpeedDefaultRange proves the fallback bounds apply when the daemon
// reports no level_gran, which most rig backends do not.
func TestSetSpeedDefaultRange(t *testing.T) {
	g := newRig(t)
	c := newTestConn(t, g, map[string]string{
		"+L KEYSPD 60\n": resp("set_level: KEYSPD 60", "RPRT 0"),
	})
	if err := g.SetSpeed(context.Background(), c, 200); err != nil {
		t.Fatalf("SetSpeed: %v", err)
	}
	c.wantSent(t, "+L KEYSPD 60\n")
}

func TestEncodeProsigns(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "no prosign", in: "CQ CQ DE OH2XYZ", want: "CQ CQ DE OH2XYZ"},
		// Hamlib defines no run-together marker, so the canonical form becomes
		// the plain letters and the spacing is the rig backend's business.
		{name: "AR", in: "^AR", want: "AR"},
		{name: "in context", in: "OH2XYZ ^SK", want: "OH2XYZ SK"},
		{name: "several", in: "^BT TEST ^AR", want: "BT TEST AR"},
		{name: "case folded", in: "^ar", want: "AR"},
		{name: "empty", in: "", want: ""},

		{name: "unknown prosign", in: "^ZZ", wantErr: "not one remoses can express"},
		{name: "truncated", in: "CQ ^A", wantErr: "truncated prosign"},
		{name: "a bare caret at the end", in: "CQ ^", wantErr: "truncated prosign"},
	}

	g := newRig(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := g.EncodeProsigns(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("EncodeProsigns(%q) = %q, want an error mentioning %q", tc.in, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("EncodeProsigns(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("EncodeProsigns(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEncodedProsignsAreSendable proves every prosign this backend claims comes
// out as characters it will then accept, which is the invariant the CW layer
// relies on when it validates after encoding.
func TestEncodedProsignsAreSendable(t *testing.T) {
	g := newRig(t)
	for name := range prosigns {
		got, err := g.EncodeProsigns("^" + name)
		if err != nil {
			t.Errorf("^%s: %v", name, err)
			continue
		}
		if err := validateChunk(got); err != nil {
			t.Errorf("^%s encodes to %q, which the charset rejects: %v", name, got, err)
		}
	}
}

// itoa keeps the test request strings readable without importing strconv for
// one call.
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

var _ backend.MorseSender = (*Rig)(nil)
