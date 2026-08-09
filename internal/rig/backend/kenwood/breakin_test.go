package kenwood

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// The family spells break-in four ways, and two of them cannot say semi from
// full without the SD delay. These tests pin the wire traffic for each, because
// a wrong command here is the failure that transmits nothing while reporting
// success.

func TestSetBreakInPerModel(t *testing.T) {
	tests := []struct {
		model string
		v     radio.BreakIn
		// delay is the SD reading the rig starts from, in ms.
		delay int32
		want  []string
	}{
		// TS-590: VX, plus SD to choose between semi and full.
		{"ts590sg", radio.BreakInOff, 300, []string{"VX0;", "VX;"}},
		{"ts590sg", radio.BreakInFull, 300, []string{"SD0000;", "SD;", "VX1;", "VX;"}},
		// Already on a non-zero delay, so the operator's own value is left alone.
		{"ts590sg", radio.BreakInSemi, 300, []string{"VX1;", "VX;"}},
		// Coming from full there is no previous delay to keep: SD is 0, which
		// IS full, so semi has to pick a value.
		{"ts590sg", radio.BreakInSemi, 0, []string{"SD0300;", "SD;", "VX1;", "VX;"}},

		// The TS-480 is the same VX shape. Its reference does not say so — see
		// the profile for why remoses reads the silence as an omission — so
		// this is the assertion to revisit first if an operator reports VOX
		// coming on when they send CW.
		{"ts480", radio.BreakInOff, 300, []string{"VX0;", "VX;"}},
		{"ts480", radio.BreakInSemi, 300, []string{"VX1;", "VX;"}},
		{"ts480", radio.BreakInFull, 300, []string{"SD0000;", "SD;", "VX1;", "VX;"}},

		// TS-890S: the same two-value shape on BI instead of VX.
		{"ts890s", radio.BreakInOff, 300, []string{"BI0;", "BI;"}},
		{"ts890s", radio.BreakInFull, 300, []string{"SD0000;", "SD;", "BI1;", "BI;"}},
		{"ts890s", radio.BreakInSemi, 0, []string{"SD0300;", "SD;", "BI1;", "BI;"}},

		// TS-990S: three values on BI, so the delay is never touched.
		{"ts990s", radio.BreakInOff, 0, []string{"BI0;", "BI;"}},
		{"ts990s", radio.BreakInSemi, 0, []string{"BI1;", "BI;"}},
		{"ts990s", radio.BreakInFull, 300, []string{"BI2;", "BI;"}},
	}
	for _, tt := range tests {
		t.Run(tt.model+"/"+string(tt.v), func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(radio.ModeCW))
			k.breakInDelayMS.Store(tt.delay)
			c := newTestConn(t, k, answersFor(k.profile))
			if err := k.SetBreakIn(context.Background(), c, tt.v); err != nil {
				t.Fatalf("SetBreakIn: %v", err)
			}
			c.wantSent(t, tt.want...)
		})
	}
}

// The S and the SG are one radio as far as this backend is concerned: their
// reference is a single document, which marks the commands remoses uses
// "[TS-590S / TS-590SG common]" throughout, and the two differ only in the ID
// they answer with. Everything verified on the S therefore holds for the SG,
// and this test is what keeps that true — a fix applied to one profile and not
// the other would be invisible until somebody put an SG on the air.
func TestTS590SAndSGAreOneProfile(t *testing.T) {
	s, err := LookupModel("ts590s")
	if err != nil {
		t.Fatalf("LookupModel(ts590s): %v", err)
	}
	sg, err := LookupModel("ts590sg")
	if err != nil {
		t.Fatalf("LookupModel(ts590sg): %v", err)
	}
	if s.ID == sg.ID {
		t.Errorf("both report ID %d; the ID is the one thing that separates them", s.ID)
	}
	// Normalise away the three fields that are allowed to differ, then require
	// everything else — break-in style included — to match exactly.
	s.Name, s.Label, s.ID = "", "", 0
	sg.Name, sg.Label, sg.ID = "", "", 0
	if !reflect.DeepEqual(s, sg) {
		t.Errorf("the TS-590S and TS-590SG profiles have diverged:\n S = %+v\nSG = %+v", s, sg)
	}
	if s.BreakIn != BreakInVX {
		t.Errorf("break-in style = %v, want BreakInVX: this generation has no BI command", s.BreakIn)
	}
}

func TestSetBreakInUnsupported(t *testing.T) {
	// generic is a deliberate abstention rather than a gap. It copies the
	// TS-590 shape, whose break-in command is VX, and VX means VOX on a radio
	// that turns out not to be a Kenwood of that era — this dialect is spoken
	// by Elecraft and modern Yaesu too. Writing it blind would leave VOX on,
	// which surfaces later in SSB rather than where it was caused, so remoses
	// declines to guess for an unidentified radio.
	for _, name := range []string{"generic"} {
		t.Run(name, func(t *testing.T) {
			k := newModelRig(t, name)
			k.mode.Store(uint32(radio.ModeCW))
			c := newTestConn(t, k, answersFor(k.profile))
			err := k.SetBreakIn(context.Background(), c, radio.BreakInSemi)
			if err == nil {
				t.Fatal("SetBreakIn accepted on a radio with no break-in command")
			}
			if !errors.Is(err, backend.ErrUnsupported) {
				t.Errorf("error = %v, want backend.ErrUnsupported", err)
			}
			if k.Caps().BreakInControl {
				t.Error("Caps advertises break-in control on a radio that has none")
			}
			c.wantSent(t) // and nothing went to the radio
		})
	}
}

// The VX style is the dangerous one: outside CW the very same command is VOX,
// so a break-in request there would switch voice VOX on behind the operator.
func TestSetBreakInRefusedOutsideCWOnVXModels(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	k.mode.Store(uint32(radio.ModeUSB))
	c := newTestConn(t, k, answersFor(k.profile))
	if err := k.SetBreakIn(context.Background(), c, radio.BreakInSemi); err == nil {
		t.Fatal("SetBreakIn wrote VX in USB, which sets VOX rather than break-in")
	}
	c.wantSent(t) // nothing at all
}

func TestBreakInDecode(t *testing.T) {
	tests := []struct {
		name  string
		model string
		mode  radio.Mode
		// frames are decoded in order; the last one's patch is checked.
		frames []string
		want   radio.BreakIn
		// wantPatch is whether the final frame should publish a value.
		wantPatch bool
	}{
		{"VX1 with a delay is semi", "ts590sg", radio.ModeCW,
			[]string{"SD0300", "VX1"}, radio.BreakInSemi, true},
		{"VX1 with no delay is full", "ts590sg", radio.ModeCW,
			[]string{"SD0000", "VX1"}, radio.BreakInFull, true},
		{"VX0 is off", "ts590sg", radio.ModeCW,
			[]string{"SD0300", "VX0"}, radio.BreakInOff, true},
		// A VX frame in USB is the VOX setting. Publishing it as break-in would
		// make the CW guard trust a number about something else entirely.
		{"VX in USB is not break-in", "ts590sg", radio.ModeUSB,
			[]string{"VX1"}, radio.BreakInUnknown, false},
		{"BI2 is full without any delay read", "ts990s", radio.ModeCW,
			[]string{"BI2"}, radio.BreakInFull, true},
		{"BI1 is semi on the three-value style", "ts990s", radio.ModeCW,
			[]string{"BI1"}, radio.BreakInSemi, true},
		// The two-value style needs the delay, and BI1 alone with SD unread
		// leaves the delay at its zero value, which reads as full.
		{"BI1 with a delay is semi", "ts890s", radio.ModeCW,
			[]string{"SD0300", "BI1"}, radio.BreakInSemi, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newModelRig(t, tt.model)
			k.mode.Store(uint32(tt.mode))
			var last backend.Update
			for _, f := range tt.frames {
				u, err := k.Decode([]byte(f))
				if err != nil {
					t.Fatalf("Decode(%q): %v", f, err)
				}
				last = u
			}
			if got := k.BreakIn(); got != tt.want {
				t.Errorf("BreakIn() = %q, want %q", got, tt.want)
			}
			if got := last.Patch.BreakIn != nil; got != tt.wantPatch {
				t.Errorf("patch published = %v, want %v", got, tt.wantPatch)
			}
			if tt.wantPatch && *last.Patch.BreakIn != tt.want {
				t.Errorf("patch = %q, want %q", *last.Patch.BreakIn, tt.want)
			}
		})
	}
}

// A frame still has to complete its transaction even when it says nothing
// publishable, or the poll that asked for it sits out the full timeout.
func TestBreakInFrameAlwaysCompletesItsTransaction(t *testing.T) {
	k := newModelRig(t, "ts590sg")
	k.mode.Store(uint32(radio.ModeUSB))
	u, err := k.Decode([]byte("VX1"))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if u.Key != keyVX {
		t.Errorf("key = %q, want %q so the pending VX; read is answered", u.Key, keyVX)
	}
}
