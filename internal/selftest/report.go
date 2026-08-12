// Package selftest exercises everything a configured radio says it can do and
// writes down what happened, in a form somebody else can diagnose from.
//
// It exists because of an arithmetic problem this project cannot solve on its
// own: nearly forty radio profiles, transcribed from manufacturers' references,
// and only a handful that anybody here can plug in. Every radio that has been
// connected found bugs — values written but never read back, commands that
// silently changed a neighbouring setting, capabilities that described a
// different radio — and none of those were visible from the documentation. The
// remaining profiles are not more correct than those were. They are less
// tested.
//
// So the run is designed to be handed to a stranger: "point this at your rig
// and send me the file". That shapes everything here.
//
//   - It is driven by Caps rather than by a per-model script. Whatever the
//     radio advertises gets exercised; whatever it denies gets a request anyway,
//     to check the denial is real. One engine, thirty-seven radios, and a new
//     profile is covered the day it is written.
//   - It transmits only when told to, and never by accident. See Options.
//   - It puts the radio back the way it found it, including when it fails or is
//     interrupted.
//   - A failure has to be unambiguous to be called one. A log full of maybes
//     wastes the time of the person who ran it and the person reading it, so
//     anything that merely *might* be wrong is recorded as information and left
//     for a human — or an agent — to judge with the wire trace in front of them.
package selftest

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/hessu/remoses/internal/radio"
)

// FormatVersion is the schema of the JSON Lines output. A reader should check
// it before trusting field names; it is bumped when something is removed or
// changes meaning, not when a field is added.
const FormatVersion = 1

// Verdict is what a step proved, if anything.
type Verdict string

const (
	// Pass: the radio did what it was asked and the read-back agrees.
	Pass Verdict = "pass"

	// Fail is deliberately narrow — see the package doc. It is used only where
	// the evidence is unambiguous: a write the radio accepted that changed
	// nothing, a capability advertised and then refused, or a capability denied
	// and then accepted anyway.
	Fail Verdict = "fail"

	// Refused: the request was rejected, and that was the expected answer.
	Refused Verdict = "refused"

	// Skipped: the radio has no such control, or the run was not authorised to
	// try it. Recorded rather than omitted, so a reader can tell "not tested"
	// from "not present".
	Skipped Verdict = "skipped"

	// Info: it happened, it is written down, and nothing here claims it was
	// right or wrong. Most of a transmit test is this: nobody but the operator
	// can say whether the CW was audible.
	Info Verdict = "info"
)

// WireFrame is one CAT frame, as the session's trace saw it.
//
// This is the field that makes a submitted log worth having. Decoded state
// hides exactly the mistakes worth finding — a mode table read off the wrong
// column, a frequency field a digit too short, a command the radio does not
// actually implement — and all of them are obvious in the hex.
type WireFrame struct {
	Dir  string `json:"dir"`            // to-rig | from-rig
	Hex  string `json:"hex"`            // always
	Text string `json:"text,omitempty"` // the ASCII dialects only
	Key  string `json:"key,omitempty"`
	OK   *bool  `json:"ok,omitempty"`
}

// Step is one thing the run did to the radio.
type Step struct {
	Record  string  `json:"record"` // always "step"
	Seq     int     `json:"seq"`
	Group   string  `json:"group"`
	Name    string  `json:"name"`
	Verdict Verdict `json:"verdict"`

	// Request is what was asked for, rendered the way a client would send it,
	// so that a reader can reproduce the step with curl.
	Request string `json:"request,omitempty"`
	// Want and Got are filled in where there was a definite expectation.
	Want string `json:"want,omitempty"`
	Got  string `json:"got,omitempty"`
	// Note carries the reasoning when a verdict needs any, and it is written
	// for the person diagnosing rather than for the person running.
	Note string `json:"note,omitempty"`
	Err  string `json:"err,omitempty"`

	Wire       []WireFrame `json:"wire,omitempty"`
	DurationMS int64       `json:"duration_ms"`
}

// Header is the first line of the file: everything needed to interpret the rest
// without asking the person who ran it a single follow-up question.
type Header struct {
	Record        string      `json:"record"` // always "header"
	FormatVersion int         `json:"format_version"`
	Remoses       string      `json:"remoses_version"`
	Go            string      `json:"go_version"`
	OS            string      `json:"os"`
	StartedAt     time.Time   `json:"started_at"`
	RadioID       string      `json:"radio_id"`
	RadioName     string      `json:"radio_name"`
	Backend       string      `json:"backend"`
	Model         string      `json:"model"`
	Transport     string      `json:"transport"`
	Caps          radio.Caps  `json:"caps"`
	InitialState  radio.State `json:"initial_state"`

	// Authorised says what the operator allowed, so that a reader never has to
	// wonder whether a missing transmit section means "it failed" or "nobody
	// said it could".
	Authorised Authorised `json:"authorised"`
}

// Authorised records the run's permissions, verbatim from the command line.
type Authorised struct {
	Transmit      bool   `json:"transmit"`
	TXFrequency   uint64 `json:"tx_frequency,omitempty"`
	TXPowerPct    int    `json:"tx_power_pct,omitempty"`
	PowerSwitch   bool   `json:"power_switch"`
	CWText        string `json:"cw_text,omitempty"`
	OperatorNotes string `json:"operator_notes,omitempty"`
}

// Summary is the last line, and the first thing a reader should look at.
type Summary struct {
	Record     string          `json:"record"` // always "summary"
	FinishedAt time.Time       `json:"finished_at"`
	ElapsedMS  int64           `json:"elapsed_ms"`
	Counts     map[Verdict]int `json:"counts"`
	// Failures repeats the failing steps by name, so that the headline is
	// legible without parsing the whole file.
	Failures []string `json:"failures,omitempty"`
	// Restored says whether the radio was put back as found. False is worth
	// shouting about: it means somebody's rig is left where this run left it.
	Restored    bool   `json:"restored"`
	RestoreErr  string `json:"restore_err,omitempty"`
	Interrupted bool   `json:"interrupted,omitempty"`
	Aborted     string `json:"aborted,omitempty"`
}

// writer emits the JSON Lines stream and keeps the running tally.
type writer struct {
	enc      *json.Encoder
	seq      int
	counts   map[Verdict]int
	failures []string
}

func newWriter(w io.Writer) *writer {
	enc := json.NewEncoder(w)
	return &writer{enc: enc, counts: map[Verdict]int{}}
}

func (w *writer) header(h Header) error {
	h.Record = "header"
	h.FormatVersion = FormatVersion
	return w.enc.Encode(h)
}

func (w *writer) step(s Step) error {
	w.seq++
	s.Record = "step"
	s.Seq = w.seq
	w.counts[s.Verdict]++
	if s.Verdict == Fail {
		w.failures = append(w.failures, fmt.Sprintf("%s/%s: %s", s.Group, s.Name, firstLine(s.Note, s.Err)))
	}
	return w.enc.Encode(s)
}

// summary completes the tally and writes it. It returns the finished value
// rather than only encoding it, because the caller prints the same numbers to
// the terminal and filling them into a copy left the operator staring at an
// empty result.
func (w *writer) summary(s Summary) (Summary, error) {
	s.Record = "summary"
	s.Counts = w.counts
	s.Failures = w.failures
	return s, w.enc.Encode(s)
}

// firstLine picks the most useful of the strings offered, for the failure list.
func firstLine(candidates ...string) string {
	for _, c := range candidates {
		if c != "" {
			return c
		}
	}
	return "no detail"
}
