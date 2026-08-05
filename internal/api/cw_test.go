package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
)

func TestGetCWReportsTheQueue(t *testing.T) {
	e := newEnv(t)
	e.cw.mu.Lock()
	e.cw.status = radio.CWStatus{Busy: true, Queued: 12, WPM: 28, EstRemainingMS: 4200}
	e.cw.mu.Unlock()

	rr := e.do(http.MethodGet, "/radios/"+connectedRadio+"/cw", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got struct {
		Busy           bool `json:"busy"`
		Queued         int  `json:"queued"`
		WPM            int  `json:"wpm"`
		EstRemainingMS int  `json:"est_remaining_ms"`
	}
	e.decode(rr, &got)
	if !got.Busy || got.Queued != 12 || got.WPM != 28 || got.EstRemainingMS != 4200 {
		t.Errorf("status = %+v, want the sender's", got)
	}
}

// A radio with no keyer still answers with an empty queue: a polling client
// should not have to special-case it, and GET /radios is where it learns that
// the radio cannot send.
func TestGetCWOnARadioWithoutAKeyer(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodGet, "/radios/"+disconnectedRadio+"/cw", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var got struct {
		Queued int `json:"queued"`
	}
	e.decode(rr, &got)
	if got.Queued != 0 {
		t.Errorf("queued = %d, want 0", got.Queued)
	}
}

func TestSendCWQueuesAndEstimates(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "CQ TEST DE OH2XYZ ^AR", "wpm": 28}, token)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}

	var got cwAcceptedDTO
	e.decode(rr, &got)
	if got.QueuedChars != 21 {
		t.Errorf("queued_chars = %d, want 21", got.QueuedChars)
	}
	if got.Position != 1 {
		t.Errorf("position = %d, want 1 on an empty queue", got.Position)
	}
	// Counted in elements, not characters: 21 characters at 28 wpm is several
	// seconds, and a character count would put it nowhere near.
	if got.EstDurationMS < 3000 || got.EstDurationMS > 20000 {
		t.Errorf("est_duration_ms = %d, want a plausible duration", got.EstDurationMS)
	}
	if wpm := e.cw.Status().WPM; wpm != 28 {
		t.Errorf("keyer speed = %d, want the requested 28", wpm)
	}

	// The next text queues behind the first.
	rr = e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "TU"}, token)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rr.Code)
	}
	e.decode(rr, &got)
	if got.Position != 22 {
		t.Errorf("position = %d, want 22 behind the first text", got.Position)
	}
}

// Both target rigs silently mangle a character they cannot key, so the API has
// to name it rather than let it go on the air as something else.
func TestSendCWUnsendableCharacterIs422(t *testing.T) {
	e := newEnv(t)
	e.cw.fail(&cw.CharError{Char: '#', Offset: 3, Charset: "ABCDE"})
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "AB #"}, token)
	doc := e.problemOf(rr, http.StatusUnprocessableEntity)

	if doc["character"] != "#" {
		t.Errorf("character = %v, want #", doc["character"])
	}
	if doc["offset"] != float64(3) {
		t.Errorf("offset = %v, want 3", doc["offset"])
	}
	if doc["charset"] != "ABCDE" {
		t.Errorf("charset = %v, want ABCDE", doc["charset"])
	}
	if doc["radio_id"] != connectedRadio {
		t.Errorf("radio_id = %v, want %s", doc["radio_id"], connectedRadio)
	}
}

func TestSendCWOnARadioThatCannotKeyIs422(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(disconnectedRadio)

	rr := e.doLocked(http.MethodPost, "/radios/"+disconnectedRadio+"/cw",
		map[string]any{"text": "TEST"}, token)
	e.problemOf(rr, http.StatusUnprocessableEntity)
}

func TestSendCWRejectsBadRequests(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	for name, body := range map[string]any{
		"no text":       map[string]any{"wpm": 25},
		"empty text":    map[string]any{"text": ""},
		"unknown mode":  map[string]any{"text": "TEST", "mode": "prepend"},
		"unknown field": `{"text": "TEST", "speed": 25}`,
	} {
		t.Run(name, func(t *testing.T) {
			rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw", body, token)
			e.problemOf(rr, http.StatusUnprocessableEntity)
		})
	}
}

func TestSendCWReplaceReachesTheSender(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "FIRST"}, token)
	rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "SECOND", "mode": "replace"}, token)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}

	// The fake drops the queue on replace, so what is left is the new text.
	if got := e.cw.Status().Queued; got != len("SECOND") {
		t.Errorf("queued = %d after a replace, want %d", got, len("SECOND"))
	}
}

func TestAbortCW(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "CQ CQ"}, token)

	rr := e.doLocked(http.MethodDelete, "/radios/"+connectedRadio+"/cw", nil, token)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("204 with a body: %s", rr.Body.String())
	}
	if e.cw.abortCount() != 1 {
		t.Errorf("sender aborted %d times, want 1", e.cw.abortCount())
	}
}

// Aborting is a safety action; a radio with no keyer has nothing to stop, and
// failing would leave a client reasoning about errors mid-transmission.
func TestAbortCWWithoutAKeyerSucceeds(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(disconnectedRadio)

	rr := e.doLocked(http.MethodDelete, "/radios/"+disconnectedRadio+"/cw", nil, token)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestCWRequestsAreAudited(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/cw",
		map[string]any{"text": "CQ"}, token)
	e.doLocked(http.MethodDelete, "/radios/"+connectedRadio+"/cw", nil, token)

	logged := e.logs.String()
	for _, want := range []string{"action=" + actionSendCW, "action=" + actionAbortCW, "user=" + testUser} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log does not carry %q:\n%s", want, logged)
		}
	}
}
