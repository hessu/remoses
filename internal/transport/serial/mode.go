package serial

import (
	"fmt"
	"strings"

	"github.com/hessu/remoses/internal/config"
	bugst "go.bug.st/serial"
)

// lineState is the resting state configured for a port's modem outputs. It is
// carried separately from the Mode because the port is always opened with both
// low and the configured levels are applied afterwards; see newMode.
type lineState struct {
	dtr bool
	rts bool
}

// newMode maps the config's human-facing port settings onto the driver's enums,
// and returns the control-line levels alongside rather than inside them.
//
// Omitted framing means 8N1, which is what every rig in scope uses; anything
// unrecognised is rejected instead of silently defaulted, because a typo here
// surfaces much later as garbled CAT rather than as a startup failure.
func newMode(p config.Port) (*bugst.Mode, lineState, error) {
	if p.Baud <= 0 {
		return nil, lineState{}, fmt.Errorf("serial: baud rate must be set (got %d)", p.Baud)
	}
	bits := p.DataBits
	if bits == 0 {
		bits = 8
	}
	if bits < 5 || bits > 8 {
		return nil, lineState{}, fmt.Errorf("serial: data bits must be 5..8, got %d", bits)
	}
	par, err := parseParity(p.Parity)
	if err != nil {
		return nil, lineState{}, err
	}
	stop, err := parseStopBits(p.StopBits)
	if err != nil {
		return nil, lineState{}, err
	}
	dtr, err := parseLineLevel("dtr", p.DTR)
	if err != nil {
		return nil, lineState{}, err
	}
	rts, err := parseLineLevel("rts", p.RTS)
	if err != nil {
		return nil, lineState{}, err
	}
	return &bugst.Mode{
		BaudRate: p.Baud,
		DataBits: bits,
		Parity:   par,
		StopBits: stop,
		// Always open with both outputs LOW, whatever the port is configured
		// for. Dial raises the configured ones immediately afterwards.
		//
		// The levels are deliberately not passed here. What some rigs react to
		// is the low->high transition, not the level: a TS-590S on its built-in
		// USB bridge stays mute for a port opened with DTR and RTS already high
		// — the right speed, the right device, correct frames going out, and
		// not one byte back — and answers ID; at once when the same port is
		// opened low and the lines are then raised. Opening low costs a few
		// milliseconds of idle line and makes both cases work.
		//
		// It is also the safe direction for the other kind of port: on a keying
		// interface DTR or RTS is the key or PTT, and asserting one at open
		// would transmit (DESIGN.md §6). Such a port is configured low and
		// never gets raised at all.
		InitialStatusBits: &bugst.ModemOutputBits{RTS: false, DTR: false},
	}, lineState{dtr: dtr, rts: rts}, nil
}

// parseLineLevel reads a modem output's configured resting state. Empty means
// low, which is the safe direction: see newMode.
func parseLineLevel(name, s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "low", "off", "false":
		return false, nil
	case "high", "on", "true":
		return true, nil
	}
	return false, fmt.Errorf("serial: port.%s %q, want high or low", name, s)
}

func parseParity(s string) (bugst.Parity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none", "n":
		return bugst.NoParity, nil
	case "odd", "o":
		return bugst.OddParity, nil
	case "even", "e":
		return bugst.EvenParity, nil
	case "mark", "m":
		return bugst.MarkParity, nil
	case "space", "s":
		return bugst.SpaceParity, nil
	default:
		return 0, fmt.Errorf("serial: unknown parity %q (want none, odd, even, mark or space)", s)
	}
}

func parseStopBits(s string) (bugst.StopBits, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "1", "1.0", "one":
		return bugst.OneStopBit, nil
	case "1.5", "one_point_five":
		return bugst.OnePointFiveStopBits, nil
	case "2", "2.0", "two":
		return bugst.TwoStopBits, nil
	default:
		return 0, fmt.Errorf("serial: unknown stop bits %q (want 1, 1.5 or 2)", s)
	}
}
