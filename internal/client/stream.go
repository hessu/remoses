package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/hessu/remoses/internal/wire"
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

// EventKind discriminates a server message. The set is closed: the contract
// says so, and an unrecognised type is skipped rather than surfaced.
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
		return string(wire.WSHelloTypeHello)
	case EventState:
		return string(wire.WSStateTypeState)
	case EventDelta:
		return string(wire.WSDeltaTypeDelta)
	case EventCW:
		return string(wire.WSCWTypeCW)
	case EventConn:
		return string(wire.WSConnTypeConn)
	case EventResync:
		return string(wire.WSResyncTypeResync)
	}
	return "unknown"
}

// Event is one decoded server message.
//
// It is flatter than the generated union, which carries one struct per frame
// type: a monitor handles all six in one switch, and lifting the fields they
// share out of six types is what lets it. The union is still what decides which
// frame this is, so the flattening cannot invent a field the spec does not have.
type Event struct {
	Kind  EventKind
	Radio string
	Seq   int64
	TS    time.Time

	// State is the snapshot carried by a state message.
	State wire.State
	// Changed is the `changed` object of a delta message, as it arrived. See
	// ApplyDelta for why the bytes are kept rather than the decoded form.
	Changed json.RawMessage

	// CW carries a cw message.
	CW wire.CWStatus
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
// The decode goes straight onto the snapshot because the names in `changed` are
// wire.State's own JSON tags — both come from the same schema, StateFields — so
// json.Unmarshal applies exactly the fields the server named and leaves the
// rest alone. That includes the nulls: a transmit meter that has gone away
// arrives as an explicit null and lands as a nil pointer, which is what stops
// the last transmission's SWR sitting on the display for ever.
//
// It is done with the bytes rather than by merging the decoded
// wire.StateDelta field by field. The decoded form can express everything the
// bytes can — the fields that clear are declared nullable, so the generated
// type holds absent, null and a value apart — but merging it means fifty
// `if d.X.IsSpecified() { st.X = d.X }` lines, which is a list that falls
// behind the spec the first time somebody adds a field to it. Decoding onto
// the snapshot applies exactly what the server named, for ever, with no list.
//
// st goes through its own JSON first, to get a copy whose pointers nobody else
// holds: decoding into a struct with non-nil pointer fields writes THROUGH
// them, so without it a delta would reach back into the caller's previous
// snapshot. Copying the pointer fields by hand instead would be a list that
// silently falls behind the spec, which is the failure this whole arrangement
// exists to prevent.
func (e Event) ApplyDelta(st wire.State) (wire.State, error) {
	raw, err := json.Marshal(st)
	if err != nil {
		return st, fmt.Errorf("copying state for %s: %w", e.Radio, err)
	}
	var next wire.State
	if err := json.Unmarshal(raw, &next); err != nil {
		return st, fmt.Errorf("copying state for %s: %w", e.Radio, err)
	}

	if len(e.Changed) > 0 {
		if err := json.Unmarshal(e.Changed, &next); err != nil {
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
	if err := checkRadioID(radioID); err != nil {
		return nil, err
	}

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
			body, _ := io.ReadAll(io.LimitReader(resp.Body, problemLimit))
			return nil, errorFromResponse(u.String(), resp, body)
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

// deltaPayload keeps the `changed` object of a delta frame as bytes.
//
// wire.WSDelta decodes it into a StateDelta, which is the right type for a
// client that wants to look at one field of one delta. It is the wrong thing to
// apply a delta with; see ApplyDelta.
type deltaPayload struct {
	Changed json.RawMessage `json:"changed"`
}

// decodeEvent turns one frame into an Event. The bool reports whether the frame
// was a type this client knows; an unknown one is not an error, because the
// contract entitles a client to ignore it.
func decodeEvent(data []byte) (Event, bool, error) {
	var msg wire.WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return Event{}, false, fmt.Errorf("decoding websocket message: %w", err)
	}
	kind, err := msg.Discriminator()
	if err != nil {
		return Event{}, false, fmt.Errorf("decoding websocket message: %w", err)
	}

	switch kind {
	case string(wire.WSHelloTypeHello):
		m, err := msg.AsWSHello()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding hello: %w", err)
		}
		return Event{
			Kind:    EventHello,
			TS:      m.ServerTime,
			Version: m.Version,
			Radios:  m.Radios,
		}, true, nil

	case string(wire.WSStateTypeState):
		m, err := msg.AsWSState()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding state snapshot: %w", err)
		}
		return Event{
			Kind:  EventState,
			Radio: m.Radio,
			Seq:   m.Seq,
			TS:    m.TS,
			State: m.State,
		}, true, nil

	case string(wire.WSDeltaTypeDelta):
		m, err := msg.AsWSDelta()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding delta: %w", err)
		}
		var payload deltaPayload
		if err := json.Unmarshal(data, &payload); err != nil {
			return Event{}, false, fmt.Errorf("decoding delta: %w", err)
		}
		return Event{
			Kind:    EventDelta,
			Radio:   m.Radio,
			Seq:     m.Seq,
			TS:      m.TS,
			Changed: payload.Changed,
		}, true, nil

	case string(wire.WSCWTypeCW):
		m, err := msg.AsWSCW()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding cw event: %w", err)
		}
		return Event{
			Kind:  EventCW,
			Radio: m.Radio,
			Seq:   m.Seq,
			TS:    m.TS,
			CW:    wire.CWStatus{Busy: m.Busy, Queued: m.Queued, WPM: m.WPM},
		}, true, nil

	case string(wire.WSConnTypeConn):
		m, err := msg.AsWSConn()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding conn event: %w", err)
		}
		return Event{
			Kind:      EventConn,
			Radio:     m.Radio,
			Seq:       m.Seq,
			TS:        m.TS,
			Connected: m.Connected,
			Err:       valueOr(m.Error),
		}, true, nil

	case string(wire.WSResyncTypeResync):
		m, err := msg.AsWSResync()
		if err != nil {
			return Event{}, false, fmt.Errorf("decoding resync: %w", err)
		}
		return Event{Kind: EventResync, Radio: m.Radio, Seq: m.Seq, TS: m.TS}, true, nil
	}
	return Event{}, false, nil
}
