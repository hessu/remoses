package civ

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/rig/backend"
)

// The session finds this interface by type assertion, so a signature that drifts
// from it does not fail to compile — it simply stops being found, and every
// antenna request answers "this radio cannot" with nothing anywhere saying why.
var _ backend.AntennaSelector = (*Rig)(nil)

// antennaSim points a simulator at one model and gives its command 12 the shape
// that model's own table prints: two operands where there is a receive-antenna
// flag, one where there is not.
//
// The width is the sim's business rather than the backend's on purpose. A set of
// the wrong width draws an NG there, so a backend that sent a trailing flag byte
// to an IC-9100 fails here instead of on somebody's radio.
func antennaSim(t *testing.T, model string) (*Rig, *simRig) {
	t.Helper()
	r, s := modelRig(t, model)
	s.antennaBytes = 1
	if r.model.RXAntenna {
		s.antennaBytes = 2
	}
	return r, s
}

// TestAntennaIsPerModel pins which radios have command 12 and in which of its
// three shapes, because "no Icom has an antenna selector" is what this backend
// asserted until the references were opened.
func TestAntennaIsPerModel(t *testing.T) {
	for _, tt := range []struct {
		model     string
		sockets   int
		rx        bool
		rxSockets int
	}{
		// The selectors. Two sockets each on the IC-7610 and IC-7600 (Ref. Guide
		// p. 3; printed p. 160), four on the IC-7760 (Ref. Guide p. 3).
		{"ic-7610", 2, true, 2},
		{"ic-7600", 2, true, 2},
		{"ic-7760", 4, true, 4},
		// Four sockets with ANT4's flag fixed at OFF: "00 ... fix" against that
		// row alone (IC-7700 printed p. 14-3, IC-7850 printed p. 18-3).
		{"ic-7700", 4, true, 3},
		{"ic-7850", 4, true, 3},
		// Two sockets and an empty data column: a bare selector with no receive
		// antenna behind it (printed p. 184).
		{"ic-9100", 2, false, 0},
		// And the mirror image: a receive antenna with nothing to select
		// between. "12 00 Sets or reads the ON/OFF status of a receiving
		// antenna" is its whole command 12 (Ref. Guide p. 4).
		{"ic-7300mk2", 0, true, 0},
		// No command 12 at all: these tables step 11 straight to 13.
		{"ic-9700", 0, false, 0},
		{"ic-905", 0, false, 0},
		{"ic-7300", 0, false, 0},
		{"ic-718", 0, false, 0},
		{"ic-703", 0, false, 0},
		{"ic-910h", 0, false, 0},
		{"ic-706mkiig", 0, false, 0},
		{"generic", 0, false, 0},
	} {
		r, _ := modelRig(t, tt.model)
		c := r.Caps()
		if c.Antennas != tt.sockets {
			t.Errorf("%s: caps.antennas = %d, want %d", tt.model, c.Antennas, tt.sockets)
		}
		if c.RXAntennaControl != tt.rx {
			t.Errorf("%s: caps.rx_antenna_control = %v, want %v",
				tt.model, c.RXAntennaControl, tt.rx)
		}
		if got := r.Model().RXAntennaSockets; got != tt.rxSockets {
			t.Errorf("%s: %d sockets accept RX ANT ON, want %d", tt.model, got, tt.rxSockets)
		}
	}
}

// TestSetAntennaUsesTheSocketByteNotTheDataByte is the trap this command sets,
// and the reason the capability was deferred rather than guessed at.
//
// The IC-7610 prints "12 00 00 or 01 Select/read ANT1 selection (00=RX ANT OFF,
// 01=RX ANT ON)". The socket is the FIRST operand and the data byte is that
// socket's receive-antenna flag — so a SetAntenna that wrote n into the data
// byte would leave the radio on ANT1 and switch its receive antenna instead,
// which the radio accepts without complaint.
func TestSetAntennaUsesTheSocketByteNotTheDataByte(t *testing.T) {
	r, s := antennaSim(t, "ic-7610")
	if err := r.SetAntenna(context.Background(), s, 2); err != nil {
		t.Fatalf("SetAntenna(2): %v", err)
	}
	// ANT2 is socket byte 01, counting from zero as the wire does.
	if s.antenna != 0x01 {
		t.Errorf("SetAntenna(2) left the socket byte at %#02x, want 01", s.antenna)
	}
	// The last frame must be 12 <socket> <flag> and nothing longer or shorter.
	last := s.log[len(s.log)-1]
	if len(last) != 8 || last[4] != cmdAntenna {
		t.Fatalf("SetAntenna sent % X, want a three-byte command 12", last)
	}
}

// TestSetAntennaCarriesTheReceiveFlagAcross: both fields ride in one frame and
// there is no encoding for "leave the other alone", so selecting a socket has to
// preserve the receive-antenna setting or it would switch it out as a side
// effect nobody asked for.
func TestSetAntennaCarriesTheReceiveFlagAcross(t *testing.T) {
	r, s := antennaSim(t, "ic-7610")
	s.antenna, s.rxAntenna = 0x00, 0x01 // ANT1 with the receive antenna in

	if err := r.SetAntenna(context.Background(), s, 2); err != nil {
		t.Fatalf("SetAntenna(2): %v", err)
	}
	if s.antenna != 0x01 {
		t.Errorf("socket = %#02x, want 01 (ANT2)", s.antenna)
	}
	if s.rxAntenna != 0x01 {
		t.Error("selecting ANT2 switched the receive antenna out; the flag shares " +
			"the frame and has to be read and carried across")
	}
	// And the read that carried it across really happened, rather than the
	// backend trusting a cached value that a slow poll could have left stale.
	if got := s.requests(); len(got) != 2 || got[0] != "12" || got[1] != "12" {
		t.Errorf("conversation was %v, want a bare 12 read then a 12 write", got)
	}
}

// TestSetRXAntennaKeepsTheSocket is the same argument in the other direction.
func TestSetRXAntennaKeepsTheSocket(t *testing.T) {
	r, s := antennaSim(t, "ic-7610")
	s.antenna, s.rxAntenna = 0x01, 0x01 // ANT2 with the receive antenna in

	if err := r.SetRXAntenna(context.Background(), s, false); err != nil {
		t.Fatalf("SetRXAntenna(false): %v", err)
	}
	if s.rxAntenna != 0x00 {
		t.Errorf("receive antenna flag = %#02x, want 00", s.rxAntenna)
	}
	if s.antenna != 0x01 {
		t.Error("switching the receive antenna moved the radio back to ANT1; the " +
			"socket shares the frame and has to be carried across")
	}
}

// TestAntennaDecodesBothFields: a value written and never read back reports
// whatever remoses last wrote for ever, and on these radios the antenna can move
// with no command sent at all — 1A 05 02 89 puts the [ANT] switch in Auto, where
// the radio picks per band from its own memories.
func TestAntennaDecodesBothFields(t *testing.T) {
	r, _ := antennaSim(t, "ic-7610")
	for _, tt := range []struct {
		socket, flag byte
		wantAnt      int
		wantRX       bool
	}{
		{0x00, 0x00, 1, false},
		{0x00, 0x01, 1, true},
		{0x01, 0x00, 2, false},
		{0x01, 0x01, 2, true},
	} {
		u, err := r.Decode(fromRig(cmdAntenna, tt.socket, tt.flag))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if u.Key != KeyAntenna {
			t.Errorf("a command 12 answer keyed %q, want %q", u.Key, KeyAntenna)
		}
		if u.Patch.Antenna == nil || *u.Patch.Antenna != tt.wantAnt {
			t.Errorf("% X decoded antenna %v, want %d", []byte{tt.socket, tt.flag},
				u.Patch.Antenna, tt.wantAnt)
		}
		if u.Patch.RXAntenna == nil || *u.Patch.RXAntenna != tt.wantRX {
			t.Errorf("% X decoded rx antenna %v, want %v", []byte{tt.socket, tt.flag},
				u.Patch.RXAntenna, tt.wantRX)
		}
	}

	// A socket past the end of this radio's two is a frame misread rather than
	// an antenna to publish — but it must still resolve the pending read, or the
	// poll fails and the failures eventually tear down a healthy link.
	u, err := r.Decode(fromRig(cmdAntenna, 0x03, 0x00))
	if err != nil {
		t.Fatal(err)
	}
	if u.Key != KeyAntenna {
		t.Errorf("an out-of-range socket keyed %q, want %q", u.Key, KeyAntenna)
	}
	if u.Patch.Antenna != nil {
		t.Errorf("published antenna %d from a socket this radio has not got", *u.Patch.Antenna)
	}
}

// TestAntennaReadIsBare is a safety property rather than a formatting one.
//
// The obvious way to read "what is ANT1's flag" would be to send 12 00 and wait
// for the answer. On an IC-9100 that is not a read at all: 12 00 is a complete
// set frame and means "select ANT1". A poll must never be one byte away from
// moving somebody's antenna, so the read carries no operands on any model.
func TestAntennaReadIsBare(t *testing.T) {
	for _, model := range []string{"ic-7610", "ic-9100", "ic-7300mk2"} {
		r, s := antennaSim(t, model)
		_ = r.Poll(context.Background(), s, backend.PollSlow)

		var seen bool
		for _, f := range s.log {
			if len(f) < 5 || f[4] != cmdAntenna {
				continue
			}
			seen = true
			// FE FE to from 12 FD, with no operand between the command and the
			// end of message.
			if len(f) != 6 {
				t.Errorf("%s: the slow poll sent % X, which is a command 12 with "+
					"operands — that is a SET on the radios whose socket is the "+
					"first byte", model, f)
			}
		}
		if !seen {
			t.Errorf("%s: the slow tier never asked for the antenna, so state.antenna "+
				"would only ever hold what remoses last wrote", model)
		}
	}
}

// TestIC9100AntennaHasNoFlagByte covers the shortest of the three shapes. Its
// data column is empty, so a trailing flag byte would be a parameter its parser
// is not expecting — and there is no receive antenna to ask about.
func TestIC9100AntennaHasNoFlagByte(t *testing.T) {
	r, s := antennaSim(t, "ic-9100")
	if err := r.SetAntenna(context.Background(), s, 2); err != nil {
		t.Fatalf("SetAntenna(2): %v", err)
	}
	last := s.log[len(s.log)-1]
	if len(last) != 7 || last[4] != cmdAntenna || last[5] != 0x01 {
		t.Errorf("SetAntenna(2) sent % X, want command 12 with the single byte 01", last)
	}
	// No read first, because there is no second field to preserve.
	if got := s.requests(); len(got) != 1 {
		t.Errorf("conversation was %v, want the write alone", got)
	}

	err := r.SetRXAntenna(context.Background(), s, true)
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("SetRXAntenna = %v, want ErrUnsupported on a radio whose command 12 "+
			"has no flag byte", err)
	}

	// And a decode publishes the socket and no flag, from a one-byte answer.
	u, err := r.Decode(fromRig(cmdAntenna, 0x01))
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.Antenna == nil || *u.Patch.Antenna != 2 {
		t.Errorf("antenna = %v, want 2", u.Patch.Antenna)
	}
	if u.Patch.RXAntenna != nil {
		t.Errorf("published a receive antenna %v on a radio that reports none", *u.Patch.RXAntenna)
	}
}

// TestIC7300MK2HasTheFlagAndNoSelector is the other half of that: the same
// command, the same first byte, and the opposite meaning.
func TestIC7300MK2HasTheFlagAndNoSelector(t *testing.T) {
	r, s := antennaSim(t, "ic-7300mk2")

	err := r.SetAntenna(context.Background(), s, 1)
	if !errors.Is(err, backend.ErrUnsupported) {
		t.Errorf("SetAntenna = %v, want ErrUnsupported: this radio's 12 00 is a "+
			"receive antenna, not socket 1", err)
	}
	if err := r.SetRXAntenna(context.Background(), s, true); err != nil {
		t.Fatalf("SetRXAntenna: %v", err)
	}
	// 12 00 01, with the fixed 00 its single row prints and no socket read
	// beforehand — there is nothing to select and nothing to preserve.
	last := s.log[len(s.log)-1]
	if len(last) != 8 || last[4] != cmdAntenna || last[5] != 0x00 || last[6] != 0x01 {
		t.Errorf("SetRXAntenna(true) sent % X, want 12 00 01", last)
	}
	if got := s.requests(); len(got) != 1 {
		t.Errorf("conversation was %v, want the write alone", got)
	}

	// The socket byte is a sub-command here, so nothing may publish it as an
	// antenna selection.
	u, err := r.Decode(fromRig(cmdAntenna, 0x00, 0x01))
	if err != nil {
		t.Fatal(err)
	}
	if u.Patch.Antenna != nil {
		t.Errorf("published antenna %d on a radio with no socket selection", *u.Patch.Antenna)
	}
	if u.Patch.RXAntenna == nil || !*u.Patch.RXAntenna {
		t.Errorf("rx antenna = %v, want true", u.Patch.RXAntenna)
	}
}

// TestFixedRXAntennaSocket covers the IC-7700's and IC-7850's ANT4, whose row
// prints a data column of "00" alone and the word "fix".
func TestFixedRXAntennaSocket(t *testing.T) {
	for _, model := range []string{"ic-7700", "ic-7850"} {
		t.Run(model, func(t *testing.T) {
			r, s := antennaSim(t, model)
			s.antenna, s.rxAntenna = 0x00, 0x01 // ANT1, receive antenna in

			// Selecting ANT4 succeeds and takes the flag off with it. Sending 01
			// there would draw an NG for a distinction the radio does not make,
			// and refusing the request outright would be refusing a perfectly
			// ordinary antenna change.
			if err := r.SetAntenna(context.Background(), s, 4); err != nil {
				t.Fatalf("SetAntenna(4): %v", err)
			}
			if s.antenna != 0x03 || s.rxAntenna != 0x00 {
				t.Errorf("ANT4 left the radio at socket %#02x flag %#02x, want 03 00",
					s.antenna, s.rxAntenna)
			}

			// And with ANT4 selected, switching the receive antenna on is refused
			// here rather than sent as a byte the table forbids.
			err := r.SetRXAntenna(context.Background(), s, true)
			if !errors.Is(err, backend.ErrUnsupported) {
				t.Fatalf("SetRXAntenna(true) on ANT4 = %v, want ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), "ANT4") {
				t.Errorf("refusal was %q, which does not name the socket in the way", err)
			}
			// Switching it OFF on ANT4 is not a refusal: 00 is the only value that
			// row takes, so the request is already satisfied by what goes out.
			if err := r.SetRXAntenna(context.Background(), s, false); err != nil {
				t.Errorf("SetRXAntenna(false) on ANT4: %v", err)
			}
		})
	}
}

// TestAntennaRefusesASocketTheRadioLacks: the counts really do differ, and a
// socket from one radio must not be sent to another.
func TestAntennaRefusesASocketTheRadioLacks(t *testing.T) {
	r, s := antennaSim(t, "ic-7610") // ANT1 and ANT2, and that is all
	for _, n := range []int{0, 3, 4} {
		if err := r.SetAntenna(context.Background(), s, n); !errors.Is(err, backend.ErrUnsupported) {
			t.Errorf("SetAntenna(%d) = %v, want ErrUnsupported on a two-socket radio", n, err)
		}
	}
	if len(s.log) != 0 {
		t.Errorf("a refused request still put %d frames on the wire", len(s.log))
	}
}

// TestAntennaCapsAndSettersAgree is the invariant that outlives any one model: a
// capability list promising what the next call rejects is a failure this project
// has already shipped once (DESIGN.md §5.5, Caps.VFOs).
func TestAntennaCapsAndSettersAgree(t *testing.T) {
	ctx := context.Background()
	for _, name := range ModelNames() {
		r, s := antennaSim(t, name)
		c := r.Caps()
		for _, tc := range []struct {
			what   string
			claims bool
			err    error
		}{
			{"antenna", c.Antennas > 0, r.SetAntenna(ctx, s, 1)},
			{"receive antenna", c.RXAntennaControl, r.SetRXAntenna(ctx, s, true)},
		} {
			switch {
			case tc.claims && tc.err != nil:
				t.Errorf("%s claims %s and then refused it: %v", name, tc.what, tc.err)
			case !tc.claims && tc.err == nil:
				t.Errorf("%s accepted an %s set it does not claim", name, tc.what)
			case !tc.claims && !errors.Is(tc.err, backend.ErrUnsupported):
				t.Errorf("%s refused %s with %v, which is not ErrUnsupported and would "+
					"be a 500", name, tc.what, tc.err)
			}
		}
	}
}

// TestAntennaIsNotPolledWhereAbsent: a radio whose table has no command 12 must
// never be asked, or every slow tick would carry a rejection for a field that
// can never arrive.
func TestAntennaIsNotPolledWhereAbsent(t *testing.T) {
	for _, name := range ModelNames() {
		r, s := antennaSim(t, name)
		_ = r.Poll(context.Background(), s, backend.PollSlow)

		var asked bool
		for _, f := range s.log {
			if len(f) >= 5 && f[4] == cmdAntenna {
				asked = true
			}
		}
		c := r.Caps()
		want := c.Antennas > 0 || c.RXAntennaControl
		if asked != want {
			t.Errorf("%s: the slow tier asked for the antenna %v, want %v", name, asked, want)
		}
	}
}
