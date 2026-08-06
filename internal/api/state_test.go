package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/rig"
)

func TestGetStateServesTheCacheWithItsAge(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodGet, "/radios/"+connectedRadio+"/state", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got stateBody
	e.decode(rr, &got)
	if !got.Connected {
		t.Error("connected = false on a started radio")
	}
	if got.Frequency != 14025000 {
		t.Errorf("frequency = %d, want 14025000", got.Frequency)
	}
	if got.Stale {
		t.Errorf("stale = true on a freshly polled radio (age %d ms)", got.AgeMS)
	}
	if got.AgeMS < 0 {
		t.Errorf("age_ms = %d, want a non-negative age", got.AgeMS)
	}
}

// Staleness is measured against this radio's own fast poll interval, not
// against a constant: a rig polled twice a second and one polled every five
// seconds are both healthy.
func TestGetStateMarksASnapshotStaleAfterThreePollIntervals(t *testing.T) {
	skew := 4 * 50 * time.Millisecond // four fast poll intervals
	e := newEnv(t, func(o *envOpts) {
		o.now = func() time.Time { return time.Now().Add(skew) }
	})

	var got stateBody
	e.decode(e.do(http.MethodGet, "/radios/"+connectedRadio+"/state", nil), &got)
	if !got.Stale {
		t.Errorf("stale = false with a %s old snapshot (age_ms %d)", skew, got.AgeMS)
	}
}

// A disconnected radio still answers from the cache: the state is published
// through an atomic pointer precisely so that a dead port cannot stall a
// reader, and connected/stale say what the client is looking at.
func TestGetStateOfADisconnectedRadioIsStillServed(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodGet, "/radios/"+disconnectedRadio+"/state", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var got stateBody
	e.decode(rr, &got)
	if got.Connected {
		t.Error("connected = true on a radio that was never started")
	}
}

func TestPatchStateAppliesAndReadsBackFromTheRig(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state", map[string]any{
		"mode":        "CW",
		"frequency":   14100000,
		"filter_slot": 2,
		"passband_hz": 400,
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got stateBody
	e.decode(rr, &got)
	if got.Frequency != 14100000 {
		t.Errorf("frequency = %d, want 14100000", got.Frequency)
	}
	if got.Mode != "CW" {
		t.Errorf("mode = %s, want CW", got.Mode)
	}
	if got.FilterSlot != 2 {
		t.Errorf("filter_slot = %d, want 2", got.FilterSlot)
	}
	if got.PassbandHz != 400 {
		t.Errorf("passband_hz = %d, want 400", got.PassbandHz)
	}
}

// The response has to say what the radio is doing, not what was asked for.
func TestPatchStateReportsTheClampedPower(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"power_pct": 100}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got stateBody
	e.decode(rr, &got)
	if got.Power.Pct != 80 {
		t.Errorf("power.pct = %v, want 80 (limits.max_power_pct)", got.Power.Pct)
	}
}

// power_w and power_pct are two spellings of one setting, and a body carrying
// both does not say which one the operator meant.
func TestPatchStateRejectsBothPowerFields(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"power_w": 40, "power_pct": 40}, token)
	doc := e.problemOf(rr, http.StatusUnprocessableEntity)

	detail, _ := doc["detail"].(string)
	if !strings.Contains(detail, "power_w") || !strings.Contains(detail, "power_pct") {
		t.Errorf("detail = %q, want it to name both fields", detail)
	}
	if doc["radio_id"] != connectedRadio {
		t.Errorf("radio_id = %v, want %s", doc["radio_id"], connectedRadio)
	}
}

func TestPatchStateRejectsBadBodies(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	bodies := map[string]any{
		"malformed json":   `{"frequency": `,
		"unknown field":    `{"frequancy": 14025000}`,
		"wrong type":       `{"frequency": "fourteen"}`,
		"unknown mode":     `{"mode": "SSB"}`,
		"unknown vfo":      `{"vfo": "third"}`,
		"empty body":       ``,
		"trailing garbage": `{"ptt": false} {"ptt": true}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state", body, token)
			e.problemOf(rr, http.StatusUnprocessableEntity)
		})
	}
}

func TestPatchStateCapsTheRequestBody(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	huge := `{"frequency": 14025000, "mode": "` + strings.Repeat("C", 128<<10) + `"}`
	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state", huge, token)
	doc := e.problemOf(rr, http.StatusUnprocessableEntity)
	if detail, _ := doc["detail"].(string); !strings.Contains(detail, "exceeds") {
		t.Errorf("detail = %q, want it to mention the body limit", detail)
	}
}

func TestPatchStateErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		radio  string
		body   map[string]any
		inject func(*env)
		want   int
	}{
		{
			name: "outside the configured band limits",
			body: map[string]any{"frequency": 21200000},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "mode this radio does not have",
			body: map[string]any{"mode": "AM"},
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "filter slot this radio does not have",
			body: map[string]any{"filter_slot": 9},
			want: http.StatusUnprocessableEntity,
		},
		{
			name:  "radio not connected",
			radio: disconnectedRadio,
			body:  map[string]any{"frequency": 14100000},
			want:  http.StatusServiceUnavailable,
		},
		{
			name: "rig did not answer",
			body: map[string]any{"frequency": 14100000},
			inject: func(e *env) {
				e.rigs[connectedRadio].failOn("frequency",
					fmt.Errorf("radio %s: %w", connectedRadio, rig.ErrTimeout))
			},
			want: http.StatusGatewayTimeout,
		},
		{
			name: "rig dropped the connection",
			body: map[string]any{"frequency": 14100000},
			inject: func(e *env) {
				e.rigs[connectedRadio].failOn("frequency",
					fmt.Errorf("radio %s: %w", connectedRadio, rig.ErrDisconnected))
			},
			want: http.StatusServiceUnavailable,
		},
		{
			// "Not now" is not "no". The radio answered, so the request was
			// well formed and the link is fine — 503 tells the client to try
			// again, where the 422 an outright rejection earns would tell it to
			// rewrite a request that was already correct.
			name: "rig was busy",
			body: map[string]any{"frequency": 14100000},
			inject: func(e *env) {
				e.rigs[connectedRadio].failOn("frequency",
					fmt.Errorf("yaesu: FA;: the rig answered ?: %w", rig.ErrBusy))
			},
			want: http.StatusServiceUnavailable,
		},
		{
			name: "something nobody anticipated",
			body: map[string]any{"frequency": 14100000},
			inject: func(e *env) {
				e.rigs[connectedRadio].failOn("frequency", errors.New("backend fell over"))
			},
			want: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.inject != nil {
				tc.inject(e)
			}
			id := tc.radio
			if id == "" {
				id = connectedRadio
			}
			token := e.acquire(id)

			rr := e.doLocked(http.MethodPatch, "/radios/"+id+"/state", tc.body, token)
			doc := e.problemOf(rr, tc.want)
			if doc["radio_id"] != id {
				t.Errorf("radio_id = %v, want %s", doc["radio_id"], id)
			}
		})
	}
}

// The status code is only half of it: a client that gets a 503 has to be able
// to tell "the radio was busy, send it again" from "the radio is not connected"
// without parsing an error string, so the detail says which it was.
func TestBusyRigIsARetryable503(t *testing.T) {
	e := newEnv(t)
	e.rigs[connectedRadio].failOn("ptt",
		fmt.Errorf("yaesu: TX;: the rig answered ?: %w", rig.ErrBusy))
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"ptt": true}, token)
	doc := e.problemOf(rr, http.StatusServiceUnavailable)

	detail, _ := doc["detail"].(string)
	if !strings.Contains(detail, "busy") || !strings.Contains(detail, "retried") {
		t.Errorf("detail = %q, want it to say the radio was busy and the request can be retried", detail)
	}
	// Nothing here is the client's fault, so it must never be told to rewrite
	// the request.
	if rr.Code == http.StatusUnprocessableEntity {
		t.Error("a busy radio was reported as an unprocessable request")
	}
}

// A 500 must say nothing about the daemon's insides, but the operator reading
// the log must still get the real reason.
func TestInternalErrorsAreLoggedButNotPublished(t *testing.T) {
	e := newEnv(t)
	e.rigs[connectedRadio].failOn("frequency", errors.New("a serial port fell into the sea"))
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"frequency": 14100000}, token)
	doc := e.problemOf(rr, http.StatusInternalServerError)

	if strings.Contains(fmt.Sprint(doc), "sea") {
		t.Errorf("problem document leaks the internal error: %v", doc)
	}
	if !strings.Contains(e.logs.String(), "sea") {
		t.Errorf("the real error never reached the log:\n%s", e.logs.String())
	}
}

func TestPatchStateWritesAnAuditLine(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"frequency": 14100000, "ptt": false}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	logged := e.logs.String()
	for _, want := range []string{
		"msg=audit",
		"user=" + testUser,
		"radio=" + connectedRadio,
		"action=" + actionPatchState,
		"result=ok",
		"frequency=14100000",
		"ptt=false",
		"lock=" + tokenID(token),
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit line does not carry %q:\n%s", want, logged)
		}
	}
	// The token is a bearer credential; the log gets a fingerprint of it, not
	// the thing itself.
	if strings.Contains(logged, token) {
		t.Errorf("the lock token was written to the log:\n%s", logged)
	}
}

// A refused state change is still a state-changing request, and the operator
// wants to see the attempt.
func TestDeniedRequestsAreAudited(t *testing.T) {
	e := newEnv(t)
	e.acquire(connectedRadio) // held, but the request below sends no token

	rr := e.do(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"ptt": true})
	e.problemOf(rr, http.StatusLocked)

	logged := e.logs.String()
	if !strings.Contains(logged, "action="+actionPatchState) || !strings.Contains(logged, "result=denied") {
		t.Errorf("denied request was not audited:\n%s", logged)
	}
}
