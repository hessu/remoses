package cw

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// chunk is one block of text on its way into a rig's CW buffer.
type chunk struct {
	// canon is the canonical (upper-cased) source of this chunk. It is kept
	// alongside the encoded form because timing has to be done on canonical
	// text: after EncodeProsigns, "^AR" may have become a bare '_' that no
	// Morse table has ever heard of.
	canon string
	// encoded is what goes on the wire, in the rig's own dialect.
	encoded string
	// chars is the canonical character count, so queue depth is reported in the
	// same units the client submitted.
	chars int
	// id makes a chunk identifiable after the pacing loop has released the lock
	// to wait for buffer space: the queue it came from may have been replaced
	// or aborted in the meantime.
	id uint64
}

// minMaxChunk is the smallest MaxChunk a backend may declare. Both target rigs
// are far above it (24 on Kenwood, 30 on Icom); the floor exists so that a
// misconfigured backend fails at construction rather than by hard-splitting
// every prosign it is given.
const minMaxChunk = 8

// validate checks canonical text against a rig's charset and the one syntax
// rule canonical text has.
//
// A stray '^' is reported as a CharError naming the caret itself. It is a
// client mistake, not a server fault, and the API turns a CharError into the
// 422 that says so (§11.3).
func validate(text, charset string) error {
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		if r == '^' {
			if i+1 >= len(rs) || !isLetter(rs[i+1]) {
				return &CharError{Char: '^', Offset: i, Charset: charset}
			}
			continue
		}
		if !inCharset(r, charset) {
			return &CharError{Char: r, Offset: i, Charset: charset}
		}
	}
	return nil
}

func inCharset(r rune, charset string) bool {
	return strings.ContainsRune(charset, r) ||
		strings.ContainsRune(charset, asciiUpperRune(r)) ||
		strings.ContainsRune(charset, asciiLowerRune(r))
}

// buildChunks turns validated canonical text into rig-dialect chunks of at most
// maxChunk characters each.
//
// Chunks break on word boundaries, so the rig never hears a character or a
// prosign split across two buffer loads. The separating space is carried at the
// HEAD of the following chunk rather than the tail of the previous one, because
// Kenwood's KY block is fixed width and the rig cannot tell a real trailing
// space from the padding it strips — a word gap parked at the end of a chunk
// would simply vanish.
//
// A word too long for one chunk is the one case where a hard split is
// unavoidable; it is done between characters, never inside one and never
// between a prosign and its '^'.
func buildChunks(text string, maxChunk int, encode func(string) (string, error)) ([]chunk, error) {
	if maxChunk < 1 {
		return nil, fmt.Errorf("cw: maximum chunk length %d is not usable", maxChunk)
	}
	pieces, err := split(text, maxChunk, encode)
	if err != nil {
		return nil, err
	}

	var (
		out  []chunk
		cur  chunk
		have bool
	)
	flush := func() {
		if have {
			cur.chars = utf8.RuneCountInString(cur.canon)
			out = append(out, cur)
			cur, have = chunk{}, false
		}
	}
	for _, p := range pieces {
		if have && len(cur.encoded)+len(p.encoded) > maxChunk {
			flush()
		}
		cur.canon += p.canon
		cur.encoded += p.encoded
		have = true
	}
	flush()
	return out, nil
}

// piece is an indivisible unit of a chunk: a whole word with the space that
// precedes it, or — when a word will not fit in one chunk — a single character.
type piece struct {
	canon   string
	encoded string
}

func split(text string, maxChunk int, encode func(string) (string, error)) ([]piece, error) {
	var pieces []piece
	for _, group := range groups(text) {
		enc, err := encode(group)
		if err != nil {
			return nil, fmt.Errorf("cw: encoding %q for this radio: %w", group, err)
		}
		if len(enc) <= maxChunk {
			pieces = append(pieces, piece{canon: group, encoded: enc})
			continue
		}
		// The word does not fit. Fall back to a character-at-a-time split; the
		// leading spaces travel with the first character so that no piece is a
		// bare space that could land at the end of a chunk.
		units := units(group)
		for i, u := range units {
			enc, err := encode(u)
			if err != nil {
				return nil, fmt.Errorf("cw: encoding %q for this radio: %w", u, err)
			}
			if len(enc) > maxChunk {
				return nil, fmt.Errorf("cw: %q needs %d characters of buffer, this radio takes %d at a time",
					u, len(enc), maxChunk)
			}
			pieces = append(pieces, piece{canon: units[i], encoded: enc})
		}
	}
	return pieces, nil
}

// groups splits text into "the spaces before a word, plus that word". A run of
// trailing spaces with no word after it forms a final group of its own.
func groups(text string) []string {
	rs := []rune(text)
	var out []string
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && rs[j] == ' ' {
			j++
		}
		k := j
		for k < len(rs) && rs[k] != ' ' {
			k++
		}
		out = append(out, string(rs[i:k]))
		i = k
	}
	return out
}

// units splits one group into the smallest things that may be separated: single
// characters, and whole prosigns. Any leading spaces are glued to the first
// unit.
func units(group string) []string {
	rs := []rune(group)
	var out []string
	i := 0
	for i < len(rs) && rs[i] == ' ' {
		i++
	}
	lead := string(rs[:i])
	for i < len(rs) {
		j := i + 1
		if rs[i] == '^' {
			for j < len(rs) && isLetter(rs[j]) {
				j++
			}
		}
		out = append(out, lead+string(rs[i:j]))
		lead = ""
		i = j
	}
	if len(out) == 0 && lead != "" {
		out = append(out, lead)
	}
	return out
}

func isLetter(r rune) bool { return (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') }

func asciiUpperRune(r rune) rune {
	if r >= 'a' && r <= 'z' {
		return r - 'a' + 'A'
	}
	return r
}

func asciiLowerRune(r rune) rune {
	if r >= 'A' && r <= 'Z' {
		return r - 'A' + 'a'
	}
	return r
}

// asciiUpper folds case for keying, where case is not significant. It folds
// ASCII only: Unicode folding can change a string's length, which would shift
// the character offsets we report back to the client.
func asciiUpper(s string) string { return strings.Map(asciiUpperRune, s) }
