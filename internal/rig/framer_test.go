package rig

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/transport"
)

// A backend implementing backend.ReplyFramer is told what is about to go on the
// wire so it can size the answer, and the whole value of the hook is that the
// session does the telling from inside the write lock. These tests pin that,
// because a backend that recorded the fact itself would have to do so before
// calling Do — outside the lock — and the poller and an HTTP setter overlapping
// there is the ordinary case, not a rare one.
//
// The failure the hook prevents is not a dropped reply. It is a permanently
// misframed stream: on a protocol with no delimiters, one answer sized against
// the wrong command offsets every answer after it.

// framingRig is a fakeRig that also implements backend.ReplyFramer, logging
// each call so a test can compare it against what reached the port.
type framingRig struct {
	*fakeRig
	log *wireLog
}

func (r *framingRig) Expect(req []byte) { r.log.add("expect", req) }

// wireLog records Expect calls and port writes in one ordered list, which is
// the only way to see the interleaving of two goroutines.
type wireLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *wireLog) add(kind string, b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf("%s %s", kind, b))
}

func (l *wireLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// loggingDialer wraps the fake dialer so every write is recorded next to the
// Expect that should immediately precede it.
type loggingDialer struct {
	inner *fakeDialer
	log   *wireLog
}

func (d *loggingDialer) Dial(ctx context.Context) (transport.Transport, error) {
	t, err := d.inner.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &loggingPort{Transport: t, log: d.log}, nil
}

func (d *loggingDialer) Describe() string { return d.inner.Describe() }

type loggingPort struct {
	transport.Transport
	log *wireLog
}

func (p *loggingPort) Write(b []byte) (int, error) {
	p.log.add("write", b)
	return p.Transport.Write(b)
}

// framingHarness is newHarness with a framing backend and a logging port.
func framingHarness(t *testing.T) (*Session, *wireLog) {
	t.Helper()
	log := &wireLog{}
	dev := newFakeDevice()
	dl := &loggingDialer{inner: newFakeDialer(dev), log: log}
	r := &framingRig{fakeRig: newFakeRig(), log: log}

	s, err := NewSession(config.Radio{
		ID:      "framer",
		Backend: "fake",
		Poll: config.Poll{
			Interval:     config.Duration(20 * time.Millisecond),
			SlowInterval: config.Duration(100 * time.Millisecond),
		},
	}, r, dl,
		WithLogger(testLogger()),
		WithCommandTimeout(150*time.Millisecond),
		WithEventQueue(256),
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	s.backoffMin = 2 * time.Millisecond
	s.backoffMax = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.Close()
	})
	s.Start(ctx)
	waitFor(t, "radio to connect", s.Connected)
	return s, log
}

// TestFramerIsToldBeforeEachWrite is the ordering requirement. A reply can be
// back before the writing goroutine is scheduled again, so a backend told after
// the write would already have had to frame it.
func TestFramerIsToldBeforeEachWrite(t *testing.T) {
	s, log := framingHarness(t)

	if _, err := s.SetFrequency(context.Background(), 0, 14_074_000); err != nil {
		t.Fatalf("SetFrequency: %v", err)
	}

	entries := log.snapshot()
	if len(entries) == 0 {
		t.Fatal("nothing was written")
	}
	for i, e := range entries {
		if i%2 == 0 {
			if len(e) < 6 || e[:6] != "expect" {
				t.Fatalf("entry %d is %q, want an expect; the log must alternate expect/write", i, e)
			}
			continue
		}
		if e[:5] != "write" {
			t.Fatalf("entry %d is %q, want a write", i, e)
		}
		// Same bytes, so the backend sized the answer to the command that
		// actually went out rather than to some other one.
		if e[5:] != entries[i-1][6:] {
			t.Fatalf("write %q does not match the preceding %q", e, entries[i-1])
		}
	}
}

// TestFramerStaysPairedUnderConcurrentCommands is the reason the hook exists at
// all. Eight goroutines issuing commands is the ordinary case — a poller and
// several HTTP handlers — and every write must still be immediately preceded by
// its own expect. A backend storing this for itself before calling Do would
// interleave here, and the stream would never resynchronise.
func TestFramerStaysPairedUnderConcurrentCommands(t *testing.T) {
	s, log := framingHarness(t)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 4 {
				var err error
				if j%2 == 0 {
					_, err = s.SetFrequency(context.Background(), 0, uint64(14_000_000+i*1000+j))
				} else {
					_, err = s.SetPTT(context.Background(), false)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent command: %v", err)
	}

	entries := log.snapshot()
	pairs := 0
	for i := 1; i < len(entries); i += 2 {
		if entries[i-1][:6] != "expect" || entries[i][:5] != "write" {
			t.Fatalf("entries %d/%d are %q/%q, want an expect then its write",
				i-1, i, entries[i-1], entries[i])
		}
		if entries[i][5:] != entries[i-1][6:] {
			t.Fatalf("write %q was preceded by expect %q: a concurrent command interleaved, "+
				"and a framing backend would now be sizing the wrong answer", entries[i], entries[i-1])
		}
		pairs++
	}
	if pairs < workers*4 {
		t.Fatalf("only %d expect/write pairs for %d commands", pairs, workers*4)
	}
}

// TestNonFramingBackendIsNotCalled keeps the hook opt-in: the three
// self-delimiting protocols must not acquire a per-write call they have no use
// for.
func TestNonFramingBackendIsNotCalled(t *testing.T) {
	h := startedHarness(t, nil)
	c := h.s.conn.Load()
	if c == nil {
		t.Fatal("no connection")
	}
	if c.framer != nil {
		t.Error("a backend that does not implement ReplyFramer was wired up as one")
	}
}
