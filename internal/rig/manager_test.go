package rig

import (
	"context"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

func init() {
	// A backend name used only by this test binary.
	backend.Register("faketest", func(r *config.Radio) (backend.Rig, error) {
		return newFakeRig(), nil
	})
}

func managerConfig(ids ...string) *config.Config {
	cfg := &config.Config{}
	for _, id := range ids {
		cfg.Radios = append(cfg.Radios, config.Radio{
			ID:      id,
			Name:    "Radio " + id,
			Backend: "faketest",
			Poll: config.Poll{
				Interval:     config.Duration(20 * time.Millisecond),
				SlowInterval: config.Duration(100 * time.Millisecond),
			},
		})
	}
	return cfg
}

func newTestManager(t *testing.T, ids ...string) (*Manager, map[string]*fakeDevice, map[string]*fakeDialer) {
	t.Helper()
	devs := map[string]*fakeDevice{}
	dials := map[string]*fakeDialer{}

	m, err := NewManager(managerConfig(ids...),
		WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond),
		WithDialerFactory(func(rc *config.Radio) (transport.Dialer, error) {
			dev := newFakeDevice()
			d := newFakeDialer(dev)
			devs[rc.ID] = dev
			dials[rc.ID] = d
			return d, nil
		}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	for _, s := range m.List() {
		s.backoffMin = 2 * time.Millisecond
		s.backoffMax = 10 * time.Millisecond
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = m.Close()
	})
	m.Start(ctx)
	for _, s := range m.List() {
		waitFor(t, "radio "+s.ID()+" to connect", s.Connected)
	}
	return m, devs, dials
}

func TestManagerListIsInConfigurationOrder(t *testing.T) {
	m, _, _ := newTestManager(t, "ic7610", "ts590sg", "ft857")

	want := []string{"ic7610", "ts590sg", "ft857"}
	// Repeated, because a map-backed implementation would look right once.
	for range 20 {
		var got []string
		for _, s := range m.List() {
			got = append(got, s.ID())
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("List() = %v, want %v", got, want)
			}
		}
	}

	s, ok := m.Get("ts590sg")
	if !ok || s.ID() != "ts590sg" {
		t.Fatalf("Get(ts590sg) = %v, %v", s, ok)
	}
	if _, ok := m.Get("nope"); ok {
		t.Error("Get returned a radio that does not exist")
	}
}

func TestManagerRequiresADialerFactory(t *testing.T) {
	if _, err := NewManager(managerConfig("a")); err == nil {
		t.Error("NewManager without a dialer factory: want an error")
	}
	if _, err := NewManager(nil); err == nil {
		t.Error("NewManager(nil): want an error")
	}
}

func TestManagerRejectsDuplicateRadioIDs(t *testing.T) {
	cfg := managerConfig("dup", "dup")
	_, err := NewManager(cfg, WithLogger(testLogger()),
		WithDialerFactory(func(rc *config.Radio) (transport.Dialer, error) {
			return newFakeDialer(newFakeDevice()), nil
		}))
	if err == nil {
		t.Error("duplicate radio ids accepted")
	}
}

func TestManagerSubscribeMultiplexesAllRadios(t *testing.T) {
	m, _, _ := newTestManager(t, "one", "two")

	ch, unsub := m.Subscribe()
	defer unsub()

	one, _ := m.Get("one")
	two, _ := m.Get("two")
	if _, err := one.SetFrequency(context.Background(), radio.VFOCurrent, 7020000); err != nil {
		t.Fatalf("SetFrequency one: %v", err)
	}
	if _, err := two.SetFrequency(context.Background(), radio.VFOCurrent, 21020000); err != nil {
		t.Fatalf("SetFrequency two: %v", err)
	}

	seen := map[string]bool{}
	deadline := time.After(5 * time.Second)
	for len(seen) < 2 {
		select {
		case ev := <-ch:
			if ev.Kind == EventState {
				seen[ev.RadioID] = true
			}
		case <-deadline:
			t.Fatalf("aggregate stream carried only %v", seen)
		}
	}
}

func TestManagerAggregateSubscriberCannotStallASession(t *testing.T) {
	m, devs, _ := newTestManager(t, "one")
	m.queue = 1

	stalled, unsub := m.Subscribe()
	defer unsub()

	one, _ := m.Get("one")
	for i := range 40 {
		if _, err := one.SetFrequency(context.Background(), radio.VFOCurrent, uint64(14000000+i*100)); err != nil {
			t.Fatalf("SetFrequency: %v", err)
		}
	}
	if f, _, _, _ := devs["one"].snapshot(); f != 14003900 {
		t.Fatalf("radio frequency = %d, want 14003900", f)
	}

	drainForDrop(t, stalled, func() {
		for i := range 5 {
			if _, err := one.SetFrequency(context.Background(), radio.VFOCurrent, uint64(14100000+i*100)); err != nil {
				t.Errorf("SetFrequency: %v", err)
			}
		}
	})
}

func TestManagerUnsubscribeClosesAggregate(t *testing.T) {
	m, _, _ := newTestManager(t, "one", "two")
	ch, unsub := m.Subscribe()
	unsub()
	unsub()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("aggregate channel not closed")
		}
	}
}

func TestManagerCloseClosesOutstandingAggregates(t *testing.T) {
	m, _, _ := newTestManager(t, "one")
	ch, _ := m.Subscribe()

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Manager.Close left an aggregate subscription open")
		}
	}
}

func TestNewManagerWithSessions(t *testing.T) {
	h1 := newHarness(t, nil)
	h2 := newHarness(t, func(rc *config.Radio) { rc.ID = "second" })

	m, err := NewManagerWithSessions(h1.s, h2.s)
	if err != nil {
		t.Fatalf("NewManagerWithSessions: %v", err)
	}
	if len(m.List()) != 2 {
		t.Fatalf("List() = %d sessions, want 2", len(m.List()))
	}
	if _, ok := m.Get("second"); !ok {
		t.Error("Get(second) failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	waitFor(t, "both radios", func() bool { return h1.s.Connected() && h2.s.Connected() })
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
