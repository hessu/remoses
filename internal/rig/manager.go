package rig

import (
	"context"
	"fmt"
	"sync"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// DialerFactory builds the transport dialer for one configured radio.
//
// It is a hook rather than a direct import of the serial package on purpose:
// this package must stay free of any dependency on real hardware so that the
// whole session stack can be exercised with a fake transport, and so that the
// rigctld backend can bring a TCP dialer instead.
type DialerFactory func(*config.Radio) (transport.Dialer, error)

var (
	dialerMu      sync.RWMutex
	dialerFactory DialerFactory
)

// SetDialerFactory installs the process-wide dialer factory. main calls it once
// at startup with the serial implementation, before building a Manager.
func SetDialerFactory(f DialerFactory) {
	dialerMu.Lock()
	defer dialerMu.Unlock()
	dialerFactory = f
}

// WithDialerFactory overrides the process-wide factory for one Manager. Tests
// use it to substitute a fake transport.
func WithDialerFactory(f DialerFactory) Option {
	return func(o *options) { o.dialerFor = f }
}

func currentDialerFactory() DialerFactory {
	dialerMu.RLock()
	defer dialerMu.RUnlock()
	return dialerFactory
}

// Manager holds one Session per configured radio and multiplexes their events.
type Manager struct {
	sessions []*Session // configuration order, which is the order the API lists
	byID     map[string]*Session
	queue    int

	mu     sync.Mutex
	aggs   map[*aggregate]struct{}
	closed bool

	startOnce sync.Once
	closeOnce sync.Once
}

// NewManager builds a session for every radio in the configuration. It performs
// no I/O: a radio that is unplugged at startup is still created, and its
// supervisor keeps trying.
func NewManager(cfg *config.Config, opts ...Option) (*Manager, error) {
	if cfg == nil {
		return nil, fmt.Errorf("rig: nil configuration")
	}
	o := resolve(opts)
	newDialer := o.dialerFor
	if newDialer == nil {
		newDialer = currentDialerFactory()
	}
	if newDialer == nil {
		return nil, fmt.Errorf("rig: no dialer factory configured; call rig.SetDialerFactory")
	}

	m := newManager(o.queue)
	for i := range cfg.Radios {
		rc := &cfg.Radios[i]
		if _, dup := m.byID[rc.ID]; dup {
			return nil, fmt.Errorf("rig: duplicate radio id %q", rc.ID)
		}
		r, err := backend.New(rc)
		if err != nil {
			return nil, err
		}
		d, err := newDialer(rc)
		if err != nil {
			return nil, fmt.Errorf("rig: radio %q: %w", rc.ID, err)
		}
		s, err := NewSession(*rc, r, d, opts...)
		if err != nil {
			return nil, err
		}
		m.add(s)
	}
	return m, nil
}

// NewManagerWithSessions builds a Manager around sessions that already exist.
// It is the seam for tests and for any wiring that constructs sessions itself.
func NewManagerWithSessions(sessions ...*Session) (*Manager, error) {
	m := newManager(0)
	for _, s := range sessions {
		if _, dup := m.byID[s.id]; dup {
			return nil, fmt.Errorf("rig: duplicate radio id %q", s.id)
		}
		m.add(s)
	}
	return m, nil
}

func newManager(queue int) *Manager {
	if queue <= 0 {
		queue = defaultQueue
	}
	return &Manager{
		byID:  map[string]*Session{},
		queue: queue,
		aggs:  map[*aggregate]struct{}{},
	}
}

func (m *Manager) add(s *Session) {
	m.sessions = append(m.sessions, s)
	m.byID[s.id] = s
}

// Start launches every session's supervisor.
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		for _, s := range m.sessions {
			s.Start(ctx)
		}
	})
}

// Close stops every session and waits for their goroutines, then closes any
// aggregate subscriptions so their consumers see the stream end.
func (m *Manager) Close() error {
	var firstErr error
	m.closeOnce.Do(func() {
		for _, s := range m.sessions {
			if err := s.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		m.mu.Lock()
		m.closed = true
		aggs := make([]*aggregate, 0, len(m.aggs))
		for a := range m.aggs {
			aggs = append(aggs, a)
		}
		m.aggs = map[*aggregate]struct{}{}
		m.mu.Unlock()
		for _, a := range aggs {
			a.stop()
		}
	})
	return firstErr
}

// Get returns the session for a radio id.
func (m *Manager) Get(id string) (*Session, bool) {
	s, ok := m.byID[id]
	return s, ok
}

// List returns the sessions in configuration order, which is the order the API
// presents them in — stable across restarts and independent of map iteration.
func (m *Manager) List() []*Session {
	out := make([]*Session, len(m.sessions))
	copy(out, m.sessions)
	return out
}

// Subscribe returns an instance-wide event stream carrying every radio, because
// the WebSocket endpoint is instance-wide: one connection sees all radios.
//
// Backpressure works exactly as it does per session — the send is non-blocking
// and a subscriber that falls behind is told through Event.Dropped rather than
// being allowed to stall a serial port. Drops counted by the underlying session
// subscriptions are carried through, so the count is not lost at the join.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	a := &aggregate{ch: make(chan Event, m.queue)}

	// Each session is forwarded by its own goroutine, started per subscriber so
	// that the manager keeps no permanent forwarding goroutines alive for radios
	// nobody is watching.
	for _, s := range m.sessions {
		in, stop := s.Subscribe()
		a.stops = append(a.stops, stop)
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for ev := range in {
				a.send(ev)
			}
		}()
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		a.stop()
		return a.ch, func() {}
	}
	m.aggs[a] = struct{}{}
	m.mu.Unlock()

	return a.ch, func() {
		m.mu.Lock()
		delete(m.aggs, a)
		m.mu.Unlock()
		a.stop()
	}
}

// aggregate joins several session subscriptions into one channel.
type aggregate struct {
	ch    chan Event
	stops []func()
	wg    sync.WaitGroup

	mu      sync.Mutex
	dropped uint64
	closed  bool

	once sync.Once
}

func (a *aggregate) send(ev Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	ev.Dropped += a.dropped
	select {
	case a.ch <- ev:
		a.dropped = 0
	default:
		a.dropped++
	}
}

func (a *aggregate) stop() {
	a.once.Do(func() {
		for _, stop := range a.stops {
			stop()
		}
		a.wg.Wait()
		a.mu.Lock()
		a.closed = true
		close(a.ch)
		a.mu.Unlock()
	})
}
