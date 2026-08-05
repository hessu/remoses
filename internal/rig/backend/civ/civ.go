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
	return &Rig{rigAddr: rigAddr, ctrlAddr: ctrlAddr, model: model}, nil
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
		// Only the operating (selected) VFO is addressable here: commands 03/05
		// act on whatever the rig is tuned to. Main/sub and VFO A/B need the
		// 25/29 command family, which is deferred with backend.SubReceiver.
		VFOs:              []radio.VFO{radio.VFOCurrent},
		PowerWattAccurate: false,
		FilterWidth:       r.model.FilterWidth,
		FilterSlots:       filterSlots,
		SMeterScale:       sMeterScale,
		SubReceiver:       false,
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
	return r.readAll(ctx, c,
		request{KeyFrequency, r.frame(cmdReadFreq)},
		request{KeyMode, r.frame(cmdReadMode)},
		request{KeyPower, r.frame(cmdLevel, subRFPower)},
		request{KeySMeter, r.frame(cmdMeter, subSMeter)},
		request{KeyPTT, r.frame(cmdTransceiver, r.model.PTTSub)},
	)
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
		return r.readAll(ctx, c,
			request{KeyFrequency, r.frame(cmdReadFreq)},
			request{KeyMode, r.frame(cmdReadMode)},
			request{KeyPTT, r.frame(cmdTransceiver, r.model.PTTSub)},
			request{KeySMeter, r.frame(cmdMeter, subSMeter)},
		)
	case backend.PollSlow:
		reqs := []request{{KeyPower, r.frame(cmdLevel, subRFPower)}}
		// Asking a radio without 1A 03 would draw an NG every slow tick. The
		// session tolerates that (a rig that refuses is still alive), but there
		// is no reason to generate the noise.
		if r.model.FilterWidth {
			reqs = append(reqs, request{KeyFilterWidth, r.frame(cmdMisc, subFilterWidth)})
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
	mb, ok := modeByte(m)
	if !ok {
		return fmt.Errorf("civ: mode %s has no CI-V code", m)
	}
	if !r.model.supportsMode(m) {
		return fmt.Errorf("civ: %s does not have mode %s", r.model.Label, m)
	}
	if dataMode && !supportsDataMode(m) {
		return fmt.Errorf("civ: data mode is not available in %s", m)
	}
	if err := r.set(ctx, c, "mode", r.frame(cmdSetMode, mb)); err != nil {
		return err
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
		return r.set(ctx, c, "data mode off", r.frame(cmdMisc, subDataMode, 0x00, 0x00))
	}
	// Turning data on does need a filter byte, and 1A 06 offers no "leave it
	// alone" encoding. Read back the filter the mode change just selected so
	// that enabling data does not also move the filter.
	u, err := r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
	if err != nil {
		return err
	}
	slot := 1
	if u.Patch.FilterSlot != nil {
		slot = *u.Patch.FilterSlot
	}
	// DATA1 is used for "data mode on": DATA2 and DATA3 differ only in which
	// modulation input they select, which is a station wiring choice remoses
	// does not model.
	return r.set(ctx, c, "data mode on", r.frame(cmdMisc, subDataMode, 0x01, byte(slot)))
}

// SetPower sets RF output (command 14 0A).
//
// Watts are refused rather than approximated. 14 0A is a relative 0000-0255
// index and the reference gives no watt calibration for it, so any conversion
// would be a number invented by remoses about a transmitter's output.
func (r *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
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
func (r *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	var v byte
	if on {
		v = 0x01
	}
	return r.set(ctx, c, "PTT", r.frame(cmdTransceiver, r.model.PTTSub, v))
}

// SetFilterWidth sets the IF filter width (command 1A 03).
//
// The width is a mode-dependent index, so this reads the current mode first.
// That round trip is the reason Decode leaves radio.State.PassbandHz alone when
// a 1A 03 frame arrives: a decoder is pure and stateless and cannot know which
// of the rig's four width tables the byte belongs to, whereas a setter can
// afford to ask.
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

// SetFilterSlot selects FIL1..FIL3 (command 06 with an explicit filter byte).
//
// The mode has to be re-sent with the filter, so the current one is read first.
// Command 1A 06 can also carry a filter byte, but it carries the data-mode
// setting with it and is not valid in CW, which is this daemon's main use.
func (r *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	if slot < 1 || slot > filterSlots {
		return fmt.Errorf("civ: filter slot %d out of range (1-%d)", slot, filterSlots)
	}
	u, err := r.read(ctx, c, KeyMode, r.frame(cmdReadMode))
	if err != nil {
		return err
	}
	if u.Patch.Mode == nil {
		return fmt.Errorf("civ: cannot set filter slot: the rig reported an unrecognised mode")
	}
	mb, ok := modeByte(*u.Patch.Mode)
	if !ok {
		return fmt.Errorf("civ: cannot set filter slot: mode %s is not supported", *u.Patch.Mode)
	}
	return r.set(ctx, c, "filter slot", r.frame(cmdSetMode, mb, byte(slot)))
}
