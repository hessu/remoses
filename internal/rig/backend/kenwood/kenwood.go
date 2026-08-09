// Package kenwood implements the Kenwood ASCII CAT protocol as spoken by the
// TS-480, TS-590S, TS-590SG, TS-890S and TS-990S.
//
// Everything here is verified against the manufacturers' own PC Control Command
// Reference Guides, one per radio. The framing and most parameter encodings are
// family-wide; what differs per model lives in one place, model.go, and is
// selected by `kenwood.model` in the configuration. See Model for why that is a
// table rather than a run-time probe.
//
// # Shape
//
// The package obeys the backend contract literally: no goroutines, no timers, no
// locks, no Transport. Every command is a pure byte-builder handed to a
// backend.Conn, and every inbound frame goes through the same Decode, whether it
// answers a request or arrives unbidden because Auto Information is on.
//
// # Three quirks drive the design
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
// The third belongs to the newest radios. The TS-890S and TS-990S have no IF;
// at all, and IF; is the only query on this protocol that carries the TX/RX
// flag: TX;/RX; are set-only and answer nothing unless AI is on. On those two
// models PTT therefore CANNOT BE POLLED — it is observable only through the AI
// push frames Decode already handles, which makes leaving AI on a requirement
// rather than a preference there. It is a genuine limitation of the command set,
// not an omission here. See Poll.
//
// # Mutable state
//
// Two values are carried between calls — the last decoded mode and the Data-mode
// flag. They are atomics, not fields: Decode runs on the session's reader
// goroutine while Poll and the setters run on the command goroutine, and the
// contract forbids a lock. Both are hints. Mode scales the PC watt-to-percent
// conversion (AM tops out at a quarter of the rig's nominal power) and gates FW;
// the Data-mode flag picks the poll shape. Neither is authoritative state — that
// lives in the session's cache.
package kenwood

import (
	"context"
	"fmt"
	"log/slog"
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
	reqOM = "OM0;" // P1 selects the display area: 0 is the left one, the main receiver
	reqDA = "DA;"
	reqPC = "PC;"
	reqFW = "FW;"
	reqIF = "IF;"
	reqSM = "SM0;" // P1 is the meter selector, 0 for the main receiver's meter
	// reqSMNoSelector is the TS-890S form. Its SM has no meter selector at all,
	// so asking with SM0; would be a syntax error and the answer is one
	// character shorter. See Model.SMeterRequest.
	reqSMNoSelector = "SM;"
	reqKY           = "KY;"
	reqSD           = "SD;" // CW break-in delay; 0 ms means full break-in
	reqFR           = "FR;" // receive VFO; sending it also forces simplex
	reqFT           = "FT;" // transmit VFO; sending it also forces split
	reqRM           = "RM;" // meter function; answers SWR, COMP and ALC in turn
)

// Rig is the Kenwood ASCII CAT backend. Construct it with New.
type Rig struct {
	// ai is the AI parameter to send at Init: 0 off, 2 on, 4 on with backup.
	ai int
	// bulkPoll enables the IF; fast poll. Config default is on. It is an upper
	// bound, not a promise: profile.BulkPoll can veto it for a radio that has no
	// IF; at all.
	bulkPoll bool
	// profile is the model's capability table, and the only thing in this
	// backend that knows one radio from another.
	profile Model
	// model is the configured model name in display form, used only in
	// messages. It stays empty when the configuration named none, so Model()
	// does not claim an identity the operator never asserted — the profile still
	// falls back to DefaultModel.
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
	// id is the numeric ID; answer, e.g. 21 for a TS-590S. See ModelForID.
	id atomic.Uint32

	// breakIn holds a radio.BreakIn, the last reading of the break-in setting.
	// The CW path consults it before every send, so it must not cost a round
	// trip; nil until the first BI/VX answer is decoded.
	breakIn atomic.Value
	// breakInDelayMS is the last SD reading. On the styles whose on/off command
	// has only two values it is the only thing that separates semi from full,
	// so it is kept even though State does not publish it.
	breakInDelayMS atomic.Int32

	// transmitting is the last PTT reading, from an IF answer or a TX;/RX;
	// push. It decides two things: whether an SM answer is the S-meter or the
	// RF power meter, and whether the fast poll asks for RM at all.
	transmitting atomic.Bool

	// receiveVFO and transmitVFO each hold a radio.VFO, from FR;, FT; or the
	// P10 and P12 fields of an IF answer. Split is the relationship between
	// them rather than a flag either one carries, and SetSplit needs the
	// receive one before it can name a transmit VFO.
	receiveVFO  atomic.Value
	transmitVFO atomic.Value
}

// New builds the backend from a radio's configuration. A missing kenwood block
// is not an error: the defaults (AI2, bulk polling on, DefaultModel) are the
// ones the config package would have filled in.
func New(r *config.Radio) (*Rig, error) {
	k := &Rig{ai: 2, bulkPoll: true}
	name := ""
	if r != nil && r.Kenwood != nil {
		k.ai = r.Kenwood.AutoInformation
		k.bulkPoll = r.Kenwood.BulkPoll
		name = r.Kenwood.Model
		k.model = normaliseModel(name)
	}

	profile, err := LookupModel(name)
	if err != nil {
		return nil, err
	}
	k.profile = profile

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
//
// What the rig says wins here because ID; really does name a model — it is fixed
// in firmware, unlike an Icom's menu-configurable bus address. It does not
// change the profile, though: see checkIdentity.
func (k *Rig) Model() string {
	if n := k.id.Load(); n != 0 {
		if m, ok := ModelForID(int(n)); ok {
			return m.Label
		}
		return fmt.Sprintf("Kenwood ID %03d", n)
	}
	return k.model
}

// lastMode is the mode the rig most recently reported, or ModeUnknown before the
// first MD; or IF; comes back.
func (k *Rig) lastMode() radio.Mode { return radio.Mode(k.mode.Load()) }

// Caps describes the configured radio, not the family.
//
// Every field that differs between models comes from the profile, because a
// capability list is a promise to the client: advertising a filter width on a
// TS-890S, or a 30-dot meter on a rig whose meter has 70, produces a UI that
// looks right and reads wrong.
func (k *Rig) Caps() radio.Caps {
	return radio.Caps{
		// Fresh slice per call: Caps is published through the API and a shared
		// backing array would be one mutation away from a data race.
		Modes: append([]radio.Mode(nil), k.profile.Modes...),
		// Only the models whose FA and FB are two VFOs of one receiver. This
		// used to advertise A and B on every radio here while nothing could
		// address either, so a client that believed the list got a 422 for
		// every request naming one.
		VFOs: k.vfos(),

		// FA and FB address a VFO by name, so the labels are stable: A is
		// always A. Contrast the IC-9700, where the protocol only offers
		// "selected" and "unselected" and remoses has to say so.
		VFOAddressing: k.vfoAddressing(),
		// FR and FT set which VFO is received and which transmitted, which is
		// what split is on this protocol.
		Split: k.profile.VFOPair.twoVFOs(),
		// But there is no per-VFO mode: MD applies to the selected VFO and
		// nothing addresses the other one's. See SetVFOMode.
		PerVFOMode: false,
		// One receiver, one VFO received at a time.
		DualWatch: false,

		// PC is in real watts, which is unusual enough to be worth advertising:
		// clients can show a watt slider instead of a meaningless percentage.
		// TX;/RX; and PC are on every radio in this family.
		PTTControl:        true,
		PowerControl:      true,
		PowerWattAccurate: true,
		MaxPowerW:         float64(k.profile.MaxPowerW),

		FilterWidth: k.profile.FilterWidth,
		FilterSlots: k.profile.FilterSelect.slots(),
		SMeterScale: k.profile.SMeterScale,
		// All three, family-wide. Forward power has no command of its own: SM
		// reads the RF power meter instead of the S-meter while keyed. SWR and
		// ALC come from RM, which answers with both (and COMP) at once.
		PowerMeter: true,
		SWRMeter:   true,
		ALCMeter:   true,
		// False even on the TS-990S, which has a second receiver: this backend
		// reads and writes one of them, so claiming otherwise would promise
		// control it does not implement.
		SubReceiver: false,

		// Break-in is per model, and on the TS-480 there is no command for it
		// at all. See BreakInStyle.
		BreakInControl: k.profile.BreakIn != BreakInNone,

		// KY and KS are family-wide, so CW is not per model: a 24-character
		// buffer and a 4-60 wpm keyer on every radio here.
		CWMethod:  radio.CWViaCAT,
		CWCharset: Charset,
		CWMinWPM:  minWPM,
		CWMaxWPM:  maxWPM,
	}
}

// vfos is the addressable VFO list for this model.
func (k *Rig) vfos() []radio.VFO {
	if k.profile.VFOPair.twoVFOs() {
		return []radio.VFO{radio.VFOCurrent, radio.VFOA, radio.VFOB}
	}
	return []radio.VFO{radio.VFOCurrent}
}

// vfoAddressing is empty where there is no addressing to describe, so the field
// stays out of the published capabilities rather than claiming a scheme.
func (k *Rig) vfoAddressing() string {
	if k.profile.VFOPair.twoVFOs() {
		// Stable labels: FA is VFO A on every one of these radios, unlike the
		// IC-9700's selected/unselected pair.
		return "named"
	}
	return ""
}

// read is one query and the key its answer arrives under. The poll and Init
// lists are built rather than written out, because which reads apply is a
// property of the model.
type read struct {
	req string
	key backend.Key
}

// Init enables push updates and reads enough of the rig to fill State.
//
// AI is written without waiting for an answer: the rig answers a set command
// only when AI is already on, so the reply is present exactly when it is not
// needed. ID; immediately after doubles as the link check, and its answer says
// which radio this is.
//
// On a model without IF; there is no PTT read at all, here or later, so State
// starts with PTT false and only an AI push can correct it. See Poll.
func (k *Rig) Init(ctx context.Context, c backend.Conn) error {
	if err := send(ctx, c, fmt.Sprintf("AI%d;", k.ai)); err != nil {
		return fmt.Errorf("kenwood: setting auto-information: %w", err)
	}

	reads := []read{
		{reqID, keyID},
		{reqFA, keyFA},
		{k.profile.modeReq(), k.profile.modeKey()},
	}
	// Both VFOs and the receive/transmit selection at connect, so the first
	// client to look sees VFO B filled in rather than a zero, and so that
	// State.Frequency is anchored on the VFO the operator is actually on before
	// the first poll rather than assuming it is A.
	if k.profile.VFOPair.twoVFOs() {
		reads = append(reads, read{reqFB, keyFB}, read{reqFR, keyFR}, read{reqFT, keyFT})
	}
	// DA; must precede IF;: it decides whether IF; will answer at all. On the OM
	// models there is nothing to ask — the mode read above already settled DATA
	// — and on the TS-480 there is no DA command to ask with.
	if k.profile.DataMode == DataModeCommand {
		reads = append(reads, read{reqDA, keyDA})
	}
	// PC; must follow the mode read: the watt ceiling depends on the mode.
	reads = append(reads, read{reqPC, keyPC})
	if fl := k.profile.filterSlotRead(); fl != "" {
		reads = append(reads, read{fl, keyFL})
	}

	for _, r := range reads {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	k.checkIdentity()

	// IF; is read here regardless of the bulk_poll setting — it is the only way
	// to learn PTT at startup — but never on a radio that does not have it, and
	// never in Data mode, where the rig would simply not answer and the
	// transaction would burn a full timeout.
	if !k.profile.BulkPoll {
		return nil
	}
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

// checkIdentity warns when the rig's ID; answer names a different radio than the
// configuration does.
//
// It is a cross-check, never model detection. The ID is trustworthy, but acting
// on it would mean silently switching command sets under an operator who wrote
// something specific, and a radio remoses has no profile for answers with an ID
// too. Naming the mismatch is what an operator needs; the configuration stays
// authoritative.
func (k *Rig) checkIdentity() {
	reported := int(k.id.Load())
	if reported == 0 || k.profile.ID == 0 || reported == k.profile.ID {
		return
	}
	detail := fmt.Sprintf("%03d", reported)
	if m, ok := ModelForID(reported); ok {
		detail = fmt.Sprintf("%03d (a %s)", reported, m.Label)
	}
	slog.Warn("kenwood: the radio identifies itself as a different model than configured",
		"configured_model", k.profile.Name,
		"configured_id", fmt.Sprintf("%03d", k.profile.ID),
		"reported_id", detail,
		"note", "kenwood.model may not match this radio; remoses keeps using the configured command set")
}

// useBulkPoll reports whether the IF; fast poll is available right now. The
// profile has the final say: on a radio without IF; there is nothing for the
// bulk_poll setting to enable.
func (k *Rig) useBulkPoll() bool {
	return k.profile.BulkPoll && k.bulkPoll && !k.dataMode.Load() && !k.ifBlocked.Load()
}

// Poll refreshes one tier of state.
//
// # The fast tier and the Data-mode fallback
//
// The preferred fast poll is IF; plus the S-meter — two transactions, of which
// the first returns frequency, RX/TX and mode in one 38-byte answer.
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
//
// # The TS-890S and TS-990S have no bulk poll and no PTT at all
//
// Neither radio implements IF;. Their fast poll is therefore permanently the
// discrete one — FA; + OM0; + the S-meter — and since IF; is the only query in
// this protocol that carries the TX/RX flag, PTT IS NEVER POLLED ON THEM. It is
// not that this backend chooses not to: TX; and RX; are set commands that answer
// nothing unless AI is on, and there is no read form to ask with. PTT reaches
// State only as an AI push frame, which Decode handles, so a station running
// AI0 on one of these radios will see PTT stuck at whatever it was last told.
// That is a property of the command set, and the reason the configuration
// default is AI2.
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
		// SM before RM, and it matters: IF; has just settled whether the rig is
		// transmitting, which is what decides whether the SM answer is the
		// S-meter or the RF power meter.
		if _, err := do(ctx, c, k.profile.SMeterRequest, keySM); err != nil {
			return err
		}
		return k.pollTXMeters(ctx, c)
	}

	// The frequency of the VFO being received, which is not always A: on a
	// radio parked on VFO B, polling FA; would keep overwriting State with the
	// frequency of the VFO the operator is not listening to. The bulk path
	// above has no such problem, since IF; reports the displayed frequency.
	req, key := k.receiveFreqRead()
	if _, err := do(ctx, c, req, key); err != nil {
		return err
	}
	if _, err := do(ctx, c, k.profile.modeReq(), k.profile.modeKey()); err != nil {
		return err
	}
	if _, err := do(ctx, c, k.profile.SMeterRequest, keySM); err != nil {
		return err
	}
	return k.pollTXMeters(ctx, c)
}

// pollTXMeters reads SWR and ALC, and only while the transmitter is up.
//
// In receive they are meaningless — there is no forward power and no ALC action
// — and RM would spend a transaction to publish two zeroes a client could not
// tell from real readings. Forward power is not read here: it arrives on SM,
// which the fast poll already sends in both directions.
//
// One RM; draws three answers, of which the transaction consumes the first and
// Decode folds the rest in as unsolicited frames.
func (k *Rig) pollTXMeters(ctx context.Context, c backend.Conn) error {
	for _, r := range k.txMeterReads() {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}
	return nil
}

// receiveFreqRead is the frequency query for whichever VFO is being received.
// FA until something says otherwise, which is both the safe default and what
// this backend did before it could tell the two apart.
func (k *Rig) receiveFreqRead() (string, backend.Key) {
	if k.profile.VFOPair.twoVFOs() && k.rxVFO() == radio.VFOB {
		return reqFB, keyFB
	}
	return reqFA, keyFA
}

func (k *Rig) pollSlow(ctx context.Context, c backend.Conn) error {
	reads := []read{{reqPC, keyPC}}
	// The parked VFO and the split selection. The received VFO's frequency is
	// already on the fast tier; this is the other one, which changes only when
	// somebody moves it.
	if k.profile.VFOPair.twoVFOs() {
		reads = append(reads, read{reqFB, keyFB}, read{reqFR, keyFR}, read{reqFT, keyFT})
	}
	if fl := k.profile.filterSlotRead(); fl != "" {
		reads = append(reads, read{fl, keyFL})
	}
	// DATA has a command of its own only where MD does the mode: the OM models
	// carry it in the mode code the fast poll already read, and the TS-480 has
	// no DATA mode to read.
	if k.profile.DataMode == DataModeCommand {
		reads = append(reads, read{reqDA, keyDA})
	}
	for _, r := range reads {
		if _, err := do(ctx, c, r.req, r.key); err != nil {
			return err
		}
	}

	// Break-in, but only in CW: on the TS-890S and TS-990S the reference says
	// BI "can only be performed in CW mode" and returns 0 in any other, and on
	// the TS-590 the VX it shares with VOX would answer about VOX instead. In
	// both cases a reading taken outside CW would be a confident wrong answer,
	// which is worse than not asking.
	if req := k.breakInRead(); req != "" {
		// The delay first: it is what turns "on" into semi or full.
		if k.profile.BreakIn.binary() {
			if _, err := do(ctx, c, reqSD, keySD); err != nil {
				return err
			}
		}
		if _, err := do(ctx, c, req, breakInKey(k.profile.BreakIn)); err != nil {
			return err
		}
	}

	// FW carries a bandwidth only on the models that have it as a width command
	// at all, and there only in CW and FSK. In SSB and AM the rig refuses it
	// outright, and in FM it answers with a modulation-degree switch that would
	// land in State as a 0 Hz passband, so it is skipped in both cases rather
	// than asked and discarded.
	if !k.profile.FilterWidth || !filterWidthLegal(k.lastMode()) {
		return nil
	}
	_, err := do(ctx, c, reqFW, keyFW)
	return err
}

// SetFrequency writes FA; or FB;.
//
// VFOCurrent means the VFO the radio is receiving on, which FR; reports and
// which an IF answer carries on every fast poll. It used to mean VFO A
// unconditionally, because nothing tracked the selection — so tuning a rig
// parked on VFO B moved the other VFO and the operator's frequency did not
// change. Until something has reported the selection it still falls back to A,
// which is the same assumption as before and settled at connect.
//
// On a model whose FA and FB are not two VFOs, A is all there is to write and a
// request naming B is refused; see SetVFOFrequency.
func (k *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if vfo == radio.VFOCurrent {
		vfo = radio.VFOA
		if k.rxVFO() == radio.VFOB {
			vfo = radio.VFOB
		}
	}
	switch vfo {
	case radio.VFOA, radio.VFOB:
	default:
		return fmt.Errorf("kenwood: the %s has no %s VFO: %w",
			k.profile.Label, vfo, backend.ErrUnsupported)
	}
	if vfo == radio.VFOA && !k.profile.VFOPair.twoVFOs() {
		// The one frequency this radio's FA addresses, whatever it is called.
		return k.writeFrequency(ctx, c, "FA", keyFA, hz)
	}
	return k.SetVFOFrequency(ctx, c, vfo, hz)
}

// writeFrequency is the set-then-read-back both paths share.
func (k *Rig) writeFrequency(ctx context.Context, c backend.Conn, cmd string, key backend.Key, hz uint64) error {
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

// SetMode writes the model's mode command and, where DATA is a command of its
// own and the mode allows it, DA;.
//
// The two arrangements are genuinely different. On MD the DATA flag is a second
// command, rejected in CW and FSK, so it is sent only in LSB, USB, FM and AM;
// skipping it elsewhere is safe rather than sloppy, because the reference notes
// that "when used in any mode other than DATA mode, the P1 parameter response is
// always 0", so a rig switched from USB-DATA to CW reports Data mode off by
// itself and the next slow poll picks that up. On OM the flag is already inside
// the code just written and there is nothing to follow up with.
func (k *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	if dataMode {
		if err := k.profile.checkDataMode(m); err != nil {
			return err
		}
	}
	set, err := k.profile.modeSet(m, dataMode)
	if err != nil {
		return err
	}

	if err := send(ctx, c, set); err != nil {
		return err
	}
	if _, err := do(ctx, c, k.profile.modeReq(), k.profile.modeKey()); err != nil {
		return err
	}

	if k.profile.DataMode != DataModeCommand || !supportsDataMode(m) {
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
	w, err := k.profile.wattsFromSet(p, k.lastMode())
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
//
// On the TS-890S and TS-990S it fails outright, whatever the mode. FW exists on
// those radios but is the FM narrow/normal switch throughout, so sending it
// would not be refused — it would change the operator's FM deviation while
// remoses reported a passband it never set. Refusing a request is the only
// honest answer when the command that looks right does something else.
func (k *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	if !k.profile.FilterWidth {
		return fmt.Errorf("kenwood: the %s has no IF filter width command: FW selects FM modulation "+
			"(normal/narrow) there, and the passband is shaped with SH/SL", k.profile.Label)
	}
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

// SetFilterSlot writes FL;. Slot numbering is the API's, 1-based: on the TS-590
// generation slots 1 and 2 are IF Filter A and B and go out as FL1 and FL2, and
// on the TS-890S and TS-990S slots 1 to 4 go out as FL0 to FL3. See
// FilterStyle.param.
func (k *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	set, err := k.profile.filterSlotSet(slot)
	if err != nil {
		return err
	}
	if err := send(ctx, c, set); err != nil {
		return err
	}
	read := k.profile.filterSlotRead()
	if read == "" {
		// No form that reads without also setting; see Model.filterSlotRead.
		return nil
	}
	_, err = do(ctx, c, read, keyFL)
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
