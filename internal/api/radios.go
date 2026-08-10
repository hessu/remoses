package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// staleFactor is how many fast poll intervals may pass before a snapshot is
// reported stale. Two would trip on a single missed poll, which happens
// routinely when a command and the poller contend for the port; three means
// the poller has genuinely stopped delivering.
const staleFactor = 3

type radioDTO struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Backend   string       `json:"backend"`
	Connected bool         `json:"connected"`
	Caps      radio.Caps   `json:"caps"`
	Limits    limitsDTO    `json:"limits"`
	Lock      lockStateDTO `json:"lock"`
}

// limitsDTO publishes config.Limits in the wire spelling. It is a separate
// type because the configuration struct is written for YAML: its duration
// would marshal as "2m0s" where the API promises tx_timeout_s in whole
// seconds, and its bands are values rather than strings.
type limitsDTO struct {
	MaxPowerW   float64  `json:"max_power_w,omitempty"`
	MaxPowerPct float64  `json:"max_power_pct,omitempty"`
	TXTimeoutS  int      `json:"tx_timeout_s,omitempty"`
	Bands       []string `json:"bands,omitempty"`
}

func limitsOf(l config.Limits) limitsDTO {
	out := limitsDTO{
		MaxPowerW:   l.MaxPowerW,
		MaxPowerPct: l.MaxPowerPct,
		TXTimeoutS:  int(l.TXTimeout.D().Seconds()),
	}
	for _, b := range l.Bands {
		out.Bands = append(out.Bands, b.String())
	}
	return out
}

// stateDTO is a state snapshot plus the two fields only the API can compute.
//
// radio.State is embedded rather than copied field by field so that a new
// field in the state cache reaches clients without an edit here.
type stateDTO struct {
	radio.State
	// AgeMS is how long ago the poller last refreshed the snapshot.
	AgeMS int64 `json:"age_ms"`
	// Stale says the poller has not refreshed within the expected interval,
	// which is the client's cue to distrust the numbers rather than to guess
	// from age_ms what "too old" means for this radio.
	Stale bool `json:"stale"`
}

func (s *server) listRadios(w http.ResponseWriter, r *http.Request) {
	sessions := s.mgr.List()

	out := make([]radioDTO, 0, len(sessions))
	for _, sess := range sessions {
		// The token is resolved per radio: a browser holding two radios sends
		// one cookie for each, and the header, when present, applies to all.
		out = append(out, s.describe(sess, lockToken(r, sess.ID())))
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *server) getRadio(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, "")
	if !ok {
		return
	}
	s.writeJSON(w, http.StatusOK, s.describe(sess, lockToken(r, id)))
}

// describe builds the radio descriptor, including who holds the radio.
func (s *server) describe(sess *rig.Session, token string) radioDTO {
	id := sess.ID()
	return radioDTO{
		ID:        id,
		Name:      sess.Name(),
		Backend:   sess.Backend(),
		Connected: sess.Connected(),
		Caps:      sess.Caps(),
		Limits:    limitsOf(sess.Limits()),
		Lock:      s.lockState(id, token),
	}
}

func (s *server) getState(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, "")
	if !ok {
		return
	}
	s.writeState(w, http.StatusOK, id, sess.State())
}

// writeState answers with a snapshot, aged against the radio's own poll
// interval. The snapshot is always served, even from a disconnected radio:
// state is published through an atomic pointer precisely so that a wedged
// serial port cannot stall a reader, and connected/stale tell the client what
// it is looking at.
func (s *server) writeState(w http.ResponseWriter, status int, radioID string, st radio.State) {
	age := s.now().Sub(st.UpdatedAt)
	if age < 0 {
		age = 0
	}
	s.writeJSON(w, status, stateDTO{
		State: st,
		AgeMS: age.Milliseconds(),
		Stale: age > staleFactor*s.pollInterval(radioID),
	})
}

// pollInterval is the configured fast poll for one radio.
func (s *server) pollInterval(radioID string) time.Duration {
	if rc := s.cfg.Radio(radioID); rc != nil {
		if d := rc.Poll.Interval.D(); d > 0 {
			return d
		}
	}
	return config.DefaultPollInterval
}

// statePatchBody is the request body of PATCH /state.
//
// Every field is a pointer because "absent" and "zero" are different requests:
// {"ptt": false} means stop transmitting, while an omitted ptt means leave the
// transmitter alone.
type statePatchBody struct {
	Frequency  *uint64     `json:"frequency"`
	Mode       *radio.Mode `json:"mode"`
	DataMode   *bool       `json:"data_mode"`
	VFO        *radio.VFO  `json:"vfo"`
	PassbandHz *int        `json:"passband_hz"`
	FilterSlot *int        `json:"filter_slot"`
	PowerW     *float64    `json:"power_w"`
	PowerPct   *float64    `json:"power_pct"`
	PTT        *bool       `json:"ptt"`

	// Split moves transmit to the other VFO; DualWatch receives on both at
	// once. Both need caps.split / caps.dual_watch, and a radio without them
	// answers 422 rather than pretending.
	Split     *bool `json:"split"`
	DualWatch *bool `json:"dual_watch"`

	// VFOMode true returns the radio to VFO operation, out of memory mode. It
	// is an action rather than a state: remoses models no memory mode, so
	// there is nothing to read back and nothing false could mean.
	VFOMode *bool `json:"vfo_mode"`

	// BreakIn is off, semi or full. It decides whether CW sent over CAT is
	// audible, so a client offering a CW box should offer this beside it.
	BreakIn *radio.BreakIn `json:"break_in"`

	// PowerSwitch switches the radio itself: "on", "off", or "off_deep" for the
	// lowest standby current a radio offers where it offers a choice.
	//
	// Switching off is the one command whose success ends the conversation, so
	// it answers with the state as it was rather than a read-back.
	PowerSwitch *string `json:"power_switch"`

	// The receive front end. Each needs the matching capability — preamp_levels,
	// attenuator_db, rf_gain_control, agc_settings, ip_plus_control,
	// digi_sel_control, digi_sel_shift_control — and a radio without it answers
	// 422 with what it does have.
	//
	// Preamp is 0 for off and 1..preamp_levels; AttenuatorDB is the attenuation
	// in dB rather than a step index, 0 for switched out; RFGain and
	// DigiSelShift are percentages.
	Preamp       *int       `json:"preamp"`
	AttenuatorDB *int       `json:"attenuator_db"`
	RFGain       *float64   `json:"rf_gain"`
	AGC          *radio.AGC `json:"agc"`
	IPPlus       *bool      `json:"ip_plus"`
	DigiSel      *bool      `json:"digi_sel"`
	DigiSelShift *float64   `json:"digi_sel_shift"`

	// Tuner switches the antenna tuner in or out of line: "off" or "on" only.
	// The state can also read "tuning", but that is not something to ask for.
	Tuner *radio.Tuner `json:"tuner"`
	// TunerTune true starts a tuning cycle, which TRANSMITS for a second or
	// two. An action rather than a state, and a separate field from Tuner so
	// that a client echoing back a state it just read can never key the radio.
	TunerTune *bool `json:"tuner_tune"`
}

// toRequest converts the body into the session's patch request.
//
// It performs the one check the session cannot: the API offers power in two
// units and the wire format allows both to be sent at once, whereas
// radio.PowerSet has room for exactly one meaning. Everything else — band
// limits, capabilities, ordering — is the session's business and is left to it.
func (b statePatchBody) toRequest() (rig.PatchRequest, error) {
	if b.PowerW != nil && b.PowerPct != nil {
		return rig.PatchRequest{}, fmt.Errorf(
			"%w: give exactly one of power_w and power_pct", errUnprocessable)
	}

	req := rig.PatchRequest{
		Mode:          b.Mode,
		DataMode:      b.DataMode,
		Frequency:     b.Frequency,
		FilterSlot:    b.FilterSlot,
		FilterWidthHz: b.PassbandHz,
		PTT:           b.PTT,
		Split:         b.Split,
		DualWatch:     b.DualWatch,
		VFOMode:       b.VFOMode,
		BreakIn:       b.BreakIn,
		Tuner:         b.Tuner,
		TunerTune:     b.TunerTune,
		Preamp:        b.Preamp,
		AttenuatorDB:  b.AttenuatorDB,
		RFGain:        b.RFGain,
		AGC:           b.AGC,
		IPPlus:        b.IPPlus,
		DigiSel:       b.DigiSel,
		DigiSelShift:  b.DigiSelShift,
	}
	if b.VFO != nil {
		req.VFO = *b.VFO
	}
	switch {
	case b.PowerW != nil:
		req.Power = &radio.PowerSet{Watts: b.PowerW}
	case b.PowerPct != nil:
		req.Power = &radio.PowerSet{Pct: b.PowerPct}
	}
	return req, nil
}

// applyPowerSwitch runs a power on or off request.
//
// It refuses to share a request with anything else. A patch that switched the
// radio off and set a frequency has no sensible ordering — one of the two is
// always addressed to a radio that is not listening — and quietly choosing one
// would be worse than saying so.
func (s *server) applyPowerSwitch(ctx context.Context, sess *rig.Session, b statePatchBody) (radio.State, error) {
	if !b.onlyPowerSwitch() {
		return sess.State(), fmt.Errorf(
			"%w: power_switch cannot be combined with other fields; the radio is not "+
				"listening on one side of it", errUnprocessable)
	}
	switch *b.PowerSwitch {
	case "on":
		return sess.PowerOn(ctx)
	case "off":
		return sess.PowerOff(ctx, false)
	case "off_deep":
		return sess.PowerOff(ctx, true)
	}
	return sess.State(), fmt.Errorf(
		"%w: power_switch %q, want on, off or off_deep", errUnprocessable, *b.PowerSwitch)
}

// onlyPowerSwitch reports whether power_switch is the sole field set.
func (b statePatchBody) onlyPowerSwitch() bool {
	return b.Frequency == nil && b.Mode == nil && b.DataMode == nil && b.VFO == nil &&
		b.PassbandHz == nil && b.FilterSlot == nil && b.PowerW == nil && b.PowerPct == nil &&
		b.PTT == nil && b.Split == nil && b.DualWatch == nil && b.VFOMode == nil &&
		b.BreakIn == nil && b.Tuner == nil && b.TunerTune == nil &&
		b.Preamp == nil && b.AttenuatorDB == nil && b.RFGain == nil && b.AGC == nil &&
		b.IPPlus == nil && b.DigiSel == nil && b.DigiSelShift == nil
}

// auditAttrs names what the request asked for, so the audit line records the
// intent as well as the outcome.
func (b statePatchBody) auditAttrs() []any {
	var attrs []any
	if b.Frequency != nil {
		attrs = append(attrs, "frequency", *b.Frequency)
	}
	if b.Mode != nil {
		attrs = append(attrs, "mode", b.Mode.String())
	}
	if b.DataMode != nil {
		attrs = append(attrs, "data_mode", *b.DataMode)
	}
	if b.PassbandHz != nil {
		attrs = append(attrs, "passband_hz", *b.PassbandHz)
	}
	if b.FilterSlot != nil {
		attrs = append(attrs, "filter_slot", *b.FilterSlot)
	}
	if b.PowerW != nil {
		attrs = append(attrs, "power_w", *b.PowerW)
	}
	if b.PowerPct != nil {
		attrs = append(attrs, "power_pct", *b.PowerPct)
	}
	if b.PTT != nil {
		attrs = append(attrs, "ptt", *b.PTT)
	}
	if b.Tuner != nil {
		attrs = append(attrs, "tuner", string(*b.Tuner))
	}
	// The receive front end. Audited like any other setting, so that "why is
	// this receiver deaf" has an answer that does not depend on somebody
	// remembering what they changed.
	if b.Preamp != nil {
		attrs = append(attrs, "preamp", *b.Preamp)
	}
	if b.AttenuatorDB != nil {
		attrs = append(attrs, "attenuator_db", *b.AttenuatorDB)
	}
	if b.RFGain != nil {
		attrs = append(attrs, "rf_gain", *b.RFGain)
	}
	if b.AGC != nil {
		attrs = append(attrs, "agc", string(*b.AGC))
	}
	if b.IPPlus != nil {
		attrs = append(attrs, "ip_plus", *b.IPPlus)
	}
	if b.DigiSel != nil {
		attrs = append(attrs, "digi_sel", *b.DigiSel)
	}
	if b.DigiSelShift != nil {
		attrs = append(attrs, "digi_sel_shift", *b.DigiSelShift)
	}
	// Audited because it takes a station off the air, and because "why did the
	// radio switch off at 03:00" is a question a log should answer.
	if b.PowerSwitch != nil {
		attrs = append(attrs, "power_switch", *b.PowerSwitch)
	}
	// Audited because it transmits: an operator reading the log later should
	// find every one of those, not only the ones that went through the PTT
	// field.
	if b.TunerTune != nil {
		attrs = append(attrs, "tuner_tune", *b.TunerTune)
	}
	return attrs
}

func (s *server) patchState(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, actionPatchState)
	if !ok {
		return
	}

	var body statePatchBody
	if err := decodeJSON(w, r, &body, true); err != nil {
		s.fail(w, r, id, actionPatchState, err)
		return
	}
	// The power switch takes its own path. Everything else in a patch is a
	// setting applied to a live radio and read back afterwards; this one either
	// ends the conversation or has to happen without one, so it cannot share
	// either the ordering or the connected check.
	if body.PowerSwitch != nil {
		st, err := s.applyPowerSwitch(r.Context(), sess, body)
		if err != nil {
			s.fail(w, r, id, actionPatchState, err)
			return
		}
		s.audit(r, actionPatchState, id, http.StatusOK, nil, body.auditAttrs()...)
		s.writeState(w, http.StatusOK, id, st)
		return
	}

	req, err := body.toRequest()
	if err != nil {
		s.fail(w, r, id, actionPatchState, err)
		return
	}

	st, err := sess.ApplyPatch(r.Context(), req)
	if err != nil {
		s.fail(w, r, id, actionPatchState, err)
		return
	}

	s.audit(r, actionPatchState, id, http.StatusOK, nil, body.auditAttrs()...)
	s.writeState(w, http.StatusOK, id, st)
}
