package cw

import (
	"errors"
	"strings"
	"testing"
)

func chunkTexts(cs []chunk) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.encoded
	}
	return out
}

func mustChunks(t *testing.T, text string, maxChunk int, enc func(string) (string, error)) []chunk {
	t.Helper()
	cs, err := buildChunks(text, maxChunk, enc)
	if err != nil {
		t.Fatalf("buildChunks(%q, %d): %v", text, maxChunk, err)
	}
	// Whatever the split, the rig must receive exactly what was submitted.
	if got := strings.Join(chunkTexts(cs), ""); got != mustEncode(t, text, enc) {
		t.Fatalf("chunks rejoin to %q, want %q", got, mustEncode(t, text, enc))
	}
	for _, c := range cs {
		if len(c.encoded) > maxChunk {
			t.Fatalf("chunk %q is %d characters, over the %d limit", c.encoded, len(c.encoded), maxChunk)
		}
	}
	return cs
}

func mustEncode(t *testing.T, text string, enc func(string) (string, error)) string {
	t.Helper()
	// The encoders are pure substitutions, so encoding the whole string is the
	// same as encoding it word by word.
	s, err := enc(text)
	if err != nil {
		t.Fatalf("encode(%q): %v", text, err)
	}
	return s
}

func TestChunkOnWordBoundaries(t *testing.T) {
	cs := mustChunks(t, "CQ CQ DE OH2XYZ ^AR", 8, encodeIcom)
	want := []string{"CQ CQ DE", " OH2XYZ", " ^AR"}
	if got := chunkTexts(cs); !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	// A word gap parked at the end of a chunk is the one thing Kenwood's fixed
	// width block cannot carry, so no chunk may end in a space.
	for _, c := range cs {
		if strings.HasSuffix(c.encoded, " ") {
			t.Errorf("chunk %q ends with a space", c.encoded)
		}
	}
}

func TestChunkKeepsCanonicalTextForTiming(t *testing.T) {
	cs := mustChunks(t, "CQ ^AR", 24, encodeKenwood)
	if len(cs) != 1 {
		t.Fatalf("got %d chunks, want 1", len(cs))
	}
	if cs[0].encoded != "CQ _" {
		t.Errorf("encoded: got %q, want %q", cs[0].encoded, "CQ _")
	}
	if cs[0].canon != "CQ ^AR" {
		t.Errorf("canon: got %q, want %q", cs[0].canon, "CQ ^AR")
	}
	// Queue depth is reported in the characters the client submitted, not in
	// the rig's shorthand.
	if cs[0].chars != 6 {
		t.Errorf("chars: got %d, want 6", cs[0].chars)
	}
}

func TestChunkSplitsAWordOnlyWhenItMustAndOnlyBetweenCharacters(t *testing.T) {
	cs := mustChunks(t, "ABCDEFGHIJKL", 5, encodeIcom)
	if got, want := chunkTexts(cs), []string{"ABCDE", "FGHIJ", "KL"}; !equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}

	// The space before an over-long word travels with its first character, so
	// it cannot be stranded at the end of a chunk.
	cs = mustChunks(t, "DE ABCDEFGHIJKL", 5, encodeIcom)
	for _, c := range cs {
		if strings.HasSuffix(c.encoded, " ") {
			t.Errorf("chunk %q ends with a space", c.encoded)
		}
	}
}

func TestChunkNeverSplitsAProsignFromItsMarker(t *testing.T) {
	for _, maxChunk := range []int{5, 6, 7, 9, 12} {
		cs := mustChunks(t, "HELLO ^AR WORLD ^SK", maxChunk, encodeIcom)
		for _, c := range cs {
			if strings.HasSuffix(c.encoded, "^") {
				t.Errorf("maxChunk %d: chunk %q ends on a bare marker", maxChunk, c.encoded)
			}
			if i := strings.IndexByte(c.encoded, '^'); i >= 0 && i+1 < len(c.encoded) {
				if !isLetter(rune(c.encoded[i+1])) {
					t.Errorf("maxChunk %d: chunk %q has a marker with no prosign", maxChunk, c.encoded)
				}
			}
			// The whole prosign has to arrive in one piece: the rig keys the
			// run it is given with no gaps, so half of one is a different
			// character.
			for _, p := range []string{"^AR", "^SK"} {
				if i := strings.Index(c.encoded, p[:2]); i >= 0 && !strings.Contains(c.encoded, p) {
					t.Errorf("maxChunk %d: chunk %q carries part of %s", maxChunk, c.encoded, p)
				}
			}
		}
	}
}

func TestChunkRejectsAProsignTooLongForTheRig(t *testing.T) {
	// Icom keeps the '^', so a nine-letter run-together needs ten characters of
	// buffer. There is no way to split it, so it must be refused rather than
	// mangled.
	_, err := buildChunks("^ABCDEFGHI", 8, encodeIcom)
	if err == nil {
		t.Fatal("expected an error for a prosign longer than a chunk")
	}
}

func TestChunkReportsAnEncodingFailure(t *testing.T) {
	// Kenwood has a symbol for eight prosigns and no way to spell any other.
	if _, err := buildChunks("^XYZ", 24, encodeKenwood); err == nil {
		t.Fatal("expected an error for a prosign the rig cannot spell")
	}
}

func TestChunkEmptyAndSpaces(t *testing.T) {
	if cs, err := buildChunks("", 24, encodeIcom); err != nil || len(cs) != 0 {
		t.Errorf("empty text: got %d chunks, %v", len(cs), err)
	}
	cs := mustChunks(t, "   ", 24, encodeIcom)
	if len(cs) != 1 || cs[0].encoded != "   " {
		t.Errorf("got %q", chunkTexts(cs))
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		text   string
		char   rune
		offset int
		ok     bool
	}{
		{text: "CQ CQ DE OH2XYZ ^AR", ok: true},
		{text: "PSE QSY 14.055", ok: true},
		{text: "CQ; DE", char: ';', offset: 2},
		{text: "CQ ^ DE", char: '^', offset: 3},
		{text: "^", char: '^', offset: 0},
		{text: "CQ ^1", char: '^', offset: 3},
		{text: "OH2XYZä", char: 'ä', offset: 6},
	}
	for _, c := range cases {
		err := validate(asciiUpper(c.text), kenwoodCharset)
		if c.ok {
			if err != nil {
				t.Errorf("validate(%q): %v", c.text, err)
			}
			continue
		}
		var ce *CharError
		if !errors.As(err, &ce) {
			t.Errorf("validate(%q): got %v, want CharError", c.text, err)
			continue
		}
		if ce.Char != c.char || ce.Offset != c.offset {
			t.Errorf("validate(%q): got %q at %d, want %q at %d", c.text, ce.Char, ce.Offset, c.char, c.offset)
		}
		if ce.Charset != kenwoodCharset {
			t.Errorf("validate(%q): charset not reported", c.text)
		}
	}
}

func TestValidateAcceptsEitherCase(t *testing.T) {
	if err := validate("cq de oh2xyz", kenwoodCharset); err != nil {
		t.Errorf("lower case rejected: %v", err)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
