package rig

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hessu/remoses/internal/rig/backend"
	"github.com/hessu/remoses/internal/transport"
)

// pollLoop refreshes the state cache on two tickers.
//
// With Transceive or AI enabled the poller is mostly a safety net against a
// missed push, but it is also the only thing that notices a rig that has
// stopped answering while its USB adapter stays enumerated — switched off at
// the front panel, say. Nothing arrives on the port in that case, so the reader
// never errors and only a run of poll timeouts reveals it.
func (s *Session) pollLoop(ctx context.Context, c *conn) {
	fast := time.NewTicker(s.pollFast)
	defer fast.Stop()
	slow := time.NewTicker(s.pollSlow)
	defer slow.Stop()

	// Failures are counted per tier. A rig that answers the slow tier but has
	// stopped answering the fast one is still broken, and letting one tier's
	// success reset the other's counter would hide exactly that.
	var fastFails, slowFails int
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-fast.C:
			if !s.pollTier(ctx, c, backend.PollFast, &fastFails) {
				return
			}
			s.refreshCW()
			drain(fast)
		case <-slow.C:
			if !s.pollTier(ctx, c, backend.PollSlow, &slowFails) {
				return
			}
			drain(slow)
		}
	}
}

// pollTier runs one poll and reports whether the loop should continue.
func (s *Session) pollTier(ctx context.Context, c *conn, tier backend.PollTier, fails *int) bool {
	err := s.rig.Poll(ctx, c, tier)
	if err == nil {
		*fails = 0
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, transport.ErrDisconnected) {
		// The reader is already on its way out; let it drive the teardown.
		return false
	}

	*fails++
	s.log.Warn("poll failed", "tier", tierName(tier), "fails", *fails, "err", err)
	if *fails >= maxPollFailures {
		// The port is open but the radio is not answering. Kill the connection
		// so the supervisor redials, rather than reporting a radio as connected
		// while its state quietly goes stale.
		c.fail(fmt.Errorf("radio %s: %d consecutive poll failures: %w", s.id, *fails, err))
		return false
	}
	return true
}

// drain discards a tick that piled up while a poll was in flight. A ticker
// already drops ticks when the receiver is slow, but it can still hold one
// stale tick that would fire a second poll the instant the first returns.
// Skipping is right here: poll results are last-value-wins.
func drain(t *time.Ticker) {
	select {
	case <-t.C:
	default:
	}
}

func tierName(t backend.PollTier) string {
	if t == backend.PollSlow {
		return "slow"
	}
	return "fast"
}
