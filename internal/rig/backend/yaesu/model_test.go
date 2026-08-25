package yaesu

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/rig/backend/yaesubin"
)

// TestModelListMatchesConfig is the drift guard. config cannot import this
// package — it sits below rig/backend — so its list is a copy, and this is the
// direction that can see both.
//
// It is the union of two registries, because `yaesu.model` is one namespace
// covering two protocols: a name this backend does not know may still be a
// valid configuration, and config has to accept it. Which is also why the two
// registries must not overlap — a name in both would make the dispatch depend
// on the order it was checked in.
func TestModelListMatchesConfig(t *testing.T) {
	got := append(ModelNames(), yaesubin.ModelNames()...)
	slices.Sort(got)
	want := slices.Clone(config.YaesuModels)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("yaesu + yaesubin model names = %v, config.YaesuModels = %v; the lists have drifted", got, want)
	}

	for _, n := range ModelNames() {
		if yaesubin.Handles(n) {
			t.Errorf("%q is in both registries; the dispatch would depend on which was asked first", n)
		}
	}
}

func TestLookupModel(t *testing.T) {
	// An unnamed model lands on generic rather than a specific radio: guessing
	// one would mean guessing a mode table, which is the one thing this backend
	// must never guess.
	m, err := LookupModel("")
	if err != nil {
		t.Fatalf("LookupModel(\"\"): %v", err)
	}
	if DefaultModel != "generic" || m.Name != DefaultModel {
		t.Errorf("default model = %q (DefaultModel %q), want generic", m.Name, DefaultModel)
	}

	// Yaesu's own hyphenation varies by model, so both spellings work either
	// way round.
	for _, tt := range []struct{ in, want string }{
		{"ft-991a", "ft-991a"},
		{"FT-991A", "ft-991a"},
		{"ft991a", "ft-991a"},
		{"  FT991a  ", "ft-991a"},
		{"ftdx10", "ftdx10"},
		{"FTDX-10", "ftdx10"},
		{"ftx-1", "ftx-1"},
		{"FTX1", "ftx-1"},
		{"ftdx101mp", "ftdx101mp"},
	} {
		got, err := LookupModel(tt.in)
		if err != nil {
			t.Fatalf("LookupModel(%q): %v", tt.in, err)
		}
		if got.Name != tt.want {
			t.Errorf("LookupModel(%q) = %q, want %q", tt.in, got.Name, tt.want)
		}
	}

	// The error has to list the alternatives: the operator is editing a file,
	// possibly a long way from the radio.
	_, err = LookupModel("ft-857")
	if err == nil {
		t.Fatal("LookupModel accepted a model with no profile")
	}
	for _, want := range ModelNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not offer %q", err, want)
		}
	}
}

func TestNewRejectsUnknownModel(t *testing.T) {
	if _, err := New(&config.Radio{Yaesu: &config.Yaesu{Model: "ts590sg"}}); err == nil {
		t.Fatal("New accepted a model with no profile")
	}
	// A radio with no yaesu block at all is not an error: the defaults are the
	// ones config would have filled in.
	y, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if y.profile.Name != DefaultModel || !y.ai {
		t.Errorf("New(nil) = %q, ai %v; want %q with AI on", y.profile.Name, y.ai, DefaultModel)
	}
}

// TestModelIDs pins the ID; answer of each radio, which is the one identity
// check this protocol offers — and unlike an Icom's bus address it is fixed in
// firmware, so it genuinely names a model.
func TestModelIDs(t *testing.T) {
	want := map[string][]int{
		"ft-950":   {310},
		"ftdx5000": {362},
		"ftdx3000": {462},
		// Two numbers, one radio: 0582 says the optional FFT-1 unit is fitted
		// and 0583 says it is not, so both have to match or half the population
		// warns at every connect.
		"ftdx1200":  {582, 583},
		"ft-891":    {650},
		"ft-991a":   {670},
		"ftdx101d":  {681},
		"ftdx101mp": {682},
		"ftdx10":    {761},
		"ft-710":    {800},
		"ftx-1":     {840},
	}
	for name, ids := range want {
		m := modelNamed(t, name)
		if !slices.Equal(m.IDs, ids) {
			t.Errorf("%s IDs = %v, want %v", name, m.IDs, ids)
		}
		if !m.HasID {
			t.Errorf("%s has IDs but HasID is false; Init would never ask", name)
		}
		for _, id := range ids {
			if got, ok := ModelForID(id); !ok || got.Name != name {
				t.Errorf("ModelForID(%d) = %q, %v; want %q", id, got.Name, ok, name)
			}
		}
	}
	// The FTdx101D and MP are the pair worth naming: same radio to look at,
	// different numbers, so they are distinguishable at runtime.
	if slices.Equal(modelNamed(t, "ftdx101d").IDs, modelNamed(t, "ftdx101mp").IDs) {
		t.Error("the FTdx101D and MP share an ID; the mismatch warning could not tell them apart")
	}

	// generic and the FTdx9000 both claim no number, for opposite reasons:
	// generic stands for a radio whose number remoses does not know, while the
	// FTdx9000's command list has no ID row at all. HasID is what separates
	// them, and it is what decides whether Init sends the command.
	for _, name := range []string{"generic", "ftdx9000"} {
		if m := modelNamed(t, name); len(m.IDs) != 0 {
			t.Errorf("%s IDs = %v, want none", name, m.IDs)
		}
	}
	if !modelNamed(t, "generic").HasID {
		t.Error("generic HasID is false; an unprofiled Yaesu still answers ID;")
	}
	if modelNamed(t, "ftdx9000").HasID {
		t.Error("the FTdx9000 HasID is true; its command list has no ID row, so asking costs a timeout")
	}
	if _, ok := ModelForID(0); ok {
		t.Error("ModelForID(0) matched a model; generic must not be reachable by ID")
	}
	// No two profiles may claim the same number, or the mismatch warning would
	// name whichever the scan reached first.
	seen := map[int]string{}
	for _, m := range allModels(t) {
		for _, id := range m.IDs {
			if other, dup := seen[id]; dup {
				t.Errorf("%s and %s both claim ID %04d", other, m.Name, id)
			}
			seen[id] = m.Name
		}
	}
}

// TestFTdx9000MissingCommands pins the commands that radio does not have.
//
// They are capability gaps rather than omissions here: a Yaesu answers a
// command it does not implement with silence, so asking would cost the
// session's full per-command timeout and return nothing. Its Control Command
// List (FTdx9000 Operating Manual, page 2) has no AI, ID, NA, RM, PS or RA row,
// and no AI column in the list at all — the column every other manual here uses
// to mark which commands report themselves unasked.
func TestFTdx9000MissingCommands(t *testing.T) {
	m := modelNamed(t, "ftdx9000")
	if m.HasID || m.HasNarrow || m.HasAI || m.HasMeters || m.HasPowerSwitch {
		t.Errorf("the FTdx9000 claims ID=%v NA=%v AI=%v RM=%v PS=%v; its command list has none of them",
			m.HasID, m.HasNarrow, m.HasAI, m.HasMeters, m.HasPowerSwitch)
	}
	if len(m.Attenuator) != 0 {
		t.Errorf("the FTdx9000 claims an attenuator ladder %v; it has no RA row", m.Attenuator)
	}
	// AI is the difference between a radio that reports a front-panel knob
	// movement and one that is poll-only, so it is pinned in its own right — and
	// it is the ONLY profiled radio here that is poll-only.
	// Its SH is the WIDTH knob's position, 00-31 with 16 centred, and no table
	// in its manual turns that into Hz.
	if m.hasFilterWidth() {
		t.Error("the FTdx9000 claims a bandwidth table; its SH is a knob position")
	}
	// Everybody else has all of them.
	for _, other := range allModels(t) {
		if other.Name == "ftdx9000" {
			continue
		}
		if !other.HasAI {
			t.Errorf("%s claims no AI; the FTdx9000 is the only profiled radio without it", other.Name)
		}
		if !other.HasMeters || !other.HasPowerSwitch || !other.HasTuner {
			t.Errorf("%s: RM=%v PS=%v AC=%v, want all true — the FTdx9000 is the only radio here "+
				"missing any of them", other.Name, other.HasMeters, other.HasPowerSwitch, other.HasTuner)
		}
		if !other.HasID || !other.HasNarrow || !other.hasFilterWidth() {
			t.Errorf("%s: ID=%v NA=%v width=%v, want all true",
				other.Name, other.HasID, other.HasNarrow, other.hasFilterWidth())
		}
		if len(other.Attenuator) == 0 {
			t.Errorf("%s has no attenuator ladder; only the FTdx9000 lacks RA", other.Name)
		}
	}
}

// TestFTdx9000IsNeverAskedForACommandItLacks is the invariant behind the
// profile fields, and it is worth more than any table assertion: it drives every
// entry point this backend has against the one radio with a short command list
// and proves that not one of the six missing commands reaches the wire.
//
// The cost of getting this wrong is not a wrong answer, it is a stall. Each of
// these would be answered with silence and burn the session's whole per-command
// timeout, every poll tick, for as long as the station was up.
func TestFTdx9000IsNeverAskedForACommandItLacks(t *testing.T) {
	y := newModelRig(t, "ftdx9000")
	c := newTestConn(t, y, answersFor(y.profile))
	ctx := context.Background()

	if err := y.Init(ctx, c); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := y.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("PollFast: %v", err)
	}
	// Once transmitting, which is when the meters would otherwise be read.
	mustDecode(t, y, "TX1")
	if err := y.Poll(ctx, c, backend.PollFast); err != nil {
		t.Fatalf("PollFast transmitting: %v", err)
	}
	if err := y.Poll(ctx, c, backend.PollSlow); err != nil {
		t.Fatalf("PollSlow: %v", err)
	}
	// And every setter that might reach for one of them, refused or not.
	_ = y.SetNoiseBlanker(ctx, c, 1)
	_ = y.SetNBLevel(ctx, c, 50)
	_ = y.SetNotch(ctx, c, false)
	_ = y.SetNotchFreq(ctx, c, 50)
	_ = y.SetAntenna(ctx, c, 2)
	_ = y.SetRXAntenna(ctx, c, true)
	_ = y.SetAttenuator(ctx, c, 6)
	_ = y.SetTuner(ctx, c, true)
	_ = y.StartTune(ctx, c)
	_ = y.PowerOn(ctx, c)
	_ = y.PowerOff(ctx, c, false)

	for _, req := range c.sent {
		for _, absent := range []string{"AI", "ID", "NA", "RM", "PS", "RA", "AC"} {
			if strings.HasPrefix(req, absent) {
				t.Errorf("sent %q; the FTdx9000's command list has no %s row (or, for AC, "+
					"not in a shape remoses can drive), so it answers with silence and costs "+
					"a full per-command timeout", req, absent)
			}
		}
	}
}

// TestFTdx9000CapsMatchItsCommandList is the other half: what the client is
// told. A meter that cannot be read must not be advertised, or a UI draws three
// bars that never move and an operator concludes the radio is broken.
func TestFTdx9000CapsMatchItsCommandList(t *testing.T) {
	caps := newModelRig(t, "ftdx9000").Caps()
	for _, tt := range []struct {
		name string
		got  bool
	}{
		{"power_meter", caps.PowerMeter},
		{"swr_meter", caps.SWRMeter},
		{"alc_meter", caps.ALCMeter},
		{"power_switch", caps.PowerSwitch},
		{"tuner_control", caps.TunerControl},
		{"tuner_tune", caps.TunerTune},
	} {
		if tt.got {
			t.Errorf("ftdx9000 claims %s; its command list has no command behind it", tt.name)
		}
	}
	// And every other model does claim them, so the gate is per model and not a
	// family-wide retreat.
	for _, name := range ModelNames() {
		if name == "ftdx9000" {
			continue
		}
		caps := newModelRig(t, name).Caps()
		if !caps.PowerMeter || !caps.SWRMeter || !caps.ALCMeter {
			t.Errorf("%s claims no transmit meters; RM is in its command list", name)
		}
		if !caps.PowerSwitch || !caps.TunerControl {
			t.Errorf("%s claims no PS or AC; both are in its command list", name)
		}
	}
}

// TestEveryLevelHasACeiling is the drift guard behind the per-model ranges.
//
// Five of the twelve profiles are built by older(), which deliberately leaves
// NL's and BP's ceilings unset because its five radios disagree about both —
// so a new entry that forgot to fill them in would compile, poll, and publish
// every reading as 0% while sending 000 for full scale. scaleFrom returns 0
// when its range is empty, which is exactly the confident wrong number this
// backend exists to avoid, and it would look like a radio nobody had touched.
func TestEveryLevelHasACeiling(t *testing.T) {
	for _, m := range allModels(t) {
		if m.NBLevelMax <= m.NBLevelMin {
			t.Errorf("%s: NL range %d-%d is empty; every model here has a blanker level",
				m.Name, m.NBLevelMin, m.NBLevelMax)
		}
		if m.NRLevelMax <= m.NRLevelMin {
			t.Errorf("%s: RL range %d-%d is empty", m.Name, m.NRLevelMin, m.NRLevelMax)
		}
		if m.Notch && m.NotchFreqMax <= notchFreqLo {
			t.Errorf("%s: BP position range 1-%d is empty", m.Name, m.NotchFreqMax)
		}
		if m.RFGain && m.RFGainMax == 0 {
			t.Errorf("%s: RG counts to 0", m.Name)
		}
		if m.MicGain && m.MicGainMax == 0 {
			t.Errorf("%s: MG counts to 0", m.Name)
		}
		if m.ProcLevel && m.ProcLevelMax <= m.ProcLevelMin {
			t.Errorf("%s: PL range %d-%d is empty", m.Name, m.ProcLevelMin, m.ProcLevelMax)
		}
		// And a shape that names a receive antenna has to have sockets for the
		// fifth position to be past, or decodeAN would read a socket as one.
		if m.AntennaShape != AntennaPlain && m.Antennas == 0 {
			t.Errorf("%s: claims a receive-antenna answer shape with no AN sockets", m.Name)
		}
	}
}

// TestPowerSwitchIsNotFamilyWide. PS is in eleven of the twelve command lists
// and absent from the FTdx9000's, so that radio cannot be switched on or off
// over CAT at all — and PowerOn in particular must refuse rather than run its
// escalation, which would spend two per-command timeouts plus the documented
// one-to-two-second wake window on a command the rig has never heard of.
func TestPowerSwitchIsNotFamilyWide(t *testing.T) {
	ctx := context.Background()
	for _, name := range ModelNames() {
		y := newModelRig(t, name)
		c := newTestConn(t, y, map[string]string{reqPS: "PS1"})
		if name == "ftdx9000" {
			for _, tt := range []struct {
				call string
				err  error
			}{
				{"PowerOn", y.PowerOn(ctx, c)},
				{"PowerOff", y.PowerOff(ctx, c, false)},
			} {
				if tt.err == nil {
					t.Errorf("%s was accepted on a radio with no PS command", tt.call)
					continue
				}
				if !errors.Is(tt.err, backend.ErrUnsupported) ||
					!strings.Contains(tt.err.Error(), y.profile.Label) {
					t.Errorf("%s refusal %q should wrap ErrUnsupported and name the radio",
						tt.call, tt.err)
				}
			}
			if len(c.sent) != 0 {
				t.Errorf("wrote %q despite refusing", c.sent)
			}
			continue
		}
		if err := y.PowerOff(ctx, c, false); err != nil {
			t.Fatalf("%s: PowerOff: %v", name, err)
		}
		c.wantSent(t, reqPowerOff)
	}
}

// TestTunerIsDeclinedOnTheFTdx9000. Its AC is one parameter, not three, and the
// legend has no "Tuner ON" value in it: "0: Tuner "OFF" or Tuning Stop (While
// Tuner is engaged) / 1: Start Antenna Tuning (While Tuner is engaged) / 2:
// Tuning has failed (Answer only)".
//
// So there is nothing to send that switches the tuner in, and an answering 0
// means EITHER off OR engaged and idle — publishing TunerOff for it would be a
// confident wrong reading half the time. The whole control is declined rather
// than half-driven, which also stops a malformed AC000; going out and an AC;
// being polled for an answer nothing can be made of.
func TestTunerIsDeclinedOnTheFTdx9000(t *testing.T) {
	y := newModelRig(t, "ftdx9000")
	ctx := context.Background()
	c := newTestConn(t, y, answersFor(y.profile))
	for _, tt := range []struct {
		call string
		err  error
	}{
		{"SetTuner", y.SetTuner(ctx, c, true)},
		{"StartTune", y.StartTune(ctx, c)},
	} {
		if tt.err == nil {
			t.Errorf("%s was accepted; the FTdx9000's AC has no Tuner ON value", tt.call)
			continue
		}
		if !errors.Is(tt.err, backend.ErrUnsupported) ||
			!strings.Contains(tt.err.Error(), y.profile.Label) {
			t.Errorf("%s refusal %q should wrap ErrUnsupported and name the radio", tt.call, tt.err)
		}
	}
	if len(c.sent) != 0 {
		t.Errorf("wrote %q despite refusing", c.sent)
	}
	// And its AC answer is not decoded into a state, in either of the two forms
	// that could arrive: its own one-character one and the three-character one
	// the rest of the family sends.
	for _, frame := range []string{"AC0", "AC1", "AC001", "AC002"} {
		u := mustDecode(t, y, frame)
		if u.Key != keyAC {
			t.Errorf("%s keyed %q, want %q so a read still completes", frame, u.Key, keyAC)
		}
		if u.Patch.Tuner != nil {
			t.Errorf("%s published tuner %q; a 0 there means either off or engaged and idle",
				frame, *u.Patch.Tuner)
		}
	}
	// Everybody else drives it as before.
	other := newModelRig(t, "ft-710")
	c = newTestConn(t, other, answersFor(other.profile))
	if err := other.SetTuner(ctx, c, true); err != nil {
		t.Fatalf("ft-710: SetTuner: %v", err)
	}
	c.wantSent(t, "AC001;", reqAC)
}

// TestFrequencyWidthPerModel is the structural split between the two
// generations. Getting it wrong sends a field the rig cannot parse, which on a
// protocol with no error response is silence and a full timeout.
func TestFrequencyWidthPerModel(t *testing.T) {
	older := map[string]bool{
		"ft-950": true, "ftdx1200": true, "ftdx3000": true,
		"ftdx5000": true, "ftdx9000": true,
	}
	for _, m := range allModels(t) {
		want := freqDigitsModern
		if older[m.Name] {
			want = freqDigitsOld
		}
		if m.FreqDigits != want {
			t.Errorf("%s FreqDigits = %d, want %d", m.Name, m.FreqDigits, want)
		}
		// Whatever the width, the model's own ceiling has to fit in it.
		if _, err := formatFrequency(m.MaxHz, m.FreqDigits); err != nil {
			t.Errorf("%s: its own MaxHz does not fit its FA field: %v", m.Name, err)
		}
	}
}

// TestModeCodeE is the hazard this whole per-model table exists for. The same
// character means two different operating modes depending on the radio, and
// decoding an FT-991A with the family table would report a rig sitting in C4FM
// as PSK — a wrong answer rather than a missing one.
func TestModeCodeE(t *testing.T) {
	tests := []struct {
		model string
		want  radio.Mode
		have  bool
	}{
		{"ft-991a", radio.ModeC4FM, true},
		{"ft-710", radio.ModePSK, true},
		{"ftdx10", radio.ModePSK, true},
		{"ftdx101d", radio.ModePSK, true},
		{"ftdx101mp", radio.ModePSK, true},
		{"ftx-1", radio.ModePSK, true},
		{"generic", radio.ModePSK, true},
		// The FT-891's table has no E at all, so a frame carrying one reports
		// nothing rather than a mode it does not have. Neither has any radio of
		// the FT-950 generation: E arrived with the FTdx101, so there is no PSK
		// and no C4FM anywhere in the older five.
		{"ft-891", radio.ModeUnknown, false},
		{"ft-950", radio.ModeUnknown, false},
		{"ftdx1200", radio.ModeUnknown, false},
		{"ftdx3000", radio.ModeUnknown, false},
		{"ftdx5000", radio.ModeUnknown, false},
		{"ftdx9000", radio.ModeUnknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			m := modelNamed(t, tt.model)
			got, data, ok := m.decodeMode('E')
			if ok != tt.have {
				t.Fatalf("decodeMode('E') ok = %v, want %v", ok, tt.have)
			}
			if !ok {
				return
			}
			if got != tt.want || data {
				t.Errorf("decodeMode('E') = (%s, data %v), want %s", got, data, tt.want)
			}
			if !m.supportsMode(tt.want) {
				t.Errorf("caps omit %s, which E selects on this radio", tt.want)
			}
		})
	}

	// The other half: the FT-991A must not claim PSK, and nobody but the
	// FT-991A may claim plain C4FM.
	if modelNamed(t, "ft-991a").supportsMode(radio.ModePSK) {
		t.Error("the FT-991A advertises PSK; its E code is C4FM")
	}
	for _, name := range []string{"ft-950", "ftdx1200", "ftdx3000", "ftdx5000", "ftdx9000"} {
		if modelNamed(t, name).supportsMode(radio.ModePSK) {
			t.Errorf("%s advertises PSK; this generation has no E code at all", name)
		}
	}
	for _, m := range allModels(t) {
		if m.Name == "ft-991a" {
			continue
		}
		if m.supportsMode(radio.ModeC4FM) {
			t.Errorf("%s advertises plain C4FM", m.Name)
		}
	}
}

// TestModeCodeA covers the two radios with no DATA-FM code, so DATA in FM has
// to be refused rather than approximated with the plain FM code. The FT-891's
// table simply stops short of A; the FTdx1200's prints it "----", which is the
// manual saying outright that the code is unused.
func TestModeCodeA(t *testing.T) {
	without := []string{"ft-891", "ftdx1200"}
	for _, name := range without {
		m := modelNamed(t, name)
		if _, _, ok := m.decodeMode('A'); ok {
			t.Errorf("%s decoded code A; its table does not list one", name)
		}
		if code, err := m.encodeMode(radio.ModeFM, true); err == nil {
			t.Errorf("encodeMode(FM, data) = %q on a %s, want an error", code, name)
		}
	}
	// Everyone else has it, and it means DATA-FM on the newer radios and PKT-FM
	// on the older ones — different words in the manuals for one mode code.
	for _, other := range allModels(t) {
		if slices.Contains(without, other.Name) {
			continue
		}
		mode, data, ok := other.decodeMode('A')
		if !ok || mode != radio.ModeFM || !data {
			t.Errorf("%s decodeMode('A') = (%s, %v, %v), want DATA-FM", other.Name, mode, data, ok)
		}
	}
}

// TestModeCodeD covers AM-N, which three of the five older radios do not have.
// It decodes to plain AM where it exists, so its absence costs nothing on the
// set path — encodeMode always picks 5 — and only means an AM-N frame reports
// nothing rather than AM.
func TestModeCodeD(t *testing.T) {
	without := []string{"ftdx1200", "ftdx5000", "ftdx9000"}
	for _, name := range without {
		if _, _, ok := modelNamed(t, name).decodeMode('D'); ok {
			t.Errorf("%s decoded code D; its MD table has no AM-N row", name)
		}
	}
	for _, other := range allModels(t) {
		if slices.Contains(without, other.Name) {
			continue
		}
		mode, data, ok := other.decodeMode('D')
		if !ok || mode != radio.ModeAM || data {
			t.Errorf("%s decodeMode('D') = (%s, %v, %v), want AM", other.Name, mode, data, ok)
		}
	}
	// AM is still settable everywhere: 5 is the wide code and D was never the
	// one encodeMode would choose.
	for _, m := range allModels(t) {
		if cmd, err := m.modeSet(radio.ModeAM, false); err != nil || cmd != "MD05;" {
			t.Errorf("%s: modeSet(AM) = (%q, %v), want MD05;", m.Name, cmd, err)
		}
	}
}

// TestOlderDataModeNaming pins the one thing the older manuals rename. They
// spell the data modes PKT-L, PKT-U and PKT-FM where the newer ones say
// DATA-L, DATA-U and DATA-FM — same codes, same meaning, so they must decode to
// the same radio.Mode with the DATA flag rather than to a mode of their own.
func TestOlderDataModeNaming(t *testing.T) {
	for _, name := range []string{"ft-950", "ftdx1200", "ftdx3000", "ftdx5000", "ftdx9000"} {
		m := modelNamed(t, name)
		for _, tt := range []struct {
			code byte
			mode radio.Mode
		}{
			{'8', radio.ModeLSB}, // PKT-L / DATA-LSB
			{'C', radio.ModeUSB}, // PKT-U / DATA-USB
		} {
			got, data, ok := m.decodeMode(tt.code)
			if !ok || got != tt.mode || !data {
				t.Errorf("%s decodeMode(%q) = (%s, data %v, %v), want %s with DATA",
					name, tt.code, got, data, ok, tt.mode)
			}
			back, err := m.encodeMode(tt.mode, true)
			if err != nil || back != tt.code {
				t.Errorf("%s encodeMode(%s, data) = (%q, %v), want %q", name, tt.mode, back, err, tt.code)
			}
		}
	}
}

// TestC4FMModes pins the three C4FM values against the two radios that have
// them. They are separate modes because the radios put the distinction in
// different places: the FTX-1 has DN and VW as mode codes, while the FT-991A
// has one C4FM mode and chooses the sub-mode in a menu item remoses does not
// touch.
func TestC4FMModes(t *testing.T) {
	tests := []struct {
		model string
		code  byte
		mode  radio.Mode
	}{
		{"ft-991a", 'E', radio.ModeC4FM},
		{"ftx-1", 'H', radio.ModeC4FMDN},
		{"ftx-1", 'I', radio.ModeC4FMVW},
	}
	for _, tt := range tests {
		t.Run(tt.mode.String(), func(t *testing.T) {
			m := modelNamed(t, tt.model)

			got, data, ok := m.decodeMode(tt.code)
			if !ok || got != tt.mode || data {
				t.Fatalf("decodeMode(%q) = (%s, %v, %v), want %s", tt.code, got, data, ok, tt.mode)
			}
			back, err := m.encodeMode(tt.mode, false)
			if err != nil {
				t.Fatalf("encodeMode(%s): %v", tt.mode, err)
			}
			if back != tt.code {
				t.Fatalf("encodeMode(%s) = %q, want %q", tt.mode, back, tt.code)
			}
			if !m.supportsMode(tt.mode) {
				t.Errorf("caps omit %s", tt.mode)
			}
			if cmd, err := m.modeSet(tt.mode, false); err != nil || cmd != "MD0"+string(tt.code)+";" {
				t.Errorf("modeSet(%s) = (%q, %v)", tt.mode, cmd, err)
			}
		})
	}

	// The mapping is one to one in both directions, so nothing is silently
	// changed: an FTX-1 in VW reads back as VW.
	ftx := modelNamed(t, "ftx-1")
	if ftx.supportsMode(radio.ModeC4FM) {
		t.Error("the FTX-1 advertises plain C4FM; its codes are DN and VW")
	}
	f991 := modelNamed(t, "ft-991a")
	for _, m := range []radio.Mode{radio.ModeC4FMDN, radio.ModeC4FMVW} {
		if f991.supportsMode(m) {
			t.Errorf("the FT-991A advertises %s; its sub-mode is a menu item, not a mode code", m)
		}
		if _, err := f991.encodeMode(m, false); err == nil {
			t.Errorf("encodeMode(%s) succeeded on an FT-991A", m)
		}
	}
	// And no other radio here has C4FM at all.
	for _, m := range allModels(t) {
		if m.Name == "ft-991a" || m.Name == "ftx-1" {
			continue
		}
		for _, c := range []byte{'H', 'I'} {
			if mode, _, ok := m.decodeMode(c); ok {
				t.Errorf("%s decoded %q as %s", m.Name, c, mode)
			}
		}
	}
}

// TestModeTableRoundTrip walks every code of every model in both directions.
// The codes that decode to a mode another code also decodes to — FM-N, AM-N,
// DATA-FM-N — are the exception: they read back as their wide sibling, which is
// what encodeMode is documented to do.
func TestModeTableRoundTrip(t *testing.T) {
	// wide names the code encodeMode is expected to choose for a mode that has
	// two.
	wide := map[byte]byte{'B': '4', 'D': '5', 'F': 'A'}

	for _, m := range allModels(t) {
		t.Run(m.Name, func(t *testing.T) {
			for code, want := range m.Codes {
				mode, data, ok := m.decodeMode(code)
				if !ok || mode != want.mode || data != want.data {
					t.Errorf("decodeMode(%q) = (%s, %v, %v), want (%s, %v)",
						code, mode, data, ok, want.mode, want.data)
				}
				// Case is not significant anywhere in this protocol.
				if lower := code | 0x20; lower != code {
					if got, _, ok := m.decodeMode(lower); !ok || got != want.mode {
						t.Errorf("decodeMode(%q) = (%s, %v), want %s", lower, got, ok, want.mode)
					}
				}

				back, err := m.encodeMode(want.mode, want.data)
				if err != nil {
					t.Fatalf("encodeMode(%s, %v): %v", want.mode, want.data, err)
				}
				expect := code
				if w, narrow := wide[code]; narrow {
					expect = w
				}
				if back != expect {
					t.Errorf("encodeMode(%s, %v) = %q, want %q", want.mode, want.data, back, expect)
				}
			}

			// Codes the manuals mark unused, and letters past the table, report
			// nothing rather than ModeUnknown: letting one through would
			// overwrite a good cached mode.
			for _, c := range []byte{'0', 'G', 'J', 'K', 'Z', ' ', '#'} {
				if mode, _, ok := m.decodeMode(c); ok {
					t.Errorf("decodeMode(%q) = %s, want no mode", c, mode)
				}
			}
			if _, err := m.encodeMode(radio.ModeUnknown, false); err == nil {
				t.Error("encodeMode(unknown) succeeded")
			}
		})
	}
}

// TestEncodeModePrefersTheWideCode pins the rule that makes encoding
// deterministic. Several modes have two codes, and Yaesu splits narrow off into
// the separate NA command, so the wide one is always the right choice — an
// unordered map scan would pick either at random.
func TestEncodeModePrefersTheWideCode(t *testing.T) {
	m := modelNamed(t, "ft-710")
	for _, tt := range []struct {
		mode radio.Mode
		data bool
		want byte
	}{
		{radio.ModeFM, false, '4'}, // not B, FM-N
		{radio.ModeAM, false, '5'}, // not D, AM-N
		{radio.ModeFM, true, 'A'},  // not F, DATA-FM-N
		{radio.ModeLSB, true, '8'},
		{radio.ModeUSB, true, 'C'},
		{radio.ModeCW, false, '3'},
		{radio.ModeCWR, false, '7'},
		{radio.ModeFSK, false, '6'},
		{radio.ModeFSKR, false, '9'},
		{radio.ModePSK, false, 'E'},
	} {
		// Run it repeatedly: an unordered scan would pass by luck once.
		for range 20 {
			got, err := m.encodeMode(tt.mode, tt.data)
			if err != nil || got != tt.want {
				t.Fatalf("encodeMode(%s, data=%v) = (%q, %v), want %q", tt.mode, tt.data, got, err, tt.want)
			}
		}
	}
}

// TestModeSetRejectsWhatTheRadioLacks covers the guard before anything reaches
// the wire, which matters more here than on other backends: a Yaesu answers a
// command it will not accept with silence, so the request would cost a full
// timeout instead of a rejection.
func TestModeSetRejectsWhatTheRadioLacks(t *testing.T) {
	tests := []struct {
		model    string
		mode     radio.Mode
		data     bool
		wantWord string
	}{
		{"ft-991a", radio.ModePSK, false, "does not have mode"},
		{"ft-891", radio.ModePSK, false, "does not have mode"},
		{"ft-891", radio.ModeC4FM, false, "does not have mode"},
		{"ftdx101d", radio.ModeC4FMDN, false, "does not have mode"},
		{"ft-710", radio.ModeCW, true, "no DATA mode code"},
		{"ft-710", radio.ModePSK, true, "no DATA mode code"},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+tt.mode.String(), func(t *testing.T) {
			m := modelNamed(t, tt.model)
			cmd, err := m.modeSet(tt.mode, tt.data)
			if err == nil {
				t.Fatalf("modeSet(%s, data=%v) = %q, want an error", tt.mode, tt.data, cmd)
			}
			if !strings.Contains(err.Error(), tt.wantWord) {
				t.Errorf("error %q does not explain itself (want %q in it)", err, tt.wantWord)
			}
		})
	}
}

// TestModeSetCarriesTheReceiverSelector pins the shape of the command itself.
// P1 is mandatory even on the single-receiver radios, where the manual
// documents it as "0: Fixed", so MD3; — the Kenwood spelling — is malformed.
func TestModeSetCarriesTheReceiverSelector(t *testing.T) {
	for _, m := range allModels(t) {
		cmd, err := m.modeSet(radio.ModeCW, false)
		if err != nil {
			t.Fatalf("%s: modeSet(CW): %v", m.Name, err)
		}
		if cmd != "MD03;" {
			t.Errorf("%s: modeSet(CW) = %q, want MD03;", m.Name, cmd)
		}
	}
}

func TestCheckFrequency(t *testing.T) {
	tests := []struct {
		model string
		hz    uint64
		ok    bool
	}{
		{"ft-991a", 14_025_000, true},
		{"ft-991a", 435_000_000, true},
		{"ft-991a", 470_000_001, false},
		{"ft-991a", 29_999, false},
		// The FT-891 is HF and 6 m only, so 2 m is out of range where the
		// FT-991A takes it.
		{"ft-891", 50_100_000, true},
		{"ft-891", 144_200_000, false},
		{"ft-710", 70_200_000, true},
		{"ft-710", 144_200_000, false},
		{"ftx-1", 435_000_000, true},
		// The FT-950 and FTdx1200 stop at 56 MHz where the FTdx3000, FTdx5000
		// and FTdx9000 reach 60.
		{"ft-950", 50_100_000, true},
		{"ft-950", 57_000_000, false},
		{"ftdx1200", 57_000_000, false},
		{"ftdx3000", 57_000_000, true},
		{"ftdx5000", 60_000_000, true},
		{"ftdx5000", 60_000_001, false},
		{"ftdx9000", 1_840_000, true},
		{"ftdx9000", 144_200_000, false},
		// generic is deliberately permissive: refusing a frequency the radio
		// can tune would be worse than letting the rig ignore one it cannot.
		{"generic", 435_000_000, true},
	}
	for _, tt := range tests {
		m := modelNamed(t, tt.model)
		err := m.checkFrequency(tt.hz)
		if (err == nil) != tt.ok {
			t.Errorf("%s checkFrequency(%d) = %v, want ok=%v", tt.model, tt.hz, err, tt.ok)
		}
		if err != nil && !strings.Contains(err.Error(), m.Label) {
			t.Errorf("error %q does not name the radio", err)
		}
	}
}

func TestCapsPerModel(t *testing.T) {
	tests := []struct {
		model      string
		maxPowerW  float64
		rawPower   bool
		noWidth    bool
		extraModes []radio.Mode
	}{
		{model: "ft-991a", maxPowerW: 100, extraModes: []radio.Mode{radio.ModeC4FM}},
		{model: "ft-891", maxPowerW: 100},
		{model: "ft-710", maxPowerW: 100, extraModes: []radio.Mode{radio.ModePSK}},
		{model: "ftdx10", maxPowerW: 100, extraModes: []radio.Mode{radio.ModePSK}},
		{model: "ftdx101d", maxPowerW: 100, extraModes: []radio.Mode{radio.ModePSK}},
		{model: "ftdx101mp", maxPowerW: 200, extraModes: []radio.Mode{radio.ModePSK}},
		// The FTX-1's field head alone is a 10 W radio; PC; refines this once
		// the SPA-1 answers.
		{model: "ftx-1", maxPowerW: 10,
			extraModes: []radio.Mode{radio.ModePSK, radio.ModeC4FMDN, radio.ModeC4FMVW}},
		{model: "generic", maxPowerW: 100, extraModes: []radio.Mode{radio.ModePSK}},

		// The FT-950 generation: no mode beyond the common eight anywhere.
		{model: "ft-950", maxPowerW: 100},
		{model: "ftdx1200", maxPowerW: 100},
		{model: "ftdx3000", maxPowerW: 100},
		// The FTdx5000 and FTdx9000 report no watt ceiling at all, because
		// their PC is an uncalibrated index and publishing the nameplate rating
		// would invite a client to read the index as watts.
		{model: "ftdx5000", maxPowerW: 0, rawPower: true},
		{model: "ftdx9000", maxPowerW: 0, rawPower: true, noWidth: true},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			c := newModelRig(t, tt.model).Caps()

			if c.MaxPowerW != tt.maxPowerW {
				t.Errorf("MaxPowerW = %v, want %v", c.MaxPowerW, tt.maxPowerW)
			}
			if c.PowerWattAccurate == tt.rawPower {
				t.Errorf("PowerWattAccurate = %v, want %v", c.PowerWattAccurate, !tt.rawPower)
			}
			if c.SMeterScale != 255 {
				t.Errorf("SMeterScale = %d, want 255", c.SMeterScale)
			}
			if c.FilterWidth == tt.noWidth {
				t.Errorf("FilterWidth = %v, want %v", c.FilterWidth, !tt.noWidth)
			}
			if c.FilterSlots != 0 {
				t.Errorf("FilterSlots = %d, want 0; there is no FL-equivalent command", c.FilterSlots)
			}
			if c.SubReceiver {
				t.Error("SubReceiver true; this backend reads and writes MAIN only")
			}
			// No Yaesu here can key arbitrary text over CAT, so the daemon must
			// steer the operator to serial_key.
			if c.CWMethod != radio.CWNone {
				t.Errorf("CWMethod = %q, want %q", c.CWMethod, radio.CWNone)
			}
			if c.CWCharset != "" {
				t.Errorf("CWCharset = %q, want empty", c.CWCharset)
			}

			for _, m := range modesCommon() {
				if !c.SupportsMode(m) {
					t.Errorf("caps omit %s", m)
				}
			}
			for _, m := range []radio.Mode{
				radio.ModePSK, radio.ModeC4FM, radio.ModeC4FMDN, radio.ModeC4FMVW,
			} {
				want := slices.Contains(tt.extraModes, m)
				if got := c.SupportsMode(m); got != want {
					t.Errorf("SupportsMode(%s) = %v, want %v", m, got, want)
				}
			}
			// PSK-R has no code on any of these radios.
			if c.SupportsMode(radio.ModePSKR) {
				t.Error("caps advertise PSK-R; no Yaesu here has a code for it")
			}
		})
	}
}

// TestCapsModesAreCopied guards the one shared-state hazard in the registry: a
// profile's mode slice is handed out through the API on every call.
func TestCapsModesAreCopied(t *testing.T) {
	y := newModelRig(t, "ftx-1")
	c := y.Caps()
	c.Modes[0] = radio.ModeUnknown
	if y.Caps().Modes[0] == radio.ModeUnknown {
		t.Error("Caps shares its mode slice with the registry; a client could mutate every session's capabilities")
	}
}

// TestInitWarnsOnIdentityMismatch covers the cross-check. The configuration
// stays authoritative — a mismatch is a warning, not a refusal — because acting
// on the ID would silently switch mode tables under an operator who wrote
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
			name: "an FT-991A configured as an FT-710", model: "ft-710", id: "ID0670",
			wantWords: []string{"ft-710", "FT-991A", "0670"},
		},
		{
			// The two FTdx101 variants are the mistake most worth catching:
			// same radio to look at, different power ceiling.
			name: "an MP configured as a D", model: "ftdx101d", id: "ID0682",
			wantWords: []string{"ftdx101d", "FTdx101MP", "0682"},
		},
		{
			name: "an ID no profile claims", model: "ft-710", id: "ID0999",
			wantWords: []string{"ft-710", "0999"},
		},
		{
			name: "an FT-950 configured as an FTdx3000", model: "ftdx3000", id: "ID0310",
			wantWords: []string{"ftdx3000", "FT-950", "0310"},
		},
		{name: "the configured model", model: "ft-710", id: "ID0800", wantSilent: true},
		{name: "generic never warns", model: "generic", id: "ID0800", wantSilent: true},
		// Both of the FTdx1200's numbers are the FTdx1200, so neither warns.
		{name: "an FTdx1200 with the FFT-1", model: "ftdx1200", id: "ID0582", wantSilent: true},
		{name: "an FTdx1200 without it", model: "ftdx1200", id: "ID0583", wantSilent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			restore := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(restore) })

			y := newModelRig(t, tt.model)
			answers := answersFor(y.profile)
			answers[reqID] = tt.id
			if err := y.Init(context.Background(), newTestConn(t, y, answers)); err != nil {
				t.Fatalf("Init: %v", err)
			}
			if y.profile.Name != tt.model {
				t.Errorf("profile changed to %q; the configuration is authoritative", y.profile.Name)
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

// TestModelReportsTheRig covers the identity path. The rig wins over the
// configuration for display, because ID; genuinely names a model.
func TestModelReportsTheRig(t *testing.T) {
	for _, tt := range []struct {
		configured string
		frame      string
		want       string
	}{
		{"ft-710", "ID0800", "Yaesu FT-710"},
		{"ftdx101d", "ID0682", "Yaesu FTdx101MP"},
		{"ft-991a", "ID0670", "Yaesu FT-991A"},
		{"ftx-1", "ID0840", "Yaesu FTX-1"},
		{"generic", "ID0999", "Yaesu ID 0999"},
		{"ft-950", "ID0310", "Yaesu FT-950"},
		// Either number names the same radio.
		{"ftdx1200", "ID0582", "Yaesu FTdx1200"},
		{"ftdx1200", "ID0583", "Yaesu FTdx1200"},
		{"generic", "ID0583", "Yaesu FTdx1200"},
	} {
		y := newModelRig(t, tt.configured)
		mustDecode(t, y, tt.frame)
		if got := y.Model(); got != tt.want {
			t.Errorf("model %q after %s = %q, want %q", tt.configured, tt.frame, got, tt.want)
		}
	}

	// Before any ID; the configured name stands in, and an unnamed model claims
	// no identity at all.
	if got := newModelRig(t, "ftdx10").Model(); got != "Yaesu FTdx10" {
		t.Errorf("Model = %q before Init, want the configured name", got)
	}
	y, err := New(&config.Radio{Yaesu: &config.Yaesu{AutoInformation: true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := y.Model(); got != "" {
		t.Errorf("Model = %q with no configured model, want empty", got)
	}
}
