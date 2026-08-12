package selftest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// fakeRadio is a rig that does as it is told and remembers what it was told to
// do, which is all this package needs to test its own sequencing and — the part
// that matters — that it puts the radio back.
type fakeRadio struct {
	caps  radio.Caps
	state radio.State

	patches   []rig.PatchRequest
	failAfter int  // return an error once this many patches have been applied
	connected bool // reported by Connected
	poweredOn bool
}

func newFakeRadio() *fakeRadio {
	return &fakeRadio{
		connected: true,
		poweredOn: true,
		caps: radio.Caps{
			Modes:        []radio.Mode{radio.ModeUSB, radio.ModeCW},
			VFOs:         []radio.VFO{radio.VFOCurrent},
			PTTControl:   true,
			PowerControl: true,
			FilterWidth:  true,
			SMeterScale:  255,
			CWMethod:     radio.CWNone,
		},
		state: radio.State{
			Frequency:  14_025_000,
			Mode:       radio.ModeCW,
			PassbandHz: 500,
			Power:      radio.Power{Pct: 35},
			Connected:  true,
		},
	}
}

func (f *fakeRadio) ID() string         { return "fake" }
func (f *fakeRadio) Name() string       { return "Fake Radio" }
func (f *fakeRadio) Backend() string    { return "fake" }
func (f *fakeRadio) Caps() radio.Caps   { return f.caps }
func (f *fakeRadio) State() radio.State { return f.state }
func (f *fakeRadio) Connected() bool    { return f.connected }
func (f *fakeRadio) CW() cw.Sender      { return nil }

func (f *fakeRadio) ApplyPatch(_ context.Context, req rig.PatchRequest) (radio.State, error) {
	f.patches = append(f.patches, req)
	if f.failAfter > 0 && len(f.patches) > f.failAfter {
		return f.state, fmt.Errorf("fake: the radio stopped answering: %w", rig.ErrDisconnected)
	}
	if req.Frequency != nil {
		f.state.Frequency = *req.Frequency
	}
	if req.Mode != nil {
		f.state.Mode = *req.Mode
	}
	if req.DataMode != nil {
		f.state.DataMode = *req.DataMode
	}
	if req.FilterWidthHz != nil {
		f.state.PassbandHz = *req.FilterWidthHz
	}
	if req.Power != nil && req.Power.Pct != nil {
		f.state.Power.Pct = *req.Power.Pct
	}
	if req.PTT != nil {
		f.state.PTT = *req.PTT
	}
	return f.state, nil
}

func (f *fakeRadio) PowerOff(context.Context, bool) (radio.State, error) {
	f.poweredOn = false
	f.state.Standby = true
	return f.state, nil
}

func (f *fakeRadio) PowerOn(context.Context) (radio.State, error) {
	f.poweredOn = true
	f.state.Standby = false
	return f.state, nil
}

// run drives a report and hands back the parsed records.
func run(t *testing.T, r Session, opts Options) (Header, []Step, Summary) {
	t.Helper()
	_, cp := NewCapture(slog.NewTextHandler(io.Discard, nil))
	var buf bytes.Buffer
	sum, err := Run(context.Background(), r, cp, opts, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	h, steps := parse(t, &buf)
	return h, steps, sum
}

func parse(t *testing.T, buf *bytes.Buffer) (Header, []Step) {
	t.Helper()
	var h Header
	var steps []Step
	sc := bufio.NewScanner(buf)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<22)
	for sc.Scan() {
		line := sc.Bytes()
		var probe struct {
			Record string `json:"record"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("line is not JSON: %v: %s", err, line)
		}
		switch probe.Record {
		case "header":
			if err := json.Unmarshal(line, &h); err != nil {
				t.Fatalf("header: %v", err)
			}
		case "step":
			var s Step
			if err := json.Unmarshal(line, &s); err != nil {
				t.Fatalf("step: %v", err)
			}
			steps = append(steps, s)
		}
	}
	return h, steps
}

func find(steps []Step, group, name string) (Step, bool) {
	for _, s := range steps {
		if s.Group == group && s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// Every line has to be a self-contained JSON object, because the file is meant
// to be read by a program somebody else wrote — including one that streams it.
func TestReportIsWellFormedJSONLines(t *testing.T) {
	h, steps, sum := run(t, newFakeRadio(), Options{Version: "test"})

	if h.FormatVersion != FormatVersion {
		t.Errorf("format_version = %d, want %d", h.FormatVersion, FormatVersion)
	}
	if h.RadioID != "fake" || h.Backend != "fake" {
		t.Errorf("header identifies the radio as %q/%q", h.RadioID, h.Backend)
	}
	if len(steps) == 0 {
		t.Fatal("no steps were recorded")
	}
	if sum.Counts[Pass] == 0 {
		t.Error("nothing passed against a radio that does as it is told")
	}
	for i, s := range steps {
		if s.Seq != i+1 {
			t.Errorf("step %d carries seq %d; the numbering has to be dense so a truncated file is obvious", i, s.Seq)
		}
		if s.Group == "" || s.Name == "" || s.Verdict == "" {
			t.Errorf("step %d is missing an identifying field: %+v", i, s)
		}
	}
}

// The promise that makes it reasonable to ask a stranger to run this.
func TestRadioIsRestoredAfterAGoodRun(t *testing.T) {
	r := newFakeRadio()
	before := r.state

	_, steps, sum := run(t, r, Options{Version: "test"})

	if !sum.Restored {
		t.Fatalf("summary says the radio was not restored: %s", sum.RestoreErr)
	}
	if r.state.Frequency != before.Frequency {
		t.Errorf("frequency left at %d, want %d", r.state.Frequency, before.Frequency)
	}
	if r.state.Mode != before.Mode {
		t.Errorf("mode left at %s, want %s", r.state.Mode, before.Mode)
	}
	if r.state.PassbandHz != before.PassbandHz {
		t.Errorf("passband left at %d, want %d", r.state.PassbandHz, before.PassbandHz)
	}
	if r.state.Power.Pct != before.Power.Pct {
		t.Errorf("power left at %.0f%%, want %.0f%%", r.state.Power.Pct, before.Power.Pct)
	}
	if _, ok := find(steps, "restore", "as-found"); !ok {
		t.Error("the restore is not recorded in the report; a reader cannot tell it happened")
	}
}

// And the same promise when the run falls over halfway, which is when it
// matters: a radio abandoned mid-sequence is somebody's station left wrong.
func TestRadioIsRestoredAfterAFailedRun(t *testing.T) {
	r := newFakeRadio()
	before := r.state
	r.failAfter = 3

	_, _, sum := run(t, r, Options{Version: "test"})

	// The restore patch is applied last and the fake is still refusing, so the
	// summary must say so rather than claim a restore that did not happen.
	if sum.Restored {
		t.Error("the summary claims a restore on a radio that was refusing every command")
	}
	if sum.RestoreErr == "" {
		t.Error("no reason recorded for the failed restore")
	}
	// Whatever happened, it must have tried.
	if len(r.patches) == 0 {
		t.Fatal("nothing was ever sent")
	}
	last := r.patches[len(r.patches)-1]
	if last.Frequency == nil || *last.Frequency != before.Frequency {
		t.Errorf("the last thing sent was not a restore to %d: %+v", before.Frequency, last)
	}
}

// An interrupted run still restores. Ctrl-C is the likeliest way a nervous
// operator stops this, and it must not be the way the radio gets left wrong.
func TestInterruptedRunStillRestores(t *testing.T) {
	r := newFakeRadio()
	before := r.state
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, cp := NewCapture(slog.NewTextHandler(io.Discard, nil))
	var buf bytes.Buffer
	sum, err := Run(ctx, r, cp, Options{Version: "test"}, &buf)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sum.Interrupted {
		t.Error("the summary does not record that the run was interrupted")
	}
	if !sum.Restored {
		t.Errorf("an interrupted run left the radio unrestored: %s", sum.RestoreErr)
	}
	if r.state.Frequency != before.Frequency {
		t.Errorf("frequency left at %d, want %d", r.state.Frequency, before.Frequency)
	}
}

// The default has to be silent on the air. This is the check that a mistake in
// the sequencing cannot turn an ordinary run into a transmission.
func TestNothingTransmitsWithoutATXFrequency(t *testing.T) {
	r := newFakeRadio()
	_, steps, _ := run(t, r, Options{Version: "test"})

	for _, p := range r.patches {
		if p.PTT != nil && *p.PTT {
			t.Fatal("the radio was keyed without -tx-freq")
		}
		if p.TunerTune != nil {
			t.Fatal("a tuning cycle was started without -tx-freq; it transmits")
		}
	}
	s, ok := find(steps, "transmit", "all")
	if !ok || s.Verdict != Skipped {
		t.Errorf("the skipped transmit tests are not recorded as such: %+v", s)
	}
	if !strings.Contains(s.Note, "tx-freq") {
		t.Errorf("the skip does not say how to enable it: %q", s.Note)
	}
}

func TestTransmitRunsOnlyWhenAuthorised(t *testing.T) {
	r := newFakeRadio()
	_, steps, _ := run(t, r, Options{Version: "test", TXFrequency: 14_030_000, TXPowerPct: 10})

	keyed := false
	for _, p := range r.patches {
		if p.PTT != nil && *p.PTT {
			keyed = true
		}
	}
	if !keyed {
		t.Error("the radio was never keyed despite -tx-freq")
	}
	if s, ok := find(steps, "transmit", "ptt-keyed"); !ok || s.Verdict != Pass {
		t.Errorf("ptt-keyed = %+v", s)
	}
	// And it must have gone to the frequency it was told to, not wherever the
	// radio happened to be.
	if s, ok := find(steps, "transmit", "setup"); !ok || s.Got != "14030000" {
		t.Errorf("transmit setup landed on %q, want 14030000", s.Got)
	}
}

// The power switch is the one test that can leave an operator with a radio they
// have to walk to, so it stays off unless asked for by name.
func TestPowerSwitchNeedsItsOwnFlag(t *testing.T) {
	r := newFakeRadio()
	r.caps.PowerSwitch = true

	_, steps, _ := run(t, r, Options{Version: "test", TXFrequency: 14_030_000})
	if !r.poweredOn {
		t.Fatal("the radio was switched off without -test-power-switch")
	}
	if s, ok := find(steps, "power-switch", "off-and-on"); !ok || s.Verdict != Skipped {
		t.Errorf("power-switch = %+v", s)
	}

	r2 := newFakeRadio()
	r2.caps.PowerSwitch = true
	_, steps2, _ := run(t, r2, Options{Version: "test", PowerSwitch: true})
	if s, ok := find(steps2, "power-switch", "off"); !ok || s.Verdict == Skipped {
		t.Errorf("power-switch off = %+v, want it to have run", s)
	}
	if !r2.poweredOn {
		t.Error("the radio was left switched off")
	}
}

// A capability the radio denies, and then accepts anyway, is the worst class of
// bug this project has met: a request that succeeds and means something else.
func TestDeniedControlAcceptedIsAFailure(t *testing.T) {
	r := newFakeRadio() // no split in caps, and ApplyPatch accepts everything
	_, steps, sum := run(t, r, Options{Version: "test"})

	s, ok := find(steps, "denied", "split")
	if !ok {
		t.Fatal("the denied-split check never ran")
	}
	if s.Verdict != Fail {
		t.Errorf("a control the radio denies was accepted and scored %q, want fail", s.Verdict)
	}
	if sum.Counts[Fail] == 0 || len(sum.Failures) == 0 {
		t.Error("the failure is missing from the summary")
	}
}

// And the same check passing, on a radio that refuses properly.
func TestDeniedControlRefusedIsARefusal(t *testing.T) {
	r := &refusingRadio{fakeRadio: newFakeRadio()}
	_, steps, _ := run(t, r, Options{Version: "test"})

	s, ok := find(steps, "denied", "split")
	if !ok {
		t.Fatal("the denied-split check never ran")
	}
	if s.Verdict != Refused {
		t.Errorf("a properly refused control scored %q, want refused", s.Verdict)
	}
}

// refusingRadio refuses anything the capabilities do not advertise, which is
// what a correct backend does.
type refusingRadio struct{ *fakeRadio }

func (f *refusingRadio) ApplyPatch(ctx context.Context, req rig.PatchRequest) (radio.State, error) {
	if req.Split != nil || req.DualWatch != nil || req.ExchangeBands != nil ||
		req.Tuner != nil || req.TunerTune != nil || req.Preamp != nil || req.BreakIn != nil ||
		req.VFO != radio.VFOCurrent {
		return f.state, fmt.Errorf("fake: no such control: %w", rig.ErrUnsupported)
	}
	return f.fakeRadio.ApplyPatch(ctx, req)
}

// A value the radio accepts and then does not report back is the failure this
// project has met more often than any other.
func TestAcceptedButUnmovedIsAFailure(t *testing.T) {
	r := &deafRadio{fakeRadio: newFakeRadio()}
	_, steps, _ := run(t, r, Options{Version: "test"})

	var sawFail bool
	for _, s := range steps {
		if s.Group == "filter" && strings.HasPrefix(s.Name, "width-") && s.Verdict == Fail {
			sawFail = true
			if !strings.Contains(s.Note, "never read back") {
				t.Errorf("the note does not explain the failure: %q", s.Note)
			}
		}
	}
	if !sawFail {
		t.Error("a filter width written and never read back was not reported as a failure")
	}
}

// deafRadio accepts a filter width and never reports it, exactly as a backend
// that writes a value it never reads does.
type deafRadio struct{ *fakeRadio }

func (f *deafRadio) ApplyPatch(ctx context.Context, req rig.PatchRequest) (radio.State, error) {
	req.FilterWidthHz = nil
	return f.fakeRadio.ApplyPatch(ctx, req)
}

// WaitConnected must not spin for ever on a radio that never answers.
func TestWaitConnectedGivesUp(t *testing.T) {
	r := newFakeRadio()
	r.connected = false
	err := WaitConnected(context.Background(), r, 150*time.Millisecond)
	if err == nil {
		t.Fatal("WaitConnected returned nil for a radio that never connected")
	}
	if !strings.Contains(err.Error(), "did not connect") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestWaitConnectedHonoursCancellation(t *testing.T) {
	r := newFakeRadio()
	r.connected = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WaitConnected(ctx, r, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}
