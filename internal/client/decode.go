package client

import (
	"encoding/json"

	"github.com/hessu/remoses/internal/radio"
)

// Decoding rule for this package: whatever the daemon emits, this client can
// read. Two mode values break that if the wire types are used naively.
//
// The first is UNKNOWN. The API reports it for a radio that has not connected
// yet, and radio.ParseMode refuses it on purpose — UNKNOWN is never accepted as
// *input*. Decoding output with an input parser therefore fails on precisely
// the radio a monitor most needs to display: the one that is unplugged.
//
// The second is a mode name a newer daemon knows and this build does not. That
// is one field of one snapshot, and it is not a reason to discard the
// frequency, the meters and the CW queue that came with it.
//
// So mode is lifted out of the object and parsed leniently, and everything else
// is decoded by the ordinary rules.

// unmarshalState decodes a state object, or the `changed` object of a delta,
// onto st. Fields the object does not mention are left untouched, which is what
// makes the same function serve a full snapshot and a partial update.
func unmarshalState(data []byte, st *radio.State) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	if raw, ok := fields["mode"]; ok {
		delete(fields, "mode")
		var name string
		if err := json.Unmarshal(raw, &name); err != nil {
			return err
		}
		st.Mode = lenientMode(name)
	}

	rest, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(rest, st)
}

// lenientMode parses a mode name for display, falling back to UNKNOWN — which
// is what the daemon itself would report for a value it could not make sense
// of.
func lenientMode(name string) radio.Mode {
	m, err := radio.ParseMode(name)
	if err != nil {
		return radio.ModeUnknown
	}
	return m
}

// UnmarshalJSON decodes a state snapshot plus the two members only the API
// computes.
func (s *State) UnmarshalJSON(b []byte) error {
	if err := unmarshalState(b, &s.State); err != nil {
		return err
	}
	var extra struct {
		AgeMS int64 `json:"age_ms"`
		Stale bool  `json:"stale"`
	}
	if err := json.Unmarshal(b, &extra); err != nil {
		return err
	}
	s.AgeMS, s.Stale = extra.AgeMS, extra.Stale
	return nil
}

// UnmarshalJSON decodes a radio descriptor, tolerating a capability list that
// names a mode this build has never heard of.
func (r *Radio) UnmarshalJSON(b []byte) error {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}

	if raw, ok := fields["caps"]; ok {
		delete(fields, "caps")
		caps, err := unmarshalCaps(raw)
		if err != nil {
			return err
		}
		r.Caps = caps
	}

	rest, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	// plain drops the method set, so this does not call itself.
	type plain Radio
	return json.Unmarshal(rest, (*plain)(r))
}

// unmarshalCaps decodes capabilities, dropping mode names it cannot parse.
//
// Dropping rather than mapping to UNKNOWN: a capability list is a statement
// about what the radio will accept, and "this radio supports UNKNOWN" is not a
// statement worth making.
func unmarshalCaps(data []byte) (radio.Caps, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return radio.Caps{}, err
	}

	var modes []radio.Mode
	if raw, ok := fields["modes"]; ok {
		delete(fields, "modes")
		var names []string
		if err := json.Unmarshal(raw, &names); err != nil {
			return radio.Caps{}, err
		}
		for _, n := range names {
			if m, err := radio.ParseMode(n); err == nil {
				modes = append(modes, m)
			}
		}
	}

	rest, err := json.Marshal(fields)
	if err != nil {
		return radio.Caps{}, err
	}
	var caps radio.Caps
	if err := json.Unmarshal(rest, &caps); err != nil {
		return radio.Caps{}, err
	}
	caps.Modes = modes
	return caps, nil
}
