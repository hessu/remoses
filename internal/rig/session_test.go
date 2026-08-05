package rig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

type harness struct {
	s   *Session
	rig *fakeRig
	dev *fakeDevice
	dl  *fakeDialer
}

// newHarness builds an unstarted session wired to the fakes.
func newHarness(t *testing.T, mutate func(*config.Radio)) *harness {
	t.Helper()
	dev := newFakeDevice()
	dl := newFakeDialer(dev)
	r := newFakeRig()

	rc := config.Radio{
		ID:      "testrig",
		Name:    "Test Rig",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(100 * time.Millisecond),
		},
	}
	if mutate != nil {
		mutate(&rc)
	}

	s, err := NewSession(rc, r, dl,
		WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond),
		WithEventQueue(256))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond

	return &harness{s: s, rig: r, dev: dev, dl: dl}
}

// start brings the session up and waits until the radio is usable.
func (h *harness) start(t *testing.T) *harness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = h.s.Close()
	})
	h.s.Start(ctx)
	waitFor(t, "radio to connect", h.s.Connected)
	return h
}

func startedHarness(t *testing.T, mutate func(*config.Radio)) *harness {
	t.Helper()
	return newHarness(t, mutate).start(t)
}

func TestSessionConnectsRunsInitAndFillsState(t *testing.T) {
	h := startedHarness(t, nil)

	if got := h.rig.inits.Load(); got != 1 {
		t.Fatalf("Init called %d times, want 1", got)
	}
	st := h.s.State()
	if !st.Connected {
		t.Fatal("state reports disconnected after connect")
	}
	if st.Frequency != 14025000 {
		t.Errorf("frequency = %d, want 14025000", st.Frequency)
	}
	if st.Mode != radio.ModeCW {
		t.Errorf("mode = %s, want CW", st.Mode)
	}
	if st.Power.Pct != 50 {
		t.Errorf("power pct = %v, want 50", st.Power.Pct)
	}
	if st.SMeter.Raw != 12 || st.SMeter.Scale != 30 {
		t.Errorf("s-meter = %+v, want raw 12 scale 30", st.SMeter)
	}
	if h.s.ID() != "testrig" || h.s.Name() != "Test Rig" || h.s.Backend() != "fake" {
		t.Errorf("descriptor = %q/%q/%q", h.s.ID(), h.s.Name(), h.s.Backend())
	}
	if len(h.s.Caps().Modes) == 0 {
		t.Error("caps not published after Init")
	}
}

func TestSetFrequencyReadsBackFromRig(t *testing.T) {
	h := startedHarness(t, nil)

	st, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 7010000)
	if err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	if st.Frequency != 7010000 {
		t.Errorf("returned state frequency = %d, want 7010000", st.Frequency)
	}
	if f, _, _, _ := h.dev.snapshot(); f != 7010000 {
		t.Errorf("radio frequency = %d, want 7010000", f)
	}
}

func TestOutOfBandRejected(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.Bands = []config.Band{{LowHz: 14000000, HighHz: 14350000}}
	})

	before, _, _, _ := h.dev.snapshot()
	_, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 7010000)
	if !errors.Is(err, ErrOutOfBand) {
		t.Fatalf("SetFrequency out of band: err = %v, want ErrOutOfBand", err)
	}
	if after, _, _, _ := h.dev.snapshot(); after != before {
		t.Errorf("radio was retuned to %d despite band rejection", after)
	}

	if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 14200000); err != nil {
		t.Fatalf("in-band SetFrequency: %v", err)
	}
}

func TestPowerClampedAndReportedClamped(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.MaxPowerPct = 80
	})

	pct := 100.0
	st, err := h.s.SetPower(context.Background(), radio.PowerSet{Pct: &pct})
	if err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	if st.Power.Pct != 80 {
		t.Errorf("reported power = %v%%, want the clamped 80%%", st.Power.Pct)
	}
	if _, _, _, p := h.dev.snapshot(); p != 80 {
		t.Errorf("radio power = %d, want 80", p)
	}
}

func TestPowerClampedInWatts(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.MaxPowerW = 40
	})

	w := 100.0
	st, err := h.s.SetPower(context.Background(), radio.PowerSet{Watts: &w})
	if err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	if st.Power.Native != 40 {
		t.Errorf("radio native power = %d, want 40", st.Power.Native)
	}
}

func TestPowerRequestValidated(t *testing.T) {
	h := startedHarness(t, nil)
	if _, err := h.s.SetPower(context.Background(), radio.PowerSet{}); err == nil {
		t.Error("SetPower with neither watts nor pct: want error")
	}
	w, p := 10.0, 10.0
	if _, err := h.s.SetPower(context.Background(), radio.PowerSet{Watts: &w, Pct: &p}); err == nil {
		t.Error("SetPower with both watts and pct: want error")
	}
}

func TestUnsupportedCapabilitiesRejected(t *testing.T) {
	h := startedHarness(t, nil)
	ctx := context.Background()

	if _, err := h.s.SetMode(ctx, radio.ModeAM, false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetMode(AM) = %v, want ErrUnsupported", err)
	}
	if _, err := h.s.SetFilterSlot(ctx, 4); !errors.Is(err, ErrUnsupported) {
		t.Errorf("SetFilterSlot(4) = %v, want ErrUnsupported", err)
	}
	if _, err := h.s.SetFilterSlot(ctx, 2); err != nil {
		t.Errorf("SetFilterSlot(2): %v", err)
	}
}

func TestApplyPatchSetsModeBeforeFrequency(t *testing.T) {
	h := startedHarness(t, nil)

	mode := radio.ModeUSB
	freq := uint64(14200000)
	slot := 2
	pct := 30.0
	ptt := false
	st, err := h.s.ApplyPatch(context.Background(), PatchRequest{
		Mode:       &mode,
		Frequency:  &freq,
		FilterSlot: &slot,
		Power:      &radio.PowerSet{Pct: &pct},
		PTT:        &ptt,
	})
	if err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	log := h.rig.setLog()
	var order []string
	for _, e := range log {
		switch {
		case len(e) > 4 && e[:4] == "mode":
			order = append(order, "mode")
		case len(e) > 4 && e[:4] == "freq":
			order = append(order, "freq")
		case len(e) > 4 && e[:4] == "slot":
			order = append(order, "slot")
		}
	}
	if len(order) < 2 || order[0] != "mode" || order[1] != "freq" {
		t.Fatalf("command order = %v (full log %v), want mode before frequency", order, log)
	}

	if st.Mode != radio.ModeUSB || st.Frequency != 14200000 || st.FilterSlot != 2 || st.Power.Pct != 30 {
		t.Errorf("state after patch = %+v", st)
	}
}

func TestApplyPatchValidatesBeforeWriting(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.Bands = []config.Band{{LowHz: 14000000, HighHz: 14350000}}
	})

	mode := radio.ModeUSB
	freq := uint64(7000000)
	_, err := h.s.ApplyPatch(context.Background(), PatchRequest{Mode: &mode, Frequency: &freq})
	if !errors.Is(err, ErrOutOfBand) {
		t.Fatalf("ApplyPatch = %v, want ErrOutOfBand", err)
	}
	// The whole request must be rejected before anything reaches the rig,
	// otherwise the mode would have changed while the frequency did not.
	if st := h.s.State(); st.Mode != radio.ModeCW {
		t.Errorf("mode changed to %s despite a rejected patch", st.Mode)
	}
	for _, e := range h.rig.setLog() {
		if len(e) > 4 && e[:4] == "mode" {
			t.Fatalf("mode was written despite a rejected patch: %v", h.rig.setLog())
		}
	}
}

func TestUnsolicitedFrameUpdatesStateWithoutWaiter(t *testing.T) {
	h := startedHarness(t, nil)

	// No poll or command asked for this: it is a Transceive/AI style push.
	h.dl.lastPort().push("UF00007123456")
	waitFor(t, "unsolicited frequency", func() bool {
		return h.s.State().Frequency == 7123456
	})
}

func TestSeqIsMonotonicAndOnlyAdvancesOnChange(t *testing.T) {
	h := startedHarness(t, nil)

	var (
		mu   sync.Mutex
		seqs []uint64
	)
	ch, unsub := h.s.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			mu.Lock()
			seqs = append(seqs, ev.Seq)
			mu.Unlock()
		}
	}()

	for i := range 20 {
		h.dl.lastPort().push("UF0000700000" + string(rune('0'+i%10)))
		time.Sleep(time.Millisecond)
	}
	waitFor(t, "events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seqs) >= 5
	})
	unsub()
	<-done

	mu.Lock()
	defer mu.Unlock()
	var last uint64
	for i, s := range seqs {
		if i > 0 && s <= last {
			t.Fatalf("seq went backwards or repeated: %v", seqs)
		}
		last = s
	}
	if h.s.State().Seq < last {
		t.Fatalf("state seq %d behind published seq %d", h.s.State().Seq, last)
	}
}

func TestStalledSubscriberDoesNotBlockSession(t *testing.T) {
	h := newHarness(t, nil)
	// A queue of one makes the stall immediate.
	h.s.subs = newSubscribers(1)
	h.start(t)

	stalled, unsubStalled := h.s.Subscribe()
	defer unsubStalled()
	live, unsubLive := h.s.Subscribe()
	defer unsubLive()

	// Nobody reads `stalled`. The session must keep serving commands.
	for i := range 50 {
		hz := uint64(14000000 + i*100)
		if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, hz); err != nil {
			t.Fatalf("SetFrequency %d with a stalled subscriber: %v", hz, err)
		}
	}
	if f, _, _, _ := h.dev.snapshot(); f != 14004900 {
		t.Fatalf("radio frequency = %d, want 14004900: commands were lost", f)
	}

	// The stalled subscriber must be told how much it missed rather than
	// silently seeing a gap.
	drainForDrop(t, stalled, func() {
		for i := range 5 {
			if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, uint64(14100000+i*100)); err != nil {
				t.Errorf("SetFrequency: %v", err)
			}
		}
	})

	// The attentive subscriber is unaffected.
	select {
	case ev := <-live:
		if ev.RadioID != "testrig" {
			t.Errorf("event radio = %q", ev.RadioID)
		}
	default:
		t.Error("attentive subscriber got nothing")
	}
}

func TestUnsubscribeClosesChannel(t *testing.T) {
	h := startedHarness(t, nil)
	ch, unsub := h.s.Subscribe()
	unsub()
	unsub() // idempotent
	select {
	case _, ok := <-ch:
		if ok {
			// A buffered event may still be there; drain until closed.
			for range ch {
			}
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed by unsubscribe")
	}
	if got := h.s.subs.count(); got != 0 {
		t.Errorf("subscriber count = %d after unsubscribe, want 0", got)
	}
}

func TestDeadManTimerForcesReceive(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.TXTimeout = config.Duration(60 * time.Millisecond)
	})
	sender := newFakeCW()
	h.s.SetCWSender(sender)

	if _, err := h.s.SetPTT(context.Background(), true); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	if _, _, ptt, _ := h.dev.snapshot(); !ptt {
		t.Fatal("radio did not key")
	}

	waitFor(t, "dead-man timer to drop PTT", func() bool {
		_, _, ptt, _ := h.dev.snapshot()
		return !ptt
	})
	waitFor(t, "CW abort", func() bool { return sender.abortCount() > 0 })
	waitFor(t, "state to show receive", func() bool { return !h.s.State().PTT })
}

func TestDeadManDisarmedOnManualUnkey(t *testing.T) {
	h := startedHarness(t, func(rc *config.Radio) {
		rc.Limits.TXTimeout = config.Duration(200 * time.Millisecond)
	})
	sender := newFakeCW()
	h.s.SetCWSender(sender)

	ctx := context.Background()
	if _, err := h.s.SetPTT(ctx, true); err != nil {
		t.Fatalf("SetPTT(true): %v", err)
	}
	if _, err := h.s.SetPTT(ctx, false); err != nil {
		t.Fatalf("SetPTT(false): %v", err)
	}
	waitFor(t, "state to show receive", func() bool { return !h.s.State().PTT })

	time.Sleep(300 * time.Millisecond)
	if n := sender.abortCount(); n != 0 {
		t.Errorf("dead-man fired after a manual unkey: %d aborts", n)
	}
}

func TestForceRXWhenDisconnected(t *testing.T) {
	h := newHarness(t, nil)
	sender := newFakeCW()
	h.s.SetCWSender(sender)

	// Never started: no connection at all. ForceRX must still flush CW and
	// must not panic or block.
	h.s.ForceRX("test")
	if sender.abortCount() != 1 {
		t.Errorf("aborts = %d, want 1", sender.abortCount())
	}
}

func TestCWStatusPublished(t *testing.T) {
	h := startedHarness(t, nil)
	sender := newFakeCW()
	h.s.SetCWSender(sender)

	ch, unsub := h.s.Subscribe()
	defer unsub()

	sender.setStatus(radio.CWStatus{Busy: true, Queued: 12, WPM: 28})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == EventCW && ev.State.CW.Queued == 12 {
				if ev.State.CW.WPM != 28 {
					t.Errorf("cw wpm = %d, want 28", ev.State.CW.WPM)
				}
				return
			}
		case <-deadline:
			t.Fatal("no CW event published")
		}
	}
}

func TestCommandsRejectedWhenDisconnected(t *testing.T) {
	h := newHarness(t, nil) // not started
	ctx := context.Background()

	checks := []struct {
		name string
		err  error
	}{
		{"SetFrequency", second(h.s.SetFrequency(ctx, radio.VFOCurrent, 14000000))},
		{"SetMode", second(h.s.SetMode(ctx, radio.ModeCW, false))},
		{"SetPTT", second(h.s.SetPTT(ctx, true))},
		{"SetFilterWidth", second(h.s.SetFilterWidth(ctx, 500))},
		{"SetFilterSlot", second(h.s.SetFilterSlot(ctx, 1))},
		{"ApplyPatch", second(h.s.ApplyPatch(ctx, PatchRequest{PTT: ptrTo(true)}))},
	}
	for _, c := range checks {
		if !errors.Is(c.err, ErrDisconnected) {
			t.Errorf("%s while disconnected = %v, want ErrDisconnected", c.name, c.err)
		}
	}
}

func second(_ radio.State, err error) error { return err }

// drainForDrop churns state until the subscriber is handed an event carrying a
// Dropped marker. The marker rides on the first event that fits AFTER the
// overflow, so a single round of churn is not enough to observe it.
func drainForDrop(t *testing.T, ch <-chan Event, churn func()) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		churn()
		for drained := false; !drained; {
			select {
			case ev := <-ch:
				if ev.Dropped > 0 {
					return
				}
			default:
				drained = true
			}
		}
	}
	t.Fatal("subscriber was never told it had missed events")
}

func TestPatchRequestEmpty(t *testing.T) {
	if !(PatchRequest{}).Empty() {
		t.Error("zero PatchRequest is not empty")
	}
	if (PatchRequest{PTT: ptrTo(false)}).Empty() {
		t.Error("PatchRequest with PTT reported empty")
	}
}

func TestStateSnapshotIsACopy(t *testing.T) {
	h := startedHarness(t, nil)
	a := h.s.State()
	a.Frequency = 1
	if b := h.s.State(); b.Frequency == 1 {
		t.Fatal("State() handed out a mutable reference to the cache")
	}
}
