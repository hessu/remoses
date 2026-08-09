// Package yaesubin implements Yaesu's five-byte binary CAT protocol, as spoken
// by the FT-857, FT-857D, FT-897 and FT-897D.
//
// Everything here is transcribed from the CAT Operation chapter of those four
// radios' own operating manuals. The chapters are the same document twice: the
// FT-857 and FT-857D charts are identical to the value, the FT-897 and FT-897D
// charts are identical to each other, and the two pairs differ only in
// typesetting and in three printing slips noted where they matter. So unlike
// the ASCII backend there is almost nothing per model here — see model.go.
//
// # How a radio gets here
//
// By its name, not by a backend of its own. The configuration is
// `backend: yaesu` with `yaesu.model` naming one of the four, exactly as it
// would for an FTdx10, and the yaesu package's factory dispatches on the model
// — see Handles. Which of Yaesu's two CAT systems a radio speaks is a fact
// about the radio, so an operator is never asked to encode it a second time,
// and there is no way to pair the wrong backend with the right model.
//
// One consequence is worth stating: an FT-857 must be named. A `backend: yaesu`
// with no model falls back to the ASCII dialect, which these radios do not
// speak at all.
//
// # Why this is not the yaesu package
//
// It shares the manufacturer and nothing else. The yaesu backend speaks ASCII:
// two letters, decimal parameters, a ';' terminator, case-insensitive,
// self-delimiting. This one is binary and fixed-width:
//
//	43 97 00 00 01      set the VFO to 439.700 MHz
//	00 00 00 00 03      read frequency and mode
//	00 00 00 00 08      key the transmitter
//
// Five bytes, always, opcode LAST, parameters first, dummy padding where a
// command has no parameters. Frequencies are packed BCD in units of ten hertz
// rather than decimal digits in hertz. Modes are a byte, not a character, and
// the byte values are not the ASCII dialect's values shifted — 02 is CW here
// and '2' is USB there. There are seventeen opcodes in total against the
// hundreds a modern Yaesu has, and no command letter to be wrong about, because
// there are no letters.
//
// # Three things the protocol does not have
//
// **No framing.** This is the difference that shapes the whole package. An
// answer has no terminator, no length, no opcode, no checksum and no
// self-identification of any kind. Answers are one byte or five, and the two
// cannot be told apart by content: an acknowledgement of 00 and the leading
// digit pair of a status answer on the 1.8 MHz band are the same byte. The only
// thing that knows where a frame ends is the command that provoked it, so this
// is the one backend here that implements backend.ReplyFramer — the session
// tells it what is going out, under the write lock, and Split sizes the answer
// from that. A backend that tried to record it for itself would have to do so
// before calling Do and therefore outside that lock, where the poller and an
// HTTP setter could store in one order and write in the other.
//
// **No error response.** Not a NAK, not a busy answer, not the yaesu backend's
// '?'. A frequency the radio cannot tune, a mode it does not have, an opcode
// that does not exist: the manuals define no way for it to say so. Every value
// with a documented range is therefore checked here before it goes out, nothing
// speculative is ever sent, and the read-back after each set is what reports
// what the radio actually did.
//
// **No push updates.** There is no AI, no Transceive, nothing unsolicited. A
// front-panel knob movement is invisible until the next poll, and that is a
// property of the radio rather than a gap here: the fast tier exists to be the
// only source of state, not as a safety net behind a push channel.
//
// # The acknowledgement, which is not in any manual
//
// Every command that is not one of the three reads is answered by a single
// byte. The manuals do not mention it — their command chart lists a reply only
// for the three reads — but the radios send it, and remoses waits for it.
//
// That is not thoroughness, it is the framing. On a stream with no delimiters
// an unconsumed byte is not harmless noise: it becomes the first byte of the
// next answer and offsets everything after it permanently. The yaesu and
// kenwood backends can fire a set off with Conn.Send and never look, because a
// stray frame there is skipped at the next ';'. Here there is no next ';'. So
// every command in this package goes out through Conn.Do, and the answer is
// consumed even when nothing is done with it.
//
// Its value is deliberately NOT read as a verdict. 00 and F0 are both reported
// in the field, F0 meaning roughly "already in that state" — which is exactly
// what a redundant PTT-off is, and the safety path sends those. Treating a
// value as a rejection would make ForceRX report a failure for a radio that was
// already receiving. So the byte is carried in Update.Raw for the wire trace
// and never turned into an error; the read-back is the authority, as everywhere
// else in this package.
//
// # How a slipped stream recovers
//
// It cannot recover in place: with no delimiter there is nothing to
// resynchronise on. What it can do is notice. The frequency-and-mode answer is
// four bytes of packed BCD, eight nibbles that must all be 0-9, so a frame
// offset by even one byte fails that test about forty-nine times in fifty.
// decodeFreqMode reports it as not-OK, the session turns that into an error,
// and five consecutive poll failures tear the connection down. The reconnect
// starts a clean stream. That path is the reason the check is there.
//
// # What these radios cannot do at all
//
//   - **Set transmit power.** There is no opcode for it, in a seventeen-command
//     set that has room for DCS codes and repeater shifts. SetPower refuses
//     rather than pretending, Caps reports no watt scale, and the front panel is
//     the only way.
//   - **Report or set a filter width, or select a filter slot.** The optional
//     YF-122 filters are chosen with the front panel's [B] and [C] keys. No CAT
//     command reads or writes either.
//   - **Send CW over CAT.** There is no keyer buffer command, so this package
//     does not implement backend.MorseSender, Caps reports CWMethod none, and
//     the daemon steers the operator to cw.method: serial_key.
//   - **Address a VFO by name.** The only VFO command, opcode 81, is a blind
//     toggle: it swaps A and B and answers with an acknowledgement that says
//     nothing about which one it landed on, and no read anywhere reports the
//     selection. Everything remoses reads and writes therefore applies to
//     whichever VFO the operator has selected, Caps advertises only
//     radio.VFOCurrent, and SetFrequency refuses A and B rather than tuning one
//     and labelling it the other.
//   - **Say which radio it is.** There is no ID command in this generation, so
//     the configured model is all remoses will ever know and there is no
//     identity cross-check to make.
//
// # Two more opcodes that exist and are not used
//
// LOCK (00/80), CLAR (05/85 and F5), SPLIT (02/82), repeater shift (09 and F9)
// and CTCSS/DCS (0A, 0B, 0C) are all in the chart and all deliberately
// unimplemented: remoses models none of them, and DESIGN.md's rule that the
// operator's radio is not ours to reconfigure applies to every one. The SPLIT
// bit in the TX status answer is decoded and dropped for the same reason —
// radio.State has no split field.
//
// # Mutable state
//
// Two values are carried between calls, and both are atomics rather than
// fields because Split and Decode run on the session's reader goroutine while
// the setters and Poll run on the command goroutine.
//
// pending is the framing, described above; it is written only by Expect, which
// the session calls under its write lock. transmitting is the last PTT reading,
// and shapes the poll: the RX status read is skipped while keyed, because its
// answer is a receive meter and the radio is not receiving.
package yaesubin

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Rig is the Yaesu five-byte binary CAT backend. Construct it with New.
type Rig struct {
	// profile is the model's table, and on this generation it is nearly the
	// same table for every radio. See model.go.
	profile Model
	// model is the configured model name in display form, used only in
	// messages. It stays empty when the configuration named none, so Model()
	// does not claim an identity the operator never asserted.
	model string

	// pending is what the command in flight will be answered with, and it is
	// this backend's entire framing. Written by Expect on the command goroutine
	// under the session's write lock, read by Split and Decode on the reader
	// goroutine. See the package doc and backend.ReplyFramer.
	pending atomic.Uint32

	// transmitting is the last PTT reading, from the TX status byte. A hint,
	// not state — the session's cache holds that — used only to decide whether
	// asking for a receive meter makes sense this tick.
	transmitting atomic.Bool
}

// New builds the backend from a radio's configuration, reading the same
// `yaesu:` block the ASCII backend reads.
//
// One block for two protocols is deliberate: which of them a radio speaks is a
// fact about the model, not a choice an operator should have to make, so
// `backend: yaesu` plus a model name is the whole configuration and remoses
// works out the rest. See Handles, which is what decides, and config.Yaesu.
//
// Unlike the ASCII backend a model is required here, because a name is what
// routed the radio to this package in the first place.
func New(r *config.Radio) (*Rig, error) {
	name := ""
	if r != nil && r.Yaesu != nil {
		name = r.Yaesu.Model
	}
	profile, err := LookupModel(name)
	if err != nil {
		return nil, err
	}
	return &Rig{profile: profile, model: profile.Label}, nil
}

// Model reports the configured radio.
//
// Unlike every other backend here this can never be confirmed or contradicted
// by the radio: this generation has no ID command at all, so there is nothing
// to ask and nothing to cross-check. What the operator wrote is what remoses
// reports.
func (y *Rig) Model() string { return y.model }

// Caps describes the configured radio.
//
// Most of it is a list of things these radios cannot do, and each false is a
// documented absence rather than an unimplemented feature — see the package
// doc. Publishing them honestly is what stops a client offering a power slider
// that would only ever draw an error.
func (y *Rig) Caps() radio.Caps {
	return radio.Caps{
		// Fresh slice per call: Caps is published through the API and a shared
		// backing array would be one mutation away from a data race.
		Modes: append([]radio.Mode(nil), y.profile.Modes...),

		// Only the VFO the rig is on. Opcode 81 toggles A and B blindly and
		// nothing reports which is selected, so naming one would be a guess
		// that a set command would then act on.
		VFOs: []radio.VFO{radio.VFOCurrent},

		// Opcode 08/88 keys the transmitter, so PTT is available even though
		// almost nothing else in this command set is.
		PTTControl: true,
		// The transmit status byte carries a forward-power reading and a
		// high-SWR flag, and nothing else: there is no ALC meter here, and the
		// SWR "meter" is one bit rather than a deflection. Both are published
		// only while the radio reports itself keyed.
		PowerMeter: true,
		SWRMeter:   true,
		ALCMeter:   false,
		// There is no power command of any kind, so there is no scale and no
		// ceiling to publish. Leaving MaxPowerW at zero rather than filling in
		// the nameplate 100 W is what makes the session refuse a request in
		// watts instead of sending one nothing can carry.
		PowerControl:      false,
		PowerWattAccurate: false,
		MaxPowerW:         0,

		FilterWidth: false,
		FilterSlots: 0,

		SMeterScale: meterScale,
		SubReceiver: false,

		// No keyer buffer command exists, so this type does not implement
		// backend.MorseSender at all and the daemon names serial_key as the
		// fix. Both radios have a KEY jack and a rear-panel data port, so
		// locally generated keying on a second serial port is the whole answer.
		CWMethod: radio.CWNone,
	}
}

// Init reads enough of the rig to fill State, and doubles as the link check.
//
// There is nothing to enable first. This generation has no auto-information, no
// transceive and no unsolicited traffic of any kind, so where the other
// backends open with AI2; or a Transceive check, this one opens by reading —
// which is also the only way to find out whether anything is listening. A radio
// that is switched off, or wired to the wrong jack, answers nothing and the
// first read times out.
//
// The reads are exactly the fast poll, so it is the fast poll: connecting and
// polling need the same three values, and writing them out twice would be two
// places to keep in step.
func (y *Rig) Init(ctx context.Context, c backend.Conn) error {
	if err := y.pollFast(ctx, c); err != nil {
		return fmt.Errorf("yaesubin: reading initial state from the %s: %w", y.profile.Label, err)
	}
	return nil
}

// Poll refreshes one tier of state.
//
// The fast tier is everything this protocol can report and the slow tier is
// empty, which is unusual enough to state plainly: power, filter width and
// filter slot are what the slow tier carries on every other backend, and these
// radios have no command for any of the three. Returning nil rather than
// inventing something to ask keeps the port quiet between fast ticks.
func (y *Rig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	switch tier {
	case backend.PollFast:
		return y.pollFast(ctx, c)
	case backend.PollSlow:
		return nil
	}
	return fmt.Errorf("yaesubin: unknown poll tier %d", tier)
}

// pollFast reads frequency, mode, PTT and a meter.
//
// Two or three transactions, and the order matters. Opcode 03 carries frequency
// and mode together. F7 is the only source of PTT — there is no bulk status
// answer with a TX/RX flag in it, and no push channel to learn it from — and it
// also settles whether the third read is worth making.
//
// E7 is skipped while transmitting. Its answer is the S-meter, which a radio
// that is transmitting is not measuring; these radios are reported to answer
// FF there, which decodes to a plausible full-scale reading rather than to
// anything recognisable as "no signal". Not asking is both cheaper and more
// honest than asking and discarding, and it means the meter a remote operator
// is watching goes from receive signal to transmit power and back without ever
// showing a value that was never real. See decodeTXStatus for the other half of
// that.
func (y *Rig) pollFast(ctx context.Context, c backend.Conn) error {
	if _, err := do(ctx, c, read(opReadFreqMode)); err != nil {
		return err
	}
	if _, err := do(ctx, c, read(opReadTXStatus)); err != nil {
		return err
	}
	if y.transmitting.Load() {
		return nil
	}
	_, err := do(ctx, c, read(opReadRXStatus))
	return err
}

// SetFrequency writes opcode 01.
//
// VFO A and B are refused rather than mapped onto the current VFO. The ASCII
// backend can treat VFOCurrent as A because FA reads and writes A by name;
// here there is no such command. Opcode 81 toggles the two blindly and nothing
// reports which is selected, so accepting "A" would mean tuning whichever VFO
// the operator happened to be on and telling the client it had tuned A —
// which, when they were on B, is a wrong answer of the kind this project
// refuses to give. Caps advertises only radio.VFOCurrent so a client can know
// this before asking.
func (y *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if vfo != radio.VFOCurrent {
		return fmt.Errorf("yaesubin: the %s has no CAT command that addresses VFO %s: "+
			"its only VFO command toggles A and B blindly and nothing reports which is selected, "+
			"so remoses tunes the VFO the rig is on and asks for it by no other name: %w",
			y.profile.Label, vfo, backend.ErrUnsupported)
	}
	if err := y.profile.checkFrequency(hz); err != nil {
		return err
	}
	f, err := encodeFrequency(hz)
	if err != nil {
		return err
	}
	if _, err := do(ctx, c, block(f[0], f[1], f[2], f[3], opSetFrequency)); err != nil {
		return err
	}
	// Not ceremony: the field is in units of ten hertz, so a request finer than
	// that was rounded before it went out, and the radio has its own idea of
	// what it will tune besides.
	_, err = do(ctx, c, read(opReadFreqMode))
	return err
}

// SetMode writes opcode 07.
//
// DATA is not a separate command here any more than it is on a modern Yaesu:
// PKT is its own mode code, so USB-with-data has no spelling and FM-with-data
// is one write. Which code carries which mode is Model.Codes.
func (y *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	code, err := y.profile.modeSet(m, dataMode)
	if err != nil {
		return err
	}
	if _, err := do(ctx, c, block(code, 0, 0, 0, opSetMode)); err != nil {
		return err
	}
	_, err = do(ctx, c, read(opReadFreqMode))
	return err
}

// SetPower always fails.
//
// Nothing in the seventeen-opcode chart sets or reads transmit power on any of
// these four radios. Caps says so — no watt scale and no ceiling — so a
// well-behaved client will not ask, and the session refuses a request in watts
// before it reaches here; this covers a request in percent, which it cannot.
func (y *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	return fmt.Errorf("yaesubin: the %s has no CAT command for transmit power, "+
		"in either watts or a relative scale; on this generation it is the "+
		"RF POWER SET menu item, reachable only from the front panel: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// SetPTT writes opcode 08 or 88.
//
// Unlike the ASCII dialects these are unambiguous — the opcode itself is the
// whole command and there is no read form to confuse them with. The answer is
// the usual acknowledgement, and it is waited for rather than fired and
// forgotten: on a stream with no delimiters an unconsumed byte would offset
// every answer after it. See the package doc.
//
// The value that comes back is not read as a verdict. Keying a radio that is
// already keyed is reported to answer F0 rather than 00, and the safety path
// sends a redundant unkey by design, so treating that as a failure would make
// ForceRX report an error for doing exactly what it was asked.
func (y *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	op := byte(opPTTOff)
	if on {
		op = opPTTOn
	}
	_, err := do(ctx, c, read(op))
	return err
}

// SetFilterWidth always fails.
//
// These radios have IF filters — the optional YF-122S, YF-122C and YF-122CN —
// and no CAT command that reads or selects one. The front panel's [B] and [C]
// keys are the only control, so there is nothing to send.
func (y *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	return fmt.Errorf("yaesubin: the %s has no CAT command for IF bandwidth; "+
		"its optional YF-122 filters are selected with the front-panel keys: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// SetFilterSlot always fails, for the same reason as SetFilterWidth. Caps
// reports FilterSlots 0, which is how a client learns not to ask.
func (y *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	return fmt.Errorf("yaesubin: the %s has no CAT command to select an IF filter; "+
		"the FIL-1 and FIL-2 slots are chosen with the front-panel keys: %w",
		y.profile.Label, backend.ErrUnsupported)
}

// do runs one transaction: five bytes out, and the answer that command
// produces back.
//
// The key comes from replyTo, the same function that sizes the answer for
// Split, so a transaction can never wait for a key that framing will not
// produce. Every command in this package goes through here, sets included —
// there is no Conn.Send call anywhere in this backend, and that is the point.
func do(ctx context.Context, c backend.Conn, req []byte) (backend.Update, error) {
	u, err := c.Do(ctx, req, replyTo(req).key())
	if err != nil {
		return u, fmt.Errorf("yaesubin: %s: %w", describe(req), err)
	}
	return u, nil
}
