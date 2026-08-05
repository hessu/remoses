package civ

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/hessu/remoses/internal/config"
)

// testRig is a backend addressed like a factory-default IC-7610.
func testRig(t *testing.T) *Rig {
	t.Helper()
	r, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// fromRig builds a frame as the rig would send it: to the controller, from the
// rig.
func fromRig(cmd byte, data ...byte) []byte {
	f := []byte{preamble, preamble, DefaultControllerAddress, DefaultRigAddress, cmd}
	f = append(f, data...)
	return append(f, eom)
}

// broadcast builds a transceive frame, which the rig addresses to the whole bus.
func broadcast(cmd byte, data ...byte) []byte {
	f := []byte{preamble, preamble, addrBroadcast, DefaultRigAddress, cmd}
	f = append(f, data...)
	return append(f, eom)
}

func TestFrameBuild(t *testing.T) {
	r := testRig(t)
	tests := []struct {
		name string
		got  []byte
		want []byte
	}{
		{"read frequency", r.frame(cmdReadFreq),
			[]byte{0xFE, 0xFE, 0x98, 0xE0, 0x03, 0xFD}},
		{"set frequency", r.frame(cmdSetFreq, 0x00, 0x50, 0x02, 0x14, 0x00),
			[]byte{0xFE, 0xFE, 0x98, 0xE0, 0x05, 0x00, 0x50, 0x02, 0x14, 0x00, 0xFD}},
		{"read s-meter", r.frame(cmdMeter, subSMeter),
			[]byte{0xFE, 0xFE, 0x98, 0xE0, 0x15, 0x02, 0xFD}},
		{"ptt on", r.frame(cmdTransceiver, subPTT, 0x01),
			[]byte{0xFE, 0xFE, 0x98, 0xE0, 0x1C, 0x00, 0x01, 0xFD}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.got, tc.want) {
				t.Errorf("frame = % X, want % X", tc.got, tc.want)
			}
		})
	}
}

func TestFrameUsesConfiguredAddresses(t *testing.T) {
	r := &Rig{rigAddr: 0x94, ctrlAddr: 0xE1}
	got := r.frame(cmdReadFreq)
	want := []byte{0xFE, 0xFE, 0x94, 0xE1, 0x03, 0xFD}
	if !bytes.Equal(got, want) {
		t.Errorf("frame = % X, want % X", got, want)
	}
}

// scanAll runs the whole of in through Split via a bufio.Scanner, which is how
// the session drives it.
func scanAll(t *testing.T, r *Rig, in []byte, chunked bool) [][]byte {
	t.Helper()
	var src *bufio.Scanner
	if chunked {
		// One byte per Read: every frame arrives split across buffer refills.
		src = bufio.NewScanner(iotest.OneByteReader(bytes.NewReader(in)))
	} else {
		src = bufio.NewScanner(bytes.NewReader(in))
	}
	src.Split(r.Split)
	var out [][]byte
	for src.Scan() {
		out = append(out, bytes.Clone(src.Bytes()))
	}
	if err := src.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}
	return out
}

func TestSplit(t *testing.T) {
	r := testRig(t)
	ack := fromRig(codeOK)
	freq := fromRig(cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00)

	tests := []struct {
		name string
		in   []byte
		want [][]byte
	}{
		{
			name: "single frame",
			in:   ack,
			want: [][]byte{ack},
		},
		{
			name: "two frames in one read",
			in:   concat(freq, ack),
			want: [][]byte{freq, ack},
		},
		{
			name: "leading garbage",
			in:   concat([]byte{0x00, 0x17, 0xAA, 0xFD}, freq),
			want: [][]byte{freq},
		},
		{
			name: "lone preamble byte in the garbage",
			in:   concat([]byte{0xFE, 0x11}, freq),
			want: [][]byte{freq},
		},
		{
			name: "extra preamble bytes",
			in:   concat([]byte{0xFE, 0xFE, 0xFE, 0xFE}, freq[2:]),
			want: [][]byte{freq},
		},
		{
			name: "power-on wake-up preamble",
			in:   concat(bytes.Repeat([]byte{0xFE}, 25), freq[2:], ack),
			want: [][]byte{freq, ack},
		},
		{
			name: "truncated frame then a good one",
			in:   concat([]byte{0xFE, 0xFE, 0xE0, 0x98, 0x03, 0x00}, freq),
			// A rig that stops mid-frame is recognised by the next preamble,
			// since FE cannot occur inside a frame.
			want: [][]byte{freq},
		},
		{
			name: "runt frame",
			in:   concat([]byte{0xFE, 0xFE, 0xFD}, ack),
			want: [][]byte{ack},
		},
		{
			name: "trailing partial frame is dropped at eof",
			in:   concat(ack, []byte{0xFE, 0xFE, 0xE0, 0x98, 0x03}),
			want: [][]byte{ack},
		},
		{
			name: "trailing lone preamble is dropped at eof",
			in:   concat(ack, []byte{0xFE}),
			want: [][]byte{ack},
		},
		{
			name: "noise only",
			in:   []byte{0x01, 0x02, 0x03},
			want: nil,
		},
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunked := range []bool{false, true} {
				got := scanAll(t, r, tc.in, chunked)
				if len(got) != len(tc.want) {
					t.Fatalf("chunked=%v: got %d frames %X, want %d %X", chunked, len(got), got, len(tc.want), tc.want)
				}
				for i := range got {
					if !bytes.Equal(got[i], tc.want[i]) {
						t.Errorf("chunked=%v: frame %d = % X, want % X", chunked, i, got[i], tc.want[i])
					}
				}
			}
		})
	}
}

// TestSplitPartial checks the SplitFunc contract directly: a frame that has not
// finished arriving must ask for more data rather than yielding a token.
func TestSplitPartial(t *testing.T) {
	r := testRig(t)
	frame := fromRig(cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00)
	for n := 1; n < len(frame); n++ {
		advance, token, err := r.Split(frame[:n], false)
		if err != nil {
			t.Fatalf("Split(%d bytes): %v", n, err)
		}
		if token != nil {
			t.Fatalf("Split(%d bytes) returned a token % X", n, token)
		}
		if advance != 0 {
			t.Errorf("Split(%d bytes) advanced %d, discarding part of a frame", n, advance)
		}
	}
	advance, token, err := r.Split(frame, false)
	if err != nil || advance != len(frame) || !bytes.Equal(token, frame) {
		t.Fatalf("Split(complete) = %d, % X, %v", advance, token, err)
	}
}

// TestSplitLongPreambleDoesNotAccumulate makes sure a stream of preamble bytes
// is consumed as it arrives instead of being buffered until a frame appears.
func TestSplitLongPreambleDoesNotAccumulate(t *testing.T) {
	r := testRig(t)
	in := bytes.Repeat([]byte{0xFE}, 100)
	advance, token, err := r.Split(in, false)
	if err != nil || token != nil {
		t.Fatalf("Split = %d, % X, %v", advance, token, err)
	}
	if advance != len(in)-2 {
		t.Errorf("Split advanced %d of %d preamble bytes; want all but the last two", advance, len(in))
	}
}

// TestSplitUnterminatedResyncs covers a line that is stuck mid-frame: rather
// than buffering without limit, Split gives up on the preamble.
func TestSplitUnterminatedResyncs(t *testing.T) {
	r := testRig(t)
	in := append([]byte{0xFE, 0xFE, 0xE0, 0x98, 0x03}, bytes.Repeat([]byte{0x01}, maxFrameLen)...)
	advance, token, err := r.Split(in, false)
	if err != nil || token != nil {
		t.Fatalf("Split = %d, % X, %v", advance, token, err)
	}
	if advance == 0 {
		t.Error("Split did not resynchronise past an over-long frame")
	}
}

func TestEchoAndAddressing(t *testing.T) {
	r := testRig(t)
	ours := r.frame(cmdReadFreq)
	tests := []struct {
		name            string
		frame           []byte
		echo, addressed bool
	}{
		{"our own frame", ours, true, false},
		{"reply from rig", fromRig(cmdReadFreq, 0, 0, 0, 0, 0), false, true},
		{"transceive broadcast", broadcast(cmdXcvFreq, 0, 0, 0, 0, 0), false, true},
		{"another controller", []byte{0xFE, 0xFE, 0x98, 0xE1, 0x03, 0xFD}, false, false},
		{"another rig", []byte{0xFE, 0xFE, 0xE0, 0x94, 0x03, 0xFD}, false, false},
		{"malformed", []byte{0xFE, 0xFE, 0xFD}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.isEcho(tc.frame); got != tc.echo {
				t.Errorf("isEcho(% X) = %v, want %v", tc.frame, got, tc.echo)
			}
			if got := r.addressedToUs(tc.frame); got != tc.addressed {
				t.Errorf("addressedToUs(% X) = %v, want %v", tc.frame, got, tc.addressed)
			}
		})
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestSplitOnRealisticStream feeds an echoing bus: our own frame, then the
// rig's answer, repeatedly, one byte per read.
func TestSplitOnRealisticStream(t *testing.T) {
	r := testRig(t)
	var stream []byte
	var want [][]byte
	for i := 0; i < 4; i++ {
		ours := r.frame(cmdReadFreq)
		reply := fromRig(cmdReadFreq, 0x00, 0x50, 0x02, 0x14, 0x00)
		stream = concat(stream, ours, reply)
		want = append(want, ours, reply)
	}
	got := scanAll(t, r, stream, true)
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range got {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d = % X, want % X", i, got[i], want[i])
		}
	}
}

func TestNewAddressValidation(t *testing.T) {
	if _, err := New(nil); err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	bad := []struct {
		name string
		civ  config.CIV
	}{
		{"rig address reserved", config.CIV{RigAddress: 0xFE}},
		{"controller address reserved", config.CIV{ControllerAddress: 0xFF}},
		{"negative address", config.CIV{RigAddress: -1}},
		{"identical addresses", config.CIV{RigAddress: 0xE0, ControllerAddress: 0xE0}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(&config.Radio{CIV: &tc.civ}); err == nil {
				t.Error("New accepted an invalid address configuration")
			} else if !strings.Contains(err.Error(), "civ:") {
				t.Errorf("error %q is not attributed to the backend", err)
			}
		})
	}
}
