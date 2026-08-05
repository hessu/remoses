package civ

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// captureConn records the last request and acknowledges everything, for tests
// that care about what went on the wire rather than what came back.
type captureConn struct{ last []byte }

func (c *captureConn) Do(_ context.Context, req []byte, _ ...backend.Key) (backend.Update, error) {
	c.last = bytes.Clone(req)
	return backend.Update{Key: KeyAck, OK: true}, nil
}

func (c *captureConn) Send(_ context.Context, req []byte) error {
	c.last = bytes.Clone(req)
	return nil
}

// config validates civ.model against its own literal list, because importing
// this package would be a dependency cycle. This is the direction that can
// import both, so it is where the two are kept honest.
func TestModelListMatchesConfig(t *testing.T) {
	got := ModelNames()
	want := slices.Clone(config.CIVModels)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("civ.ModelNames() = %v, config.CIVModels = %v; the lists have drifted", got, want)
	}
}

// Factory bus addresses, transcribed from each radio's CI-V reference guide.
func TestModelAddresses(t *testing.T) {
	want := map[string]byte{
		"ic-7610":    0x98,
		"ic-7300mk2": 0xB6,
		"ic-7760":    0xB2,
		"ic-9700":    0xA2,
		"ic-905":     0xAC,
	}
	for name, addr := range want {
		m, err := LookupModel(name)
		if err != nil {
			t.Fatalf("LookupModel(%q): %v", name, err)
		}
		if m.Address != addr {
			t.Errorf("%s address = 0x%02X, want 0x%02X", name, m.Address, addr)
		}
		if got, ok := ModelForAddress(addr); !ok || got.Name != name {
			t.Errorf("ModelForAddress(0x%02X) = %q, %v; want %q", addr, got.Name, ok, name)
		}
	}
}

// Which modes a radio has is the main thing that differs between these models.
func TestModelModes(t *testing.T) {
	tests := []struct {
		model   string
		have    []radio.Mode
		haveNot []radio.Mode
	}{
		{"ic-7610", []radio.Mode{radio.ModePSK, radio.ModePSKR}, []radio.Mode{radio.ModeDV, radio.ModeDD}},
		{"ic-7760", []radio.Mode{radio.ModePSK, radio.ModePSKR}, []radio.Mode{radio.ModeDV, radio.ModeATV}},
		// The MK2 reference lists no PSK in its operating mode table.
		{"ic-7300mk2", []radio.Mode{radio.ModeCW, radio.ModeFM}, []radio.Mode{radio.ModePSK, radio.ModeDV}},
		{"ic-9700", []radio.Mode{radio.ModeDV, radio.ModeDD}, []radio.Mode{radio.ModePSK, radio.ModeATV}},
		{"ic-905", []radio.Mode{radio.ModeDV, radio.ModeDD, radio.ModeATV}, []radio.Mode{radio.ModePSK}},
	}
	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			m, err := LookupModel(tc.model)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range tc.have {
				if !m.supportsMode(x) {
					t.Errorf("%s should have mode %s", tc.model, x)
				}
			}
			for _, x := range tc.haveNot {
				if m.supportsMode(x) {
					t.Errorf("%s should not have mode %s", tc.model, x)
				}
			}
		})
	}
}

func TestNewUsesModelAddress(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-9700"}})
	if err != nil {
		t.Fatal(err)
	}
	if r.rigAddr != 0xA2 {
		t.Errorf("rigAddr = 0x%02X, want 0xA2 from the model", r.rigAddr)
	}

	// An explicit address wins: it is menu-configurable on every one of these.
	r, err = New(&config.Radio{CIV: &config.CIV{Model: "ic-9700", RigAddress: 0x42}})
	if err != nil {
		t.Fatal(err)
	}
	if r.rigAddr != 0x42 {
		t.Errorf("rigAddr = 0x%02X, want the configured 0x42", r.rigAddr)
	}
}

func TestNewDefaultsAndRejections(t *testing.T) {
	// No model at all keeps the pre-model behaviour.
	r, err := New(&config.Radio{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Model().Name != DefaultModel || r.rigAddr != DefaultRigAddress {
		t.Errorf("default = %s/0x%02X, want %s/0x%02X",
			r.Model().Name, r.rigAddr, DefaultModel, DefaultRigAddress)
	}

	if _, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-706"}}); err == nil {
		t.Error("New accepted an unknown model")
	}

	// generic has no factory address, so one must be configured rather than
	// guessed: guessing would address the wrong radio on a shared bus.
	if _, err := New(&config.Radio{CIV: &config.CIV{Model: "generic"}}); err == nil {
		t.Error("New accepted generic without a rig_address")
	}
	if _, err := New(&config.Radio{CIV: &config.CIV{Model: "generic", RigAddress: 0x94}}); err != nil {
		t.Errorf("generic with an explicit address: %v", err)
	}
}

func TestCapsFollowModel(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-905"}})
	if err != nil {
		t.Fatal(err)
	}
	caps := r.Caps()
	if !caps.SupportsMode(radio.ModeATV) {
		t.Error("IC-905 caps should offer ATV")
	}
	if caps.SupportsMode(radio.ModePSK) {
		t.Error("IC-905 caps should not offer PSK")
	}

	// Caps must hand out a fresh slice; it is published through the API.
	caps.Modes[0] = radio.ModeUnknown
	if r.Caps().Modes[0] == radio.ModeUnknown {
		t.Error("Caps shares its backing array with the model")
	}
}

// The IC-905 sends and expects twelve digits on its 10 GHz band and ten below
// it. Sending the wrong width would be rejected by the radio.
func TestWideFrequency(t *testing.T) {
	if _, err := encodeFrequency(wideThresholdHz, false); err == nil {
		t.Error("encodeFrequency accepted 10 GHz in the five-byte field")
	}

	b, err := encodeFrequency(10_368_100_000, true)
	if err != nil {
		t.Fatalf("wide encode: %v", err)
	}
	if len(b) != freqBytesWide {
		t.Fatalf("wide encode produced %d bytes, want %d", len(b), freqBytesWide)
	}
	// 10.3681 GHz. byte0 holds 1/10 Hz and byte5 the 10/100 GHz digits, low
	// nibble first as everywhere else: the 10 GHz digit is 1 and the 100 GHz
	// digit 0, so byte5 is 0x01 rather than 0x10.
	if want := []byte{0x00, 0x00, 0x10, 0x68, 0x03, 0x01}; !bytes.Equal(b, want) {
		t.Errorf("encode(10.3681 GHz) = % X, want % X", b, want)
	}
	if got, ok := decodeFrequency(b); !ok || got != 10_368_100_000 {
		t.Errorf("decode round trip = %d (ok=%v)", got, ok)
	}
}

func TestSetFrequencyPicksWidth(t *testing.T) {
	tests := []struct {
		model   string
		hz      uint64
		wantLen int
	}{
		{"ic-905", 1_296_000_000, freqBytes},      // below the 10 GHz band
		{"ic-905", 10_368_100_000, freqBytesWide}, // on it
		{"ic-9700", 144_300_000, freqBytes},       // no wide field at all
	}
	for _, tc := range tests {
		r, err := New(&config.Radio{CIV: &config.CIV{Model: tc.model}})
		if err != nil {
			t.Fatal(err)
		}
		c := &captureConn{}
		if err := r.SetFrequency(t.Context(), c, radio.VFOCurrent, tc.hz); err != nil {
			t.Fatalf("%s SetFrequency(%d): %v", tc.model, tc.hz, err)
		}
		// FE FE to from 05 <field...> FD
		if got := len(c.last) - 6; got != tc.wantLen {
			t.Errorf("%s at %d Hz sent a %d-byte field, want %d", tc.model, tc.hz, got, tc.wantLen)
		}
	}
}

// idConn answers the identity probe with a chosen address, and everything else
// with a bare acknowledgement.
type idConn struct {
	rigAddr  byte
	ctrlAddr byte
	reported byte
	refuse   bool // answer NG, as a radio without command 19 00 would
	asked    bool
}

func (c *idConn) Do(_ context.Context, req []byte, _ ...backend.Key) (backend.Update, error) {
	if len(req) > 5 && req[4] == cmdReadID {
		c.asked = true
		if c.refuse {
			return backend.Update{Key: KeyAck, OK: false}, nil
		}
		raw := []byte{0xFE, 0xFE, c.ctrlAddr, c.rigAddr, cmdReadID, subReadID, c.reported, 0xFD}
		return backend.Update{Key: KeyID, OK: true, Raw: raw}, nil
	}
	return backend.Update{Key: KeyAck, OK: true}, nil
}

func (c *idConn) Send(context.Context, []byte) error { return nil }

// The probe is advisory. A radio that reports an unexpected address, or refuses
// the command outright, must still be usable — the configuration is what
// remoses acts on, and an older Icom without command 19 00 is not a fault.
func TestIdentityProbeIsAdvisory(t *testing.T) {
	tests := []struct {
		name     string
		reported byte
		refuse   bool
	}{
		{"agrees", 0x98, false},
		{"disagrees", 0xA2, false}, // an IC-9700 answering on an IC-7610 config
		{"not implemented", 0x00, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-7610"}})
			if err != nil {
				t.Fatal(err)
			}
			c := &idConn{
				rigAddr: r.rigAddr, ctrlAddr: r.ctrlAddr,
				reported: tc.reported, refuse: tc.refuse,
			}
			r.checkIdentity(t.Context(), c)
			if !c.asked {
				t.Error("Init did not ask the radio for its identity")
			}
		})
	}
}

func TestSetModeRejectsModesTheRadioLacks(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-9700"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetMode(t.Context(), &captureConn{}, radio.ModePSK, false); err == nil {
		t.Error("SetMode accepted PSK on an IC-9700")
	}
	if err := r.SetMode(t.Context(), &captureConn{}, radio.ModeDV, false); err != nil {
		t.Errorf("SetMode(DV) on an IC-9700: %v", err)
	}
}
