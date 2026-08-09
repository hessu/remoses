package kenwood

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// modelNamed resolves a profile for a test, where an unknown name is a typo in
// the test rather than a condition to handle.
func modelNamed(t *testing.T, name string) Model {
	t.Helper()
	m, err := LookupModel(name)
	if err != nil {
		t.Fatalf("LookupModel(%q): %v", name, err)
	}
	return m
}

// newModelRig builds a backend for a named model with the defaults config would
// have supplied.
func newModelRig(t *testing.T, name string) *Rig {
	t.Helper()
	k, err := New(&config.Radio{
		ID:      "rig",
		Backend: Name,
		Kenwood: &config.Kenwood{Model: name, AutoInformation: 2, BulkPoll: true},
	})
	if err != nil {
		t.Fatalf("New(%q): %v", name, err)
	}
	return k
}

// answersFor is a rig of any model sitting on 14.025 MHz in CW, 50 W,
// receiving, with the first filter selected.
func answersFor(m Model) map[string]string {
	a := map[string]string{
		reqFA: "FA00014025000",
		reqPC: "PC050",
		reqIF: sampleIF,
		reqFW: "FW0500",
	}
	if m.ID != 0 {
		a[reqID] = fmt.Sprintf("ID%03d", m.ID)
	}
	if m.ModeCmd == ModeOM {
		a[reqOM] = "OM03"
	} else {
		a[reqMD] = "MD3"
		a[reqDA] = "DA0"
	}
	// Break-in is VX on the TS-590 generation and BI on the newer one, and the
	// two-value styles need the SD delay to tell semi from full.
	switch m.BreakIn {
	case BreakInVX:
		a[reqSD] = "SD0300"
		a["VX;"] = "VX1"
	case BreakInBI2:
		a[reqSD] = "SD0300"
		a["BI;"] = "BI1"
	case BreakInBI3:
		a["BI;"] = "BI1"
	}
	// FL is not one command with one shape: FL1/FL2 on the TS-590, FL0 plus a
	// selection on the TS-890S, FL0 plus band and selection on the TS-990S. The
	// TS-890S has no read form that does not also set, so it asks for nothing.
	switch m.FilterSelect {
	case FilterSelectAB:
		a[m.filterSlotRead()] = "FL1"
	case FilterSelectBandABC:
		a[m.filterSlotRead()] = "FL000"
	}
	if m.smeterArgLen() == 4 {
		a[m.SMeterRequest] = "SM0000"
	} else {
		a[m.SMeterRequest] = "SM00000"
	}
	return a
}

// TestModelListMatchesConfig is the drift guard. config cannot import this
// package — it sits below rig/backend — so its list is a copy, and this is the
// direction that can see both.
func TestModelListMatchesConfig(t *testing.T) {
	got := ModelNames()
	want := slices.Clone(config.KenwoodModels)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("kenwood.ModelNames() = %v, config.KenwoodModels = %v; the lists have drifted", got, want)
	}
}

func TestLookupModel(t *testing.T) {
	// An unnamed model must keep landing on the profile this backend was
	// originally written against, or an existing configuration would change
	// radios under its operator.
	m, err := LookupModel("")
	if err != nil {
		t.Fatalf("LookupModel(\"\"): %v", err)
	}
	if DefaultModel != "ts590sg" || m.Name != DefaultModel {
		t.Errorf("default model = %q (DefaultModel %q), want ts590sg", m.Name, DefaultModel)
	}

	// Case and hyphens are how the name is printed on the front panel.
	for _, name := range []string{"ts590sg", "TS590SG", "ts-590sg", "TS-590SG", "  ts-590SG  "} {
		got, err := LookupModel(name)
		if err != nil {
			t.Fatalf("LookupModel(%q): %v", name, err)
		}
		if got.Name != "ts590sg" {
			t.Errorf("LookupModel(%q) = %q, want ts590sg", name, got.Name)
		}
	}

	err = nil
	if _, err = LookupModel("ts2000"); err == nil {
		t.Fatal("LookupModel accepted a model with no profile")
	}
	// The error has to list the alternatives: the operator is editing a file,
	// possibly a long way from the radio.
	for _, want := range ModelNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %q", err, want)
		}
	}
}

func TestNewRejectsUnknownModel(t *testing.T) {
	_, err := New(&config.Radio{Kenwood: &config.Kenwood{Model: "ft-991a", AutoInformation: 2}})
	if err == nil {
		t.Fatal("New accepted a model with no profile")
	}
}

// TestModelIDs pins the ID; answer of each radio, which is the one identity
// check this protocol offers.
func TestModelIDs(t *testing.T) {
	want := map[string]int{
		"ts480":   20,
		"ts590s":  21,
		"ts990s":  22,
		"ts590sg": 23,
		"ts890s":  24,
	}
	for name, id := range want {
		m := modelNamed(t, name)
		if m.ID != id {
			t.Errorf("%s ID = %d, want %d", name, m.ID, id)
		}
		if got, ok := ModelForID(id); !ok || got.Name != name {
			t.Errorf("ModelForID(%d) = %q, %v; want %q", id, got.Name, ok, name)
		}
	}

	// generic has no ID, and must not be reachable through one: a radio remoses
	// has no profile for still answers ID; with something.
	if m := modelNamed(t, "generic"); m.ID != 0 {
		t.Errorf("generic ID = %d, want 0", m.ID)
	}
	if _, ok := ModelForID(0); ok {
		t.Error("ModelForID(0) matched a model; generic must not be reachable by ID")
	}
}

// TestOMModeRoundTrip covers the whole OM P2 table in both directions. The DATA
// codes are the point: OM has no DA, so LSB with DATA has to encode as C and C
// has to decode back to LSB *with* the flag.
func TestOMModeRoundTrip(t *testing.T) {
	tests := []struct {
		code byte
		mode radio.Mode
		data bool
	}{
		{'1', radio.ModeLSB, false},
		{'2', radio.ModeUSB, false},
		{'3', radio.ModeCW, false},
		{'4', radio.ModeFM, false},
		{'5', radio.ModeAM, false},
		{'6', radio.ModeFSK, false},
		{'7', radio.ModeCWR, false},
		{'9', radio.ModeFSKR, false},
		{'A', radio.ModePSK, false},
		{'B', radio.ModePSKR, false},
		{'C', radio.ModeLSB, true},
		{'D', radio.ModeUSB, true},
		{'E', radio.ModeFM, true},
		{'F', radio.ModeAM, true},
	}
	for _, tt := range tests {
		name := tt.mode.String()
		if tt.data {
			name += "-DATA"
		}
		t.Run(name, func(t *testing.T) {
			mode, data, ok := decodeOMMode(tt.code)
			if !ok || mode != tt.mode || data != tt.data {
				t.Fatalf("decodeOMMode(%q) = (%s, %v, %v), want (%s, %v, true)",
					tt.code, mode, data, ok, tt.mode, tt.data)
			}
			back, err := encodeOMMode(tt.mode, tt.data)
			if err != nil {
				t.Fatalf("encodeOMMode(%s, %v): %v", tt.mode, tt.data, err)
			}
			if back != tt.code {
				t.Fatalf("encodeOMMode(%s, %v) = %q, want %q", tt.mode, tt.data, back, tt.code)
			}
		})
	}
}

// TestOMUnusedCodes pins the reason 0 and 8 are absent from the table: the
// reference calls them "Unused", exactly like MD's setting-failure values, and
// folding them into ModeUnknown would let a rejected set wipe a good mode.
func TestOMUnusedCodes(t *testing.T) {
	for _, c := range []byte{'0', '8'} {
		if m, _, ok := decodeOMMode(c); ok {
			t.Errorf("decodeOMMode(%q) = %s, want no mode", c, m)
		}
	}
	for _, c := range []byte{'G', 'z', ' ', '#'} {
		if _, _, ok := decodeOMMode(c); ok {
			t.Errorf("decodeOMMode(%q) accepted a value the reference does not define", c)
		}
	}
	// Case is not significant anywhere else in this protocol either.
	if m, data, ok := decodeOMMode('d'); !ok || m != radio.ModeUSB || !data {
		t.Errorf("decodeOMMode('d') = (%s, %v, %v), want USB-DATA", m, data, ok)
	}
	// PSK has no DATA variant, and CW has no OM DATA code at all.
	for _, m := range []radio.Mode{radio.ModePSK, radio.ModeCW, radio.ModeCWR, radio.ModeFSK} {
		if c, err := encodeOMMode(m, true); err == nil {
			t.Errorf("encodeOMMode(%s, data) = %q, want an error", m, c)
		}
	}
	if c, err := encodeOMMode(radio.ModeUnknown, false); err == nil {
		t.Errorf("encodeOMMode(unknown) = %q, want an error", c)
	}
}

// TestMDModeTableUnchanged guards the older half of the family against the OM
// table: the two share modes but not codes, and 3 is CW in both only by
// coincidence of the digits.
func TestMDModeTableUnchanged(t *testing.T) {
	for d, m := range map[byte]radio.Mode{
		'1': radio.ModeLSB, '2': radio.ModeUSB, '3': radio.ModeCW, '4': radio.ModeFM,
		'5': radio.ModeAM, '6': radio.ModeFSK, '7': radio.ModeCWR, '9': radio.ModeFSKR,
	} {
		got, ok := decodeMode(d)
		if !ok || got != m {
			t.Errorf("decodeMode(%q) = (%s, %v), want %s", d, got, ok, m)
		}
	}
	// MD carries no DATA and no PSK: those are the two things OM added.
	for _, m := range []radio.Mode{radio.ModePSK, radio.ModePSKR} {
		if _, err := encodeMode(m); err == nil {
			t.Errorf("encodeMode(%s) succeeded; MD has no code for it", m)
		}
	}
}

func TestCapsPerModel(t *testing.T) {
	tests := []struct {
		model       string
		maxPowerW   float64
		smeterScale int
		filterWidth bool
		filterSlots int
		extraModes  []radio.Mode
	}{
		{"ts480", 100, 20, true, 0, nil},
		{"ts590s", 100, 30, true, 2, nil},
		{"ts590sg", 100, 30, true, 2, nil},
		// Three, not four: FL0 selects receive filter A, B or C. FL1, FL2 and
		// FL3 are different commands entirely (Roofing Filter, IF Filter Shape,
		// AF Filter Type), not further slots.
		{"ts890s", 100, 70, false, 3, []radio.Mode{radio.ModePSK, radio.ModePSKR}},
		{"ts990s", 200, 70, false, 3, []radio.Mode{radio.ModePSK, radio.ModePSKR}},
		{"generic", 100, 30, true, 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			c := newModelRig(t, tt.model).Caps()

			if c.MaxPowerW != tt.maxPowerW {
				t.Errorf("MaxPowerW = %v, want %v", c.MaxPowerW, tt.maxPowerW)
			}
			if c.SMeterScale != tt.smeterScale {
				t.Errorf("SMeterScale = %d, want %d", c.SMeterScale, tt.smeterScale)
			}
			if c.FilterWidth != tt.filterWidth {
				t.Errorf("FilterWidth = %v, want %v", c.FilterWidth, tt.filterWidth)
			}
			if c.FilterSlots != tt.filterSlots {
				t.Errorf("FilterSlots = %d, want %d", c.FilterSlots, tt.filterSlots)
			}
			if !c.PowerWattAccurate {
				t.Error("PowerWattAccurate false; PC is in real watts on every model here")
			}

			// The common set is on every radio.
			for _, m := range modesCommon() {
				if !c.SupportsMode(m) {
					t.Errorf("caps omit %s", m)
				}
			}
			// PSK is a mode OM can select directly. The MD models decode it in
			// software through their DATA modes and have no code for it, so
			// advertising it there would be a promise the backend cannot keep.
			for _, m := range []radio.Mode{radio.ModePSK, radio.ModePSKR} {
				want := slices.Contains(tt.extraModes, m)
				if got := c.SupportsMode(m); got != want {
					t.Errorf("SupportsMode(%s) = %v, want %v", m, got, want)
				}
			}
		})
	}
}

// TestCapsModesAreCopied guards the one shared-state hazard in the registry: a
// profile's mode slice is handed out through the API on every call.
func TestCapsModesAreCopied(t *testing.T) {
	k := newModelRig(t, "ts990s")
	c := k.Caps()
	c.Modes[0] = radio.ModeUnknown
	if k.Caps().Modes[0] == radio.ModeUnknown {
		t.Error("Caps shares its mode slice with the registry; a client could mutate every session's capabilities")
	}
}

// TestSMeterPerModel covers both halves of the S-meter difference: the request
// form and the scale. Getting the form wrong on a TS-890S would read four digits
// at the wrong offset and report a tenth of the signal.
func TestSMeterPerModel(t *testing.T) {
	tests := []struct {
		model     string
		wantReq   string
		answer    string
		wantRaw   int
		wantScale int
	}{
		{"ts480", "SM0;", "SM00012", 12, 20},
		{"ts590s", "SM0;", "SM00015", 15, 30},
		{"ts590sg", "SM0;", "SM00015", 15, 30},
		// No meter selector: SM; answers SMnnnn, four digits and no band digit.
		{"ts890s", "SM;", "SM0042", 42, 70},
		{"ts990s", "SM0;", "SM00042", 42, 70},
		{"generic", "SM0;", "SM00015", 15, 30},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			if k.profile.SMeterRequest != tt.wantReq {
				t.Fatalf("S-meter request = %q, want %q", k.profile.SMeterRequest, tt.wantReq)
			}

			u := mustDecode(t, k, tt.answer)
			if u.Key != keySM {
				t.Fatalf("key = %q, want %q", u.Key, keySM)
			}
			if u.Patch.SMeter == nil {
				t.Fatal("no S-meter in the patch")
			}
			if got := *u.Patch.SMeter; got.Raw != tt.wantRaw || got.Scale != tt.wantScale {
				t.Errorf("meter = %+v, want {Raw:%d Scale:%d}", got, tt.wantRaw, tt.wantScale)
			}
		})
	}
}

// TestSMeterWrongFormIgnored covers the mismatch directly: a TS-890S answer
// arriving at a TS-590 profile must be discarded rather than read as a signal a
// factor of ten out.
func TestSMeterWrongFormIgnored(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	u := mustDecode(t, k, "SM0042")
	if u.Patch.SMeter != nil {
		t.Errorf("a four-digit SM answer was accepted as %+v on a five-digit model", *u.Patch.SMeter)
	}

	k = newModelRig(t, "ts890s")
	u = mustDecode(t, k, "SM00042")
	if u.Patch.SMeter != nil {
		t.Errorf("a five-digit SM answer was accepted as %+v on a four-digit model", *u.Patch.SMeter)
	}
}

// TestPollFastWithoutBulkPoll is the TS-890S limitation in miniature: no IF;,
// and therefore no PTT query anywhere in the poll. PTT can only arrive as an AI
// push.
func TestPollFastWithoutBulkPoll(t *testing.T) {
	for _, name := range []string{"ts890s", "ts990s"} {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, "FA;", "OM0;", k.profile.SMeterRequest)

			for _, req := range c.sent {
				switch req {
				case "IF;", "TX;", "RX;":
					t.Errorf("fast poll sent %q; the %s has no way to read it", req, k.profile.Label)
				}
			}
			if k.useBulkPoll() {
				t.Errorf("useBulkPoll true on a radio with no IF; command")
			}
		})
	}
}

// TestPollFastPerModel pins the shape of the fast poll on the models that do
// have IF;.
func TestPollFastPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		{"ts480", []string{"IF;", "SM0;"}},
		{"ts590sg", []string{"IF;", "SM0;"}},
		{"ts890s", []string{"FA;", "OM0;", "SM;"}},
		{"ts990s", []string{"FA;", "OM0;", "SM0;"}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.Poll(context.Background(), c, backend.PollFast); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestPollSlowPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		// No DA and no FL to read, and no break-in command either: the TS-480
		// has none of the three.
		{"ts480", []string{"PC;", "FW;"}},
		{"ts590sg", []string{"PC;", "FL;", "DA;", "SD;", "VX;", "FW;"}},
		// DATA came with the mode code, and FW is not a width here. The TS-890S
		// asks for no filter at all: its FL0 read form carries the selection, so
		// there is no way to ask without also setting. Its BI is two-valued, so
		// SD comes first; the TS-990S's is three-valued and needs no delay.
		{"ts890s", []string{"PC;", "SD;", "BI;"}},
		{"ts990s", []string{"PC;", "FL00;", "BI;"}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(radio.ModeCW)) // FW carries a width in CW
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.Poll(context.Background(), c, backend.PollSlow); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestInitPerModel(t *testing.T) {
	tests := []struct {
		model string
		want  []string
	}{
		{"ts480", []string{"AI2;", "ID;", "FA;", "MD;", "PC;", "IF;"}},
		{"ts590s", []string{"AI2;", "ID;", "FA;", "MD;", "DA;", "PC;", "FL;", "IF;"}},
		// No IF; to end on, so PTT is unknown until the rig pushes a TX; or RX;.
		{"ts890s", []string{"AI2;", "ID;", "FA;", "OM0;", "PC;"}},
		{"ts990s", []string{"AI2;", "ID;", "FA;", "OM0;", "PC;", "FL00;"}},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.Init(context.Background(), c); err != nil {
				t.Fatalf("Init: %v", err)
			}
			c.wantSent(t, tt.want...)
			if k.lastMode() != radio.ModeCW {
				t.Errorf("mode = %s after Init, want CW", k.lastMode())
			}
		})
	}
}

func TestSetModeOM(t *testing.T) {
	tests := []struct {
		name string
		mode radio.Mode
		data bool
		want []string
	}{
		// P1 is ignored on the set form — the reference says to enter any value
		// — so it is always 0, and the read that follows asks for the left
		// display area.
		{"CW", radio.ModeCW, false, []string{"OM03;", "OM0;"}},
		{"USB", radio.ModeUSB, false, []string{"OM02;", "OM0;"}},
		// The whole point of the OM table: DATA is in the code, so there is no
		// DA to follow with.
		{"USB-DATA", radio.ModeUSB, true, []string{"OM0D;", "OM0;"}},
		{"LSB-DATA", radio.ModeLSB, true, []string{"OM0C;", "OM0;"}},
		{"FM-DATA", radio.ModeFM, true, []string{"OM0E;", "OM0;"}},
		{"AM-DATA", radio.ModeAM, true, []string{"OM0F;", "OM0;"}},
		{"PSK", radio.ModePSK, false, []string{"OM0A;", "OM0;"}},
		{"PSK-R", radio.ModePSKR, false, []string{"OM0B;", "OM0;"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, "ts890s")
			code, err := encodeOMMode(tt.mode, tt.data)
			if err != nil {
				t.Fatalf("encodeOMMode: %v", err)
			}
			c := newTestConn(t, k, map[string]string{reqOM: fmt.Sprintf("OM0%c", code)})
			if err := k.SetMode(context.Background(), c, tt.mode, tt.data); err != nil {
				t.Fatalf("SetMode: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

func TestSetModeErrorsPerModel(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		mode     radio.Mode
		data     bool
		wantWord string
	}{
		// The TS-480 has no DA command at all, so a DATA request cannot be
		// approximated — it has to be refused.
		{"TS-480 has no DATA mode", "ts480", radio.ModeUSB, true, "no DATA mode"},
		{"TS-890S rejects DATA in CW", "ts890s", radio.ModeCW, true, "LSB, USB, FM and AM"},
		{"TS-890S rejects DATA in PSK", "ts890s", radio.ModePSK, true, "LSB, USB, FM and AM"},
		{"TS-590SG has no PSK", "ts590sg", radio.ModePSK, false, "does not have mode"},
		{"TS-480 has no PSK", "ts480", radio.ModePSKR, false, "does not have mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			err := k.SetMode(context.Background(), c, tt.mode, tt.data)
			if err == nil {
				t.Fatalf("SetMode(%s, data=%v) succeeded on a %s", tt.mode, tt.data, k.profile.Label)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain itself (want %q in it)", err, tt.wantWord)
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q despite rejecting the request", c.sent)
			}
		})
	}
}

// TestSetModeTS480SendsNoDA covers the other half of a radio with no DATA mode:
// even a plain mode change must not trail a DA0.
func TestSetModeTS480SendsNoDA(t *testing.T) {
	k := newModelRig(t, "ts480")
	c := newTestConn(t, k, map[string]string{reqMD: "MD2"})
	if err := k.SetMode(context.Background(), c, radio.ModeUSB, false); err != nil {
		t.Fatalf("SetMode: %v", err)
	}
	c.wantSent(t, "MD2;", "MD;")
}

// TestDecodeOM covers the answer side, including the display area this backend
// deliberately ignores.
func TestDecodeOM(t *testing.T) {
	mode := func(m radio.Mode) *radio.Mode { return &m }
	flag := func(b bool) *bool { return &b }

	tests := []struct {
		name     string
		frame    string
		wantMode *radio.Mode
		wantData *bool
	}{
		{"CW", "OM03", mode(radio.ModeCW), flag(false)},
		{"USB-DATA", "OM0D", mode(radio.ModeUSB), flag(true)},
		{"AM-DATA", "OM0F", mode(radio.ModeAM), flag(true)},
		{"PSK", "OM0A", mode(radio.ModePSK), flag(false)},
		{"lower case", "om0c", mode(radio.ModeLSB), flag(true)},
		// P1 = 1 is the right-hand display area, the sub receiver. State
		// publishes one mode, so folding it in would let the second receiver
		// overwrite the first.
		{"sub receiver is ignored", "OM12", nil, nil},
		{"unused code", "OM00", nil, nil},
		{"unused code 8", "OM08", nil, nil},
		{"truncated", "OM0", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, "ts890s")
			u := mustDecode(t, k, tt.frame)
			if u.Key != keyOM {
				t.Fatalf("key = %q, want %q; the transaction would never complete", u.Key, keyOM)
			}
			if !u.OK {
				t.Error("OK false")
			}
			comparePatch(t, u.Patch, radio.Patch{Mode: tt.wantMode, DataMode: tt.wantData})
		})
	}
}

// TestSetFilterWidthRejectedWithoutIt is the difference that would otherwise be
// silent: FW exists on the TS-890S and TS-990S, so the command would be accepted
// — and would move the FM modulation setting instead of a passband.
func TestSetFilterWidthRejectedWithoutIt(t *testing.T) {
	for _, name := range []string{"ts890s", "ts990s"} {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			k.mode.Store(uint32(radio.ModeCW)) // legal on a model that has FW
			c := newTestConn(t, k, answersFor(k.profile))

			err := k.SetFilterWidth(context.Background(), c, 500)
			if err == nil {
				t.Fatal("SetFilterWidth accepted a width on a radio whose FW is the FM modulation switch")
			}
			if !strings.Contains(err.Error(), "FM modulation") {
				t.Errorf("error %q does not say what FW does there", err)
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q anyway", c.sent)
			}
		})
	}

	// The models that do have it are unaffected.
	for _, name := range []string{"ts480", "ts590s", "ts590sg", "generic"} {
		k := newModelRig(t, name)
		k.mode.Store(uint32(radio.ModeCW))
		c := newTestConn(t, k, map[string]string{reqFW: "FW0500"})
		if err := k.SetFilterWidth(context.Background(), c, 500); err != nil {
			t.Fatalf("%s: SetFilterWidth: %v", name, err)
		}
		c.wantSent(t, "FW0500;", "FW;")
	}
}

func TestSetFilterSlotPerModel(t *testing.T) {
	tests := []struct {
		model string
		slot  int
		want  string
		fail  bool
	}{
		{model: "ts590sg", slot: 1, want: "FL1;"},
		{model: "ts590sg", slot: 2, want: "FL2;"},
		{model: "ts590sg", slot: 3, fail: true},
		// FL0 is one command whose parameter picks receive filter A, B or C, so
		// slot 1..3 becomes FL00..FL02. The API's numbering starts at 1 and the
		// rig's parameter at 0, hence the offset.
		{model: "ts890s", slot: 1, want: "FL00;"},
		{model: "ts890s", slot: 3, want: "FL02;"},
		// Four is not a filter here: FL3 is the AF Filter Type command.
		{model: "ts890s", slot: 4, fail: true},
		{model: "ts890s", slot: 0, fail: true},
		// The TS-990S puts the band first, so the selection is the second
		// parameter: main band, filter B.
		{model: "ts990s", slot: 2, want: "FL001;"},
		// No filter selection at all.
		{model: "ts480", slot: 1, fail: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%d", tt.model, tt.slot), func(t *testing.T) {
			k := newModelRig(t, tt.model)
			c := newTestConn(t, k, answersFor(k.profile))
			err := k.SetFilterSlot(context.Background(), c, tt.slot)
			if tt.fail {
				if err == nil {
					t.Fatalf("SetFilterSlot(%d) succeeded on a %s", tt.slot, k.profile.Label)
				}
				if len(c.sent) != 0 {
					t.Errorf("wrote %q anyway", c.sent)
				}
				return
			}
			if err != nil {
				t.Fatalf("SetFilterSlot(%d): %v", tt.slot, err)
			}
			want := []string{tt.want}
			if read := k.profile.filterSlotRead(); read != "" {
				want = append(want, read)
			}
			c.wantSent(t, want...)
		})
	}
}

// TestDecodeFilterSlotPerModel is the read side of the same offset.
func TestDecodeFilterSlotPerModel(t *testing.T) {
	tests := []struct {
		model string
		frame string
		want  int // 0 means "nothing published"
	}{
		{"ts590sg", "FL1", 1},
		{"ts590sg", "FL2", 2},
		{"ts590sg", "FL0", 0},
		// TS-890S: FL0 then the selection, then the 270 Hz option flag.
		{"ts890s", "FL00", 1},
		{"ts890s", "FL021", 3},
		{"ts890s", "FL03", 0}, // no fourth receive filter
		// TS-990S: FL0, then the band, then the selection. Reading the first
		// character would report the band as a filter slot.
		{"ts990s", "FL000", 1},
		{"ts990s", "FL002", 3},
		// Sub band: ignored rather than published as the main band's slot,
		// matching how an OM frame for the second display area is handled.
		{"ts990s", "FL010", 0},
		{"ts480", "FL1", 0},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.frame, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			u := mustDecode(t, k, tt.frame)
			switch {
			case tt.want == 0:
				if u.Patch.FilterSlot != nil {
					t.Errorf("slot = %d, want nothing published", *u.Patch.FilterSlot)
				}
			case u.Patch.FilterSlot == nil:
				t.Errorf("no slot published, want %d", tt.want)
			case *u.Patch.FilterSlot != tt.want:
				t.Errorf("slot = %d, want %d", *u.Patch.FilterSlot, tt.want)
			}
		})
	}
}

// TestPowerScalesWithTheModel covers the TS-990S, the only 200 W radio here:
// 100 W is half power there and full power everywhere else, and the AM ceiling
// follows the nominal rating rather than being a fixed 25 W.
func TestPowerScalesWithTheModel(t *testing.T) {
	watts := func(v float64) radio.PowerSet { return radio.PowerSet{Watts: &v} }
	pct := func(v float64) radio.PowerSet { return radio.PowerSet{Pct: &v} }

	tests := []struct {
		name  string
		model string
		mode  radio.Mode
		set   radio.PowerSet
		want  string
	}{
		{"200 W is full scale", "ts990s", radio.ModeUSB, pct(100), "PC200;"},
		{"half scale", "ts990s", radio.ModeUSB, pct(50), "PC100;"},
		{"watts pass through", "ts990s", radio.ModeUSB, watts(150), "PC150;"},
		{"clamped to 200 W", "ts990s", radio.ModeUSB, watts(250), "PC200;"},
		{"AM is a quarter of nominal", "ts990s", radio.ModeAM, pct(100), "PC050;"},
		{"AM clamps at 50 W", "ts990s", radio.ModeAM, watts(200), "PC050;"},
		// Unchanged on the 100 W radios.
		{"100 W is full scale", "ts890s", radio.ModeUSB, pct(100), "PC100;"},
		{"AM still 25 W", "ts590sg", radio.ModeAM, pct(100), "PC025;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(tt.mode))
			c := newTestConn(t, k, map[string]string{reqPC: "PC050"})
			if err := k.SetPower(context.Background(), c, tt.set); err != nil {
				t.Fatalf("SetPower: %v", err)
			}
			c.wantSent(t, tt.want, "PC;")
		})
	}
}

// TestDecodePowerScalesWithTheModel is the read side: the same three digits are
// a different fraction of full power on a 200 W radio.
func TestDecodePowerScalesWithTheModel(t *testing.T) {
	tests := []struct {
		model   string
		mode    radio.Mode
		frame   string
		wantPct float64
	}{
		{"ts590sg", radio.ModeUSB, "PC100", 100},
		{"ts990s", radio.ModeUSB, "PC100", 50},
		{"ts990s", radio.ModeUSB, "PC200", 100},
		{"ts990s", radio.ModeAM, "PC050", 100},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.frame, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(tt.mode))
			u := mustDecode(t, k, tt.frame)
			if u.Patch.Power == nil {
				t.Fatal("no power in the patch")
			}
			if got := u.Patch.Power.Pct; got != tt.wantPct {
				t.Errorf("Pct = %v, want %v", got, tt.wantPct)
			}
		})
	}
}

// TestModeSetRejectsAnImpossibleCombination reaches modeSet directly, where
// SetMode's own DATA check would normally have stopped first. The two guards
// have to agree: PSK is a mode the TS-890S has, but PSK-DATA is not a code OM
// can express.
func TestModeSetRejectsAnImpossibleCombination(t *testing.T) {
	m := modelNamed(t, "ts890s")
	if cmd, err := m.modeSet(radio.ModePSK, true); err == nil {
		t.Errorf("modeSet(PSK, data) = %q, want an error: OM has no PSK-DATA code", cmd)
	}
	if cmd, err := m.modeSet(radio.ModeUSB, true); err != nil || cmd != "OM0D;" {
		t.Errorf("modeSet(USB, data) = (%q, %v), want OM0D;", cmd, err)
	}
}

// TestInitWarnsOnIdentityMismatch covers the cross-check. The configuration
// stays authoritative — a mismatch is a warning, not a refusal — because acting
// on the ID would silently switch command sets under an operator who wrote
// something specific.
func TestInitWarnsOnIdentityMismatch(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		id         string
		wantWords  []string
		wantSilent bool
	}{
		{
			name: "a TS-890S configured as a TS-590SG", model: "ts590sg", id: "ID024",
			wantWords: []string{"ts590sg", "TS-890S", "024"},
		},
		{
			// A Kenwood-compatible clone, or a model remoses has no profile for.
			name: "an ID no profile claims", model: "ts590sg", id: "ID099",
			wantWords: []string{"ts590sg", "099"},
		},
		{
			name: "the configured model", model: "ts890s", id: "ID024", wantSilent: true,
		},
		{
			// generic has no ID to compare against, so there is nothing to warn
			// about however the radio answers.
			name: "generic never warns", model: "generic", id: "ID024", wantSilent: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			k := newModelRig(t, tt.model)
			answers := answersFor(k.profile)
			answers[reqID] = tt.id
			if err := k.Init(context.Background(), newTestConn(t, k, answers)); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if k.profile.Name != tt.model {
				t.Errorf("profile changed to %q; the configuration is authoritative", k.profile.Name)
			}

			log := buf.String()
			if tt.wantSilent {
				if strings.Contains(log, "different model") {
					t.Errorf("warned about a model that matches: %s", log)
				}
				return
			}
			for _, w := range tt.wantWords {
				if !strings.Contains(log, w) {
					t.Errorf("warning does not mention %q: %s", w, log)
				}
			}
		})
	}
}

// TestModelReportsTheRig covers the identity path for the models added here.
func TestModelReportsTheRig(t *testing.T) {
	for _, tt := range []struct {
		configured string
		frame      string
		want       string
	}{
		{"ts480", "ID020", "TS-480"},
		{"ts890s", "ID024", "TS-890S"},
		{"ts990s", "ID022", "TS-990S"},
		// The rig wins over the configuration: ID; genuinely names a model.
		{"ts590sg", "ID024", "TS-890S"},
	} {
		k := newModelRig(t, tt.configured)
		mustDecode(t, k, tt.frame)
		if got := k.Model(); got != tt.want {
			t.Errorf("model %q after %s = %q, want %q", tt.configured, tt.frame, got, tt.want)
		}
	}

	// Before any ID; the configured name stands in.
	k := newModelRig(t, "ts890s")
	if got := k.Model(); got != "TS-890S" {
		t.Errorf("Model = %q before Init, want the configured name", got)
	}
}
