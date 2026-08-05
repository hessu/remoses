package civ

import (
	"context"
	"fmt"
	"strings"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Charset is the set of characters command 17 will key, in ASCII order:
// digits, both letter cases, and the punctuation the reference tabulates
// (' ( ) / = ? + . " - @ , : and space).
//
// The rig silently mangles anything outside this set instead of rejecting it,
// which is why validation is strict and happens before a single byte goes out.
//
// '^' is included because it is part of the canonical text remoses accepts, not
// because it is keyed: it marks a run of characters sent with no
// inter-character space, so "^AR" is one prosign rather than two letters. If it
// were excluded here every prosign would be rejected by the API's charset check
// before EncodeProsigns ever saw it. EncodeProsigns enforces what may follow it.
const Charset = " \"'()+,-./0123456789:=?@ABCDEFGHIJKLMNOPQRSTUVWXYZ^abcdefghijklmnopqrstuvwxyz"

// prosignMarker starts a run keyed with no inter-character spacing.
const prosignMarker = '^'

// maxChunk is the documented limit on command 17: up to 30 characters.
const maxChunk = 30

// MaxChunk is the largest text block one command 17 can carry.
func (r *Rig) MaxChunk() int { return maxChunk }

// Charset returns the characters this rig will key.
func (r *Rig) Charset() string { return Charset }

// BufferFree reports that the buffer cannot be queried.
//
// The IC-7610 has no command that answers how much of its 30-character CW
// buffer is free — the reference lists no read form of 17 — so the pacing loop
// has to estimate drain from the keyer speed and the elements queued. Reporting
// ok=false is how that fallback is selected; returning an invented number would
// be worse, since overrunning the buffer loses characters silently.
func (r *Rig) BufferFree(ctx context.Context, c backend.Conn) (free int, ok bool, err error) {
	return 0, false, nil
}

// SendChunk queues one block of text for the rig to key (command 17).
//
// Note that the rig only actually transmits when it is already in a keying
// state: the reference notes that in CW mode the message goes out as CW if
// TRANSMIT or an external TX switch is on, or break-in is enabled. remoses does
// not assert PTT here; whether to do so is a per-station option, because a
// sequencer or amplifier may need the lead-in.
func (r *Rig) SendChunk(ctx context.Context, c backend.Conn, text string) error {
	if text == "" {
		return fmt.Errorf("civ: empty CW chunk")
	}
	if len(text) > maxChunk {
		return fmt.Errorf("civ: CW chunk of %d characters exceeds the %d the rig accepts", len(text), maxChunk)
	}
	if err := validateCW(text); err != nil {
		return err
	}
	return r.set(ctx, c, "CW message", r.frame(cmdSendCW, []byte(text)...))
}

// Abort stops the rig sending and discards what is still in its buffer
// (command 17 with data FF).
func (r *Rig) Abort(ctx context.Context, c backend.Conn) error {
	return r.set(ctx, c, "CW abort", r.frame(cmdSendCW, 0xFF))
}

// SetSpeed sets the keyer speed (command 14 0C). Out-of-range values clamp to
// the rig's 6-48 wpm rather than erroring; the usable range is published in
// radio.Caps so a client can present it honestly.
func (r *Rig) SetSpeed(ctx context.Context, c backend.Conn, wpm int) error {
	v := encodeBCD2(nativeFromWPM(wpm))
	return r.set(ctx, c, "keyer speed", r.frame(cmdLevel, subKeyerSpeed, v[0], v[1]))
}

// EncodeProsigns validates canonical text and returns it in the rig's own
// encoding, which for Icom is the same thing: '^AR' is already what command 17
// wants. The work here is therefore all validation.
//
// The reference does not state how a '^' run ends — it says only that '^' is
// used to transmit a string of characters with no inter-character space. This
// implementation takes the run to be the letters immediately following the
// marker, which is the convention every other client uses, and requires at
// least one, so a stray caret is reported to the operator instead of being
// handed to a rig whose behaviour is undocumented.
func (r *Rig) EncodeProsigns(text string) (string, error) {
	if err := validateCW(text); err != nil {
		return "", err
	}
	return text, nil
}

// validateCW rejects anything command 17 cannot key. The offset in a returned
// cw.CharError is a byte offset into text, which is also the character position
// for anything that got as far as being valid.
func validateCW(text string) error {
	for i, c := range text {
		if !strings.ContainsRune(Charset, c) {
			return &cw.CharError{Char: c, Offset: i, Charset: Charset}
		}
		if c == prosignMarker && !startsWithLetter(text[i+1:]) {
			return fmt.Errorf("cw: '^' at offset %d must be followed by the letters of a prosign, as in ^AR", i)
		}
	}
	return nil
}

func startsWithLetter(s string) bool {
	if s == "" {
		return false
	}
	c := s[0]
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
