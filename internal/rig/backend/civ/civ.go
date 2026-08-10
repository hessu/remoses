// Package civ implements the Icom CI-V protocol as a backend.Rig.
//
// Everything here is pure protocol: it builds request bytes, splits the inbound
// stream into frames, and turns a frame into a radio.Patch. There are no
// goroutines, no timers and no locks, and it never touches a transport — the
// session owns all of that and hands the backend a backend.Conn to run
// transactions with. See the backend package documentation for why.
//
// The wire format is
//
//	FE FE <to> <from> <cmd> [sub] [data...] FD
//
// with FB as the rig's OK reply and FA as its NG reply. Controller-to-rig
// frames carry to=rig address (0x98 on an IC-7610) and from=controller (0xE0);
// the rig's replies swap them. Numeric fields are packed BCD throughout.
//
// Everything encoded here is verified against the IC-7610 CI-V Reference Guide
// (Icom, May 2021 revision); comments cite command numbers from its command
// table. Where the guide is silent the comment says so and names the
// assumption, because an unverifiable guess about a transmitter is worse than
// an honest gap.
package civ

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

func init() {
	backend.Register("civ", func(r *config.Radio) (backend.Rig, error) { return New(r) })
}

// Rig is the CI-V protocol backend.
//
// It holds only the two bus addresses, which are fixed for the life of a
// connection. Keeping it free of mutable state is what lets the session call it
// from its reader goroutine (Split, Decode) and from command goroutines
// (Poll, setters) without any locking of its own.
type Rig struct {
	rigAddr  byte
	ctrlAddr byte
	model    Model
	// baud is the configured port speed, used only to size the wake-up preamble
	// for command 18 01, which the reference tabulates per baud rate. Zero on a
	// radio built without a port — a test — where the shortest preamble is
	// harmless because nothing is asleep.
	baud int

	// mode is the last mode the rig reported, and exists for one reason: the
	// 1A 03 filter width is an index whose meaning depends on which of the
	// four width tables applies, and only the mode says which. See decodeMode,
	// which stores it, and filterWidthHz, which spends it.
	//
	// It is an atomic rather than a field because Decode runs on the session's
	// reader goroutine while Poll and the setters run on the command goroutine,
	// and the backend contract forbids a lock. It is a hint, never state: the
	// session's cache holds the authoritative mode, and this is only ever used
	// to choose a column.
	mode atomic.Uint32

	// dualWatch is the last 07 C2 reading, and shapes the poll: with dual watch
	// off the second receiver is not running, so its S-meter is not worth
	// asking for and would be a stale number if it were.
	dualWatch atomic.Bool

	// tuner holds a radio.Tuner, the last 1C 01 reading. It shapes the poll:
	// while a cycle is running the state is worth watching at the fast rate,
	// and the rest of the time it belongs on the slow one.
	tuner atomic.Value

	// digiSel is the last 16 4E reading, kept only to explain a refusal.
	//
	// An IC-7610 with DIGI-SEL engaged answers NG to 16 02 01 — it will not
	// switch a preamplifier in behind the preselector — while 16 02 00 is
	// accepted and the read works throughout. Nothing in the CI-V reference
	// mentions the interlock, so without this the operator gets a bare "command
	// rejected" for a request that is refused for a reason they can act on.
	// Verified on the radio: with DIGI-SEL off, both preamplifiers select.
	digiSel atomic.Bool

	// transmitting is the last PTT reading, and shapes the poll the same way:
	// the transmit meters are asked for only while the transmitter is up,
	// because in receive all three read zero and mean nothing.
	//
	// It comes from every 1C answer and every 1C transceive push, so a
	// transmission started at the radio's own PTT switch is picked up as
	// readily as one remoses keyed itself.
	transmitting atomic.Bool

	// breakIn is the last 16 47 reading, held as a string so the CW path can
	// ask whether a message would actually be transmitted before queueing one.
	breakIn atomic.Value // radio.BreakIn

	// vfoA and vfoB accumulate what the per-VFO commands report separately. 25
	// answers a frequency, 26 a mode with its data flag and filter, and a
	// 29-prefixed 1A 03 a passband — but a Patch carries a whole VFOState, so
	// each decode has to merge into what the others last said rather than blank
	// them.
	//
	// Pointers rather than fields because Decode runs on the session's reader
	// goroutine while Poll and the setters run on the command goroutine, and
	// the contract forbids a lock. Each store publishes a fresh value; nothing
	// mutates one in place.
	vfoA atomic.Pointer[radio.VFOState]
	vfoB atomic.Pointer[radio.VFOState]
}

// filterWidthLegal reports whether 1A 03 carries a passband in the mode the rig
// is in, so the poll can skip it rather than provoke a rejection.
//
// The same question the yaesu backend asks before sending SH. FM, DV, DD and
// ATV have fixed filters and no row in the width table, and an IC-9700 in FM
// answers the read with an NG.
func (r *Rig) filterWidthLegal() bool {
	m := radio.Mode(r.mode.Load())
	if m == radio.ModeUnknown {
		// Nothing has been read yet. Not knowing the mode is not the same as
		// knowing it has no passband, and the cost of asking is now one
		// tolerated refusal rather than a step towards a reconnect.
		return true
	}
	_, ok := filterWidthHz(m, 0)
	return ok
}

// vfoSnapshot is what the backend last read for one VFO, so a decode that
// learns only part of it can fill in the rest.
func (r *Rig) vfoSnapshot(vfo radio.VFO) radio.VFOState {
	p := r.vfoA.Load()
	if vfo == radio.VFOB {
		p = r.vfoB.Load()
	}
	if p == nil {
		return radio.VFOState{}
	}
	return *p
}

func (r *Rig) storeVFO(vfo radio.VFO, s radio.VFOState) {
	if vfo == radio.VFOB {
		r.vfoB.Store(&s)
		return
	}
	r.vfoA.Store(&s)
}

// Model reports the configured radio model.
func (r *Rig) Model() Model { return r.model }

var (
	_ backend.Rig         = (*Rig)(nil)
	_ backend.MorseSender = (*Rig)(nil)
)

// New builds a CI-V backend from a radio configuration. A nil r.CIV is accepted
// and means "IC-7610 defaults".
//
// Two configuration fields are deliberately not acted on here:
//
//   - CIV.Echo. The 13-pin CI-V bus echoes our own frames back at us, and so
//     does a USB connection when the operator has "CI-V USB Echo Back" (Set
//     mode, reachable as command 1A 05 0116) switched on. A wrong flag would
//     mean feeding our own request bytes back into the state cache, so echo
//     suppression is unconditional: Decode drops any frame addressed *to* the
//     rig regardless of what the configuration claims. The flag is kept in the
//     config schema because it documents the wiring for the operator.
//   - CIV.Transceive. See Init.
func New(r *config.Radio) (*Rig, error) {
	name := ""
	if r != nil && r.CIV != nil {
		name = r.CIV.Model
	}
	model, err := LookupModel(name)
	if err != nil {
		return nil, err
	}

	// The model's factory address is the default; an explicit rig_address wins,
	// because the address is menu-configurable on every one of these radios.
	rigAddr, ctrlAddr := model.Address, byte(DefaultControllerAddress)
	if r != nil && r.CIV != nil {
		if r.CIV.RigAddress != 0 {
			if !validAddress(r.CIV.RigAddress) {
				return nil, fmt.Errorf("civ: rig_address 0x%02X out of range (0x01-0xEF)", r.CIV.RigAddress)
			}
			rigAddr = byte(r.CIV.RigAddress)
		}
		if r.CIV.ControllerAddress != 0 {
			if !validAddress(r.CIV.ControllerAddress) {
				return nil, fmt.Errorf("civ: controller_address 0x%02X out of range (0x01-0xEF)", r.CIV.ControllerAddress)
			}
			ctrlAddr = byte(r.CIV.ControllerAddress)
		}
	}
	if rigAddr == 0 {
		// Only the generic profile has no factory address to fall back on.
		// Guessing one would silently address the wrong radio on a shared bus.
		return nil, fmt.Errorf("civ: model %q has no default bus address; set civ.rig_address, "+
			"or name a specific model (%s)", model.Name, strings.Join(ModelNames(), ", "))
	}
	if rigAddr == ctrlAddr {
		return nil, fmt.Errorf("civ: rig_address and controller_address are both 0x%02X; "+
			"echo suppression cannot tell our frames from the rig's", rigAddr)
	}
	// The port's baud rate, kept for one purpose: sizing the wake-up preamble
	// that command 18 01 needs. See power.go.
	baud := 0
	if r != nil {
		baud = r.Port.Baud
	}
	return &Rig{rigAddr: rigAddr, ctrlAddr: ctrlAddr, model: model, baud: baud}, nil
}

// validAddress reports whether a is usable as a CI-V bus address. 0x00 is the
// broadcast address the rig uses for transceive frames, and 0xFA..0xFF are the
// reserved OK/NG/end/preamble codes, so neither may name a station.
func validAddress(a int) bool { return a >= 0x01 && a <= 0xEF }

// Caps describes an IC-7610 class radio.
//
// Power is reported as not watt-accurate: command 14 0A is a relative 0000-0255
// index with no documented watt meaning, so radio.Power.Watts stays nil and
// MaxPowerW is left unset rather than filled in with the model's nameplate
// rating, which would invite clients to treat the index as watts.
func (r *Rig) Caps() radio.Caps {
	// A radio without command 17 has no CAT CW at all, so it advertises neither
	// a method nor a charset: a client that offered a CW box for it would be
	// promising something the hardware cannot do.
	cwMethod, charset := radio.CWNone, ""
	if r.model.CWBuffer {
		cwMethod, charset = radio.CWViaCAT, Charset
	}
	return radio.Caps{
		// Fresh slice per call: Caps is published through the API and a shared
		// backing array would be one mutation away from a data race. The set
		// comes from the model, so a client is not offered PSK on an IC-9700 or
		// DV on an IC-7610.
		Modes: append([]radio.Mode(nil), r.model.Modes...),
		// What a client may address. Commands 03/05 act on whatever the rig is
		// tuned to, so the operating VFO is always available; A and B join it
		// only on a radio with the 25/26 family, which can name a VFO instead
		// of operating on whichever is selected.
		VFOs: r.addressableVFOs(),
		// Both false on the IC-706 family, which has no transmitter command and
		// no power level. A client reads these rather than offering a PTT
		// button that cannot key and a slider that moves nothing.
		PTTControl:        r.model.PTT,
		PowerControl:      r.model.Power,
		PowerWattAccurate: false,
		FilterWidth:       r.model.FilterWidth,
		FilterSlots:       r.model.FilterSlots,
		// Zero where there is no meter to read, which a client can tell from a
		// meter that reads zero.
		SMeterScale: r.sMeterScale(),
		// 15 11, 15 12 and 15 13 arrive together on the radios that have them
		// and are absent together on the ones that do not.
		PowerMeter: r.model.TXMeters,
		SWRMeter:   r.model.TXMeters,
		ALCMeter:   r.model.TXMeters,
		// 1C 01 does both halves on the radios that have it: 00/01 switch the
		// tuner in and out, 02 starts a cycle.
		TunerControl: r.model.Tuner,
		TunerTune:    r.model.Tuner,
		PowerSwitch:  r.model.PowerSwitch,

		// The receive front end, straight from the model table. The attenuator
		// list gets a fresh slice for the same reason the mode list does.
		PreampLevels:        r.model.Preamp,
		AttenuatorDB:        append([]int(nil), r.model.Attenuator...),
		RFGainControl:       r.model.RFGain,
		AGCSettings:         agcSettings(r.model.AGC),
		IPPlusControl:       r.model.IPPlus,
		DigiSelControl:      r.model.DigiSel,
		DigiSelShiftControl: r.model.DigiSelShift,

		// SubReceiver is whether the radio *has* a second receiver;
		// SubReceiverReadable is whether remoses can report it. They differ on
		// the IC-9700, which has one and offers no command that addresses it —
		// only "select the sub band", which would move the operator's focus.
		SubReceiver:         r.model.SubReceiver,
		SubReceiverReadable: r.model.DualWatch,
		Split:               r.model.Split,
		DualWatch:           r.model.DualWatch,
		// Command 26 carries mode, data mode and filter per VFO, so on those
		// radios all three are per-VFO rather than properties of the set.
		PerVFOMode:     r.model.DualVFO,
		VFOAddressing:  r.vfoAddressing(),
		BreakInControl: r.model.BreakIn != BreakInNone,
		// Capability, not configuration: a radio with command 17 has a CAT CW
		// buffer whether or not this station is configured to use it, and one
		// without it — the IC-718 — cannot send Morse over CAT at all, however
		// the station is configured. Choosing between cat and serial_key is the
		// session's job; reporting which are possible is this one's.
		CWMethod:  cwMethod,
		CWCharset: charset,
		CWMinWPM:  minWPM,
		CWMaxWPM:  r.model.MaxWPM,
	}
}

// metersAreLive reports whether the transmit meters are worth reading.
//
// The rig's own PTT, and nothing else. Reading them during a tuning cycle as
// well was tried on an IC-7610 and abandoned: that radio reports PTT false for
// the whole cycle and answers zero to all three meters, so the only thing the
// extra traffic bought was a zero SWR published as a perfect 1.0:1 match while
// the tuner was in fact failing to find one. A TS-590S, which does report PTT
// during a cycle, gets real readings through this path already.
func (r *Rig) metersAreLive() bool {
	return r.transmitting.Load()
}

// sMeterScale is the full-scale meter reading, or 0 on a radio with no readable
// meter at all — the IC-706 and IC-706MKII, whose command sets contain no 15.
func (r *Rig) sMeterScale() int {
	if !r.model.SMeter {
		return 0
	}
	return sMeterScale
}

// Init reads the full state once, which doubles as the liveness check for a
// freshly opened port: if the rig is off or the bus address is wrong, the first
// read times out and the session sees the failure immediately instead of
// publishing a plausible-looking empty state.
//
// It deliberately does NOT enable Transceive. On the IC-7610 CI-V Transceive is
// a Set-mode menu item (SET > Connectors > CI-V > CI-V Transceive), and the only
// way to reach it over the bus is the generic menu-writing command 1A 05 0112,
// which rewrites the operator's persistent configuration. That setting survives
// disconnection and rig power-off, so remoses would be permanently reconfiguring
// somebody's radio as a side effect of connecting to it. remoses therefore polls
// (see Poll) and treats any transceive broadcasts the operator has chosen to
// enable as a bonus: Decode understands commands 00 and 01 and folds them into
// state exactly like a solicited reply.
func (r *Rig) Init(ctx context.Context, c backend.Conn) error {
	r.checkIdentity(ctx, c)
	// Frequency and mode are the two every radio here answers, and between them
	// they prove the link. The rest are per model: on an IC-706 there is no
	// transmitter command, no power level and — before the MKIIG — no meter, so
	// asking would draw three rejections at every connect and publish nothing.
	reqs := []request{
		{KeyFrequency, r.frame(cmdReadFreq)},
		{KeyMode, r.frame(cmdReadMode)},
	}
	if r.model.Power {
		reqs = append(reqs, request{KeyPower, r.frame(cmdLevel, subRFPower)})
	}
	if r.model.SMeter {
		reqs = append(reqs, request{KeySMeter, r.frame(cmdMeter, subSMeter)})
	}
	if r.model.PTT {
		reqs = append(reqs, request{KeyPTT, r.frame(cmdTransceiver, r.model.PTTSub)})
	}
	if err := r.readAll(ctx, c, reqs...); err != nil {
		return err
	}
	if !r.model.DualVFO {
		return nil
	}
	// Both VFOs at connect, so the first client to look sees the second one
	// filled in rather than zeroes — and so that the dual-watch flag is settled
	// before the first fast poll decides whether to ask for the sub meter.
	return r.ReadVFOs(ctx, c)
}

// checkIdentity asks the radio who it is (command 19 00) and warns if the
// answer disagrees with the configuration.
//
// It is a CROSS-CHECK, never model detection, because 19 00 does not report a
// model. It reports the rig's CI-V bus address, which is a poor proxy for one:
//
//   - the address is menu-configurable on every supported radio — that is the
//     entire reason config.CIV.RigAddress exists — so an operator who changed it
//     breaks any identification based on it;
//   - two different models set to the same address are indistinguishable;
//   - a model remoses has no profile for still answers with something.
//
// So the configured model stays authoritative and this only catches the case
// worth catching: a configuration naming one radio pointed at another. Failure
// is deliberately silent — an older Icom that does not implement 19 00 must
// still connect — and the address is logged either way, which is what an
// operator needs when the answer is "not what you configured".
func (r *Rig) checkIdentity(ctx context.Context, c backend.Conn) {
	u, err := c.Do(ctx, r.frame(cmdReadID, subReadID), KeyID, KeyAck)
	if err != nil || !u.OK || u.Key != KeyID {
		return
	}
	// FE FE to from 19 00 <id> FD — the byte after the sub-command.
	if len(u.Raw) < 7 {
		return
	}
	reported := u.Raw[len(u.Raw)-2]
	if reported == r.rigAddr {
		return
	}

	detail := ""
	if m, ok := ModelForAddress(reported); ok {
		detail = fmt.Sprintf(" (the factory address of the %s)", m.Label)
	}
	slog.Warn("civ: the radio reports a different bus address than configured",
		"configured_model", r.model.Name,
		"configured_address", fmt.Sprintf("0x%02X", r.rigAddr),
		"reported_address", fmt.Sprintf("0x%02X%s", reported, detail),
		"note", "civ.model and civ.rig_address may not match this radio")
}

// Poll refreshes one tier of state.
//
// The fast tier is what moves under the operator's hand, and is four round
// trips because CI-V has no bulk status read: unlike Kenwood's IF;, every value
// costs its own transaction. The slow tier holds the settings that only move
// when somebody changes them.
func (r *Rig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	switch tier {
	case backend.PollFast:
		reqs := []request{
			{KeyFrequency, r.frame(cmdReadFreq)},
			{KeyMode, r.frame(cmdReadMode)},
		}
		// Both per model: an IC-706 has no transmitter command to ask and no
		// meter until the MKIIG, so polling them twice a second would be a
		// steady stream of rejections for two values that can never arrive.
		if r.model.PTT {
			reqs = append(reqs, request{KeyPTT, r.frame(cmdTransceiver, r.model.PTTSub)})
		}
		if r.model.SMeter {
			reqs = append(reqs, request{KeySMeter, r.frame(cmdMeter, subSMeter)})
		}
		// The transmit meters, and only while the transmitter is up. In receive
		// they read zero and mean nothing — there is no forward power to
		// measure and no ALC acting — so asking would spend three transactions
		// a tick to publish three zeroes that a client cannot tell from a real
		// reading. Which is also why State drops them when PTT falls.
		//
		// This uses the PTT the last poll decoded rather than a fresh read: one
		// tick of lag at the start of a transmission costs a single sample, and
		// the alternative is serialising every fast poll behind a PTT read.
		if r.model.TXMeters && r.metersAreLive() {
			reqs = append(reqs,
				request{KeyPOMeter, r.frame(cmdMeter, subPOMeter)},
				request{KeySWRMeter, r.frame(cmdMeter, subSWRMeter)},
				request{KeyALCMeter, r.frame(cmdMeter, subALCMeter)})
		}
		// A tuning cycle lasts a second or two, so while one is running the
		// tuner belongs on this tier: on the slow one a whole cycle could begin
		// and end between two reads, and a client would see nothing happen.
		// Once it finishes this drops back to the slow tier by itself.
		if r.model.Tuner && r.Tuner() == radio.TunerTuning {
			reqs = append(reqs, request{KeyTuner, r.frame(cmdTransceiver, subTuner)})
		}
		// The second receiver's meter belongs in the fast tier for the same
		// reason the first one does — it is a signal level, and a client draws
		// it as a live bar — but only while dual watch is actually running.
		// With it off that receiver is not listening to anything, and a reading
		// from it would sit in the cache looking current.
		if r.model.DualVFO && r.dualWatch.Load() {
			reqs = append(reqs, request{KeySubSMeter,
				r.frame(cmdBand, bandSub, cmdMeter, subSMeter)})
		}
		return r.readAll(ctx, c, reqs...)
	case backend.PollSlow:
		var reqs []request
		if r.model.Tuner {
			reqs = append(reqs, request{KeyTuner, r.frame(cmdTransceiver, subTuner)})
		}
		if r.model.Power {
			reqs = append(reqs, request{KeyPower, r.frame(cmdLevel, subRFPower)})
		}
		// Asking a radio without 1A 03 would draw an NG every slow tick. The
		// session tolerates that (a rig that refuses is still alive), but there
		// is no reason to generate the noise.
		//
		// The mode matters as well as the model. FM, DV, DD and ATV have no
		// adjustable passband — filterWidthHz has no table for them — and an
		// IC-9700 in FM answers the read with an NG rather than a value. Asking
		// anyway would draw a rejection every slow tick for a setting that does
		// not exist in that mode.
		if r.model.FilterWidth && r.filterWidthLegal() {
			reqs = append(reqs, request{KeyFilterWidth, r.frame(cmdMisc, subFilterWidth)})
		}
		// Both VFOs, split and dual watch. The slow tier because they move only
		// when somebody changes them — and because on a dual-VFO radio this is
		// four extra transactions, which has no business on a 500 ms tick.
		// The sub receiver's meter is the exception and rides the fast tier;
		// see PollFast.
		if r.model.DualVFO {
			reqs = append(reqs,
				request{KeyVFOFreq, r.frame(cmdBandFreq, bandMain)},
				request{KeyVFOFreq, r.frame(cmdBandFreq, bandSub)},
				request{KeyVFOMode, r.frame(cmdBandMode, bandMain)},
				request{KeyVFOMode, r.frame(cmdBandMode, bandSub)})
		}
		if r.model.Split {
			reqs = append(reqs, request{KeySplit, r.frame(cmdSplit)})
		}
		if r.model.DualWatch {
			reqs = append(reqs, request{KeyDualWatch, r.frame(cmdVFO, subDualWatch)})
		}
		// Break-in belongs in the slow tier — it moves only when somebody
		// changes it — but it has to be read at all, because the CW path
		// consults it before queueing and an unknown reading is not something
		// to guess at.
		if r.model.BreakIn != BreakInNone {
			reqs = append(reqs, request{KeyBreakIn, r.frame(cmdFunc, subBreakIn)})
		}
		// Data mode has to be read, not merely written. Nothing else reports it:
		// there is no data-mode flag in any other answer, and the rig does not
		// broadcast 1A 06, so without this state.data_mode never moves off its
		// zero value and a radio sitting in USB-D is published as plain USB.
		// Confirmed on an IC-7610, which acknowledged 1A 06 01 02 with FB while
		// remoses went on reporting data mode off.
		//
		// Guarded by the model exactly as the setter and the decoder are: 1A 06
		// is RIT on the IC-910H, and polling it there would read an RIT setting
		// and publish it as a data-mode change every slow tick.
		if r.model.DataMode {
			reqs = append(reqs, request{KeyDataMode, r.dataModeRead()})
		}
		// The receive front end. All of it is slow-tier: these are settings an
		// operator changes by hand, and each is one more transaction per tick on
		// a bus that also has to carry the fast poll.
		if r.model.Preamp > 0 {
			reqs = append(reqs, request{KeyPreamp, r.frame(cmdFunc, subPreamp)})
		}
		if len(r.model.Attenuator) > 0 {
			reqs = append(reqs, request{KeyAttenuator, r.frame(cmdAttenuator)})
		}
		if r.model.RFGain {
			reqs = append(reqs, request{KeyRFGain, r.frame(cmdLevel, subRFGain)})
		}
		if len(r.model.AGC) > 0 {
			reqs = append(reqs, request{KeyAGC, r.frame(cmdFunc, subAGC)})
		}
		if r.model.IPPlus {
			reqs = append(reqs, request{KeyIPPlus, r.frame(cmdFunc, subIPPlus)})
		}
		if r.model.DigiSel {
			reqs = append(reqs, request{KeyDigiSel, r.frame(cmdFunc, subDigiSel)})
		}
		if r.model.DigiSelShift {
			reqs = append(reqs, request{KeyDigiSelShift, r.frame(cmdLevel, subDigiSelShift)})
		}
		return r.readAll(ctx, c, reqs...)
	default:
		return fmt.Errorf("civ: unknown poll tier %d", tier)
	}
}

// request pairs a built frame with the key its reply will carry.
type request struct {
	key backend.Key
	req []byte
}

// readAll runs the requests in order and stops at the first failure. The
// session applies the patches from the replies itself, so nothing is returned
// but the error.
func (r *Rig) readAll(ctx context.Context, c backend.Conn, reqs ...request) error {
	for _, q := range reqs {
		if _, err := r.read(ctx, c, q.key, q.req); err != nil {
			return err
		}
	}
	return nil
}

// read runs one read transaction. KeyAck is included in the wanted keys so that
// a rig which rejects the command answers the caller straight away instead of
// leaving it to time out: a NG frame decodes to KeyAck with OK false.
func (r *Rig) read(ctx context.Context, c backend.Conn, key backend.Key, req []byte) (backend.Update, error) {
	u, err := c.Do(ctx, req, key, KeyAck)
	if err != nil {
		return u, fmt.Errorf("civ: read %s: %w", key, err)
	}
	if !u.OK {
		return u, fmt.Errorf("civ: rig rejected read %s", key)
	}
	if u.Key != key {
		return u, fmt.Errorf("civ: read %s: unexpected reply %s", key, u.Key)
	}
	return u, nil
}

// set runs one write transaction. The rig answers every set with FB (OK) or FA
// (NG), both of which decode to KeyAck, so one wanted key covers both outcomes.
func (r *Rig) set(ctx context.Context, c backend.Conn, what string, req []byte) error {
	u, err := c.Do(ctx, req, KeyAck)
	if err != nil {
		return fmt.Errorf("civ: set %s: %w", what, err)
	}
	if !u.OK {
		return fmt.Errorf("civ: rig rejected %s", what)
	}
	return nil
}

// SetFrequency sets the operating frequency (command 05).
//
// Only radio.VFOCurrent is accepted: 05 addresses whichever VFO the rig is on,
// and pretending that VFOA means "current" would silently tune the wrong VFO on
// a rig that happens to be on B.
func (r *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if vfo != radio.VFOCurrent {
		return fmt.Errorf("civ: VFO %s is not addressable; this backend sets the operating VFO only", vfo)
	}
	// The six-byte field exists only on radios that have a 10 GHz band, and
	// only above it: sending it to a radio expecting five bytes would be
	// rejected, so the width follows the target frequency.
	wide := r.model.WideFrequency && hz >= wideThresholdHz
	f, err := encodeFrequency(hz, wide)
	if err != nil {
		return err
	}
	return r.set(ctx, c, "frequency", r.frame(cmdSetFreq, f...))
}

// SetMode sets the operating mode (command 06) and then the data-mode setting
// (command 1A 06), which the IC-7610 keeps orthogonal to the mode itself.
//
// The filter byte is omitted from command 06 on purpose: the reference states
// that when it is skipped the rig selects the default filter for the mode being
// entered, which is what an operator expects from a mode change. Use
// SetFilterSlot to pick a different one afterwards.
func (r *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	if !r.model.supportsMode(m) {
		return fmt.Errorf("civ: %s does not have mode %s", r.model.Label, m)
	}
	mb, ok := r.model.modeByte(m)
	if !ok {
		return fmt.Errorf("civ: mode %s has no CI-V code on %s", m, r.model.Label)
	}
	if dataMode && !r.model.DataMode {
		return fmt.Errorf("civ: %s has no data mode", r.model.Label)
	}
	if dataMode && !supportsDataMode(m) {
		return fmt.Errorf("civ: data mode is not available in %s", m)
	}
	// Read the mode before writing one, because re-sending the mode the rig is
	// already in is not free. Command 06 with no filter byte makes the radio
	// fall back to that mode's *default* filter, so a request that changes only
	// the data flag — which the API turns into SetMode(current mode, flag),
	// since data mode is orthogonal at that layer — would silently move the
	// operator's filter selection. Confirmed on an IC-7610: a data-mode-only
	// PATCH on USB/FIL1 came back on FIL2.
	//
	// On a genuine mode change the filter is deliberately left to the rig. Its
	// default for the new mode is very likely what the operator wants, and
	// forcing the old slot could select a passband that mode has no use for.
	cur, err := r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
	if err != nil {
		return err
	}
	changed := cur.Patch.Mode == nil || *cur.Patch.Mode != m
	if changed {
		if err := r.set(ctx, c, "mode", r.frame(cmdSetMode, mb)); err != nil {
			return err
		}
	}
	if !r.model.DataMode {
		// Sub-command 06 of 1A is not data mode everywhere: on the IC-910H it
		// is RIT on/off, so sending "data mode off" after every mode change
		// would quietly switch RIT off. Radios without a data mode never see
		// the command at all.
		return nil
	}
	if !supportsDataMode(m) {
		// 1A 06 covers the SSB/AM/FM data modes only, so leave it alone in CW,
		// RTTY and PSK rather than risk an NG on a command that has nothing to
		// say about them. Which modes carry a data setting is not stated in the
		// CI-V guide; SSB/AM/FM is taken from the operating manual's DATA menu.
		return nil
	}
	if !dataMode {
		// The reference is explicit that when the data byte is 00 the filter
		// byte must be 00 too, so switching data off needs no filter lookup.
		return r.set(ctx, c, "data mode off", r.dataModeFrame(0x00, 0))
	}
	if !r.model.DataModeFilter {
		// No filter byte on this radio's form of the command, so there is
		// nothing to preserve and no reason to read the mode back first.
		return r.set(ctx, c, "data mode on", r.dataModeFrame(0x01, 0))
	}
	// Turning data on does need a filter byte, and 1A 06 offers no "leave it
	// alone" encoding, so the current filter has to be carried into it or
	// enabling data would move the filter.
	//
	// Which read that comes from depends on whether a mode went out: after a
	// real mode change the rig has picked the new mode's default and has to be
	// asked again, but when nothing was sent the read above is still current
	// and asking twice would only widen the window for the operator to turn the
	// filter knob between them.
	state := cur
	if changed {
		state, err = r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
		if err != nil {
			return err
		}
	}
	slot := 1
	if state.Patch.FilterSlot != nil {
		slot = *state.Patch.FilterSlot
	}
	// DATA1 is used for "data mode on": DATA2 and DATA3 differ only in which
	// modulation input they select, which is a station wiring choice remoses
	// does not model.
	return r.set(ctx, c, "data mode on", r.dataModeFrame(0x01, slot))
}

// dataModeFrame builds a data-mode set for this model.
//
// The sub-command is per model — 1A 06 across the modern family, 1A 04 on the
// IC-703 — and so is whether a filter byte follows the flag. Sending the modern
// two-byte form to a radio whose command takes one would hand its parser a
// parameter it is not expecting.
func (r *Rig) dataModeFrame(on byte, slot int) []byte {
	if !r.model.DataModeFilter {
		return r.frame(cmdMisc, r.model.DataModeSub, on)
	}
	return r.frame(cmdMisc, r.model.DataModeSub, on, byte(slot))
}

// dataModeRead is the read form of the same command.
func (r *Rig) dataModeRead() []byte {
	return r.frame(cmdMisc, r.model.DataModeSub)
}

// SetPower sets RF output (command 14 0A).
//
// Watts are refused rather than approximated. 14 0A is a relative 0000-0255
// index and the reference gives no watt calibration for it, so any conversion
// would be a number invented by remoses about a transmitter's output.
func (r *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	if !r.model.Power {
		return fmt.Errorf("civ: the %s has no RF power command; its output is set on "+
			"the radio: %w", r.model.Label, backend.ErrUnsupported)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Watts != nil {
		return fmt.Errorf("civ: this radio has no watt-accurate power scale; set power as a percentage")
	}
	pct := *p.Pct
	if pct < 0 || pct > 100 {
		return fmt.Errorf("civ: power %.1f%% out of range (0-100)", pct)
	}
	n := int(pct/100*levelMax + 0.5)
	v := encodeBCD2(min(max(n, 0), levelMax))
	return r.set(ctx, c, "power", r.frame(cmdLevel, subRFPower, v[0], v[1]))
}

// SetPTT keys or unkeys the transmitter (command 1C 00).
//
// Refused outright on a radio with no such command. The IC-706 family has none
// at any sub-command, so there is nothing to send and nothing that would report
// the result — an operator there keys with a footswitch, the microphone, or a
// control line driven by cw.serial_key.
func (r *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.PTT {
		return fmt.Errorf("civ: the %s has no transmitter command; key it with a "+
			"footswitch, the microphone or a serial control line: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	var v byte
	if on {
		v = 0x01
	}
	return r.set(ctx, c, "PTT", r.frame(cmdTransceiver, r.model.PTTSub, v))
}

// SetFilterWidth sets the IF filter width (command 1A 03).
//
// The width is a mode-dependent index, so this reads the current mode first
// rather than trusting the hint Decode keeps. A setter can afford the round
// trip, and this is the direction where being one fast-poll stale would put a
// wrong filter into the operator's radio instead of merely reporting one.
//
// Requests that fall between steps snap down, matching how the Kenwood backend
// treats an off-step FW; and erring towards the narrower filter.
func (r *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	if !r.model.FilterWidth {
		return fmt.Errorf("civ: %s has no IF filter width command", r.model.Label)
	}
	u, err := r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
	if err != nil {
		return err
	}
	if u.Patch.Mode == nil {
		return fmt.Errorf("civ: cannot set filter width: the rig reported an unrecognised mode")
	}
	idx, err := filterWidthIndex(*u.Patch.Mode, hz)
	if err != nil {
		return err
	}
	// Single-byte BCD index. The reference tabulates the value range (0-49) but
	// not the field width; one byte matches every other small 1A sub-command.
	return r.set(ctx, c, "filter width", r.frame(cmdMisc, subFilterWidth, bcdByte(int(idx))))
}

// SetFilterSlot selects FIL1..FIL3, by whichever of the two commands can do it
// without changing something else.
//
// Neither command is a filter selector. Command 06 sets the *mode* and takes a
// filter byte with it; command 1A 06 sets *data mode* and takes a filter byte
// with it. Which one is safe therefore depends on what the rig is doing:
//
//   - In a data mode, 06 is wrong. Sending it resets the 1A 06 data flag, so
//     picking a filter would quietly drop the operator out of USB-D. Confirmed
//     on an IC-7610. 1A 06 with the data byte still 01 moves the filter and
//     leaves the mode alone.
//   - Otherwise 1A 06 is wrong, or unavailable: it is not valid in CW, RTTY or
//     PSK, and on an IC-910H sub-command 06 is RIT rather than data mode. So
//     the mode is read and re-sent with the filter, which changes nothing
//     because it is the mode the rig is already in.
//
// Between them every case is covered without a command that means something
// else, which is what the earlier single-command version could not manage.
func (r *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	if n := r.model.FilterSlots; n == 0 {
		return fmt.Errorf("civ: %s has no IF filter selection", r.model.Label)
	} else if slot < 1 || slot > n {
		return fmt.Errorf("civ: filter slot %d out of range (1-%d)", slot, n)
	}
	u, err := r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
	if err != nil {
		return err
	}
	if u.Patch.Mode == nil {
		return fmt.Errorf("civ: cannot set filter slot: the rig reported an unrecognised mode")
	}
	mode := *u.Patch.Mode
	mb, ok := r.model.modeByte(mode)
	if !ok {
		return fmt.Errorf("civ: cannot set filter slot: mode %s is not supported", mode)
	}

	// Only ask about data mode where the question means anything. On a radio
	// without one the command is absent or is something else entirely, and in
	// CW, RTTY and PSK it carries no data setting to preserve.
	if r.model.DataMode && supportsDataMode(mode) {
		d, err := r.read(ctx, c, KeyDataMode, r.dataModeRead())
		if err != nil {
			return err
		}
		if d.Patch.DataMode != nil && *d.Patch.DataMode {
			return r.set(ctx, c, "filter slot", r.dataModeFrame(0x01, slot))
		}
	}
	return r.set(ctx, c, "filter slot", r.frame(cmdSetMode, mb, r.model.filterByte(slot)))
}
