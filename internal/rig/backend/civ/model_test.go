package civ

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// captureConn records every request and acknowledges everything, for tests that
// care about what went on the wire rather than what came back.
//
// It keeps the whole conversation rather than just the last frame: a setter may
// send several commands, and asserting only on the last one hides both the
// earlier frames and any command that should not have been sent at all.
type captureConn struct{ sent [][]byte }

func (c *captureConn) Do(_ context.Context, req []byte, _ ...backend.Key) (backend.Update, error) {
	c.sent = append(c.sent, bytes.Clone(req))
	return backend.Update{Key: KeyAck, OK: true}, nil
}

func (c *captureConn) Send(_ context.Context, req []byte) error {
	c.sent = append(c.sent, bytes.Clone(req))
	return nil
}

// last returns the final frame sent, or nil.
func (c *captureConn) last() []byte {
	if len(c.sent) == 0 {
		return nil
	}
	return c.sent[len(c.sent)-1]
}

// commands lists the command (and sub-command where the frame has one) of each
// frame sent, as "1A/06" style strings, for asserting on a whole conversation.
func (c *captureConn) commands() []string {
	var out []string
	for _, f := range c.sent {
		if len(f) < 6 {
			continue
		}
		switch f[4] {
		case cmdMisc, cmdLevel, cmdMeter, cmdTransceiver:
			if len(f) >= 7 {
				out = append(out, fmt.Sprintf("%02X/%02X", f[4], f[5]))
				continue
			}
		}
		out = append(out, fmt.Sprintf("%02X", f[4]))
	}
	return out
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
		if got := len(c.last()) - 6; got != tc.wantLen {
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

// The IC-718 is the model that does not fit the family assumptions, so each of
// its differences is pinned here. All four come from its own command table
// (Advanced Manual section 5).
func TestIC718Differences(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-718"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("PTT is 1C 01", func(t *testing.T) {
		c := &captureConn{}
		if err := r.SetPTT(t.Context(), c, true); err != nil {
			t.Fatalf("SetPTT: %v", err)
		}
		// FE FE to from 1C <sub> <state> FD
		if len(c.last()) != 8 {
			t.Fatalf("frame % X has unexpected length", c.last())
		}
		if c.last()[4] != cmdTransceiver || c.last()[5] != 0x01 || c.last()[6] != 0x01 {
			t.Errorf("SetPTT sent % X, want command 1C sub 01 data 01", c.last())
		}
	})

	t.Run("decodes PTT from 1C 01", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x01, 0x01, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if u.Key != KeyPTT || u.Patch.PTT == nil || !*u.Patch.PTT {
			t.Errorf("decode of % X gave key %q patch %+v", frame, u.Key, u.Patch)
		}
		// 1C 00 is not PTT on this radio and must not be read as it.
		other := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdTransceiver, 0x00, 0x01, 0xFD}
		if u, _ := r.Decode(other); u.Patch.PTT != nil {
			t.Error("1C 00 decoded as PTT on an IC-718")
		}
	})

	t.Run("no CAT CW", func(t *testing.T) {
		if m := r.Caps().CWMethod; m != radio.CWNone {
			t.Errorf("Caps.CWMethod = %q, want none", m)
		}
		if err := r.SendChunk(t.Context(), &captureConn{}, "CQ"); err == nil {
			t.Error("SendChunk succeeded on a radio with no command 17")
		}
		if err := r.Abort(t.Context(), &captureConn{}); err == nil {
			t.Error("Abort succeeded on a radio with no command 17")
		}
	})

	t.Run("no filter width", func(t *testing.T) {
		if r.Caps().FilterWidth {
			t.Error("Caps.FilterWidth = true on a radio with no 1A 03")
		}
		if err := r.SetFilterWidth(t.Context(), &captureConn{}, 500); err == nil {
			t.Error("SetFilterWidth succeeded on a radio with no 1A 03")
		}
	})

	t.Run("keyer runs to 60 wpm", func(t *testing.T) {
		if got := r.Caps().CWMaxWPM; got != 60 {
			t.Errorf("Caps.CWMaxWPM = %d, want 60", got)
		}
	})

	t.Run("no FM and no PSK", func(t *testing.T) {
		caps := r.Caps()
		for _, m := range []radio.Mode{radio.ModeFM, radio.ModePSK, radio.ModePSKR} {
			if caps.SupportsMode(m) {
				t.Errorf("IC-718 should not have mode %s", m)
			}
		}
		if !caps.SupportsMode(radio.ModeCWR) {
			t.Error("IC-718 should have CW-R")
		}
	})
}

// The IC-910H is the only radio here that does not use the family mode table.
// Decoding 0x04 with the family table would report RTTY for a radio sitting in
// FM — a wrong answer rather than a missing one, which is worse.
func TestIC910HModeCodes(t *testing.T) {
	r, err := New(&config.Radio{CIV: &config.CIV{Model: "ic-910h"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("04 is FM, not RTTY", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, 0x04, 0xFD}
		u, err := r.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if u.Patch.Mode == nil || *u.Patch.Mode != radio.ModeFM {
			t.Errorf("decode of mode byte 0x04 gave %v, want FM", u.Patch.Mode)
		}
	})

	t.Run("sets FM as 04 and touches nothing else", func(t *testing.T) {
		c := &captureConn{}
		if err := r.SetMode(t.Context(), c, radio.ModeFM, false); err != nil {
			t.Fatalf("SetMode(FM): %v", err)
		}
		// Exactly one frame: command 06. In particular NOT 1A 06, which is RIT
		// on this radio — sending "data mode off" here would switch RIT off as
		// a side effect of changing mode.
		if got := c.commands(); len(got) != 1 || got[0] != "06" {
			t.Fatalf("SetMode(FM) sent %v, want just [06]", got)
		}
		f := c.sent[0]
		if len(f) < 6 || f[5] != 0x04 {
			t.Errorf("SetMode(FM) sent % X, want mode byte 04", f)
		}
	})

	t.Run("refuses data mode", func(t *testing.T) {
		if err := r.SetMode(t.Context(), &captureConn{}, radio.ModeUSB, true); err == nil {
			t.Error("SetMode accepted data mode on a radio that has none")
		}
	})

	// 1A 06 is RIT here, so an unsolicited one must not land in state as a
	// data-mode change.
	t.Run("1A 06 is not decoded as data mode", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdMisc, subDataMode, 0x01, 0xFD}
		if u, _ := r.Decode(frame); u.Patch.DataMode != nil {
			t.Errorf("1A 06 decoded as data mode = %v on an IC-910H", *u.Patch.DataMode)
		}
	})

	t.Run("05 is not a mode here", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, 0x05, 0xFD}
		if u, _ := r.Decode(frame); u.Patch.Mode != nil {
			t.Errorf("mode byte 0x05 decoded as %v on an IC-910H", *u.Patch.Mode)
		}
	})

	t.Run("no AM, RTTY or CW-R", func(t *testing.T) {
		caps := r.Caps()
		for _, m := range []radio.Mode{radio.ModeAM, radio.ModeFSK, radio.ModeCWR} {
			if caps.SupportsMode(m) {
				t.Errorf("IC-910H should not have mode %s", m)
			}
		}
	})

	t.Run("no filter slots, no CW buffer, 60 wpm", func(t *testing.T) {
		caps := r.Caps()
		if caps.FilterSlots != 0 {
			t.Errorf("FilterSlots = %d, want 0", caps.FilterSlots)
		}
		if caps.CWMethod != radio.CWNone {
			t.Errorf("CWMethod = %q, want none", caps.CWMethod)
		}
		if caps.CWMaxWPM != 60 {
			t.Errorf("CWMaxWPM = %d, want 60", caps.CWMaxWPM)
		}
		if err := r.SetFilterSlot(t.Context(), &captureConn{}, 1); err == nil {
			t.Error("SetFilterSlot succeeded on a radio with no filter selection")
		}
	})

	// A radio with no filter byte must not have a trailing byte read as a slot.
	t.Run("trailing byte is not a filter slot", func(t *testing.T) {
		frame := []byte{0xFE, 0xFE, r.ctrlAddr, r.rigAddr, cmdReadMode, 0x03, 0x02, 0xFD}
		u, _ := r.Decode(frame)
		if u.Patch.FilterSlot != nil {
			t.Errorf("filter slot %d decoded on a radio with none", *u.Patch.FilterSlot)
		}
	})
}

// Every model's declared mode list and its code table must agree, or a mode
// would be advertised that cannot be encoded, or decoded to one never offered.
func TestModeListsMatchCodeTables(t *testing.T) {
	for _, name := range ModelNames() {
		m, err := LookupModel(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(name, func(t *testing.T) {
			for _, mode := range m.Modes {
				if _, ok := m.modeByte(mode); !ok {
					t.Errorf("%s advertises %s but its code table has no byte for it", name, mode)
				}
			}
			if m.Codes == nil {
				return // the family table intentionally covers more than any one radio
			}
			for b, mode := range m.Codes {
				if !m.supportsMode(mode) {
					t.Errorf("%s maps 0x%02X to %s, which it does not advertise", name, b, mode)
				}
			}
		})
	}
}

// Every other supported model keys PTT on 1C 00; only the IC-718 differs. A
// regression here would key the wrong command on a transmitter.
func TestPTTSubCommandPerModel(t *testing.T) {
	for _, name := range ModelNames() {
		m, err := LookupModel(name)
		if err != nil {
			t.Fatal(err)
		}
		want := byte(0x00)
		if name == "ic-718" {
			want = 0x01
		}
		if m.PTTSub != want {
			t.Errorf("%s PTT sub-command = 0x%02X, want 0x%02X", name, m.PTTSub, want)
		}
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
