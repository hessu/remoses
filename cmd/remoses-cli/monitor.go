package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/hessu/remoses/internal/client"
)

// dialTimeout bounds the WebSocket handshake. Without it a dial to a host that
// blackholes packets would sit past every reconnect the backoff schedules.
const dialTimeout = 15 * time.Second

// tickInterval is how often the display is refreshed with nothing new to show.
// The age of the snapshot is part of the picture — it is what tells an operator
// that a disconnected radio has been gone for two minutes — so it has to keep
// counting even when no events arrive.
const tickInterval = time.Second

// descInterval refreshes the radio descriptor.
//
// It exists for the lock: the WebSocket contract lists a lock frame that the
// server does not emit, so the only way to see that another operator has taken
// the radio is to ask. A GET every quarter minute is cheap, and it is a GET —
// this program still never acquires anything.
const descInterval = 15 * time.Second

// errStreamClosed is the stream ending without an error of its own.
var errStreamClosed = errors.New("stream closed by the server")

// updateKind says what caused a render, so a renderer that emits lines can
// decide whether this one is worth one. A renderer that redraws in place
// ignores it and diffs frames instead.
type updateKind int

const (
	updateState updateKind = iota
	updateLink
	updateResync
	updateTick
)

// renderer displays the view. Two implement it: one redraws a block in a
// terminal, one writes lines into a pipe.
type renderer interface {
	update(v *view, kind updateKind)
	close()
}

type monitor struct {
	cl      *client.Client
	radioID string
	view    *view
	out     renderer
	bo      *backoff
	once    bool

	// sleep is time.After in production; a test replaces it so the reconnect
	// schedule can be asserted without waiting for it.
	sleep func(time.Duration) <-chan time.Time
}

func newMonitor(cl *client.Client, radioID string, out renderer, v *view) *monitor {
	return &monitor{
		cl:      cl,
		radioID: radioID,
		view:    v,
		out:     out,
		bo:      newBackoff(),
		sleep:   time.After,
	}
}

// run fetches the current state, then keeps it current until ctx is done.
func (m *monitor) run(ctx context.Context) error {
	// The first fetch is fatal on failure, and deliberately so: it is also the
	// credential check and the "is there such a radio" check. A monitor that
	// answered a wrong password by entering a reconnect loop would never get
	// round to saying what was wrong.
	if err := m.fetchAll(ctx); err != nil {
		return err
	}
	m.out.update(m.view, updateState)

	if m.once {
		return nil
	}

	for {
		err := m.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if client.Fatal(err) {
			return err
		}

		attempt, wait := m.bo.next()
		m.view.link = linkReconnecting
		m.view.linkNote = fmt.Sprintf("retry in %s (attempt %d): %s",
			formatAge(wait), attempt, err)
		m.out.update(m.view, updateLink)

		select {
		case <-ctx.Done():
			return nil
		case <-m.sleep(wait):
		}
	}
}

// fetchAll takes the descriptor and the state over REST, so that something
// useful is on screen before the stream has finished its handshake.
func (m *monitor) fetchAll(ctx context.Context) error {
	desc, err := m.cl.Radio(ctx, m.radioID)
	if err != nil {
		return err
	}
	m.view.desc = desc
	return m.fetchState(ctx)
}

func (m *monitor) fetchState(ctx context.Context) error {
	st, err := m.cl.State(ctx, m.radioID)
	if err != nil {
		return err
	}
	m.view.setSnapshot(st)
	return nil
}

// session runs one WebSocket connection to completion and reports why it
// ended.
func (m *monitor) session(ctx context.Context) error {
	m.view.link = linkConnecting
	m.view.linkNote = ""
	m.out.update(m.view, updateLink)

	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	stream, err := m.cl.Stream(dctx, m.radioID)
	cancel()
	if err != nil {
		return err
	}
	defer stream.Close()

	// Only now: a dial that succeeded and a stream that carries messages are
	// different things, but the handshake completing is the strongest evidence
	// available that this connection is not going to fail the same way again.
	m.bo.reset()
	m.view.link = linkLive
	m.view.linkNote = ""
	m.out.update(m.view, updateLink)

	rctx, stopReader := context.WithCancel(ctx)
	defer stopReader()

	events := make(chan streamMsg, 8)
	go readStream(rctx, stream, events)

	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	desc := time.NewTicker(descInterval)
	defer desc.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-tick.C:
			m.out.update(m.view, updateTick)

		case <-desc.C:
			m.refreshDescriptor(ctx)

		case msg, ok := <-events:
			if !ok {
				return errStreamClosed
			}
			if msg.err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return msg.err
			}
			if err := m.handle(ctx, msg.ev); err != nil {
				return err
			}
		}
	}
}

type streamMsg struct {
	ev  client.Event
	err error
}

// readStream turns the blocking reader into a channel, so that the display can
// keep counting the snapshot's age while nothing is arriving.
func readStream(ctx context.Context, s *client.Stream, out chan<- streamMsg) {
	defer close(out)
	for {
		ev, err := s.Next(ctx)
		select {
		case out <- streamMsg{ev: ev, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

// handle applies one server message. Only an error that makes the connection
// useless is returned; everything else is displayed.
func (m *monitor) handle(ctx context.Context, ev client.Event) error {
	if ev.Kind != client.EventHello && ev.Radio != m.radioID {
		// The subscription filter should have prevented this. Ignoring it is
		// safer than displaying another radio's frequency under this one's name.
		return nil
	}

	switch ev.Kind {
	case client.EventHello:
		// hello reports what the connection actually carries, which is not
		// always what was asked for: unknown ids are dropped rather than
		// refused, so this is where a client finds out.
		if len(ev.Radios) > 0 && !slices.Contains(ev.Radios, m.radioID) {
			m.view.linkNote = "the stream carries no data for this radio"
			m.out.update(m.view, updateLink)
		}
		return nil

	case client.EventState:
		m.view.setState(ev.State)

	case client.EventDelta:
		if err := m.view.applyDelta(ev); err != nil {
			return err
		}

	case client.EventCW:
		m.view.applyCW(ev.CW)

	case client.EventConn:
		m.view.applyConn(ev.Connected, ev.Err)

	case client.EventResync:
		// The server dropped events for this client. What was missed is
		// unknowable from here, so the state is refetched rather than guessed
		// at.
		m.out.update(m.view, updateResync)
		if err := m.fetchState(ctx); err != nil {
			if client.Fatal(err) {
				return err
			}
			// A refetch that failed for a transient reason is not worth
			// dropping a working stream over; the next event still arrives.
			m.view.linkNote = "resync refetch failed: " + err.Error()
			m.out.update(m.view, updateLink)
			return nil
		}
	}

	m.out.update(m.view, updateState)
	return nil
}

// refreshDescriptor re-reads the radio descriptor for the lock display. Errors
// are ignored on purpose: this is cosmetic, and anything that actually broke
// the connection will surface through the stream instead.
func (m *monitor) refreshDescriptor(ctx context.Context) {
	desc, err := m.cl.Radio(ctx, m.radioID)
	if err != nil {
		return
	}
	m.view.desc = desc
	m.out.update(m.view, updateState)
}
