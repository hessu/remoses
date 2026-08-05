package kenwood

import (
	"context"
	"fmt"
	"strings"

	"github.com/hessu/remoses/internal/rig/backend"
)

// MaxChunk is the width of KY's text field. It is not a maximum in the usual
// sense: P2 is a *fixed* 24 characters, and a shorter chunk is padded out with
// spaces. Those pad spaces are explicitly not converted to Morse, so padding
// costs nothing but the wire bytes and no special case is needed for a short
// final chunk.
const MaxChunk = 24

// Keyer speed limits from KS. The rig clamps out-of-range values itself; the
// same clamp is applied here so the speed remoses reports matches the speed the
// rig is actually keying at.
const (
	minWPM = 4
	maxWPM = 60
)

// Charset is what may appear in KY's text field.
//
// It lists the *encoded* form, so it includes the eight single-character
// prosign abbreviations. Validate against it after EncodeProsigns, never
// before: the canonical "^AR" marker is consumed there and is not a character
// the rig can key.
//
// ';' is absent for the obvious reason — it would terminate the command
// mid-text — and its absence is enforced separately in SendChunk, since a
// smuggled semicolon corrupts the command stream rather than merely producing
// wrong Morse.
const Charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"abcdefghijklmnopqrstuvwxyz" +
	"0123456789" +
	" '\"()*+,-./:=?@" +
	`_[>]<\#%`

// charsetSet is Charset as a lookup table, built once.
var charsetSet = func() map[byte]bool {
	m := make(map[byte]bool, len(Charset))
	for i := 0; i < len(Charset); i++ {
		m[Charset[i]] = true
	}
	return m
}()

// prosignSymbols maps the canonical remoses prosign names to the single ASCII
// characters the TS-590 substitutes for them.
//
// This is the whole reason EncodeProsigns exists. Icom uses "^" as a marker
// meaning "run the following characters together"; Kenwood instead defines
// abbreviations, one symbol per prosign, and supports exactly these eight. A
// client that hard-coded either dialect would be wrong on the other rig, so the
// API speaks "^AR" and each backend translates.
var prosignSymbols = map[string]byte{
	"AR": '_',
	"BT": '[',
	"SK": '>',
	"KN": ']',
	"AS": '<',
	"BK": '\\',
	"HH": '#',
	"SN": '%',
}

// supportedProsigns is the list quoted back in the error for an unknown one,
// spelled out rather than derived so the order is stable.
const supportedProsigns = "^AR ^AS ^BK ^BT ^HH ^KN ^SK ^SN"

// MaxChunk implements backend.MorseSender.
func (k *Rig) MaxChunk() int { return MaxChunk }

// Charset implements backend.MorseSender.
func (k *Rig) Charset() string { return Charset }

// BufferFree asks the rig whether it will accept another chunk.
//
// KY answers a single bit — KY0; means there is buffer space, KY1; means there
// is none — so "free" is all or nothing: a whole chunk, or zero. That is enough
// for the pacing loop, and it has to be respected: writing to a full buffer is a
// hard error on this rig, not a truncation.
func (k *Rig) BufferFree(ctx context.Context, c backend.Conn) (int, bool, error) {
	u, err := do(ctx, c, reqKY, keyKY)
	if err != nil {
		return 0, false, err
	}
	if len(u.Raw) < 3 {
		return 0, false, fmt.Errorf("kenwood: short KY answer %q", u.Raw)
	}
	switch u.Raw[2] {
	case '0':
		return MaxChunk, true, nil
	case '1':
		return 0, true, nil
	default:
		return 0, false, fmt.Errorf("kenwood: unexpected KY answer %q, want KY0 or KY1", u.Raw)
	}
}

// SendChunk queues one block of text for keying.
//
// The command is written fire-and-forget: like every Kenwood set command it
// produces no answer, and the pacing loop's feedback comes from BufferFree
// rather than from an acknowledgement.
func (k *Rig) SendChunk(ctx context.Context, c backend.Conn, text string) error {
	if err := validateChunk(text); err != nil {
		return err
	}
	// "KY", P1 (always a space in this form), the fixed 24-character field, then
	// the terminator.
	var b strings.Builder
	b.Grow(len(text) + MaxChunk + 4)
	b.WriteString("KY ")
	b.WriteString(text)
	for i := len(text); i < MaxChunk; i++ {
		b.WriteByte(' ')
	}
	b.WriteByte(';')
	return send(ctx, c, b.String())
}

// Abort stops the rig keying and discards whatever is still in its buffer.
//
// KY0; is the Set 2 form of the same command whose answer means "space
// available"; the collision is the rig's, not ours. Any P1 other than 0 is an
// error, so there is nothing else this can be.
func (k *Rig) Abort(ctx context.Context, c backend.Conn) error {
	return send(ctx, c, "KY0;")
}

// SetSpeed sets the internal keyer speed, clamped to the rig's 4..60 wpm range.
//
// No read-back: SetSpeed is called from the keying path, sometimes between
// chunks, and the wpm figure has no home in radio.Patch, so a round trip would
// buy latency and nothing else. Clamping here means the value remoses reports is
// the value the rig took.
func (k *Rig) SetSpeed(ctx context.Context, c backend.Conn, wpm int) error {
	return send(ctx, c, fmt.Sprintf("KS%03d;", min(maxWPM, max(minWPM, wpm))))
}

// EncodeProsigns rewrites the canonical "^AR" form into the rig's own
// single-character abbreviations, and names any prosign the TS-590 cannot key.
//
// Matching is case-insensitive on the two letters, so "^ar" works, but the
// output is the rig's symbol either way. A bare "^" at the end of the text, or
// one followed by fewer than two characters, is reported rather than passed
// through, since "^" is not in the rig's charset and would be silently mangled.
func (k *Rig) EncodeProsigns(text string) (string, error) {
	if !strings.Contains(text, "^") {
		return text, nil
	}

	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if text[i] != '^' {
			b.WriteByte(text[i])
			continue
		}
		if i+2 >= len(text) {
			return "", fmt.Errorf("kenwood: truncated prosign %q: expected two letters after ^ (supported: %s)", text[i:], supportedProsigns)
		}
		name := upperASCII(text[i+1 : i+3])
		sym, ok := prosignSymbols[name]
		if !ok {
			return "", fmt.Errorf("kenwood: prosign %q is not one the TS-590 can key (supported: %s)", "^"+name, supportedProsigns)
		}
		b.WriteByte(sym)
		i += 2
	}
	return b.String(), nil
}

// validateChunk checks a chunk against everything the KY command demands of it.
// The rig does not report bad characters — it keys something else, or nothing —
// so the check has to happen here.
func validateChunk(text string) error {
	if len(text) > MaxChunk {
		return fmt.Errorf("kenwood: CW chunk %q is %d characters, KY takes at most %d", text, len(text), MaxChunk)
	}
	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == ';':
			return fmt.Errorf("kenwood: CW text cannot contain ';' at offset %d: it would terminate the KY command", i)
		case !charsetSet[text[i]]:
			return fmt.Errorf("kenwood: CW text contains %q at offset %d, which the TS-590 cannot key (accepted: %s)", text[i], i, Charset)
		}
	}
	return nil
}
