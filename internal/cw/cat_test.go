package cw

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
)

func newCAT(t *testing.T, f *fakeMorse, cfg config.CW) Sender {
	t.Helper()
	s, err := NewCAT(f, fakeConn{}, cfg)
	if err != nil {
		t.Fatalf("NewCAT: %v", err)
	}
	t.Cleanup(func() { s.(io.Closer).Close() })
	return s
}

// waitFor polls a condition, so that a loaded machine makes the test slower
// rather than flaky.
func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", timeout, what)
}

// TestCATClosedLoopWaitsForBufferSpace is the Kenwood case: KY; answers "full"
// or "space available", and writing to a full buffer is a hard error, so the
// sender must poll and hold off.
func TestCATClosedLoopWaitsForBufferSpace(t *testing.T) {
	f := &fakeMorse{
		maxChunk: 24,
		charset:  kenwoodCharset,
		encode:   encodeKenwood,
		freeOK:   true,
		free:     0, // the rig is busy sending something else
	}
	s := newCAT(t, f, config.CW{DefaultWPM: 20, ChunksInFlight: 1})

	if _, err := s.Enqueue("CQ CQ CQ DE OH2XYZ OH2XYZ ^AR", Append); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Nothing may go out while the buffer is full, however long we wait.
	time.Sleep(150 * time.Millisecond)
	if n := f.sentCount(); n != 0 {
		t.Fatalf("sent %d chunks to a full buffer", n)
	}
	if p := f.pollCount(); p < 2 {
		t.Errorf("polled %d times in 150 ms at 20 wpm; the poll should track one dit time", p)
	}
	if !s.Status().Busy {
		t.Error("status should be busy while text is waiting for the rig")
	}

	// Space appears: the first chunk goes, and the fake's buffer is full again.
	f.setFree(24)
	waitFor(t, "the first chunk", 2*time.Second, func() bool { return f.sentCount() == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := f.sentCount(); n != 1 {
		t.Fatalf("sent %d chunks, want 1: the buffer was full again", n)
	}

	f.setFree(24)
	waitFor(t, "the second chunk", 2*time.Second, func() bool { return f.sentCount() == 2 })

	if f.overrun != "" {
		t.Fatalf("closed loop overran the rig buffer: %s", f.overrun)
	}
}

// TestCATOpenLoopRespectsChunksInFlight is the Icom case: there is no buffer
// query, so the sender paces on the element-timing estimate.
func TestCATOpenLoopRespectsChunksInFlight(t *testing.T) {
	f := &fakeMorse{maxChunk: 8, charset: icomCharset, encode: encodeIcom, freeOK: false}
	s := newCAT(t, f, config.CW{DefaultWPM: 60, ChunksInFlight: 1})

	// Four words of four dits: one chunk each, 13 units, 260 ms at 60 wpm.
	// The gap between words makes the later chunks 20 units, 400 ms.
	const chunk1 = 13 * 20 * time.Millisecond
	if _, err := s.Enqueue("EEEE EEEE EEEE EEEE", Append); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// One chunk sounding plus ChunksInFlight in the rig's buffer, and no more.
	waitFor(t, "the first two chunks", time.Second, func() bool { return f.sentCount() == 2 })
	time.Sleep(chunk1 / 2)
	if n := f.sentCount(); n != 2 {
		t.Fatalf("sent %d chunks before the first had drained, want 2", n)
	}

	waitFor(t, "the whole message", 4*time.Second, func() bool { return f.sentCount() == 4 })

	sent := f.sentChunks()
	if got := f.sentText(); got != "EEEE EEEE EEEE EEEE" {
		t.Errorf("the rig received %q", got)
	}
	// The second chunk fills the buffer immediately; the third waits for the
	// first to drain. Only the lower bound is asserted, since a loaded machine
	// can be late but never early.
	if d := sent[1].at.Sub(sent[0].at); d > 200*time.Millisecond {
		t.Errorf("the second chunk waited %v; it should fill the buffer at once", d)
	}
	if d := sent[2].at.Sub(sent[0].at); d < chunk1/2 {
		t.Errorf("the third chunk went out after %v, before the first could have drained (%v)", d, chunk1)
	}
	if f.overrun != "" {
		t.Fatalf("%s", f.overrun)
	}
}

func TestCATRejectsCharactersTheRigCannotKey(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 24}
	s := newCAT(t, f, config.CW{DefaultWPM: 20})

	// ';' would terminate a Kenwood command, so it is not in the charset.
	n, err := s.Enqueue("CQ; DE", Append)
	var ce *CharError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want *CharError", err)
	}
	if ce.Char != ';' || ce.Offset != 2 {
		t.Errorf("got %q at %d, want ';' at 2", ce.Char, ce.Offset)
	}
	if ce.Charset != kenwoodCharset {
		t.Error("CharError should carry the accepted charset for the 422")
	}
	if n != 0 {
		t.Errorf("queued %d characters from rejected text", n)
	}

	// A stray caret is a client mistake too, and is named as one.
	if _, err := s.Enqueue("CQ ^ DE", Append); !errors.As(err, &ce) || ce.Char != '^' {
		t.Errorf("stray caret: got %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if f.sentCount() != 0 {
		t.Error("rejected text reached the radio")
	}
}

func TestCATEnqueueCountsSubmittedCharacters(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 0}
	s := newCAT(t, f, config.CW{DefaultWPM: 20})

	const text = "CQ TEST DE OH2XYZ ^AR"
	n, err := s.Enqueue(text, Append)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if n != len(text) {
		t.Errorf("queued %d, want %d", n, len(text))
	}
	if got := s.Status().Queued; got != len(text) {
		t.Errorf("status queued %d, want %d", got, len(text))
	}
}

func TestCATReplaceDropsWhatHasNotGoneToTheRadio(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 0}
	s := newCAT(t, f, config.CW{DefaultWPM: 20})

	if _, err := s.Enqueue("AAAA BBBB CCCC DDDD EEEE FFFF", Append); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue("QRL?", Replace); err != nil {
		t.Fatal(err)
	}
	if got, want := s.Status().Queued, 4; got != want {
		t.Errorf("queued %d, want %d", got, want)
	}
	f.setFree(24)
	waitFor(t, "the replacement text", 2*time.Second, func() bool { return f.sentCount() > 0 })
	if got := f.sentText(); got != "QRL?" {
		t.Errorf("the rig received %q, want QRL?", got)
	}
}

// TestCATAbortStopsBothHalves covers the reason Abort is not just a queue
// truncation: a full buffer may already be inside the radio.
func TestCATAbortStopsBothHalves(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 24}
	s := newCAT(t, f, config.CW{DefaultWPM: 20, ChunksInFlight: 1})

	if _, err := s.Enqueue("CQ CQ CQ DE OH2XYZ OH2XYZ OH2XYZ ^AR", Append); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the first chunk", time.Second, func() bool { return f.sentCount() > 0 })

	s.Abort()

	if got := f.abortCount(); got != 1 {
		t.Errorf("the radio was told to stop %d times, want 1", got)
	}
	st := s.Status()
	if st.Queued != 0 || st.Busy {
		t.Errorf("after abort: %+v, want an empty idle queue", st)
	}
	before := f.sentCount()
	time.Sleep(100 * time.Millisecond)
	if after := f.sentCount(); after != before {
		t.Errorf("%d more chunks went out after the abort", after-before)
	}

	// The sender is usable again afterwards.
	if _, err := s.Enqueue("R R", Append); err != nil {
		t.Fatalf("Enqueue after abort: %v", err)
	}
	waitFor(t, "sending to resume after an abort", 2*time.Second, func() bool { return f.sentCount() > before })
}

func TestCATStatusEstimatesFromElements(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 0}
	s := newCAT(t, f, config.CW{DefaultWPM: 20})

	// PARIS and its word gap is 50 dot units: three seconds at 20 wpm.
	if _, err := s.Enqueue("PARIS ", Append); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if st.WPM != 20 {
		t.Errorf("wpm %d, want 20", st.WPM)
	}
	if st.Queued != 6 {
		t.Errorf("queued %d, want 6", st.Queued)
	}
	if st.EstRemainingMS < 2900 || st.EstRemainingMS > 3100 {
		t.Errorf("est remaining %d ms, want about 3000", st.EstRemainingMS)
	}

	// An idle sender is not busy and has nothing left.
	s.Abort()
	if st := s.Status(); st.Busy || st.EstRemainingMS != 0 {
		t.Errorf("after abort: %+v", st)
	}
}

func TestCATSetSpeedClampsAndDelegates(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 24}
	s := newCAT(t, f, config.CW{DefaultWPM: 20})

	for _, c := range []struct{ ask, want int }{{25, 25}, {500, MaxWPM}, {1, MinWPM}, {0, DefaultWPM}} {
		if err := s.SetSpeed(c.ask); err != nil {
			t.Fatalf("SetSpeed(%d): %v", c.ask, err)
		}
		if got := s.Status().WPM; got != c.want {
			t.Errorf("SetSpeed(%d): status says %d, want %d", c.ask, got, c.want)
		}
		f.mu.Lock()
		rig := f.speed
		f.mu.Unlock()
		if rig != c.want {
			t.Errorf("SetSpeed(%d): the rig keyer was set to %d, want %d", c.ask, rig, c.want)
		}
	}
}

func TestNewCATRejectsUnusableBackends(t *testing.T) {
	if _, err := NewCAT(nil, fakeConn{}, config.CW{}); err == nil {
		t.Error("expected an error with no MorseSender")
	}
	if _, err := NewCAT(&fakeMorse{maxChunk: 24}, fakeConn{}, config.CW{}); err == nil {
		t.Error("expected an error with an empty charset")
	}
	if _, err := NewCAT(&fakeMorse{maxChunk: 2, charset: icomCharset}, fakeConn{}, config.CW{}); err == nil {
		t.Error("expected an error with a MaxChunk too small to carry a prosign")
	}
}

func TestCATChunksNeverExceedMaxChunk(t *testing.T) {
	f := &fakeMorse{maxChunk: 24, charset: kenwoodCharset, encode: encodeKenwood, freeOK: true, free: 24}
	s := newCAT(t, f, config.CW{DefaultWPM: 40, ChunksInFlight: 2})

	const text = "CQ CQ CQ DE OH2XYZ OH2XYZ OH2XYZ PSE K ^AR ANTIDISESTABLISHMENTARIANISM"
	if _, err := s.Enqueue(text, Append); err != nil {
		t.Fatal(err)
	}
	want, err := encodeKenwood(text)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the fake's buffer topped up so the whole message gets through. The
	// drain estimate keeps Status busy for the twenty seconds the rig would
	// really take, so the test watches what the rig received instead.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && f.sentText() != want {
		f.setFree(24)
		time.Sleep(2 * time.Millisecond)
	}
	if got := f.sentText(); got != want {
		t.Errorf("the rig received %q, want %q", got, want)
	}
	if f.overrun != "" {
		t.Fatalf("%s", f.overrun)
	}
}
