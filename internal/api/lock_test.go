package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/lock"
)

func TestAcquireLockIssuesAToken(t *testing.T) {
	e := newEnv(t)

	rr := e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}

	var got lockDTO
	e.decode(rr, &got)
	if got.Token == "" {
		t.Error("no token in the response")
	}
	if got.TTLSeconds != 30 {
		t.Errorf("ttl_seconds = %d, want 30", got.TTLSeconds)
	}
	if _, err := time.Parse(time.RFC3339, got.ExpiresAt); err != nil {
		t.Errorf("expires_at = %q: %v", got.ExpiresAt, err)
	}
}

func TestGetLockReportsHolderAndWhetherItIsMine(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	var mine lockStateDTO
	e.decode(e.doLocked(http.MethodGet, "/radios/"+connectedRadio+"/lock", nil, token), &mine)
	if !mine.Held || mine.Holder != testUser || !mine.IsMine {
		t.Errorf("with the token: %+v, want held by %s and mine", mine, testUser)
	}

	var theirs lockStateDTO
	e.decode(e.do(http.MethodGet, "/radios/"+connectedRadio+"/lock", nil), &theirs)
	if !theirs.Held || theirs.IsMine {
		t.Errorf("without the token: %+v, want held but not mine", theirs)
	}

	var free lockStateDTO
	e.decode(e.do(http.MethodGet, "/radios/"+disconnectedRadio+"/lock", nil), &free)
	if free.Held || free.Holder != "" {
		t.Errorf("unlocked radio: %+v, want held=false", free)
	}
}

// Reads must not renew. A client redrawing its screen twice a second would
// otherwise hold a radio forever without ever sending a command.
func TestReadsDoNotSlideTheLeaseButCommandsDo(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	before, _ := e.locks.Inspect(connectedRadio)

	e.doLocked(http.MethodGet, "/radios", nil, token)
	e.doLocked(http.MethodGet, "/radios/"+connectedRadio+"/lock", nil, token)
	e.doLocked(http.MethodGet, "/radios/"+connectedRadio+"/state", nil, token)

	after, _ := e.locks.Inspect(connectedRadio)
	if !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Errorf("a read slid the lease: %s -> %s", before.ExpiresAt, after.ExpiresAt)
	}

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"frequency": 14100000}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: status = %d (body %s)", rr.Code, rr.Body.String())
	}
	renewed, _ := e.locks.Inspect(connectedRadio)
	if !renewed.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("an accepted command did not slide the lease: %s -> %s",
			before.ExpiresAt, renewed.ExpiresAt)
	}
}

func TestListRadiosReportsTheLockWithoutRenewingIt(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	var radios []radioDTO
	e.decode(e.doLocked(http.MethodGet, "/radios", nil, token), &radios)

	if !radios[0].Lock.Held || !radios[0].Lock.IsMine {
		t.Errorf("lock in the listing = %+v, want held and mine", radios[0].Lock)
	}
	if radios[1].Lock.Held {
		t.Errorf("second radio reported as locked: %+v", radios[1].Lock)
	}
}

// Every state-changing route is gated, and no read is.
func TestStateChangingRoutesRequireTheLock(t *testing.T) {
	e := newEnv(t)

	for _, rt := range e.srv.routes() {
		if rt.lockAction == "" {
			continue
		}
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			// No lock exists at all.
			rr := e.do(rt.method, pathFor(rt.path, disconnectedRadio), map[string]any{})
			doc := e.problemOf(rr, http.StatusLocked)
			if doc["radio_id"] != disconnectedRadio {
				t.Errorf("radio_id = %v, want %s", doc["radio_id"], disconnectedRadio)
			}
		})
	}
}

func TestForeignTokenIsAConflict(t *testing.T) {
	e := newEnv(t)
	held, err := e.locks.Acquire(connectedRadio, "someone-else", false)
	if err != nil {
		t.Fatalf("acquiring as another user: %v", err)
	}

	rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"ptt": false}, "not-the-right-token")
	doc := e.problemOf(rr, http.StatusConflict)

	if doc["locked_by"] != "someone-else" {
		t.Errorf("locked_by = %v, want someone-else", doc["locked_by"])
	}
	if doc["expires_at"] != held.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("expires_at = %v, want %s", doc["expires_at"], held.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

// The cookie exists only because a browser cannot easily put a header on every
// request; a stale one must never override what the client actually sent.
func TestLockHeaderBeatsCookie(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	t.Run("cookie alone is accepted", func(t *testing.T) {
		r := e.req(http.MethodPatch, "/radios/"+connectedRadio+"/state",
			map[string]any{"frequency": 14050000})
		r.AddCookie(&http.Cookie{Name: lockCookiePrefix + connectedRadio, Value: token})
		if rr := e.send(r); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("header wins over a stale cookie", func(t *testing.T) {
		r := e.req(http.MethodPatch, "/radios/"+connectedRadio+"/state",
			map[string]any{"frequency": 14060000})
		r.Header.Set(lockHeader, token)
		r.AddCookie(&http.Cookie{Name: lockCookiePrefix + connectedRadio, Value: "stale"})
		if rr := e.send(r); rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
		}
	})

	t.Run("a wrong header is not rescued by a good cookie", func(t *testing.T) {
		r := e.req(http.MethodPatch, "/radios/"+connectedRadio+"/state",
			map[string]any{"frequency": 14070000})
		r.Header.Set(lockHeader, "stale")
		r.AddCookie(&http.Cookie{Name: lockCookiePrefix + connectedRadio, Value: token})
		rr := e.send(r)
		if rr.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
		}
	})
}

// The cookie is per radio, so holding one rig must not appear to unlock
// another.
func TestLockCookieIsPerRadio(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	r := e.req(http.MethodPatch, "/radios/"+disconnectedRadio+"/state", map[string]any{"ptt": false})
	r.AddCookie(&http.Cookie{Name: lockCookiePrefix + connectedRadio, Value: token})
	rr := e.send(r)
	e.problemOf(rr, http.StatusLocked)
}

func TestRenewSlidesTheLease(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)
	before, _ := e.locks.Inspect(connectedRadio)

	rr := e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/lock/renew", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	var got lockDTO
	e.decode(rr, &got)
	if got.Token != token {
		t.Errorf("renew changed the token: %q -> %q", token, got.Token)
	}
	after, _ := e.locks.Inspect(connectedRadio)
	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Errorf("renew did not extend the lease: %s -> %s", before.ExpiresAt, after.ExpiresAt)
	}
}

// Releasing is a safety event: a client that releases while transmitting must
// not leave a carrier up.
func TestReleaseDropsPTTAndFlushesTheCWQueue(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)

	if rr := e.doLocked(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"ptt": true}, token); rr.Code != http.StatusOK {
		t.Fatalf("keying: status = %d (body %s)", rr.Code, rr.Body.String())
	}

	rr := e.doLocked(http.MethodDelete, "/radios/"+connectedRadio+"/lock", nil, token)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rr.Code, rr.Body.String())
	}

	var st stateBody
	e.decode(e.do(http.MethodGet, "/radios/"+connectedRadio+"/state", nil), &st)
	if st.PTT {
		t.Error("the transmitter is still keyed after releasing the lock")
	}
	if e.cw.abortCount() == 0 {
		t.Error("the CW queue was not flushed on release")
	}
	if _, held := e.locks.Inspect(connectedRadio); held {
		t.Error("the lock survived its own release")
	}
}

// The spec documents 423 and no 409 for a release: from this client's point of
// view somebody else's token is simply not a valid one.
func TestReleaseWithTheWrongTokenIsLocked(t *testing.T) {
	e := newEnv(t)
	if _, err := e.locks.Acquire(connectedRadio, "someone-else", false); err != nil {
		t.Fatalf("acquiring as another user: %v", err)
	}

	rr := e.doLocked(http.MethodDelete, "/radios/"+connectedRadio+"/lock", nil, "not-mine")
	e.problemOf(rr, http.StatusLocked)

	if _, held := e.locks.Inspect(connectedRadio); !held {
		t.Error("a stranger's release dropped the lock")
	}
}

func TestAcquireHeldByAnotherUserIsAConflict(t *testing.T) {
	e := newEnv(t)
	if _, err := e.locks.Acquire(connectedRadio, "someone-else", false); err != nil {
		t.Fatalf("acquiring as another user: %v", err)
	}

	rr := e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", nil)
	doc := e.problemOf(rr, http.StatusConflict)
	if doc["locked_by"] != "someone-else" {
		t.Errorf("locked_by = %v, want someone-else", doc["locked_by"])
	}
	if doc["expires_at"] == nil {
		t.Error("409 without expires_at")
	}
}

func TestStealIsForbiddenUnlessConfigured(t *testing.T) {
	e := newEnv(t)
	if _, err := e.locks.Acquire(connectedRadio, "someone-else", false); err != nil {
		t.Fatalf("acquiring as another user: %v", err)
	}

	rr := e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", map[string]any{"force": true})
	doc := e.problemOf(rr, http.StatusForbidden)
	if doc["locked_by"] != "someone-else" {
		t.Errorf("locked_by = %v, want someone-else", doc["locked_by"])
	}
}

func TestStealSucceedsWhenConfigured(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.allowSteal = true })
	if _, err := e.locks.Acquire(connectedRadio, "someone-else", false); err != nil {
		t.Fatalf("acquiring as another user: %v", err)
	}

	rr := e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", map[string]any{"force": true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
	if l, _ := e.locks.Inspect(connectedRadio); l.User != testUser {
		t.Errorf("holder = %q, want %s", l.User, testUser)
	}
}

// force on a free radio is not a steal and must not be refused.
func TestForceOnAFreeRadioIsNotRefused(t *testing.T) {
	e := newEnv(t)
	rr := e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock", map[string]any{"force": true})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rr.Code, rr.Body.String())
	}
}

func TestLockingDisabledLetsEverythingThrough(t *testing.T) {
	e := newEnv(t, func(o *envOpts) { o.lockEnabled = false })

	rr := e.do(http.MethodPatch, "/radios/"+connectedRadio+"/state",
		map[string]any{"frequency": 14100000})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch without a token: status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}

	rr = e.do(http.MethodPost, "/radios/"+connectedRadio+"/cw", map[string]any{"text": "TEST"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("cw without a token: status = %d, want 202", rr.Code)
	}

	rr = e.do(http.MethodPost, "/radios/"+connectedRadio+"/lock/renew", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("renew without locking: status = %d, want 200", rr.Code)
	}

	rr = e.do(http.MethodDelete, "/radios/"+connectedRadio+"/lock", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("release without locking: status = %d, want 204", rr.Code)
	}
}

// The mapping is part of the contract, so it is asserted directly rather than
// only through the routes that happen to be able to produce each sentinel.
func TestErrorClassification(t *testing.T) {
	e := newEnv(t)

	tests := []struct {
		err  error
		want int
	}{
		{errNoSuchRadio, http.StatusNotFound},
		{fmt.Errorf("wrapped: %w", lock.ErrHeldByOther), http.StatusConflict},
		{fmt.Errorf("wrapped: %w", lock.ErrNoLock), http.StatusLocked},
		{fmt.Errorf("wrapped: %w", lock.ErrBadToken), http.StatusLocked},
		{fmt.Errorf("wrapped: %w", lock.ErrExpired), http.StatusLocked},
	}
	for _, tc := range tests {
		got, _, _, _ := e.srv.classify(connectedRadio, tc.err)
		if got != tc.want {
			t.Errorf("classify(%v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

func TestLockChangesAreAudited(t *testing.T) {
	e := newEnv(t)
	token := e.acquire(connectedRadio)
	e.doLocked(http.MethodPost, "/radios/"+connectedRadio+"/lock/renew", nil, token)
	e.doLocked(http.MethodDelete, "/radios/"+connectedRadio+"/lock", nil, token)

	logged := e.logs.String()
	for _, want := range []string{
		"action=" + actionAcquireLock,
		"action=" + actionRenewLock,
		"action=" + actionReleaseLock,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("audit log does not carry %q:\n%s", want, logged)
		}
	}
}
