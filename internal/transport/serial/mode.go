package serial

import (
	"fmt"
	"strings"

	"github.com/hessu/remoses/internal/config"
	bugst "go.bug.st/serial"
)

// newMode maps the config's human-facing port settings onto the driver's enums.
//
// Omitted framing means 8N1, which is what every rig in scope uses; anything
// unrecognised is rejected instead of silently defaulted, because a typo here
// surfaces much later as garbled CAT rather than as a startup failure.
func newMode(p config.Port) (*bugst.Mode, error) {
	if p.Baud <= 0 {
		return nil, fmt.Errorf("serial: baud rate must be set (got %d)", p.Baud)
	}
	bits := p.DataBits
	if bits == 0 {
		bits = 8
	}
	if bits < 5 || bits > 8 {
		return nil, fmt.Errorf("serial: data bits must be 5..8, got %d", bits)
	}
	par, err := parseParity(p.Parity)
	if err != nil {
		return nil, err
	}
	stop, err := parseStopBits(p.StopBits)
	if err != nil {
		return nil, err
	}
	return &bugst.Mode{
		BaudRate: p.Baud,
		DataBits: bits,
		Parity:   par,
		StopBits: stop,
		// Open with DTR and RTS low. On many interfaces these lines are PTT or
		// the CW key (DESIGN.md §11.2), and the driver's default of asserting
		// both would put the radio into transmit the moment remoses starts. The
		// unix implementation still emits a pulse of a few milliseconds before
		// it can apply this, which is a driver limitation we cannot close from
		// here; it is short enough not to key a rig with a PTT lead-in.
		InitialStatusBits: &bugst.ModemOutputBits{RTS: false, DTR: false},
	}, nil
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
