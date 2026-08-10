package ws

import (
	"time"

	"github.com/hessu/remoses/internal/radio"
)

// Message type discriminators. Every server frame carries one, and the set is
// closed: clients are entitled to ignore anything they do not recognise, so a
// new type is a protocol change rather than a free extension point.
const (
	typeHello  = "hello"
	typeState  = "state"
	typeDelta  = "delta"
	typeCW     = "cw"
	typeConn   = "conn"
	typeResync = "resync"
)

// Client to server. The socket is a read-only stream — all control stays on
// REST — so this is deliberately tiny and carries no authorisation surface.
const (
	clientPing      = "ping"
	clientSubscribe = "subscribe"
)

// helloMsg opens every connection.
//
// Radios lists what THIS connection will carry, not what the instance has
// configured: with ?radios= in play the two differ, and a client needs to know
// which stream it actually got rather than what it asked for.
type helloMsg struct {
	Type       string    `json:"type"`
	Version    string    `json:"version"`
	Radios     []string  `json:"radios"`
	ServerTime time.Time `json:"server_time"`
}

// stateMsg is a full snapshot: sent once per radio on connect, on a new
// subscription, and whenever a change cannot be expressed as a delta.
type stateMsg struct {
	Type  string      `json:"type"`
	Radio string      `json:"radio"`
	Seq   uint64      `json:"seq"`
	TS    time.Time   `json:"ts"`
	State radio.State `json:"state"`
}

// deltaMsg carries only the fields that changed.
//
// Changed is a map rather than a struct because radio.Patch is an internal
// type with pointer fields and no JSON tags; the wire names come from
// radio.State's tags so that a client can apply a delta onto a snapshot field
// by field without a second vocabulary.
type deltaMsg struct {
	Type    string         `json:"type"`
	Radio   string         `json:"radio"`
	Seq     uint64         `json:"seq"`
	TS      time.Time      `json:"ts"`
	Changed map[string]any `json:"changed"`
}

// cwMsg reports the Morse queue. Discrete: never coalesced, because "queue
// went empty" and "queue went busy again" are different facts, not two
// versions of one.
type cwMsg struct {
	Type   string `json:"type"`
	Radio  string `json:"radio"`
	Busy   bool   `json:"busy"`
	Queued int    `json:"queued"`
	WPM    int    `json:"wpm"`
}

// connMsg reports a connection transition. Error carries the reason the port
// went away, which is the one thing an operator at the other end of the link
// cannot see for themselves.
type connMsg struct {
	Type      string `json:"type"`
	Radio     string `json:"radio"`
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// resyncMsg says "your view of this radio has a hole in it; refetch". It is
// the alternative to either wedging on a slow client or dropping its
// connection, both of which are worse.
type resyncMsg struct {
	Type  string `json:"type"`
	Radio string `json:"radio"`
}

// clientMsg is the whole client-to-server vocabulary. Unknown fields are
// ignored; unknown types are ignored by the reader.
type clientMsg struct {
	Type   string   `json:"type"`
	Radios []string `json:"radios"`
}

// changedFields renders a radio.Patch as the JSON object of a delta.
//
// radio.Patch is internal: pointer fields, no tags. The names here are
// radio.State's JSON tags, so `changed` is always a subset of `state` and a
// client needs one schema rather than two. st supplies the values for fields
// whose patch representation is narrower than the state representation — the
// patch records only that CW went busy, while the state carries the whole
// CWStatus, and sending half of it would be worse than sending all of it.
func changedFields(p radio.Patch, st radio.State) map[string]any {
	m := make(map[string]any, 8)
	if p.Frequency != nil {
		m["frequency"] = *p.Frequency
	}
	if p.Mode != nil {
		m["mode"] = *p.Mode
	}
	if p.DataMode != nil {
		m["data_mode"] = *p.DataMode
	}
	if p.PassbandHz != nil {
		m["passband_hz"] = *p.PassbandHz
	}
	if p.FilterSlot != nil {
		m["filter_slot"] = *p.FilterSlot
	}
	if p.Power != nil {
		m["power"] = *p.Power
	}
	if p.PTT != nil {
		m["ptt"] = *p.PTT
	}
	if p.SMeter != nil {
		m["s_meter"] = *p.SMeter
	}
	if p.CWBusy != nil {
		m["cw"] = st.CW
	}
	if p.Connected != nil {
		m["connected"] = *p.Connected
	}

	// The dual-VFO fields. A whole VFO goes out at once because that is how the
	// radio answers for one — an Icom's 26 carries mode, data mode and filter
	// together — and a client redrawing "VFO B" wants all of it anyway.
	if p.VFO != nil {
		m["vfo"] = *p.VFO
	}
	if p.VFOA != nil {
		m["vfo_a"] = *p.VFOA
	}
	if p.VFOB != nil {
		m["vfo_b"] = *p.VFOB
	}
	if p.Split != nil {
		m["split"] = *p.Split
	}
	if p.DualWatch != nil {
		m["dual_watch"] = *p.DualWatch
	}
	if p.SubSMeter != nil {
		m["sub_s_meter"] = *p.SubSMeter
	}
	if p.BreakIn != nil {
		m["break_in"] = *p.BreakIn
	}

	// The transmit meters go out as a group, and from st rather than from the
	// patch, because they are absent in receive rather than zero.
	//
	// A patch cannot express "this went away": Diff sets the field to the new
	// value, and the new value at the end of a transmission is nil, which is
	// indistinguishable from "not mentioned". Reading them off st instead means
	// a cleared meter is sent as an explicit null, which is what stops the last
	// transmission's SWR staying on a client's display for ever.
	//
	// PTT dropping is therefore a trigger in its own right: that is the moment
	// State.Apply clears all four, and the moment there is nothing left in the
	// patch to notice it by.
	if p.PowerMeter != nil || p.SWR != nil || p.ALC != nil || p.SWRRatio != nil ||
		(p.PTT != nil && !*p.PTT) {
		m["power_meter"] = st.PowerMeter
		m["swr"] = st.SWR
		m["alc"] = st.ALC
		m["swr_ratio"] = st.SWRRatio
	}

	if p.Tuner != nil {
		m["tuner"] = *p.Tuner
	}
	if p.Standby != nil {
		m["standby"] = *p.Standby
	}

	// The receive front end. Sent as the pointers themselves rather than
	// dereferenced, so that a control which stops being reported goes out as an
	// explicit null — the same reason the transmit meters do.
	if p.Preamp != nil {
		m["preamp"] = p.Preamp
	}
	if p.AttenuatorDB != nil {
		m["attenuator_db"] = p.AttenuatorDB
	}
	if p.RFGain != nil {
		m["rf_gain"] = p.RFGain
	}
	if p.AGC != nil {
		m["agc"] = *p.AGC
	}
	if p.IPPlus != nil {
		m["ip_plus"] = p.IPPlus
	}
	if p.DigiSel != nil {
		m["digi_sel"] = p.DigiSel
	}
	if p.DigiSelShift != nil {
		m["digi_sel_shift"] = p.DigiSelShift
	}
	return m
}
