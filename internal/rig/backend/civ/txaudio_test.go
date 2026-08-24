package civ

import (
	"context"
	"errors"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The session finds this interface by type assertion, so a method whose
// signature drifts from it does not fail to compile — it simply stops being
// found, and every transmit audio request answers "this radio cannot" with
// nothing anywhere saying why. Hence the assertion.
var _ backend.TXAudioController = (*Rig)(nil)

// TestTXAudioLevelsRoundTrip covers 14 0B and 14 0E, and the round trip is the
// point: a level remoses writes and never reads back would report whatever it
// last wrote for ever, which is the defect this backend has already been bitten
// by twice (DESIGN.md §5.4, "values written but never read back").
//
// The percentages are chosen to land on exact BCD counts so the assertion is
// about the encoding rather than about rounding: 0-255 over 0-100% is 2.55
// counts per point.
func TestTXAudioLevelsRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		what string
		pct  float64
		want [2]byte
	}{
		{"minimum", 0, [2]byte{0x00, 0x00}},
		{"half", 50, [2]byte{0x01, 0x28}}, // 128 of 255
		{"maximum", 100, [2]byte{0x02, 0x55}},
	} {
		r, s := modelRig(t, "ic-7610")
		ctx := context.Background()

		if err := r.SetTXAudioGain(ctx, s, tt.pct); err != nil {
			t.Fatalf("SetTXAudioGain(%v): %v", tt.pct, err)
		}
		if s.micGain != tt.want {
			t.Errorf("SetTXAudioGain(%v%%) sent %X, want %X", tt.pct, s.micGain, tt.want)
		}
		u, err := r.Decode(fromRig(cmdLevel, subMicGain, s.micGain[0], s.micGain[1]))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Key != KeyMicGain {
			t.Errorf("a 14 0B answer decoded as key %q, want %q", u.Key, KeyMicGain)
		}
		if u.Patch.TXAudioGain == nil || int(*u.Patch.TXAudioGain+0.5) != int(tt.pct) {
			t.Errorf("%s mic gain decoded as %v, want %v%%", tt.what, u.Patch.TXAudioGain, tt.pct)
		}

		if err := r.SetProcLevel(ctx, s, tt.pct); err != nil {
			t.Fatalf("SetProcLevel(%v): %v", tt.pct, err)
		}
		if s.procLevel != tt.want {
			t.Errorf("SetProcLevel(%v%%) sent %X, want %X", tt.pct, s.procLevel, tt.want)
		}
		u, err = r.Decode(fromRig(cmdLevel, subProcLevel, s.procLevel[0], s.procLevel[1]))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Key != KeyProcLevel {
			t.Errorf("a 14 0E answer decoded as key %q, want %q", u.Key, KeyProcLevel)
		}
		if u.Patch.ProcLevel == nil || int(*u.Patch.ProcLevel+0.5) != int(tt.pct) {
			t.Errorf("%s compressor level decoded as %v, want %v%%",
				tt.what, u.Patch.ProcLevel, tt.pct)
		}
	}
}

// TestTXAudioLevelsAreNotConfusedWithTheirNeighbours is why this file exists at
// all.
//
// 14 0B sits between the RF power (14 0A) and the keyer speed (14 0C), and
// 14 0E next to the notch position (14 0D). All four are two-byte BCD levels in
// one command group, so a sub-command that is one out is ACCEPTED by the radio
// and silently sets the wrong thing — the RF power, or the operator's keyer
// speed, from a request that said "microphone gain". Nothing in the exchange
// would say so.
func TestTXAudioLevelsAreNotConfusedWithTheirNeighbours(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	ctx := context.Background()

	before := struct{ power, speed, notch [2]byte }{s.power, s.speed, s.notchFreq}
	if err := r.SetTXAudioGain(ctx, s, 25); err != nil {
		t.Fatalf("SetTXAudioGain: %v", err)
	}
	if err := r.SetProcLevel(ctx, s, 75); err != nil {
		t.Fatalf("SetProcLevel: %v", err)
	}
	if s.power != before.power {
		t.Errorf("the RF power moved to %X from %X: 14 0B went out as 14 0A",
			s.power, before.power)
	}
	if s.speed != before.speed {
		t.Errorf("the keyer speed moved to %X from %X: a transmit level went out as 14 0C",
			s.speed, before.speed)
	}
	if s.notchFreq != before.notch {
		t.Errorf("the notch position moved to %X from %X: 14 0E went out as 14 0D",
			s.notchFreq, before.notch)
	}
	if s.micGain != [2]byte{0x00, 0x64} { // 25% of 255 = 63.75, rounded
		t.Errorf("mic gain is %X, want 00 64", s.micGain)
	}
	if s.procLevel != [2]byte{0x01, 0x91} { // 75% of 255 = 191.25, rounded
		t.Errorf("compressor level is %X, want 01 91", s.procLevel)
	}
}

// TestProcSwitchEncoding covers 16 44 in both directions.
func TestProcSwitchEncoding(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	for _, tt := range []struct {
		on   bool
		want byte
	}{
		{true, 0x01},
		{false, 0x00},
	} {
		if err := r.SetProc(context.Background(), s, tt.on); err != nil {
			t.Fatalf("SetProc(%v): %v", tt.on, err)
		}
		if s.proc != tt.want {
			t.Errorf("SetProc(%v) sent %#02x, want %#02x", tt.on, s.proc, tt.want)
		}
		u, err := r.Decode(fromRig(cmdFunc, subProc, s.proc))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Key != KeyProc {
			t.Errorf("a 16 44 answer decoded as key %q, want %q", u.Key, KeyProc)
		}
		if u.Patch.Proc == nil || *u.Patch.Proc != tt.on {
			t.Errorf("%#02x decoded as %v, want %v", s.proc, u.Patch.Proc, tt.on)
		}
	}
}

// TestTXAudioLevelsRefuseOutOfRange: setLevel takes a percentage, and anything
// outside 0-100 is the caller's mistake rather than something to clamp into a
// transmitter.
func TestTXAudioLevelsRefuseOutOfRange(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	ctx := context.Background()
	for _, pct := range []float64{-1, 101} {
		if err := r.SetTXAudioGain(ctx, s, pct); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("SetTXAudioGain(%v) returned %v, want ErrUnsupported", pct, err)
		}
		if err := r.SetProcLevel(ctx, s, pct); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("SetProcLevel(%v) returned %v, want ErrUnsupported", pct, err)
		}
	}
}

// TestTXAudioIsPerModel: the three are claimed exactly where a radio's own
// command table prints the row, which is not the same set for all three.
func TestTXAudioIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model                    string
		micGain, proc, procLevel bool
	}{
		// The current generation, whose reference guides print all three in the
		// same rows with the same ranges.
		{"ic-7610", true, true, true},
		{"ic-9700", true, true, true},
		{"ic-7760", true, true, true},
		{"ic-905", true, true, true},
		{"ic-7300mk2", true, true, true},
		{"ic-7700", true, true, true},
		{"ic-7850", true, true, true},
		{"ic-7300", true, true, true},
		{"ic-7600", true, true, true},
		{"ic-9100", true, true, true},
		// The IC-910H has all three too, which is worth asserting because this
		// profile is the family's outlier everywhere else — and because the
		// entry used to claim it had no command 14 at all. Its 14 0E is a plain
		// 0-100% over 0-255, which is what setLevel writes.
		{"ic-910h", true, true, true},
		// The IC-718 has the gain and the switch and no level: its 14 group
		// jumps 0C straight to 0F, so there is no 14 0E to send.
		{"ic-718", true, true, false},
		// The IC-703 has all three rows printed, and its 14 0E is still not
		// claimed — that table gives the compressor level as a 0-to-10 field
		// where the rest of the family gives 0-255 carrying a 0-10 scale, and
		// this backend has one level encoder. See the model entry.
		{"ic-703", true, true, false},
		// The IC-706MKIIG's table names "COMP setting" in its 16 group and has
		// no command 14 whatsoever.
		{"ic-706mkiig", false, true, false},
		// The other two IC-706s have no command 16 either — their tables run 05
		// to 10 and stop.
		{"ic-706", false, false, false},
		{"ic-706mkii", false, false, false},
		// And an unidentified Icom gets the two every table agrees about and
		// not the one they do not: 14 0E is absent on the IC-718 and is a
		// 0-to-10 field on the IC-703, so writing 255 to an unknown radio could
		// be an out-of-range level rather than a refusal.
		{"generic", true, true, false},
	} {
		r, _ := modelRig(t, tt.model)
		c := r.Caps()
		got := []bool{c.TXAudioGainControl, c.ProcControl, c.ProcLevelControl}
		want := []bool{tt.micGain, tt.proc, tt.procLevel}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: caps %v, want %v", tt.model, got, want)
				break
			}
		}
	}
}

// TestCapsAndSettersAgree is the invariant that outlives any one model.
//
// A capability list promising what the next call rejects is a failure this
// project has already shipped once (DESIGN.md §5.5, Caps.VFOs). So for every
// model in the registry, each of the three flags must agree with what its
// setter does — claimed and accepted, or unclaimed and refused with
// ErrUnsupported, which the API turns into a 422 rather than a 500.
func TestCapsAndSettersAgree(t *testing.T) {
	ctx := context.Background()
	for _, name := range ModelNames() {
		r, s := modelRig(t, name)
		c := r.Caps()
		for _, tc := range []struct {
			what   string
			claims bool
			err    error
		}{
			{"mic gain", c.TXAudioGainControl, r.SetTXAudioGain(ctx, s, 50)},
			{"speech compressor", c.ProcControl, r.SetProc(ctx, s, true)},
			{"compressor level", c.ProcLevelControl, r.SetProcLevel(ctx, s, 50)},
		} {
			switch {
			case tc.claims && tc.err != nil:
				t.Errorf("%s claims %s and then refused it: %v", name, tc.what, tc.err)
			case !tc.claims && tc.err == nil:
				t.Errorf("%s accepted a %s set it does not claim", name, tc.what)
			case !tc.claims && !errors.Is(tc.err, backend.ErrUnsupported):
				t.Errorf("%s refused %s with %v, which is not ErrUnsupported and would "+
					"be a 500", name, tc.what, tc.err)
			}
		}
	}
}

// TestTXAudioReadsFollowCaps: a value written and never read reports whatever
// remoses last wrote, so everything claimed has to be on the poll — and nothing
// that is not claimed may be, or a radio without the row draws an NG every slow
// tick for a field that can never arrive.
func TestTXAudioReadsFollowCaps(t *testing.T) {
	for _, name := range ModelNames() {
		r, s := modelRig(t, name)
		c := r.Caps()
		// A rejection somewhere in the tier is expected on the older profiles
		// and is not what this is about; readAll carries on past one.
		_ = r.Poll(context.Background(), s, backend.PollSlow)

		asked := map[string]bool{}
		for _, f := range s.log {
			if len(f) < 6 {
				continue
			}
			switch {
			case f[4] == cmdLevel && f[5] == subMicGain:
				asked["mic gain"] = true
			case f[4] == cmdFunc && f[5] == subProc:
				asked["speech compressor"] = true
			case f[4] == cmdLevel && f[5] == subProcLevel:
				asked["compressor level"] = true
			}
		}
		for what, want := range map[string]bool{
			"mic gain":          c.TXAudioGainControl,
			"speech compressor": c.ProcControl,
			"compressor level":  c.ProcLevelControl,
		} {
			if asked[what] != want {
				t.Errorf("%s: the slow tier asked for %s %v, want %v",
					name, what, asked[what], want)
			}
		}
	}
}

// TestTXAudioSurvivesTheSlowTier is the IC-9700's starvation bug applied to the
// new reads: they are appended after the noise group, so they sit behind 16 57
// — the command that radio rejects in FM, and the one that used to skip
// everything queued after it.
func TestTXAudioSurvivesTheSlowTier(t *testing.T) {
	r, s := modelRig(t, "ic-9700")
	s.reject = map[string]bool{"16/57": true}

	if err := r.Poll(context.Background(), s, backend.PollSlow); !errors.Is(err, ErrRejected) {
		t.Fatalf("Poll returned %v, want an ErrRejected the session treats as non-fatal", err)
	}
	var sawLevel bool
	for _, f := range s.log {
		if len(f) >= 6 && f[4] == cmdLevel && f[5] == subProcLevel {
			sawLevel = true
		}
	}
	if !sawLevel {
		t.Error("the compressor level read never went out: one refused command starved " +
			"the reads queued behind it")
	}
}

// TestProcLevelIsPolledWithTheCompressorOff records a decision rather than a
// protocol fact.
//
// Kenwood refuses a noise level while its circuit is off and says so in its
// reference, which is why that backend orders the switch before the level.
// Nothing in any Icom reference read here says the compressor level is refused
// with the compressor out, and radio.State.ProcLevel promises the value a
// client would return to — so the read goes out either way.
func TestProcLevelIsPolledWithTheCompressorOff(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	s.proc = 0x00
	_ = r.Poll(context.Background(), s, backend.PollSlow)
	for _, f := range s.log {
		if len(f) >= 6 && f[4] == cmdLevel && f[5] == subProcLevel {
			return
		}
	}
	t.Error("14 0E was not read with the compressor off; state.proc_level would then " +
		"be absent exactly when a client wants to draw the control")
}

// TestMicGainIsNotAboutAConnector is a documentation test, and it is here
// because getting this wrong is a promise to an operator rather than a bug.
//
// Which socket feeds the modulator is a Set-mode item on this family — the
// IC-7610's 1A 05 00 91 chooses between MIC, ACC, USB and LAN — and remoses
// neither reads nor writes it. So 14 0B is the gain on whatever input the radio
// is taking audio from, and nothing here may claim otherwise. That is why the
// command Icom calls MIC gain is published as tx_audio_gain.
func TestMicGainIsNotAboutAConnector(t *testing.T) {
	r, s := modelRig(t, "ic-7610")
	if err := r.SetTXAudioGain(context.Background(), s, 40); err != nil {
		t.Fatalf("SetTXAudioGain: %v", err)
	}
	for _, f := range s.log {
		if len(f) >= 5 && f[4] == cmdMisc {
			t.Errorf("setting the transmit gain sent a 1A frame (% X); the connector "+
				"selection is the operator's persistent configuration and is not "+
				"remoses's to write", f)
		}
	}
}
