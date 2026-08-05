package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/radio"
)

// streamReadLimit bounds one server message. A full state snapshot is a few
// hundred bytes; this leaves room for a radio with a long capability list
// without letting a confused peer allocate without limit.
const streamReadLimit = 256 << 10

// Keepalive timings. The server pings us and hangs up on a client that stops
// answering, but that only protects the server: a client whose network path
// died silently would sit forever on a socket nobody is going to close. So the
// stream pings in the other direction too, and a ping that goes unanswered is
// what turns a half-open connection into a reconnect.
const (
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
)

// EventKind discriminates a server message. The set is closed: the server
// contract says so, and an unrecognised type is skipped rather than surfaced.
type EventKind int

const (
	EventHello EventKind = iota
	EventState
	EventDelta
	EventCW
	EventConn
	EventResync
)

func (k EventKind) String() string {
	switch k {
	case EventHello:
		return "hello"
	case EventState:
		return "state"
	case EventDelta:
		return "delta"
	case EventCW:
		return "cw"
	case EventConn:
		return "conn"
	case EventResync:
		return "resync"
	}
	return "unknown"
}

// Event is one decoded server message.
type Event struct {
	Kind  EventKind
	Radio string
	Seq   uint64
	TS    time.Time

	// State is the snapshot carried by a state message.
	State radio.State
	// Changed is the raw object of a delta message, and Fields names its keys.
	// The raw form is kept because the keys are radio.State's own JSON tags,
	// which makes applying a delta a decode onto the current snapshot rather
	// than a field-by-field switch that would have to be edited every time the
	// state grows a member.
	Changed json.RawMessage
	Fields  []string

	// CW carries a cw message. Queued and WPM are the message's; Busy too.
	CW radio.CWStatus
	// Connected and Err carry a conn message.
	Connected bool
	Err       string

	// Version and Radios carry hello. Radios is what this connection actually
	// got, which is not always what was asked for.
	Version string
	Radios  []string
}

// ApplyDelta returns st with the delta's changed fields applied.
//
// The decode goes straight onto a copy of the snapshot because the wire names
// in `changed` are radio.State's JSON tags by construction — the server builds
// them from that struct — so json.Unmarshal applies exactly the fields present
// and leaves the rest alone.
func (e Event) ApplyDelta(st radio.State) (radio.State, error) {
	next := st
	// A shallow copy shares the meter pointers with st, and decoding into a
	// non-nil pointer field writes through it. Without this, applying a delta
	// would mutate the caller's previous snapshot as a side effect — which
	// matters, because the previous snapshot is what a renderer diffs against.
	if st.SWR != nil {
		v := *st.SWR
		next.SWR = &v
	}
	if st.ALC != nil {
		v := *st.ALC
		next.ALC = &v
	}

	if len(e.Changed) > 0 {
		if err := unmarshalState(e.Changed, &next); err != nil {
			return st, fmt.Errorf("applying delta for %s: %w", e.Radio, err)
		}
	}
	next.Seq = e.Seq
	if !e.TS.IsZero() {
		next.UpdatedAt = e.TS
	}
	return next, nil
}

// Stream is a live subscription to GET /ws.
//
// Next must be called continuously: it is the only thing that processes control
// frames, so the keepalive's pong — and the server's ping — depend on a reader
// being in flight. That is the normal shape of a monitor anyway.
type Stream struct {
	conn   *websocket.Conn
	url    string
	cancel context.CancelFunc
}

// Stream subscribes to state changes for one radio.
//
// Authentication is the ordinary Basic header on the upgrade. The ticket flow
// in the API exists for browsers, which cannot set headers on a WebSocket
// handshake; a programmatic client that used it would be doing an extra round
// trip to work around a limitation it does not have.
func (c *Client) Stream(ctx context.Context, radioID string) (*Stream, error) {
	u := *c.base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path += "/ws"
	q := url.Values{}
	q.Set("radios", radioID)
	u.RawQuery = q.Encode()

	h := http.Header{}
	h.Set("Authorization", "Basic "+
		base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))

	conn, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPClient: c.ws,
		HTTPHeader: h,
	})
	if err != nil {
		// A rejected upgrade is an ordinary HTTP response, and reporting it as
		// one is the difference between "authentication failed" and a dial
		// error the operator has to guess at.
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			defer resp.Body.Close()
			return nil, errorFromResponse(u.String(), resp)
		}
		return nil, fmt.Errorf("websocket %s: %w", u.String(), err)
	}
	conn.SetReadLimit(streamReadLimit)

	// Detached from the dial context on purpose: that one may carry a
	// connect timeout, and the stream outlives its own handshake.
	kctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &Stream{conn: conn, url: u.String(), cancel: cancel}
	go s.keepalive(kctx)
	return s, nil
}

// URL is the stream endpoint, for error messages.
func (s *Stream) URL() string { return s.url }

// Next returns the next server message, skipping types this client does not
// know. It blocks until one arrives, ctx is done, or the connection ends.
func (s *Stream) Next(ctx context.Context) (Event, error) {
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			return Event{}, err
		}
		if typ != websocket.MessageText {
			continue
		}
		ev, ok, err := decodeEvent(data)
		if err != nil {
			return Event{}, err
		}
		if !ok {
			continue
		}
		return ev, nil
	}
}

// Close ends the stream. CloseNow rather than the close handshake: the peer we
// are hanging up on may be the one that has stopped answering, and a monitor
// exiting on Ctrl-C should not wait seconds to find that out.
func (s *Stream) Close() error {
	s.cancel()
	return s.conn.CloseNow()
}

func (s *Stream) keepalive(ctx context.Context) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := s.conn.Ping(pctx)
		cancel()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		// Closing here is the point: it is what makes the blocked Next return
		// an error, which is what starts the reconnect.
		_ = s.conn.CloseNow()
		return
	}
}

// envelope is every server frame in one struct. The message set is small and
// closed, so one decode with a type discriminator is simpler — and cheaper —
// than a two-pass decode into per-type structs.
type envelope struct {
	Type  string    `json:"type"`
	Radio string    `json:"radio"`
	Seq   uint64    `json:"seq"`
	TS    time.Time `json:"ts"`

	// State is raw because a snapshot has to go through unmarshalState; see
	// decode.go.
	State   json.RawMessage `json:"state"`
	Changed json.RawMessage `json:"changed"`

	Busy   bool `json:"busy"`
	Queued int  `json:"queued"`
	WPM    int  `json:"wpm"`

	Connected bool   `json:"connected"`
	Error     string `json:"error"`

	Version string   `json:"version"`
	Radios  []string `json:"radios"`
}

// decodeEvent turns one frame into an Event. The bool reports whether the frame
// was a type this client knows; an unknown one is not an error, because the
// contract entitles a client to ignore it.
func decodeEvent(data []byte) (Event, bool, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Event{}, false, fmt.Errorf("decoding websocket message: %w", err)
	}

	ev := Event{Radio: env.Radio, Seq: env.Seq, TS: env.TS}
	switch env.Type {
	case "hello":
		ev.Kind, ev.Version, ev.Radios = EventHello, env.Version, env.Radios
	case "state":
		ev.Kind = EventState
		if err := unmarshalState(env.State, &ev.State); err != nil {
			return Event{}, false, fmt.Errorf("decoding state snapshot: %w", err)
		}
	case "delta":
		ev.Kind, ev.Changed = EventDelta, env.Changed
		ev.Fields = changedFieldNames(env.Changed)
	case "cw":
		ev.Kind = EventCW
		ev.CW = radio.CWStatus{Busy: env.Busy, Queued: env.Queued, WPM: env.WPM}
	case "conn":
		ev.Kind, ev.Connected, ev.Err = EventConn, env.Connected, env.Error
	case "resync":
		ev.Kind = EventResync
	default:
		return Event{}, false, nil
	}
	return ev, true, nil
}

// changedFieldNames lists the keys of a delta, sorted so that output built from
// them is stable.
func changedFieldNames(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
