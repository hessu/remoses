// Package rigctld speaks the TCP line protocol of Hamlib's rigctld daemon.
//
// This is remoses' long-tail escape hatch (DESIGN §2.1): rigctld runs as a
// separate process and talks to the radio, so every rig Hamlib knows becomes
// reachable without cgo, without libhamlib at build or run time, and without
// Hamlib's LGPL linking obligations.
//
// # Extended response mode
//
// Every command is prefixed with '+'. The man page calls this the Extended
// Response Protocol, and it is the difference between a parseable protocol and
// a guessing game: without it rigctld answers a query with bare positional
// values and answers a set command with nothing at all, so a reply carries no
// evidence of what it is a reply to.
//
// With '+' the daemon writes, and this backend has verified against
// tests/rigctl_parse.c in the Hamlib source:
//
//	get_freq:              <- echo: long command name, colon, the arguments
//	Frequency: 14074000    <- zero or more values
//	RPRT 0                 <- the Hamlib return code, 0 for success
//
// The echo is printed before the command runs (rigctl_parse.c, the
// `interactive && *ext_resp_ptr && !prompt` block), so it is present even when
// the command fails, and RPRT terminates the block either way.
//
// Two details are easy to get wrong and are worth stating:
//
//   - The '+' is consumed per command. rigctl_parse.c clears ext_resp as soon as
//     it prints RPRT, so every single command must carry the prefix.
//   - get_level does NOT label its value. Its `fprintf(fout, "%s: ", cmd->arg2)`
//     is guarded by `interactive && prompt`, which is rigctl's own terminal
//     prompt, not a rigctld socket. So `+l RFPOWER` answers with a bare number
//     between the echo and the RPRT, and the echo is the only thing that says
//     which level it belongs to. That is why the level name is part of the
//     correlation key.
//
// # Framing
//
// Split returns one whole response block — echo through RPRT — as a single
// frame, rather than one line at a time. Line framing cannot work here: the
// RPRT line does not name its command, so a line-framed decoder would have to
// carry state between frames to know what the preceding values meant, and a
// transaction would complete on the echo line before its values had arrived.
// Block framing keeps Decode a pure function of one frame, which is what the
// backend contract is for.
//
// # VFO mode
//
// rigctld is deliberately NOT run with -o. In VFO mode every command grows a
// mandatory VFO argument and the echo changes shape with it; without it, all
// commands address whatever VFO the rig is on. remoses therefore exposes
// VFOCurrent only, and says so in Caps rather than pretending VFO A means
// something here.
//
// # Honesty about capabilities
//
// This backend serves rigs nobody has tested against remoses, so Caps is built
// from what the daemon reports (\dump_state, \dump_caps) and nothing else.
// Where a capability cannot be established it is reported absent: a client that
// hides a control it could have shown is a nuisance, whereas one that offers a
// control the rig will reject looks broken.
package rigctld

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/radio"
	"github.com/hessu/remoses/internal/rig/backend"
)

// Name is the backend's registered name, matching `backend: rigctld` in the
// configuration file.
const Name = "rigctld"

func init() {
	backend.Register(Name, func(r *config.Radio) (backend.Rig, error) {
		return New(r)
	})
}

// Requests, spelled out once so a typo cannot hide in a format string. Each
// carries its own '+': the daemon clears extended-response mode after every
// RPRT, so the prefix is per command and not per connection.
const (
	reqDumpState = "+\\dump_state\n"
	reqDumpCaps  = "+\\dump_caps\n"
	reqGetFreq   = "+f\n"
	reqGetMode   = "+m\n"
	reqGetPTT    = "+t\n"
	reqStopMorse = "+\\stop_morse\n"
)

// Compile-time proof that the backend satisfies both contracts. MorseSender is
// satisfied statically but only *usable* when the rig reports send_morse; see
// Caps and morse.go.
var (
	_ backend.Rig         = (*Rig)(nil)
	_ backend.MorseSender = (*Rig)(nil)
)

// Rig is the Hamlib rigctld client backend. Construct it with New.
//
// Everything learned at Init lives in atomics rather than plain fields: Caps
// and Decode run on the session's reader goroutine while Init, Poll and the
// setters run on the command goroutine, and the backend contract forbids a
// lock.
type Rig struct {
	// address is the daemon's host:port, carried only for error messages; the
	// session owns the transport that dials it.
	address string

	// caps is published once by Init. Nil until then, which Caps reports as a
	// radio that can do nothing rather than as a radio that can do everything.
	caps atomic.Pointer[radio.Caps]

	// modeName is the last Hamlib mode token seen, verbatim. It is kept as the
	// token rather than as a radio.Mode because rigctld has no set-passband
	// command: changing the filter width means re-sending set_mode with the
	// mode the rig is already in, and a mode remoses cannot name (WFM, DSTAR)
	// must still round-trip unchanged.
	modeName atomic.Pointer[string]

	// modeList is the set of mode tokens dump_state reported, so SetMode can
	// refuse a mode the rig does not have with a message naming the ones it
	// does. Nil means dump_state said nothing and every mode is worth trying.
	modeList atomic.Pointer[[]string]

	// disabled is a bitmask of poll items the rig has refused as unsupported.
	// Without it a rig lacking, say, get_ptt would fail every fast poll, and
	// maxPollFailures consecutive failures tear the connection down.
	disabled atomic.Uint32

	// transmitting is the last get_ptt reading. The transmit meters are polled
	// only while it is set, since forward power, SWR and ALC all mean nothing
	// in receive.
	transmitting atomic.Bool

	// sendMorse and stopMorse record what \dump_caps said about CW keying.
	sendMorse atomic.Bool
	stopMorse atomic.Bool

	// keyerMin and keyerMax bound KEYSPD, from level_gran when the daemon
	// supplies it.
	keyerMin atomic.Int32
	keyerMax atomic.Int32

	// model and modelName identify the rig for operator-facing messages.
	model     atomic.Int32
	modelName atomic.Pointer[string]
}

// New builds the backend from a radio's configuration.
//
// It does not dial anything and it does not launch rigctld: the session owns
// the transport, and spawning is main's business through Spawn. A backend that
// started a process would own a goroutine, which the contract forbids.
func New(r *config.Radio) (*Rig, error) {
	g := &Rig{}
	if r != nil && r.Rigctld != nil {
		g.address = r.Rigctld.Address
	}
	if g.address == "" {
		return nil, fmt.Errorf("rigctld: radio %q has no rigctld.address", radioID(r))
	}
	g.keyerMin.Store(defaultMinWPM)
	g.keyerMax.Store(defaultMaxWPM)
	return g, nil
}

func radioID(r *config.Radio) string {
	if r == nil {
		return ""
	}
	return r.ID
}

// Model reports the rig behind the daemon: the model name from \dump_caps, or
// the numeric Hamlib model id from \dump_state when the name is unavailable.
func (g *Rig) Model() string {
	if s := g.modelName.Load(); s != nil && *s != "" {
		return *s
	}
	if n := g.model.Load(); n != 0 {
		return fmt.Sprintf("Hamlib model %d", n)
	}
	return ""
}

// Caps describes what the daemon said this rig can do.
//
// Before Init it reports an empty capability set. That is the honest answer —
// nothing has been asked yet — and it is also the safe one: an empty Modes list
// makes SupportsMode false, so a client cannot be told a mode is available
// before anything has confirmed it.
func (g *Rig) Caps() radio.Caps {
	if c := g.caps.Load(); c != nil {
		return *c
	}
	return radio.Caps{VFOs: []radio.VFO{radio.VFOCurrent}, CWMethod: radio.CWNone}
}

// CapsKnown implements backend.CapsAtConnect: it reports whether Init has run
// and Caps therefore describes the radio rather than the empty placeholder
// above.
//
// The placeholder says CWNone, which is honest — nothing is known — and was
// read as "this radio cannot send Morse" by the daemon's startup check, so
// `cw.method: cat` was refused for every rigctld radio however capable. That is
// what this exists to prevent.
func (g *Rig) CapsKnown() bool { return g.caps.Load() != nil }

// Init interrogates the daemon and publishes Caps.
//
// \dump_state is the link check as well as the capability source. It is
// answered from Hamlib's own tables rather than from the radio — rigctl_parse.c
// allows it even when the rig is powered off — so a successful dump_state
// proves the daemon is there, not that the rig is. The reads that follow do
// that.
//
// \dump_caps is optional. It is the only place in the protocol that reports
// whether the rig can send Morse at all, but it is also the one response that
// can run to several kilobytes, so a failure to fetch or parse it costs the CW
// capability and nothing else.
func (g *Rig) Init(ctx context.Context, c backend.Conn) error {
	u, err := g.do(ctx, c, reqDumpState, keyDumpState)
	if err != nil {
		return fmt.Errorf("rigctld %s: reading rig capabilities: %w", g.address, err)
	}
	st := parseDumpState(bodyOf(u.Raw))

	var dc dumpCaps
	if u, err := g.do(ctx, c, reqDumpCaps, keyDumpCaps); err == nil {
		dc = parseDumpCaps(bodyOf(u.Raw))
	}

	g.publish(&st, &dc)

	// A first snapshot, so the state cache is populated before the poller's
	// first tick. Each read is tolerant in exactly the way Poll is: a rig that
	// cannot answer one of them is a rig with fewer features, not a failure.
	for _, item := range []pollItem{itemFreq, itemMode, itemPTT} {
		if err := g.pollItem(ctx, c, item); err != nil {
			return err
		}
	}
	return nil
}

// publish folds the two capability dumps into radio.Caps and stores everything
// else the backend needs from them.
func (g *Rig) publish(st *dumpState, dc *dumpCaps) {
	g.model.Store(int32(st.model))
	if name := dc.modelLabel(); name != "" {
		g.modelName.Store(&name)
	}
	if names := st.modeTokens(); len(names) > 0 {
		g.modeList.Store(&names)
	}
	if lo, hi, ok := st.keyerRange(); ok {
		g.keyerMin.Store(int32(lo))
		g.keyerMax.Store(int32(hi))
	}
	g.sendMorse.Store(dc.sendMorse)
	g.stopMorse.Store(dc.stopMorse)

	// A level the rig cannot report is not worth polling for. Seeding the
	// disabled mask from dump_state saves the first poll a guaranteed
	// rejection; anything dump_state got wrong is caught by Poll itself.
	if !st.hasGetLevel(levelSTRENGTH) {
		g.disable(itemStrength)
	}
	if !st.hasGetLevel(levelRFPOWER) {
		g.disable(itemPower)
	}
	// The transmit meters are the levels most often missing: plenty of Hamlib
	// backends report STRENGTH and none of these three.
	if !st.hasGetLevel(levelRFPOWERMETER) {
		g.disable(itemPowerMeter)
	}
	if !st.hasGetLevel(levelSWR) {
		g.disable(itemSWR)
	}
	if !st.hasGetLevel(levelALC) {
		g.disable(itemALC)
	}

	caps := buildCaps(st, dc, int(g.keyerMin.Load()), int(g.keyerMax.Load()))
	g.caps.Store(&caps)
}

// Split is a bufio.SplitFunc over the inbound stream. See splitBlocks.
func (g *Rig) Split(data []byte, atEOF bool) (int, []byte, error) {
	return splitBlocks(data, atEOF)
}

// pollItem names one thing a poll can refresh. The values double as bits in
// Rig.disabled.
type pollItem uint32

const (
	itemFreq pollItem = 1 << iota
	itemMode
	itemPTT
	itemStrength
	itemPower
	itemPowerMeter
	itemSWR
	itemALC
)

// pollRequests is the request and correlation key for each item.
var pollRequests = map[pollItem]struct {
	req string
	key backend.Key
	// name is what appears in an error, in the daemon's own vocabulary so an
	// operator can reproduce it with rigctl.
	name string
}{
	itemFreq:     {reqGetFreq, keyGetFreq, "get_freq"},
	itemMode:     {reqGetMode, keyGetMode, "get_mode"},
	itemPTT:      {reqGetPTT, keyGetPTT, "get_ptt"},
	itemStrength: {reqGetLevel(levelSTRENGTH), levelKey(cmdGetLevel, levelSTRENGTH), "get_level STRENGTH"},
	itemPower:    {reqGetLevel(levelRFPOWER), levelKey(cmdGetLevel, levelRFPOWER), "get_level RFPOWER"},
	itemPowerMeter: {reqGetLevel(levelRFPOWERMETER), levelKey(cmdGetLevel, levelRFPOWERMETER),
		"get_level RFPOWER_METER"},
	itemSWR: {reqGetLevel(levelSWR), levelKey(cmdGetLevel, levelSWR), "get_level SWR"},
	itemALC: {reqGetLevel(levelALC), levelKey(cmdGetLevel, levelALC), "get_level ALC"},
}

func (g *Rig) disable(item pollItem) { g.disabled.Or(uint32(item)) }

func (g *Rig) isDisabled(item pollItem) bool { return g.disabled.Load()&uint32(item) != 0 }

// Poll refreshes one tier of state.
//
// The fast tier is four transactions where the Kenwood backend needs two: there
// is no bulk status command that is safe to assume. \get_vfo_info would return
// frequency, mode and width together, but it arrived in Hamlib 4.1 and rigctld
// answers a command it does not know with complete silence rather than an
// error, so an older daemon would cost a full timeout on every poll. Four
// small commands cost four TCP round trips on a socket that is almost always
// on localhost, and exactly the same serial traffic to the radio, which is
// where the time actually goes.
func (g *Rig) Poll(ctx context.Context, c backend.Conn, tier backend.PollTier) error {
	var items []pollItem
	switch tier {
	case backend.PollFast:
		items = []pollItem{itemFreq, itemMode, itemPTT, itemStrength}
		// The transmit meters, and only while keyed. get_ptt is in the same
		// list and runs first, so this reflects the reading just taken.
		if g.transmitting.Load() {
			items = append(items, itemPowerMeter, itemSWR, itemALC)
		}
	case backend.PollSlow:
		items = []pollItem{itemPower}
	default:
		return fmt.Errorf("rigctld: unknown poll tier %d", tier)
	}
	for _, item := range items {
		if err := g.pollItem(ctx, c, item); err != nil {
			return err
		}
	}
	return nil
}

// pollItem runs one read, and permanently drops the item if the rig says it
// cannot do it.
//
// Dropping matters more here than on a backend written for one known radio.
// The poller kills the connection after maxPollFailures consecutive failures,
// so a rig without an S-meter — which is most VHF handhelds and a fair number
// of HF sets under Hamlib — would otherwise reconnect in a loop forever.
//
// Frequency is the exception: it is never dropped. Every Hamlib backend
// implements get_freq, so a rig that keeps refusing it is not a rig with fewer
// features, it is a link that has gone wrong, and the poller should be allowed
// to notice.
func (g *Rig) pollItem(ctx context.Context, c backend.Conn, item pollItem) error {
	if g.isDisabled(item) {
		return nil
	}
	p := pollRequests[item]
	if _, err := g.do(ctx, c, p.req, p.key); err != nil {
		var he *Error
		if item != itemFreq && errors.As(err, &he) && he.Unsupported() {
			g.disable(item)
			return nil
		}
		return err
	}
	return nil
}

// SetFrequency writes F.
//
// Only VFOCurrent is accepted. rigctld addresses a specific VFO only when it
// was started with -o, which remoses does not do (see the package doc), so
// accepting VFOA here would mean silently retuning whichever VFO the operator
// happened to be on and calling it A.
//
// There is no read-back. Unlike a Kenwood PC or FW, the frequency the rig takes
// differs from the one requested only by its tuning step, and the fast poll is
// half a second behind at worst — whereas a read-back doubles the cost of the
// one command a client sends continuously while dragging a VFO dial. The
// echoed set_freq argument gives State the new value immediately; see Decode.
func (g *Rig) SetFrequency(ctx context.Context, c backend.Conn, vfo radio.VFO, hz uint64) error {
	if vfo != radio.VFOCurrent {
		return fmt.Errorf("rigctld: this backend addresses the rig's current VFO only, not %s: "+
			"targeting a VFO needs rigctld's -o option, which changes the wire protocol", vfo)
	}
	_, err := g.do(ctx, c, fmt.Sprintf("+F %d\n", hz), keySetFreq)
	return err
}

// SetMode writes M with the mode token and a passband of 0.
//
// Zero is RIG_PASSBAND_NORMAL: it asks the backend for the mode's own default
// width rather than leaving the width unset. RIG_PASSBAND_NOCHANGE (-1) would
// keep the current width, which sounds tidier but carries the previous mode's
// filter into the new one — a 500 Hz CW filter surviving a switch to SSB.
//
// The read-back is not ceremony. The echo says what was asked for; only get_mode
// says which passband the rig actually landed on, and remoses publishes that.
func (g *Rig) SetMode(ctx context.Context, c backend.Conn, m radio.Mode, dataMode bool) error {
	token, err := encodeMode(m, dataMode)
	if err != nil {
		return err
	}
	if err := g.checkMode(token); err != nil {
		return err
	}
	if _, err := g.do(ctx, c, fmt.Sprintf("+M %s %d\n", token, passbandNormal), keySetMode); err != nil {
		return err
	}
	_, err = g.do(ctx, c, reqGetMode, keyGetMode)
	return err
}

// checkMode rejects a mode dump_state did not list, so the operator gets the
// available modes rather than a bare "RPRT -1" from a rig that was asked for
// something it has never had.
func (g *Rig) checkMode(token string) error {
	list := g.modeList.Load()
	if list == nil || len(*list) == 0 {
		return nil
	}
	for _, have := range *list {
		if have == token {
			return nil
		}
	}
	return fmt.Errorf("rigctld: %s has no %s mode (it reports: %s)",
		g.describe(), token, strings.Join(*list, " "))
}

func (g *Rig) describe() string {
	if s := g.Model(); s != "" {
		return s
	}
	return "this rig"
}

// SetPower writes L RFPOWER.
//
// Hamlib's RFPOWER is a normalised float from 0.0 to 1.0 with no watt meaning
// whatsoever (rig.h: "RF Power, arg float [0.0 ... 1.0]"), so a watt request is
// refused rather than converted. Hamlib does offer \mW2power for that
// conversion, but it needs the frequency and mode as arguments and returns
// whatever the rig's own power curve says, which is a different and much larger
// promise than remoses makes here.
//
// The read-back is worth its round trip: a rig with a coarse power control
// quantises the request, and Caps says the scale is not watt-accurate, so the
// percentage remoses reports had better be the one the rig took.
func (g *Rig) SetPower(ctx context.Context, c backend.Conn, p radio.PowerSet) error {
	v, err := levelFromPowerSet(p)
	if err != nil {
		return err
	}
	if _, err := g.do(ctx, c, fmt.Sprintf("+L %s %.3f\n", levelRFPOWER, v), levelKey(cmdSetLevel, levelRFPOWER)); err != nil {
		return err
	}
	_, err = g.do(ctx, c, reqGetLevel(levelRFPOWER), levelKey(cmdGetLevel, levelRFPOWER))
	return err
}

// SetPTT writes T.
//
// T 1 is RIG_PTT_ON, the rig's normal transmit input. RIG_PTT_ON_MIC (2) and
// RIG_PTT_ON_DATA (3) exist and pick which input modulates the transmitter,
// which is a decision a plain PTT flag does not carry.
//
// No read-back: the echoed set_ptt argument already gives State the new value
// the moment RPRT 0 arrives, and this is the command where latency is felt.
func (g *Rig) SetPTT(ctx context.Context, c backend.Conn, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := g.do(ctx, c, fmt.Sprintf("+T %d\n", v), keySetPTT)
	return err
}

// SetFilterWidth re-sends M with the current mode and the requested width.
//
// rigctld has no set-passband command; width is only ever the second argument
// of set_mode. That is why the backend keeps the last mode token: sending the
// width without it would change the mode as a side effect of changing the
// filter.
func (g *Rig) SetFilterWidth(ctx context.Context, c backend.Conn, hz int) error {
	if hz <= 0 {
		return fmt.Errorf("rigctld: filter width must be positive, have %d Hz", hz)
	}
	token := g.modeName.Load()
	if token == nil || *token == "" {
		return errors.New("rigctld: cannot set the filter width before the rig has reported its mode: " +
			"rigctld carries the passband only as the second argument of set_mode")
	}
	if _, err := g.do(ctx, c, fmt.Sprintf("+M %s %d\n", *token, hz), keySetMode); err != nil {
		return err
	}
	_, err := g.do(ctx, c, reqGetMode, keyGetMode)
	return err
}

// SetFilterSlot is not implementable over this protocol. Hamlib models the
// passband as a width in Hz and has no notion of the rig's filter slots, so
// there is nothing to map an Icom FIL1/FIL2 or a Kenwood IF Filter A/B onto.
// Caps reports FilterSlots as 0; this error is for a caller that ignored it.
func (g *Rig) SetFilterSlot(ctx context.Context, c backend.Conn, slot int) error {
	return fmt.Errorf("rigctld: Hamlib has no filter-slot command; slot %d cannot be selected through rigctld", slot)
}

// do runs one transaction and turns a rejection into an error naming the
// Hamlib code.
//
// The session already turns OK=false into ErrNAK, but it does so without the
// code, and the code is the whole message: -11 means the rig cannot do this at
// all, while -5 means it did not answer in time. Poll depends on telling those
// apart.
func (g *Rig) do(ctx context.Context, c backend.Conn, req string, want backend.Key) (backend.Update, error) {
	u, err := c.Do(ctx, []byte(req), want)
	if err == nil {
		return u, nil
	}
	if !u.OK && len(u.Raw) > 0 {
		if code, ok := blockCode(u.Raw); ok {
			return u, &Error{Command: label(req), Code: code}
		}
		return u, fmt.Errorf("rigctld: %s: response carried no RPRT line: %q", label(req), u.Raw)
	}
	return u, fmt.Errorf("rigctld: %s: %w", label(req), err)
}

// label renders a request the way an operator would type it into rigctl:
// without the extended-response '+' and without the terminating newline.
func label(req string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req), "+"))
}

// Error is a Hamlib return code carried by an RPRT line.
type Error struct {
	// Command is the request that provoked it, as an operator would type it.
	Command string
	// Code is the RPRT value: 0 is success, negative values are the negated
	// Hamlib error enum from rig.h.
	Code int
}

func (e *Error) Error() string {
	return fmt.Sprintf("rigctld rejected %q with RPRT %d (%s)", e.Command, e.Code, hamlibError(e.Code))
}

// Unsupported reports whether the code means "this radio cannot do that",
// as opposed to "that did not work this time".
//
// The list is Hamlib's own RIG_IS_SOFT_ERRCODE from rig.h, minus the two
// entries that are about the moment rather than the radio: ESECURITY is a
// missing rigctld password and EPOWER is a rig switched off, and both come back
// on their own. Everything left describes a function the backend does not have,
// a VFO it cannot target, or an argument it will never accept — none of which
// improve by being asked again every half second.
func (e *Error) Unsupported() bool {
	switch -e.Code {
	case errEINVAL, errENIMPL, errERJCTED, errETRUNC, errENAVAIL, errENTARGET, errEVFO, errEDOM, errEDEPRECATED:
		return true
	}
	return false
}

// The Hamlib error enum from include/hamlib/rig.h. RPRT carries the negation of
// these.
const (
	errEINVAL      = 1  // invalid parameter
	errECONF       = 2  // invalid configuration
	errENOMEM      = 3  // memory shortage
	errENIMPL      = 4  // function not implemented
	errETIMEOUT    = 5  // communication timed out
	errEIO         = 6  // IO error, including open failed
	errEINTERNAL   = 7  // internal Hamlib error
	errEPROTO      = 8  // protocol error
	errERJCTED     = 9  // command rejected by the rig
	errETRUNC      = 10 // command performed, but argument truncated
	errENAVAIL     = 11 // function not available
	errENTARGET    = 12 // VFO not targetable
	errBUSERROR    = 13 // error talking on the bus
	errBUSBUSY     = 14 // collision on the bus
	errEARG        = 15 // invalid pointer parameter
	errEVFO        = 16 // invalid VFO
	errEDOM        = 17 // argument out of domain
	errEDEPRECATED = 18 // function deprecated
	errESECURITY   = 19 // security error
	errEPOWER      = 20 // rig not powered on
)

var hamlibErrors = map[int]string{
	errEINVAL:      "invalid parameter",
	errECONF:       "invalid configuration, check the rigctld serial settings",
	errENOMEM:      "memory shortage in rigctld",
	errENIMPL:      "function not implemented for this rig",
	errETIMEOUT:    "the rig did not answer rigctld in time",
	errEIO:         "I/O error between rigctld and the rig",
	errEINTERNAL:   "internal Hamlib error",
	errEPROTO:      "protocol error between rigctld and the rig",
	errERJCTED:     "the rig rejected the command",
	errETRUNC:      "performed, but the argument was truncated",
	errENAVAIL:     "this rig's Hamlib backend does not have that function",
	errENTARGET:    "that VFO cannot be targeted",
	errBUSERROR:    "error talking on the rig's bus",
	errBUSBUSY:     "collision on the rig's bus",
	errEARG:        "invalid argument",
	errEVFO:        "invalid VFO",
	errEDOM:        "argument out of range",
	errEDEPRECATED: "function deprecated",
	errESECURITY:   "rigctld requires a password (started with -A)",
	errEPOWER:      "the rig is powered off",
}

// hamlibError names a code. An unknown one is reported as a number rather than
// guessed at: Hamlib adds to this enum, and a wrong name is worse than none.
func hamlibError(code int) string {
	if s, ok := hamlibErrors[-code]; ok {
		return s
	}
	if code == 0 {
		return "success"
	}
	return "unrecognised Hamlib error code"
}
