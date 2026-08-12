package selftest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"time"

	"github.com/hessu/remoses/internal/cw"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// Options is what the operator authorised, and nothing here goes beyond it.
type Options struct {
	// TXFrequency is the one switch that lets this run transmit. Zero means it
	// never keys the radio, and that is the default for the obvious reason:
	// this is meant to be run by people whose antennas, licences and neighbours
	// are none of remoses's business.
	//
	// Given, it authorises the whole transmit surface — a keyed carrier, the
	// transmit meters, a short CW message and, where the radio has one, a tuner
	// cycle. They are one permission rather than four because they are one
	// decision: either it is safe to put this radio on the air on this
	// frequency for a few seconds, or it is not.
	TXFrequency uint64

	// TXPowerPct caps the transmit tests. It is a percentage even on radios
	// calibrated in watts, because the only thing this needs is "low", and a
	// percentage is the one unit every radio here can express.
	TXPowerPct int

	// PowerSwitch authorises switching the radio off and waking it again.
	// Separate from TXFrequency because it fails differently: a wake that does
	// not work leaves somebody with a radio they have to walk to, which is a
	// worse outcome than a transmission that did not happen.
	PowerSwitch bool

	// CWText is what a transmit run sends. It defaults to something short and
	// identifiable; an operator should set their own callsign.
	CWText string

	// OperatorNotes is free text copied into the header — the rig, the
	// interface, anything the person running it thinks a reader will want.
	OperatorNotes string

	// Version of the remoses build, and Model as the configuration named it.
	// The model matters more than it looks: on several backends it is the only
	// thing that decides which command table was used, and a report against the
	// wrong one explains itself instantly with this in the header.
	Version string
	Model   string

	// Transport describes how the radio is reached — the device and line
	// settings, or the address. It is in the header because it is the first
	// thing anybody asks about a report that shows nothing working, and because
	// several of the bugs found so far were a port opened the wrong way rather
	// than a protocol misread.
	Transport string

	// Progress receives a line per step for the terminal. Optional.
	Progress func(string)
}

// DefaultCWText is deliberately not a callsign: sending somebody else's
// callsign is not this program's to do, and "TEST" is what an unattended check
// should say if the operator did not think about it.
const DefaultCWText = "TEST TEST DE REMOSES"

// Runner drives one radio through everything it claims to do.
type Runner struct {
	s    Session
	opts Options
	cap  *capture
	w    *writer

	// initial is the state as found, and the whole of the restore plan. It is
	// captured before anything is written and put back at the end, whatever
	// happened in between.
	initial radio.State
	caps    radio.Caps

	started time.Time
}

// Session is the part of *rig.Session this package uses. It is an interface so
// the run can be tested against a fake radio without a serial port, which is
// the only way the sequencing and the restore logic get any coverage at all.
type Session interface {
	ID() string
	Name() string
	Backend() string
	Caps() radio.Caps
	State() radio.State
	Connected() bool
	ApplyPatch(ctx context.Context, req rig.PatchRequest) (radio.State, error)
	PowerOff(ctx context.Context, deep bool) (radio.State, error)
	PowerOn(ctx context.Context) (radio.State, error)

	// CW returns the attached keyer, or nil where the configuration enabled
	// none. cw.Sender whole rather than a narrower interface of this package's
	// own, because Go matches method signatures exactly and *rig.Session has to
	// satisfy this without an adapter.
	CW() cw.Sender
}

// Run exercises the radio and writes the report to out.
//
// It returns an error only when the run could not be carried out — the radio
// never connected, the report could not be written. A radio that failed steps
// is a successful run with failures in it, because the file is the product and
// a non-zero exit that suppressed it would be exactly backwards.
func Run(ctx context.Context, s Session, cp *capture, opts Options, out io.Writer) (Summary, error) {
	if opts.CWText == "" {
		opts.CWText = DefaultCWText
	}
	if opts.TXPowerPct <= 0 {
		opts.TXPowerPct = 10
	}

	r := &Runner{
		s:       s,
		opts:    opts,
		cap:     cp,
		w:       newWriter(out),
		caps:    s.Caps(),
		initial: s.State(),
		started: time.Now(),
	}

	if err := r.w.header(Header{
		Remoses:      opts.Version,
		Go:           runtime.Version(),
		OS:           runtime.GOOS,
		StartedAt:    r.started,
		RadioID:      s.ID(),
		RadioName:    s.Name(),
		Backend:      s.Backend(),
		Model:        opts.Model,
		Transport:    opts.Transport,
		Caps:         r.caps,
		InitialState: r.initial,
		Authorised: Authorised{
			Transmit:      opts.TXFrequency > 0,
			TXFrequency:   opts.TXFrequency,
			TXPowerPct:    opts.TXPowerPct,
			PowerSwitch:   opts.PowerSwitch,
			CWText:        opts.CWText,
			OperatorNotes: opts.OperatorNotes,
		},
	}); err != nil {
		return Summary{}, fmt.Errorf("selftest: writing the report header: %w", err)
	}

	// The run proper. A context that has been cancelled — Ctrl-C — stops the
	// sequence but never the restore below, which gets a context of its own.
	aborted := ""
	if err := r.sequence(ctx); err != nil {
		aborted = err.Error()
	}

	// Put the radio back, always, on a fresh context so that an interrupted run
	// still leaves somebody's rig where they left it. This is the part that
	// earns the right to ask a stranger to run the thing.
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	restoreErr := r.restore(restoreCtx)

	sum := Summary{
		FinishedAt:  time.Now(),
		ElapsedMS:   time.Since(r.started).Milliseconds(),
		Restored:    restoreErr == nil,
		Interrupted: ctx.Err() != nil,
		Aborted:     aborted,
	}
	if restoreErr != nil {
		sum.RestoreErr = restoreErr.Error()
	}
	sum, err := r.w.summary(sum)
	if err != nil {
		return sum, fmt.Errorf("selftest: writing the summary: %w", err)
	}
	return sum, nil
}

// do runs one step: it collects the wire trace around the action, times it, and
// writes the result out.
//
// Everything the run does goes through here, so that no step can forget to
// attach its frames — the attachment being most of why a submitted log is worth
// reading.
func (r *Runner) do(group, name, request string, fn func() (verdict Verdict, want, got, note string, err error)) {
	if r.opts.Progress != nil {
		r.opts.Progress(fmt.Sprintf("  %-18s %s", group, name))
	}
	r.cap.start()
	start := time.Now()
	verdict, want, got, note, err := fn()
	dur := time.Since(start)
	frames := r.cap.take()

	st := Step{
		Group:      group,
		Name:       name,
		Verdict:    verdict,
		Request:    request,
		Want:       want,
		Got:        got,
		Note:       note,
		Wire:       frames,
		DurationMS: dur.Milliseconds(),
	}
	if err != nil {
		st.Err = err.Error()
	}
	_ = r.w.step(st)
}

// skip records a control the radio does not have, or one this run was not
// allowed to touch. Written down rather than omitted: a reader has to be able
// to tell "not present" from "not tested", and a file with a silent gap in it
// invites exactly the wrong conclusion.
func (r *Runner) skip(group, name, why string) {
	r.do(group, name, "", func() (Verdict, string, string, string, error) {
		return Skipped, "", "", why, nil
	})
}

// patch applies a request and reports what came back, which is the shape almost
// every step here has.
func (r *Runner) patch(ctx context.Context, req rig.PatchRequest) (radio.State, error) {
	return r.s.ApplyPatch(ctx, req)
}

// expectRefused issues a request that the radio's own capabilities say must not
// be accepted, and fails if it was.
//
// This is the half of the run that catches the worst class of bug this project
// has met: a request that succeeds and means something else. A VFO applied to
// the wrong receiver, a tuner command that was really PTT, a power setting on a
// radio with no power command — each looked like a success from the client's
// side, and each is caught here by asking for something the radio said it could
// not do and insisting on being told no.
func (r *Runner) expectRefused(ctx context.Context, group, name, request string, req rig.PatchRequest) {
	r.do(group, name, request, func() (Verdict, string, string, string, error) {
		_, err := r.patch(ctx, req)
		switch {
		case err == nil:
			return Fail, "a refusal", "accepted",
				"the capabilities say this radio has no such control, but the request was accepted — " +
					"either the capability is wrong or the command went somewhere unintended", nil
		case errors.Is(err, rig.ErrUnsupported):
			return Refused, "a refusal", "refused", "", nil
		default:
			return Info, "a refusal", "refused, for another reason", "", err
		}
	})
}

// restore puts back everything the run changed.
func (r *Runner) restore(ctx context.Context) error {
	if !r.s.Connected() {
		return errors.New("the radio is not connected; nothing could be restored")
	}
	in := r.initial
	req := rig.PatchRequest{
		Mode:      &in.Mode,
		DataMode:  &in.DataMode,
		Frequency: &in.Frequency,
	}
	if r.caps.FilterWidth && in.PassbandHz > 0 {
		req.FilterWidthHz = &in.PassbandHz
	}
	if r.caps.FilterSlots > 0 && in.FilterSlot > 0 {
		req.FilterSlot = &in.FilterSlot
	}
	if r.caps.PowerControl {
		pct := in.Power.Pct
		req.Power = &radio.PowerSet{Pct: &pct}
	}
	if r.caps.BreakInControl && in.BreakIn != "" {
		req.BreakIn = &in.BreakIn
	}

	var errs []error
	if _, err := r.patch(ctx, req); err != nil {
		errs = append(errs, fmt.Errorf("operating state: %w", err))
	}
	if err := r.restoreFrontEnd(ctx); err != nil {
		errs = append(errs, err)
	}
	r.do("restore", "as-found", describePatch(req), func() (Verdict, string, string, string, error) {
		got := r.s.State()
		if err := errors.Join(errs...); err != nil {
			return Fail, describeState(in), describeState(got),
				"the radio could not be put back as it was found", err
		}
		return Pass, describeState(in), describeState(got), "", nil
	})
	return errors.Join(errs...)
}

// restoreFrontEnd puts the receive controls back. Separate from the operating
// state because a radio may refuse one of these in the mode it has been left
// in, and a single failure there should not abandon the rest.
func (r *Runner) restoreFrontEnd(ctx context.Context) error {
	in := r.initial
	var errs []error
	try := func(req rig.PatchRequest) {
		if _, err := r.patch(ctx, req); err != nil {
			errs = append(errs, err)
		}
	}
	if r.caps.PreampLevels > 0 && in.Preamp != nil {
		try(rig.PatchRequest{Preamp: in.Preamp})
	}
	if r.caps.AttenuatorControl() && in.AttenuatorDB != nil {
		try(rig.PatchRequest{AttenuatorDB: in.AttenuatorDB})
	}
	if r.caps.RFGainControl && in.RFGain != nil {
		try(rig.PatchRequest{RFGain: in.RFGain})
	}
	if r.caps.AGCControl() && in.AGC != "" {
		agc := in.AGC
		try(rig.PatchRequest{AGC: &agc})
	}
	if r.caps.IPPlusControl && in.IPPlus != nil {
		try(rig.PatchRequest{IPPlus: in.IPPlus})
	}
	if r.caps.NoiseBlankerLevels > 0 && in.NoiseBlanker != nil {
		try(rig.PatchRequest{NoiseBlanker: in.NoiseBlanker})
	}
	if r.caps.NoiseReductionLevels > 0 && in.NoiseReduction != nil {
		try(rig.PatchRequest{NoiseReduction: in.NoiseReduction})
	}
	if r.caps.NotchControl && in.Notch != nil {
		try(rig.PatchRequest{Notch: in.Notch})
	}
	if r.caps.AutoNotchControl && in.AutoNotch != nil {
		try(rig.PatchRequest{AutoNotch: in.AutoNotch})
	}
	return errors.Join(errs...)
}

// waitConnected blocks until the radio is usable, so that a run never starts
// against a session that is still dialling.
func WaitConnected(ctx context.Context, s Session, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if s.Connected() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the radio did not connect within %s", d)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// NewCapture wraps a handler so that CAT frames can be attached to steps, and
// returns both the handler to give the session and the capture to give Run.
func NewCapture(next slog.Handler) (slog.Handler, *capture) {
	c := newCapture(next)
	return c, c
}
