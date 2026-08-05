// Package morse holds pure Morse knowledge: the element table, canonical text
// parsing, and element timing arithmetic.
//
// It is a leaf package and imports nothing from remoses. Both consumers need
// it and neither should have to depend on the other: the CAT pacing loop needs
// timing estimates to pace a rig that cannot be asked how full its buffer is,
// and the local element generator needs the elements themselves.
package morse

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Element is one keyed mark.
type Element uint8

const (
	// Dot is a mark of one dot unit.
	Dot Element = iota
	// Dash is a mark of three dot units.
	Dash
)

// Units is the length of the mark in dot units.
func (e Element) Units() int {
	if e == Dash {
		return 3
	}
	return 1
}

func (e Element) String() string {
	if e == Dash {
		return "-"
	}
	return "."
}

// patterns is the element table, written as dots and dashes so that it reads
// like a Morse chart and can be checked against one by eye.
//
// The punctuation set is exactly what the two target rigs accept over CAT
// (§5.3), so that locally generated keying and rig-generated keying agree on
// what is sendable. ' * ' is not an ITU punctuation mark; Kenwood accepts it and
// ITU-R M.1677-1 gives the multiplication sign as -..-, which is what we key.
var patterns = map[rune]string{
	'A': ".-", 'B': "-...", 'C': "-.-.", 'D': "-..", 'E': ".",
	'F': "..-.", 'G': "--.", 'H': "....", 'I': "..", 'J': ".---",
	'K': "-.-", 'L': ".-..", 'M': "--", 'N': "-.", 'O': "---",
	'P': ".--.", 'Q': "--.-", 'R': ".-.", 'S': "...", 'T': "-",
	'U': "..-", 'V': "...-", 'W': ".--", 'X': "-..-", 'Y': "-.--",
	'Z': "--..",

	'0': "-----", '1': ".----", '2': "..---", '3': "...--", '4': "....-",
	'5': ".....", '6': "-....", '7': "--...", '8': "---..", '9': "----.",

	'\'': ".----.", // apostrophe (WG)
	'"':  ".-..-.", // quotation mark (AF)
	'(':  "-.--.",  // left parenthesis (KN)
	')':  "-.--.-", // right parenthesis (KK)
	'*':  "-..-",   // multiplication sign (X)
	'+':  ".-.-.",  // plus / AR
	',':  "--..--", // comma (MIM)
	'-':  "-....-", // hyphen (DU)
	'.':  ".-.-.-", // full stop (AAA)
	'/':  "-..-.",  // solidus (DN)
	':':  "---...", // colon (OS)
	'=':  "-...-",  // double hyphen / BT
	'?':  "..--..", // question mark (IMI)
	'@':  ".--.-.", // commercial at (AC)
}

// table is patterns decoded once, so parsing never walks a string of runes.
var table = func() map[rune][]Element {
	m := make(map[rune][]Element, len(patterns))
	for r, p := range patterns {
		els := make([]Element, 0, len(p))
		for _, c := range p {
			if c == '-' {
				els = append(els, Dash)
			} else {
				els = append(els, Dot)
			}
		}
		m[r] = els
	}
	return m
}()

// charset is every character Encode accepts, in an order a human can scan.
// Space is included because a word gap is something we can key.
const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 '\"()*+,-./:=?@"

// Charset returns the characters this package can key, for capability
// reporting and for the API's 422 on an unsendable character. Locally
// generated keying uses this set; a rig with a CAT buffer reports its own,
// which is usually smaller.
func Charset() string { return charset }

// Encode returns the elements of a single character. Case is not significant.
func Encode(r rune) ([]Element, bool) {
	els, ok := table[upper(r)]
	return els, ok
}

// prosigns are the conventional run-together sequences and what they mean.
//
// The parser does not consult this table: canonical text may name ANY run of
// letters after '^' and a locally generated keyer will happily send it. The
// table exists so the API can advertise the conventional set, and so a caller
// can resolve a name without knowing that a prosign is just its letters keyed
// with no gap between them.
var prosigns = map[string]string{
	"AR":  "end of message",
	"AS":  "wait",
	"BK":  "break in",
	"BT":  "separator, new paragraph (=)",
	"CL":  "closing down",
	"CT":  "attention, start of transmission (KA)",
	"HH":  "error",
	"KA":  "attention (CT)",
	"KN":  "go ahead, named station only",
	"SK":  "end of contact (VA)",
	"SN":  "understood (VE)",
	"SOS": "distress",
	"VA":  "end of contact (SK)",
	"VE":  "understood (SN)",
}

// Prosign returns the elements of a named prosign, keyed with no
// inter-character spacing. Any run of letters is a valid prosign; ok is false
// only when name is empty or contains something that is not a letter.
func Prosign(name string) ([]Element, bool) {
	if name == "" {
		return nil, false
	}
	var els []Element
	for _, r := range name {
		if !isLetter(r) {
			return nil, false
		}
		els = append(els, table[upper(r)]...)
	}
	return els, true
}

// ProsignNames lists the conventional prosign names, sorted.
func ProsignNames() []string {
	names := make([]string, 0, len(prosigns))
	for n := range prosigns {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ProsignMeaning describes a conventional prosign, for help text.
func ProsignMeaning(name string) (string, bool) {
	m, ok := prosigns[strings.ToUpper(name)]
	return m, ok
}

// Kind distinguishes the three things canonical text can contain.
type Kind uint8

const (
	// KindChar is one letter, digit or punctuation mark.
	KindChar Kind = iota
	// KindProsign is a '^' run, keyed with no inter-character spacing.
	KindProsign
	// KindSpace is a word gap and keys nothing.
	KindSpace
)

func (k Kind) String() string {
	switch k {
	case KindProsign:
		return "prosign"
	case KindSpace:
		return "space"
	default:
		return "char"
	}
}

// Symbol is one parsed unit of canonical text.
type Symbol struct {
	Kind Kind
	// Text is the canonical source of this symbol, upper-cased: "A", "^AR", " ".
	// Keeping it lets a queue report depth in the same character units the API
	// accepted, rather than in elements.
	Text string
	// Elements are the marks, separated by one inter-element gap each. Empty
	// for Space.
	Elements []Element
}

// Chars is how many canonical characters this symbol occupied, so "^AR" counts
// as the three characters the client sent.
func (s Symbol) Chars() int { return utf8.RuneCountInString(s.Text) }

// CharError reports a character with no entry in the element table. Offset is
// the rune index in the parsed text, which the API quotes back to the client.
type CharError struct {
	Char   rune
	Offset int
}

func (e *CharError) Error() string {
	return "morse: no element table entry for " + string(e.Char)
}

// SyntaxError reports malformed canonical text. Today the only syntax rule is
// that '^' introduces a prosign, so a stray caret is caught here rather than
// being keyed as punctuation the operator did not intend.
type SyntaxError struct {
	Offset int
	Msg    string
}

func (e *SyntaxError) Error() string { return "morse: " + e.Msg }

// Parse turns canonical text into symbols. Case is not significant, '^'
// followed by a run of letters is one run-together prosign, and '^' followed
// by anything else is a SyntaxError.
func Parse(text string) ([]Symbol, error) {
	return parse(text, false)
}

// nominal is what an unrecognised character costs in the tolerant parse: the
// elements of K, nine units with its inter-element gaps, which is close to the
// average for English text.
var nominal = []Element{Dash, Dot, Dash}

func parse(text string, tolerant bool) ([]Symbol, error) {
	rs := []rune(text)
	out := make([]Symbol, 0, len(rs))
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case r == '^':
			j := i + 1
			for j < len(rs) && isLetter(rs[j]) {
				j++
			}
			if j == i+1 {
				if tolerant {
					out = append(out, Symbol{Kind: KindChar, Text: string(r), Elements: nominal})
					i++
					continue
				}
				return nil, &SyntaxError{
					Offset: i,
					Msg:    "'^' must be followed by the letters of a prosign",
				}
			}
			var els []Element
			for k := i + 1; k < j; k++ {
				els = append(els, table[upper(rs[k])]...)
			}
			out = append(out, Symbol{
				Kind:     KindProsign,
				Text:     "^" + upperString(string(rs[i+1:j])),
				Elements: els,
			})
			i = j
		case r == ' ':
			out = append(out, Symbol{Kind: KindSpace, Text: " "})
			i++
		default:
			els, ok := table[upper(r)]
			if !ok {
				if tolerant {
					out = append(out, Symbol{Kind: KindChar, Text: string(r), Elements: nominal})
					i++
					continue
				}
				return nil, &CharError{Char: r, Offset: i}
			}
			out = append(out, Symbol{Kind: KindChar, Text: string(upper(r)), Elements: els})
			i++
		}
	}
	return out, nil
}

// Valid reports whether text parses. It is a convenience for callers that only
// want a yes or no.
func Valid(text string) bool {
	_, err := Parse(text)
	return err == nil
}

func isLetter(r rune) bool { return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') }

// upper folds ASCII only. Case is not significant in canonical text, but
// Unicode case folding can change a string's length, which would shift the
// offsets we report in errors.
func upper(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 'a' + 'A'
	}
	return r
}

func upperString(s string) string {
	return strings.Map(upper, s)
}
