package main

import (
	"testing"
	"time"
)

// A schedule without jitter, so the shape is asserted rather than a range: it
// doubles from half a second and stops at the ceiling instead of growing
// without bound.
func TestBackoffSchedule(t *testing.T) {
	b := newBackoff()
	b.rnd = func() float64 { return 0 }

	want := []time.Duration{
		500 * time.Millisecond,
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	for i, w := range want {
		attempt, got := b.next()
		if attempt != i+1 {
			t.Errorf("attempt %d reported as %d", i+1, attempt)
		}
		if got != w {
			t.Errorf("delay %d = %v, want %v", i+1, got, w)
		}
	}
}

// A stream that actually opened is evidence that the next failure is a new
// problem, so the schedule starts again rather than punishing a daemon that
// restarts twice.
func TestBackoffReset(t *testing.T) {
	b := newBackoff()
	b.rnd = func() float64 { return 0 }

	b.next()
	b.next()
	b.reset()

	attempt, d := b.next()
	if attempt != 1 || d != 500*time.Millisecond {
		t.Errorf("after reset: attempt %d, delay %v", attempt, d)
	}
}

// Jitter only ever shortens a wait, so the documented ceiling stays the worst
// case. Several clients losing the same instance at the same instant must not
// all come back at the same instant either.
func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	b := newBackoff()
	b.rnd = func() float64 { return 1 }

	_, d := b.next()
	if want := time.Duration(float64(500*time.Millisecond) * (1 - backoffJitter)); d != want {
		t.Errorf("fully jittered delay = %v, want %v", d, want)
	}

	seen := map[time.Duration]bool{}
	for range 20 {
		_, d := newBackoff().next()
		if d > 500*time.Millisecond || d < 400*time.Millisecond {
			t.Fatalf("jittered delay %v is outside [400ms, 500ms]", d)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Error("jitter produced the same delay every time")
	}
}
