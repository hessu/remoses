package rigctld

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

func TestDecodeMode(t *testing.T) {
	tests := []struct {
		token string
		mode  radio.Mode
		data  bool
		ok    bool
	}{
		{"LSB", radio.ModeLSB, false, true},
		{"USB", radio.ModeUSB, false, true},
		{"CW", radio.ModeCW, false, true},
		{"CWR", radio.ModeCWR, false, true},
		{"CW-R", radio.ModeCWR, false, true},
		{"AM", radio.ModeAM, false, true},
		{"FM", radio.ModeFM, false, true},
		{"RTTY", radio.ModeFSK, false, true},
		{"RTTYR", radio.ModeFSKR, false, true},
		{"RTTY-R", radio.ModeFSKR, false, true},
		{"PSK", radio.ModePSK, false, true},
		{"PSKR", radio.ModePSKR, false, true},

		// The whole point of the PKT family: mode plus an orthogonal data flag.
		{"PKTUSB", radio.ModeUSB, true, true},
		{"USB-D", radio.ModeUSB, true, true},
		{"PKTLSB", radio.ModeLSB, true, true},
		{"LSB-D", radio.ModeLSB, true, true},
		{"PKTFM", radio.ModeFM, true, true},
		{"FM-D", radio.ModeFM, true, true},
		{"PKTFMN", radio.ModeFM, true, true},
		{"PKTAM", radio.ModeAM, true, true},
		{"AM-D", radio.ModeAM, true, true},

		// Narrow variants collapse onto the parent mode; see the table's doc.
		{"FMN", radio.ModeFM, false, true},
		{"AMN", radio.ModeAM, false, true},
		{"CWN", radio.ModeCW, false, true},

		// Modes remoses has no word for report false so the caller leaves the
		// cached mode alone rather than overwriting it with ModeUnknown.
		{"WFM", radio.ModeUnknown, false, false},
		{"D-STAR", radio.ModeUnknown, false, false},
		{"", radio.ModeUnknown, false, false},
		{"usb", radio.ModeUnknown, false, false}, // rigctld always emits upper case
	}

	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			mode, data, ok := decodeMode(tc.token)
			if ok != tc.ok || mode != tc.mode || data != tc.data {
				t.Errorf("decodeMode(%q) = (%v, %v, %v), want (%v, %v, %v)",
					tc.token, mode, data, ok, tc.mode, tc.data, tc.ok)
			}
		})
	}
}

func TestEncodeMode(t *testing.T) {
	tests := []struct {
		mode    radio.Mode
		data    bool
		want    string
		wantErr string
	}{
		{mode: radio.ModeLSB, want: "LSB"},
		{mode: radio.ModeUSB, want: "USB"},
		{mode: radio.ModeCW, want: "CW"},
		{mode: radio.ModeCWR, want: "CWR"},
		{mode: radio.ModeAM, want: "AM"},
		{mode: radio.ModeFM, want: "FM"},
		{mode: radio.ModeFSK, want: "RTTY"},
		{mode: radio.ModeFSKR, want: "RTTYR"},
		{mode: radio.ModePSK, want: "PSK"},
		{mode: radio.ModePSKR, want: "PSKR"},

		{mode: radio.ModeUSB, data: true, want: "PKTUSB"},
		{mode: radio.ModeLSB, data: true, want: "PKTLSB"},
		{mode: radio.ModeFM, data: true, want: "PKTFM"},
		{mode: radio.ModeAM, data: true, want: "PKTAM"},

		// Hamlib has no data variant of the modes that are already digital.
		{mode: radio.ModeCW, data: true, wantErr: "no data-mode variant"},
		{mode: radio.ModeFSK, data: true, wantErr: "no data-mode variant"},
		{mode: radio.ModePSK, data: true, wantErr: "no data-mode variant"},

		{mode: radio.ModeUnknown, wantErr: "no mode token"},
	}

	for _, tc := range tests {
		got, err := encodeMode(tc.mode, tc.data)
		switch {
		case tc.wantErr != "":
			if err == nil {
				t.Errorf("encodeMode(%v, %v) = %q, want an error naming %q", tc.mode, tc.data, got, tc.wantErr)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("encodeMode(%v, %v) error = %v, want it to mention %q", tc.mode, tc.data, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("encodeMode(%v, %v): %v", tc.mode, tc.data, err)
		case got != tc.want:
			t.Errorf("encodeMode(%v, %v) = %q, want %q", tc.mode, tc.data, got, tc.want)
		}
	}
}

// TestModeRoundTrip proves every token encode produces decodes back to the pair
// it came from. A mismatch here would mean setting a mode and reading it back
// disagreed with itself.
func TestModeRoundTrip(t *testing.T) {
	for spec, token := range encodeTokens {
		mode, data, ok := decodeMode(token)
		if !ok {
			t.Errorf("encodeMode produces %q, which decodeMode does not know", token)
			continue
		}
		if mode != spec.mode || data != spec.data {
			t.Errorf("%q round-trips to (%v, %v), want (%v, %v)", token, mode, data, spec.mode, spec.data)
		}
	}
}

func TestTokensFromMask(t *testing.T) {
	tests := []struct {
		name string
		mask uint64
		want []string
	}{
		{name: "none", mask: 0},
		{
			// AM|CW|USB|LSB|RTTY|FM|CWR|RTTYR|PKTLSB|PKTUSB|PKTFM, which is the
			// shape of a typical HF transceiver's range list.
			name: "typical HF rig",
			mask: 0x1DBF,
			want: []string{"AM", "CW", "USB", "LSB", "RTTY", "FM", "CWR", "RTTYR", "PKTLSB", "PKTUSB", "PKTFM"},
		},
		{
			// Bit 6 is WFM and bit 24 is D-STAR: real modes with no remoses
			// name, dropped rather than guessed at.
			name: "unmappable bits are dropped",
			mask: (1 << 6) | (1 << 24) | (1 << 2),
			want: []string{"USB"},
		},
		{
			name: "the top bits do not panic",
			mask: 1 << 63,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tokensFromMask(tc.mask)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("tokensFromMask(%#x) = %v, want %v", tc.mask, got, tc.want)
			}
		})
	}
}
