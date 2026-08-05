package client

import (
	"time"

	"github.com/hessu/remoses/internal/radio"
)

// State is GET /radios/{id}/state.
//
// radio.State is embedded rather than restated so the two cannot drift: those
// JSON tags are the wire contract, and a field added to the daemon's state
// cache arrives here without an edit. AgeMS and Stale are the two members only
// the API computes.
type State struct {
	radio.State
	AgeMS int64 `json:"age_ms"`
	Stale bool  `json:"stale"`
}

// Age is the snapshot's age as the server measured it, which is the honest one:
// comparing UpdatedAt against a local clock would report whatever skew exists
// between the two machines as staleness.
func (s State) Age() time.Duration { return time.Duration(s.AgeMS) * time.Millisecond }

// Radio is GET /radios/{id}.
type Radio struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Backend   string     `json:"backend"`
	Connected bool       `json:"connected"`
	Caps      radio.Caps `json:"caps"`
	Limits    Limits     `json:"limits"`
	Lock      LockState  `json:"lock"`
}

// Limits are the configured transmit interlocks. A read-only monitor cannot hit
// them, but showing them tells an operator why the radio will refuse something.
type Limits struct {
	MaxPowerW   float64  `json:"max_power_w"`
	MaxPowerPct float64  `json:"max_power_pct"`
	TXTimeoutS  int      `json:"tx_timeout_s"`
	Bands       []string `json:"bands"`
}

// LockState says who holds exclusive control. This client never acquires one;
// it displays the answer so the operator can see whether somebody else is on
// the radio.
type LockState struct {
	Held      bool   `json:"held"`
	Holder    string `json:"holder"`
	ExpiresAt string `json:"expires_at"`
	IsMine    bool   `json:"is_mine"`
}
