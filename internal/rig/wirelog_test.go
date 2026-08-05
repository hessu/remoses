package rig

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

func TestWireHexIsAlwaysRendered(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		want  string
	}{
		{
			// The frame a CI-V manual prints, byte for byte, so a log line can
			// be compared against the page without transcribing anything.
			name:  "civ read frequency",
			frame: []byte{0xFE, 0xFE, 0x98, 0xE0, 0x03, 0xFD},
			want:  "FE FE 98 E0 03 FD",
		},
		{
			name:  "kenwood frequency answer",
			frame: []byte("FA00014025000;"),
			want:  "46 41 30 30 30 31 34 30 32 35 30 30 30 3B",
		},
		{
			name:  "empty",
			frame: nil,
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wireHex(tt.frame); got != tt.want {
				t.Errorf("wireHex = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWireTextRendersAsciiAndEscapesTheRest(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		want  string
		wants bool
	}{
		{
			name:  "kenwood frame is printable",
			frame: []byte("FA00014025000;"),
			want:  "FA00014025000;",
			wants: true,
		},
		{
			name:  "kenwood cw buffer keeps its padding",
			frame: []byte("KY HELLO                   ;"),
			want:  "KY HELLO                   ;",
			wants: true,
		},
		{
			// A binary CI-V frame gets hex only: rendering it as text would be
			// a row of escapes hiding the one field being looked at.
			name:  "civ frame is not text",
			frame: []byte{0xFE, 0xFE, 0xE0, 0x98, 0x03, 0x00, 0x50, 0x02, 0x14, 0x00, 0xFD},
			wants: false,
		},
		{
			// A mostly-ASCII frame keeps its rendering, and the stray byte is
			// escaped rather than emitted: a bare CR would break the log line.
			name:  "control byte in an ascii frame is escaped",
			frame: []byte("FA0001402\x0d000;"),
			want:  `FA0001402\r000;`,
			wants: true,
		},
		{
			name:  "nul is escaped as hex",
			frame: []byte("AB\x00CDEF;"),
			want:  `AB\x00CDEF;`,
			wants: true,
		},
		{
			name:  "escape character is escaped",
			frame: []byte("AB\x1bCDEF;"),
			want:  `AB\x1BCDEF;`,
			wants: true,
		},
		{
			// The rigctld backend frames a whole response block, newlines and
			// all, and \n reads better than \x0A in a multi-line one.
			name:  "rigctld block keeps its line structure readable",
			frame: []byte("get_freq:\nFrequency: 14074000\nRPRT 0\n"),
			want:  `get_freq:\nFrequency: 14074000\nRPRT 0\n`,
			wants: true,
		},
		{
			name:  "backslash is doubled so an escape is unambiguous",
			frame: []byte(`KY BK\;`),
			want:  `KY BK\\;`,
			wants: true,
		},
		{
			name:  "empty",
			frame: nil,
			wants: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := wireText(tt.frame)
			if ok != tt.wants {
				t.Fatalf("wireText ok = %v, want %v (got %q)", ok, tt.wants, got)
			}
			if ok && got != tt.want {
				t.Errorf("wireText = %q, want %q", got, tt.want)
			}
			if !ok && got != "" {
				t.Errorf("wireText returned %q with ok=false", got)
			}
		})
	}
}

func TestWireKeyNamesUnsolicitedFrames(t *testing.T) {
	if got := wireKey(backend.KeyUnsolicited); got != "unsolicited" {
		t.Errorf("wireKey(unsolicited) = %q", got)
	}
	if got := wireKey("1A/03"); got != "1A/03" {
		t.Errorf("wireKey = %q", got)
	}
}

// The point of the whole feature is that it costs nothing when off, because the
// reader goroutine is in the CW timing path. The handler below fails the test
// the moment a wire record reaches it, and since the only place a wire line is
// formatted is behind the same guard that decides whether to emit one, a silent
// handler is evidence that no formatting ran either.
func TestWireLogOffDoesNoWork(t *testing.T) {
	capture := newCaptureHandler(t)
	capture.forbidWire = true

	h := startedHarness(t, func(rc *config.Radio) { rc.DebugWire = false },
		WithLogger(slog.New(capture)))

	if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 14200000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}
	h.dl.lastPort().push("UF00007123456")
	waitFor(t, "unsolicited frequency", func() bool { return h.s.State().Frequency == 7123456 })

	if n := len(capture.wireLines()); n != 0 {
		t.Fatalf("%d wire lines logged with debug_wire off", n)
	}
}

func TestWireLogRecordsBothDirections(t *testing.T) {
	h, capture := wireHarness(t)

	if _, err := h.s.SetFrequency(context.Background(), radio.VFOCurrent, 14200000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}

	// The request as it went out.
	req := findWireLine(t, capture, func(r logRecord) bool {
		return r.attrs["dir"] == wireToRig && r.attrs["text"] == "FA00014200000;"
	}, "the frequency set command")
	if got := req.attrs["radio"]; got != "testrig" {
		t.Errorf("to-rig line radio = %q, want testrig: the line is not self-contained", got)
	}
	const wantHex = "46 41 30 30 30 31 34 32 30 30 30 30 30 3B"
	if got := req.attrs["hex"]; got != wantHex {
		t.Errorf("to-rig hex = %q, want %q", got, wantHex)
	}
	if got := req.attrs["len"]; got != "14" {
		t.Errorf("to-rig len = %q, want 14", got)
	}

	// And the rig's answer, carrying the key remoses correlated it with — the
	// layer where a wrong assumption about a rig would otherwise be invisible.
	// The fake backend's Split drops the terminator, as a real one may, so the
	// inbound frame is one byte shorter than the outbound.
	reply := findWireLine(t, capture, func(r logRecord) bool {
		return r.attrs["dir"] == wireFromRig && r.attrs["text"] == "FA00014200000"
	}, "the frequency answer")
	if got := reply.attrs["key"]; got != "FA" {
		t.Errorf("from-rig key = %q, want FA", got)
	}
	if got := reply.attrs["ok"]; got != "true" {
		t.Errorf("from-rig ok = %q, want true", got)
	}
	if got := reply.attrs["radio"]; got != "testrig" {
		t.Errorf("from-rig line radio = %q, want testrig", got)
	}
}

// An Icom Transceive broadcast or a Kenwood/Yaesu AI push answers no request. It
// arrives on the same path as a reply, and it is the traffic a trace is most
// often opened for, so it has to be logged and it has to be recognisable.
func TestWireLogRecordsUnsolicitedFrames(t *testing.T) {
	h, capture := wireHarness(t)

	h.dl.lastPort().push("UF00007123456")
	waitFor(t, "unsolicited frequency", func() bool { return h.s.State().Frequency == 7123456 })

	line := findWireLine(t, capture, func(r logRecord) bool {
		return r.attrs["text"] == "UF00007123456"
	}, "the unsolicited frequency report")
	if got := line.attrs["dir"]; got != wireFromRig {
		t.Errorf("unsolicited dir = %q, want %s", got, wireFromRig)
	}
	if got := line.attrs["key"]; got != "unsolicited" {
		t.Errorf("unsolicited key = %q, want unsolicited", got)
	}
}

// A rig emitting a control byte must not put it into the log, and must still be
// readable: this is what a trace of a rig powering up looks like.
func TestWireLogEscapesControlBytesFromTheRig(t *testing.T) {
	h, capture := wireHarness(t)

	h.dl.lastPort().push("AB\x01CDEF")
	findWireLine(t, capture, func(r logRecord) bool {
		return r.attrs["text"] == `AB\x01CDEF`
	}, "the frame carrying a control byte")

	for _, line := range capture.wireLines() {
		if strings.ContainsRune(line.attrs["text"], 0x01) {
			t.Fatalf("a raw control byte reached the log: %q", line.attrs["text"])
		}
	}
}

// wireHarness starts a session with the wire trace on and a handler that keeps
// every record for inspection.
func wireHarness(t *testing.T) (*harness, *captureHandler) {
	t.Helper()
	capture := newCaptureHandler(t)
	h := startedHarness(t, func(rc *config.Radio) { rc.DebugWire = true },
		WithLogger(slog.New(capture)))
	return h, capture
}

// findWireLine returns the first captured wire line matching want, waiting for
// it to arrive: the reader logs on its own goroutine.
func findWireLine(t *testing.T, capture *captureHandler, want func(logRecord) bool, what string) logRecord {
	t.Helper()
	var found logRecord
	waitFor(t, what, func() bool {
		for _, line := range capture.wireLines() {
			if want(line) {
				found = line
				return true
			}
		}
		return false
	})
	return found
}

// logRecord is one captured slog record, flattened to what the assertions need.
type logRecord struct {
	msg   string
	attrs map[string]string
}

// captureHandler keeps records so the tests can assert on attributes.
//
// It is a slog.Handler rather than a buffer of formatted output because the
// assertions are about what was logged, and parsing a TextHandler's lines back
// apart would test the formatter instead. It also has to carry WithAttrs
// through, since that is how the session stamps the radio id onto every line.
type captureHandler struct {
	t *testing.T
	// forbidWire fails the test if a wire line is handled at all.
	forbidWire bool

	mu    *sync.Mutex
	recs  *[]logRecord
	attrs []slog.Attr
}

func newCaptureHandler(t *testing.T) *captureHandler {
	return &captureHandler{t: t, mu: new(sync.Mutex), recs: new([]logRecord)}
}

// Enabled admits everything, so that a line the code should not have produced
// is caught rather than filtered out before it is seen.
func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message == wireMsg && h.forbidWire {
		// Errorf, not Fatalf: this runs on the session's goroutines.
		h.t.Errorf("a CAT wire line was formatted and logged with debug_wire off")
	}
	rec := logRecord{msg: r.Message, attrs: make(map[string]string, r.NumAttrs()+len(h.attrs))}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})

	h.mu.Lock()
	*h.recs = append(*h.recs, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	n := *h
	n.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &n
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

// wireLines returns the CAT wire lines captured so far, in arrival order.
func (h *captureHandler) wireLines() []logRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []logRecord
	for _, r := range *h.recs {
		if r.msg == wireMsg {
			out = append(out, r)
		}
	}
	return out
}
