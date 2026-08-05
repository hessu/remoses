package rig

// CAT wire logging: the exact bytes that went out and came back, per radio, at
// debug level.
//
// It exists because the ordinary logs show decoded state, which is precisely
// the layer where a wrong assumption is invisible. A mode table transcribed
// from the wrong column, a manual that draws eleven digits where its own range
// needs eight, a rig that does not answer a command its documentation lists —
// all three look identical from above, and all three are obvious in one line of
// hex. DESIGN.md §5.4, §5.5 and §5.6 each end with a list of assumptions marked
// "worth revisiting on hardware"; this is the instrument that revisits them.

import (
	"strings"

	"github.com/hessu/remoses/internal/rig/backend"
)

const (
	// wireMsg is the slog message every wire line shares, so a trace can be
	// grepped out of a busy log or dropped by a handler. It is deliberately
	// stable: people will put it in scripts.
	wireMsg = "cat wire"

	// Direction values for the "dir" attribute. Spelled out rather than
	// abbreviated because a log line has to be readable by whoever is standing
	// at the radio, not only by whoever wrote this.
	wireToRig   = "to-rig"
	wireFromRig = "from-rig"
)

const hexDigits = "0123456789ABCDEF"

// wireHex renders a frame as uppercase, space-separated hex.
//
// Always logged, whatever the protocol: CI-V is binary, and the whole point of
// the trace is to be comparable byte for byte against a manual — which prints
// frames exactly this way, "FE FE 98 E0 03 FD".
func wireHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	out := make([]byte, 0, 3*len(b)-1)
	for i, c := range b {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

// wireText renders a frame as text when most of it is printable ASCII, and
// reports whether it did.
//
// The threshold is what keeps this useful rather than noise. Kenwood and Yaesu
// frames ("FA00014025000;") are entirely printable and are miserable to read as
// hex alone, while a CI-V frame is binary and its "text" would be a row of
// escapes hiding the one field being examined. Three quarters separates the two
// dialects with room to spare, and the margin is what lets an ASCII frame
// carrying a stray byte — a rig still powering up — keep its rendering.
func wireText(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	printable := 0
	for _, c := range b {
		if isWirePrintable(c) {
			printable++
		}
	}
	if printable*4 < len(b)*3 {
		return "", false
	}

	var sb strings.Builder
	sb.Grow(len(b) + 8)
	for _, c := range b {
		switch {
		case c == '\\':
			// Escaped, so that an escape in the output is unambiguously one.
			sb.WriteString(`\\`)
		case isWirePrintable(c):
			sb.WriteByte(c)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		case c == '\t':
			sb.WriteString(`\t`)
		default:
			// Control and high bytes are escaped rather than emitted. A bare CR
			// would break the log line in two, and a NUL or a stray 0x1B would
			// otherwise be invisible in exactly the trace opened to find it.
			sb.WriteString(`\x`)
			sb.WriteByte(hexDigits[c>>4])
			sb.WriteByte(hexDigits[c&0x0f])
		}
	}
	return sb.String(), true
}

func isWirePrintable(c byte) bool { return c >= 0x20 && c < 0x7f }

// wireKey names the correlation key a frame decoded to.
//
// The empty key is spelled out rather than logged as "": a frame answering no
// request — an Icom Transceive broadcast, a Kenwood or Yaesu AI push — is
// exactly what a trace is opened for when a rig sends traffic nobody expected,
// and it has to be identifiable at a glance.
func wireKey(k backend.Key) string {
	if k == backend.KeyUnsolicited {
		return "unsolicited"
	}
	return string(k)
}

// logWire emits one line for one frame. The radio id rides on the logger, which
// the session stamped with it.
//
// Callers MUST test c.wire first. Everything here formats and allocates, and
// the reader goroutine it runs on is in the CW timing path; the guard is at the
// call sites so that it is impossible to reach this by accident.
//
// Neither call site holds a lock the other goroutine contends for. The reader
// logs before it folds the patch into state or looks for a waiter, so it takes
// neither stateMu nor c.mu; the writer logs under cmdMu only, the transaction
// lock it already holds across the whole round trip.
func (c *conn) logWire(dir string, frame []byte, extra ...any) {
	args := make([]any, 0, 6+len(extra))
	args = append(args, "dir", dir, "len", len(frame), "hex", wireHex(frame))
	if text, ok := wireText(frame); ok {
		args = append(args, "text", text)
	}
	c.log.Debug(wireMsg, append(args, extra...)...)
}
