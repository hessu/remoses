// Package backend defines the contract every rig protocol implementation
// satisfies.
//
// The split of responsibility is deliberate and load-bearing:
//
//   - A backend is PURE PROTOCOL. It builds request bytes, splits the incoming
//     stream into frames, and decodes a frame into a state patch. It contains no
//     goroutines, no timers, no locks, and never touches a Transport.
//   - The SESSION owns all concurrency. It runs the single reader goroutine,
//     serialises writes, correlates replies with pending requests, applies
//     timeouts, and folds decoded patches into the state cache.
//
// That is why every command method takes a Conn: the backend asks the session
// to perform a transaction rather than doing I/O itself. It keeps backends
// trivially unit-testable — feed bytes to Decode, assert on the Patch — and it
// means unsolicited frames (Icom Transceive, Kenwood AI) flow through exactly
// the same decode path as solicited replies.
package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
)

// ErrBusy means the rig declined a command because it could not deal with it
// just then, and that sending the identical command again is the recovery.
//
// It is TRANSIENT, and that is the whole point of it having a name. Nothing may
// treat it as a persistent failure: it must not disable a poll item, must not
// mark a capability absent, must not tear the connection down, and must not be
// cached or remembered anywhere. The next poll tick retries by itself.
//
// It lives in this package rather than in one backend because two layers that
// must not import a backend implementation have to recognise it: the session,
// which decides whether a poll failure is fatal, and the API, which answers 503
// "try again" rather than 422 "your request was wrong". internal/rig aliases it
// as rig.ErrBusy so a caller needs only one check.
//
// Yaesu's ?; is what made it necessary, and it is ambiguous in the wild: it
// means "not now" and also "not allowed in the state the rig is in". remoses
// treats both as transient, because the recovery is identical either way and
// the alternative — disabling a command permanently on the strength of one
// answer — risks losing a capability over a momentary condition.
var ErrBusy = errors.New("backend: rig busy, the command can be retried")

// ErrUnsupported means this radio cannot do what was asked, and no retry will
// change that: the command does not exist in its CAT set, the value is outside
// the range it accepts, or the control is a front-panel one.
//
// It lives here for the same reason ErrBusy does. internal/rig aliases it as
// rig.ErrUnsupported, which the API already maps to 422 with the error's own
// text — so a backend that wraps its refusals in this gets "your radio has no
// such control", with the explanation, instead of a bare 500 that reads as a
// daemon bug. A backend cannot import internal/rig to reach that sentinel, and
// a plain fmt.Errorf lands in the 500 catch-all.
//
// Wrap the refusals a client can provoke by asking for something reasonable.
// Do not wrap a programming error — a bad poll tier, a malformed internal
// request — which really is a 500.
var ErrUnsupported = errors.New("backend: unsupported by this radio")

// Key correlates a decoded frame with the request that asked for it.
//
// Backends choose their own key space, so long as it is stable: the Kenwood
// backend uses the command letters ("FA", "IF", "KY"), the CI-V backend uses
// "cmd" or "cmd/sub" in hex ("03", "14/0A"). The session compares keys by
// equality only.
type Key string

// KeyUnsolicited is the key for a frame that answers no request — a Transceive
// broadcast or an AI push. The session never matches it against a pending
// request; it only applies the patch.
const KeyUnsolicited Key = ""

// Update is the meaning the backend extracted from one complete frame.
type Update struct {
	// Key identifies what the frame is a reply to. Empty for unsolicited
	// frames, which are applied to state but match no pending request.
	Key Key
	// Patch carries whatever state fields the frame reported. May be empty:
	// a bare acknowledgement (Icom FB) says nothing about state.
	Patch radio.Patch
	// OK is false when the frame is a negative acknowledgement (Icom FA).
	// The session turns a matched NAK into an error for the caller.
	OK bool
	// Raw is the frame as received, retained for logging and tests.
	Raw []byte
}

// Conn is the transaction interface the session hands to a backend.
//
// Implementations serialise concurrent callers, so a backend method may assume
// its own requests do not interleave with another's.
type Conn interface {
	// Do writes req and blocks until a decoded frame carries one of want, the
	// context is done, or the session's per-command timeout elapses. Passing no
	// keys is an error; use Send for fire-and-forget.
	Do(ctx context.Context, req []byte, want ...Key) (Update, error)

	// Send writes req and returns as soon as it is on the wire. Use it for
	// commands the rig does not answer — Kenwood TX;/RX; are silent unless AI
	// happens to be on, so waiting for a reply would stall until timeout.
	Send(ctx context.Context, req []byte) error
}

// PollTier selects which set of values a Poll call should refresh.
type PollTier int

const (
	// PollFast covers values that change under the operator's hand: frequency,
	// mode, PTT, meters. On Kenwood this is a single IF; plus SM0;.
	PollFast PollTier = iota
	// PollSlow covers values that rarely move: power, filter width and slot.
	PollSlow
)

// Rig is one radio protocol. Implementations must be safe for concurrent
// method calls only in the sense that they hold no mutable state of their own;
// the session guarantees commands do not overlap on the wire.
type Rig interface {
	// Caps describes what this radio supports. Called after Init, so a backend
	// may refine it from what the rig reported (firmware version, sub receiver).
	Caps() radio.Caps

	// Init runs once per connection, before any polling. This is where a
	// backend enables push updates — Kenwood AI2;, Icom Transceive — and reads
	// anything it needs to size later requests.
	Init(ctx context.Context, c Conn) error

	// Split is a bufio.SplitFunc over the inbound byte stream. It must handle
	// partial frames by returning (0, nil, nil) to ask for more data, and must
	// resynchronise rather than error on garbage, since a rig powering up emits
	// noise.
	Split(data []byte, atEOF bool) (advance int, token []byte, err error)

	// Decode turns one complete frame from Split into an Update. It must not
	// error on frames it does not recognise; return an Update with
	// KeyUnsolicited and an empty Patch so unknown traffic is ignored quietly.
	Decode(frame []byte) (Update, error)

	// Poll refreshes one tier of state. The session applies patches from the
	// replies automatically, so Poll returns only an error.
	Poll(ctx context.Context, c Conn, tier PollTier) error

	SetFrequency(ctx context.Context, c Conn, vfo radio.VFO, hz uint64) error
	SetMode(ctx context.Context, c Conn, m radio.Mode, dataMode bool) error
	SetPower(ctx context.Context, c Conn, p radio.PowerSet) error
	SetPTT(ctx context.Context, c Conn, on bool) error
	SetFilterWidth(ctx context.Context, c Conn, hz int) error
	SetFilterSlot(ctx context.Context, c Conn, slot int) error
}

// ReplyFramer is implemented by a backend that cannot frame the inbound stream
// from its bytes alone, and has to know what was asked for instead.
//
// Every other protocol here delimits itself: Kenwood and Yaesu ASCII end each
// frame with ';', CI-V wraps one in FE FE … FD. Yaesu's five-byte binary CAT
// does neither. Its answers are one byte or five, carry no opcode, no length
// and no terminator, and nothing in an answer says which of the two it is — so
// the only thing that can tell the reader where a frame ends is the command
// that provoked it.
//
// Implementing this interface is what lets a backend record that. The session
// calls Expect immediately before req goes on the wire, holding the same lock
// that serialises writes, which is the point: a backend that stored the fact
// itself would have to do so before calling Do, outside that lock, and two
// concurrent callers — the poller and an HTTP setter, which is the ordinary
// case — could then store in one order and write in the other. The reader would
// frame the wrong number of bytes and the stream would never recover.
//
// A backend whose framing is self-contained does not implement this and is not
// called.
type ReplyFramer interface {
	// Expect records that req is about to be written, so Split can size the
	// answer. It is called with the write lock held and must not block, take a
	// lock of its own, or perform I/O — the reader goroutine is concurrently
	// inside Split reading whatever it stored.
	Expect(req []byte)
}

// MorseSender is implemented by backends whose rig has a CAT CW buffer.
//
// The pacing loop lives in internal/cw and is shared; this interface is only
// the rig-specific parts of it.
type MorseSender interface {
	// MaxChunk is the largest text block one command can carry: 30 on the
	// IC-7610 (command 17), 24 on Kenwood (KY, fixed width).
	MaxChunk() int

	// Charset is the set of characters the rig will key, as a plain string of
	// allowed runes. The API validates against it and rejects anything else,
	// because the rigs silently mangle unsupported characters rather than
	// reporting an error.
	Charset() string

	// BufferFree reports how many characters the rig will currently accept.
	// ok is false when the rig cannot be asked — the IC-7610 has no buffer
	// query — in which case the pacing loop falls back to time estimation.
	//
	// Kenwood answers KY0; (space available) or KY1; (full). Writing to a full
	// Kenwood buffer is a hard error, so the closed loop must be respected.
	BufferFree(ctx context.Context, c Conn) (free int, ok bool, err error)

	// SendChunk queues one chunk, already prosign-encoded and no longer than
	// MaxChunk. Any fixed-width padding is the backend's business.
	SendChunk(ctx context.Context, c Conn, text string) error

	// Abort stops the rig sending and discards whatever is in its buffer.
	Abort(ctx context.Context, c Conn) error

	// SetSpeed sets the rig's keyer speed in words per minute.
	SetSpeed(ctx context.Context, c Conn, wpm int) error

	// EncodeProsigns rewrites canonical "^AR" style prosigns into the rig's own
	// encoding, and returns an error naming any it cannot represent.
	//
	// The two target rigs differ completely here: Icom uses "^" as a
	// run-together marker, while Kenwood substitutes single ASCII symbols
	// (_ = AR, [ = BT, > = SK, ] = KN, < = AS, \ = BK, # = HH, % = SN).
	EncodeProsigns(text string) (string, error)
}

// SubReceiver is implemented by backends with a second receiver, such as the
// IC-7610. Not required for v1.
type SubReceiver interface {
	SetSubFrequency(ctx context.Context, c Conn, hz uint64) error
	SetSubMode(ctx context.Context, c Conn, m radio.Mode) error
}

// Factory builds a Rig from its radio configuration.
type Factory func(r *config.Radio) (Rig, error)

var (
	mu        sync.RWMutex
	factories = map[string]Factory{}
)

// Register makes a backend available by name. It is intended to be called from
// a package init function and panics on a duplicate name, since that can only
// be a programming error.
func Register(name string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := factories[name]; dup {
		panic("backend: duplicate registration for " + name)
	}
	factories[name] = f
}

// New builds the backend named by r.Backend.
func New(r *config.Radio) (Rig, error) {
	mu.RLock()
	f, ok := factories[r.Backend]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("backend: unknown backend %q for radio %q (known: %v)",
			r.Backend, r.ID, Registered())
	}
	return f(r)
}

// Registered lists the registered backend names, sorted.
func Registered() []string {
	mu.RLock()
	defer mu.RUnlock()
	names := make([]string, 0, len(factories))
	for n := range factories {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
