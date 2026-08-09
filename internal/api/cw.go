package api

import (
	"fmt"
	"net/http"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/morse"
	"github.com/hessu/remoses/internal/radio"
)

// cwRequestBody is the request body of POST /cw. Text is in remoses canonical
// form: prosigns are "^AR", never a rig's own dialect.
type cwRequestBody struct {
	Text string `json:"text"`
	WPM  int    `json:"wpm"`
	Mode string `json:"mode"`
}

type cwAcceptedDTO struct {
	QueuedChars int `json:"queued_chars"`
	// Position is where this text starts in the queue, counted in characters
	// and 1-based, so an empty queue answers 1.
	Position      int `json:"position"`
	EstDurationMS int `json:"est_duration_ms"`
}

// sendMode maps the wire value onto the queue policy. An unrecognised value is
// refused rather than defaulted: "replace" misspelled and silently treated as
// "append" would put a second copy of a call on the air.
func (b cwRequestBody) sendMode() (cw.Mode, error) {
	switch b.Mode {
	case "", string(cw.Append):
		return cw.Append, nil
	case string(cw.Replace):
		return cw.Replace, nil
	}
	return "", fmt.Errorf("%w: mode must be %q or %q", errUnprocessable, cw.Append, cw.Replace)
}

func (s *server) getCW(w http.ResponseWriter, r *http.Request) {
	sess, _, ok := s.session(w, r, "")
	if !ok {
		return
	}
	// A radio with no keyer still answers, with an empty queue: the capability
	// flags in GET /radios are where a client learns that it cannot send, and
	// a status endpoint that failed would make polling clients special-case it.
	var status radio.CWStatus
	if snd := sess.CW(); snd != nil {
		status = snd.Status()
	}
	s.writeJSON(w, http.StatusOK, status)
}

func (s *server) sendCW(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, actionSendCW)
	if !ok {
		return
	}

	var body cwRequestBody
	if err := decodeJSON(w, r, &body, true); err != nil {
		s.fail(w, r, id, actionSendCW, err)
		return
	}
	if body.Text == "" {
		s.fail(w, r, id, actionSendCW, fmt.Errorf("%w: text is required", errUnprocessable))
		return
	}
	mode, err := body.sendMode()
	if err != nil {
		s.fail(w, r, id, actionSendCW, err)
		return
	}

	snd := sess.CW()
	if snd == nil {
		s.fail(w, r, id, actionSendCW, cw.ErrNotSupported)
		return
	}

	if body.WPM != 0 {
		// Clamped rather than refused: the speed range is a property of the
		// keyer and the rig, both of which clamp anyway, and the achieved
		// speed comes back in the status.
		if err := snd.SetSpeed(cw.ClampWPM(body.WPM)); err != nil {
			s.fail(w, r, id, actionSendCW, err)
			return
		}
	}

	// Do not queue Morse that would go nowhere. On a radio whose break-in is
	// off, the rig accepts the message, drains its buffer on schedule and
	// transmits nothing — every signal here says success and the operator hears
	// silence. This switches break-in on where cw.break_in allows it, and
	// otherwise returns a 422 naming the fix.
	if err := sess.EnsureCWWillTransmit(r.Context()); err != nil {
		s.fail(w, r, id, actionSendCW, err)
		return
	}

	// Read before enqueueing: what is already queued is what this text sits
	// behind.
	ahead := snd.Status().Queued

	queued, err := snd.Enqueue(body.Text, mode)
	if err != nil {
		s.fail(w, r, id, actionSendCW, err)
		return
	}

	s.audit(r, actionSendCW, id, http.StatusAccepted, nil,
		"chars", queued, "cw_mode", string(mode), "wpm", snd.Status().WPM)

	s.writeJSON(w, http.StatusAccepted, cwAcceptedDTO{
		QueuedChars:   queued,
		Position:      ahead + 1,
		EstDurationMS: s.estimateMS(id, body, snd),
	})
}

// estimateMS is how long this text will take to key.
//
// It counts Morse elements rather than characters, because "E" and "0" are one
// character each and differ by a factor of nineteen. Where the radio keys from
// a locally generated element stream the configured weighting applies; a rig
// keying from its own CAT buffer uses its own, which is not knowable here, so
// neutral weighting is assumed.
func (s *server) estimateMS(radioID string, body cwRequestBody, snd cw.Sender) int {
	wpm := snd.Status().WPM
	if wpm <= 0 {
		wpm = cw.ClampWPM(body.WPM)
	}

	weight := morse.NeutralWeight
	if rc := s.cfg.Radio(radioID); rc != nil && rc.CW.SerialKey != nil && rc.CW.SerialKey.Weight > 0 {
		weight = rc.CW.SerialKey.Weight
	}
	return int(morse.Estimate(body.Text, wpm, weight).Milliseconds())
}

// abortCW drops the local queue and tells the rig to stop. Both halves matter:
// up to a full buffer may already be inside the radio.
func (s *server) abortCW(w http.ResponseWriter, r *http.Request) {
	sess, id, ok := s.session(w, r, actionAbortCW)
	if !ok {
		return
	}
	// A radio with no keyer has nothing to abort, and answering 204 keeps an
	// abort — which is a safety action — free of failure modes a client would
	// have to reason about mid-transmission.
	if snd := sess.CW(); snd != nil {
		snd.Abort()
	}

	s.audit(r, actionAbortCW, id, http.StatusNoContent, nil)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
