// Package kenwood implements the Kenwood ASCII CAT protocol as spoken by the
// TS-590S and TS-590SG.
//
// Everything here is verified against the manufacturer's *TS-590S/TS-590SG PC
// Control Command Reference Guide* (B5A-0316-00). Where the two models differ
// the difference is noted at the command; almost nothing this backend uses is
// model-specific.
//
// # Shape
//
// The package obeys the backend contract literally: no goroutines, no timers, no
// locks, no Transport. Every command is a pure byte-builder handed to a
// backend.Conn, and every inbound frame goes through the same Decode, whether it
// answers a request or arrives unbidden because Auto Information is on.
//
// # Two quirks drive the design
//
// The first is that IF; — the one command that returns most of State in a single
// 38-byte answer — is simply refused while the rig is in Data mode. That is not
// an error path the poller can ignore, because Data mode is where a great many
// operators live. Poll therefore keeps a Data-mode flag, fed by every DA; it
// decodes, and drops to discrete FA;/MD; queries whenever the flag is set or the
// rig has already refused an IF;. See Poll.
//
// The second is that a Kenwood set command produces no answer at all unless AI
// happens to be on. Waiting for one would stall until the session's timeout, so
// sets are written with Conn.Send and, where the value has a home in State,
// followed by an explicit read. The read is not ceremony: PC rounds power down
// to the rig's 5 W grid, FW snaps the width onto its own ladder, and KS clamps
// the keyer speed, so what was asked for and what the rig took are routinely
// different numbers.
//
// # Mutable state
//
// Two values are carried between calls — the last decoded mode and the Data-mode
// flag. They are atomics, not fields: Decode runs on the session's reader
// goroutine while Poll and the setters run on the command goroutine, and the
// contract forbids a lock. Both are hints. Mode scales the PC watt-to-percent
// conversion (AM tops out at 25 W, everything else at 100) and gates FW; the
// Data-mode flag picks the poll shape. Neither is authoritative state — that
// lives in the session's cache.
package kenwood

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Name is the backend's registered name, matching `backend: kenwood` in the
// configuration file.
const Name = "kenwood"

func init() {
	backend.Register(Name, func(r *config.Radio) (backend.Rig, error) {
		k, err := New(r)
		if err != nil {
			return nil, err
		}
		return k, nil
	})
}

// Read commands, spelled out once so a typo cannot hide in a format string.
const (
	reqID = "ID;"
	reqFA = "FA;"
	reqFB = "FB;"
	reqMD = "MD;"
	reqDA = "DA;"
	reqPC = "PC;"
	reqFL = "FL;"
	reqFW = "FW;"
	reqIF = "IF;"
	reqSM = "SM0;" // P1 is the meter selector and is always 0 on this rig
	reqKY = "KY;"
)

// Rig is the Kenwood ASCII CAT backend. Construct it with New.
type Rig struct {
	// ai is the AI parameter to send at Init: 0 off, 2 on, 4 on with backup.
	ai int
	// bulkPoll enables the IF; fast poll. Config default is on.
	bulkPoll bool
	// model is the configured model name, used only in messages.
	model string

	// mode is the last mode decoded from MD; or from the mode digit inside an
	// IF answer. See the package doc for why these are atomics.
	mode atomic.Uint32
	// dataMode is the last DA; reading. IF; does not answer in Data mode.
	dataMode atomic.Bool
	// ifBlocked records that the rig refused or ignored an IF;. It is cleared by
	// a good IF answer or by a DA; that reports Data mode off, which gives the
	// bulk poll a retry cadence of one slow poll rather than never.
	ifBlocked atomic.Bool
	// id is the numeric ID; answer: 21 for a TS-590S, 23 for a TS-590SG.
	id atomic.Uint32
}

// New builds the backend from a radio's configuration. A missing kenwood block
// is not an error: the defaults (AI2, bulk polling on) are the ones the config
// package would have filled in.
func New(r *config.Radio) (*Rig, error) {
	k := &Rig{ai: 2, bulkPoll: true}
	if r != nil && r.Kenwood != nil {
		k.ai = r.Kenwood.AutoInformation
		k.bulkPoll = r.Kenwood.BulkPoll
		k.model = normaliseModel(r.Kenwood.Model)
	}
	// config.validate already rejects other values, but a backend built
	// directly in a test or by a future caller should not put a bad AI
	// parameter on the wire and desynchronise the stream.
	switch k.ai {
	case 0, 2, 4:
	default:
		return nil, fmt.Errorf("kenwood: auto_information must be 0, 2 or 4, have %d", k.ai)
	}
	return k, nil
}

// Model reports the rig identified by ID; at Init, falling back to the
// configured model name before Init has run.
func (k *Rig) Model() string {
	if n := k.id.Load(); n != 0 {
		if s, ok := modelNames[int(n)]; ok {
			return s
		}
		return fmt.Sprintf("Kenwood ID %03d", n)
	}
	return k.model
}

// lastMode is the mode the rig most recently reported, or ModeUnknown before the
// first MD; or IF; comes back.
func (k *Rig) lastMode() radio.Mode { return radio.Mode(k.mode.Load()) }

// Caps describes the TS-590 family.
func (k *Rig) Caps() radio.Caps {
	return radio.Caps{
		Modes: []radio.Mode{
			radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeCWR,
			radio.ModeAM, radio.ModeFM, radio.ModeFSK, radio.ModeFSKR,
		},
		VFOs: []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB},

		// PC is in real watts, which is unusual enough to be worth advertising:
		// clients can show a watt slider instead of a meaningless percentage.
		PowerWattAccurate: true,
		MaxPowerW:         nominalMaxPowerW,

		FilterWidth: true,
		FilterSlots: 2,
		SMeterScale: smeterScale,
		SubReceiver: false,

		CWMethod:  radio.CWViaCAT,
		CWCharset: Charset,
		CWMinWPM:  minWPM,
		CWMaxWPM:  maxWPM,
	}
}

// Init enables push updates and reads enough of the rig to fill State.
//
// AI is written without waiting for an answer: the rig answers a set command
// only when AI is already on, so the reply is present exactly when it is not
// needed. ID; immediately after doubles as the link check, and its answer tells
// the two models apart.
func (k *Rig) Init(ctx context.Context, c backend.Conn) error {
	if err := send(ctx, c, fmt.Sprintf("AI%d;", k.ai)); err != nil {
		return fmt.Errorf("kenwood: setting auto-information: %w", err)
	}

	reads := []struct {
		req string
		key backend.Key
	}{
		{reqID, keyID},
		{reqFA, keyFA},
		{reqMD, keyMD},
		{reqDA, keyDA}, // must precede IF;: it decides whether IF; will answer
		{reqPC, keyPC}, // must follow MD;: the watt ceiling depends on the mode
		{reqFL, keyFL},
	}
	for _, r := range reads {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}

	// IF; is read here regardless of the bulk_poll setting — it is the only way
	// to learn PTT at startup — but never in Data mode, where the rig would
	// simply not answer and the transaction would burn a full timeout.
	if k.dataMode.Load() {
		k.ifBlocked.Store(true)
		return nil
	}
	if _, err := do(ctx, c, reqIF, keyIF); err != nil {
		// Not fatal: the reads above already proved the link, and every field
		// IF; would have supplied except PTT has been read individually. Mark
		// it blocked so the fast poll uses the discrete path until a DA; says
		// otherwise.
		k.ifBlocked.Store(true)
	}
	return nil
}

// useBulkPoll reports whether the IF; fast poll is available right now.
func (k *Rig) useBulkPoll() bool {
	return k.bulkPoll && !k.dataMode.Load() && !k.ifBlocked.Load()
}

// Poll refreshes one tier of state.
//
// # The fast tier and the Data-mode fallback
//
// The preferred fast poll is IF; plus SM0; — two transactions, of which the
// first returns frequency, RX/TX and mode in one 38-byte answer.
//
// It is not always available. The reference states plainly that "the IF command
// cannot read the transceiver status while it is in Data mode", and the rig does
// not answer with an error: it says nothing, so a blind IF; costs a full
// timeout on every poll for as long as the operator stays in Data mode. The
// backend therefore tracks Data mode from every DA; it decodes and falls back to
// FA; + MD; + SM0; whenever it is set — as it also does when bulk polling is
// switched off in config, and after any IF; that failed.
//
// The fallback is three transactions instead of two and, more importantly, it
// cannot report PTT: TX;/RX; have no read form and IF; is the only query that
// carries the flag. In Data mode, PTT is observable only through the unsolicited
// TX;/RX; frames that Auto Information pushes, which is a good reason to leave
// AI on. (RI; would return frequency, mode and Data-mode status in one answer
// and is a natural future improvement, but it is absent from TS-590S firmware
// below 1.08 and does not carry PTT either.)
//
// A failed IF; is not retried inside the same call. One skipped fast poll is
// cheaper than three stacked timeouts on a link that has gone quiet, and the
// next poll takes the discrete path.
func (k *Rig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	switch tier {
	case backend.PollFast:
		return k.pollFast(ctx, c)
	case backend.PollSlow:
		return k.pollSlow(ctx, c)
	}
	return fmt.Errorf("kenwood: unknown poll tier %d", tier)
}

func (k *Rig) pollFast(ctx context.Context, c backend.Conn) error {
	if k.useBulkPoll() {
		if _, err := do(ctx, c, reqIF, keyIF); err != nil {
			k.ifBlocked.Store(true)
			return err
		}
		_, err := do(ctx, c, reqSM, keySM)
		return err
	}

	if _, err := do(ctx, c, reqFA, keyFA); err != nil {
		return err
	}
	if _, err := do(ctx, c, reqMD, keyMD); err != nil {
		return err
	}
	_, err := do(ctx, c, reqSM, keySM)
	return err
}

func (k *Rig) pollSlow(ctx context.Context, c backend.Conn) error {
	for _, r := range []struct {
		req string
		key backend.Key
	}{
		{reqPC, keyPC},
		{reqFL, keyFL},
		{reqDA, keyDA},
	} {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}

	// FW carries a bandwidth only in CW and FSK. In SSB and AM the rig refuses
	// it outright, and in FM it answers with a modulation-degree switch that
	// would land in State as a 0 Hz passband, so it is skipped in both cases
	// rather than asked and discarded.
	if !filterWidthLegal(k.lastMode()) {
		return nil
	}
	_, err := do(ctx, c, reqFW, keyFW)
	return err
}

// SetFrequency writes FA; or FB;.
//
// VFOCurrent maps to VFO A. The backend has no FR;/FT; tracking, and every read
// path it has — the FA; fast poll, the frequency inside IF; — is anchored on
// VFO A, so quietly following the rig's VFO selection here would produce a State
// whose frequency and its own writes disagreed.
func (k *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	var cmd string
	var key backend.Key
	switch vfo {
	case radio.VFOCurrent, radio.VFOA:
		cmd, key = "FA", keyFA
	case radio.VFOB:
		cmd, key = "FB", keyFB
	default:
		return fmt.Errorf("kenwood: the TS-590 has no %s VFO", vfo)
	}

	digits, err := formatFrequency(hz)
	if err != nil {
		return err
	}
	if err := send(ctx, c, cmd+digits+";"); err != nil {
		return err
	}
	_, err = do(ctx, c, cmd+";", key)
	return err
}

// SetMode writes MD; and, where the mode allows it, DA;.
//
// DA is rejected in CW and FSK, so it is sent only in LSB, USB, FM and AM.
// Skipping it elsewhere is safe rather than sloppy: the reference notes that
// "when used in any mode other than DATA mode, the P1 parameter response is
// always 0", so a rig switched from USB-DATA to CW reports Data mode off by
// itself and the next slow poll picks that up.
func (k *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	digit, err := encodeMode(m)
	if err != nil {
		return err
	}
	if dataMode && !supportsDataMode(m) {
		return fmt.Errorf("kenwood: %s has no DATA mode on the TS-590; DA is accepted only in LSB, USB, FM and AM", m)
	}

	if err := send(ctx, c, fmt.Sprintf("MD%c;", digit)); err != nil {
		return err
	}
	if _, err := do(ctx, c, reqMD, keyMD); err != nil {
		return err
	}

	if !supportsDataMode(m) {
		return nil
	}
	on := 0
	if dataMode {
		on = 1
	}
	if err := send(ctx, c, fmt.Sprintf("DA%d;", on)); err != nil {
		return err
	}
	_, err = do(ctx, c, reqDA, keyDA)
	return err
}

// SetPower writes PC; in watts.
//
// The requested value is clamped to the mode's range but deliberately not
// rounded onto the rig's 5 W grid: that grid only applies while the Power Fine
// setting is off, and there is no command to ask which it is. The read-back
// reports whatever the rig actually took.
func (k *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	w, err := wattsFromSet(p, k.lastMode())
	if err != nil {
		return err
	}
	if err := send(ctx, c, fmt.Sprintf("PC%03d;", w)); err != nil {
		return err
	}
	_, err = do(ctx, c, reqPC, keyPC)
	return err
}

// SetPTT writes TX; or RX;.
//
// These are the one pair that must never be sent with Do: they produce no answer
// unless AI is on, so waiting would stall the transmit path until the timeout —
// on the command that most needs to be prompt.
//
// TX; without a parameter means TX0, SEND, the microphone input. TX1 (DATA SEND,
// ACC2/USB) is not selected automatically even in Data mode: it changes which
// input modulates the transmitter, and a plain PTT flag carries no such
// intention.
func (k *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	if on {
		return send(ctx, c, "TX;")
	}
	return send(ctx, c, "RX;")
}

// SetFilterWidth writes FW;, after snapping the request onto the ladder the rig
// uses for the current mode. It fails rather than guessing in SSB and AM, where
// FW is not the right command, and in FM, where FW means something else
// entirely. See filterWidths.
func (k *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	snapped, err := snapFilterWidth(hz, k.lastMode())
	if err != nil {
		return err
	}
	if err := send(ctx, c, fmt.Sprintf("FW%04d;", snapped)); err != nil {
		return err
	}
	_, err = do(ctx, c, reqFW, keyFW)
	return err
}

// SetFilterSlot writes FL;: 1 selects IF Filter A, 2 selects IF Filter B.
func (k *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	if slot != 1 && slot != 2 {
		return fmt.Errorf("kenwood: filter slot must be 1 (IF Filter A) or 2 (IF Filter B), have %d", slot)
	}
	if err := send(ctx, c, fmt.Sprintf("FL%d;", slot)); err != nil {
		return err
	}
	_, err := do(ctx, c, reqFL, keyFL)
	return err
}

// do runs a read transaction, waiting for the command's own answer or for one of
// the rig's three error answers.
func do(ctx context.Context, c backend.Conn, req string, want backend.Key) (backend.Update, error) {
	keys := append([]backend.Key{want}, errorKeys...)
	u, err := c.Do(ctx, []byte(req), keys...)
	if err != nil {
		return u, fmt.Errorf("kenwood: %s: %w", req, err)
	}
	if !u.OK {
		return u, rejection(req, u)
	}
	return u, nil
}

// send writes a command the rig will not answer.
func send(ctx context.Context, c backend.Conn, req string) error {
	if err := c.Send(ctx, []byte(req)); err != nil {
		return fmt.Errorf("kenwood: %s: %w", req, err)
	}
	return nil
}

// rejection turns one of the rig's three error answers into something an
// operator can act on. They are otherwise anonymous — none of them names the
// command that provoked it.
func rejection(req string, u backend.Update) error {
	switch u.Key {
	case keyErrSyntax:
		return fmt.Errorf("kenwood: rig rejected %s (?;: bad syntax, or refused in the rig's current state)", req)
	case keyErrComm:
		return fmt.Errorf("kenwood: rig reported a serial error on %s (E;: overrun or framing; check baud rate and cabling)", req)
	case keyErrBusy:
		return fmt.Errorf("kenwood: rig could not finish processing %s (O;: try a slower poll interval)", req)
	}
	return fmt.Errorf("kenwood: rig rejected %s with %q", req, u.Raw)
}
