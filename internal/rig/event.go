package rig

import (
	"sync"
	"time"

	"github.com/hessu/remoses/internal/radio"
)

// EventKind names the three streams a subscriber sees. They are kept separate
// because the WebSocket layer treats them differently: state is last-value-wins
// and may be coalesced, while cw and conn are discrete and must not be merged.
type EventKind string

const (
	// EventState is a change to the radio's operating state. Patch carries only
	// the fields that actually changed.
	EventState EventKind = "state"
	// EventCW is a change to the CW queue: busy, depth or speed. The CW queue
	// lives in the cw.Sender, not in the state cache, so it gets its own kind
	// rather than being squeezed into a Patch that has no field for depth.
	EventCW EventKind = "cw"
	// EventConn is a connection transition. Err carries the reason when
	// Connected went false.
	EventConn EventKind = "conn"
)

// Event is one published change for one radio.
//
// It always carries the complete post-change State as well as the Patch, so a
// consumer can either send a delta (Patch) or fall back to a full snapshot
// (State) without asking the session anything. That matters for backpressure:
// a WebSocket client that fell behind can be resynchronised from the newest
// event alone.
type Event struct {
	Kind    EventKind
	RadioID string
	// Seq is the state cache sequence number at the time of the event. It is
	// monotonic per radio, so a consumer can detect gaps independently of
	// Dropped.
	Seq   uint64
	At    time.Time
	State radio.State
	// Patch holds the changed fields for EventState. It is empty for EventCW,
	// whose payload is State.CW.
	Patch radio.Patch
	// Err is the reason a connection went away, for EventConn only.
	Err string
	// Dropped is how many events this subscriber missed immediately before this
	// one, because its queue was full. Non-zero means "you are out of date":
	// the WebSocket layer turns it into a resync message. It is never a reason
	// to slow the session down.
	Dropped uint64
}

// defaultQueue is the per-subscriber buffer. Deep enough to absorb a burst from
// a spun VFO knob, shallow enough that a wedged consumer is noticed quickly.
const defaultQueue = 64

type subscriber struct {
	ch chan Event
	// dropped counts events discarded since the last successful send. It is
	// guarded by the owning session's subMu, not by the subscriber.
	dropped uint64
	closed  bool
}

// subscribers is the fan-out set shared by Session and Manager.
//
// Every send is non-blocking. A subscriber that does not keep up loses events
// and is told so through Event.Dropped; the publisher never waits. This is the
// whole point: a stalled WebSocket client must not be able to stall a serial
// port.
type subscribers struct {
	mu   sync.Mutex
	set  map[*subscriber]struct{}
	size int
}

func newSubscribers(size int) *subscribers {
	if size <= 0 {
		size = defaultQueue
	}
	return &subscribers{set: map[*subscriber]struct{}{}, size: size}
}

// add registers a new subscriber and returns its channel plus an idempotent
// unsubscribe function. The channel is closed on unsubscribe so range loops
// terminate.
func (s *subscribers) add() (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, s.size)}
	s.mu.Lock()
	s.set[sub] = struct{}{}
	s.mu.Unlock()

	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			delete(s.set, sub)
			// Safe to close here: publish() holds the same mutex, so no send
			// can be in flight. closeAll may already have done it, hence the
			// flag — a session that shuts down under a live subscriber must not
			// turn into a double close.
			if !sub.closed {
				sub.closed = true
				close(sub.ch)
			}
		})
	}
}

// publish delivers ev to every subscriber that has room, and records a drop
// against those that do not.
func (s *subscribers) publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		if sub.closed {
			continue
		}
		e := ev
		e.Dropped = sub.dropped
		select {
		case sub.ch <- e:
			sub.dropped = 0
		default:
			sub.dropped++
		}
	}
}

// closeAll shuts every subscriber channel. Used when a Manager is torn down.
func (s *subscribers) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.set {
		if !sub.closed {
			sub.closed = true
			close(sub.ch)
		}
		delete(s.set, sub)
	}
}

// count reports the number of live subscribers. Used by tests.
func (s *subscribers) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.set)
}
