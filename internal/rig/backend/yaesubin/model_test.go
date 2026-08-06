package yaesubin

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/radio"
)

// TestHandlesIsTheDispatch pins the one function the yaesu package calls to
// decide which protocol a configured radio speaks. Getting it wrong in either
// direction sends a radio to a backend that cannot talk to it at all.
func TestHandlesIsTheDispatch(t *testing.T) {
	mine := []string{"ft-857", "FT-857D", "ft897", "FT-897D"}
	for _, n := range mine {
		if !Handles(n) {
			t.Errorf("Handles(%q) = false; that radio speaks the binary protocol", n)
		}
	}
	// Everything the ASCII backend owns, including the empty name that means
	// "unnamed Yaesu" and must keep falling through to it.
	notMine := []string{
		"", "generic", "ft-710", "ft-891", "ft-950", "ft-991a",
		"ftdx10", "ftdx101d", "ftdx101mp", "ftdx1200", "ftdx3000", "ftdx5000",
		"ftdx9000", "ftx-1", "ft-818", "nonsense",
	}
	for _, n := range notMine {
		if Handles(n) {
			t.Errorf("Handles(%q) = true; this package must not claim it", n)
		}
	}
}

// TestFT891IsNotFT897 is the near-miss the dispatch has to get right: one
// character apart, and an entire protocol apart.
func TestFT891IsNotFT897(t *testing.T) {
	if Handles("ft-891") {
		t.Fatal("the FT-891 is an ASCII-CAT radio and must not be routed here")
	}
	if !Handles("ft-897") {
		t.Fatal("the FT-897 is a binary-CAT radio and must be routed here")
	}
}

func TestLookupModelFolding(t *testing.T) {
	for _, n := range []string{"FT-857D", "ft857d", "FT857D", "  ft-857d  "} {
		m, err := LookupModel(n)
		if err != nil {
			t.Fatalf("LookupModel(%q): %v", n, err)
		}
		if m.Name != "ft-857d" {
			t.Errorf("LookupModel(%q) resolved to %q", n, m.Name)
		}
	}
}

// TestLookupModelRequiresAName is the difference from the ASCII backend: there
// is no default here, because reaching this package means a name matched.
func TestLookupModelRequiresAName(t *testing.T) {
	if _, err := LookupModel(""); err == nil {
		t.Error("LookupModel(\"\") succeeded; there is no default model in this package")
	}
	if _, err := LookupModel("ftdx10"); err == nil {
		t.Error("LookupModel accepted an ASCII-dialect radio")
	}
}

// TestAllFourProfilesAreIdentical states the finding. Four manuals, one CAT
// chapter: if a difference ever turns up, this is the test that has to change
// and the comment on `models` with it.
func TestAllFourProfilesAreIdentical(t *testing.T) {
	base := models["ft-857"]
	for _, n := range ModelNames() {
		m := models[n]
		if !reflect.DeepEqual(m.Codes, base.Codes) {
			t.Errorf("%s has a different mode table from the FT-857", n)
		}
		if !reflect.DeepEqual(m.Modes, base.Modes) {
			t.Errorf("%s has a different mode list from the FT-857", n)
		}
		if m.MinHz != base.MinHz || m.MaxHz != base.MaxHz {
			t.Errorf("%s tunes %d-%d Hz, the FT-857 %d-%d", n, m.MinHz, m.MaxHz, base.MinHz, base.MaxHz)
		}
		if m.Label == "" || m.Name != n {
			t.Errorf("%s: Name %q Label %q", n, m.Name, m.Label)
		}
	}
}

// TestModeTable is the opcode chart, transcribed a second time. Every value
// here was read off the printed table rather than derived from the map under
// test.
func TestModeTable(t *testing.T) {
	m := models["ft-857d"]
	cases := []struct {
		code byte
		mode radio.Mode
		data bool
	}{
		{0x00, radio.ModeLSB, false},
		{0x01, radio.ModeUSB, false},
		{0x02, radio.ModeCW, false},
		{0x03, radio.ModeCWR, false},
		{0x04, radio.ModeAM, false},
		{0x06, radio.ModeWFM, false},
		{0x08, radio.ModeFM, false},
		{0x0A, radio.ModeDIG, false},
		{0x0C, radio.ModeFM, true}, // PKT
		{0x82, radio.ModeCW, false},
		{0x88, radio.ModeFM, false},
	}
	for _, c := range cases {
		mode, data, ok := m.decodeMode(c.code)
		if !ok {
			t.Errorf("code %02X is not in the table", c.code)
			continue
		}
		if mode != c.mode || data != c.data {
			t.Errorf("code %02X = %v data %v, want %v data %v", c.code, mode, data, c.mode, c.data)
		}
	}
	// The chart lists eleven codes and no more. A twelfth would be an
	// invention; several byte values in between are simply not modes.
	if len(m.Codes) != len(cases) {
		t.Errorf("the table has %d codes, the chart prints %d", len(m.Codes), len(cases))
	}
	for _, code := range []byte{0x05, 0x07, 0x09, 0x0B, 0x0D, 0x80, 0xFF} {
		if _, _, ok := m.decodeMode(code); ok {
			t.Errorf("code %02X decoded to something; the chart does not list it", code)
		}
	}
}

// TestEncodePrefersTheWideCode is why the scan over the table is ordered.
// Narrow is the front panel's filter selection on these radios, not something a
// mode change should assert.
func TestEncodePrefersTheWideCode(t *testing.T) {
	m := models["ft-857d"]
	cases := []struct {
		mode radio.Mode
		data bool
		code byte
	}{
		{radio.ModeLSB, false, 0x00},
		{radio.ModeUSB, false, 0x01},
		{radio.ModeCW, false, 0x02}, // not 82, CW-N
		{radio.ModeCWR, false, 0x03},
		{radio.ModeAM, false, 0x04},
		{radio.ModeFM, false, 0x08}, // not 88, FM-N
		{radio.ModeFM, true, 0x0C},  // PKT
		{radio.ModeDIG, false, 0x0A},
	}
	for _, c := range cases {
		got, err := m.modeSet(c.mode, c.data)
		if err != nil {
			t.Errorf("modeSet(%v, %v): %v", c.mode, c.data, err)
			continue
		}
		if got != c.code {
			t.Errorf("modeSet(%v, %v) = %02X, want %02X", c.mode, c.data, got, c.code)
		}
	}
}

// TestWFMIsReadOnly is the one asymmetry between the two tables the manuals
// print: the status answer has a code for wide FM and the mode-set command
// does not.
func TestWFMIsReadOnly(t *testing.T) {
	m := models["ft-857d"]
	if mode, _, ok := m.decodeMode(0x06); !ok || mode != radio.ModeWFM {
		t.Fatalf("code 06 decoded to %v, %v; want WFM", mode, ok)
	}
	if m.supportsMode(radio.ModeWFM) {
		t.Error("WFM is advertised as settable; no mode-set code exists for it")
	}
	_, err := m.modeSet(radio.ModeWFM, false)
	if err == nil {
		t.Fatal("modeSet(WFM) succeeded")
	}
	// The refusal has to say why, because the mode is one the client just saw
	// in the state it read.
	if got := err.Error(); !strings.Contains(got, "cannot be put into it over CAT") {
		t.Errorf("modeSet(WFM) error is unhelpful: %v", got)
	}
}

// TestNoDataVariantOfSSB records what these radios genuinely lack. There is no
// USB-DATA code: DIG and PKT are the data modes, and PKT is FM.
func TestNoDataVariantOfSSB(t *testing.T) {
	m := models["ft-857d"]
	for _, mode := range []radio.Mode{radio.ModeLSB, radio.ModeUSB, radio.ModeCW, radio.ModeAM} {
		if _, err := m.modeSet(mode, true); err == nil {
			t.Errorf("modeSet(%v, data) succeeded; this generation has no DATA variant of it", mode)
		}
	}
}

func TestCapsModesAreSettable(t *testing.T) {
	m := models["ft-897d"]
	for _, mode := range m.Modes {
		if _, err := m.modeSet(mode, false); err != nil {
			t.Errorf("Caps advertises %v but modeSet refuses it: %v", mode, err)
		}
	}
}

func TestCheckFrequency(t *testing.T) {
	m := models["ft-857d"]
	for _, hz := range []uint64{100_000, 14_250_000, 145_500_000, 470_000_000} {
		if err := m.checkFrequency(hz); err != nil {
			t.Errorf("checkFrequency(%d): %v", hz, err)
		}
	}
	for _, hz := range []uint64{0, 99_999, 470_000_001, 1_300_000_000} {
		if err := m.checkFrequency(hz); err == nil {
			t.Errorf("checkFrequency(%d) accepted an out-of-range frequency", hz)
		}
	}
	// A frequency in one of the coverage gaps is deliberately allowed through:
	// the manuals say nothing about what the radio does with it, and the
	// read-back reports what actually happened.
	if err := m.checkFrequency(60_000_000); err != nil {
		t.Errorf("checkFrequency inside a coverage gap: %v", err)
	}
}
