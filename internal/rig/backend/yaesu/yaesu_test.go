package yaesu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The backend contract, checked at compile time.
var _ backend.Rig = (*Rig)(nil)

// TestNoMorseSender is a promise, not an omission.
//
// No Yaesu in this set has a CAT command that streams arbitrary Morse: KY plays
// a stored keyer memory, and the text lives in KM, which is the operator's own
// saved messages and which remoses never writes. Implementing MorseSender
// partially would be worse than not implementing it, because the daemon's type
// assertion would succeed and every message would draw a failure that looks to
// the operator like it was sent — the IC-718 lesson in DESIGN.md §5.4.
func TestNoMorseSender(t *testing.T) {
	var r backend.Rig = newModelRig(t, "ft-710")
	if _, ok := r.(backend.MorseSender); ok {
		t.Fatal("the yaesu backend implements MorseSender; no radio here has a CAT CW buffer")
	}
	for _, m := range ModelNames() {
		if got := newModelRig(t, m).Caps().CWMethod; got != radio.CWNone {
			t.Errorf("%s CWMethod = %q, want %q so the daemon steers to serial_key", m, got, radio.CWNone)
		}
	}
}

// TestNeverWritesKeyerMemory pins the other half of the same decision at the
// source level: KM writes the operator's stored CW messages, which remoses does
// not do — connecting to a radio must not destroy what is saved in it.
//
// Every model, because every one of the twelve has KM and KY and the temptation
// is identical on all of them. EX is checked with them: writing a menu item is
// the same rule (DESIGN.md §5, the rule at the head), and it is the other thing
// remoses could reach for to make a command work.
func TestNeverWritesKeyerMemory(t *testing.T) {
	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, name)
			c := newTestConn(t, y, answersFor(y.profile))
			ctx := context.Background()

			if err := y.Init(ctx, c); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if err := y.Poll(ctx, c, backend.PollFast); err != nil {
				t.Fatalf("PollFast: %v", err)
			}
			if err := y.Poll(ctx, c, backend.PollSlow); err != nil {
				t.Fatalf("PollSlow: %v", err)
			}
			if err := y.SetMode(ctx, c, radio.ModeCW, false); err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			if err := y.SetFrequency(ctx, c, radio.VFOA, 14_025_000); err != nil {
				t.Fatalf("SetFrequency: %v", err)
			}
			if err := y.SetPTT(ctx, c, true); err != nil {
				t.Fatalf("SetPTT: %v", err)
			}
			for _, req := range c.sent {
				for _, forbidden := range []string{"KM", "KY", "EX"} {
					if strings.HasPrefix(req, forbidden) {
						t.Errorf("wrote %q; keyer memories and menu items belong to the operator", req)
					}
				}
			}
		})
	}
}

func TestBackendRegistered(t *testing.T) {
	r, err := backend.New(&config.Radio{
		ID:      "rig",
		Backend: Name,
		Yaesu:   &config.Yaesu{Model: "ftdx101mp", AutoInformation: true},
	})
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	if got := r.Caps().MaxPowerW; got != 200 {
		t.Errorf("MaxPowerW = %v, want 200; the registered factory did not read the model", got)
	}
	if _, err := backend.New(&config.Radio{ID: "rig", Backend: Name,
		Yaesu: &config.Yaesu{Model: "ft-857"}}); err == nil {
		t.Error("backend.New accepted a model with no profile")
	}
}

func TestInitPerModel(t *testing.T) {
	// The order is load-bearing at the end: MD0; settles the mode and NA0; the
	// narrow setting, and neither SH; answer means anything without both.
	want := []string{"AI1;", "ID;", "FA;", "MD0;", "PC;", "TX;", "NA0;", "SH0;"}
	// The FTdx9000 has no ID and no NA command, and no bandwidth table for SH
	// to index, so three of the eight are simply not sent. Asking anyway would
	// cost a full per-command timeout each — and the ID; read would then fail
	// the connect outright. AI it does have, so push updates are enabled there
	// like everywhere else.
	wantFTdx9000 := []string{"AI1;", "FA;", "MD0;", "PC;", "TX;"}

	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, name)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.Init(context.Background(), c); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if name == "ftdx9000" {
				c.wantSent(t, wantFTdx9000...)
			} else {
				c.wantSent(t, want...)
			}
			if y.lastMode() != radio.ModeCW {
				t.Errorf("mode = %s after Init, want CW", y.lastMode())
			}
			// AI2 is Kenwood's spelling and a syntax error on a Yaesu.
			for _, req := range c.sent {
				if req == "AI2;" || req == "RX;" {
					t.Errorf("Init sent %q, which is not a Yaesu command", req)
				}
			}
		})
	}
}

func TestInitAutoInformationOff(t *testing.T) {
	y := newModelRig(t, "ft-710")
	y.ai = false
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if c.sent[0] != "AI0;" {
		t.Errorf("first request = %q, want AI0;", c.sent[0])
	}
}

// TestInitFTdx9000UsesAIButNotIDOrNA covers that radio's capability list where
// it differs from the family: it takes AI, so it pushes changes and is not
// poll-only, and it has neither ID nor NA, so neither may ever reach the wire —
// a Yaesu answers a command it does not implement with silence, and the ID;
// read would fail the connect after a full per-command timeout.
func TestInitFTdx9000UsesAIButNotIDOrNA(t *testing.T) {
	for _, tt := range []struct {
		ai   bool
		want string
	}{
		{true, "AI1;"},
		{false, "AI0;"},
	} {
		y := newModelRig(t, "ftdx9000")
		y.ai = tt.ai
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.Init(context.Background(), c); err != nil {
			t.Fatalf("Init: %v", err)
		}
		if len(c.sent) == 0 || c.sent[0] != tt.want {
			t.Errorf("Init(ai=%v) sent %q, want %q first", tt.ai, c.sent, tt.want)
		}
		for _, req := range c.sent {
			if strings.HasPrefix(req, "ID") || strings.HasPrefix(req, "NA") {
				t.Errorf("Init(ai=%v) sent %q; the FTdx9000 has no such command", tt.ai, req)
			}
		}
	}
}

// TestInitSkipsFilterWidthWhereItHasNone covers the one conditional read. A
// Yaesu answers a command it will not take with silence, so asking for SH in FM
// would cost a full session timeout every time the operator sat in FM.
func TestInitSkipsFilterWidthWhereItHasNone(t *testing.T) {
	y := newModelRig(t, "ft-710")
	answers := answersFor(y.profile)
	answers[reqMD] = "MD04" // FM
	c := newTestConn(t, y, answers)
	if err := y.Init(context.Background(), c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	c.wantSent(t, "AI1;", "ID;", "FA;", "MD0;", "PC;", "TX;", "NA0;")
}

// TestInitFailsOnASilentRig is what a dead link looks like from inside the
// backend, and the reason Init reads ID; second — or, on the one radio with no
// ID command, FA; first.
func TestInitFailsOnASilentRig(t *testing.T) {
	for _, tt := range []struct{ model, want string }{
		{"ft-710", reqID},
		{"ftdx9000", reqFA},
	} {
		y := newModelRig(t, tt.model)
		c := newTestConn(t, y, nil)
		err := y.Init(context.Background(), c)
		if err == nil {
			t.Fatalf("%s: Init succeeded against a rig that answered nothing", tt.model)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: error %q does not name the request that failed", tt.model, err)
		}
	}
}

// TestPollFast pins the three transactions and why there are three. Kenwood
// needs two because its IF carries a TX/RX flag; Yaesu's does not carry one
// anywhere, so PTT has to be asked for separately.
func TestPollFast(t *testing.T) {
	for _, name := range ModelNames() {
		t.Run(name, func(t *testing.T) {
			y := newModelRig(t, name)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.Poll(context.Background(), c, backend.PollFast); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, "IF;", "TX;", "SM0;")
		})
	}
}

func TestPollSlow(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want []string
	}{
		// NA before SH: it decides which column of the bandwidth table the
		// index is read against.
		{name: "CW", mode: "MD03", want: []string{"PC;", "NA0;", "SH0;"}},
		{name: "USB", mode: "MD02", want: []string{"PC;", "NA0;", "SH0;"}},
		{name: "USB-DATA", mode: "MD0C", want: []string{"PC;", "NA0;", "SH0;"}},
		// SH has no bandwidth table in AM or FM on any model here.
		{name: "FM", mode: "MD04", want: []string{"PC;", "NA0;"}},
		{name: "AM", mode: "MD05", want: []string{"PC;", "NA0;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, "ftdx101d")
			mustDecode(t, y, tt.mode)
			c := newTestConn(t, y, answersFor(y.profile))
			if err := y.Poll(context.Background(), c, backend.PollSlow); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestPollUnknownTier(t *testing.T) {
	y := newModelRig(t, "generic")
	if err := y.Poll(context.Background(), newTestConn(t, y, nil), backend.PollTier(42)); err == nil {
		t.Fatal("Poll accepted an unknown tier")
	}
}

// TestSetPTT is the single most important test in this package.
//
// TX1; keys and TX0; unkeys. A bare TX; is the READ — it is what a Kenwood uses
// to key, and sending it here would answer a query while the operator believed
// the transmitter was on. There is no RX; command at all.
func TestSetPTT(t *testing.T) {
	for _, tt := range []struct {
		on   bool
		want string
	}{
		{true, "TX1;"},
		{false, "TX0;"},
	} {
		for _, name := range ModelNames() {
			y := newModelRig(t, name)
			c := newTestConn(t, y, nil)
			if err := y.SetPTT(context.Background(), c, tt.on); err != nil {
				t.Fatalf("%s: SetPTT(%v): %v", name, tt.on, err)
			}
			// One write and no read-back: a set answers nothing unless AI is
			// on, so waiting would stall the transmit path until the timeout.
			c.wantSent(t, tt.want)
			for _, req := range c.sent {
				if req == "TX;" || req == "RX;" {
					t.Fatalf("%s: SetPTT sent %q — TX; is the read and RX; is not a Yaesu command", name, req)
				}
			}
		}
	}
}

func TestSetFrequency(t *testing.T) {
	tests := []struct {
		name  string
		model string
		vfo   radio.VFO
		hz    uint64
		want  []string
		fail  bool
	}{
		{name: "VFO A", model: "ft-710", vfo: radio.VFOA, hz: 14_025_000,
			want: []string{"FA014025000;", "FA;"}},
		{name: "current is VFO A", model: "ft-710", vfo: radio.VFOCurrent, hz: 14_025_000,
			want: []string{"FA014025000;", "FA;"}},
		{name: "VFO B", model: "ft-710", vfo: radio.VFOB, hz: 14_100_000,
			want: []string{"FB014100000;", "FB;"}},
		// The FT-950 generation takes eight digits. Sending nine would be a
		// malformed command, and a Yaesu answers one of those with silence.
		{name: "eight digits on an FT-950", model: "ft-950", vfo: radio.VFOA, hz: 14_025_000,
			want: []string{"FA14025000;", "FA;"}},
		{name: "eight digits at the bottom", model: "ftdx1200", vfo: radio.VFOA, hz: 30_000,
			want: []string{"FA00030000;", "FA;"}},
		{name: "eight digits on VFO B", model: "ftdx5000", vfo: radio.VFOB, hz: 14_100_000,
			want: []string{"FB14100000;", "FB;"}},
		{name: "eight digits on the FTdx9000", model: "ftdx9000", vfo: radio.VFOA, hz: 7_030_000,
			want: []string{"FA07030000;", "FA;"}},
		{name: "6 m on an FT-950", model: "ft-950", vfo: radio.VFOA, hz: 50_100_000,
			want: []string{"FA50100000;", "FA;"}},
		// 57 MHz is inside the FTdx3000's range and outside the FT-950's.
		{name: "above the FT-950's ceiling", model: "ft-950", vfo: radio.VFOA, hz: 57_000_000, fail: true},
		{name: "inside the FTdx3000's", model: "ftdx3000", vfo: radio.VFOA, hz: 57_000_000,
			want: []string{"FA57000000;", "FA;"}},
		{name: "70 cm on a radio that has it", model: "ft-991a", vfo: radio.VFOA, hz: 435_000_000,
			want: []string{"FA435000000;", "FA;"}},
		// The range check is remoses's own: an out-of-range frequency would not
		// be refused, it would be answered with silence and a full timeout.
		{name: "2 m on an HF radio", model: "ft-891", vfo: radio.VFOA, hz: 144_200_000, fail: true},
		{name: "below the bottom", model: "ft-710", vfo: radio.VFOA, hz: 1_000, fail: true},
		{name: "no such VFO", model: "ft-710", vfo: radio.VFOSub, hz: 14_025_000, fail: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			c := newTestConn(t, y, map[string]string{
				reqFA: "FA014025000",
				reqFB: "FB014100000",
			})
			err := y.SetFrequency(context.Background(), c, tt.vfo, tt.hz)
			if tt.fail {
				if err == nil {
					t.Fatal("SetFrequency accepted a request it cannot make")
				}
				if len(c.sent) != 0 {
					t.Errorf("wrote %q anyway", c.sent)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetFrequency: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

// TestSetMode covers the shape of the write and the absence of a follow-up.
// Yaesu has no DA command: DATA is inside the mode code, so USB-DATA is one
// write rather than a mode and then a flag.
func TestSetMode(t *testing.T) {
	tests := []struct {
		name  string
		model string
		mode  radio.Mode
		data  bool
		want  []string
	}{
		{"CW", "ft-710", radio.ModeCW, false, []string{"MD03;", "MD0;"}},
		{"USB", "ft-710", radio.ModeUSB, false, []string{"MD02;", "MD0;"}},
		{"USB-DATA", "ft-710", radio.ModeUSB, true, []string{"MD0C;", "MD0;"}},
		{"LSB-DATA", "ft-710", radio.ModeLSB, true, []string{"MD08;", "MD0;"}},
		{"DATA-FM", "ft-710", radio.ModeFM, true, []string{"MD0A;", "MD0;"}},
		{"PSK", "ft-710", radio.ModePSK, false, []string{"MD0E;", "MD0;"}},
		// The same E on the radio where it means something else.
		{"C4FM", "ft-991a", radio.ModeC4FM, false, []string{"MD0E;", "MD0;"}},
		{"C4FM-DN", "ftx-1", radio.ModeC4FMDN, false, []string{"MD0H;", "MD0;"}},
		{"C4FM-VW", "ftx-1", radio.ModeC4FMVW, false, []string{"MD0I;", "MD0;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			code, err := y.profile.encodeMode(tt.mode, tt.data)
			if err != nil {
				t.Fatalf("encodeMode: %v", err)
			}
			c := newTestConn(t, y, map[string]string{reqMD: "MD0" + string(code)})
			if err := y.SetMode(context.Background(), c, tt.mode, tt.data); err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			c.wantSent(t, tt.want...)
			// The read-back is the authority on what the rig took.
			if y.lastMode() != tt.mode || y.dataMode.Load() != tt.data {
				t.Errorf("after SetMode the cached mode is %s data=%v, want %s data=%v",
					y.lastMode(), y.dataMode.Load(), tt.mode, tt.data)
			}
		})
	}
}

func TestSetModeRefusedWritesNothing(t *testing.T) {
	y := newModelRig(t, "ft-991a")
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetMode(context.Background(), c, radio.ModePSK, false); err == nil {
		t.Fatal("SetMode(PSK) succeeded on an FT-991A, whose E is C4FM")
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q despite rejecting the request", c.sent)
	}
}

func TestSetPower(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name  string
		model string
		set   radio.PowerSet
		want  string
	}{
		{"watts", "ft-710", watts(50), "PC050;"},
		{"full scale", "ft-710", pct(100), "PC100;"},
		{"clamped", "ft-710", watts(150), "PC100;"},
		{"floor", "ft-710", watts(1), "PC005;"},
		// The one 200 W radio here: 100 W is half power on it and full power
		// everywhere else.
		{"200 W radio", "ftdx101mp", pct(100), "PC200;"},
		{"200 W radio at half", "ftdx101mp", pct(50), "PC100;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			c := newTestConn(t, y, map[string]string{reqPC: "PC050"})
			if err := y.SetPower(context.Background(), c, tt.set); err != nil {
				t.Fatalf("SetPower: %v", err)
			}
			c.wantSent(t, tt.want, "PC;")
		})
	}
}

// TestSetPowerFTX1Head covers the model whose PC has a different shape. The
// head selector says which amplifier chain the value applies to, so remoses
// sends back whichever the rig reported rather than choosing one — and until it
// has reported, the bare 10 W head is assumed, which clamps low.
func TestSetPowerFTX1Head(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }

	y := newModelRig(t, "ftx-1")
	c := newTestConn(t, y, map[string]string{reqPC: "PC1010"})
	if err := y.SetPower(context.Background(), c, watts(50)); err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	c.wantSent(t, "PC1010;", "PC;")

	// With the SPA-1 reported, the ceiling and the selector both move.
	y = newModelRig(t, "ftx-1")
	mustDecode(t, y, "PC2050")
	c = newTestConn(t, y, map[string]string{reqPC: "PC2100"})
	if err := y.SetPower(context.Background(), c, watts(100)); err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	c.wantSent(t, "PC2100;", "PC;")

	// Every other watt-scaled model sends the plain three-digit form; the head
	// would be malformed there.
	for _, name := range ModelNames() {
		m := modelNamed(t, name)
		if m.PowerHead || m.PowerRaw {
			continue
		}
		y := newModelRig(t, name)
		c := newTestConn(t, y, map[string]string{reqPC: "PC050"})
		if err := y.SetPower(context.Background(), c, watts(50)); err != nil {
			t.Fatalf("%s: SetPower: %v", name, err)
		}
		c.wantSent(t, "PC050;", "PC;")
	}
}

// TestSetPowerRawScale covers the two models whose PC is not watts.
//
// A request in watts is refused rather than converted: their manuals give the
// parameter as 000-255 and calibrate it against nothing, and the FTdx9000 in
// particular is sold as both a 200 W and a 400 W radio with no CAT command that
// says which is on the desk. Inventing a conversion would misreport what the
// transmitter is doing.
func TestSetPowerRawScale(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	for _, name := range []string{"ftdx5000", "ftdx9000"} {
		t.Run(name, func(t *testing.T) {
			for _, tt := range []struct {
				set  radio.PowerSet
				want string
			}{
				{pct(100), "PC255;"},
				{pct(50), "PC128;"}, // 127.5 rounds up
				{pct(0), "PC000;"},
			} {
				y := newModelRig(t, name)
				c := newTestConn(t, y, map[string]string{reqPC: "PC128"})
				if err := y.SetPower(context.Background(), c, tt.set); err != nil {
					t.Fatalf("SetPower: %v", err)
				}
				c.wantSent(t, tt.want, "PC;")
			}

			y := newModelRig(t, name)
			c := newTestConn(t, y, answersFor(y.profile))
			err := y.SetPower(context.Background(), c, watts(100))
			if err == nil {
				t.Fatal("SetPower accepted watts on a radio with no watt scale")
			}
			if !strings.Contains(err.Error(), "percentage") {
				t.Errorf("error %q does not name the alternative", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q anyway", c.sent)
			}
		})
	}
}

func TestSetPowerRejectsAnEmptyRequest(t *testing.T) {
	y := newModelRig(t, "ft-710")
	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.SetPower(context.Background(), c, radio.PowerSet{}); err == nil {
		t.Fatal("SetPower accepted a request naming neither watts nor a percentage")
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q anyway", c.sent)
	}
}

// TestSetFilterWidth covers the whole of what SH is: a table index rather than
// a width, snapped from the request, spelled differently on three of the
// models, and read back because what was asked for and what the rig took are
// routinely different numbers.
func TestSetFilterWidth(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		mode   radio.Mode
		narrow bool
		hz     int
		want   string
	}{
		// The FT-991A's six-byte form against the FT-891's seven, on a table
		// the two radios share byte for byte.
		{"991A CW", "ft-991a", radio.ModeCW, true, 500, "SH010;"},
		{"991A CW rounds down", "ft-991a", radio.ModeCW, true, 480, "SH009;"},
		{"891 CW", "ft-891", radio.ModeCW, true, 500, "SH0110;"},
		{"891 CW wide", "ft-891", radio.ModeCW, false, 800, "SH0011;"},
		// The FT-891 is the only model that carries the narrow flag in SH.
		{"891 SSB narrow", "ft-891", radio.ModeUSB, true, 850, "SH0104;"},
		{"891 SSB wide", "ft-891", radio.ModeUSB, false, 2400, "SH0014;"},
		// The seven-byte form the other four share.
		{"dx101 CW", "ftdx101d", radio.ModeCW, false, 500, "SH0010;"},
		{"dx101 SSB", "ftdx101d", radio.ModeUSB, false, 2400, "SH0014;"},
		{"dx10 SSB", "ftdx10", radio.ModeUSB, false, 2400, "SH0014;"},
		// The FT-710 and FTX-1 tables run further, so a wide request reaches a
		// rung the FTdx101 does not have.
		{"710 SSB top", "ft-710", radio.ModeUSB, false, 4000, "SH0023;"},
		{"dx101 SSB top clamps lower", "ftdx101d", radio.ModeUSB, false, 4000, "SH0021;"},
		{"ftx-1 CW top", "ftx-1", radio.ModeCW, false, 4000, "SH0021;"},
		// Below the ladder clamps up to its lowest rung.
		{"dx101 CW floor", "ftdx101d", radio.ModeCW, false, 10, "SH0001;"},
		// The FT-950 generation takes the six-byte form, and each of the four
		// snaps onto a different ladder. 2400 Hz in wide SSB is index 13 on the
		// FT-950 and the FTdx5000 and index 14 on the FTdx1200 and FTdx3000.
		{"950 SSB", "ft-950", radio.ModeUSB, false, 2400, "SH013;"},
		{"950 CW", "ft-950", radio.ModeCW, false, 500, "SH007;"},
		{"950 CW narrow rounds down", "ft-950", radio.ModeCW, true, 250, "SH004;"},
		{"1200 SSB", "ftdx1200", radio.ModeUSB, false, 2400, "SH014;"},
		{"1200 SSB top", "ftdx1200", radio.ModeUSB, false, 4000, "SH025;"},
		{"3000 CW", "ftdx3000", radio.ModeCW, false, 500, "SH010;"},
		{"5000 SSB", "ftdx5000", radio.ModeUSB, false, 2400, "SH013;"},
		// The FTdx5000's narrow SSB column stops at 1500, so a wider request
		// clamps there rather than reaching the wide column's rungs.
		{"5000 SSB narrow top", "ftdx5000", radio.ModeUSB, true, 4000, "SH007;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, tt.model)
			y.mode.Store(uint32(tt.mode))
			y.narrow.Store(tt.narrow)
			c := newTestConn(t, y, map[string]string{reqSH: shAnswer(y.profile, 10, tt.narrow)})

			if err := y.SetFilterWidth(context.Background(), c, tt.hz); err != nil {
				t.Fatalf("SetFilterWidth: %v", err)
			}
			c.wantSent(t, tt.want, "SH0;")
		})
	}
}

// TestSetFilterWidthRefusedWhereSHIsNotAWidth is the honest refusal. In AM and
// FM the command either has no table column or holds a single fixed value per
// mode code, and a rejected command on a Yaesu is silence rather than an error.
func TestSetFilterWidthRefusedWhereSHIsNotAWidth(t *testing.T) {
	for _, mode := range []radio.Mode{radio.ModeAM, radio.ModeFM, radio.ModeUnknown} {
		y := newModelRig(t, "ft-710")
		y.mode.Store(uint32(mode))
		c := newTestConn(t, y, answersFor(y.profile))

		err := y.SetFilterWidth(context.Background(), c, 2400)
		if err == nil {
			t.Fatalf("SetFilterWidth succeeded in %s", mode)
		}
		if !strings.Contains(err.Error(), "SSB, CW, RTTY, PSK") {
			t.Errorf("error %q does not say where SH does work", err)
		}
		if len(c.sent) != 0 {
			t.Errorf("wrote %q anyway", c.sent)
		}
	}
}

// TestSetFilterWidthRefusedOnTheFTdx9000 is the other refusal, and it is a
// property of the radio rather than of the mode it is in. Its SH parameter is
// the position of the WIDTH knob — 00 anticlockwise to 31 clockwise — and no
// table in its manual turns that into Hz, so a request in Hz cannot be honoured
// in any mode. Sending a number anyway would move the knob to an arbitrary
// place.
func TestSetFilterWidthRefusedOnTheFTdx9000(t *testing.T) {
	for _, mode := range []radio.Mode{radio.ModeCW, radio.ModeUSB, radio.ModeFSK, radio.ModeAM} {
		y := newModelRig(t, "ftdx9000")
		y.mode.Store(uint32(mode))
		c := newTestConn(t, y, answersFor(y.profile))

		err := y.SetFilterWidth(context.Background(), c, 500)
		if err == nil {
			t.Fatalf("SetFilterWidth succeeded in %s on an FTdx9000", mode)
		}
		if !strings.Contains(err.Error(), "WIDTH knob") {
			t.Errorf("error %q does not say what SH is on this radio", err)
		}
		if len(c.sent) != 0 {
			t.Errorf("wrote %q anyway", c.sent)
		}
	}
}

// TestPollSlowFTdx9000 covers the poll on the one radio with neither NA nor a
// bandwidth table: power is all there is to ask for.
func TestPollSlowFTdx9000(t *testing.T) {
	for _, mode := range []string{"MD03", "MD02", "MD04"} {
		y := newModelRig(t, "ftdx9000")
		mustDecode(t, y, mode)
		c := newTestConn(t, y, answersFor(y.profile))
		if err := y.Poll(context.Background(), c, backend.PollSlow); err != nil {
			t.Fatalf("Poll: %v", err)
		}
		c.wantSent(t, "PC;")
	}
}

// TestSetFilterSlotAlwaysFails covers the capability that is simply absent.
// There is no FL-equivalent command in any of these manuals, and Caps publishes
// zero slots so a client knows not to ask.
func TestSetFilterSlotAlwaysFails(t *testing.T) {
	for _, name := range ModelNames() {
		y := newModelRig(t, name)
		c := newTestConn(t, y, answersFor(y.profile))
		err := y.SetFilterSlot(context.Background(), c, 1)
		if err == nil {
			t.Fatalf("%s: SetFilterSlot succeeded", name)
		}
		if !strings.Contains(err.Error(), "no IF filter selection") {
			t.Errorf("%s: error %q does not explain itself", name, err)
		}
		if len(c.sent) != 0 {
			t.Errorf("%s: wrote %q anyway", name, c.sent)
		}
	}
}

// TestSetsAreFireAndForget pins the pattern every setter follows: the write goes
// out with Send, because a Yaesu answers a set command only when AI is on, and
// the read that follows is what reports the value the rig actually took.
func TestSetsAreFireAndForget(t *testing.T) {
	y := newModelRig(t, "ft-710")
	y.mode.Store(uint32(radio.ModeCW))
	c := newTestConn(t, y, map[string]string{
		reqFA: "FA014025000",
		reqMD: "MD03",
		reqPC: "PC050",
		reqSH: "SH0010",
	})
	ctx := context.Background()

	if err := y.SetFrequency(ctx, c, radio.VFOA, 14_025_000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	if err := y.SetMode(ctx, c, radio.ModeCW, false); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	if err := y.SetPower(ctx, c, radio.PowerSet{Pct: ptr(50.0)}); err != nil {
		t.Fatalf("SetPower: %v", err)
	}
	if err := y.SetFilterWidth(ctx, c, 500); err != nil {
		t.Fatalf("SetFilterWidth: %v", err)
	}
	c.wantSent(t,
		"FA014025000;", "FA;",
		"MD03;", "MD0;",
		"PC050;", "PC;",
		"SH0010;", "SH0;",
	)
}

// deadConn is a port that has gone away: every write fails. It covers the paths
// a silent rig cannot reach, since a Yaesu answers a refused command with
// silence rather than an error and those two look different from in here.
type deadConn struct{}

func (deadConn) Do(context.Context, []byte, ...backend.Key) (backend.Update, error) {
	return backend.Update{}, errDead
}
func (deadConn) Send(context.Context, []byte) error { return errDead }

var errDead = errors.New("port closed")

// TestTransportFailuresAreNamed checks that every command path reports which
// request failed. An operator reading a log needs the command, not just "port
// closed" — and on this protocol a timeout is the normal failure, so the two
// have to be distinguishable.
func TestTransportFailuresAreNamed(t *testing.T) {
	y := newModelRig(t, "ft-710")
	y.mode.Store(uint32(radio.ModeCW))
	ctx := context.Background()
	c := deadConn{}

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"Init", func() error { return y.Init(ctx, c) }, "auto-information"},
		{"PollFast", func() error { return y.Poll(ctx, c, backend.PollFast) }, reqIF},
		{"PollSlow", func() error { return y.Poll(ctx, c, backend.PollSlow) }, reqPC},
		{"SetFrequency", func() error { return y.SetFrequency(ctx, c, radio.VFOA, 14_025_000) }, "FA014025000;"},
		{"SetMode", func() error { return y.SetMode(ctx, c, radio.ModeCW, false) }, "MD03;"},
		{"SetPower", func() error { return y.SetPower(ctx, c, radio.PowerSet{Pct: ptr(50.0)}) }, "PC050;"},
		{"SetPTT on", func() error { return y.SetPTT(ctx, c, true) }, "TX1;"},
		{"SetPTT off", func() error { return y.SetPTT(ctx, c, false) }, "TX0;"},
		{"SetFilterWidth", func() error { return y.SetFilterWidth(ctx, c, 500) }, "SH0010;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("succeeded against a dead port")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

// TestReadBackFailurePropagates covers the half of each setter that comes after
// the write: the rig took the command and then said nothing, which is what a
// refusal looks like on a protocol with no error response.
func TestReadBackFailurePropagates(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(*Rig, backend.Conn) error
	}{
		{"SetFrequency", func(y *Rig, c backend.Conn) error {
			return y.SetFrequency(ctx, c, radio.VFOA, 14_025_000)
		}},
		{"SetMode", func(y *Rig, c backend.Conn) error { return y.SetMode(ctx, c, radio.ModeCW, false) }},
		{"SetPower", func(y *Rig, c backend.Conn) error {
			return y.SetPower(ctx, c, radio.PowerSet{Pct: ptr(50.0)})
		}},
		{"SetFilterWidth", func(y *Rig, c backend.Conn) error { return y.SetFilterWidth(ctx, c, 500) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, "ft-710")
			y.mode.Store(uint32(radio.ModeCW))
			c := newTestConn(t, y, nil) // answers nothing
			if err := tt.call(y, c); err == nil {
				t.Fatal("succeeded although the rig answered nothing")
			}
			if len(c.sent) != 2 {
				t.Errorf("sent %q, want the write and its read-back", c.sent)
			}
		})
	}
}

// TestBusyFailsFast is what the busy key is for. A rig that answers '?' has
// said "not now", and every read and every set's read-back has to hear it in
// one round trip instead of sitting out the session's per-command timeout.
//
// The speed is the feature, so it is asserted: the fake conn charges the full
// timeout for a transaction whose wait list did not include the busy key, which
// is exactly what this backend cost before the key existed.
func TestBusyFailsFast(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		call func(*Rig, backend.Conn) error
	}{
		{"Init", func(y *Rig, c backend.Conn) error { return y.Init(ctx, c) }},
		{"PollFast", func(y *Rig, c backend.Conn) error { return y.Poll(ctx, c, backend.PollFast) }},
		{"PollSlow", func(y *Rig, c backend.Conn) error { return y.Poll(ctx, c, backend.PollSlow) }},
		{"SetFrequency", func(y *Rig, c backend.Conn) error {
			return y.SetFrequency(ctx, c, radio.VFOA, 14_025_000)
		}},
		{"SetMode", func(y *Rig, c backend.Conn) error { return y.SetMode(ctx, c, radio.ModeCW, false) }},
		{"SetPower", func(y *Rig, c backend.Conn) error {
			return y.SetPower(ctx, c, radio.PowerSet{Pct: ptr(50.0)})
		}},
		{"SetFilterWidth", func(y *Rig, c backend.Conn) error { return y.SetFilterWidth(ctx, c, 500) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := newModelRig(t, "ft-710")
			y.mode.Store(uint32(radio.ModeCW))
			c := newBusyConn(t, y)

			start := time.Now()
			err := tt.call(y, c)
			elapsed := time.Since(start)

			if err == nil {
				t.Fatal("succeeded although the rig answered ?; to everything")
			}
			if !errors.Is(err, backend.ErrBusy) {
				t.Errorf("error %v is not backend.ErrBusy; the caller cannot tell it is worth retrying", err)
			}
			// A busy answer must never arrive as the session's permanent
			// rejection: that is the error the API turns into 422, which tells
			// the client to rewrite a request that was perfectly correct.
			if errors.Is(err, errFakeNAK) {
				t.Errorf("error %v is a rejection; ?; means busy, not no", err)
			}
			if elapsed > testCmdTimeout/3 {
				t.Errorf("took %s of a %s timeout; the busy key is not on the wait list",
					elapsed, testCmdTimeout)
			}
		})
	}

	// SetPTT is the one command with nothing to fail fast. It is written with
	// Send and answered by nothing, so a '?' it provokes matches no transaction
	// and is dropped; the next fast poll's TX; is what notices. Keying must not
	// report an error it cannot have observed.
	y := newModelRig(t, "ft-710")
	if err := y.SetPTT(ctx, newBusyConn(t, y), true); err != nil {
		t.Errorf("SetPTT: %v, want success: the rig answers nothing at all to TX1;", err)
	}
}

// TestBusyIsNotRemembered is the other half of treating ?; as transient. It may
// not disable a poll item, mark a capability absent or be cached anywhere: the
// next tick asks for exactly the same things, and that retry is the whole
// recovery mechanism.
func TestBusyIsNotRemembered(t *testing.T) {
	ctx := context.Background()
	y := newModelRig(t, "ftdx101d")
	mustDecode(t, y, "MD03") // CW, so SH carries a bandwidth

	busy := newBusyConn(t, y)
	if err := y.Poll(ctx, busy, backend.PollSlow); !errors.Is(err, backend.ErrBusy) {
		t.Fatalf("PollSlow against a busy rig = %v, want backend.ErrBusy", err)
	}
	if err := y.Poll(ctx, busy, backend.PollFast); !errors.Is(err, backend.ErrBusy) {
		t.Fatalf("PollFast against a busy rig = %v, want backend.ErrBusy", err)
	}

	c := newTestConn(t, y, answersFor(y.profile))
	if err := y.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("PollFast after a busy one: %v", err)
	}
	if err := y.Poll(ctx, c, backend.PollSlow); err != nil {
		t.Fatalf("PollSlow after a busy one: %v", err)
	}
	c.wantSent(t, "IF;", "TX;", "SM0;", "PC;", "NA0;", "SH0;")

	// Capabilities are untouched too: a busy answer says nothing about what the
	// radio can do.
	if !y.Caps().FilterWidth {
		t.Error("a busy answer cleared FilterWidth; ?; is not a statement about the command set")
	}
}

func ptr[T any](v T) *T { return &v }
