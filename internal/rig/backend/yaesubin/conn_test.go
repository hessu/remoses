package yaesubin

import (
	"context"
	"fmt"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
)

// testConn stands in for the session's transaction layer.
//
// It is thinner than the ASCII backends' fakes in one place and thicker in
// another. Thinner, because there is no busy answer to script: this protocol
// has no rejection of any kind, so an unscripted request produces the only
// thing a real radio produces for one — silence, and a timeout.
//
// Thicker, because it reproduces the framing contract. A real answer is sized
// by Expect, which the session calls under its write lock, so the fake calls it
// too and then routes the answer through the real Split before the real Decode.
// That is what makes a framing bug visible in a unit test rather than only on a
// serial port: a test that fed Decode directly would never notice that Split
// had taken the wrong number of bytes.
type testConn struct {
	t *testing.T
	y *Rig

	// answers maps a request, as hex, to the bytes the rig would answer with.
	answers map[string][]byte

	// sent records every request written, in order, as hex.
	sent []string
}

func newTestConn(t *testing.T, y *Rig, answers map[string][]byte) *testConn {
	t.Helper()
	if answers == nil {
		answers = map[string][]byte{}
	}
	return &testConn{t: t, y: y, answers: answers}
}

func hex(b []byte) string { return fmt.Sprintf("% 02X", b) }

func (c *testConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	s := hex(req)
	c.sent = append(c.sent, s)
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("testConn: Do(%s) with no wanted keys", s)
	}

	// Exactly what conn.writeLocked does, and in the same place: before the
	// bytes go out, so the reader can size what comes back.
	c.y.Expect(req)

	answer, ok := c.answers[s]
	if !ok {
		// No scripted answer is what an unimplemented or malformed command
		// costs on this protocol: nothing comes back at all.
		return backend.Update{}, fmt.Errorf("testConn: timeout waiting for an answer to %s", s)
	}

	frames, err := scan(c.y, answer)
	if err != nil {
		return backend.Update{}, err
	}
	if len(frames) != 1 {
		return backend.Update{}, fmt.Errorf("testConn: answer %s to %s framed as %d frames, want 1",
			hex(answer), s, len(frames))
	}

	u, err := c.y.Decode(frames[0])
	if err != nil {
		return u, err
	}
	for _, w := range want {
		if u.Key != w {
			continue
		}
		if !u.OK {
			// The session hands back the update AND an error; see checkUpdate
			// in internal/rig. Reproduced rather than imported, because a
			// backend may not import internal/rig.
			return u, fmt.Errorf("testConn: rejected %s (what the session reports as rig.ErrNAK)", s)
		}
		return u, nil
	}
	return backend.Update{}, fmt.Errorf("testConn: answer %s keyed %q, none of %v",
		hex(answer), u.Key, want)
}

func (c *testConn) Send(ctx context.Context, req []byte) error {
	c.t.Errorf("Send(%s): this backend must never fire and forget — "+
		"an unconsumed answer offsets the stream permanently", hex(req))
	return nil
}

// scan runs bytes through the backend's Split the way bufio.Scanner does,
// returning the frames it produced.
func scan(y *Rig, data []byte) ([][]byte, error) {
	var frames [][]byte
	for i := 0; i <= len(data); {
		adv, tok, err := y.Split(data[i:], false)
		if err != nil {
			return nil, err
		}
		if adv == 0 && tok == nil {
			break // wants more data
		}
		i += adv
		if tok != nil {
			frames = append(frames, tok)
		}
		if adv == 0 {
			return nil, fmt.Errorf("scan: Split returned a token without advancing")
		}
	}
	return frames, nil
}

// wantSent asserts the exact request sequence.
func (c *testConn) wantSent(t *testing.T, want ...string) {
	t.Helper()
	if len(c.sent) != len(want) {
		t.Fatalf("sent %q, want %q", c.sent, want)
	}
	for i := range want {
		if c.sent[i] != want[i] {
			t.Fatalf("request %d was %q, want %q (full: %q)", i, c.sent[i], want[i], c.sent)
		}
	}
}

// ack is the single byte every set command is answered with.
var ack = []byte{0x00}

// answersFor is a rig sitting on 14.250 MHz in USB, receiving, with a
// mid-scale S-meter. It answers the three reads and acknowledges everything
// else.
func answersFor() map[string][]byte {
	return map[string][]byte{
		hex(read(opReadFreqMode)): {0x01, 0x42, 0x50, 0x00, 0x01},
		hex(read(opReadTXStatus)): {0x80}, // PTT off
		hex(read(opReadRXStatus)): {0x08}, // S-meter 8 of 15
	}
}

// withAcks adds an acknowledgement for each of the given set commands.
func withAcks(m map[string][]byte, reqs ...[]byte) map[string][]byte {
	for _, r := range reqs {
		m[hex(r)] = ack
	}
	return m
}

// radioCfg is the configuration for one of these radios: the shared yaesu
// block, which is what the dispatch in the yaesu package hands over.
func radioCfg(model string) *config.Radio {
	return &config.Radio{
		ID:      "test",
		Backend: config.BackendYaesu,
		Yaesu:   &config.Yaesu{Model: model},
	}
}

func testRig(t *testing.T, model string) *Rig {
	t.Helper()
	y, err := New(radioCfg(model))
	if err != nil {
		t.Fatalf("New(%q): %v", model, err)
	}
	return y
}
