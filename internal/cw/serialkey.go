package cw

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/morse"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/transport"
)

// spinWindow is how long before an edge the keyer stops sleeping and starts
// checking the clock.
//
// Go has used high-resolution waitable timers on Windows since 1.16 and the
// other platforms were never the problem, so this is belt and braces. But a
// late edge is audible where a late HTTP response is not, and at 20 wpm this
// costs about one millisecond of spinning per 60 — a couple of percent of one
// core while transmitting, and nothing at all while idle.
const spinWindow = time.Millisecond

// line names a modem control output.
type line uint8

const (
	lineNone line = iota
	lineDTR
	lineRTS
)

func parseLine(name string) (line, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "none":
		return lineNone, nil
	case "dtr":
		return lineDTR, nil
	case "rts":
		return lineRTS, nil
	}
	return lineNone, fmt.Errorf("cw: %q is not a control line, use dtr or rts", name)
}

func (l line) String() string {
	switch l {
	case lineDTR:
		return "DTR"
	case lineRTS:
		return "RTS"
	}
	return "none"
}

// serialSender generates the Morse itself and keys a modem control line.
//
// This is viable only because remoses runs next to the radio: the network is
// never in the timing path, only in the path that queues text.
type serialSender struct {
	lines   transport.ControlLines
	keyLine line
	pttLine line
	lead    time.Duration
	tail    time.Duration
	weight  int

	mu    sync.Mutex
	queue []morse.Symbol
	wpm   int
	// gen is bumped by Abort. The keyer carries the generation it started with
	// and refuses to raise a line once it no longer matches, which is what
	// makes an abort land between two instructions rather than a few
	// microseconds late.
	gen      uint64
	busy     bool
	curChars int
	curEnd   time.Time
	closed   bool

	// abort is closed by Abort and replaced under the mutex, so a keyer parked
	// in a gap wakes at once rather than at the end of the character.
	abort chan struct{}
	// wake carries one pending "there is work" signal.
	wake chan struct{}
	done chan struct{}
}

// NewSerialKey builds a sender for rigs with no usable CAT CW buffer. It keys
// DTR or RTS and can assert the other line as PTT with a lead-in and tail.
func NewSerialKey(cl transport.ControlLines, cfg config.CW) (Sender, error) {
	if cl == nil {
		return nil, errors.New("cw: NewSerialKey needs a port with control lines")
	}
	sk := cfg.SerialKey
	if sk == nil {
		return nil, errors.New("cw: serial keying needs a serial_key configuration block")
	}
	keyLine, err := parseLine(sk.KeyLine)
	if err != nil {
		return nil, err
	}
	if keyLine == lineNone {
		return nil, errors.New("cw: serial keying needs key_line set to dtr or rts")
	}
	pttLine, err := parseLine(sk.PTTLine)
	if err != nil {
		return nil, err
	}
	if pttLine != lineNone && pttLine == keyLine {
		return nil, fmt.Errorf("cw: key_line and ptt_line are both %s", keyLine)
	}
	if sk.PTTLeadMS < 0 || sk.PTTTailMS < 0 {
		return nil, errors.New("cw: ptt_lead_ms and ptt_tail_ms cannot be negative")
	}

	s := &serialSender{
		lines:   cl,
		keyLine: keyLine,
		pttLine: pttLine,
		lead:    time.Duration(sk.PTTLeadMS) * time.Millisecond,
		tail:    time.Duration(sk.PTTTailMS) * time.Millisecond,
		weight:  sk.Weight,
		wpm:     ClampWPM(cfg.DefaultWPM),
		abort:   make(chan struct{}),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	// Start from a known state: a line left asserted by a previous run would
	// hold the transmitter down.
	s.mu.Lock()
	_ = s.write(s.keyLine, false)
	if s.pttLine != lineNone {
		_ = s.write(s.pttLine, false)
	}
	s.mu.Unlock()

	go s.run()
	return s, nil
}

// Charset is the full local table, which is richer than either rig's CAT
// buffer accepts. Capability flags report the difference so clients need not
// guess.
func (s *serialSender) Charset() string { return morse.Charset() }

func (s *serialSender) Enqueue(text string, mode Mode) (int, error) {
	syms, err := morse.Parse(text)
	if err != nil {
		return 0, s.charError(err)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, ErrClosed
	}
	if mode == Replace {
		s.queue = nil
	}
	s.queue = append(s.queue, syms...)
	s.mu.Unlock()

	s.signal()
	return utf8.RuneCountInString(text), nil
}

// charError restates a parse failure in the terms the API reports: the
// offending character and the charset that would have been accepted.
func (s *serialSender) charError(err error) error {
	var ce *morse.CharError
	if errors.As(err, &ce) {
		return &CharError{Char: ce.Char, Offset: ce.Offset, Charset: morse.Charset()}
	}
	var se *morse.SyntaxError
	if errors.As(err, &se) {
		return &CharError{Char: '^', Offset: se.Offset, Charset: morse.Charset()}
	}
	return err
}

// Abort stops inside one element. It drops both lines here rather than only
// asking the keyer to, because the keyer may not be scheduled for a while and
// a transmitter must not be left keyed on the strength of a goroutine wakeup.
func (s *serialSender) Abort() {
	s.mu.Lock()
	s.abortLocked()
	s.mu.Unlock()
}

func (s *serialSender) abortLocked() {
	s.gen++
	s.queue = nil
	s.busy = false
	s.curChars = 0
	s.curEnd = time.Time{}
	if !s.closed {
		close(s.abort)
		s.abort = make(chan struct{})
	}
	_ = s.write(s.keyLine, false)
	if s.pttLine != lineNone {
		_ = s.write(s.pttLine, false)
	}
}

func (s *serialSender) Status() radio.CWStatus {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	t := morse.NewTiming(s.wpm, s.weight)
	st := radio.CWStatus{WPM: s.wpm, Queued: s.curChars}
	var remaining time.Duration
	if s.curEnd.After(now) {
		remaining = s.curEnd.Sub(now)
	}
	for _, sym := range s.queue {
		st.Queued += sym.Chars()
	}
	if len(s.queue) > 0 {
		if s.busy {
			remaining += t.CharGap
		}
		remaining += t.Symbols(s.queue)
	}
	st.EstRemainingMS = int(remaining.Milliseconds())
	st.Busy = s.busy || len(s.queue) > 0
	return st
}

// SetSpeed is local state here: nothing else generates these elements. A
// change takes effect at the next symbol, so it never disturbs the schedule of
// the character being keyed.
func (s *serialSender) SetSpeed(wpm int) error {
	wpm = ClampWPM(wpm)
	s.mu.Lock()
	s.wpm = wpm
	s.mu.Unlock()
	return nil
}

// Close stops the keyer and leaves both lines low.
func (s *serialSender) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.gen++
	s.queue = nil
	s.closed = true
	close(s.abort)
	_ = s.write(s.keyLine, false)
	if s.pttLine != lineNone {
		_ = s.write(s.pttLine, false)
	}
	s.mu.Unlock()

	s.signal()
	<-s.done
	return nil
}

func (s *serialSender) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// timing reads the current speed and weighting.
func (s *serialSender) timing() morse.Timing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return morse.NewTiming(s.wpm, s.weight)
}

// write sets a control line. It must be called with the mutex held, so that an
// abort and an edge cannot interleave.
func (s *serialSender) write(l line, on bool) error {
	var err error
	switch l {
	case lineDTR:
		err = s.lines.SetDTR(on)
	case lineRTS:
		err = s.lines.SetRTS(on)
	default:
		return nil
	}
	return err
}

// assert raises a line for the transmission identified by gen. The generation
// check and the write share the mutex with Abort, so an abort can never be
// overtaken by the edge it was meant to prevent.
func (s *serialSender) assert(gen uint64, l line) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gen != gen || s.closed {
		return false
	}
	if err := s.write(l, true); err != nil {
		slog.Error("cw: serial keying failed, dropping the queue", "line", l.String(), "err", err)
		s.abortLocked()
		return false
	}
	return true
}

// release drops a line. Dropping is always safe, so it needs no generation
// check.
func (s *serialSender) release(l line) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.write(l, false)
}

func (s *serialSender) run() {
	// The keyer owns its OS thread: element edges are tens of milliseconds
	// apart and being rescheduled behind another goroutine is audible.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)

	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.closed {
			abort := s.abort
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-abort:
			}
			s.mu.Lock()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		gen, abort := s.gen, s.abort
		s.mu.Unlock()

		s.transmit(gen, abort)
	}
}

// transmit keys everything in the queue as one transmission.
//
// Every edge is scheduled as an offset from a single absolute start instant,
// so an element that runs late does not push the ones after it late as well.
// Accumulating sleeps would drift, and drift is audible.
func (s *serialSender) transmit(gen uint64, abort <-chan struct{}) {
	defer func() {
		s.mu.Lock()
		if s.gen == gen {
			s.busy = false
			s.curChars = 0
		}
		s.mu.Unlock()
	}()

	start := time.Now()
	var at time.Duration

	if s.pttLine != lineNone {
		if !s.assert(gen, s.pttLine) {
			return
		}
		at += s.lead
	}

	var prevChar, keyed bool
	for {
		t := s.timing()

		s.mu.Lock()
		if s.gen != gen || s.closed {
			s.mu.Unlock()
			return
		}
		if len(s.queue) == 0 {
			s.busy = false
			s.curChars = 0
			s.mu.Unlock()

			// Underrun. Hold PTT for the tail while watching for more text: a
			// keyboard client sends a word at a time, and dropping PTT between
			// words would make an amplifier chatter.
			if !s.waitForWork(start.Add(at+s.tail), abort) {
				if s.pttLine != lineNone {
					s.release(s.pttLine)
				}
				return
			}
			// Real time moved on while we waited. Re-anchor only if the
			// schedule is already behind, so a client that keeps up pays
			// nothing for the round trip.
			if resume := start.Add(at + t.CharGap); time.Now().After(resume) {
				start, at, prevChar = time.Now(), 0, false
			}
			continue
		}

		sym := s.queue[0]
		s.queue = s.queue[1:]
		s.busy = true
		s.curChars = sym.Chars()
		symEnd := at + t.Symbol(sym)
		if prevChar && sym.Kind != morse.KindSpace {
			symEnd += t.CharGap
		}
		s.curEnd = start.Add(symEnd)
		s.mu.Unlock()

		if sym.Kind == morse.KindSpace {
			// A word gap before the first mark is silence nobody can hear.
			if keyed {
				at += t.WordGap
			}
			prevChar = false
			continue
		}

		if prevChar {
			at += t.CharGap
		}
		for i, e := range sym.Elements {
			if i > 0 {
				at += t.ElementGap
			}
			if !s.waitUntil(start.Add(at), abort) {
				return
			}
			if !s.assert(gen, s.keyLine) {
				return
			}
			at += t.Mark(e)
			if !s.waitUntil(start.Add(at), abort) {
				s.release(s.keyLine)
				return
			}
			s.release(s.keyLine)
		}
		prevChar, keyed = true, true
	}
}

// waitUntil sleeps to just short of an edge and then spins out the remainder.
// It returns false if the transmission was aborted, which must stop the keyer
// inside the current element rather than at the end of the character.
func (s *serialSender) waitUntil(at time.Time, abort <-chan struct{}) bool {
	if d := time.Until(at) - spinWindow; d > 0 {
		timer := time.NewTimer(d)
		select {
		case <-timer.C:
		case <-abort:
			timer.Stop()
			return false
		}
	}
	for time.Now().Before(at) {
		select {
		case <-abort:
			return false
		default:
		}
	}
	select {
	case <-abort:
		return false
	default:
	}
	return true
}

// waitForWork waits for more text until the PTT tail expires. It reports
// whether text arrived in time to continue the same transmission.
func (s *serialSender) waitForWork(deadline time.Time, abort <-chan struct{}) bool {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		s.mu.Lock()
		queued := len(s.queue)
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return false
		}
		if queued > 0 {
			return true
		}
		select {
		case <-s.wake:
		case <-timer.C:
			return false
		case <-abort:
			return false
		}
	}
}
