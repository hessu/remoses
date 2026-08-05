package civ

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/cw"
)

func TestCharsetIsTheDocumentedSet(t *testing.T) {
	// Exactly what the reference tabulates for command 17, plus the '^' marker
	// that canonical prosign syntax needs.
	want := "0123456789" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ" +
		"abcdefghijklmnopqrstuvwxyz" +
		"'()/=?+.\"-@,: " + string(prosignMarker)
	for _, c := range want {
		if !strings.ContainsRune(Charset, c) {
			t.Errorf("charset omits %q", c)
		}
	}
	if len(Charset) != len(want) {
		t.Errorf("charset has %d characters, the documented set has %d", len(Charset), len(want))
	}
	for _, c := range Charset {
		if !strings.ContainsRune(want, c) {
			t.Errorf("charset contains %q, which the rig does not key", c)
		}
	}
}

func TestEncodeProsigns(t *testing.T) {
	r := testRig(t)
	// Icom's own encoding already uses '^', so valid text passes through
	// unchanged; the value of this method is the validation.
	pass := []string{
		"",
		"CQ CQ DE OH2XYZ K",
		"cq test",
		"^AR",
		"CQ TEST DE OH2XYZ ^AR",
		"^SK ^BT ^KN",
		"599 = 5NN/OH2XYZ?",
		"@ , : . - \" ' ( ) + / =",
		"^ARK", // a run may be longer than two letters
	}
	for _, in := range pass {
		t.Run(in, func(t *testing.T) {
			got, err := r.EncodeProsigns(in)
			if err != nil {
				t.Fatalf("EncodeProsigns(%q): %v", in, err)
			}
			if got != in {
				t.Errorf("EncodeProsigns(%q) = %q, want it unchanged", in, got)
			}
		})
	}
}

func TestEncodeProsignsRejectsBadMarkers(t *testing.T) {
	r := testRig(t)
	bad := []string{
		"^",
		"CQ ^",
		"^1",
		"^ AR",
		"^^AR",
		"DE OH2XYZ ^.",
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			if _, err := r.EncodeProsigns(in); err == nil {
				t.Errorf("EncodeProsigns(%q) accepted a caret that starts no prosign", in)
			}
		})
	}
}

func TestEncodeProsignsRejectsBadCharacters(t *testing.T) {
	r := testRig(t)
	tests := []struct {
		name   string
		in     string
		char   rune
		offset int
	}{
		{"semicolon", "CQ; DE", ';', 2},
		{"underscore is a Kenwood prosign, not an Icom one", "CQ _", '_', 3},
		{"bracket", "[BT]", '[', 0},
		{"percent", "50%", '%', 2},
		{"asterisk", "A*B", '*', 1},
		{"hash", "#", '#', 0},
		{"newline", "CQ\n", '\n', 2},
		{"tab", "\tCQ", '\t', 0},
		{"non-ascii", "OH2XYZ ä", 'ä', 7},
		{"dollar", "$5", '$', 0},
		{"less than", "<AS>", '<', 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.EncodeProsigns(tc.in)
			if err == nil {
				t.Fatalf("EncodeProsigns(%q) accepted a character the rig would mangle", tc.in)
			}
			var ce *cw.CharError
			if !errors.As(err, &ce) {
				t.Fatalf("error %v is a %T, not a *cw.CharError the API can turn into a 422", err, err)
			}
			if ce.Char != tc.char {
				t.Errorf("reported character %q, want %q", ce.Char, tc.char)
			}
			if ce.Offset != tc.offset {
				t.Errorf("reported offset %d, want %d", ce.Offset, tc.offset)
			}
			if ce.Charset != Charset {
				t.Error("the error does not carry the accepted charset")
			}
		})
	}
}

func TestMaxChunkAndCharsetAccessors(t *testing.T) {
	r := testRig(t)
	if r.MaxChunk() != 30 {
		t.Errorf("MaxChunk = %d, want 30", r.MaxChunk())
	}
	if r.Charset() != Charset {
		t.Error("Charset does not return the documented set")
	}
}

func TestBufferFreeIsUnavailable(t *testing.T) {
	s := newSim(t)
	free, ok, err := s.backend.BufferFree(context.Background(), s)
	if err != nil {
		t.Fatalf("BufferFree: %v", err)
	}
	if ok {
		t.Error("BufferFree claims the IC-7610 can be asked how full its CW buffer is")
	}
	if free != 0 {
		t.Errorf("BufferFree returned %d with ok false; a number here would be invented", free)
	}
	if len(s.log) != 0 {
		t.Error("BufferFree put a frame on the wire")
	}
}

func TestSendChunk(t *testing.T) {
	s := newSim(t)
	const text = "CQ TEST DE OH2XYZ ^AR"
	if err := s.backend.SendChunk(context.Background(), s, text); err != nil {
		t.Fatalf("SendChunk: %v", err)
	}
	if len(s.cwMessages) != 1 || s.cwMessages[0] != text {
		t.Fatalf("rig received %q, want %q", s.cwMessages, text)
	}
	want := append([]byte{0xFE, 0xFE, 0x98, 0xE0, 0x17}, text...)
	want = append(want, 0xFD)
	if !bytes.Equal(s.log[0], want) {
		t.Errorf("wire frame = % X, want % X", s.log[0], want)
	}
}

func TestSendChunkAtTheLimit(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SendChunk(context.Background(), s, strings.Repeat("A", 30)); err != nil {
		t.Fatalf("SendChunk of 30 characters: %v", err)
	}
	if err := s.backend.SendChunk(context.Background(), s, strings.Repeat("A", 31)); err == nil {
		t.Error("SendChunk accepted 31 characters; the rig takes 30")
	}
	if err := s.backend.SendChunk(context.Background(), s, ""); err == nil {
		t.Error("SendChunk accepted empty text")
	}
	if len(s.cwMessages) != 1 {
		t.Errorf("rig received %d messages, want 1", len(s.cwMessages))
	}
}

func TestSendChunkValidatesText(t *testing.T) {
	s := newSim(t)
	if err := s.backend.SendChunk(context.Background(), s, "CQ; DE"); err == nil {
		t.Error("SendChunk sent a character the rig would silently mangle")
	}
	if err := s.backend.SendChunk(context.Background(), s, "CQ ^"); err == nil {
		t.Error("SendChunk sent a dangling prosign marker")
	}
	if len(s.log) != 0 {
		t.Errorf("rejected text still put %d frames on the wire", len(s.log))
	}
}

func TestAbort(t *testing.T) {
	s := newSim(t)
	if err := s.backend.Abort(context.Background(), s); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if s.cwAborts != 1 {
		t.Errorf("rig saw %d aborts, want 1", s.cwAborts)
	}
	want := []byte{0xFE, 0xFE, 0x98, 0xE0, 0x17, 0xFF, 0xFD}
	if !bytes.Equal(s.log[0], want) {
		t.Errorf("wire frame = % X, want % X", s.log[0], want)
	}
}

func TestSetSpeed(t *testing.T) {
	tests := []struct {
		wpm  int
		want [2]byte
	}{
		{6, [2]byte{0x00, 0x00}},   // the documented bottom of the scale
		{48, [2]byte{0x02, 0x55}},  // and the top
		{27, [2]byte{0x01, 0x28}},  // mid-scale under the linear assumption
		{1, [2]byte{0x00, 0x00}},   // clamped up
		{100, [2]byte{0x02, 0x55}}, // clamped down
	}
	for _, tc := range tests {
		s := newSim(t)
		if err := s.backend.SetSpeed(context.Background(), s, tc.wpm); err != nil {
			t.Fatalf("SetSpeed(%d): %v", tc.wpm, err)
		}
		if s.speed != tc.want {
			t.Errorf("SetSpeed(%d) set % X, want % X", tc.wpm, s.speed, tc.want)
		}
		s.wantConversation(t, "14/0C")
	}
}
