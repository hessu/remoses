package rigctld

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hessu/remoses/internal/rig/backend"
)

// MaxChunk is how much text one `b` command carries.
//
// The protocol would take far more — rigctld reads the rest of the line into a
// 511-byte buffer — but the rig behind it will not. Hamlib hands the whole
// string to the rig's own backend, whose CW buffer is typically 24 characters
// (Kenwood KY) or 30 (Icom), and rig_send_morse can block the rig interface
// until what it was given has been queued. Since this backend does not know
// which radio is on the other end, 20 is chosen to sit under every buffer
// remoses knows of, while staying comfortably above the 8-character floor the
// shared pacing layer requires.
const MaxChunk = 20

// Charset is what remoses will send to an unknown rig.
//
// It is deliberately the conservative core of the ITU set: the letters, the
// digits, the word space, and the punctuation that essentially every rig keyer
// implements. Hamlib does not validate or translate the text — rig_send_morse
// passes it through to the rig's backend — so an unsupported character produces
// whatever that particular radio does with it, which is usually silence and
// occasionally nonsense. There is no way to ask a rig what it will key, so the
// only safe answer is a small set.
//
// Carriage return and newline are the two characters that would do real damage:
// the Morse text is read to the end of the line, so either one would truncate
// the command and leave the rest of the text to be parsed as new commands.
const Charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
	"0123456789" +
	" .,?/=-+"

// charsetSet is Charset as a lookup table, built once.
var charsetSet = func() map[byte]bool {
	m := make(map[byte]bool, len(Charset))
	for i := range len(Charset) {
		m[Charset[i]] = true
	}
	return m
}()

// prosigns are the ones this backend can express, as the letters they run
// together into. See EncodeProsigns.
var prosigns = map[string]string{
	"AR": "AR",
	"AS": "AS",
	"BK": "BK",
	"BT": "BT",
	"HH": "HH",
	"KA": "KA",
	"KN": "KN",
	"SK": "SK",
	"SN": "SN",
	"VA": "VA",
}

// MaxChunk implements backend.MorseSender.
func (g *Rig) MaxChunk() int { return MaxChunk }

// Charset implements backend.MorseSender.
func (g *Rig) Charset() string { return Charset }

// BufferFree always reports that the rig cannot be asked.
//
// Hamlib has no buffer-space query: rig_send_morse either queues the text or
// returns an error, and \wait_morse blocks until the rig has finished sending,
// which is useless to a pacing loop that has to stay responsive to an abort. So
// the shared open-loop model in internal/cw does the pacing from its own timing
// estimate, which is exactly what ok=false selects.
func (g *Rig) BufferFree(ctx context.Context, c backend.Conn) (int, bool, error) {
	return 0, false, nil
}

// SendChunk queues one block of text with `b`.
//
// The transaction waits for the RPRT rather than firing and forgetting. It is
// the only acknowledgement in the protocol, and on a rig whose Hamlib backend
// lacks send_morse it is the difference between one clear error and a queue
// that silently goes nowhere.
//
// The space after `b` is load-bearing, not cosmetic. send_morse takes the rest
// of the line as its argument, and rigctld reads a SECOND line when the first
// thing after the command letter is the newline — so a bare "b\n" would swallow
// whatever command came next. With the space always present, even empty text
// stays inside its own line.
func (g *Rig) SendChunk(ctx context.Context, c backend.Conn, text string) error {
	if err := g.checkMorse(); err != nil {
		return err
	}
	if err := validateChunk(text); err != nil {
		return err
	}
	_, err := g.do(ctx, c, "+b "+text+"\n", keySendMorse)
	return err
}

// Abort stops the rig sending with \stop_morse.
//
// The long name is not a stylistic choice: stop_morse has no printable command
// letter in rigctld's table (it is 0xbb), so the backslash form is the only way
// to reach it.
func (g *Rig) Abort(ctx context.Context, c backend.Conn) error {
	_, err := g.do(ctx, c, reqStopMorse, keyStopMorse)
	return err
}

// SetSpeed sets the keyer speed with L KEYSPD, clamped to whatever range the
// daemon reported for it.
//
// There is no read-back. The speed has no home in radio.Patch, and this is
// called from the keying path between chunks, so a round trip would buy latency
// and nothing else; clamping here is what keeps the speed remoses reports equal
// to the speed it asked for.
func (g *Rig) SetSpeed(ctx context.Context, c backend.Conn, wpm int) error {
	lo, hi := int(g.keyerMin.Load()), int(g.keyerMax.Load())
	wpm = min(max(wpm, lo), hi)
	_, err := g.do(ctx, c, fmt.Sprintf("+L %s %d\n", levelKEYSPD, wpm), levelKey(cmdSetLevel, levelKEYSPD))
	return err
}

// EncodeProsigns rewrites the canonical "^AR" form into the plain letters.
//
// This is the honest answer for a backend that does not know which radio it is
// driving. Hamlib passes Morse text straight through to the rig's own backend
// and defines no marker for running characters together: Icom's '^' and
// Kenwood's single-symbol abbreviations are inventions of those two protocols,
// not of Hamlib, and sending either to the wrong rig keys the punctuation
// literally or nothing at all.
//
// So "^AR" becomes "AR", and whether the rig keys it as the prosign or as two
// letters with a normal inter-character gap is decided by the rig's Hamlib
// backend. The characters are right and the timing may not be, which is the
// trade this backend exists to make.
func (g *Rig) EncodeProsigns(text string) (string, error) {
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
			return "", fmt.Errorf("rigctld: truncated prosign %q: expected two letters after ^ (supported: %s)",
				text[i:], supportedProsigns())
		}
		name := upperASCII(text[i+1 : i+3])
		letters, ok := prosigns[name]
		if !ok {
			return "", fmt.Errorf("rigctld: prosign %q is not one remoses can express as plain letters (supported: %s)",
				"^"+name, supportedProsigns())
		}
		b.WriteString(letters)
		i += 2
	}
	return b.String(), nil
}

// supportedProsigns lists the recognised names for an error message, sorted so
// the message is stable.
func supportedProsigns() string {
	names := make([]string, 0, len(prosigns))
	for name := range prosigns {
		names = append(names, "^"+name)
	}
	slices.Sort(names)
	return strings.Join(names, " ")
}

// checkMorse refuses to key a rig whose Hamlib backend has no send_morse.
//
// The MorseSender interface is satisfied statically — Go has no way to gain a
// method at run time — so this is where the capability is actually enforced.
// Caps.CWMethod is the signal a well-behaved caller reads; this is the answer
// for one that did not.
func (g *Rig) checkMorse() error {
	if g.sendMorse.Load() {
		return nil
	}
	if g.caps.Load() == nil {
		return fmt.Errorf("rigctld: cannot key %s before its capabilities have been read", g.describe())
	}
	return fmt.Errorf("rigctld: %s has no CW buffer through Hamlib (\\dump_caps reports \"Can send Morse: N\")",
		g.describe())
}

// validateChunk checks a chunk against everything `b` demands of it. Hamlib
// does not report bad characters — it forwards them — so the check has to
// happen here.
func validateChunk(text string) error {
	if len(text) > MaxChunk {
		return fmt.Errorf("rigctld: CW chunk %q is %d characters, this backend sends at most %d",
			text, len(text), MaxChunk)
	}
	for i := range len(text) {
		switch {
		case text[i] == '\n' || text[i] == '\r':
			return fmt.Errorf("rigctld: CW text cannot contain a line break at offset %d: "+
				"rigctld reads the Morse text to the end of the line", i)
		case !charsetSet[text[i]]:
			return fmt.Errorf("rigctld: CW text contains %q at offset %d, which remoses will not send to an unknown rig (accepted: %s)",
				text[i], i, Charset)
		}
	}
	return nil
}

// upperASCII is strings.ToUpper without the Unicode machinery, which cannot
// apply: prosign names are ASCII by definition.
func upperASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
