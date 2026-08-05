package kenwood

import (
	"context"
	"fmt"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// testConn stands in for the session's transaction layer. It is deliberately
// thin: requests are scripted to answers, answers go through the real Decode, and
// every request is recorded so a test can assert the exact wire conversation.
//
// One behaviour is worth calling out. A matched negative acknowledgement is
// returned as an Update with OK false and a nil error, rather than as an error.
// The session is free to do either, and returning it this way exercises the
// backend's own rejection handling instead of hiding it behind the session's.
type testConn struct {
	t *testing.T
	k *Rig

	// answers maps a request, terminator included, to the answer frame the rig
	// would send back, without its terminator. A request that is absent stands
	// for a rig that says nothing at all.
	answers map[string]string

	// sent records every request written, Do and Send alike, in order.
	sent []string
}

func newTestConn(t *testing.T, k *Rig, answers map[string]string) *testConn {
	t.Helper()
	if answers == nil {
		answers = map[string]string{}
	}
	return &testConn{t: t, k: k, answers: answers}
}

func (c *testConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	s := string(req)
	c.sent = append(c.sent, s)
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("testConn: Do(%q) with no wanted keys", s)
	}

	answer, ok := c.answers[s]
	if !ok {
		// What a silent rig looks like from inside a backend: the session's
		// per-command timeout, not a protocol error.
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %q", s)
	}

	u, err := c.k.Decode([]byte(answer))
	if err != nil {
		return u, err
	}
	for _, w := range want {
		if u.Key == w {
			return u, nil
		}
	}
	return backend.Update{}, fmt.Errorf("testConn: answer %q keyed %q, none of %v", answer, u.Key, want)
}

func (c *testConn) Send(ctx context.Context, req []byte) error {
	c.sent = append(c.sent, string(req))
	return nil
}

// wantSent asserts the exact request sequence.
func (c *testConn) wantSent(t *testing.T, want ...string) {
	t.Helper()
	if len(c.sent) != len(want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
	for i := range want {
		if c.sent[i] != want[i] {
			t.Fatalf("request %d = %q, want %q (full: %q)", i, c.sent[i], want[i], c.sent)
		}
	}
}

// newRig builds a backend with the given AI and bulk-poll settings, bypassing
// config so a test can state its intent in one line.
func newRig(t *testing.T, ai int, bulk bool) *Rig {
	t.Helper()
	k, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k.ai = ai
	k.bulkPoll = bulk
	return k
}
