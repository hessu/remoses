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

// VFOModeSelector is implemented by a backend that can take the radio out of
// memory mode and back onto a VFO.
//
// It is deliberately the only part of memory mode remoses implements. Modelling
// channels would mean a channel list, a channel-select command and a state
// field for which one is active; what an operator actually needs from a daemon
// is the way out, because a rig left on a memory channel reports readings that
// several commands answer NG for and that nothing in this API can move.
//
// Separate from DualVFO because the two are independent: a single-VFO radio can
// still be in memory mode, and a dual-VFO one need not offer an A/B switch.
type VFOModeSelector interface {
	// SelectVFOMode returns the radio to VFO operation. VFOCurrent means "leave
	// memory mode, whichever VFO that lands on"; naming A or B also selects
	// that VFO, on radios that have such a command, and is refused where the
	// two VFOs are fixed receivers with no switch between them.
	SelectVFOMode(ctx context.Context, c Conn, vfo radio.VFO) error
}

// CapsAtConnect is implemented by a backend that cannot describe the radio
// until it has talked to it.
//
// Every native backend here builds Caps from a model profile, so it can answer
// before anything is plugged in. rigctld cannot: what the rig can do comes from
// the daemon's own \dump_state and \dump_caps at connect, and until then Caps
// describes nothing.
//
// That matters because the daemon validates some configuration against Caps at
// startup, and a capability that is merely *not yet known* must not be read as
// one the radio does not have. It cost the rigctld backend its CW support:
// `cw.method: cat` was refused for every rigctld radio, including the ones
// whose Hamlib backend reports a keyer, because the check ran before the first
// dial. Where this reports false the check is deferred and the backend enforces
// the capability when the command is actually issued — which it must do anyway,
// since Go interfaces are satisfied statically and a type cannot gain or lose
// MorseSender at run time.
type CapsAtConnect interface {
	// CapsKnown reports whether Caps() describes the radio yet.
	CapsKnown() bool
}

// BandExchanger is implemented by a backend that can swap the contents of a
// radio's two receivers, so that what the sub receiver was holding becomes the
// one everything else here operates.
//
// Separate from DualVFO, which addresses the two VFOs *within* the receiver
// remoses operates. This is one level up: it moves which band that receiver is
// on. A radio can have either without the other.
//
// It exists because an IC-9700 has a band its owner cannot reach remotely at
// all — see Caps.BandExchange — and it is an exchange rather than a "select the
// sub band" for a reason that radio makes concrete. Selecting the sub band
// does work there: the operating-frequency commands follow the selection, as
// touching the display's sub field shows. But that radio's per-VFO commands are
// documented "(Only MAIN band)" and keep addressing Main, so with the sub band
// selected State.Frequency describes one receiver while State.VFOA and VFOB
// describe the other. Exchanging leaves the selection on Main, and every
// command remoses uses then describes the same receiver.
type BandExchanger interface {
	// ExchangeBands swaps the two receivers. Everything the session knows about
	// the radio is stale afterwards — frequency, mode, both filters and both
	// VFOs are all a different band's — so the caller re-reads in full rather
	// than patching what it thinks changed.
	ExchangeBands(ctx context.Context, c Conn) error
}

// FrontEndController is implemented by a backend that can work the receive
// front end: the preamplifier, the attenuator, the RF gain and the AGC.
//
// The four travel together because the radios treat them as one panel and an
// operator uses them as one — turn the preamp off, wind the attenuator in, back
// the RF gain off — but each method may still refuse individually with
// ErrUnsupported, because plenty of sets have three of the four. What a given
// radio accepts is in its Caps: PreampLevels, AttenuatorDB, RFGainControl and
// AGCSettings.
//
// Reading is not part of the interface. All four are ordinary polled values
// that arrive as patches from the slow tier, so the session never needs to ask
// a backend for one.
type FrontEndController interface {
	// SetPreamp selects preamplifier 0 (off) to Caps.PreampLevels.
	SetPreamp(ctx context.Context, c Conn, level int) error
	// SetAttenuator sets attenuation in dB, 0 for switched out. The value is
	// one of Caps.AttenuatorDB, in the radio's own dB rather than a step index,
	// because no two families step the same way.
	SetAttenuator(ctx context.Context, c Conn, db int) error
	// SetRFGain sets the receiver RF gain, 0-100%. Backends scale to whatever
	// their radio counts in — 0-255 on most, 0-100 on a TS-480.
	SetRFGain(ctx context.Context, c Conn, pct float64) error
	// SetAGC sets the AGC speed. v is one of Caps.AGCSettings, and never one of
	// the auto-resolved readings, which the radios report but will not accept.
	SetAGC(ctx context.Context, c Conn, v radio.AGC) error
}

// PreselectController is implemented by a backend whose radio has Icom's
// front-end extras: IP+ and the DIGI-SEL preselector.
//
// Separate from FrontEndController because these are one manufacturer's, and
// not even all of that manufacturer's: IP+ belongs to the direct-sampling sets
// and DIGI-SEL to the big superhets and the IC-7610. A backend implements this
// only where the model actually has them, and Caps says which.
type PreselectController interface {
	// SetIPPlus switches Icom's IP+ intermodulation-rejection mode.
	SetIPPlus(ctx context.Context, c Conn, on bool) error
	// SetDigiSel switches the DIGI-SEL preselector in or out of line.
	SetDigiSel(ctx context.Context, c Conn, on bool) error
	// SetDigiSelShift moves the preselector's centre, 0-100%.
	//
	// It is accepted while the preselector is switched out: the radio keeps the
	// setting, and refusing would make a client sequence two controls that the
	// radio itself does not care about the order of.
	SetDigiSelShift(ctx context.Context, c Conn, pct float64) error
}

// NoiseController is implemented by a backend that can work the receiver's
// noise processing and its notch filters.
//
// The two blankers and the two notches travel together because they are the
// same panel and the same job — getting an unwanted signal out of the passband
// — but each method may refuse individually with ErrUnsupported. Caps says what
// a radio has: NoiseBlankerLevels, NoiseReductionLevels, NotchControl,
// NotchFreqControl, NotchWidths and AutoNotchControl.
//
// SetNotch and SetAutoNotch are separate entry points even though one family
// spells them as a single selector, because on the other two they are
// genuinely independent and can both be on. A backend whose radio cannot hold
// both says so with Caps.NotchExclusive, and the session refuses the
// combination rather than each backend inventing a resolution.
type NoiseController interface {
	// SetNoiseBlanker selects blanker 0 (off) to Caps.NoiseBlankerLevels.
	SetNoiseBlanker(ctx context.Context, c Conn, level int) error
	// SetNBLevel sets the blanker's threshold, 0-100%.
	SetNBLevel(ctx context.Context, c Conn, pct float64) error
	// SetNoiseReduction selects reducer 0 (off) to Caps.NoiseReductionLevels.
	SetNoiseReduction(ctx context.Context, c Conn, level int) error
	// SetNRLevel sets the reducer's strength, 0-100%.
	SetNRLevel(ctx context.Context, c Conn, pct float64) error

	// SetNotch switches the manual notch, SetNotchFreq parks it (0-100% of the
	// radio's own range) and SetNotchWidth chooses how wide it bites.
	SetNotch(ctx context.Context, c Conn, on bool) error
	SetNotchFreq(ctx context.Context, c Conn, pct float64) error
	SetNotchWidth(ctx context.Context, c Conn, w radio.NotchWidth) error
	// SetAutoNotch switches the automatic notch.
	SetAutoNotch(ctx context.Context, c Conn, on bool) error
}

// AntennaSelector is implemented by a backend that can choose which antenna
// socket the radio is using.
//
// No Icom implements it, and that is a statement about those radios rather
// than a gap here: they keep the antenna as a per-band MEMORY in the Set menu
// instead of offering a live selector, so switching would mean writing a stored
// setting. See radio.State.Antenna.
type AntennaSelector interface {
	// SetAntenna selects a socket, counting from 1 to Caps.Antennas.
	SetAntenna(ctx context.Context, c Conn, n int) error
	// SetRXAntenna switches the separate receive-only input in or out.
	SetRXAntenna(ctx context.Context, c Conn, on bool) error
}

// TXAudioController is implemented by a backend that can work the transmit
// audio chain: the gain into the modulator, and the speech processor.
//
// The two travel together because they are one panel and one job — getting the
// right amount of audio into the transmitter — but each method may refuse
// individually with ErrUnsupported. Caps says what a radio has:
// TXAudioGainControl, ProcControl and ProcLevelControl.
//
// What this interface deliberately does NOT model is which connector the audio
// comes from. On most of these radios it is a menu item rather than a live
// command — a TS-590S keeps it in menu 069 — and a "mic gain" written while the
// radio is taking audio from USB adjusts the USB input on some models and the
// microphone on others. Naming the gain after the socket would be a promise
// remoses cannot keep, so the field is the transmit gain and no more. See
// radio.State.MicGain.
//
// It is not universally a menu, though, and that is worth knowing before the
// selection is modelled: the TS-890S and TS-990S carry MS, a live readable and
// writable selector for the transmission audio route. So the gap here is a
// decision not to model the selection yet, not an absence of any command
// anywhere.
//
// Reading is not part of the interface, as with the front end: all three are
// ordinary polled values that arrive as patches from the slow tier.
type TXAudioController interface {
	// SetTXAudioGain sets the transmit audio gain, 0-100%. Backends scale to
	// whatever their radio counts in — 0-255 on Icom, 0-100 on Kenwood.
	SetTXAudioGain(ctx context.Context, c Conn, pct float64) error
	// SetProc switches the speech processor.
	SetProc(ctx context.Context, c Conn, on bool) error
	// SetProcLevel sets the processor's level, 0-100%.
	//
	// The session applies it after SetProc in a request carrying both, because
	// a radio that refuses the level while the processor is off would other-
	// wise reject switching it on and setting it in one go.
	SetProcLevel(ctx context.Context, c Conn, pct float64) error
}

// BreakInController is implemented by a backend that can read and set the CW
// break-in setting.
//
// It is not a convenience. On an Icom, a CW message sent over CAT transmits
// only "if the [TRANSMIT] or an external TX switch is ON, or the Break-in
// function is ON" — so with break-in off, command 17 is accepted, the buffer
// drains on schedule and nothing goes on the air. A rig that can report this
// lets the session refuse to send Morse into silence, which is the whole
// reason the interface exists rather than being one more setter.
type BreakInController interface {
	SetBreakIn(ctx context.Context, c Conn, v radio.BreakIn) error
	// BreakIn is the last reading, from the poll rather than a fresh
	// transaction: the CW path consults it on every send and must not add a
	// round trip to the keying path.
	BreakIn() radio.BreakIn
}

// PowerSwitch is implemented by a backend that can switch the radio itself off
// and on.
//
// It is the one control here whose success looks exactly like a failure. A
// radio told to switch off stops answering, so the poll that follows times out
// and the session tears the link down — which is correct, and indistinguishable
// from a pulled cable. The session therefore treats a power-off it issued as an
// expected disconnection rather than a fault; see Session.PowerOff.
//
// Waking one up is the mirror image: the command has to go out on a link that
// is not up, to a radio that is not listening in the ordinary way. Each family
// has its own ritual for getting a sleeping CI-V or CAT circuit's attention,
// which is why PowerOn is a method rather than a value passed to a setter.
type PowerSwitch interface {
	// PowerOn wakes the radio. One method, whatever the radio needs.
	//
	// A backend tries the cheap wake first and escalates to whatever ritual its
	// family documents — a Kenwood's dummy byte, a wait and a retry inside two
	// seconds — only if the cheap one draws nothing. Callers should not have to
	// know which kind of off a radio was put into, least of all when it was the
	// front-panel switch that did it.
	//
	// It is called on a freshly opened port BEFORE Init, because a sleeping
	// radio cannot answer Init: see the wake path in the session's supervisor.
	PowerOn(ctx context.Context, c Conn) error
	// PowerOff switches the radio off. deep asks for the lowest standby current
	// the radio offers, where it offers a choice.
	//
	// The default is deliberately NOT the deepest. A Kenwood's plain off draws
	// more current but wakes on a bare PS1;, where its low-current off wants a
	// dummy byte and a two-second window — and a remote station that cannot be
	// woken is a station somebody has to drive to. A radio with only one off
	// sends it either way; the distinction is honoured where it exists rather
	// than refused where it does not.
	PowerOff(ctx context.Context, c Conn, deep bool) error
}

// TunerController is implemented by a backend that can work the rig's internal
// antenna tuner.
//
// The two halves are separate because they are different in kind. SetTuner
// switches the matching network in or out of line and is an ordinary setting;
// StartTune KEYS THE TRANSMITTER, briefly and without anyone holding a switch,
// while the radio hunts for a match. The session treats the second as a
// transmit operation — lock, band limits, dead-man timer — and the first as a
// setting, which it could not do if they shared an entry point.
type TunerController interface {
	// SetTuner puts the tuner in line or bypasses it.
	SetTuner(ctx context.Context, c Conn, on bool) error
	// StartTune begins a tuning cycle. It transmits.
	//
	// It returns as soon as the radio has accepted the command, not when the
	// cycle finishes: the rig decides how long that takes and reports progress
	// through the tuner state, which the poller follows to radio.TunerTuning
	// and back.
	StartTune(ctx context.Context, c Conn) error
	// Tuner is the last reading, from the poll rather than a fresh
	// transaction, so that the session can answer "is it still tuning" without
	// a round trip.
	Tuner() radio.Tuner
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

// DualVFO is implemented by a backend whose radio can address each VFO by name
// rather than only "the one it is on", and report both at once.
//
// The distinction is not that the radio has two VFOs — nearly all of them do —
// but that the protocol can *reach* the second one without selecting it. An
// Icom's commands 03 and 05 act on whichever VFO is operating, so reaching the
// other means selecting it, which changes what the operator is using and races
// the front panel; commands 25 and 26 name the VFO in the frame instead. Where
// a radio has only the former, remoses controls the operating VFO and refuses
// to call it A or B, and that backend does not implement this.
//
// Caps is what a client reads to know: VFOs lists what may be named, and Split,
// DualWatch and PerVFOMode describe the rest. A backend implementing this must
// set them, because the type assertion is invisible from outside the daemon.
type DualVFO interface {
	// ReadVFOs refreshes both VFOs, split and dual watch. Called at connect and
	// on the slow poll; the session applies the patches, as with Poll.
	ReadVFOs(ctx context.Context, c Conn) error

	// SetVFOFrequency and SetVFOMode address one VFO by name. VFOCurrent is not
	// a valid argument: a caller reaching these is naming a VFO deliberately,
	// and resolving "current" would make a request about one act on another.
	//
	// slot 0 on SetVFOMode means "keep the filter that VFO has". It is a
	// distinct case rather than a default because the underlying command may
	// have no encoding for "leave it alone" — Icom's 26 reads a short frame as
	// "data off and the mode's default filter" — so preserving it takes a read.
	SetVFOFrequency(ctx context.Context, c Conn, vfo radio.VFO, hz uint64) error
	SetVFOMode(ctx context.Context, c Conn, vfo radio.VFO, m radio.Mode, dataMode bool, slot int) error

	// SetSplit moves transmit to the other VFO. Of everything in this
	// interface it is the one that changes where RF comes out, so the session
	// reads it back rather than assuming it took.
	SetSplit(ctx context.Context, c Conn, on bool) error

	// SetDualWatch receives on both VFOs at once. Only while it is on does a
	// second receiver's meter mean anything.
	SetDualWatch(ctx context.Context, c Conn, on bool) error
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
