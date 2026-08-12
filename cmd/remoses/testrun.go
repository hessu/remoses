package main

// `remoses test-run` — point remoses at a radio, exercise everything the radio
// says it can do, and write down what happened.
//
// The reason it exists is arithmetic. This project carries profiles for radios
// nobody here can plug in, transcribed from manufacturers' references, and
// every single radio that *has* been connected found bugs that no amount of
// reading those references would have. The untested profiles are not better
// than the tested ones were; they are only unexamined. This is how somebody
// else's radio gets to file a report.
//
// So the design constraint is a stranger's station, unattended:
//
//   - it never transmits unless given a frequency to transmit on;
//   - it never switches the radio off unless told to, separately, because a
//     wake that does not work leaves somebody with a rig they must walk to;
//   - it puts the radio back as it found it, including when interrupted;
//   - and the file it writes is meant to be read by somebody who was not there,
//     so it carries the capabilities, the request, the read-back and the CAT
//     frames for every step.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/selftest"
)

func testRun(args []string) error {
	fs := flag.NewFlagSet("test-run", flag.ExitOnError)
	var (
		cfgPath = fs.String("config", "remoses.yaml", "path to the configuration file")
		radioID = fs.String("radio", "", "which configured radio to test (required if more than one)")
		out     = fs.String("out", "", "write the report here (default: remoses-selftest-<radio>-<time>.jsonl)")
		txFreq  = fs.Uint64("tx-freq", 0,
			"transmit test frequency in Hz. Without this the run NEVER keys the radio. "+
				"With it, the run may key the transmitter, send a short CW message and, "+
				"where the radio has one, run an antenna tuner cycle")
		txPower = fs.Int("tx-power-pct", 10, "transmit power for the tests, percent")
		cwText  = fs.String("cw-text", selftest.DefaultCWText,
			"what the CW test sends; set your own callsign")
		powerSw = fs.Bool("test-power-switch", false,
			"also switch the radio off and wake it over CAT. Off by default: whether a wake "+
				"works is a wiring question, and a radio that will not wake needs somebody at it")
		notes    = fs.String("notes", "", "free text about the station, copied into the report")
		logLevel = fs.String("log-level", "info", "terminal log level: debug, info, warn or error")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `remoses test-run — exercise a radio and write a report

usage:
  remoses test-run [flags]

The report is JSON Lines: a header with the radio's capabilities, one line per
step with the request, the read-back and the CAT frames, and a summary.

Send the finished report to %s.

Nothing here transmits unless -tx-freq is given. Pick a frequency you are
licensed and equipped to transmit on; the run keeps power low and every
transmission short.

flags:
`, ReportAddress)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	rc, err := pickRadio(cfg, *radioID)
	if err != nil {
		return err
	}

	// Wire tracing is forced on. It is the single most useful thing in the
	// report — decoded state hides exactly the mistakes worth finding — and
	// asking an operator to remember a second flag would mean half the reports
	// arriving without it.
	rc.DebugWire = true

	// The capture handler sits in front of the terminal handler: it keeps the
	// CAT frames for the step in progress and passes everything else through.
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		level = slog.LevelInfo
	}
	term := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	handler, cap := selftest.NewCapture(term)
	log := slog.New(handler)

	s, closer, err := buildSession(rc, log)
	if err != nil {
		return err
	}
	if closer != nil {
		defer func() { _ = closer.Close() }()
	}
	defer func() { _ = s.Close() }()

	path := *out
	if path == "" {
		path = fmt.Sprintf("remoses-selftest-%s-%s.jsonl", rc.ID, time.Now().Format("20060102-150405"))
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating the report: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Ctrl-C stops the sequence; the restore runs regardless. See selftest.Run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s.Start(ctx)
	fmt.Fprintf(os.Stderr, "connecting to %s...\n", rc.ID)
	if err := selftest.WaitConnected(ctx, s, 30*time.Second); err != nil {
		return fmt.Errorf("radio %q: %w", rc.ID, err)
	}

	if *txFreq > 0 {
		fmt.Fprintf(os.Stderr, "\nTRANSMIT TESTS ARE ON: %s will be keyed on %.6f MHz at %d%%.\n",
			rc.ID, float64(*txFreq)/1e6, *txPower)
	} else {
		fmt.Fprintln(os.Stderr, "\nreceive only; pass -tx-freq to include the transmit tests")
	}
	fmt.Fprintf(os.Stderr, "writing %s\n\n", path)

	sum, err := selftest.Run(ctx, s, cap, selftest.Options{
		TXFrequency:   *txFreq,
		TXPowerPct:    *txPower,
		PowerSwitch:   *powerSw,
		CWText:        *cwText,
		OperatorNotes: *notes,
		Version:       version,
		Model:         configuredModel(rc),
		Transport:     describeTransport(rc),
		Progress:      func(line string) { fmt.Fprintln(os.Stderr, line) },
	}, f)
	if err != nil {
		return err
	}

	report(os.Stderr, path, sum)
	return nil
}

// pickRadio resolves -radio, and refuses to guess when there is more than one.
func pickRadio(cfg *config.Config, id string) (config.Radio, error) {
	if id == "" {
		if len(cfg.Radios) == 1 {
			return cfg.Radios[0], nil
		}
		var names []string
		for _, r := range cfg.Radios {
			names = append(names, r.ID)
		}
		return config.Radio{}, fmt.Errorf("this configuration has %d radios (%s); name one with -radio",
			len(cfg.Radios), strings.Join(names, ", "))
	}
	for _, r := range cfg.Radios {
		if r.ID == id {
			return r, nil
		}
	}
	return config.Radio{}, fmt.Errorf("no radio %q in the configuration", id)
}

// configuredModel digs out whichever model string this radio was configured
// with. It goes in the report header because on several backends the model is
// the only thing that chose the command table, so a report filed against the
// wrong one explains itself at a glance.
func configuredModel(rc config.Radio) string {
	switch {
	case rc.CIV != nil && rc.CIV.Model != "":
		return rc.CIV.Model
	case rc.Kenwood != nil && rc.Kenwood.Model != "":
		return rc.Kenwood.Model
	case rc.Yaesu != nil && rc.Yaesu.Model != "":
		return rc.Yaesu.Model
	case rc.Rigctld != nil && rc.Rigctld.Model != 0:
		return fmt.Sprintf("hamlib-%d", rc.Rigctld.Model)
	}
	return ""
}

// describeTransport says how the radio is reached, in one line for the report
// header. Line settings are included because several of the bugs found on
// hardware so far were a port opened the wrong way rather than a protocol read
// wrongly — a TS-590S that answered nothing until its control lines were
// raised, an FT-857 that needs two stop bits.
func describeTransport(rc config.Radio) string {
	if rc.Backend == config.BackendRigctld && rc.Rigctld != nil {
		if rc.Rigctld.Spawn {
			return fmt.Sprintf("rigctld %s (spawned, device %s)", rc.Rigctld.Address, rc.Rigctld.Device)
		}
		return "rigctld " + rc.Rigctld.Address
	}
	if rc.Port.Networked() {
		return "tcp " + rc.Port.TCP
	}
	dev := rc.Port.Device
	if dev == "" && rc.Port.Match != nil {
		dev = fmt.Sprintf("match vid=%s pid=%s serial=%s",
			rc.Port.Match.VID, rc.Port.Match.PID, rc.Port.Match.Serial)
	}
	parity := rc.Port.Parity
	if parity == "" {
		parity = "none"
	}
	stop := rc.Port.StopBits
	if stop == "" {
		stop = "1"
	}
	bits := rc.Port.DataBits
	if bits == 0 {
		bits = 8
	}
	return fmt.Sprintf("%s @%d %d%s%s dtr=%s rts=%s",
		dev, rc.Port.Baud, bits, strings.ToUpper(parity[:1]), stop,
		orDefault(rc.Port.DTR, "high"), orDefault(rc.Port.RTS, "high"))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// report prints the part the operator reads before sending the file on.
func report(w *os.File, path string, sum selftest.Summary) {
	fmt.Fprintln(w)
	fmt.Fprintf(w, "done in %.1f s\n", float64(sum.ElapsedMS)/1000)
	for _, v := range []selftest.Verdict{selftest.Pass, selftest.Fail, selftest.Refused, selftest.Skipped, selftest.Info} {
		if n := sum.Counts[v]; n > 0 {
			fmt.Fprintf(w, "  %-8s %d\n", v, n)
		}
	}
	if len(sum.Failures) > 0 {
		fmt.Fprintln(w, "\nfailures:")
		for _, f := range sum.Failures {
			fmt.Fprintf(w, "  - %s\n", f)
		}
	}
	if !sum.Restored {
		fmt.Fprintf(w, "\nWARNING: the radio could NOT be put back as it was found: %s\n", sum.RestoreErr)
		fmt.Fprintln(w, "check its frequency, mode and power before using it.")
	}
	if sum.Aborted != "" {
		fmt.Fprintf(w, "\nthe run stopped early: %s\n", sum.Aborted)
	}
	fmt.Fprintf(w, "\nreport written to %s\n", path)
	fmt.Fprintf(w, "\nPlease send it to %s — that is what makes this worth having.\n", ReportAddress)
	fmt.Fprintln(w, "It carries no credentials and no part of your configuration beyond")
	fmt.Fprintln(w, "the radio's own description. Say in the message whether anything")
	fmt.Fprintln(w, "sounded wrong at the radio, and whether the CW was audible — those")
	fmt.Fprintln(w, "are the two things the file cannot tell anybody.")
}

// ReportAddress is where a finished report should go. In one place because it
// is printed by the tool and written in the documentation, and an address that
// disagrees with itself is one people give up on.
const ReportAddress = "remoses-logs@he.fi"

// errNoSubcommand keeps main's dispatch readable.
var errNoSubcommand = errors.New("not a subcommand")
