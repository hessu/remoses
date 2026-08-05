package morse

import (
	"errors"
	"strings"
	"testing"
)

func pattern(t *testing.T, r rune) string {
	t.Helper()
	els, ok := Encode(r)
	if !ok {
		t.Fatalf("Encode(%q): not in table", r)
	}
	var b strings.Builder
	for _, e := range els {
		b.WriteString(e.String())
	}
	return b.String()
}

func TestElementTable(t *testing.T) {
	cases := map[rune]string{
		'A':  ".-",
		'E':  ".",
		'K':  "-.-",
		'O':  "---",
		'Q':  "--.-",
		'T':  "-",
		'Z':  "--..",
		'0':  "-----",
		'5':  ".....",
		'9':  "----.",
		'?':  "..--..",
		'/':  "-..-.",
		'=':  "-...-",
		'@':  ".--.-.",
		',':  "--..--",
		'.':  ".-.-.-",
		'-':  "-....-",
		'\'': ".----.",
		'"':  ".-..-.",
		'(':  "-.--.",
		')':  "-.--.-",
		'+':  ".-.-.",
		':':  "---...",
		'*':  "-..-",
	}
	for r, want := range cases {
		if got := pattern(t, r); got != want {
			t.Errorf("%q: got %q, want %q", r, got, want)
		}
	}
}

func TestEncodeIsCaseInsensitive(t *testing.T) {
	if pattern(t, 'a') != pattern(t, 'A') {
		t.Fatal("lower case must encode as upper case")
	}
}

func TestCharsetMatchesTable(t *testing.T) {
	for _, r := range Charset() {
		if r == ' ' {
			continue
		}
		if _, ok := table[r]; !ok {
			t.Errorf("charset lists %q but the table has no entry", r)
		}
	}
	if !strings.ContainsRune(Charset(), ' ') {
		t.Error("charset must contain space: a word gap is keyable")
	}
	// +1 for the space, which is a gap rather than a table entry.
	if len(Charset()) != len(table)+1 {
		t.Errorf("charset has %d runes, table has %d entries", len(Charset()), len(table))
	}
}

func TestProsigns(t *testing.T) {
	// A prosign is its letters run together, so AR is A followed by R with no
	// gap: .- then .-. gives .-.-.
	els, ok := Prosign("AR")
	if !ok {
		t.Fatal("Prosign(AR): not ok")
	}
	var b strings.Builder
	for _, e := range els {
		b.WriteString(e.String())
	}
	if got, want := b.String(), ".-.-."; got != want {
		t.Errorf("AR: got %q, want %q", got, want)
	}
	if _, ok := Prosign("A1"); ok {
		t.Error("Prosign(A1): a digit is not a prosign letter")
	}
	if _, ok := Prosign(""); ok {
		t.Error("Prosign(\"\"): empty must not be ok")
	}

	want := []string{"AR", "AS", "BK", "BT", "HH", "KN", "SK", "SN"}
	have := map[string]bool{}
	for _, n := range ProsignNames() {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("conventional prosign %s is not named", n)
		}
		if _, ok := ProsignMeaning(n); !ok {
			t.Errorf("prosign %s has no meaning", n)
		}
	}
	// HH is eight dits, which only falls out if a prosign really is a letter run.
	if got := len(mustProsign(t, "HH")); got != 8 {
		t.Errorf("HH: got %d elements, want 8", got)
	}
}

func mustProsign(t *testing.T, name string) []Element {
	t.Helper()
	els, ok := Prosign(name)
	if !ok {
		t.Fatalf("Prosign(%s): not ok", name)
	}
	return els
}

func TestParse(t *testing.T) {
	syms, err := Parse("cq ^ar")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []struct {
		kind Kind
		text string
	}{
		{KindChar, "C"},
		{KindChar, "Q"},
		{KindSpace, " "},
		{KindProsign, "^AR"},
	}
	if len(syms) != len(want) {
		t.Fatalf("got %d symbols, want %d: %+v", len(syms), len(want), syms)
	}
	for i, w := range want {
		if syms[i].Kind != w.kind || syms[i].Text != w.text {
			t.Errorf("symbol %d: got %v %q, want %v %q", i, syms[i].Kind, syms[i].Text, w.kind, w.text)
		}
	}
	// A prosign counts as the characters the client actually sent.
	if got := syms[3].Chars(); got != 3 {
		t.Errorf("^AR: got %d chars, want 3", got)
	}
	// The prosign's elements are A and R run together.
	if got := len(syms[3].Elements); got != 5 {
		t.Errorf("^AR: got %d elements, want 5", got)
	}
}

func TestParseProsignRunStopsAtNonLetter(t *testing.T) {
	syms, err := Parse("^SK73")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(syms) != 3 || syms[0].Text != "^SK" || syms[1].Text != "7" || syms[2].Text != "3" {
		t.Fatalf("got %+v", syms)
	}
}

func TestParseErrors(t *testing.T) {
	t.Run("caret then non-letter", func(t *testing.T) {
		for _, text := range []string{"^", "AB^ ", "^1", "^^AR"} {
			_, err := Parse(text)
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("Parse(%q): got %v, want SyntaxError", text, err)
			}
		}
		_, err := Parse("AB^ ")
		var se *SyntaxError
		if errors.As(err, &se); se.Offset != 2 {
			t.Errorf("offset: got %d, want 2", se.Offset)
		}
	})

	t.Run("unknown character", func(t *testing.T) {
		_, err := Parse("AB;C")
		var ce *CharError
		if !errors.As(err, &ce) {
			t.Fatalf("got %v, want CharError", err)
		}
		if ce.Char != ';' || ce.Offset != 2 {
			t.Errorf("got %q at %d, want ';' at 2", ce.Char, ce.Offset)
		}
	})

	t.Run("offset counts runes", func(t *testing.T) {
		// The offset is a rune index so the API can point at the character the
		// client typed, not at a byte inside a multi-byte rune.
		_, err := Parse("ABä;")
		var ce *CharError
		if !errors.As(err, &ce) {
			t.Fatalf("got %v, want CharError", err)
		}
		if ce.Offset != 2 || ce.Char != 'ä' {
			t.Errorf("got %q at %d, want 'ä' at 2", ce.Char, ce.Offset)
		}
	})
}

func TestValid(t *testing.T) {
	if !Valid("CQ DE OH2XYZ ^AR") {
		t.Error("valid text rejected")
	}
	if Valid("CQ ^ ") {
		t.Error("stray caret accepted")
	}
}
