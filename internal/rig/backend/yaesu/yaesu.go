// Package yaesu implements the Yaesu ASCII CAT protocol as spoken by the
// FT-891, FT-991A, FT-710, FTdx10, FTdx101D, FTdx101MP and FTX-1, and by the
// older FT-950, FTdx1200, FTdx3000, FTdx5000 and FTdx9000.
//
// Everything here is transcribed from the manufacturer's own CAT Operation
// Reference Manual, one per radio. What differs per model lives in one place,
// model.go, and is selected by `yaesu.model` in the configuration.
//
// # Why this is not the kenwood backend
//
// Yaesu shares Kenwood's framing — two ASCII letters, fixed-width parameters, a
// ';' terminator, case-insensitive, sets answered by silence — and almost none
// of its fields. FA is eight or nine digits rather than eleven; MD takes a
// receiver selector and its codes run past 9 into the letters; DATA is folded
// into the mode code instead of living on its own DA; the IF answer is 27 to 30
// characters and laid out differently; SM is three digits on a 255 scale rather
// than four on a 30; SH takes a table index where FW takes Hz; AI is 0/1, so
// Kenwood's AI2; is a syntax error.
//
// Two of the shared command letters mean different things, which is the part
// that decides it. KY streams text on a Kenwood and plays a stored memory here.
// And TX — the one command where being wrong keys a transmitter — is a SET on a
// Kenwood and a READ here:
//
//	TX1;   key the transmitter
//	TX0;   unkey
//	TX;    read; answers TX0 receiving, TX1 keyed by CAT, TX2 keyed at the rig
//
// There is no RX command at all. A kenwood.SetPTT(true) aimed at a Yaesu sends
// TX;, which the radio answers as a query and does not transmit; the unkey
// would be RX;, which it does not have. That is the failure DESIGN.md §5.4
// records for the IC-718's 1C 01 — a command that succeeds and means something
// else — and it is why this is a separate package rather than a runtime branch
// inside SetPTT.
//
// # No CW over CAT, on any of them
//
// Not one of these radios has a CAT command that streams arbitrary Morse. KY is
// "play back stored keyer memory n"; the text lives in KM, which holds 50
// characters in each of five memories, cannot be queried for playback progress,
// and is the operator's own stored messages. Writing it to send a message would
// destroy them — a permanent change to somebody's radio as a side effect of
// connecting, which is the line DESIGN.md §5.2 already declined to cross for
// Icom Transceive — so remoses never writes KM, and this package deliberately
// does NOT implement backend.MorseSender. Caps reports CWMethod none on every
// model, and the daemon steers the operator to cw.method: serial_key, which
// every one of these radios supports through its PC KEYING menu item (RTS, DTR
// or DAKY). A partial MorseSender would be worse than none: the IC-718 lesson
// is that a successful type assertion produces failures that look to the
// operator like success.
//
// # Three quirks drive the rest of the design
//
// The first is that no manual documents any error, NAK or busy response, while
// the radios do send one: a bare '?'. It is handled — every transaction waits
// for it alongside its own answer, so a command refused that way fails in one
// round trip instead of the session's full per-command timeout — and it is
// handled as TRANSIENT. A '?' means "busy, ask again"; it never disables a
// poll item, never marks a capability absent and is never remembered. See
// keyBusy in codec.go and backend.ErrBusy.
//
// Everything the manuals do not cover still answers with silence, so nothing
// speculative is sent, values with a documented range are checked here before
// they go out, and a command a model does not have is recorded in its profile
// rather than probed: the FTdx9000 has no ID and no NA at all, and asking would
// cost a timeout apiece and answer nothing.
//
// The second is that the IF answer carries no TX/RX flag — Kenwood's P8 is
// Yaesu's CTCSS field — so PTT cannot come out of the bulk poll and needs its
// own TX;. The fast poll is three transactions. In exchange, PTT is readable in
// every mode, which the Kenwood backend cannot manage in Data mode, and IF; is
// never refused for being in data mode the way a TS-590's is, because data mode
// is just another mode code here.
//
// The third is that a set command produces no answer unless AI happens to be
// on. Waiting for one would stall until the timeout, so sets are written with
// Conn.Send and followed by an explicit read. The read is not ceremony: PC
// clamps, SH snaps onto a table index, and what was asked for and what the rig
// took are routinely different numbers.
//
// # Mutable state
//
// Four values are carried between calls: the last decoded mode, its DATA flag,
// the NA narrow setting and the FTX-1's power head. They are atomics, not
// fields, because Decode runs on the session's reader goroutine while Poll and
// the setters run on the command goroutine and the backend contract forbids a
// lock. All are hints. The first three choose which column of the SH bandwidth
// table applies; the last picks the power ceiling. None is authoritative state
// — that lives in the session's cache.
package yaesu

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Name is the backend's registered name, matching `backend: yaesu` in the
// configuration file.
const Name = "yaesu"

func init() {
	backend.Register(Name, func(r *config.Radio) (backend.Rig, error) {
		return New(r)
	})
}

// Read commands, spelled out once so a typo cannot hide in a format string.
//
// The selector on MD, SM, SH and NA is mandatory even on the single-receiver
// models, where the manuals document it as "0: Fixed". It is always 0 here:
// MAIN on the radios that have a second receiver, the only receiver on the
// rest.
const (
	reqID = "ID;"
	reqFA = "FA;"
	reqFB = "FB;"
	reqMD = "MD0;"
	reqPC = "PC;"
	reqIF = "IF;"
	reqSM = "SM0;"
	reqSH = "SH0;"
	reqNA = "NA0;"
	// reqTX is the PTT READ. It is not a typo for a keying command: TX1; keys
	// and TX0; unkeys. See the package doc.
	reqTX = "TX;"
)

// Rig is the Yaesu ASCII CAT backend. Construct it with New.
type Rig struct {
	// ai is whether to enable AI1 push updates at Init.
	ai bool
	// profile is the model's capability table, and the only thing in this
	// backend that knows one radio from another.
	profile Model
	// model is the configured model name in display form, used only in
	// messages. It stays empty when the configuration named none, so Model()
	// does not claim an identity the operator never asserted — the profile
	// still falls back to DefaultModel.
	model string

	// mode and dataMode are the last MD reading, or the mode code inside an IF
	// answer. Yaesu folds DATA into the code, so the two always move together.
	// See the package doc for why these are atomics.
	mode     atomic.Uint32
	dataMode atomic.Bool
	// narrow is the last NA reading, which selects the narrow or wide column of
	// the SH bandwidth table on the FT-991A and FT-891.
	narrow atomic.Bool
	// pcHead is the FTX-1's PC head selector as last reported: 1 the field
	// head, 2 the SPA-1 amplifier. Zero until PC; has been read.
	pcHead atomic.Uint32
	// id is the numeric ID; answer, e.g. 670 for an FT-991A. See ModelForID.
	id atomic.Uint32
}

// New builds the backend from a radio's configuration. A missing yaesu block is
// not an error: the defaults (AI on, DefaultModel) are the ones the config
// package would have filled in.
func New(r *config.Radio) (*Rig, error) {
	y := &Rig{ai: true}
	name := ""
	if r != nil && r.Yaesu != nil {
		y.ai = r.Yaesu.AutoInformation
		name = r.Yaesu.Model
		y.model = normaliseModel(name)
	}

	profile, err := LookupModel(name)
	if err != nil {
		return nil, err
	}
	y.profile = profile
	return y, nil
}

// Model reports the rig identified by ID; at Init, falling back to the
// configured model name before Init has run.
//
// What the rig says wins here because Yaesu's ID really does name a model: it
// is a fixed number in firmware, with no menu item for it, unlike an Icom's bus
// address. It does not change the profile, though: see checkIdentity.
func (y *Rig) Model() string {
	if n := y.id.Load(); n != 0 {
		if m, ok := ModelForID(int(n)); ok {
			return m.Label
		}
		return fmt.Sprintf("Yaesu ID %04d", n)
	}
	return y.model
}

// lastMode is the mode the rig most recently reported, or ModeUnknown before
// the first MD0; or IF; comes back.
func (y *Rig) lastMode() radio.Mode { return radio.Mode(y.mode.Load()) }

// maxPowerW is the top of the PC range right now.
//
// Only the FTX-1 makes this a question. Its PC names a head — 1 the field head
// alone, which stops at 10 W, 2 the SPA-1 amplifier, which reaches 100 — so the
// ceiling is a property of what is plugged in rather than of the model. Init
// reads PC; before anything can set power, and until it answers the bare head's
// 10 W stands: clamping low is the safe direction, and asking a 10 W radio for
// 100 would otherwise go out as a value it cannot take and answer with silence.
func (y *Rig) maxPowerW() int {
	if y.profile.PowerHead && y.pcHead.Load() == pcHeadAmp {
		return ftx1AmpMaxW
	}
	return y.profile.MaxPowerW
}

// Caps describes the configured radio, not the family.
//
// Every field that differs between models comes from the profile, because a
// capability list is a promise to the client: advertising PSK on an FT-991A,
// whose E is C4FM, would produce a UI that looks right and reads wrong.
func (y *Rig) Caps() radio.Caps {
	return radio.Caps{
		// Fresh slice per call: Caps is published through the API and a shared
		// backing array would be one mutation away from a data race.
		Modes: append([]radio.Mode(nil), y.profile.Modes...),
		VFOs:  []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB},

		// PC is in real watts on ten of the twelve, which is worth advertising:
		// clients can show a watt slider instead of a meaningless percentage.
		// On the FTdx5000 and FTdx9000 it is an uncalibrated index, so both
		// fields say so — MaxPowerW is left unset rather than filled in with
		// the nameplate rating, which would invite a client to read the index
		// as watts.
		PowerWattAccurate: !y.profile.PowerRaw,
		MaxPowerW:         float64(y.maxPowerW()),

		// False on the FTdx9000, whose SH is the WIDTH knob's position rather
		// than an index into a table of passbands.
		FilterWidth: y.profile.hasFilterWidth(),
		// There is no FL equivalent. Roofing filters exist on most of these
		// radios, and the FT-950 generation even has an RF command for them,
		// but remoses does not model a roofing filter as a filter slot: the
		// parameter mixes AUTO with fixed widths, so it is not the numbered
		// bank of IF filters FilterSlots describes.
		FilterSlots: 0,
		SMeterScale: smeterScale,
		// False even on the FTdx101 and FTX-1, which do have a second receiver:
		// this backend reads and writes MAIN only, so claiming otherwise would
		// promise control it does not implement.
		SubReceiver: false,

		// No Yaesu here can key arbitrary text over CAT. See the package doc;
		// this is what makes the daemon refuse cw.method: cat and name
		// serial_key as the fix.
		CWMethod: radio.CWNone,
	}
}

// read is one query and the key its answer arrives under. The poll and Init
// lists are built rather than written out, because which reads apply depends on
// the mode the rig is in.
type read struct {
	req string
	key backend.Key
}

// Init enables push updates and reads enough of the rig to fill State.
//
// AI is written without waiting for an answer: the rig answers a set command
// only when AI is already on, so the reply is present exactly when it is not
// needed. ID; immediately after doubles as the link check, and its answer says
// which radio this is. AI1 is safe to leave on — every manual that has an AI
// notes it reverts to off when the transceiver is switched off — so it does not
// permanently alter the operator's settings.
//
// Two of the reads are conditional, and both are the FTdx9000, which has no ID
// and no NA. Skipping them is not an optimisation: a Yaesu answers a command it
// does not implement with silence, so ID; there would burn the session's full
// per-command timeout and then fail the connect. That radio's link check is
// FA; instead, which every model has.
//
// The order matters at the end: MD0; settles the mode and its DATA flag and
// NA0; the narrow setting, and SH; cannot be turned into a bandwidth in Hz
// without both.
func (y *Rig) Init(ctx context.Context, c backend.Conn) error {
	if y.profile.HasAI {
		ai := "AI0;"
		if y.ai {
			ai = "AI1;"
		}
		if err := send(ctx, c, ai); err != nil {
			return fmt.Errorf("yaesu: setting auto-information: %w", err)
		}
	}

	var reads []read
	if y.profile.HasID {
		reads = append(reads, read{reqID, keyID})
	}
	reads = append(reads,
		read{reqFA, keyFA},
		read{reqMD, keyMD},
		read{reqPC, keyPC},
		// PTT has no other source: there is no TX/RX flag in IF, so this is the
		// only way to learn it at startup or ever.
		read{reqTX, keyTX},
	)
	if y.profile.HasNarrow {
		reads = append(reads, read{reqNA, keyNA})
	}
	for _, r := range reads {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	y.checkIdentity()

	// SH is asked only where its answer means a bandwidth. In AM and FM it
	// either has no table column at all or holds one fixed value per mode code,
	// and a rig that refuses the command answers with silence rather than an
	// error, which would cost a full timeout.
	if !y.filterWidthLegal() {
		return nil
	}
	_, err := do(ctx, c, reqSH, keySH)
	return err
}

// checkIdentity warns when the rig's ID; answer names a different radio than
// the configuration does.
//
// It is a cross-check, never model detection. Yaesu's ID is trustworthy — much
// more so than the menu-configurable bus address an Icom answers 19 00 with —
// but acting on it would mean silently switching command sets, and therefore
// mode tables, under an operator who wrote something specific. Naming the
// mismatch is what an operator needs; the configuration stays authoritative.
// The FTdx1200 is why the profile holds a list rather than one number: 0582 and
// 0583 are the same radio reporting whether the optional FFT-1 unit is fitted,
// so either one matches and neither is a mismatch.
func (y *Rig) checkIdentity() {
	reported := int(y.id.Load())
	if reported == 0 || len(y.profile.IDs) == 0 || y.profile.knowsID(reported) {
		return
	}
	detail := fmt.Sprintf("%04d", reported)
	if m, ok := ModelForID(reported); ok {
		detail = fmt.Sprintf("%04d (a %s)", reported, m.Label)
	}
	slog.Warn("yaesu: the radio identifies itself as a different model than configured",
		"configured_model", y.profile.Name,
		"configured_id", y.profile.idList(),
		"reported_id", detail,
		"note", "yaesu.model may not match this radio; remoses keeps using the configured command set, "+
			"including its mode-code table")
}

// filterWidthLegal reports whether SH carries a bandwidth in the mode the rig is
// in, so the poll can skip it rather than provoke a silent refusal.
func (y *Rig) filterWidthLegal() bool {
	_, ok := y.profile.Widths.ladder(y.lastMode(), y.dataMode.Load(), y.narrow.Load())
	return ok
}

// Poll refreshes one tier of state.
//
// The fast tier is three transactions: IF; for frequency and mode, TX; for PTT
// and SM0; for the S-meter. It cannot be two. Kenwood's IF carries a TX/RX flag
// at P8; Yaesu's P8 is CTCSS, and no field anywhere in the answer reports
// whether the rig is transmitting, so PTT has to be asked for separately.
//
// What Yaesu gives back for that is worth having: IF; answers in every mode,
// where a TS-590 refuses it outright in Data mode, so none of the Kenwood
// backend's fallback machinery has an analogue here. And TX; has a read form at
// all, which Kenwood's TX;/RX; do not — so PTT is polled unconditionally rather
// than depending on push frames.
//
// The slow tier is power, the narrow setting, and the filter width where the
// current mode has one.
func (y *Rig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	switch tier {
	case backend.PollFast:
		return y.pollFast(ctx, c)
	case backend.PollSlow:
		return y.pollSlow(ctx, c)
	}
	return fmt.Errorf("yaesu: unknown poll tier %d", tier)
}

func (y *Rig) pollFast(ctx context.Context, c backend.Conn) error {
	for _, r := range []read{{reqIF, keyIF}, {reqTX, keyTX}, {reqSM, keySM}} {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	return nil
}

func (y *Rig) pollSlow(ctx context.Context, c backend.Conn) error {
	// NA before SH: it decides which column of the bandwidth table the SH index
	// is read against on the FT-991A, FT-891 and the whole FT-950 generation.
	// The FTdx9000 has no NA command, and no bandwidth table for it to choose a
	// column of either.
	reads := []read{{reqPC, keyPC}}
	if y.profile.HasNarrow {
		reads = append(reads, read{reqNA, keyNA})
	}
	for _, r := range reads {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	if !y.filterWidthLegal() {
		return nil
	}
	_, err := do(ctx, c, reqSH, keySH)
	return err
}

// SetFrequency writes FA; or FB;.
//
// VFOCurrent maps to VFO A — MAIN on the dual-receiver models. The backend has
// no VFO-selection tracking, and every read path it has is anchored on VFO A
// (the FA; read, the frequency inside IF;), so quietly following the rig's own
// VFO selection here would produce a State whose frequency and its own writes
// disagreed.
func (y *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	var cmd string
	var key backend.Key
	switch vfo {
	case radio.VFOCurrent, radio.VFOA:
		cmd, key = "FA", keyFA
	case radio.VFOB:
		cmd, key = "FB", keyFB
	default:
		return fmt.Errorf("yaesu: the %s has no %s VFO", y.profile.Label, vfo)
	}

	if err := y.profile.checkFrequency(hz); err != nil {
		return err
	}
	// The width comes from the model, not from the wire: eight digits on the
	// FT-950 generation and nine on the FTdx101 generation, and a field of the
	// wrong width is a malformed command rather than a tolerated shorthand.
	digits, err := formatFrequency(hz, y.profile.FreqDigits)
	if err != nil {
		return err
	}
	if err := send(ctx, c, cmd+digits+";"); err != nil {
		return err
	}
	_, err = do(ctx, c, cmd+";", key)
	return err
}

// SetMode writes MD0<code>;.
//
// There is nothing to follow it with. Yaesu has no DA command: DATA is a
// property of the mode code itself, so USB-DATA is one write of MD0C; rather
// than a mode and then a flag. Which code carries which mode is per model — see
// Model.Codes — and the read-back is the model's own MD0; so a rig that refused
// the change reports what it actually did.
func (y *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	set, err := y.profile.modeSet(m, dataMode)
	if err != nil {
		return err
	}
	if err := send(ctx, c, set); err != nil {
		return err
	}
	_, err = do(ctx, c, reqMD, keyMD)
	return err
}

// SetPower writes PC;, in watts on every model but two.
//
// On the FTdx5000 and FTdx9000 the same three digits are an uncalibrated 000-255
// index, so a request in watts is refused there rather than converted with a
// number remoses invented — see rawFromSet.
//
// On the FTX-1 the command carries a head selector, and remoses sends back
// whichever head the rig last reported rather than choosing one: the selector
// says which amplifier chain the value applies to, not which to use, so
// inventing it would set the power of hardware that may not be attached.
func (y *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	from := func() (int, error) { return wattsFromSet(p, y.maxPowerW()) }
	if y.profile.PowerRaw {
		from = func() (int, error) { return rawFromSet(p) }
	}
	w, err := from()
	if err != nil {
		return err
	}

	set := fmt.Sprintf("PC%03d;", w)
	if y.profile.PowerHead {
		head := y.pcHead.Load()
		if head == 0 {
			head = pcHeadRadio
		}
		set = fmt.Sprintf("PC%d%03d;", head, w)
	}
	if err := send(ctx, c, set); err != nil {
		return err
	}
	_, err = do(ctx, c, reqPC, keyPC)
	return err
}

// SetPTT writes TX1; or TX0;.
//
// The spelling is the single most important difference in this package. A bare
// TX; is the READ — it keys nothing and answers with the current state — where
// on a Kenwood it is what keys the transmitter. There is no RX command.
//
// Like Kenwood's, these must never be sent with Do: they produce no answer
// unless AI is on, so waiting would stall the transmit path until the timeout,
// on the command that most needs to be prompt.
func (y *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	if on {
		return send(ctx, c, "TX1;")
	}
	return send(ctx, c, "TX0;")
}

// SetFilterWidth writes SH;, after turning the requested width in Hz into the
// table index the command actually takes.
//
// SH does not carry a bandwidth. Its parameter indexes a table whose meaning
// depends on the current mode and, on the FT-991A and FT-891, on the NA narrow
// setting — so a request is a reverse lookup that can fail, and it fails here
// rather than on the wire, where a rejected command would answer with silence.
// The request is snapped onto the model's ladder first; see snapWidth for the
// rule and why remoses does the rounding itself.
func (y *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	mode := y.lastMode()
	ladder, ok := y.profile.Widths.ladder(mode, y.dataMode.Load(), y.narrow.Load())
	if !ok {
		if !y.profile.hasFilterWidth() {
			return fmt.Errorf("yaesu: the %s's SH is the position of the WIDTH knob "+
				"(00 anticlockwise to 31 clockwise, 16 centred), not a bandwidth in Hz, "+
				"and its manual gives no table converting the two", y.profile.Label)
		}
		return fmt.Errorf("yaesu: SH carries no bandwidth in %s on the %s; it has a table column "+
			"only in SSB, CW, RTTY, PSK and the DATA modes", mode, y.profile.Label)
	}
	index, _, ok := snapWidth(ladder, hz)
	if !ok {
		return fmt.Errorf("yaesu: the %s has no SH bandwidth table for %s", y.profile.Label, mode)
	}
	if err := send(ctx, c, y.profile.filterSet(index, y.narrow.Load())); err != nil {
		return err
	}
	_, err := do(ctx, c, reqSH, keySH)
	return err
}

// SetFilterSlot always fails.
//
// There is no FL equivalent in any of these manuals. The FTdx10 and FTdx101 do
// have roofing filters, but no command in their command lists selects one, so
// there is nothing to send — and Caps.FilterSlots is 0, which is how a client
// learns not to ask.
func (y *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	return fmt.Errorf("yaesu: the %s has no IF filter selection over CAT; "+
		"there is no FL-equivalent command, and the roofing filter is not exposed", y.profile.Label)
}

// do runs a read transaction, waiting for the command's own answer or for the
// busy answer.
//
// The busy key is on every wait list for the same reason kenwood's error keys
// are on its: it is what turns a refusal into one round trip instead of the
// session's full per-command timeout. It also covers the sets, which are
// written with Send and answered by nothing — a ?; provoked by a set arrives
// while the read-back that follows it is waiting, and fails that instead.
//
// It carries kenwood's blind spot too: a late ?; belonging to a command that
// has already timed out can complete the next transaction. That costs one
// retry, against a full timeout on every refusal for not looking.
//
// The busy answer is checked before err, because the session reports any
// not-OK update as rig.ErrNAK — a permanent rejection, which is precisely what
// ?; is not. See keyBusy and backend.ErrBusy.
func do(ctx context.Context, c backend.Conn, req string, want backend.Key) (backend.Update, error) {
	u, err := c.Do(ctx, []byte(req), want, keyBusy)
	if u.Key == keyBusy {
		return u, fmt.Errorf("yaesu: %s: the rig answered ?; (busy, or refused in its current state): %w",
			req, backend.ErrBusy)
	}
	if err != nil {
		return u, fmt.Errorf("yaesu: %s: %w", req, err)
	}
	return u, nil
}

// send writes a command the rig will not answer.
func send(ctx context.Context, c backend.Conn, req string) error {
	if err := c.Send(ctx, []byte(req)); err != nil {
		return fmt.Errorf("yaesu: %s: %w", req, err)
	}
	return nil
}
