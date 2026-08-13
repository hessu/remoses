package ws

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig"
)

// discreteEvent is one cw or conn notification waiting to be written.
//
// It is a flattened copy rather than the rig.Event it came from because this
// queue is bounded and per client: carrying a full radio.State per entry would
// multiply the memory held on behalf of clients that are not reading, which are
// precisely the ones not worth spending memory on.
//
// seq and at are carried for the same reason the state lane carries them: every
// frame about a radio says which version of that radio it describes, so a client
// can place a queue event in the stream without correlating it against a state
// message that may be rate limited into the next second.
type discreteEvent struct {
	kind    rig.EventKind
	radioID string
	seq     uint64
	at      time.Time
	cw      radio.CWStatus
	up      bool
	err     string
}

func (d discreteEvent) message() any {
	if d.kind == rig.EventCW {
		return cwMsg{
			Type:   typeCW,
			Radio:  d.radioID,
			Seq:    d.seq,
			TS:     d.at.UTC(),
			Busy:   d.cw.Busy,
			Queued: d.cw.Queued,
			WPM:    d.cw.WPM,
		}
	}
	return connMsg{
		Type:      typeConn,
		Radio:     d.radioID,
		Seq:       d.seq,
		TS:        d.at.UTC(),
		Connected: d.up,
		Error:     d.err,
	}
}

// client is one WebSocket connection.
//
// Everything a publisher touches is behind mu and does no I/O, so enqueue is
// bounded work that cannot block: that is the whole contract between this
// package and a rig session.
type client struct {
	hub    *Hub
	conn   *websocket.Conn
	user   string
	cancel context.CancelFunc

	minInterval  time.Duration
	pingInterval time.Duration
	queueCap     int

	// alive records that something arrived from the peer since the last
	// keepalive check. A browser proves it is there by answering a control
	// ping, but a client that only sends {"type":"ping"} proves it too, and
	// hanging up on it would be wrong.
	alive atomic.Bool

	mu     sync.Mutex
	subs   []string            // subscribed radio ids, configuration order
	subSet map[string]struct{} // same, for membership tests
	// pending is the state lane: the NEWEST event per radio and nothing else.
	// Bounded by the number of radios, so it cannot overflow however long a
	// client stalls.
	pending map[string]rig.Event
	order   []string // radios with pending state, oldest first
	// discrete is the event lane: cw and conn, in order, never merged.
	discrete []discreteEvent
	resync   map[string]struct{} // radios whose history has a hole
	snapshot map[string]struct{} // radios owed a full snapshot
	lastSeq  map[string]uint64   // highest seq written, per radio
	nextAt   map[string]time.Time
	// nextResync rate limits resync the same way state is rate limited. A
	// resync costs the client a REST refetch, so turning one burst of dropped
	// events into a hundred of them would be a worse denial of service than the
	// burst was.
	nextResync map[string]time.Time
	closed     bool
	wake       chan struct{}

	stopOnce sync.Once
}

func newClient(h *Hub, conn *websocket.Conn, user string, radios []string, cancel context.CancelFunc) *client {
	c := &client{
		hub:          h,
		conn:         conn,
		user:         user,
		cancel:       cancel,
		minInterval:  h.minInterval,
		pingInterval: h.pingInterval,
		queueCap:     h.queueCap,
		subs:         radios,
		subSet:       make(map[string]struct{}, len(radios)),
		pending:      map[string]rig.Event{},
		resync:       map[string]struct{}{},
		snapshot:     make(map[string]struct{}, len(radios)),
		lastSeq:      map[string]uint64{},
		nextAt:       map[string]time.Time{},
		nextResync:   map[string]time.Time{},
		wake:         make(chan struct{}, 1),
	}
	for _, id := range radios {
		c.subSet[id] = struct{}{}
		// Every subscribed radio is owed a snapshot before any delta, so the
		// client has something to apply deltas to.
		c.snapshot[id] = struct{}{}
	}
	return c
}

// run drives the connection until it ends, then tears down everything it
// started. It runs on the HTTP handler's goroutine.
func (c *client) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer c.stop()
		c.readLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		c.pingLoop(ctx)
	}()

	c.writeLoop(ctx)
	c.stop()
	wg.Wait()
}

// stop ends the connection. Idempotent, and safe to call from any goroutine.
func (c *client) stop() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.signal()
		c.cancel()
		// CloseNow rather than the close handshake: Close writes a close frame
		// and then waits seconds for the peer to answer, and the peer we are
		// hanging up on is the one that will not. This is the layer whose job
		// is to never wait for a slow client.
		_ = c.conn.CloseNow()
	})
}

// enqueue offers one event to this client. It never blocks and never fails:
// a client that cannot keep up loses detail, not the publisher's time.
func (c *client) enqueue(ev rig.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}

	if ev.Dropped > 0 {
		// The hub's own subscription overflowed. The manager counts drops per
		// subscription rather than per radio, so there is no way to know which
		// radios lost events: tell the client to refetch everything it watches
		// rather than let it keep stale state for the radios we guessed wrong
		// about. This is rare — the hub loop only ever does non-blocking work.
		c.markResyncLocked()
		c.signal()
	}

	if _, ok := c.subSet[ev.RadioID]; !ok {
		// Not ours. Deliberately no wake: a client filtered down to one radio
		// must not pay a writer wakeup for every event of every other one.
		return
	}

	switch ev.Kind {
	case rig.EventCW:
		c.pushDiscreteLocked(discreteEvent{
			kind:    rig.EventCW,
			radioID: ev.RadioID,
			seq:     ev.Seq,
			at:      ev.At,
			cw:      ev.State.CW,
		})
	case rig.EventConn:
		c.pushDiscreteLocked(discreteEvent{
			kind:    rig.EventConn,
			radioID: ev.RadioID,
			seq:     ev.Seq,
			at:      ev.At,
			up:      ev.State.Connected,
			err:     ev.Err,
		})
	}

	// Every kind also feeds the state lane. A conn or cw event moves the state
	// cache and its seq, and a client whose snapshot silently stopped tracking
	// would see a gap it could not explain. Last-value-wins does the rest: only
	// the newest survives.
	if _, queued := c.pending[ev.RadioID]; !queued {
		c.order = append(c.order, ev.RadioID)
	}
	c.pending[ev.RadioID] = ev

	c.signal()
}

// pushDiscreteLocked appends to the event lane, or drops the lane.
//
// cw and conn events are not last-value-wins, so there is no honest way to
// compress them. When the lane is full the choice is between handing the client
// a truncated history it would believe, wedging, and admitting the loss: the
// last one is the only one that leaves the client able to recover.
func (c *client) pushDiscreteLocked(d discreteEvent) {
	if len(c.discrete) >= c.queueCap {
		c.discrete = c.discrete[:0]
		c.markResyncLocked()
		return
	}
	c.discrete = append(c.discrete, d)
}

// markResyncLocked flags every subscribed radio for a resync message.
func (c *client) markResyncLocked() {
	for _, id := range c.subs {
		c.resync[id] = struct{}{}
	}
}

func (c *client) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// setRadios replaces the subscription from a client subscribe message.
//
// An empty list means every radio, exactly as an absent ?radios= does at
// connect: one rule for both spellings of the same request.
func (c *client) setRadios(ids []string) {
	resolved := c.hub.resolveRadios(strings.Join(ids, ","))

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}

	set := make(map[string]struct{}, len(resolved))
	for _, id := range resolved {
		set[id] = struct{}{}
		if _, had := c.subSet[id]; !had {
			// Newly subscribed radios need a snapshot before their deltas mean
			// anything, just as at connect.
			c.snapshot[id] = struct{}{}
		}
	}
	c.subs, c.subSet = resolved, set

	for id := range c.pending {
		if _, ok := set[id]; !ok {
			delete(c.pending, id)
		}
	}
	keep := c.order[:0]
	for _, id := range c.order {
		if _, ok := c.pending[id]; ok {
			keep = append(keep, id)
		}
	}
	c.order = keep

	live := c.discrete[:0]
	for _, d := range c.discrete {
		if _, ok := set[d.radioID]; ok {
			live = append(live, d)
		}
	}
	c.discrete = live

	for id := range c.resync {
		if _, ok := set[id]; !ok {
			delete(c.resync, id)
		}
	}
	for id := range c.snapshot {
		if _, ok := set[id]; !ok {
			delete(c.snapshot, id)
		}
	}

	c.signal()
}

// hello builds the opening message.
func (c *client) hello() helloMsg {
	c.mu.Lock()
	radios := append([]string(nil), c.subs...)
	c.mu.Unlock()
	if radios == nil {
		radios = []string{}
	}
	return helloMsg{
		Type:       typeHello,
		Version:    c.hub.version,
		Radios:     radios,
		ServerTime: time.Now().UTC(),
	}
}

// drain collects everything due now, and reports the earliest time at which
// something held back by the rate limit becomes due (zero if nothing is).
//
// Ordering is deliberate: resync first, because it describes a hole in what
// came before and must not appear to apply to what comes after; then discrete
// events in the order they happened; then snapshots, which are the base a delta
// is relative to; then deltas.
func (c *client) drain(now time.Time) (out []any, next time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.resync) > 0 {
		for _, id := range c.subs {
			if _, ok := c.resync[id]; !ok {
				continue
			}
			if at, limited := c.nextResync[id]; limited && now.Before(at) {
				// Still flagged: a resync held back is not a resync lost.
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			out = append(out, resyncMsg{
				Type:  typeResync,
				Radio: id,
				// The last version this connection got, which is the edge of
				// the hole: everything after it is what the client has to
				// refetch to find out about.
				Seq: c.lastSeq[id],
				TS:  now.UTC(),
			})
			delete(c.resync, id)
			c.nextResync[id] = now.Add(c.minInterval)
		}
	}

	for _, d := range c.discrete {
		out = append(out, d.message())
	}
	c.discrete = c.discrete[:0]

	if len(c.snapshot) > 0 {
		for _, id := range c.subs {
			if _, ok := c.snapshot[id]; !ok {
				continue
			}
			delete(c.snapshot, id)
			st, ok := c.hub.state(id)
			if !ok {
				continue
			}
			out = append(out, stateMsg{
				Type:  typeState,
				Radio: id,
				Seq:   st.Seq,
				TS:    st.UpdatedAt.UTC(),
				State: st,
			})
			c.lastSeq[id] = st.Seq
			// The snapshot counts against the rate limit, so a flood already in
			// progress cannot follow it immediately with a delta.
			c.nextAt[id] = now.Add(c.minInterval)
		}
	}

	if len(c.pending) > 0 {
		keep := c.order[:0]
		for _, id := range c.order {
			ev, ok := c.pending[id]
			if !ok {
				continue
			}
			if at, limited := c.nextAt[id]; limited && now.Before(at) {
				keep = append(keep, id)
				if next.IsZero() || at.Before(next) {
					next = at
				}
				continue
			}
			delete(c.pending, id)
			msg, ok := c.stateMessageLocked(ev)
			if !ok {
				continue
			}
			out = append(out, msg)
			c.lastSeq[id] = ev.Seq
			c.nextAt[id] = now.Add(c.minInterval)
		}
		c.order = keep
	}

	return out, next
}

// stateMessageLocked renders one state-lane event, or reports that it is stale.
//
// seq is the session's, never this package's: it is monotonic per radio and
// clients use it for gap detection, so an event that a newer snapshot has
// already superseded is dropped rather than sent out of order.
func (c *client) stateMessageLocked(ev rig.Event) (any, bool) {
	if ev.Seq <= c.lastSeq[ev.RadioID] {
		return nil, false
	}
	if ev.Patch.Empty() {
		// A CW queue change advances seq without touching a field any delta can
		// name, so a snapshot is the only honest rendering.
		return stateMsg{
			Type:  typeState,
			Radio: ev.RadioID,
			Seq:   ev.Seq,
			TS:    ev.At.UTC(),
			State: ev.State,
		}, true
	}
	return deltaMsg{
		Type:    typeDelta,
		Radio:   ev.RadioID,
		Seq:     ev.Seq,
		TS:      ev.At.UTC(),
		Changed: changedFields(ev.Patch, ev.State),
	}, true
}

// writeLoop is the only goroutine that writes data messages.
func (c *client) writeLoop(ctx context.Context) {
	if err := c.write(ctx, c.hello()); err != nil {
		return
	}

	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	for {
		msgs, next := c.drain(time.Now())
		for _, m := range msgs {
			if err := c.write(ctx, m); err != nil {
				return
			}
		}

		if next.IsZero() {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			continue
		}

		wait := time.Until(next)
		if wait <= 0 {
			wait = time.Millisecond
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (c *client) write(ctx context.Context, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		// A message this package built cannot fail to marshal; if it does, the
		// bug is here and killing the connection would hide it.
		c.hub.log.Error("marshalling websocket message", "user", c.user, "err", err)
		return nil
	}

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, b)
}

// readLoop consumes client messages. Reading is mandatory even though the
// stream is one-way: control frames are only processed by a reader, so without
// this the keepalive would never see a pong.
func (c *client) readLoop(ctx context.Context) {
	c.conn.SetReadLimit(readLimit)
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		c.alive.Store(true)
		if typ != websocket.MessageText {
			continue
		}
		c.handle(data)
	}
}

// handle applies one client message.
//
// The vocabulary is ping and subscribe, and anything else is ignored in
// silence. That is the security property of this endpoint: all control stays on
// REST, so there is no message a client can send here that reaches a radio, and
// no error reply that would invite it to look for one.
func (c *client) handle(data []byte) {
	var m clientMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case clientPing:
		// Liveness only, already recorded by the read. There is no pong
		// envelope: the server vocabulary in the API contract is closed, and
		// the connection's own control-frame ping is what measures the link.
	case clientSubscribe:
		c.setRadios(m.Radios)
	}
}

// pingLoop closes a connection whose peer has stopped answering.
//
// This is not cosmetic. A client that has gone away still holds its radio lock
// until the lease runs out, and if it went away mid-transmission the lock
// expiry is what drops PTT. Noticing quickly is a safety property.
func (c *client) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.pingInterval)
	defer ticker.Stop()

	missed := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Clear the flag before pinging, so only traffic that arrives from here
		// on counts as an answer to this ping.
		c.alive.Store(false)

		pctx, cancel := context.WithTimeout(ctx, c.pingInterval)
		err := c.conn.Ping(pctx)
		cancel()

		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil, c.alive.Load():
			missed = 0
			continue
		case !errors.Is(err, context.DeadlineExceeded):
			// Not a missed pong: the connection itself is gone.
			c.stop()
			return
		}

		missed++
		if missed >= maxMissedPongs {
			c.hub.log.Info("closing unresponsive websocket client",
				"user", c.user, "missed_pongs", missed)
			c.stop()
			return
		}
	}
}
