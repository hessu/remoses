package civ

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// simRig is an in-process IC-7610 that answers CI-V frames, standing in for the
// session's Conn. It holds its values as the wire bytes the rig would send, not
// as decoded numbers, so a mistake in the backend's encoder cannot be cancelled
// out by the same mistake in the simulator.
type simRig struct {
	t *testing.T

	freq   [5]byte
	mode   byte
	filter byte
	data   [2]byte
	power  [2]byte
	smeter [2]byte
	speed  [2]byte
	width  byte
	ptt    byte

	// The dual-VFO side, indexed by the band selector commands 25, 26 and 29
	// take: [0] is main (VFO A), [1] is sub (VFO B). Held separately from the
	// single-VFO fields above because that is how the radio holds them — the
	// point of commands 25 and 26 is that they do not touch the other band.
	bandFreq   [2][5]byte
	bandMode   [2]byte
	bandData   [2]byte
	bandFilter [2]byte
	bandWidth  [2]byte
	subSmeter  [2]byte
	split      byte
	dualWatch  byte

	cwMessages []string
	cwAborts   int

	// echo replays our own frames back at us, as the 13-pin bus does.
	echo bool
	// nak makes the rig reject every command.
	nak bool

	backend *Rig
	log     [][]byte
}

func newSim(t *testing.T) *simRig {
	t.Helper()
	return &simRig{
		t:       t,
		freq:    [5]byte{0x00, 0x50, 0x02, 0x14, 0x00}, // 14.025000 MHz
		mode:    0x03,                                  // CW
		filter:  0x02,                                  // FIL2
		data:    [2]byte{0x00, 0x00},
		power:   [2]byte{0x01, 0x28}, // 128
		smeter:  [2]byte{0x00, 0x42}, // 42
		speed:   [2]byte{0x01, 0x28},
		width:   0x10,
		backend: testRig(t),

		// VFO A on 14.025 CW/FIL2 like the operating fields, VFO B somewhere
		// else entirely on USB/FIL1, so a test that mixes the two up produces
		// an obviously wrong answer rather than a plausible one.
		bandFreq:   [2][5]byte{{0x00, 0x50, 0x02, 0x14, 0x00}, {0x00, 0x00, 0x35, 0x28, 0x00}},
		bandMode:   [2]byte{0x03, 0x01},
		bandFilter: [2]byte{0x02, 0x01},
		// Different widths per band, so a decoder reading one against the
		// other's mode is caught: index 09 is 500 Hz in CW, index 31 is 2700 Hz
		// in USB.
		bandWidth: [2]byte{0x09, 0x31},
		subSmeter: [2]byte{0x00, 0x21}, // 21
	}
}

func (s *simRig) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	if len(want) == 0 {
		return backend.Update{}, fmt.Errorf("sim: Do called with no keys")
	}
	frames, err := s.handle(req)
	if err != nil {
		return backend.Update{}, err
	}
	if s.echo {
		// Our own frame comes back first and must decode to nothing.
		u, _ := s.backend.Decode(req)
		if u.Key != backend.KeyUnsolicited || !u.Patch.Empty() {
			s.t.Errorf("echo of % X decoded as %+v", req, u)
		}
	}
	for _, f := range frames {
		u, err := s.backend.Decode(f)
		if err != nil {
			return u, err
		}
		for _, w := range want {
			if u.Key == w {
				return u, nil
			}
		}
	}
	return backend.Update{}, fmt.Errorf("sim: no reply matching %v to % X", want, req)
}

func (s *simRig) Send(ctx context.Context, req []byte) error {
	_, err := s.handle(req)
	return err
}

// handle decodes a controller frame and returns the frames the rig would send
// back.
func (s *simRig) handle(req []byte) ([][]byte, error) {
	if !wellFormed(req) || req[2] != DefaultRigAddress || req[3] != DefaultControllerAddress {
		return nil, fmt.Errorf("sim: malformed request % X", req)
	}
	s.log = append(s.log, bytes.Clone(req))
	if s.nak {
		return [][]byte{fromRig(codeNG)}, nil
	}
	cmd, body := req[4], req[5:len(req)-1]
	ng := [][]byte{fromRig(codeNG)}
	ok := [][]byte{fromRig(codeOK)}

	switch cmd {
	case cmdReadFreq:
		return [][]byte{fromRig(cmdReadFreq, s.freq[:]...)}, nil
	case cmdSetFreq:
		if len(body) != 5 {
			return ng, nil
		}
		copy(s.freq[:], body)
		return ok, nil
	case cmdReadMode:
		return [][]byte{fromRig(cmdReadMode, s.mode, s.filter)}, nil
	case cmdSetMode:
		if len(body) < 1 {
			return ng, nil
		}
		s.mode = body[0]
		if len(body) >= 2 {
			s.filter = body[1]
		} else {
			s.filter = 0x01 // the rig picks the mode's default filter
		}
		return ok, nil
	case cmdLevel:
		if len(body) < 1 {
			return ng, nil
		}
		dst := map[byte]*[2]byte{subRFPower: &s.power, subKeyerSpeed: &s.speed}[body[0]]
		if dst == nil {
			return ng, nil
		}
		if len(body) == 1 {
			return [][]byte{fromRig(cmdLevel, body[0], dst[0], dst[1])}, nil
		}
		if len(body) != 3 {
			return ng, nil
		}
		copy(dst[:], body[1:])
		return ok, nil
	case cmdMeter:
		if len(body) != 1 || body[0] != subSMeter {
			return ng, nil
		}
		return [][]byte{fromRig(cmdMeter, subSMeter, s.smeter[0], s.smeter[1])}, nil
	case cmdMisc:
		if len(body) < 1 {
			return ng, nil
		}
		switch body[0] {
		case subFilterWidth:
			if len(body) == 1 {
				return [][]byte{fromRig(cmdMisc, subFilterWidth, s.width)}, nil
			}
			s.width = body[1]
			return ok, nil
		case subDataMode:
			if len(body) == 1 {
				return [][]byte{fromRig(cmdMisc, subDataMode, s.data[0], s.data[1])}, nil
			}
			if len(body) != 3 {
				return ng, nil
			}
			copy(s.data[:], body[1:])
			return ok, nil
		}
		return ng, nil
	case cmdTransceiver:
		if len(body) < 1 || body[0] != subPTT {
			return ng, nil
		}
		if len(body) == 1 {
			return [][]byte{fromRig(cmdTransceiver, subPTT, s.ptt)}, nil
		}
		s.ptt = body[1]
		return ok, nil
	case cmdSendCW:
		if len(body) == 1 && body[0] == 0xFF {
			s.cwAborts++
			return ok, nil
		}
		s.cwMessages = append(s.cwMessages, string(body))
		return ok, nil

	case cmdVFO:
		// Only dual watch is modelled; the rest of command 07 is selection and
		// band exchange, which remoses does not send.
		switch {
		case len(body) == 1 && body[0] == subDualWatch:
			return [][]byte{fromRig(cmdVFO, subDualWatch, s.dualWatch)}, nil
		case len(body) == 1 && body[0] == subDualWatchOn:
			s.dualWatch = 0x01
			return ok, nil
		case len(body) == 1 && body[0] == subDualWatchOff:
			s.dualWatch = 0x00
			return ok, nil
		}
		return ng, nil

	case cmdSplit:
		if len(body) == 0 {
			return [][]byte{fromRig(cmdSplit, s.split)}, nil
		}
		s.split = body[0]
		return ok, nil

	case cmdBandFreq:
		if len(body) < 1 || body[0] > 1 {
			return ng, nil
		}
		b := body[0]
		if len(body) == 1 {
			f := s.bandFreq[b]
			return [][]byte{fromRig(cmdBandFreq, append([]byte{b}, f[:]...)...)}, nil
		}
		if len(body) != 6 {
			return ng, nil
		}
		copy(s.bandFreq[b][:], body[1:])
		return ok, nil

	case cmdBandMode:
		if len(body) < 1 || body[0] > 1 {
			return ng, nil
		}
		b := body[0]
		if len(body) == 1 {
			return [][]byte{fromRig(cmdBandMode, b, s.bandMode[b], s.bandData[b], s.bandFilter[b])}, nil
		}
		// The reference allows the data and filter bytes to be omitted, in
		// which case the radio selects DATA OFF and the mode's default filter.
		// Modelled, so that a backend relying on "omit to leave alone" fails
		// here rather than on the radio.
		s.bandMode[b] = body[1]
		if len(body) >= 3 {
			s.bandData[b] = body[2]
		} else {
			s.bandData[b] = 0x00
		}
		if len(body) >= 4 {
			s.bandFilter[b] = body[3]
		} else {
			s.bandFilter[b] = 0x01
		}
		return ok, nil

	case cmdBand:
		// 29 <band> <command...>: answers as the wrapped command, with the
		// prefix echoed back, and works whether or not that band is selected.
		if len(body) < 2 || body[0] > 1 {
			return ng, nil
		}
		b, inner := body[0], body[1:]
		if len(inner) == 2 && inner[0] == cmdMeter && inner[1] == subSMeter {
			m := s.smeter
			if b == bandSub {
				m = s.subSmeter
			}
			return [][]byte{fromRig(cmdBand, b, cmdMeter, subSMeter, m[0], m[1])}, nil
		}
		// 1A 03 behind the prefix: that band's filter width. One width per
		// band, so a decoder reading the wrong one against the wrong mode gives
		// a visibly wrong passband.
		if len(inner) == 2 && inner[0] == cmdMisc && inner[1] == subFilterWidth {
			return [][]byte{fromRig(cmdBand, b, cmdMisc, subFilterWidth, s.bandWidth[b])}, nil
		}
		return ng, nil
	}
	return ng, nil
}

// requests returns the command/sub-command of every frame the rig saw, in the
// form used by the reply keys, so a test can assert on the conversation.
func (s *simRig) requests() []string {
	var out []string
	for _, f := range s.log {
		cmd := f[4]
		body := f[5 : len(f)-1]
		switch cmd {
		case cmdLevel, cmdMeter, cmdMisc, cmdTransceiver:
			if len(body) > 0 {
				out = append(out, fmt.Sprintf("%02X/%02X", cmd, body[0]))
				continue
			}
		}
		out = append(out, fmt.Sprintf("%02X", cmd))
	}
	return out
}

func (s *simRig) wantConversation(t *testing.T, want ...string) {
	t.Helper()
	got := s.requests()
	if len(got) != len(want) {
		t.Fatalf("conversation was %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("conversation was %v, want %v", got, want)
		}
	}
}

func TestRegisteredFactory(t *testing.T) {
	r, err := backend.New(&config.Radio{ID: "ic7610", Backend: "civ"})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	if _, ok := r.(*Rig); !ok {
		t.Fatalf("backend.New returned %T, want *civ.Rig", r)
	}
	if _, ok := r.(backend.MorseSender); !ok {
		t.Error("the CI-V backend does not implement backend.MorseSender")
	}
}

func TestCaps(t *testing.T) {
	c := testRig(t).Caps()
	if c.PowerWattAccurate {
		t.Error("caps claim watt-accurate power; 14 0A is a relative index")
	}
	if c.MaxPowerW != 0 {
		t.Errorf("caps report %v max watts alongside a relative power scale", c.MaxPowerW)
	}
	if c.SMeterScale != 255 {
		t.Errorf("s-meter scale = %d, want 255", c.SMeterScale)
	}
	if c.FilterSlots != 3 {
		t.Errorf("filter slots = %d, want 3", c.FilterSlots)
	}
	if c.CWMethod != radio.CWViaCAT {
		t.Errorf("cw method = %s, want cat", c.CWMethod)
	}
	if c.CWMinWPM != 6 || c.CWMaxWPM != 48 {
		t.Errorf("cw range = %d-%d wpm, want 6-48", c.CWMinWPM, c.CWMaxWPM)
	}
	if c.CWCharset != Charset {
		t.Error("caps do not publish the CW charset")
	}
	for _, m := range []radio.Mode{radio.ModeCW, radio.ModeCWR, radio.ModeUSB, radio.ModePSKR} {
		if !c.SupportsMode(m) {
			t.Errorf("caps omit %s", m)
		}
	}
	if c.SupportsMode(radio.ModeUnknown) {
		t.Error("caps claim to support the unknown mode")
	}
	// The slices must not be shared between calls: Caps is published.
	c.Modes[0] = radio.ModeUnknown
	if testRig(t).Caps().Modes[0] == radio.ModeUnknown {
		t.Error("Caps shares its mode slice between calls")
	}
}

func TestInitReadsFullState(t *testing.T) {
	s := newSim(t)
	s.echo = true // the worst case: everything we send comes back at us
	if err := s.backend.Init(context.Background(), s); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// 19 is the identity cross-check, which runs first so that a mismatch is
	// reported before any state is published. The 25/26/0F/07 tail is the
	// dual-VFO read, which the IC-7610 profile has and most models do not — it
	// runs at connect so the first client to look sees the second VFO filled
	// in, and so that dual watch is settled before the first fast poll decides
	// whether to ask for the sub meter.
	// The 29 pair is each VFO's filter width, read behind the prefix so neither
	// band has to be selected — and after the 26 reads, because turning a width
	// index into hertz needs that VFO's own mode.
	s.wantConversation(t, "19", "03", "04", "14/0A", "15/02", "1C/00",
		"25", "25", "26", "26", "29", "29", "0F", "07")
}

func TestInitFailsWhenTheRigRejects(t *testing.T) {
	s := newSim(t)
	s.nak = true
	err := s.backend.Init(context.Background(), s)
	if err == nil {
		t.Fatal("Init succeeded against a rig that rejects everything")
	}
	if !strings.Contains(err.Error(), "03") {
		t.Errorf("error %q does not name the failing command", err)
	}
}

func TestPoll(t *testing.T) {
	tests := []struct {
		name string
		tier backend.PollTier
		want []string
	}{
		{"fast", backend.PollFast, []string{"03", "04", "1C/00", "15/02"}},
		// 1A 06 is in the slow tier because data mode has no other source: no
		// other answer carries the flag and the rig does not broadcast it, so
		// without the read state.data_mode would never move.
		// 25/26 twice each is one frequency and one mode per VFO; 0F is split
		// and 07 is dual watch. All of it is slow-tier because it moves only
		// when somebody changes it, and four extra transactions have no
		// business on a 500 ms tick.
		{"slow", backend.PollSlow, []string{"14/0A", "1A/03",
			"25", "25", "26", "26", "0F", "07", "1A/06"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			if err := s.backend.Poll(context.Background(), s, tc.tier); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			s.wantConversation(t, tc.want...)
		})
	}
}

func TestPollUnknownTier(t *testing.T) {
	s := newSim(t)
	if err := s.backend.Poll(context.Background(), s, backend.PollTier(99)); err == nil {
		t.Error("Poll accepted an unknown tier")
	}
}

func TestSetFrequency(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetFrequency(context.Background(), s, radio.VFOCurrent, 7_030_000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	// 7.030000 MHz: the 10 kHz digit is 3, so it lands in the low nibble of the
	// third byte, not in the second.
	if s.freq != [5]byte{0x00, 0x00, 0x03, 0x07, 0x00} {
		t.Errorf("rig frequency = % X, want 00 00 03 07 00", s.freq)
	}
}

func TestSetFrequencyRejects(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetFrequency(context.Background(), s, radio.VFOB, 7_030_000); err == nil {
		t.Error("SetFrequency accepted VFO B, which command 05 cannot address")
	}
	if err := s.backend.SetFrequency(context.Background(), s, radio.VFOCurrent, maxFrequencyHz+1); err == nil {
		t.Error("SetFrequency accepted a frequency the field cannot hold")
	}
	if len(s.log) != 0 {
		t.Errorf("a rejected request still put %d frames on the wire", len(s.log))
	}
}

func TestSetMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     radio.Mode
		data     bool
		wantMode byte
		wantData [2]byte
		wantConv []string
	}{
		{
			// The sim is already in CW, so no mode goes out at all. Command 06
			// carries no filter byte, and the rig answers one by reverting to
			// the mode's default filter — so re-sending the mode the rig is in
			// would move the operator's filter for nothing.
			name: "cw is already set, so nothing is sent", mode: radio.ModeCW, data: false,
			wantMode: 0x03, wantData: [2]byte{0x00, 0x00},
			wantConv: []string{"04"},
		},
		{
			name: "ssb clears the data setting", mode: radio.ModeUSB, data: false,
			wantMode: 0x01, wantData: [2]byte{0x00, 0x00},
			wantConv: []string{"04", "06", "1A/06"},
		},
		{
			// Turning data on needs a filter byte, so after a real mode change
			// the mode is read again to find the filter the rig just selected.
			name: "ssb data reads the filter back", mode: radio.ModeUSB, data: true,
			wantMode: 0x01, wantData: [2]byte{0x01, 0x01},
			wantConv: []string{"04", "06", "04", "1A/06"},
		},
		{
			name: "rtty maps to fsk", mode: radio.ModeFSK, data: false,
			wantMode: 0x04, wantData: [2]byte{0x00, 0x00},
			wantConv: []string{"04", "06"},
		},
		{
			name: "psk is a literal 0x12", mode: radio.ModePSK, data: false,
			wantMode: 0x12, wantData: [2]byte{0x00, 0x00},
			wantConv: []string{"04", "06"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(t)
			if err := s.backend.SetMode(context.Background(), s, tc.mode, tc.data); err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			if s.mode != tc.wantMode {
				t.Errorf("rig mode = %#02X, want %#02X", s.mode, tc.wantMode)
			}
			if s.data != tc.wantData {
				t.Errorf("rig data setting = % X, want % X", s.data, tc.wantData)
			}
			s.wantConversation(t, tc.wantConv...)
		})
	}
}

func TestSetModeRejects(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetMode(context.Background(), s, radio.ModeUnknown, false); err == nil {
		t.Error("SetMode accepted an unknown mode")
	}
	if err := s.backend.SetMode(context.Background(), s, radio.ModeCW, true); err == nil {
		t.Error("SetMode accepted data mode in CW")
	}
	if len(s.log) != 0 {
		t.Errorf("a rejected request still put %d frames on the wire", len(s.log))
	}
}

func TestSetPower(t *testing.T) {
	tests := []struct {
		pct  float64
		want [2]byte
	}{
		{0, [2]byte{0x00, 0x00}},
		{50, [2]byte{0x01, 0x28}},  // 127.5 rounds to 128
		{100, [2]byte{0x02, 0x55}}, // the full relative scale
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%.0f%%", tc.pct), func(t *testing.T) {
			s := newSim(t)
			pct := tc.pct
			if err := s.backend.SetPower(context.Background(), s, radio.PowerSet{Pct: &pct}); err != nil {
				t.Fatalf("SetPower: %v", err)
			}
			if s.power != tc.want {
				t.Errorf("rig power = % X, want % X", s.power, tc.want)
			}
		})
	}
}

func TestSetPowerRejects(t *testing.T) {
	s := newSim(t)
	watts := 50.0
	err := s.backend.SetPower(context.Background(), s, radio.PowerSet{Watts: &watts})
	if err == nil {
		t.Error("SetPower accepted watts on a rig with no watt scale")
	} else if !strings.Contains(err.Error(), "percentage") {
		t.Errorf("error %q does not tell the caller what to send instead", err)
	}
	over := 101.0
	if err := s.backend.SetPower(context.Background(), s, radio.PowerSet{Pct: &over}); err == nil {
		t.Error("SetPower accepted more than 100%")
	}
	if err := s.backend.SetPower(context.Background(), s, radio.PowerSet{}); err == nil {
		t.Error("SetPower accepted a request with neither watts nor percent")
	}
	if len(s.log) != 0 {
		t.Errorf("a rejected request still put %d frames on the wire", len(s.log))
	}
}

func TestSetPTT(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetPTT(context.Background(), s, true); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	if s.ptt != 0x01 {
		t.Errorf("rig PTT = %#02X, want 01", s.ptt)
	}
	if err := s.backend.SetPTT(context.Background(), s, false); err != nil {
		t.Fatalf("SetPTT: %v", err)
	}
	if s.ptt != 0x00 {
		t.Errorf("rig PTT = %#02X, want 00", s.ptt)
	}
	s.wantConversation(t, "1C/00", "1C/00")
}

func TestSetFilterSlot(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SetFilterSlot(context.Background(), s, 3); err != nil {
		t.Fatalf("SetFilterSlot: %v", err)
	}
	if s.filter != 0x03 {
		t.Errorf("rig filter = %#02X, want 03", s.filter)
	}
	if s.mode != 0x03 {
		t.Errorf("rig mode changed to %#02X while selecting a filter", s.mode)
	}
	s.wantConversation(t, "04", "06")

	for _, bad := range []int{0, 4} {
		if err := s.backend.SetFilterSlot(context.Background(), s, bad); err == nil {
			t.Errorf("SetFilterSlot accepted slot %d", bad)
		}
	}
}

func TestSetFilterWidth(t *testing.T) {
	s := newSim(t)
	// The rig is in CW, where 250 Hz is index 4.
	if err := s.backend.SetFilterWidth(context.Background(), s, 250); err != nil {
		t.Fatalf("SetFilterWidth: %v", err)
	}
	if s.width != 0x04 {
		t.Errorf("rig filter width index = %#02X, want 04", s.width)
	}
	s.wantConversation(t, "04", "1A/03")

	// The same width means a different index in AM, which is why the mode is
	// read first.
	s = newSim(t)
	s.mode = 0x02 // AM
	if err := s.backend.SetFilterWidth(context.Background(), s, 1000); err != nil {
		t.Fatalf("SetFilterWidth: %v", err)
	}
	if s.width != bcdByte(4) {
		t.Errorf("rig filter width index = %#02X, want %#02X", s.width, bcdByte(4))
	}
}

func TestSetFilterWidthRejectsFM(t *testing.T) {
	s := newSim(t)
	s.mode = 0x05 // FM
	if err := s.backend.SetFilterWidth(context.Background(), s, 12000); err == nil {
		t.Error("SetFilterWidth accepted FM, whose filters are fixed")
	}
}

func TestSetsReportRejection(t *testing.T) {
	s := newSim(t)
	s.nak = true
	err := s.backend.SetPTT(context.Background(), s, true)
	if err == nil {
		t.Fatal("SetPTT succeeded against a rig that rejected it")
	}
	if !strings.Contains(err.Error(), "PTT") {
		t.Errorf("error %q does not name the rejected command", err)
	}
}

// errConn is a session whose transactions always fail, as they do when the port
// has dropped or a command times out.
type errConn struct{ err error }

func (e errConn) Do(ctx context.Context, req []byte, want ...backend.Key) (backend.Update, error) {
	return backend.Update{}, e.err
}
func (e errConn) Send(ctx context.Context, req []byte) error { return e.err }

func TestTransactionErrorsAreWrapped(t *testing.T) {
	r := testRig(t)
	c := errConn{err: errors.New("port closed")}
	ctx := context.Background()
	calls := []struct {
		name string
		call func() error
	}{
		{"Init", func() error { return r.Init(ctx, c) }},
		{"Poll", func() error { return r.Poll(ctx, c, backend.PollFast) }},
		{"SetFrequency", func() error { return r.SetFrequency(ctx, c, radio.VFOCurrent, 14_025_000) }},
		{"SetMode", func() error { return r.SetMode(ctx, c, radio.ModeCW, false) }},
		{"SetPTT", func() error { return r.SetPTT(ctx, c, true) }},
		{"SetFilterSlot", func() error { return r.SetFilterSlot(ctx, c, 2) }},
		{"SetFilterWidth", func() error { return r.SetFilterWidth(ctx, c, 500) }},
		{"SendChunk", func() error { return r.SendChunk(ctx, c, "TEST") }},
		{"Abort", func() error { return r.Abort(ctx, c) }},
		{"SetSpeed", func() error { return r.SetSpeed(ctx, c, 25) }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a failed transaction was reported as success")
			}
			if !errors.Is(err, c.err) {
				t.Errorf("error %q loses the underlying cause", err)
			}
			if !strings.HasPrefix(err.Error(), "civ:") {
				t.Errorf("error %q is not attributed to the backend", err)
			}
		})
	}
}

// TestSetModeDataFailureWhenFilterUnreadable covers the read-back that turning
// data mode on depends on.
func TestSetModeDataFailure(t *testing.T) {
	s := newSim(t)
	s.mode = 0x01
	// A rig that answers the mode read with something unrecognisable must not
	// leave the caller believing data mode was set.
	s.filter = 0x09
	if err := s.backend.SetMode(context.Background(), s, radio.ModeUSB, true); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	// The filter byte was out of range, so it is ignored and FIL1 assumed.
	if s.data != [2]byte{0x01, 0x01} {
		t.Errorf("data setting = % X, want 01 01", s.data)
	}
}

// TestReadRejectsUnexpectedReply covers the rig answering with the wrong frame:
// the caller must be told rather than handed somebody else's state.
func TestReadRejectsUnexpectedReply(t *testing.T) {
	s := newSim(t)
	_, err := s.backend.read(context.Background(), s, KeyFrequency, s.backend.frame(cmdReadMode))
	if err == nil {
		t.Error("read accepted a reply to a different command")
	}
}

// TestSlowPollSkipsWhatAModelLacks covers the two guards on the slow tier, and
// the second one is not an optimisation.
//
// 1A 03 on a radio without it just draws an NG, which is noise. 1A 06 on an
// IC-910H is RIT, so polling it there would read an RIT setting and publish it
// every slow tick as a data-mode change — a wrong value in state rather than a
// missing one, which is the failure DESIGN.md §5.4 records for that radio.
func TestSlowPollSkipsWhatAModelLacks(t *testing.T) {
	for _, model := range []string{"ic-718", "ic-910h"} {
		t.Run(model, func(t *testing.T) {
			s := newSim(t)
			r, err := New(&config.Radio{
				ID:      "rig",
				Backend: "civ",
				CIV:     &config.CIV{Model: model, RigAddress: int(s.backend.rigAddr), ControllerAddress: int(s.backend.ctrlAddr)},
			})
			if err != nil {
				t.Fatalf("New(%s): %v", model, err)
			}
			s.backend = r

			if err := r.Poll(context.Background(), s, backend.PollSlow); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			// Power only: neither radio has 1A 03, and neither has a data mode
			// on 1A 06.
			s.wantConversation(t, "14/0A")
		})
	}
}

// TestDataModeOnlyChangeKeepsTheFilter is the first of the two bugs an IC-7610
// found, and it is the reason SetMode reads before it writes.
//
// Data mode is orthogonal at the API layer, so a request that changes only the
// flag arrives here as SetMode(the mode the rig is already in, flag). Command
// 06 carries no filter byte and the rig answers one by reverting to that mode's
// default, so the old unconditional 06 moved the operator's filter as a side
// effect of touching data mode. On the radio, a data-mode-only PATCH on
// USB/FIL1 came back on FIL2.
func TestDataModeOnlyChangeKeepsTheFilter(t *testing.T) {
	s := newSim(t)
	s.mode = 0x01   // USB
	s.filter = 0x01 // FIL1

	if err := s.backend.SetMode(context.Background(), s, radio.ModeUSB, true); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	// No 06 anywhere: the mode did not change, so nothing may be sent that
	// could disturb the filter.
	s.wantConversation(t, "04", "1A/06")
	if s.filter != 0x01 {
		t.Errorf("filter moved to %#02X; turning data mode on must not touch it", s.filter)
	}
	if s.data != [2]byte{0x01, 0x01} {
		t.Errorf("data setting = % X, want 01 01 (on, FIL1)", s.data)
	}
}

// TestSetFilterSlotKeepsDataMode is the second bug, and the worse one: choosing
// a filter quietly dropped the operator out of USB-D.
//
// Command 06 sets the mode and resets the 1A 06 data flag with it, so in a data
// mode the filter has to be moved with 1A 06 instead — which carries a filter
// byte of its own and leaves the mode alone.
func TestSetFilterSlotKeepsDataMode(t *testing.T) {
	s := newSim(t)
	s.mode = 0x01                // USB
	s.filter = 0x01              // FIL1
	s.data = [2]byte{0x01, 0x01} // data mode on, FIL1

	if err := s.backend.SetFilterSlot(context.Background(), s, 3); err != nil {
		t.Fatalf("SetFilterSlot: %v", err)
	}
	// Read the mode, read the data setting, then move the filter with 1A 06.
	// A 06 here would have cleared the data flag.
	s.wantConversation(t, "04", "1A/06", "1A/06")
	if s.data != [2]byte{0x01, 0x03} {
		t.Errorf("data setting = % X, want 01 03 (still on, now FIL3)", s.data)
	}
}

// TestSetFilterSlotOutsideDataModeUsesCommand06 is the other half of the rule.
// 1A 06 is not valid in CW, and on an IC-910H it is RIT, so the mode has to be
// re-sent with the filter there — which changes nothing, being the mode the rig
// is already in.
func TestSetFilterSlotOutsideDataModeUsesCommand06(t *testing.T) {
	s := newSim(t) // CW, and CW carries no data setting
	if err := s.backend.SetFilterSlot(context.Background(), s, 3); err != nil {
		t.Fatalf("SetFilterSlot: %v", err)
	}
	// No 1A 06 read at all: in CW there is no data setting to preserve.
	s.wantConversation(t, "04", "06")
	if s.filter != 0x03 {
		t.Errorf("filter = %#02X, want 03", s.filter)
	}
	if s.mode != 0x03 {
		t.Errorf("mode = %#02X, want it left at CW", s.mode)
	}
}

// TestUSBDataOnFIL1IsReachable is the bug the other two added up to. Neither
// order of operations could express it on the radio: filter-then-data landed on
// FIL2 with data on, and both-in-one-patch landed on FIL1 with data off.
func TestUSBDataOnFIL1IsReachable(t *testing.T) {
	for _, order := range []string{"filter first", "data first"} {
		t.Run(order, func(t *testing.T) {
			s := newSim(t)
			s.mode = 0x01 // USB
			ctx := context.Background()

			if order == "filter first" {
				if err := s.backend.SetFilterSlot(ctx, s, 1); err != nil {
					t.Fatalf("SetFilterSlot: %v", err)
				}
				if err := s.backend.SetMode(ctx, s, radio.ModeUSB, true); err != nil {
					t.Fatalf("SetMode: %v", err)
				}
			} else {
				if err := s.backend.SetMode(ctx, s, radio.ModeUSB, true); err != nil {
					t.Fatalf("SetMode: %v", err)
				}
				if err := s.backend.SetFilterSlot(ctx, s, 1); err != nil {
					t.Fatalf("SetFilterSlot: %v", err)
				}
			}

			if s.mode != 0x01 {
				t.Errorf("mode = %#02X, want USB", s.mode)
			}
			if s.data[0] != 0x01 {
				t.Errorf("data mode is off; USB-D on FIL1 is not expressible")
			}
			if s.data[1] != 0x01 {
				t.Errorf("data filter = %#02X, want FIL1", s.data[1])
			}
		})
	}
}
