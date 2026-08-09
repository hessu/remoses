package serial

import (
	"testing"

	"github.com/hessu/remoses/internal/config"
	bugst "go.bug.st/serial"
)

func TestParseParity(t *testing.T) {
	tests := []struct {
		in      string
		want    bugst.Parity
		wantErr bool
	}{
		{in: "", want: bugst.NoParity},
		{in: "none", want: bugst.NoParity},
		{in: "NONE", want: bugst.NoParity},
		{in: " n ", want: bugst.NoParity},
		{in: "odd", want: bugst.OddParity},
		{in: "Even", want: bugst.EvenParity},
		{in: "mark", want: bugst.MarkParity},
		{in: "space", want: bugst.SpaceParity},
		{in: "nine", wantErr: true},
		{in: "0", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseParity(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseParity(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseParity(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseStopBits(t *testing.T) {
	tests := []struct {
		in      string
		want    bugst.StopBits
		wantErr bool
	}{
		{in: "", want: bugst.OneStopBit},
		{in: "1", want: bugst.OneStopBit},
		{in: "One", want: bugst.OneStopBit},
		{in: "1.5", want: bugst.OnePointFiveStopBits},
		{in: "2", want: bugst.TwoStopBits},
		{in: " two ", want: bugst.TwoStopBits},
		{in: "3", wantErr: true},
		{in: "1,5", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseStopBits(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseStopBits(%q) error = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseStopBits(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewMode(t *testing.T) {
	t.Run("defaults to 8N1", func(t *testing.T) {
		m, _, err := newMode(config.Port{Baud: 115200})
		if err != nil {
			t.Fatalf("newMode: %v", err)
		}
		if m.BaudRate != 115200 || m.DataBits != 8 || m.Parity != bugst.NoParity || m.StopBits != bugst.OneStopBit {
			t.Fatalf("got %+v, want 115200 8N1", m)
		}
	})

	// The open is always low, even for a port configured high: Dial raises the
	// configured lines afterwards so that they transition rather than merely
	// sit high, which is what a TS-590S needs before it will answer at all.
	t.Run("opens with control lines low whatever they are configured as", func(t *testing.T) {
		for _, p := range []config.Port{
			{Baud: 9600},
			{Baud: 9600, DTR: "high", RTS: "high"},
		} {
			m, _, err := newMode(p)
			if err != nil {
				t.Fatalf("newMode(%+v): %v", p, err)
			}
			if m.InitialStatusBits == nil || m.InitialStatusBits.DTR || m.InitialStatusBits.RTS {
				t.Fatalf("newMode(%+v): DTR/RTS must open low, got %+v", p, m.InitialStatusBits)
			}
		}
	})

	t.Run("reports the configured line levels", func(t *testing.T) {
		for _, tc := range []struct {
			port config.Port
			want lineState
		}{
			{config.Port{Baud: 9600}, lineState{}},
			{config.Port{Baud: 9600, DTR: "high"}, lineState{dtr: true}},
			{config.Port{Baud: 9600, RTS: "on"}, lineState{rts: true}},
			{config.Port{Baud: 9600, DTR: "high", RTS: "high"}, lineState{dtr: true, rts: true}},
			{config.Port{Baud: 9600, DTR: "low", RTS: "low"}, lineState{}},
		} {
			_, got, err := newMode(tc.port)
			if err != nil {
				t.Fatalf("newMode(%+v): %v", tc.port, err)
			}
			if got != tc.want {
				t.Errorf("newMode(%+v) lines = %+v, want %+v", tc.port, got, tc.want)
			}
		}
	})

	t.Run("rejects bad settings", func(t *testing.T) {
		bad := []config.Port{
			{Baud: 0},
			{Baud: -1},
			{Baud: 9600, DataBits: 4},
			{Baud: 9600, DataBits: 9},
			{Baud: 9600, Parity: "weird"},
			{Baud: 9600, StopBits: "7"},
			{Baud: 9600, DTR: "maybe"},
			{Baud: 9600, RTS: "sometimes"},
		}
		for _, p := range bad {
			if _, _, err := newMode(p); err == nil {
				t.Errorf("newMode(%+v) = nil error, want failure", p)
			}
		}
	})
}
