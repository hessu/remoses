package main

import (
	"math/rand/v2"
	"time"
)

// Reconnect backoff. The floor is short because the common cause of a dropped
// stream is the daemon being restarted, and waiting seconds to notice it came
// back is worse than one wasted dial. The ceiling bounds how long a daemon that
// has come back can go unnoticed.
//
// The ceiling was 30 s, so that a monitor left running against a daemon that is
// off for the night settled into one attempt every half minute. Five seconds
// matches what the session's own supervisor now uses for a radio, and for the
// same reason: the measured cost of that ceiling is the operator watching a
// restarted daemon for up to half a minute before the display comes back, and
// the thing being economised is one refused TCP connect to localhost. The
// monitor is something a person sits in front of, which makes the trade even
// more one-sided here than it is for the daemon.
const (
	backoffBase   = 500 * time.Millisecond
	backoffMax    = 5 * time.Second
	backoffFactor = 2.0
	// backoffJitter spreads reconnects. Several clients watching the same
	// instance all lose the stream at the same instant when it restarts, and
	// without jitter they would all come back at the same instant too.
	backoffJitter = 0.2
)

// backoff hands out the delay before each reconnect attempt.
type backoff struct {
	base   time.Duration
	max    time.Duration
	factor float64
	jitter float64
	// rnd returns a value in [0,1). It is injectable so a test can assert the
	// schedule rather than a range.
	rnd func() float64

	attempt int
	delay   time.Duration
}

func newBackoff() *backoff {
	return &backoff{
		base:   backoffBase,
		max:    backoffMax,
		factor: backoffFactor,
		jitter: backoffJitter,
		rnd:    rand.Float64,
	}
}

// next advances the schedule and returns the attempt number and how long to
// wait before it.
func (b *backoff) next() (int, time.Duration) {
	b.attempt++
	if b.delay == 0 {
		b.delay = b.base
	} else {
		b.delay = time.Duration(float64(b.delay) * b.factor)
	}
	if b.delay > b.max {
		b.delay = b.max
	}

	d := b.delay
	if b.jitter > 0 && b.rnd != nil {
		// Subtractive jitter only: it may shorten a wait but never pushes one
		// past the ceiling, so the worst case stays the documented one.
		d -= time.Duration(float64(d) * b.jitter * b.rnd())
	}
	return b.attempt, d
}

// reset returns to the floor. It is called on a stream that actually opened,
// not on one that merely dialled, so a server that accepts and immediately
// hangs up still backs off.
func (b *backoff) reset() {
	b.attempt, b.delay = 0, 0
}
