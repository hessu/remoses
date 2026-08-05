package kenwood

import (
	"context"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// TestMorseSenderInterface fails at compile time if the backend stops satisfying
// the optional interface the CW layer type-asserts for.
func TestMorseSenderInterface(t *testing.T) {
	var _ backend.MorseSender = (*Rig)(nil)
	var _ backend.Rig = (*Rig)(nil)
}

func TestSendChunkFraming(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{
			// P2 is a fixed 24 characters. The pad spaces are not keyed, so
			// this costs wire bytes and nothing else.
			name: "short chunk is padded to the full width",
			text: "CQ",
			want: "KY CQ                      ;",
		},
		{
			name: "single character",
			text: "E",
			want: "KY E                       ;",
		},
		{
			name: "exactly full width takes no padding",
			text: "CQ CQ DE OH2XYZ OH2XYZ K",
			want: "KY CQ CQ DE OH2XYZ OH2XYZ K;",
		},
		{
			name: "empty chunk is all padding",
			text: "",
			want: "KY                         ;",
		},
		{
			name: "trailing space in the text survives",
			text: "TEST ",
			want: "KY TEST                    ;",
		},
		{
			name: "prosign symbols pass through",
			text: `DE OH2XYZ _ [ > ] < \ # %`[:24],
			want: "KY " + `DE OH2XYZ _ [ > ] < \ #` + " ;",
		},
		{
			name: "full punctuation set",
			text: `?/=.,:'"()*+-@`,
			want: "KY " + `?/=.,:'"()*+-@` + strings.Repeat(" ", 10) + ";",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, nil)
			if err := k.SendChunk(context.Background(), c, tt.text); err != nil {
				t.Fatalf("SendChunk(%q): %v", tt.text, err)
			}
			c.wantSent(t, tt.want)

			// Every KY set command is "KY", the space that is P1, exactly
			// MaxChunk characters of P2, then the terminator.
			if got := len(tt.want); got != 3+MaxChunk+1 {
				t.Fatalf("command is %d bytes, want %d", got, 3+MaxChunk+1)
			}
			if !strings.HasPrefix(tt.want, "KY ") {
				t.Errorf("P1 must always be a space in the Set 1 form, got %q", tt.want[:3])
			}
			if !strings.HasSuffix(tt.want, ";") {
				t.Error("command is not terminated")
			}
		})
	}
}

func TestSendChunkRejects(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantWord string
	}{
		{
			// A smuggled semicolon would terminate the command early and leave
			// the rest of the text being parsed as commands.
			name: "semicolon", text: "CQ ; DE", wantWord: "terminate",
		},
		{
			name: "semicolon at the end", text: "CQ;", wantWord: "terminate",
		},
		{
			name: "too long", text: "CQ CQ CQ DE OH2XYZ OH2XYZ K", wantWord: "at most",
		},
		{
			// The rig silently mangles what it cannot key, so the check has to
			// happen here.
			name: "underscore is fine but tilde is not", text: "CQ ~ DE", wantWord: "cannot key",
		},
		{"caret leaks through unencoded", "CQ ^AR", "cannot key"},
		{"newline", "CQ\nDE", "cannot key"},
		{"non-ASCII", "CQ Ä", "cannot key"},
		{"exclamation mark", "HI!", "cannot key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, nil)
			err := k.SendChunk(context.Background(), c, tt.text)
			if err == nil {
				t.Fatalf("SendChunk(%q) accepted text the rig cannot send", tt.text)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain the problem (want %q in it)", err, tt.wantWord)
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q despite rejecting the text", c.sent)
			}
		})
	}
}

func TestCharset(t *testing.T) {
	k := newRig(t, 2, true)

	if k.MaxChunk() != 24 {
		t.Errorf("MaxChunk = %d, want 24", k.MaxChunk())
	}
	if strings.Contains(k.Charset(), ";") {
		t.Error("charset contains ';', which would terminate the KY command")
	}
	if strings.Contains(k.Charset(), "^") {
		t.Error("charset contains '^': that is the canonical prosign marker, consumed by EncodeProsigns, not a character the rig keys")
	}

	// Everything the reference lists for P2, plus the eight prosign symbols.
	for _, want := range []string{
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
		"abcdefghijklmnopqrstuvwxyz",
		"0123456789",
		` '"()*+,-./:=?@`,
		`_[>]<\#%`,
	} {
		for i := 0; i < len(want); i++ {
			if !strings.ContainsRune(k.Charset(), rune(want[i])) {
				t.Errorf("charset is missing %q", want[i])
			}
		}
	}

	// Duplicates would make the charset misleading where the API quotes it back.
	seen := map[byte]bool{}
	for i := 0; i < len(Charset); i++ {
		if seen[Charset[i]] {
			t.Errorf("charset lists %q twice", Charset[i])
		}
		seen[Charset[i]] = true
	}
}

func TestEncodeProsigns(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no prosigns", "CQ CQ DE OH2XYZ K", "CQ CQ DE OH2XYZ K"},
		{"empty", "", ""},
		{"AR", "TU ^AR", "TU _"},
		{"BT", "^BT", "["},
		{"SK", "73 ^SK", "73 >"},
		{"KN", "OH2XYZ ^KN", "OH2XYZ ]"},
		{"AS", "^AS", "<"},
		{"BK", "^BK", `\`},
		{"HH", "^HH", "#"},
		{"SN", "^SN", "%"},
		{"several in one string", "^BT DE ^AR ^SK", "[ DE _ >"},
		{"back to back", "^AR^SK", "_>"},
		{"lower case is accepted", "^ar", "_"},
		{"mixed case", "^Sk", ">"},
		{"at the very start", "^BTCQ", "[CQ"},
		{"at the very end", "CQ^SK", "CQ>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			got, err := k.EncodeProsigns(tt.in)
			if err != nil {
				t.Fatalf("EncodeProsigns(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("EncodeProsigns(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Whatever comes out must be sendable, or the translation has
			// produced something the rig will mangle.
			if err := validateChunk(got[:min(len(got), MaxChunk)]); err != nil {
				t.Errorf("encoded text is not sendable: %v", err)
			}
		})
	}
}

func TestEncodeProsignsErrors(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantWord string
	}{
		// VA is a common alternative spelling of SK, and the rig has no
		// abbreviation for it. Saying so beats keying "VA".
		{"unsupported prosign", "73 ^VA", "not one the TS-590 can key"},
		{"CT is not in the table", "^CT", "not one the TS-590 can key"},
		{"digits after the marker", "^12", "not one the TS-590 can key"},
		{"bare marker at the end", "CQ ^", "truncated"},
		{"one letter at the end", "CQ ^A", "truncated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			got, err := k.EncodeProsigns(tt.in)
			if err == nil {
				t.Fatalf("EncodeProsigns(%q) = %q, want an error", tt.in, got)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain the problem (want %q in it)", err, tt.wantWord)
			}
			// The error has to name the alternatives, since the client cannot
			// know a rig's prosign dialect.
			if !strings.Contains(err.Error(), "^AR") {
				t.Errorf("error %q does not list the supported prosigns", err)
			}
		})
	}
}

// TestProsignSymbolsMatchTheReference pins the mapping itself: it is the one
// place a transcription slip would produce silently wrong Morse on the air.
func TestProsignSymbolsMatchTheReference(t *testing.T) {
	want := map[string]byte{
		"BT": '[', "SK": '>',
		"AR": '_', "KN": ']',
		"AS": '<', "BK": '\\',
		"HH": '#', "SN": '%',
	}
	if len(prosignSymbols) != len(want) {
		t.Fatalf("prosign table has %d entries, the reference lists %d", len(prosignSymbols), len(want))
	}
	for name, sym := range want {
		if got, ok := prosignSymbols[name]; !ok || got != sym {
			t.Errorf("prosign %s maps to %q, want %q", name, got, sym)
		}
		if !strings.ContainsRune(Charset, rune(sym)) {
			t.Errorf("prosign symbol %q for %s is missing from the charset", sym, name)
		}
		if !strings.Contains(supportedProsigns, "^"+name) {
			t.Errorf("prosign %s is missing from the error message list", name)
		}
	}
}

func TestBufferFree(t *testing.T) {
	tests := []struct {
		name     string
		answer   string
		wantFree int
		wantOK   bool
		wantErr  bool
	}{
		{"space available", "KY0", MaxChunk, true, false},
		{"no space", "KY1", 0, true, false},
		{"unexpected value", "KY7", 0, false, true},
		{"short answer", "KY", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, map[string]string{reqKY: tt.answer})

			free, ok, err := k.BufferFree(context.Background(), c)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BufferFree accepted answer %q", tt.answer)
				}
				return
			}
			if err != nil {
				t.Fatalf("BufferFree: %v", err)
			}
			if free != tt.wantFree || ok != tt.wantOK {
				t.Fatalf("= (%d, %v), want (%d, %v)", free, ok, tt.wantFree, tt.wantOK)
			}
			c.wantSent(t, "KY;")
		})
	}
}

// TestBufferFreeIsQueryable is the difference from Icom, and the reason the
// Kenwood pacing loop can be closed rather than estimated.
func TestBufferFreeIsQueryable(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, map[string]string{reqKY: "KY0"})
	_, ok, err := k.BufferFree(context.Background(), c)
	if err != nil || !ok {
		t.Fatalf("BufferFree reported unqueryable (ok=%v, err=%v); the TS-590 answers KY;", ok, err)
	}
}

func TestAbort(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, nil)
	if err := k.Abort(context.Background(), c); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	// Set 2: any P1 other than 0 is an error, so there is exactly one form.
	c.wantSent(t, "KY0;")
}

func TestSetSpeed(t *testing.T) {
	tests := []struct {
		wpm  int
		want string
	}{
		{25, "KS025;"},
		{4, "KS004;"},
		{60, "KS060;"},
		{5, "KS005;"},
		// "An entered value of 003 or lower results in 004 being entered. A
		// value of 061 or higher results in 060." Clamping here keeps the speed
		// remoses reports equal to the speed the rig is keying.
		{3, "KS004;"},
		{0, "KS004;"},
		{-5, "KS004;"},
		{61, "KS060;"},
		{1000, "KS060;"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			k := newRig(t, 2, true)
			c := newTestConn(t, k, nil)
			if err := k.SetSpeed(context.Background(), c, tt.wpm); err != nil {
				t.Fatalf("SetSpeed(%d): %v", tt.wpm, err)
			}
			c.wantSent(t, tt.want)
		})
	}
}

// TestCWChunkLifecycle walks one chunk through the loop the CW pacer will run:
// translate prosigns, check for space, send.
func TestCWChunkLifecycle(t *testing.T) {
	k := newRig(t, 2, true)
	c := newTestConn(t, k, map[string]string{reqKY: "KY0"})
	ctx := context.Background()

	text, err := k.EncodeProsigns("TU 73 ^SK")
	if err != nil {
		t.Fatalf("EncodeProsigns: %v", err)
	}
	free, ok, err := k.BufferFree(ctx, c)
	if err != nil || !ok || free < len(text) {
		t.Fatalf("BufferFree = (%d, %v, %v)", free, ok, err)
	}
	if err := k.SendChunk(ctx, c, text); err != nil {
		t.Fatalf("SendChunk: %v", err)
	}
	c.wantSent(t, "KY;", "KY TU 73 >                 ;")
}
