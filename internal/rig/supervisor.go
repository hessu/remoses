package rig

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// supervise keeps the radio connected. USB serial is a supervised resource, not
// a handle you open once: the cable gets pulled, the rig gets switched off, and
// on Linux and macOS the device path is not even stable across a replug.
func (s *Session) supervise(ctx context.Context) {
	backoff := s.backoffMin
	for {
		if ctx.Err() != nil {
			return
		}

		connected, err := s.runConnection(ctx)
		if ctx.Err() != nil {
			return
		}

		switch {
		case s.poweredOff.Load():
			// remoses switched this radio off, so it going quiet is the command
			// working. Logging it as a failure — and at error level, once per
			// backoff, for as long as the operator leaves it off — would be
			// reporting a success as an outage.
			s.log.Info("radio is switched off; not reconnecting until it is woken",
				"target", s.dialer.Describe())
		case err == nil, errors.Is(err, transport.ErrDisconnected):
			// The expected end of a connection: someone pulled the cable or
			// switched the rig off. Not a crash, so not an error-level log.
			s.log.Info("radio disconnected", "target", s.dialer.Describe(), "err", err)
		default:
			s.log.Error("radio connection failed", "target", s.dialer.Describe(), "err", err)
		}

		if connected {
			// The connection got as far as a working rig, so whatever went
			// wrong is new. Start backing off from the bottom again.
			backoff = s.backoffMin
		}

		wait := jitter(backoff)
		s.log.Debug("reconnecting", "in", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = nextBackoff(backoff, s.backoffMin, s.backoffMax)
	}
}

// runConnection owns one connection from dial to death. It returns whether the
// rig ever became usable, which the caller uses to decide about backoff.
func (s *Session) runConnection(ctx context.Context) (connected bool, err error) {
	t, dialErr := s.dialer.Dial(ctx)
	if dialErr != nil {
		return false, fmt.Errorf("dial %s: %w", s.dialer.Describe(), dialErr)
	}

	c := newConn(s, t)
	c.start()
	s.conn.Store(c)

	defer func() {
		s.conn.Store(nil)
		s.connected.Store(false)
		c.close(nil)
		s.onDisconnected(c.lastErr())
	}()

	// A wake-up, if one is waiting. Before Init, because that is the point: a
	// radio that is off cannot answer Init, so the frames that would wake it
	// have to go out while the port is open and nothing else has failed yet.
	//
	// The wake is not verified here. A radio coming up spends seconds booting,
	// far longer than any command timeout, so Init failing straight afterwards
	// is expected rather than evidence the wake missed; the supervisor's next
	// attempt, a backoff later, is when it should be answering. That is also
	// why the request is consumed rather than retried — a wake that has been
	// sent has been sent, and repeating it on every attempt would hold a radio
	// the operator switched off at the panel permanently on.
	if s.takeWakeRequest() {
		if p, ok := s.rig.(backend.PowerSwitch); ok {
			s.log.Info("sending the radio a wake-up before init")
			if err := p.PowerOn(ctx, s); err != nil {
				s.log.Warn("wake-up failed", "err", err)
			}
		}
	}

	// Init enables push updates — Kenwood AI2;, Icom Transceive — and must run
	// again on every reconnect, because a rig that was power-cycled has
	// forgotten them (AI2 deliberately self-clears at power-off).
	if err := s.rig.Init(ctx, s); err != nil {
		// A radio that REFUSES Init is a different animal from one that does
		// not answer it. An IC-7610 switched off keeps its CI-V circuit alive
		// and answers NG to everything, frequency read included: the link is
		// perfect and the radio is asleep. Reporting that as a failed
		// connection sends somebody to check a cable that is fine.
		if errors.Is(err, ErrNAK) {
			if err := s.awaitWake(ctx, c); err != nil {
				return false, err
			}
		} else {
			return false, fmt.Errorf("init %s: %w", s.dialer.Describe(), err)
		}
	}
	// It is talking, so whatever remoses did to its power switch is history.
	s.poweredOff.Store(false)
	// Re-read: a backend may have learnt more from the rig than it knew from the
	// configuration. Through publishCaps, so that an installed CW sender's view
	// of keying survives the refresh — see there for what it is correcting.
	s.publishCaps()

	// A full state read before announcing the radio, so the first client to look
	// sees real values rather than zeroes.
	//
	// The slow tier is deliberately not fatal. It carries the nice-to-have
	// values — power, filter width — and rigs vary in which of those they
	// answer: an Icom that does not implement a command acknowledges it with FB
	// instead of returning data, and a Kenwood refuses with ?;. Treating that as
	// a connection failure would put a radio whose frequency, mode, PTT and
	// meters all work into a permanent reconnect loop. The fast tier is the
	// liveness signal, so it stays fatal.
	if err := s.rig.Poll(ctx, s, backend.PollSlow); err != nil {
		if isFatalPollErr(err) {
			return false, fmt.Errorf("initial slow poll: %w", err)
		}
		s.log.Warn("radio does not answer part of the slow poll; continuing without those values",
			"target", s.dialer.Describe(), "err", err)
	}
	if err := s.rig.Poll(ctx, s, backend.PollFast); err != nil {
		return false, fmt.Errorf("initial fast poll: %w", err)
	}

	s.connected.Store(true)
	s.apply(radio.Patch{Connected: ptrTo(true)}, EventConn, "")
	s.log.Info("radio connected", "target", s.dialer.Describe(), "backend", s.backend)

	pollCtx, stopPoll := context.WithCancel(ctx)
	defer stopPoll()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		s.pollLoop(pollCtx, c)
	}()

	select {
	case <-ctx.Done():
	case <-c.done:
	}
	stopPoll()
	<-pollDone

	return true, c.lastErr()
}

// awaitWake holds a connection open to a radio that is switched off, and
// returns once it wakes.
//
// This is the third state, and it needs one: the port is open and the radio is
// answering, so the link is not broken, but every command comes back NG so
// nothing can be read or set. Dropping the connection and redialling would be
// wrong twice over — it reports a healthy link as a fault, and it throws away
// the open port that a power-on has to be sent through.
//
// So the session stays connected, publishes Standby, and retries Init at the
// slow poll interval. Whoever wakes the radio — a power_switch request through
// remoses, or a hand on the front panel — is noticed within one interval and
// the connection carries on into its normal life.
//
// Returns nil once Init succeeds, or an error if the port dies or the session
// is shutting down.
func (s *Session) awaitWake(ctx context.Context, c *conn) error {
	s.standby.Store(true)
	// Connected AND standby: both are true, and a client needs both to say
	// "the radio is reachable but switched off" rather than guessing.
	//
	// The internal flag is set too, not just the published state. They are two
	// views of one fact and letting them disagree produced exactly the bug it
	// looks like it would: the API reported the radio connected while every
	// command was refused with "not currently connected".
	s.connected.Store(true)
	s.apply(radio.Patch{Connected: ptrTo(true), Standby: ptrTo(true)}, EventConn, "")
	s.log.Info("radio is switched off but answering; waiting for it to wake",
		"target", s.dialer.Describe())
	defer func() {
		s.standby.Store(false)
		s.apply(radio.Patch{Standby: ptrTo(false)}, EventConn, "")
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			return c.lastErr()
		case <-time.After(s.pollSlow):
		}

		// A wake armed while the session was already parked here. PowerOn sends
		// its own when there is a port to send on, so this covers the race
		// where one is armed between the port opening and standby being
		// entered — without it, that wake would wait for a reconnection that
		// this loop is deliberately preventing.
		if s.takeWakeRequest() {
			if p, ok := s.rig.(backend.PowerSwitch); ok {
				s.log.Info("sending the radio a wake-up")
				if err := p.PowerOn(ctx, s); err != nil {
					s.log.Warn("wake-up failed", "err", err)
				}
			}
		}

		err := s.rig.Init(ctx, s)
		if err == nil {
			s.log.Info("radio has woken", "target", s.dialer.Describe())
			return nil
		}
		if errors.Is(err, ErrNAK) {
			continue // still asleep
		}
		return fmt.Errorf("init %s: %w", s.dialer.Describe(), err)
	}
}

// onDisconnected publishes the loss and makes the safety state honest: with no
// port there is no way to key the radio, so PTT cannot meaningfully still be
// true, and holding CW text queued for a radio that is gone helps nobody.
func (s *Session) onDisconnected(cause error) {
	s.disarmDeadman()
	if snd := s.CW(); snd != nil {
		snd.Abort()
	}
	reason := ""
	if cause != nil {
		reason = cause.Error()
	}
	s.apply(radio.Patch{Connected: ptrTo(false), PTT: ptrTo(false)}, EventConn, reason)
	s.refreshCW()
}

// nextBackoff doubles up to the cap. 100 ms initially so a momentary glitch
// recovers immediately, 30 s at the top so an unplugged radio does not hammer
// the OS enumerating ports forever.
func nextBackoff(cur, minD, maxD time.Duration) time.Duration {
	if cur <= 0 {
		return minD
	}
	next := cur * 2
	if next > maxD {
		return maxD
	}
	return next
}

// jitter spreads reconnect attempts by +/-25%, so several radios that lost a
// shared USB hub do not retry in lockstep forever.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	delta := int64(d) / 4
	if delta <= 0 {
		return d
	}
	return time.Duration(int64(d) - delta + rand.Int64N(2*delta))
}

// isFatalPollErr reports whether a poll failure means the link is gone, as
// opposed to the radio simply declining one command.
//
// Only transport loss and silence are fatal: a rig that answers at all — even
// to refuse — is demonstrably still talking to us, and the command it refused
// is one it does not implement. Models within a family differ in which optional
// commands they answer, so without this rule a backend would only work on the
// exact model it was written against.
//
// The fatal set is enumerated rather than the tolerated set, because the
// tolerated one is open: every backend spells "the rig said something I did not
// expect" its own way, and those are all evidence the rig is alive.
//
// ErrBusy is in the tolerated set by construction, and must stay there. A rig
// answering "not now" — Yaesu's ?; — is the clearest possible proof it is still
// listening, and treating a momentary refusal as a lost link would reconnect a
// perfectly good radio. The retry is the next poll tick, not a new connection.
func isFatalPollErr(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrDisconnected), errors.Is(err, ErrTimeout):
		// The transport is gone, or the rig has stopped answering entirely.
		return true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// We are shutting down, not diagnosing the radio.
		return true
	}
	return false
}

func ptrTo[T any](v T) *T { return &v }
