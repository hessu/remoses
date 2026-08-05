package rig

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// The fake port does not return from Write until the session's reader goroutine
// has consumed the reply AND come back for more data. Every Do in this file
// therefore exercises the register-before-write ordering: a waiter registered
// after the write would provably have missed its reply. This test states that
// explicitly.
func TestDoRegistersWaiterBeforeWriting(t *testing.T) {
	h := startedHarness(t, nil)

	up, err := h.s.Do(context.Background(), []byte("FA;"), "FA")
	if err != nil {
		t.Fatalf("Do: %v (a reply delivered before the waiter was registered would time out)", err)
	}
	if up.Key != "FA" {
		t.Errorf("reply key = %q, want FA", up.Key)
	}
	if up.Patch.Frequency == nil || *up.Patch.Frequency != 14025000 {
		t.Errorf("reply patch = %+v", up.Patch)
	}
}

func TestDoCorrelatesRepliesByKey(t *testing.T) {
	h := startedHarness(t, nil)
	ctx := context.Background()

	up, err := h.s.Do(ctx, []byte("MD;"), "MD")
	if err != nil {
		t.Fatalf("Do MD: %v", err)
	}
	if up.Key != "MD" || up.Patch.Mode == nil {
		t.Fatalf("MD reply = %+v", up)
	}

	// A request that accepts several keys is satisfied by any of them.
	up, err = h.s.Do(ctx, []byte("SM;"), "IF", "SM")
	if err != nil {
		t.Fatalf("Do SM: %v", err)
	}
	if up.Key != "SM" {
		t.Errorf("multi-key reply = %q, want SM", up.Key)
	}
}

func TestDoTimeoutIsTypedErrTimeout(t *testing.T) {
	h := startedHarness(t, nil)
	h.dev.setSilent("FA", true)

	start := time.Now()
	_, err := h.s.Do(context.Background(), []byte("FA;"), "FA")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Do against a silent rig = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond || d > 2*time.Second {
		t.Errorf("timed out after %s, want about the 150ms command timeout", d)
	}

	// The connection survives a timeout: the rig may simply have been busy.
	h.dev.setSilent("FA", false)
	if _, err := h.s.Do(context.Background(), []byte("FA;"), "FA"); err != nil {
		t.Fatalf("Do after a timeout: %v", err)
	}
}

func TestDoNegativeAcknowledgementIsAnError(t *testing.T) {
	h := startedHarness(t, nil)
	h.dev.setNAK("FL", true)

	_, err := h.s.Do(context.Background(), []byte("FL2;"), "FL")
	if !errors.Is(err, ErrNAK) {
		t.Fatalf("Do against a rejecting rig = %v, want ErrNAK", err)
	}
	// And it surfaces through the setter, so the API does not report success.
	if _, err := h.s.SetFilterSlot(context.Background(), 2); !errors.Is(err, ErrNAK) {
		t.Fatalf("SetFilterSlot = %v, want ErrNAK", err)
	}
}

func TestDoWithoutKeysIsARejectedProgrammingError(t *testing.T) {
	h := startedHarness(t, nil)
	if _, err := h.s.Do(context.Background(), []byte("FA;")); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("Do with no keys = %v, want ErrNoKeys", err)
	}
}

func TestDoRespectsContextCancellation(t *testing.T) {
	h := startedHarness(t, nil)
	h.dev.setSilent("FA", true)
	defer h.dev.setSilent("FA", false)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := h.s.Do(ctx, []byte("FA;"), "FA")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Do with a cancelled context = %v, want context.Canceled", err)
	}
}

func TestConcurrentCommandsAreSerialised(t *testing.T) {
	h := startedHarness(t, nil)
	port := h.dl.lastPort()

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 4 {
				hz := uint64(14000000 + i*1000 + j)
				if _, err := h.s.SetFrequency(context.Background(), 0, hz); err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent SetFrequency: %v", err)
	}

	if got := port.maxInFlight.Load(); got != 1 {
		t.Fatalf("%d concurrent writes reached the port, want 1: commands are not serialised", got)
	}
}

func TestSendDoesNotWaitForAReply(t *testing.T) {
	h := startedHarness(t, nil)

	// PT sets are silent on the fake rig, as Kenwood TX;/RX; are. Send must
	// return promptly rather than stalling until the command timeout.
	start := time.Now()
	if err := h.s.Send(context.Background(), []byte("PT1;")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Send took %s: it waited for a reply", d)
	}
	if _, _, ptt, _ := h.dev.snapshot(); !ptt {
		t.Error("Send did not reach the radio")
	}
}

func TestConnDoAfterDisconnectReturnsDisconnected(t *testing.T) {
	h := startedHarness(t, nil)

	c := h.s.conn.Load()
	if c == nil {
		t.Fatal("no live connection")
	}
	c.close(transport.ErrDisconnected)

	_, err := c.Do(context.Background(), []byte("FA;"), "FA")
	if !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Do on a dead connection = %v, want ErrDisconnected", err)
	}
	if err := c.Send(context.Background(), []byte("FA;")); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Send on a dead connection = %v, want ErrDisconnected", err)
	}
}

func TestSessionConnFacadeWithoutConnection(t *testing.T) {
	h := newHarness(t, nil) // never started
	if _, err := h.s.Do(context.Background(), []byte("FA;"), "FA"); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Do without a connection = %v, want ErrDisconnected", err)
	}
	if err := h.s.Send(context.Background(), []byte("FA;")); !errors.Is(err, ErrDisconnected) {
		t.Errorf("Send without a connection = %v, want ErrDisconnected", err)
	}
}

func TestWaiterMatchesKeys(t *testing.T) {
	w := &waiter{keys: []backend.Key{"FA", "IF"}}
	if !w.wants("IF") || !w.wants("FA") {
		t.Error("waiter does not match its own keys")
	}
	if w.wants("MD") || w.wants(backend.KeyUnsolicited) {
		t.Error("waiter matched a key it did not ask for")
	}
}
