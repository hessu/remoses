# remoses — Design

**remoses** is a cross-platform Go daemon that exposes locally-connected amateur radio
transceivers over an authenticated HTTP/WebSocket API, so they can be operated remotely.

Initial targets: **Icom IC-7610** (CI-V) and **Kenwood TS-590S / TS-590SG** (ASCII CAT).
Long-tail rig support via Hamlib's `rigctld`.

Scope for v1 is **control only** — frequency, mode, filters, power, PTT, and CW sending.
Audio transport is explicitly out of scope and will be a separate concern.

---

## 1. Deployment model

One `remoses` instance runs on a server **physically next to the radios**, with each rig
connected by USB serial (or RS-232). Clients reach it over the network.

```
   remote client(s)                       radio site
  ┌──────────────┐                ┌──────────────────────────────┐
  │ browser /    │  HTTPS + WSS   │  remoses daemon              │
  │ CLI / logger │ ──────────────▶│    │                         │
  └──────────────┘                │    ├─ USB serial ─▶ IC-7610  │
                                  │    ├─ USB serial ─▶ TS-590SG │
                                  │    └─ USB serial ─▶ (keying) │
                                  └──────────────────────────────┘
```

This matters for CW: because remoses is local to the rig, **all CW element timing is
generated server-side**. Network latency and jitter between client and server affect only
how quickly text is queued, never the quality of the sent Morse.

---

## 2. Key decisions

| Decision | Choice | Rationale |
|---|---|---|
| Rig control library | **Native Go backends**, plus a `rigctld` client for the long tail | See §2.1 |
| Serial I/O | `go.bug.st/serial` | Maintained (v1.8.0, Jul 2026), cgo-free except macOS enumeration |
| Authorisation | **Per instance** — any authenticated user may use any radio | Small trusted station, not a multi-tenant service |
| Password hashing | bcrypt at **low cost (default 8)** + TTL verify-cache | Local trusted deployment; keeps polling cheap |
| Exclusive control | **Per-radio lock** with an opaque token, sliding TTL (default 30 s) | Prevents two operators fighting over one rig |
| State streaming | **WebSocket**, instance-wide, no lock required, many concurrent clients | Push is free from CI-V Transceive / Kenwood `AI2` |
| CW keying | CAT text buffers where available; **DTR/RTS local element generation** where not | §11 |
| cgo | Avoided | Single static binary per platform, cross-compiles to linux/arm64 for a Pi at the site |

### 2.1 Why not Hamlib bindings

There is no pure-Go port of Hamlib and there will not be one. The options were:

- **`dh1tw/goHamlib`** (cgo bindings) — last push Sep 2022, and it needs `libhamlib`
  at build *and* run time. That reintroduces the cgo cross-compilation and packaging
  problems we avoid everywhere else, and adds LGPL-2.1 static-linking obligations.
- **`rigctld` over TCP** — pure-Go client, separate process, so Hamlib's GPL/LGPL split
  is a non-issue. Kept as a **backend option** for rigs we have not implemented natively.
- **Native backends** — chosen for the primary rigs.

Native wins here for three reasons:

1. **Two dialects cover most of the fleet.** Icom CI-V covers essentially every Icom.
   Kenwood-style ASCII CAT (`FA;` / `MD;` / `IF;`) is spoken, with variations, by Kenwood,
   Elecraft (K3/K3S/K4/KX2/KX3), modern Yaesu (FT-991A, FTdx10, FTdx101, FT-710), and
   Flex SmartSDR CAT.
2. **CW needs deterministic serial transactions.** Hamlib's own developers note that
   `rig_send_morse()` can block the rig interface until it is queued, and that in-band
   speed changes are unsupported. We need to own the buffer-refill loop.
3. **Unsolicited frames are an asset.** Icom Transceive mode and Kenwood `AI2;` push state
   changes for free — front-panel knob movements appear without polling. A frame
   demultiplexer keeps that; a request/response abstraction discards it.

---

## 3. Architecture

```
                HTTP/REST                    WebSocket
                    │                            │
            ┌───────▼────────────────────────────▼───────┐
            │ api                                        │
            │  basic auth ─▶ lock check ─▶ handler       │
            └───────┬────────────────────────────▲───────┘
                    │ command (with ctx)         │ events
            ┌───────▼────────────────────────────┴───────┐
            │ rig.Manager        hub (fan-out to clients) │
            │   lock.Manager                              │
            └───────┬─────────────────────────────────────┘
                    │
       ┌────────────┴──────────────┬───────────────────────┐
       │  Session "ic7610"         │  Session "ts590sg"    │  …
       │                                                    │
       │   writer goroutine   reader goroutine   poller     │
       │   (serialises cmds)  (frames → demux)   (ticker)   │
       │   keyer goroutine (serial keying only)             │
       │                        │                            │
       │                   backend.Rig                       │
       │                civ │ kenwood │ rigctld              │
       │                        │                            │
       │            transport (serial, auto-reconnect)       │
       └─────────────────────────────────────────────────────┘
```

Two invariants carry most of the design:

**A. Exactly one goroutine owns each serial port.** Everything else submits requests over a
channel with a `context.Context`. No mutexes around the port, no interleaved half-frames.
(The one exception is modem-control lines for keying — see §11.2.)

**B. The reader never does request/response.** It continuously splits the byte stream into
frames and routes each one:

- matches a pending request → deliver to the waiting channel
- matches nothing → unsolicited transceive/AI update → fold into the state cache and publish

This gives correct behaviour for three otherwise awkward cases: CI-V bus echo (present on the
13-pin CI-V jack, absent over USB), Transceive/AI broadcasts, and a rig answering after its
request has already timed out.

### 3.1 State cache

Each session keeps the current radio state in an `atomic.Pointer[State]`. API reads and new
WebSocket subscribers are served from the snapshot and **never block on the serial port**.

```go
type State struct {
    Frequency  uint64     // Hz
    Mode       Mode       // CW, CW-R, USB, LSB, AM, FM, FSK, FSK-R, PSK…
    DataMode   bool       // Kenwood DA; orthogonal to Mode
    PassbandHz int
    FilterSlot int        // FIL1/2/3 on Icom; IF Filter A/B (FL) on Kenwood
    Power      Power
    PTT        bool
    SMeter     Meter
    SWR, ALC   *Meter
    CW         CWState    // busy, queued chars, wpm
    Connected  bool
    UpdatedAt  time.Time
    Seq        uint64     // monotonic per radio; used for WS gap detection
}
```

#### Power and meters are not naturally normalised

The two target rigs disagree about units, and pretending otherwise would lose information:

- **Kenwood `PC` is in watts** — `005`–`100` for SSB/CW/FM/FSK, `005`–`025` for AM,
  in 5 W steps unless the rig's Power Fine setting is on. Out-of-range values are clamped
  and off-step values rounded **down**.
- **Icom `14 0A` is a relative scale**, `0000`–`0255`, with no direct watt meaning.
- **Kenwood `SM` returns `0000`–`0030`** — literally "the number of dots displayed on the
  meter" — and reads the **RF power meter while transmitting**, not the S-meter.
- **Icom `15 02` returns `0000`–`0255`**.

Neither is dBm, and neither is a percentage. So both are modelled honestly, carrying the
native value plus a normalised convenience value:

```go
type Power struct {
    Watts  *float64  // nil when the rig has no watt-accurate scale (Icom)
    Pct    float64   // 0..100, normalised against the rig's max for the current mode
    Native int       // as sent on the wire
}

type Meter struct {
    Raw   int      // native value
    Scale int      // native maximum: 30 Kenwood, 255 Icom
    S     *float64 // S-units, only when a per-model calibration table exists
}
```

`PATCH /state` accepts **either** `power_w` or `power_pct` and rejects a request carrying
both. Responses always include `power` with all fields the backend can populate. Capability
flags advertise `power.watt_accurate` so clients know whether to show a watts control.
Config `limits` likewise accepts `max_power_w` or `max_power_pct`.

---

## 4. Configuration

```yaml
server:
  listen: "0.0.0.0:7342"
  base_path: /api/v1
  tls:
    cert_file: /etc/remoses/cert.pem
    key_file:  /etc/remoses/key.pem
  # insecure: true      # required to bind a non-loopback address without TLS

auth:
  realm: remoses
  bcrypt_cost: 8              # low by choice; 4..31, bcrypt default is 10
  cache_ttl: 60s              # verified-credential cache, keeps polling cheap
  users:
    - username: n0call
      password_bcrypt: "$2a$08$..."     # generate with: remoses passwd
    - username: guest
      password_bcrypt: "$2a$08$..."

lock:
  enabled: true
  ttl: 30s                    # sliding; renewed by any successful command
  allow_steal: false          # if true, force=true may take a held lock

ws:
  min_interval: 50ms          # per-radio coalescing floor for state events
  ping_interval: 30s
  send_queue: 256

radios:
  - id: ic7610                          # stable, URL-safe
    name: "Icom IC-7610"
    backend: civ
    port:
      device: /dev/tty.usbmodem14201
      match: { vid: "10C4", pid: "EA60", serial: "IC7610-001" }  # preferred over device
      baud: 115200
      # tcp: "192.168.1.50:4001"       # or reach the port over the network; see §4.1
    civ:
      model: ic-7610                    # see §5.4; sets the default address and mode set
      rig_address: 0x98                 # optional, overrides the model default
      controller_address: 0xE0
      echo: false                       # true when wired to the CI-V bus jack
      transceive: true
    poll:
      interval: 500ms                   # freq, mode, ptt, s-meter
      slow_interval: 5s                 # power, filter — rarely change
    # debug_wire: true                  # trace every CAT frame, hex + text; §6.1
    cw:
      enabled: true
      method: cat                       # rig has a CAT CW buffer (cmd 0x17)
      default_wpm: 28
    limits:
      max_power_pct: 80          # or max_power_w: on rigs with a watt-accurate scale
      tx_timeout: 120s
      bands: ["1.8-2.0MHz", "3.5-3.8MHz", "14.0-14.35MHz"]

  - id: ts590sg
    name: "Kenwood TS-590SG"
    backend: kenwood
    port: { device: COM7, baud: 115200 }
    kenwood:
      model: ts590sg                    # ts590s | ts590sg | elecraft-k3 | …
      auto_information: 2               # AI2; → push updates, self-clears at rig power-off
      bulk_poll: true                   # use IF; (38-char status) for the fast poll
    limits:
      max_power_w: 80                   # PC is in watts on Kenwood: 005..100 (005..025 AM)
      tx_timeout: 120s
    cw: { enabled: true, method: cat, default_wpm: 25 }

  - id: ft857
    name: "Yaesu FT-857 (hamlib)"
    backend: rigctld
    rigctld:
      address: "127.0.0.1:4532"
      spawn: true                       # remoses launches rigctld itself
      model: 1035
      device: /dev/ttyUSB1
    cw:
      enabled: true
      method: serial_key                # no usable CAT CW buffer
      default_wpm: 22
      serial_key:
        device: /dev/ttyUSB2            # may be the CAT port, separate is better
        key_line: dtr                   # dtr | rts
        ptt_line: rts                   # optional; omit for full break-in
        ptt_lead_ms: 15
        ptt_tail_ms: 150
        weight: 50                      # dit/dah weighting, %
```

Use **`goccy/go-yaml`** rather than `gopkg.in/yaml.v3` — actively maintained, and its errors
carry line/column, which matters when this file is hand-edited. Decode strictly, so a typo'd
key is an error rather than a silent default, and report *all* validation failures at once
with `errors.Join`: one error per run is hostile to someone editing by hand.

### 4.1 Serial ports over the network

`port.tcp: "host:port"` reaches a serial port published by a terminal server — ser2net, a
Digi or Moxa device server, an ESP32 bridge — instead of a local device. It works for **every**
serial backend, not just `rigctld`, so a rig on the far side of the shack LAN is configured
exactly like a local one.

It is mutually exclusive with `device` and `match`, and carries **bytes only**: this is plain
TCP, not RFC 2217. Two consequences follow, and both are enforced at startup rather than
discovered later:

- **Line settings live at the terminal server.** `baud`, `parity` and `stop_bits` are ignored,
  so remoses does not validate or apply them.
- **There are no modem control lines**, so a networked port cannot be keyed. `serial_key`
  therefore always needs its own local `cw.serial_key.device` — which is independent of how
  the rig is controlled, so keying a TCP- or rigctld-controlled radio through a local adapter
  is a perfectly good station.

The session's dial, backoff and reconnect logic is unchanged: a refused connection or a
vanished peer is reported as `transport.ErrDisconnected`, exactly like an unplugged USB
adapter, so a terminal server that reboots is handled by the same supervisor.

> **Defaulting needs a presence probe.** Several keys default to a non-zero value while zero is
> itself a legal, meaningful setting — `lock.enabled: false`, `auto_information: 0` (AI off),
> `cache_ttl: 0s` (cache disabled), `transceive: false`, `bulk_poll: false`, `tx_timeout: 0s`.
> A plain struct cannot distinguish "absent" from "explicitly zero", so naive defaulting would
> overwrite them and **"turn this off" would be inexpressible**. The loader makes a second
> lenient pass into a pointer struct covering exactly those keys.

---

## 5. Rig abstraction

A small mandatory core with capabilities as optional interfaces, so backends do not stub out
what a rig lacks and the API can honestly report what each radio supports.

```go
package backend

type Rig interface {
    Open(ctx context.Context, t transport.Transport) error
    Close() error
    Capabilities() Caps

    // Codec — driven by the session's reader goroutine.
    Split(data []byte, atEOF bool) (advance int, token []byte, err error)
    Decode(frame []byte) (Event, error)      // response OR unsolicited update

    GetState(ctx context.Context) (State, error)
    SetFrequency(ctx context.Context, vfo VFO, hz uint64) error
    SetMode(ctx context.Context, vfo VFO, m Mode, passband Hz) error
    SetPower(ctx context.Context, pct float64) error
    SetPTT(ctx context.Context, on bool) error
}

// Optional, discovered by type assertion:

type MorseSender interface {                 // rigs with a CAT CW buffer
    SendMorse(ctx context.Context, text string) error
    AbortMorse(ctx context.Context) error
    MorseBufferFree(ctx context.Context) (int, error)  // -1 = not queryable
    SetKeyerSpeed(ctx context.Context, wpm int) error
    MaxMorseChunk() int                      // 30 IC-7610, 24 Kenwood
    MorseCharset() string
}

type SubReceiver interface { … }             // IC-7610 dual RX
type Tuner       interface { … }
```

### Rule: the operator's radio is not ours to reconfigure

**remoses never writes a radio's persistent configuration in order to do its job.** If a
setting survives power-off, remoses may read it but must not set it.

The reason is that the alternative is invisible. A menu item written to make one command work
stays written after remoses disconnects, after the rig is power-cycled, and after the operator
goes back to using the radio by hand. They did not ask for the change, were not told about it,
and will find it much later as unexplained behaviour. Nothing remoses gains is worth that.

The test is one question: **would this still be changed after remoses is gone?** If yes, do not
write it. This has decided the same question independently on three manufacturers:

| Where | What remoses declines to write | What it costs |
|---|---|---|
| Icom `1A 05` | CI-V Transceive (Set-mode item `0112`) | Icoms are poll-only unless the operator enables Transceive in the menu; Kenwood gets push updates free |
| Yaesu `KM` | Keyer memories 1–5 | No CAT CW at all on any Yaesu — sending arbitrary text would mean overwriting the operator's stored macros, so `serial_key` is the only option |
| Yaesu `EX` | Menu items, e.g. 090 AMS TX MODE | On an FT-991A remoses can see the radio is in C4FM but not which sub-mode it will transmit in |

The rule is about *persistence*, not about writing generally — remoses sets frequency, mode and
power all day. A setting that reverts on its own is fine, and that distinction is why the
Kenwood backend defaults to `AI2` (auto-information, self-clears at rig power-down) rather than
the otherwise-identical `AI4` ("with backup", which persists).

Where the rule costs a capability, say so in the configuration and the docs rather than working
around it quietly: the operator can always enable the menu item themselves, and then remoses
picks up the benefit with no code change.

**The rule binds backends not yet written.** The table above is what it has cost so far, not a
list of the commands it applies to. Every manufacturer has its own way of writing the saved
configuration — the Elecraft and Flex backends, when they are built, will have theirs — so part
of researching a new protocol is identifying which of its commands persist, before any code
sends them. Three manufacturers have now produced three differently-spelled versions of the
same trap; assume the fourth has one too and go looking for it.

### 5.1 Backends

| Backend | Covers | Notes |
|---|---|---|
| `civ` | All Icom | Binary `FE FE <to> <from> cmd [sub] [data] FD`; BCD frequencies |
| `kenwood` | Kenwood | ASCII, `;`-terminated; per-model quirk table |
| `yaesu` | Modern Yaesu | Same framing as `kenwood`, different fields throughout — see §5.6 |
| `yaesu` | FT-857/857D/897/897D | **Binary**: five fixed bytes, opcode last, no terminator and no framing at all — see §5.7 |
| `rigctld` | Everything Hamlib supports | Pure-Go TCP client; optionally spawns `rigctld` as a child process and supervises it |

`yaesu` appears twice because Yaesu has shipped two CAT systems with nothing in common but the
manufacturer, and **the model name is what tells them apart**, not a second backend name.
`backend: yaesu` with `yaesu.model: ft-857d` builds the binary implementation; anything else
builds the ASCII one. Which protocol a radio speaks is a fact about the radio, so asking an
operator to encode it twice would only create a way to get it wrong — and `ft-891` and `ft-897`
are one character and one entire protocol apart. The two model registries are disjoint and a
test enforces that, since a name in both would make the dispatch depend on which was asked
first. The one consequence worth knowing: **an FT-857 must be named**, because an unnamed Yaesu
still means the modern dialect.

> **Elecraft and Flex are not covered by any of these**, whatever their similarity to Kenwood's
> dialect suggests. An earlier draft of this table claimed `kenwood` covered "Elecraft, modern
> Yaesu, Flex"; the Yaesu third of that turned out to be false in the worst way — `TX;` *keys* a
> Kenwood and *reads* on a Yaesu (§5.6) — so the remaining two are unverified assertions, not
> facts, and are removed rather than left to be planned against. Either may well fit once its
> documentation has been read. That reading is the work, and the rule above is part of it.

Yaesu is **not** a `kenwood` model, despite the shared framing. The two dialects agree on two
ASCII letters, a `;` and the rule that a set command is answered by silence, and disagree about
almost every field above that — including two command letters that mean *different things*.
`TX;` keys a Kenwood and is the PTT *read* on a Yaesu; `KY` streams text on a Kenwood and plays
a stored keyer memory on a Yaesu. That is the same class of difference §5.4 records for the
IC-718's `1C 01`, and putting it behind a config string would make `SetPTT` — the one method
where being wrong keys a transmitter — a runtime branch.

### 5.2 Protocol reference

Both columns below are confirmed against the manufacturers' references: the *IC-7610 CI-V
Reference Guide* and the *TS-590S/TS-590SG PC Control Command Reference Guide*
(B5A-0316-00). Everything listed for Kenwood is TS-590S/SG **common** unless noted.

Icom frames are `FE FE 98 E0 … FD` outbound; `FB` = OK, `FA` = NG. Kenwood commands are 2–3
ASCII letters (case-insensitive) plus fixed-width parameters, terminated by `;`.

| Function | IC-7610 CI-V | TS-590S/SG |
|---|---|---|
| Read / set frequency | `03` / `05` — 5-byte little-endian BCD | `FA;` / `FA00014025000;` — 11 digits in Hz, zero-padded (`FB` = VFO B) |
| Read / set mode | `04` / `06` — `00`=LSB `01`=USB `02`=AM `03`=CW `04`=RTTY `05`=FM `07`=CW-R `08`=RTTY-R `12`=PSK `13`=PSK-R, plus filter `01`–`03` | `MD;` / `MD3;` — `1`=LSB `2`=USB `3`=CW `4`=FM `5`=AM `6`=FSK `7`=CW-R `9`=FSK-R (`0`/`8` = setting failure) |
| Data mode | part of mode | **separate** `DA;` / `DA1;` — orthogonal to `MD` (USB-DATA = `MD2` + `DA1`) |
| PTT | `1C 00` — `00`=RX, `01`=TX | `TX;` (`TX0`=SEND/mic, `TX1`=DATA SEND via ACC2/USB, `TX2`=TX tune; bare `TX;` means `TX0`) / `RX;` |
| RF power | `14 0A` — `0000`–`0255` relative | `PC;` / `PC050;` — 3 digits, **watts**: `005`–`100`, `005`–`025` in AM |
| S-meter | `15 02` — `0000`–`0255` | `SM0;` → `SM0` + 4 digits, `0000`–`0030` meter dots; reads the **RF power meter while transmitting** |
| Filter width | `1A 03` | `FW;` / `FW0500;` — 4 digits Hz; **unusable in SSB and AM** (use `SH`/`SL` there); CW steps 50/80/100/150/200/250/300/400/500/600/1000/1500/2000/2500, off-step values snap **down** |
| IF filter select | mode byte `01`–`03` (FIL1–3) | `FL;` — `1` = IF Filter A, `2` = IF Filter B |
| Bulk status poll | — | `IF;` → fixed **38-char** reply (freq, RIT/XIT, RX/TX flag, mode, split…) — one round trip for most of `State`. **Does not work in Data mode** |
| Push updates | Transceive mode — **must be enabled in the rig's own menu**, see below | `AI2;` — auto-reverts to off at rig power-down, so it does not permanently alter the operator's settings. `AI4` (with backup) needs TS-590S firmware ≥ 2.00 |
| Rejections | `FA` (NAK) | `?;` unrecognised, `E;` communication error, `O;` overflow |
| **Send CW** | **`17`, ≤ 30 chars** | **`KY` — see §5.3** |
| Abort CW | `17` with `FF` | `KY0;` |
| Keyer speed | `14 0C` | `KS;` / `KS025;` — `004`–`060` wpm, clamped |

`IF;` is worth exploiting: one command returns frequency, RIT/XIT, TX/RX state, mode and
split in a single 38-byte reply, so the Kenwood fast poll is *one* transaction rather than
four. The Data-mode caveat means the poller must fall back to discrete `FA;`/`MD;`/`RI;`
queries when `DA` reports data mode.

Three consequences of the above that shaped the implementation:

- **Kenwood answers no set command at all unless AI is already on.** This is not limited to
  `TX;`/`RX;`. Every setter is therefore fire-and-forget followed by an explicit read-back,
  which is wanted anyway: the read-back reports what the rig actually took after `PC` rounds
  to its 5 W grid, `FW` snaps its ladder and `KS` clamps.
- **In Data mode the TS-590 cannot report PTT at all.** `IF;` is the only query carrying the
  TX/RX flag and it does not answer there; `TX;`/`RX;` have no read form. In Data mode PTT is
  observable only through AI push frames. Acceptable for a CW-focused v1, but it is a gap
  rather than a bug, and it argues for keeping `AI2` on.
- **Icom Transceive must be enabled in the IC-7610's menu.** It is reachable over CAT via
  `1A 05 0112`, but `1A 05` writes the operator's persistent Set-mode configuration — see the
  rule at the head of this section. remoses only reads it, so the Icom is poll-only while the
  Kenwood gets free push updates.
- **`FW` is also wrong for FM**, not just SSB and AM: there it selects modulation degree
  (`0000` normal / `0001` narrow), which would otherwise land in state as a 0 Hz passband.

The Icom CW charset (command `17`) is restricted and the rig **silently mangles** anything
outside it, so validate at the API boundary. It is identical on all supported Icoms:

```
0-9  A-Z  a-z  ' ( ) / = ? + . " - @ , :  and space
"FF" aborts;  "^" starts a run sent with no inter-character spacing (prosigns)
```

### 5.3 Kenwood `KY` (verified)

The CW buffer command has four forms:

| Form | Wire | Meaning |
|---|---|---|
| Set 1 | `KY` + **space** + **exactly 24 chars** + `;` | Send text. P1 is *always a space* in this form |
| Set 2 | `KY0;` | Stop sending. Any P1 other than `0` is an error |
| Read | `KY;` | Query buffer |
| Answer | `KY0;` / `KY1;` | `0` = **buffer space available**, `1` = **no buffer space** |

Rules that shape the pacing loop:

- **P2 is fixed-width, 24 characters.** Short chunks are padded with spaces, and *those pad
  spaces are not converted to Morse* — so trailing padding is free and needs no special case.
- **Strings of 25+ characters are sent split**, and **if the rig buffer is full the result is
  an error** — so the sender must poll `KY;` and only push when it answers `KY0;`.
- Case is accepted but not significant.
- **`;` cannot appear in P2** (it would terminate the command).
- Accepted characters: `A–Z a–z 0–9` and ``(space) ' " ( ) * + , - . / : = ? @``

**Prosigns are encoded completely differently from Icom.** Kenwood maps single ASCII symbols
to prosigns rather than using Icom's `^` run-marker:

| Prosign | Kenwood char | Prosign | Kenwood char |
|---|---|---|---|
| BT | `[` | SK | `>` |
| AR | `_` | KN | `]` |
| AS | `<` | BK | `\` |
| HH | `#` | SN | `%` |

remoses therefore defines **one canonical prosign syntax at the API — `^AR`, `^BT`, `^SK` —
and translates per backend**: pass-through for Icom, symbol substitution for Kenwood, and
direct Morse-table lookup for locally generated keying. Clients never learn a rig's dialect.

### 5.4 Icom models

CI-V is a family protocol. The opcodes and their encodings are shared, so one backend serves
every Icom; what differs per model is the factory bus address, which operating modes and
commands exist, and — on one radio — the width of the frequency field. `civ.model` names the
radio so remoses can publish honest capabilities, default the address correctly, and avoid
sending commands the radio will only reject.

Verified against each model's own CI-V documentation — the standalone reference guides for
the current models, and the control-command section of the instruction manual for the older
four:

| `civ.model` | Address | Modes beyond the common set | CW `17` | Filter `1A 03` | Data `1A 06` | FIL slots | PTT | Keyer |
|---|---|---|---|---|---|---|---|---|
| `ic-718` | `0x5E` | **no FM**, no PSK | **no** | **no** | **no** | **0** | **`1C 01`** | 6–60 |
| `ic-7300` | `0x94` | — | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7300mk2` | `0xB6` | — | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7600` | `0x7A` | PSK, PSK-R | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7610` | `0x98` | PSK, PSK-R | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7700` | `0x74` | PSK, PSK-R | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7760` | `0xB2` | PSK, PSK-R | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-7850` | `0x8E` | PSK, PSK-R | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-9100` | `0x7C` | DV | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-9700` | `0xA2` | DV, DD | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-905` | `0xAC` | DV, DD, ATV | yes | yes | yes | 3 | `1C 00` | 6–48 |
| `ic-910h` | `0x60` | **own code table**, LSB/USB/CW/FM only | **no** | **no** | **no** | **0** | `1C 00` | 6–60 |
| `generic` | none | — | yes | yes | no | 3 | `1C 00` | 6–48 |

`ic-7850` covers the IC-7851 too: they are the same radio to CI-V and share an address.

The common set is LSB, USB, AM, CW, CW-R, FM, FSK (the rig calls it RTTY) and FSK-R. The
IC-905 additionally uses a **6-byte frequency field on its 10 GHz band**; everything else
uses 5. `generic` is the escape hatch for an Icom without a profile; it has no factory
address, so `rig_address` must be given rather than guessed.

### The outliers, and what they cost

Most models are a table entry. Six are not, and between them they broke seven assumptions this
backend had baked in as constants — five of them the same shape, a value that varies per model
and was written as though it could not.

**The IC-718:**

- **PTT is `1C 01`, not `1C 00`.** Its command table has no `1C 00` row at all, and on the
  other radios `1C 01` is the antenna tuner — so a constant here would key the wrong command
  on a transmitter. The sub-command is per model on both the set and decode paths.
- **No command `17`,** so no CW over CAT at all. It reports `cw_method: none`, and the daemon
  refuses to start with `cw.method: cat` on it, naming `serial_key` as the fix. That check
  asks the *capability*, not the Go type: the backend type implements `MorseSender` for the
  family, so a type assertion would succeed and every message would draw a rejection that
  looks to the operator like it was sent.
- **No `1A 03`,** so no IF filter width. It is dropped from the slow poll rather than
  generating a rejection every tick.
- **Keyer runs to 60 wpm**, where most stop at 48.

It also has a "CI-V 731 mode" (`1A 05 27`) that shortens the frequency field to four bytes.
remoses never enables it, and since frequency decoding is length-driven a radio left in that
mode is still read correctly.

**The IC-910H** shares the missing `17` and `1A 03`, and adds the two worst kinds of
difference — the ones where a command *succeeds* and means something else:

- **Mode codes are not universal after all.** It puts FM on `0x04`, where every other radio
  here has RTTY. Decoding with the family table would report RTTY for a radio sitting in FM:
  a wrong answer rather than a missing one. `Model.Codes` therefore overrides the whole table,
  and nothing outside `mode.go` may assume a fixed mapping.
- **`1A 06` is RIT, not data mode.** Sending the usual "data mode off" after a mode change
  would quietly switch the operator's RIT off. Data mode is now a per-model capability on
  both the set and the decode path, and `generic` has it off too — on an unidentified radio,
  changing an unrelated setting is worse than not offering the feature.
- Its mode command carries **no filter byte**, so `FilterSlots` is 0 and a trailing byte must
  not be read as a slot.

**The IC-703** is a 10 W portable of the IC-718's generation, and its CI-V documentation is
section 11 of the instruction manual rather than a reference guide of its own. It shares the
IC-718's missing `17` and its 60 wpm keyer, and adds one of its own:

- **`1A 03` exists, and is not the filter width.** On this radio `1A 03` takes a *two-byte
  Set-mode item index*: `1A 0301` is the confirmation beep, `1A 0305` the CW carrier point,
  `1A 0319` the keyer dot/dash ratio. Reading it as a passband would be asking the radio about
  its beeper, and writing one would change it. This is the `1C 01` shape again — the command is
  present, answers, and means something else — and the only defence is that the capability is
  transcribed per model rather than assumed from the family.
- **Data mode is `1A 04`,** not the `1A 06` the rest of the family uses, and it is the on/off
  flag *alone* with no filter byte after it. So the sub-command and the payload shape are both
  per model now; sending the modern two-byte form would hand its parser a parameter it is not
  expecting. `1A 06` is not in its table at all.
- Its mode command carries **no filter byte**, so `FilterSlots` is 0.
- **Split is not claimed.** Its table lists `0F 00` and `0F 01` to turn split off and on and
  shows *no read form*, while the same table writes "Set/read" against `1C 00` and
  "Select/read" against `11`. A setting remoses can write and never read back is precisely the
  failure this backend keeps hitting, so it stays off until somebody puts one on the air. It
  does have `07 00`/`07 01` to select VFO A and B, and `16 47` break-in.

**The IC-706 family — IC-706, MKII and MKIIG** — are the oldest radios here by a decade, and
they broke the two largest assumptions left: that a radio can be keyed, and that it has a power
level.

- **No transmitter command at all.** Not a different sub-command, as on the IC-718 — *none*, at
  any sub-command. remoses can neither key these radios nor learn whether they are keyed; PTT is
  the microphone, a footswitch or a control line. `Caps.PTTControl` is the new field that says
  so, `ApplyPatch` refuses `ptt` with 422 before anything reaches the wire, PTT is dropped from
  the poll, and `ForceRX` — the dead-man and lock-expiry safety path — stops after aborting CW
  rather than sending a command that would fail every time it fired.
- **No RF power either** (no `14`), so `Caps.PowerControl` joins it. Output is a front-panel
  control on these radios.
- **No meter on the first two.** `Caps.SMeterScale` is 0 rather than the family's full scale,
  which a client can tell from a meter that reads zero. The MKIIG gained `15 02`.
- **The filter byte counts from zero.** The modern family numbers FIL1..FIL3 and puts `01`,
  `02`, `03` on the wire; these encode wide, normal and narrow as `00`, `01`, `02`. The same
  three slots, offset by one — so getting it wrong selects the neighbouring filter on every mode
  change, and the rig accepts it without complaint. `Model.FilterZeroBased` and the
  `filterByte`/`filterSlot` pair keep the API's 1-based slot numbering intact either way.
- **Command 06 stops at 06.** WFM is added at `0x06` and CW-R and RTTY-R are absent, so the whole
  mode table is overridden as on the IC-910H: against the family table, `07` would decode as CW-R
  on a radio that has no such mode.
- The MKIIG alone gained `16 47` break-in. It has no CAT CW buffer for that to gate, but an
  operator keying a control line still needs the rig in break-in for the transmitter to follow.

Their manuals are the thinnest documentation here — the original's command table is one narrow
column — and are **incomplete in one direction and wrong in another**, so two facts in these
profiles rest on how the radios behave rather than on what the tables print:

- The IC-706 and MKII tables list only control commands, stopping at `10`, with **no `03` or
  `04`**. Both nonetheless answer read-frequency and read-mode. That is not a nicety: remoses
  fills its state cache by polling and proves the link with those two reads at connect, so a
  radio it could command but never read would never come up at all. It is the difference between
  a working profile and no profile, and it is recorded in the code as an assumption rather than
  a transcription.
- The MKII's data-format diagram prints the transceiver address as `48`, the same as the
  IC-706's, which looks like reused artwork; its factory address is `4E`. The address is
  menu-configurable in any case, which is what `civ.rig_address` is for.

**The IC-905's frequency field is not fixed width.** Its reference specifies ten digits
(5 bytes) on 5.6 GHz and below, and twelve (6 bytes) when the 10 GHz band is selected.
Decoding is therefore driven by the length that arrived rather than by the model, which keeps
the decoder a pure function and reads an unprofiled radio correctly; only encoding has to
choose, and it chooses from the target frequency.

#### Can the model be probed?

Partly, and not reliably enough to be authoritative. Command `19 00` reads the transceiver
ID — but what it returns is the rig's **CI-V bus address**, not a model number, and that is a
poor proxy:

- the address is menu-configurable on every one of these radios, which is the entire reason
  `civ.rig_address` exists, so an operator who changed it defeats identification;
- two different models set to the same address are indistinguishable;
- a model remoses has no profile for still answers with something.

So the configuration stays authoritative and `19 00` is used as a **cross-check**: remoses
asks at connect and warns if the answer disagrees with the configured address, naming the
model whose factory address it does match. That catches the mistake worth catching — a config
naming one radio pointed at another — without silently deciding it knows better. The probe
failing is not an error: an older Icom that does not implement `19 00` must still connect.

There is a second probing trick remoses does **not** use: CI-V address `0x00` is a broadcast,
so `FE FE 00 E0 19 00 FD` makes every rig on a shared bus answer with its address. That is
how a bus is enumerated, but it is discovery rather than identification, and it adds a
failure mode (two rigs answering at once) for a station that already knows what it owns.

#### Confirmed on hardware

Every column of the table above came out of a manual. **The `ic-7610` row has now been seen on
a wire**, with `debug_wire` (§6.1) on and the radio reached over its native USB — which
presents two `cu.usbserial-*` ports, does not echo, and ignores the configured baud rate.

Confirmed: bus address `0x98` (`19 00` → `98`); the five-byte little-endian BCD frequency field
(`50 32 07 18 00` = 18.073250 MHz); mode `04` → `03 01`, CW on FIL1; **PTT on `1C 00`**, which
is the sub-command the IC-718 does not share; `14 0A` as an uncalibrated `0000`–`0255`
(`00 07` = 7, reported as a percentage with `watts` null); `15 02` on a 255 scale; `1A 03`
answering a bare index; `1A 06` accepting a data-mode set; and command `17` carrying plain
ASCII (`17 56 56 56 20 54 45 53 54` = `VVV TEST`) with `14 0C 00 85` setting 20 wpm — the value
`nativeFromWPM` computes. Around 350 poll cycles produced no decode error, no unknown frame and
no NG.

**Transceive push updates work as designed.** With it enabled in the rig's menu, turning the
VFO knob produced a stream of unsolicited `FE FE 00 98 00 <freq> FD` broadcasts, every one
decoded and folded into the cache, which tracked the knob exactly. This is §3's "the reader
never performs a request/response" paying off: the broadcasts go through the same decode path
as a poll reply and match no waiter.

**Two things the hardware found that the manuals could not**, both the same defect: a value
that remoses wrote but never read, so `State` kept reporting the zero value for ever.

- **`1A 03` was decoded to nothing.** The index means a different width per mode family and a
  decoder holds no state, so the original decision was to publish no passband at all. The cost
  was that `passband_hz` was permanently 0 on every Icom while `caps.filter_width` advertised
  true — a promise to a client that nothing ever kept. Fixed by keeping the last mode the rig
  reported, exactly as the `yaesu` backend does for `SH`, and inverting the width table that was
  already there. The radio answered `16` in CW, which is 1200 Hz, and its front panel agreed.
- **`1A 06` was never polled.** Data mode has no other source — no other answer carries the
  flag and the rig does not broadcast it — so a radio sitting in USB-D was published as plain
  USB. The rig acknowledged `1A 06 01 02` with `FB` while remoses went on reporting data mode
  off. Fixed by adding the read to the slow tier, guarded by the model for the reason §5.4 gives
  above: `1A 06` is RIT on an IC-910H, and polling it there would publish an RIT setting as a
  data-mode change every tick.

Both were invisible to a manual reading, because a manual documents the command and not whether
anything asks for it. That is the argument for doing this on hardware at all.

**A second session found two more, and they are the sharper kind: a command that succeeds and
changes something nobody asked it to.** Neither `06` nor `1A 06` is a filter selector. `06` sets
the *mode* and takes a filter byte with it; `1A 06` sets *data mode* and takes a filter byte with
it. Using either one to move a filter therefore has a side effect, and remoses was paying both:

- **`SetFilterSlot` cleared the data flag.** It sent `06 <mode> <slot>`, and `06` resets `1A 06`
  — so choosing a filter dropped the operator out of USB-D. The earlier code considered `1A 06`
  and rejected it, correctly, as invalid in CW; what went unnoticed is that the alternative is
  not side-effect-free either. It now picks by what the rig is doing: `1A 06` in a data mode,
  `06` otherwise, which covers every case without a command that means something else.
- **`SetMode` moved the filter.** It sent `06 <mode>` with no filter byte, and the rig answers
  that by reverting to the mode's *default* filter. Because data mode is orthogonal at the API
  layer, a request changing only the data flag arrives as `SetMode(the mode the rig is in, flag)`
  — so touching data mode moved the filter. It now reads the mode first and sends nothing when it
  already matches. On a genuine mode change the filter is still left to the rig, which is the
  right default for a new mode.

Together they made **USB-DATA on FIL1 unreachable**: filter-then-data landed on FIL2 with data
on, and both-in-one-patch landed on FIL1 with data off. Confirmed fixed on the radio, in both
orders.

The general lesson is worth keeping: on CI-V several settings share a command, so *writing* one
of them rewrites its neighbours. Any new setter here should ask what else its command carries
before assuming it is narrow.

#### Both VFOs, split and dual watch

Transcribed from the *IC-7610 CI-V Reference Guide* (A7380-7EX-4, Sep 2025). **Only the
`ic-7610` profile enables any of this**, because that is the only Icom reference remoses has
read for it. Several others very likely have `25` and `26`; a capability this backend has not
transcribed is one it does not claim.

| Need | Command |
|---|---|
| Read / set one VFO's frequency | `25 <band>` / `25 <band> <5-byte BCD>` |
| Read / set one VFO's mode, data mode **and** filter | `26 <band>` / `26 <band> <mode> <data> <filter>` |
| Split | `0F` read, `0F 00` / `0F 01` set |
| Dual watch | `07 C2` read, `07 C1` / `07 C0` set |
| Anything else, on the inactive band | `29 <band> <command…>` |

`00` is the main band and `01` the sub; remoses calls them VFO A and B, which is the operator's
model and the API's.

**Why not `03`/`05` and `04`/`06`.** Those read and write *the operating* frequency and mode —
whichever VFO the radio is on. There is no way to name the other, so reaching it means selecting
it, which changes what the operator is using and races the front panel. `25` and `26` put the
VFO in the frame.

**`29` is a prefix, not a command.** "Regardless of active/inactive the Main or Sub band, you can
directly specify the Main or Sub band, and send/read the supported command settings." The
supported ones are marked in the reference's own table, and the two that matter here are: `15 02`,
the S-meter, and `1A 03`, the filter width. **Frequency and mode are pointedly not marked**, which
is exactly why `25` and `26` exist as separate commands rather than as `29`-prefixed `03` and `04`.

**`26` is atomic, and that is worth more than this feature.** Mode, data mode and filter travel in
one frame, so none can disturb the others — which is precisely the collision that produced two of
the bugs above, where `06` and `1A 06` overwrote each other. Where a request names a VFO, remoses
takes this path and the collision cannot arise. Note that `26` has no "leave the filter alone"
encoding: the reference is explicit that a frame omitting the data and filter bytes selects DATA
OFF and the mode's default filter, so preserving a filter takes a read first.

**The IC-7610 is not the classic A/B pair.** On a single-receiver radio one VFO is live, the
display shows which, and split transmits on the other. Here both VFOs are real receivers: A is
always what the radio receives and transmits on, B joins in under dual watch and takes the
transmit under split. So `state.vfo` reads `A` permanently and remoses offers no VFO-select
operation, because there is nothing to select.

Two things follow the rule this backend already applies elsewhere. The sub receiver's meter is
polled **only while dual watch is on** — with it off that receiver is not running, and a reading
from it would sit in the cache looking live, the same reasoning `yaesubin` uses to gate a transmit
meter on PTT. And the per-VFO passband is read behind the `29` prefix and converted against **that
VFO's** own mode rather than the operating one; a `passband_hz` that always read zero would have
been the same defect this backend was carrying twice that morning.

#### The IC-9700 means something else by the same commands

Reading its reference settled what guessing could not. `25` and `26` exist there too and address
a **different axis**:

| | IC-7610 | IC-9700 |
|---|---|---|
| `25`/`26` selector | `00` MAIN band, `01` SUB band | `00` **selected** VFO, `01` **unselected** |
| Scope | either receiver | **"(Only MAIN band)"** |
| Reaching the sub band | `29` prefix, no selection needed | *impossible* — "You cannot set the SUB band frequency" |
| `29` prefix | yes | **not in its command table** |

One opcode, two axes: the `1C 01` shape again, and why `Model.DualVFOBandSelector` is per model
rather than a constant.

The IC-9700's sub band is real and receives independently, and the only route to it is `07 D1`,
"select the sub band" — which moves the operator's own focus and fights whoever is holding the
dial. **remoses does not send it.** `caps.sub_receiver` is true because the radio has one and
`caps.sub_receiver_readable` is false because remoses will not grab the radio to read it; a meter
reading is not worth that. There is a test asserting no `07 D0`/`07 D1` ever reaches the wire.

Which VFO is "selected" is also unknowable: `07 00` and `07 01` *set* VFO A and B and nothing
reports the current one. So on that radio A and B are **relative** labels — A is whatever the
operator has selected, B is the other — and `caps.vfo_addressing` says `relative` where the
IC-7610 says `named`, rather than leaving a client to mislabel one of them. The split rule is the
same either way: B is where transmit goes.

What this buys is that one flat two-slot model serves both radios honestly, where a nested
`receivers → VFOs` structure would have had to leave the IC-9700's sub branch permanently empty
and the IC-7610's A/B branch permanently absent. Both radios expose **two addressable tuning
slots, the second of which is the split target**; what those slots *are* differs, and caps says
so.

Confirmed on an IC-9700: `vfo_addressing: relative`, VFO A tracking the operating frequency,
VFO B set to 432.200 USB and back with the operating VFO untouched, split on and off, and no band
selection on the wire at any point.

**Memory mode is not modelled, but leaving it is.** Command `07` selects VFO mode and `08`
selects memory mode; remoses implements only the first, through `vfo_mode: true`. Modelling
channels would need a channel list, a select command and a state field; what an operator needs
from a daemon is the way *out*, because a rig on a memory channel answers `25` and `26` with NG —
the reference says so for memory, call-channel and DR modes — so its per-VFO readings go stale
with nothing in the API able to move them. Naming a VFO also selects it where `07 00`/`07 01`
exist, which is the IC-9700 and not the IC-7610.

#### CW break-in, and a command that succeeded and did nothing

`16 47` reads and sets break-in: `00` off, `01` semi, `02` full. It is in the table of both
references remoses has read, and it is not one more switch — it decides whether CW sent over CAT
reaches the air at all. Both references print the same footnote against command `17`:

> …if the [TRANSMIT] or an external TX switch is ON, or the Break-in function is ON, a message
> will be transmitted as CW code when you send it from your PC.

With break-in off and nothing keying by hand, `17` is **accepted**, the rig's buffer drains on
schedule, PTT never rises and nothing goes out. Every signal remoses has says success. That is the
`1C 01` failure wearing different clothes, and it was found the only way it could be — by sending
real Morse at a real radio and having the operator report they heard none of it.

So remoses reads break-in onto the slow tier, publishes it as `state.break_in` with
`caps.break_in_control`, lets a client set it, and **will not queue CW that would go nowhere**:
`Session.EnsureCWWillTransmit` runs before every send.

What it does about it is `cw.break_in`, which is `semi` (the default), `full` or `manual`. Under
`semi` or `full` the setting is *written* — a remote operator has no way to reach the front
panel, and a knob they cannot turn is not a safety feature. Under `manual` nothing is written and
a rig known to have break-in off is refused with a 422 naming the fix, which is what a station
that sequences its own T/R path wants.

The default is semi rather than full deliberately. Full break-in is QSK: the rig switches between
transmit and receive *inside the gaps between elements*, and on the TS-590S that was audible as
the relays clocking rapidly through every transmission. Not every station's relays, sequencer or
amplifier will tolerate that, so it is the operator's call to make on purpose — never something a
remote client turns on for them. For the same reason the check only ever raises break-in to a
state that transmits: it will not downgrade a rig already on full, nor force QSK onto one running
semi.

Three rules keep the guard from becoming its own outage. An *unknown* break-in never blocks — a
radio whose reference has not been read for `16 47` must still be able to send; a failed *write*
on an unknown does not block either, since the command not applying is not evidence the Morse
will go nowhere; and PTT already being up is accepted, because that is one of the three
conditions the footnote names.

**`16 47` is not the same command on every Icom either.** Most of the family reads it
"00=OFF, 01=semi break-in, 02=full break-in"; the IC-910H's table says only "Set break-in
(0=OFF; 1=ON)". So `Model.BreakIn` is a style rather than a flag, and the two-value radios
publish `radio.BreakInOn` instead of choosing between semi and full — the distinction is audible,
full being QSK, and the radio declined to make it.

Setting is asymmetric on purpose. On a two-value radio a request for semi *or* full sends its
single `01`: they are the same setting there, so honouring both is accurate rather than
approximate, and sending `02` would be an out-of-range parameter. On a three-value radio a bare
`on` becomes semi — the radio distinguishes them and the caller did not, so the quieter one is
chosen, which is the choice `cw.break_in`'s default already makes.

Where a table names the command and no values — the IC-706MKIIG's does exactly that, its command
table having no Data column at all — the three-value form is assumed, because it fails **loudly**
if wrong. A request for full sends `02` and draws an NG; guessing the two-value form on a
three-value radio would instead deliver semi, quietly, to somebody who asked for QSK.

Setting it exposed a second bug in the same hour: `16 47` is read on the slow tier, and
`ApplyPatch` did not mark a break-in request as needing that tier, so the write succeeded while
the response carried the value from before it. The radio's own display showed BKIN on while
remoses reported it off and went on refusing to send. **Any field read on the slow tier has to be
in that list**; there is now a test per field rather than trust.

**And then it happened again, on a TS-590S** — the same silent failure, after it had been found,
understood, documented and fixed here. The fix had been written into the `civ` backend, so the
Kenwood backend, which had no notion of break-in at all, sent Morse into a rig that was never
going to transmit it and reported success. A guard that lives in one backend protects one
backend; this one now lives in the session, where every backend that can report break-in gets it.
See §5.5 for the four different commands Kenwood needs to do so.

**The rest of the surface was exercised on the same radio**, and passed: authentication;
the whole lock lifecycle including steal and expiry; WebSocket streaming and `ws-ticket`; all
ten modes; all three filter slots; the filter-width ladder with its snapping; explicit PTT;
CW charset rejection, abort, chunking and `replace`; and **every safety interlock** — band
limits refusing out-of-band with the rig not moving, power clamping 50 % down to the configured
8 %, and `tx_timeout` forcing receive after 4.2 s against a 5 s limit and taking an in-flight CW
queue with it.

Two results are worth recording as numbers. **CW pacing is accurate**: a 43-character message
at 25 wpm was estimated at 18288 ms and took 18349 ms — 61 ms of drift, on a rig whose buffer
cannot be queried, so the whole thing is dead reckoning (§11.1). And **lock expiry does cut a
live transmission**: with a lease deliberately shorter than the message, `17 FF` and `1C 00 00`
went out mid-word and the operator heard the transmission stop inside a character.

That last one exposed a sharp edge that is **not** a bug but is worth knowing: the lease slides
on accepted API commands, and a CW transmission is one command followed by minutes of the
pacing loop talking to the rig directly. So `lock.ttl` must exceed the longest message, or the
interlock will truncate it — the opposite of §7's intent that active use keeps a lock alive.

**Reconnect** was tested by pulling the cable: detection was clean (`device not configured` →
`ErrDisconnected`, logged at INFO because a pulled cable is an expected ending), the CW abort on
teardown failed quietly as designed, backoff doubled correctly, and the reconnect re-ran `Init`
in full — identity re-checked, all of state re-read. The device path happened to survive the
replug on this hardware, so `port.match` was not needed; that is not guaranteed and is why the
option exists.

### 5.5 Kenwood models

The Kenwood family splits in two, and the split is deeper than Icom's. The TS-480 and TS-590
generation is what §5.2 describes. The TS-890S and TS-990S are a **different dialect** that
happens to share a terminator.

Verified against each radio's own PC Control Command Reference Guide:

| `kenwood.model` | `ID;` | Mode | Data mode | `IF;` | S-meter | Filter width | Filter select | Max W |
|---|---|---|---|---|---|---|---|---|
| `ts480` | 020 | `MD` | **none** | yes | `SM0;` scale **20** | `FW` Hz | **none** | 100 |
| `ts590s` | 021 | `MD` | `DA` | yes | `SM0;` scale 30 | `FW` Hz | `FL1`/`FL2` | 100 |
| `ts590sg` | 023 | `MD` | `DA` | yes | `SM0;` scale 30 | `FW` Hz | `FL1`/`FL2` | 100 |
| `ts890s` | 024 | **`OM`** | **in mode code** | **none** | **`SM;`** scale **70** | **none** | `FL0` A/B/C | 100 |
| `ts990s` | 022 | **`OM`** | **in mode code** | **none** | `SM0;` scale **70** | **none** | `FL0` band + A/B/C | **200** |
| `generic` | — | `MD` | `DA` | yes | `SM0;` scale 30 | `FW` Hz | `FL1`/`FL2` | 100 |

`KS` is `004`–`060` and the `KY` buffer is 24 characters everywhere, so CW sending is the one
thing that does not vary.

**What the TS-890S and TS-990S change:**

- **No `MD`.** Mode is `OM P1 P2;`, where P1 selects the display area (`0` left/main, `1`
  right/sub — ignored on a set) and P2 is the mode. remoses addresses the main area only.
- **DATA is folded into the mode code.** There is no `DA` command: `C`, `D`, `E` and `F`
  *are* LSB-D, USB-D, FM-D and AM-D. Encoding USB with DATA has to produce `D`, and decoding
  `C` has to report LSB with the DATA flag set, or a radio in LSB-D is published as plain LSB
  and the operator's data path is invisible. `A` and `B` add PSK and PSK-R.
- **No `IF;` at all** — the bulk status command this backend was built around. Combined with
  `TX;`/`RX;` being set-only, that means **PTT on these radios can never be polled**: it
  arrives only through AI push frames. remoses enables `AI2` at connect, so it works in
  practice, but it is a permanent limitation rather than the TS-590's data-mode-only one.
- **`FW` is not a filter width.** There it selects FM narrow/normal, so sending a width would
  change modulation. remoses refuses `SetFilterWidth` on these models.
- **`FL0` is one command, not four slots.** `FL0`, `FL1`, `FL2` and `FL3` are *unrelated*
  commands — Select the Receive Filter, Roofing Filter, IF Filter Shape, AF Filter Type — so
  treating them as filter slots would set the roofing filter when the operator asked for slot
  2. The selection is `FL0`'s parameter: A, B or C, three slots. On the TS-990S a band
  parameter comes first (`FL0` + band + selection), so the selection is the *third* character
  of the argument, not the first.
- **The TS-890S has no safe read of its filter selection.** Its manual prints the read form of
  `FL0` as `FL0 P1 ;`, indistinguishable from the set form: there is no way to ask without
  also telling. remoses does not read it back there and relies on the AI echo of the set
  instead. This is documented ambiguity, not an assumption — it is the one place the two
  manuals disagree about a command they otherwise share.

**Both VFOs are addressable, and `FA`/`FB` are how.** They read and set VFO A and VFO B
directly, without selecting either, which is real addressing rather than "the VFO the radio is
on" — the same property that makes the IC-7610's `25` usable. `FR` and `FT` select the receive
and transmit VFO, and split is the relationship between them: this protocol has no split flag to
write. The reference describes the two only by their side effects — "when using the FR command
to select VFO A or VFO B, the selected VFO changes to the simplex state. When using the FT
command, the selected VFO changes to the split state" — so switching split *off* is an `FR`
naming the receive VFO and switching it *on* is an `FT` naming the other one. Which VFO is being
received therefore has to be known before either can be sent, and `SetSplit` reads `FR;` rather
than guessing: guessing wrong transmits on the wrong frequency.

Reading it back is free where the model has `IF;`. Its P10 is the FR/FT selection and its P12 is
simplex/split, both already in this backend's field table and neither previously decoded — which
is why split was never published on a radio that reports it twice a second. The TS-890S has no
`IF;`, so `FR;` and `FT;` are read on the slow poll for every model.

There is **no per-VFO mode**: `MD` addresses whichever VFO is selected and nothing addresses the
other one's, so `Caps.PerVFOMode` is false and `SetVFOMode` refuses. Selecting the named VFO,
sending `MD` and selecting back would move the operator's radio under them and leave it wrong if
the sequence failed halfway, which is worse than not offering it.

Knowing the selection fixed a quieter bug along the way. `SetFrequency` with `VFOCurrent` used to
mean VFO A unconditionally, and the discrete fast poll always read `FA;` — so on a rig parked on
VFO B, remoses reported the frequency of the VFO nobody was listening to and tuning moved that
one instead. Both now follow `FR`, falling back to A only until the radio has said.

**On the TS-990S none of the above applies, because its `FA` and `FB` are not two VFOs.** Its
reference names them "Main Band Frequency" and "Sub Band Frequency" — two receivers, each with
its own VFO A and B underneath, reached by commands this backend does not implement. That is the
same trap as the Icom side, where the IC-7610's `25`/`26` name main and sub bands and the
IC-9700's name the selected and unselected VFO of one band: one opcode, two axes. So remoses
publishes `caps.vfos: ["current"]` there and refuses VFO addressing outright, rather than
labelling that radio's Sub band "VFO B" and moving the wrong receiver.

This is also what `Caps.VFOs` had been getting wrong. It advertised `current`, `A` and `B` on
every model in the family while no model implemented `backend.DualVFO`, so the session refused
every request that named a VFO — a capability list promising something the next call rejected,
which is the same failure the break-in work above was about.

**CW break-in is four different commands across five radios**, and getting it wrong is not
cosmetic: with break-in off, `KY` is accepted, the rig's buffer drains on schedule and nothing
is transmitted. The whole reason remoses reads and writes this is covered under §11.1; what
differs here is the spelling.

| Model | Command | Values | Semi vs full | `VX` in CW |
|---|---|---|---|---|
| TS-990S | `BI` | `0` off, `1` semi, `2` full | direct | fenced off: "cannot be set in modes other than SSB/AM/FM" |
| TS-890S | `BI` | `0` off, `1` on | `SD` delay: `0000` ms **is** full | fenced off, same wording |
| TS-590S / SG | **`VX`** | `0` off, `1` on | `SD` delay, as above | **is** break-in, stated outright |
| TS-480 | **`VX`**, inferred | `0` off, `1` on | `SD` delay, as above | not documented either way |

The TS-590 row is the trap. That radio has no break-in command at all: `VX` sets VOX, and its
reference adds that "when transmitting the VX command in CW mode, the Break-in function is set
and read, rather than the VOX function". One command, two meanings, selected by the mode the
radio happens to be in. So remoses reads `VX` **only in CW** — a reading taken in SSB would be
the VOX setting published as break-in, which is a confident wrong answer feeding the guard that
decides whether Morse will be heard — and refuses a break-in *set* outside CW rather than
switching voice VOX on behind the operator.

**The TS-480 row is an inference, and the last column is why.** Its reference documents `VX` as
the VOX function and is silent about CW. But the two facts in that table move together: the
radios with a dedicated `BI` both fence `VX` off from CW, and the radio without one overloads it.
The TS-480 has no `BI` and no fence — and it does have `SD`, "the CW Break-in time delay", so
break-in exists on the radio and something must switch it. `VX` is the only candidate. The
silence therefore reads as an omission rather than a denial; the TS-590's reference had to say it
out loud precisely because it is surprising.

The bounded downside is what makes the inference worth acting on: remoses writes `VX` only in CW,
so if it is wrong the radio gets VOX switched on rather than break-in, and nothing transmits from
it in CW. That is recoverable and reportable. It is recorded here as an assumption to revisit,
alongside the AM power ceiling below, rather than as something transcribed.

On the two-value styles, semi and full are the same `1` and are told apart by `SD`. Writing full
means `SD0000;` first; writing semi when the rig is already on full has to invent a delay, since
full *is* zero and there is no previous value to restore. remoses uses 300 ms — mid-scale, and
about a character at 20 wpm — and leaves any non-zero delay the operator already had alone.

`caps.break_in_control` is false on `generic` alone, and that is a deliberate abstention.
`generic` copies the TS-590 shape everywhere else, but the reasoning that licenses `VX` on a
TS-480 is reasoning about *Kenwood*, and this dialect is also spoken by Elecraft and by modern
Yaesu. Break-in is the one command in the profile that **writes** on the strength of the guess
rather than merely reading, and being wrong leaves VOX enabled — invisibly, since the write only
happens in CW, so it surfaces later when the operator moves to SSB and the rig keys on room
noise. A fault that appears somewhere other than where it was caused is a bad one to introduce
into a radio remoses cannot identify. Weigh that against the cost of abstaining — CW accepted and
not transmitted, which is where every unidentified radio already stood — and the asymmetry
decides it. The same reasoning is why remoses records capabilities per model instead of probing.

**Assumptions worth revisiting on hardware:**

- The AM power ceiling is taken as a quarter of the model maximum — 25 W at 100 W, 50 W at
  200 W — which matches every documented `PC` range. Per-*band* ceilings are not modelled: a
  TS-890S on 70 MHz caps at 50 W (13 W AM) where remoses would allow 100. The read-back after
  every write means the operator still sees what the rig actually took.
- The TS-480 is profiled at 100 W. The 200 W TS-480HX answers the same `ID020`, so an HX would
  be capped at half its output until the profile learns to tell them apart.
- The TS-480's break-in is on `VX`, inferred from the family rather than read from its reference
  (above). Sending CW to one and watching whether it transmits settles it in a minute; so does
  reading `VX;` in CW and comparing against the front panel.

Both are answerable in one session with `debug_wire` on (§6.1), which is the point of it: the
`PC` the rig accepts and the `PC` it reports back appear as bytes, side by side.

### 5.6 Yaesu models

Transcribed from each radio's own CAT Operation Reference Manual — twelve radios, twelve
manuals, read field by field for the wire formats below.

The models split into **two generations**, and the split is a wire format rather than a feature
list: `FA`/`FB` are **nine** digits on the FTdx101 generation and **eight** on the FT-950
generation, and the `IF` answer is a byte shorter to match, because the frequency field is
where the byte goes.

| `yaesu.model` | `ID;` | `FA` | `FA` range | `IF` | Modes beyond the common set | `SH` form | `PC` |
|---|---|---|---|---|---|---|---|
| `ft-950` | `0310` | **8** | 30 kHz – 56 MHz | **27** | **none** (no `E`) | `SH0<nn>;` | 100 W |
| `ftdx5000` | `0362` | **8** | 30 kHz – 60 MHz | **27** | **none** (no `E`) | `SH<s><nn>;` | **`000`–`255` index** |
| `ftdx3000` | `0462` | **8** | 30 kHz – 60 MHz | **27** | **none** (no `E`) | `SH0<nn>;` | 100 W |
| `ftdx1200` | `0582`/`0583` | **8** | 30 kHz – 56 MHz | **27** | **none** (no `A`, no `D`, no `E`) | `SH0<nn>;` | 100 W |
| `ftdx9000` | **none** | **8** | 30 kHz – 60 MHz | **27** | **none** (no `D`, no `E`) | **not a width** | **`000`–`255` index** |
| `ft-891` | `0650` | 9 | 30 kHz – 56 MHz | 28 | **none** (no `A`, no `E`) | `SH0<n><nn>;` — **narrow flag** | 100 W |
| `ft-991a` | `0670` | 9 | 30 kHz – 470 MHz | 28 | **C4FM (`E`)**, no PSK | `SH0<nn>;` — **6 bytes** | 100 W |
| `ftdx101d` | `0681` | 9 | 30 kHz – 75 MHz | 28 | PSK (`E`) | `SH<s>0<nn>;` | 100 W |
| `ftdx101mp` | `0682` | 9 | 30 kHz – 75 MHz | 28 | PSK (`E`) | `SH<s>0<nn>;` | **200 W** |
| `ftdx10` | `0761` | 9 | 30 kHz – 75 MHz | 28 | PSK (`E`) | `SH00<nn>;` | 100 W |
| `ft-710` | `0800` | 9 | 30 kHz – 75 MHz | 28 | PSK (`E`) | `SH00<nn>;` | 100 W |
| `ftx-1` | `0840` | 9 | 30 kHz – 470 MHz | **30** | PSK (`E`), **C4FM-DN (`H`), C4FM-VW (`I`)** | `SH<s>0<nn>;` | 10 W / **100 W with SPA-1** |
| `generic` | — | 9 | 30 kHz – 470 MHz | 28 | PSK (`E`) | `SH00<nn>;` | 100 W |

The `IF` column is the answer length **including** the terminator, as the manuals count it.

Common to all twelve: `MD<sel><code>;` with DATA folded into the code, `SM0;` → three digits on
a **255** scale, `TX;` read / `TX1;` key / `TX0;` unkey, `AI1;` for push updates, and **no
documented error response of any kind** — which is where the manuals and the radios part
company; see the `?;` note below.

**Mode code `E` is the reason the code table is per model.** It is PSK on five radios, C4FM on
the FT-991A, and does not exist at all on the FT-891 or on any radio of the FT-950 generation —
so decoding an FT-991A with the family table would report a rig sitting in C4FM as PSK, the
IC-910H failure in §5.4 on a different manufacturer. `Model.Codes` is the whole table, per
radio, and nothing in the backend falls back to a family default. The older manuals also
*rename* three codes without changing them: `8`, `C` and `A` are printed PKT-L, PKT-U and
PKT-FM where the newer ones say DATA-L, DATA-U and DATA-FM. Same codes, same meaning, same
`radio.Mode` with the DATA flag.

Which codes each radio has is not a monotone function of age. The FT-950 and FTdx3000 carry the
full twelve plus `A`; the newer FTdx1200 prints `A` as `----`, explicitly unused, and has no
`D` either; the FTdx5000 and FTdx9000 have `A` but no `D`. `D` is AM-N, which decodes to plain
AM where it exists, so its absence only means an AM-N frame reports nothing rather than AM —
`encodeMode` always picks `5` for AM.

**What no Yaesu here can do:**

- **Send arbitrary CW over CAT.** Not one model has a streaming buffer. `KY` plays a stored
  keyer memory and the text lives in `KM`, which is the operator's own saved messages, holds 50
  characters, and cannot be asked how far playback has got. remoses **never writes `KM`** — the
  same line §5.4 draws at Icom Transceive — so it reports `cw_method: none`, does not implement
  `MorseSender` at all, and names `serial_key` as the fix. Every one of these radios has a
  `PC KEYING` menu item offering RTS or DTR, so the fallback is first-class.
- **Report PTT in the bulk poll.** There is no TX/RX flag anywhere in a Yaesu `IF`; Kenwood's P8
  is Yaesu's CTCSS field. The fast poll is `IF;` + `TX;` + `SM0;`, three transactions. In
  exchange `IF;` answers in *every* mode, so none of the Kenwood backend's data-mode fallback
  machinery has an analogue, and `TX;` has a read form, which Kenwood's has not.
- **Say why it refused.** No manual documents an error, NAK or busy response, so most refusals
  are silence and cost a full session timeout: an out-of-range parameter, a command the model
  does not implement, a mode the rig will not take. The backend therefore range-checks frequency
  and power itself, never sends speculatively, and records which commands a model lacks in its
  profile instead of probing for them. The one exception is `?;`, below.
- **Select an IF filter.** No `FL` equivalent exists; `FilterSlots` is 0. The FT-950 generation
  does have an `RF` roofing-filter command, but its parameter mixes `AUTO` with fixed widths and
  is not the numbered bank of IF filters `FilterSlots` describes, so remoses does not model it.

**`?;` exists, and means "busy".** No Yaesu manual mentions it, but the radios send it: an
FT-450 answering `IF;` with `?;` and an FTdx3000 answering `FB;` the same way are both on
record. These are field reports rather than transcribed facts, which is why the table above
still says the manuals document no error response. remoses waits for it on every read and on
every set's read-back, alongside the reply key — the same `errorKeys` trick the `kenwood`
backend uses — so a command refused that way fails in **one round trip** instead of burning the
session's full per-command timeout. That speed is the entire point of handling it.

It is treated as **transient, never as a rejection**. `?;` is genuinely ambiguous in the wild: it
covers "I am busy" and "not allowed in the state I am in", and remoses does not try to tell them
apart, because the recovery is identical and the alternative — deciding a command or a
capability is gone on the strength of one answer — would lose a working feature over a momentary
condition. So a `?;` never disables a poll item, never marks a capability absent, never tears
down the connection, and is never cached or remembered. The next poll tick simply asks again.
It travels as `backend.ErrBusy` (aliased `rig.ErrBusy`), which `isFatalPollErr` deliberately
treats as non-fatal, and the API answers **503** with a detail saying the radio was busy and the
request can be retried — never **422**, which would tell a client to rewrite a request that was
already correct.

`N`, `O` and `E` are reported in the field too and are **not** handled. `N` means invalid data —
a genuine rejection of what was sent, not a "try again" — so treating it as busy would retry a
command remoses is spelling wrongly for ever. Each wants its own decision about what it means to
a caller; until then they are ignored as line noise and cost a timeout apiece.

**Three models change the wire, not just the table.** The FTX-1's `IF` is 30 bytes because its
memory-channel field is five characters rather than three, shifting every field after it by two.
The whole FT-950 generation's is 27 because its frequency field is eight digits rather than
nine, shifting every field after it back by one — the FTdx9000's own manual numbers the
parameters differently again (it splits the clarifier in two, so its mode is P7 rather than P6)
without moving a single byte. Decoding dispatches on the length that arrived, so a misconfigured
station still reads correctly; encoding takes the width from the model. The FTX-1's `PC` also
carries a head selector — `1` the field head (10 W), `2` the SPA-1 amplifier (100 W) — which
makes the power ceiling a property of what is plugged in, refined from the first `PC;` answer.

**`PC` is not always watts.** On the FTdx5000 and FTdx9000 the manuals give the same three
digits as `000`–`255` where every other model gives a watt range, in the same manuals that give
`MG`, `PL`, `SQ` and `RG` as `000`–`255` too — this is those radios' level scale, not a typo. An
independent implementation splits the family the same way, giving the FT-950, FTdx1200 and
FTdx3000 a 5–100 scale and those two the 0–255 one, which is worth having because it is the kind
of thing a manual gets wrong in isolation and two sources do not.

Nothing calibrates the index against an output. The same implementation does convert it for
display, by multiplying a 0–1 fraction by a nameplate figure — 200 W for the FTdx5000, 200 W for
the FTdx9000D and Contest, **400 W for the FTdx9000MP** — and that last figure is exactly why
remoses will not: the FTdx9000 spans two ratings, it has no `ID` command, and so nothing on the
wire says which one is on the desk. A watt reading would be right for some owners and half or
double for others. So remoses reports `power_watt_accurate: false`, leaves `max_power_w` unset
rather than publishing a rating, and **refuses a request in watts** on those two — the same
treatment the `civ` backend gives Icom's `14 0A`.

**`SH` is a table index, not a width in Hz**, and the index's meaning depends on the mode and —
on the FT-891, the FT-991A and the whole FT-950 generation — on the separate `NA` narrow
setting. remoses snaps a requested width onto the model's ladder the way it already does for
Kenwood `FW`: the closest rung at or below, clamped to the ends. In AM and FM it publishes and
sets nothing, because the older tables have no column there and the newer ones hold one fixed
value per *mode code*, a distinction `radio.Mode` does not carry. Six distinct ladders are
needed for twelve radios; the FTdx1200 and FTdx3000 share one to the digit, and the FTdx5000's
differs from theirs in three places, one of which is an index its manual prints `- - - -` and
defines for no width at all.

**The FTdx9000 is the outlier of the family, and by a wider margin than it first appeared.**
These are capability gaps worth stating plainly rather than implementation gaps. On a protocol
with no error response an unimplemented command answers with silence, so every one of these
would have cost the session's full per-command timeout and then reported nothing:

- **No `ID`.** Its command list has no row for it, so there is no identity cross-check to make
  on that radio at all. `FA;` is its link check instead.
- **No `NA`.** Nothing is lost, because it also has no bandwidth table for `NA` to choose a
  column of.
- **No `AI`** — and, uniquely among the twelve documents, its command list has no *AI column*
  either, which is the column the other manuals use to mark the self-reporting commands. It is
  the family's one poll-only radio: nothing is pushed, so nothing is written to ask for pushes.
- **No `RM`, `PS` or `RA`.** So no transmit meters, no power switch and no attenuator:
  `power_meter`, `swr_meter`, `alc_meter` and `power_switch` are all false there, and `PowerOn`
  and `PowerOff` refuse naming the model.
- **`SH` is not a bandwidth.** Its parameter is the position of the WIDTH knob — `00` fully
  anticlockwise to `31` fully clockwise, `16` centred — and no table in the manual converts that
  to Hz. `filter_width` is false there and `SetFilterWidth` refuses, rather than sending a number
  that would move the knob to an arbitrary place.
- **`AC` has no "tuner on" value at all.** It is one parameter — `0` off or tuning stopped, `1`
  start tuning, `2` tuning failed, answer only — so a `0` coming back means *either* switched out
  *or* engaged and idle, two states remoses cannot tell apart. `tuner_control` and `tuner_tune`
  are false, `AC` is not polled, and its answer is not decoded.
- **`BP` has neither a receiver selector nor a sub-command selector**, where the rest of the
  family has both: one bare `BP;` reads it, and the same answer carries the notch's switch and
  its position together. `000` is the manual notch switched out and `001`-`300` is the frequency.
  Because there is no separate "switch on" to send there, `SetNotch(true)` writes back the last
  position the radio itself reported, and refuses when none is known.

Its `PA` is a two-value IPO status rather than the family's three, so it has one amplifier, and
`PA02;` was out of range on it.

Its `TX` answer also has a fourth value the others lack, `3`, for keyed at the rig and by CAT at
once. It decodes as transmitting, like `1` and `2`.

**Assumptions worth revisiting on hardware:**

- ~~**The FT-891's manual does not say which of `1` and `2` is LSB.**~~ **Resolved.** Its manual
  labels both "SSB" and defers to a BFO menu item, so remoses read `1` as LSB by consistency
  with every other model and with the pairing structure (`3`/`7` CW, `6`/`9` RTTY, `8`/`C`
  DATA). `1` is LSB on the FT-891, and the mappings are consistent across the family — this is
  settled for the whole table, not just for this one byte.

  The exception is `E`, which is C4FM on the FT-991A and PSK on the other radios.
- **DATA is grouped with CW/RTTY/PSK in the `SH` table.** The FT-710 and FTX-1 tables say so
  outright; the FT-991A, FT-891 and the FT-950 generation name no DATA column at all. The
  FTdx5000 is the one that comes closest to confirming it: its table has separate RTTY and PSK
  columns, and they are identical.
- The watt unit for `PC` is inferred on the models that use one. No manual states it, but the
  200 W FTdx101MP's range stops at 200 where the 100 W FTdx101D's stops at 100, and the FTX-1's
  table annotates both of its ranges "(W)".
- **`FB` is taken as eight digits on the FT-950 generation, against what two manuals draw.** The
  FT-950's and FTdx1200's `FB` tables lay out eleven parameter digits where their own `FA`
  tables lay out eight — in the same manual, for a parameter both give the identical range
  (`…-56000000`, which needs eight). The FTdx3000's manual repeats the same eleven, and the
  FTdx5000's and FTdx9000's draw eight for both. Eight is the only self-consistent reading and
  is what remoses sends, but the drawing is what it is and this wants confirming on hardware.
- **The FTdx9000's `TX` read is spelled `TM;` in its manual.** That is the only place the
  spelling appears; there is no `TM` row in its command list, and its own answer to the read is
  a `TX` frame. remoses sends `TX;`, as on every other model. If the manual is literal rather
  than mistaken, PTT on that radio would read as a timeout — which is visible and safe, where
  the reverse mistake would not be.
- **The FTdx9000 has no `ID` because its command list has no row for one.** That is an absence
  rather than a documented refusal, so it is worth a minute on hardware. Not asking is the
  asymmetric choice either way: if the radio does answer `ID;`, remoses loses only the identity
  cross-check, where sending it to a radio that does not would answer with silence and fail
  every connect.
- **On the FT-991A remoses can see C4FM but not which sub-mode will transmit.** DN versus VW is
  `EX` menu item 090 there, a persistent setting orthogonal to the mode, and remoses does not
  write `EX` for the same reason it does not write `KM`. On the FTX-1 the sub-mode *is* the mode
  code, so it is visible.
- **The FTdx10's `AI` works only over its USB CAT port.** Its manual says so and no other does,
  which matters for a rig reached through the RS-232C jack or a serial-to-TCP bridge: that
  station is poll-only.
- **The FT-891 does not push `FA`/`FB`.** Its command list marks them AI = `X` where every other
  model marks them `O`. `IF` is pushed there, and the fast poll reads `IF;` regardless, so this
  costs nothing today.
- **The FT-950's `SH` documentation contradicts its own bandwidth table.** The command row gives
  the parameter as `00`–`13`; the table below it runs to `20`, and indices 14 to 20 are the only
  way to reach 2450–3000 Hz in wide SSB. remoses uses the table. If the command row is right,
  those seven requests would answer with silence and cost a timeout — which is why the row is
  recorded here rather than quietly discarded.
- **The FTdx1200's two ID numbers are recorded as equals.** `0582` means the optional FFT-1 unit
  is fitted and `0583` means it is not, so both match the same profile and neither warns. remoses
  does not act on the difference: nothing it controls depends on the FFT-1.

Every one of these is a question one radio can settle in a minute with `debug_wire` on (§6.1) —
including the ones that would otherwise be silent, since a Yaesu refusing a command usually
answers with nothing at all, and the trace is the only place the sent frame and the missing
answer are both visible.

### 5.7 The FT-857 and FT-897: Yaesu's other CAT

Transcribed from the CAT Operation chapter of four operating manuals — FT-857, FT-857D, FT-897,
FT-897D. It is the same chapter four times. The two FT-857 charts are identical to the value,
the two FT-897 charts are identical to each other, and the pairs differ only in typesetting and
in three printing slips noted below. **Nothing in the wire format varies between these four
radios**, which is why §5.6's per-model table has no analogue here and why the profiles carry
little more than a label.

This is not the `yaesu` dialect with different values. It is a different protocol:

| | ASCII Yaesu (§5.6) | FT-857/FT-897 |
|---|---|---|
| Frame | `FA014250000;` — letters, digits, `;` | `43 97 00 00 01` — **five bytes, always** |
| Command position | first | **last** |
| Frequency | decimal digits in Hz | **packed BCD in units of 10 Hz** |
| Mode | a character, `2` = USB | a byte, `02` = **CW** |
| Answer framing | `;` terminates | **nothing terminates anything** |
| Commands | hundreds | **seventeen** |
| Push updates | `AI1;` | **none at all** |

`43 97 00 00 = 439.700 MHz` and `01 42 34 56 [01] = 14.23456 MHz` are the manuals' own worked
examples, and are the only statement anywhere of what the four bytes mean. Ten hertz is also
these radios' finest synthesizer step, so the field is exactly what the radio can tune; a finer
request is rounded to the nearest step before it goes out, and the read-back reports where it
landed.

**There is no framing, and that is the design problem.** An answer has no terminator, no
length, no opcode, no checksum and nothing that identifies it. Answers are one byte or five, and
the two are not distinguishable by content — an acknowledgement of `00` and the leading
`100/10 MHz` digit pair of a status answer on the 1.8 MHz band are the same byte. The only
thing that knows where a frame ends is **the command that provoked it**.

That is why `backend.ReplyFramer` exists. It is one optional method, implemented by this backend
and by no other, that the session calls immediately before a request goes on the wire — *holding
the write lock*. The lock is the whole point. A backend that recorded the fact for itself would
have to do so before calling `Do`, and therefore outside that lock, where the poller and an HTTP
setter overlapping is the ordinary case rather than a rare one: two goroutines could store in one
order and write in the other, the reader would frame the wrong number of bytes, and **the stream
would never recover**. Concurrency belongs to the session (§3), so the fact belongs there too.

**Every command is acknowledged, and no manual says so.** The command chart lists a reply for
the three reads only, but the radios answer everything else with a single byte. remoses waits
for it — every command in this backend goes out through `Do`, and there is not one `Send` call
in it. That is framing rather than thoroughness: on a delimiter-free stream an unconsumed byte
is not harmless noise, it becomes the first byte of the next answer and offsets everything after
it permanently. `yaesu` and `kenwood` can fire a set off and never look, because a stray frame
there is skipped at the next `;`. Here there is no next `;`.

Its **value is never read as a verdict**. `00` and `F0` are both reported in the field, `F0`
meaning roughly "already in that state" — which is exactly what a redundant unkey is, and the
dead-man path (§12) sends those by design. Treating a value as a rejection would make `ForceRX`
report a failure for doing its job.

**A slipped stream cannot resynchronise in place — so it is made to fail instead.** With no
delimiter there is nothing to hunt for. What remoses can do is *notice*: the status answer's four
BCD bytes are eight nibbles that must all be 0–9, so a frame offset by even one byte fails that
test about forty-nine times in fifty. The decoder reports it not-OK, the session turns that into
a poll failure, and five consecutive failures tear the connection down (§6). The reconnect starts
a clean stream. That path is the entire reason the check is there. An unknown *mode* byte is
deliberately **not** treated the same way: the frequency beside it already proved the framing was
right, so it publishes no mode and leaves the frame good.

**What these radios simply do not have**, each an absence in a seventeen-command set that found
room for DCS codes and repeater shifts:

- **Transmit power.** No opcode reads or writes it; `RF POWER SET` is a front-panel menu item.
  `power_watt_accurate` is false and `max_power_w` unset, so the session refuses a request in
  watts before it reaches the backend, and a request in percent is refused there.
- **Filter width or filter slot.** The optional YF-122S/C/CN filters are chosen with the front
  panel's `[B]` and `[C]` keys. Between this and the above, **the slow poll tier is empty** —
  power and filter are what it carries everywhere else.
- **CW over CAT.** No keyer buffer command, so the type does not implement `MorseSender` at all,
  `cw_method` is `none`, and the daemon names `serial_key` as the fix — the §5.4 lesson that a
  successful type assertion produces failures that look like success.
- **Push updates.** No `AI`, no Transceive, nothing unsolicited. A front-panel knob movement is
  invisible until the next poll. The fast tier is the only source of state here rather than a
  safety net behind a push channel.
- **A VFO by name.** The only VFO command, opcode `81`, is a **blind toggle**: it swaps A and B
  and nothing anywhere reports which one it landed on. `caps.vfos` therefore lists only
  `current`, and `SetFrequency` refuses `A` and `B` rather than tuning whichever VFO the operator
  was on and calling it A.
- **An identity.** There is no `ID` command in this generation, so unlike every other backend
  there is nothing to cross-check the configuration against. What the operator wrote is all
  remoses will ever know — which is also why the model is required rather than defaulted.

**The two status bytes need care in one place.** Both pack a four-bit meter into the low nibble
and three flags above it, and `F7` — the transmit one, and the only source of PTT, since no bulk
answer here carries a TX/RX flag — has **inverted** polarity on PTT and split: zero means the
thing is happening. remoses publishes its power meter and its HI SWR flag **only while the PTT
bit says the radio is keyed**. Nothing in a transmitter status byte is documented as meaningful
in receive, and these radios are reported to answer `FF` there, which decodes to a plausible
full-scale power reading and a high-SWR alarm on a radio sitting quietly in receive. For the same
reason the receive-status read `E7` is skipped entirely while transmitting: a transmitting radio
is not measuring a received signal.

The power reading goes into `power_meter` (§11.4). It went into `s_meter` when this backend was
written, for want of a forward-power field to put it in — a compromise that made a transmission
drive the receive signal bar to full scale. `State` carries transmit meters now, and this is what
they are for. HI SWR is a threshold flag rather than a ratio, so it is published as 0-or-1 on a
scale of 1 and no `swr_ratio` is derived from it: one bit cannot carry one. It is the only
transmit fault these radios report over CAT, and a remote operator who cannot see the front panel
is exactly who needs it.

**Modes, and the two that need naming.** The status answer's table is the larger of the two the
manuals print, and it disagrees with the mode-set table in one direction: `06` is WFM, which is
readable and has no set code. So `WFM` is the one mode that can appear in `state.mode` and never
in `caps.modes` — reporting it as FM would call a 200 kHz passband a 15 kHz one.

`0A` is `DIG`, and it is one mode for the same reason `C4FM` is one mode on an FT-991A: which
digital mode it *is* — RTTY-L, RTTY-U, PSK31-L, PSK31-U, USER-L or USER-U — is menu item 038, a
persistent setting orthogonal to the mode that no CAT command reads. Mapping it to FSK because
RTTY-L is the factory default would report a radio sitting in PSK31 as RTTY: the IC-910H failure
of §5.4 again. `0C` is `PKT`, which is packet on FM, and maps to FM with the DATA flag exactly as
the ASCII backend's own DATA-FM code does — so `SetMode(FM, data)` reaches it with no special
case. There is no USB-DATA code anywhere: `DIG` and `PKT` are the data modes.

**Where the four manuals disagree with each other**, and what remoses does about it:

- **The FT-897 pair omits `82` (CW-N) and `88` (FM-N) from the status table** and prints `88` in
  its own mode-set table two columns away; both FT-857 manuals list them in each. remoses decodes
  them on all four. The omission is on the side that costs only a reading, and following it would
  leave an FT-897 in FM-N reporting the mode it was in before.
- **The FT-897 pair prints `P1 = 00: "+" OFFSET` and `P1 = 00: "-" OFFSET`** on adjacent lines of
  the clarifier row, where the FT-857 manuals read `P1 ≠ 00` for the second. Immaterial here —
  remoses does not implement the clarifier — but it is the clearest evidence that the FT-897
  chapter is a re-typeset of the FT-857 one rather than an independent document.
- **Both FT-857 manuals print the squelch bit as `0: Squelch "OFF"` and `1: Squelch "OFF"`.** The
  FT-897 manuals have `1: Squelch "ON"`, which is the reading that makes sense. remoses drops
  that bit — `State` has no squelch field — so nothing turns on it.
- **The two charts swap the titles of opcodes `09` and `F9`** between "Repeater Offset" and
  "Repeater Offset Frequency". Neither is implemented.

**Assumptions worth revisiting on hardware.** The three that mattered most — the undocumented
acknowledgement, `FF` in receive, and the BCD frequency field — have now been seen on a radio;
see "Confirmed on hardware" below. What is left:

- **`F0` as "already in that state"** has still not been observed. An FT-857D answered `00` to
  every set it was given, including the redundant unkey that was the case `F0` was expected for.
  remoses does not act on the value either way, so being wrong about it costs nothing.
- **The three coverage gaps** (56–76, 108–118, 164–420 MHz) are unprobed. Only the outer bounds
  are enforced, and no manual says what one of these radios does with a frequency inside a gap.
  Refusing a frequency the rig would have tuned is worse than letting it ignore one it will not,
  and the read-back reports which happened.
- **The FT-817 and FT-817ND are the same protocol and are not profiled**, because remoses has no
  manual for either and every value here came from one. An owner can name an FT-857 and find out,
  accepting that the label will then say FT-857.

**One rough edge that this radio turned into a bug, and it was `ApplyPatch`'s rather than this
backend's.** A `PATCH` naming only a mode carries the *current* data-mode flag forward, because
on a Kenwood the two really are orthogonal (§5.2). They are not here: DATA is folded into the
mode code and only FM has a data variant. So a radio sitting in PKT and asked for
`{"mode": "CW"}` was asked for CW-with-data, which does not exist, and was refused with 422.
This was written up as a rough edge on the grounds that the refusal names the two data modes and
so explains itself. On hardware it is worse than that: PKT is reachable from the front panel, and
from PKT **every** bare mode change was refused — CW, USB, LSB, AM, and DIG, this radio's other
data mode. An operator could be left in PKT with no way out over CAT.

So `ApplyPatch` now drops a flag it carried forward and retries the write plain, under two
conditions. The client must have said nothing about data mode, so an explicit `data_mode: true`
on an impossible pairing still fails — that request really was made, and answering it with a
different one would be the "succeeds and means something else" failure this document keeps
returning to. And the error must be `ErrUnsupported`, which a backend raises from its own mode
table before anything reaches the wire, so the retry cannot be a second write to a radio that has
already moved. Where the pairing does exist the flag still carries: FM out of PKT stays PKT.

That is the fix the previous note here proposed as "teaching `ApplyPatch` to ask a backend whether
the target mode has a data variant", arrived at from the other end. The backend answers by
refusing rather than by being asked, which needs no new interface and covers the ASCII Yaesus and
the TS-890S/TS-990S — which fold DATA into the mode code too — for nothing.

The serial settings are worth stating because they are not the defaults: **4800 bps** out of the
box (menu 019 `CAT RATE`, also 9600 and 38400), 8 data bits, no parity, and the manuals specify
**two stop bits** — see `remoses.example.yaml`. Menu 020 `CAT/LIN/TUN` must also be set to `CAT`,
or the rear-panel jack is driving a linear amplifier instead.

#### Confirmed on hardware

An **FT-857D** with two optional filters fitted, on an FTDI cable at 38400 bps and 8N2, with
`debug_wire` (§6.1) on throughout. It is the first radio of this generation remoses has been
connected to, and the first Yaesu of either generation.

Confirmed on the wire: the five-byte block with the opcode last; the packed-BCD frequency field
in units of ten hertz, across the whole 100 kHz – 470 MHz range, with a finer request rounded to
the step (14.070123 → 14.070120 MHz); every mode the set table has; and both status bytes, `F7`
answering `FF` in receive and `03` under a ten-watt carrier — bit 7 clear for transmit, PO nibble
3. Frequency and mode were also seen tracking the front-panel dial at the poll rate, which on
this generation is the only way they can be.

**The undocumented acknowledgement is real.** Every set answered with a single byte, and the
value was `00` in every case: set frequency, set mode, PTT on, PTT off. The entire framing design
of this backend rests on that byte existing, and it does.

**`FF` in receive is real too**, which is what the transmit meters are gated on. Ungated, an idle
radio would have published a full-scale power meter and a high-SWR alarm.

**`E7` is skipped while keyed, and the trace shows it.** Through a transmission every poll is
`03` then `F7` and nothing else, with `E7` resuming on the tick after `88`.

**`06` WFM decodes, and the asymmetry holds in both directions.** Tuning to 100 MHz put the radio
into WFM by its own band memory; remoses read the code back and published the mode, while
`{"mode": "WFM"}` is refused because no set code exists. That is the one mode that can appear in
`state.mode` and never in `caps.modes`, and both halves were seen.

**Three things the radio does that no manual here mentions:**

- **A mode change moves the frequency.** Selecting CW adds the radio's CW pitch offset to the
  displayed frequency — +600 Hz on this one — and DIG subtracts its own shift, −2120 Hz. So
  `{"mode": "CW"}` alone leaves the dial somewhere else. This is precisely the case `ApplyPatch`'s
  mode-before-frequency ordering (§8) exists for, and this is the first radio here to demonstrate
  it mattering rather than merely being prudent: `{"frequency": F, "mode": "CW"}` lands on `F`
  because the frequency write follows the mode write and undoes the shift. From the trace: set
  mode FM, read back 3559400, set frequency 3560000, read back 3560000.
- **CAT PTT in CW transmits nothing.** The radio keys — `F7` reports transmit and the T/R relay
  closes — and the PO meter stays at 0, because in CW the transmitter needs the key line. So
  `serial_key` (§11.2) is not merely the only way to *send* CW here, it is the only way to produce
  RF in CW at all, and a CAT tune-up needs FM or AM. The useful corollary is that CW is the safe
  mode for exercising the transmit interlocks, which is how the ones below were fired without
  occupying a frequency.
- **A radio switched off is silence, not refusal.** Unlike an IC-7610 in standby (§11.6), which
  answers every command with a rejection, this one simply stops answering while its USB adapter
  stays present and dialable. The standby detection therefore does not fire, and should not:
  five poll timeouts tear the connection down and `connected` goes false, which is the honest
  report. Reconnect attempts then time out one per backoff step — capped at ~5 s — until the radio
  returns, at which point the first `03` answers and the session comes up on the state it left.

**The safety interlocks were fired on this radio**, in CW where nothing reaches the antenna:
`limits.bands` refused 14.070 and 7.030 MHz while allowing 3.560 and 28.030; a five-second
`tx_timeout` forced receive mid-transmission; and a lapsed lease produced `lock expired` →
`forcing receive` → `88` on the wire, with read-only polling correctly not renewing it.

**And it found three bugs, none of them in this backend** — which is the same lesson §5.4 records
for the IC-7610, that a first connection tests the layers above the protocol at least as hard as
the protocol. Two were in `ApplyPatch`: a named VFO validated after the empty-request
short-circuit, so `{"vfo": "B"}` was answered 200 by a radio whose only VFO command is a blind
toggle, and the data-mode carry-forward above. The third was in `remoses-cli`, which drew
`power  0 %` and `passband  0 Hz` for commands this radio does not have — the "absent rather than
zero" rule of §11.4 not yet applied to the three fields that predate it.

**Still unexercised here: CW itself.** `serial_key` needs a second serial port, and this radio was
reached over the only one on the machine, so the keying path — confirmed on an IC-7610 on both
DTR and RTS (§11.2), and not Yaesu-specific — has still never been run against an FT-857. The
desynchronisation check has not been provoked either: nothing in a healthy session offsets the
stream, and forcing one would mean corrupting the port from outside.

---

## 6. Session lifecycle

**Connect** → open port → apply rig init (`AI2;` on Kenwood; Icom Transceive is menu-only,
see §5.2) → full state read → mark `connected` → start poller.

> **Opening a serial port keys the transmitter, briefly.** `go.bug.st/serial` asserts DTR and
> RTS by default; on an interface wired for PTT or CW keying that means transmitting at
> startup. remoses opens both lines low via `InitialStatusBits`, but the unix driver still
> emits a **few-millisecond pulse** before that takes effect. A remoses restart will therefore
> produce a brief key/PTT blip on any rig whose PTT or key line is driven from that port. This
> is a driver limitation, not something the daemon can fully suppress — prefer a dedicated
> keying adapter, or an RC delay on the line, where it matters.

**Every port is opened with DTR and RTS low, then driven to its configured state.** The levels
are `port.dtr` and `port.rts`, defaulted to `high` for a CAT port and left low for the keying
port, which remoses builds in code.

That is two steps rather than one argument to the open, and the second step is not belt and
braces: it is the only part that works on some hardware. A **TS-590S** on its built-in USB
bridge answered *nothing whatsoever* when the port was opened with both lines already
high — correct speed, correct device, `AI2;` and `ID;` going out well-formed, and not one byte
coming back, for as long as the daemon cared to retry. The same port opened with both lines low
and then raised answers `ID021;` immediately. What that radio reacts to is the **low-to-high
transition**, not the level, and passing the levels in `Mode.InitialStatusBits` produces no
transition at all.

It cost an evening to find, because a standalone probe using the library the same way worked on
the first try and the daemon never did; the difference was three lines of setup that looked
equivalent and were not. Opening low also happens to be the safe direction for the other kind of
port, where DTR or RTS is a key or PTT and asserting one at open would transmit — such a port is
configured low and never gets raised.

**Poll** at two rates: `interval` for volatile values (frequency, mode, PTT, S-meter) and
`slow_interval` for stable ones (power, filter). With transceive/AI enabled the poller is
mostly a safety net against missed pushes.

On Kenwood the fast poll is a single `IF;` — one 38-character reply carrying frequency,
RIT/XIT, TX/RX state, mode and split — plus `SM0;`. The exception is Data mode, where `IF;`
does not answer, so the poller checks `DA;` and falls back to discrete `FA;` / `MD;` / `RI;`
queries. On Icom the fast poll is `03`, `04`, `1C 00`, `15 02`.

**Disconnect** — USB serial dies when a cable is pulled or the rig is switched off. Treat the
port as a supervised resource: on read error, close, mark `connected: false`, publish a WS
event, and retry with exponential backoff (100 ms doubling, **capped at 5 s**). Re-open **by
VID/PID + serial** where configured, not by device path — `/dev/ttyUSB0` is not stable across
replug.

The ceiling was 30 s until it was measured. Pulling and reseating a USB cable on the IC-7610
took **56 seconds** to recover, of which roughly 35 were the supervisor asleep at the ceiling
after the cable was already back. The ceiling is not a throughput knob: it is *how long a radio
that is plugged in can go on being reported as disconnected*, and half a minute of that reads
as a broken daemon. What it buys is proportionally tiny — a failed dial on `port.device` is one
`open()` on a path that is not there, and even `port.match`'s USB enumeration is nothing every
five seconds. `remoses-cli` uses the same 5 s ceiling for its reconnect to the daemon, where
the argument is stronger still: somebody is watching that display.

A dead port is not the only failure mode. A rig switched off behind a powered USB adapter
leaves the port open and readable while answering nothing, so the session also counts
consecutive poll failures and tears the connection down after five. Without that it would
poll into the void indefinitely and keep reporting a stale snapshot as live.

**But a rig that refuses a command is not a broken link.** Rigs differ in which of the
optional commands they implement, and they decline in protocol-specific ways: an Icom
acknowledges an unimplemented command with `FB` instead of returning data, a Kenwood answers
`?;`, and a Yaesu that will not run a command just now answers `?;` too — which remoses reads
as *busy* and retries on the next tick (§5.6), never as a reason to disable anything. Only
transport loss and outright silence are treated as fatal — anything else means the radio
replied, which proves it is still there. Concretely:

- a **slow-tier** poll failure is never fatal. Power and filter width are nice-to-have, and
  losing them must not put a radio whose frequency, mode, PTT and meters all work into a
  permanent reconnect loop;
- the **fast tier** stays fatal, because it is the liveness signal;
- a **read-back after a successful write** tolerates the same refusals. The write already
  happened; reporting it as an error would tempt the operator into repeating it.

Concretely: the CI-V backend polls `1A 03` for filter width because the IC-7610 documents it.
An Icom model that does not implement that command answers `FB` — a bare acknowledgement —
rather than data. Without the rule above, every such model would reconnect-loop forever
despite frequency, mode, PTT and metering all working. The rule is what lets one backend
serve the range of Icom models rather than only the one it was written against.

Enumeration uses `enumerator.GetDetailedPortsList()`. This is the one call requiring cgo on
macOS (IOKit); gate it behind a build tag if a cgo-free macOS build is wanted, falling back to
device-path matching.

### 6.1 CAT wire logging

Every rig fact in §5 was read out of a manual. When a radio is first plugged in and something
is wrong, the only question worth asking is **what actually went down the wire** — and the
ordinary logs cannot answer it, because they show decoded state, which is exactly the layer
where a wrong assumption is invisible. A mode table transcribed from the wrong column, a
manual that draws eleven digits where its own range needs eight, a command the documentation
lists and the radio does not implement: all three look identical from above, and all three are
obvious in one line of hex. `debug_wire` logs the bytes.

Two ways in, per radio, because they are for different moments:

```yaml
radios:
  - id: ic7610
    debug_wire: true        # leave it on at a site
```

```sh
remoses -debug-wire=ic7610,ts590sg -log-level=debug   # what you reach for at 2am
remoses -debug-wire=all -log-level=debug
```

The flag **only ever enables**, and it is merged into the configuration before anything reads
it, so there is one source of truth and the file cannot countermand the command line. An id
naming no configured radio is a startup error rather than a no-op, because the failure mode of
a mistyped id is silence, and silence is also what a broken radio looks like.

It is **off by default and per radio, because it is genuinely noisy.** Polling runs at 2 Hz per
radio, so a connected rig with nothing happening still produces several frames a second — a
Kenwood fast poll alone is `IF;` plus `SM0;` and their answers — and a station with three
radios traced at once is unreadable. Trace the radio in question.

Every request written and every frame decoded is logged at **debug** level under the message
`cat wire`, with the radio, the direction, the length, and the bytes as **hex** — always,
since CI-V is binary and reading it as text would be useless:

```
level=DEBUG msg="cat wire" radio=ic7610 dir=to-rig len=6 hex="FE FE 98 E0 03 FD"
level=DEBUG msg="cat wire" radio=ic7610 dir=from-rig len=11 hex="FE FE E0 98 03 00 50 02 14 00 FD" key=03 ok=true
```

A frame that is mostly printable ASCII carries a `text` rendering alongside, because Kenwood
and Yaesu frames are meant to be read by a human and hex alone would make diagnosing them
miserable:

```
level=DEBUG msg="cat wire" radio=ts590sg dir=to-rig len=3 hex="46 41 3B" text=FA;
level=DEBUG msg="cat wire" radio=ts590sg dir=from-rig len=13 hex="46 41 30 30 30 31 34 30 32 35 30 30 30" text=FA00014025000 key=FA ok=true
level=DEBUG msg="cat wire" radio=ts590sg dir=from-rig len=1 hex=3F text=? key=? ok=false
```

Details that matter when reading a trace:

- **`key` is how remoses correlated the frame**, in the backend's own key space (`FA` on
  Kenwood, `03` or `1A/03` on CI-V). It is the decoder's interpretation printed next to the
  bytes it interpreted, which is the comparison the whole feature exists to make.
- **`key=unsolicited` marks a frame that answered no request** — an Icom Transceive broadcast,
  a Kenwood or Yaesu AI push. These arrive on the same path as replies (§3, invariant B), and
  they are what a trace is most often opened for: a rig pushing traffic nobody expected is
  otherwise only visible as state changing for no reason.
- **`ok=false` is the rig declining**, an Icom `FA`, a Kenwood `?;`, or a Yaesu `?;` — which the
  yaesu backend reads as *busy* and retries rather than as a refusal (§5.6).
- **Control and high bytes are escaped** (`\r`, `\x1B`) rather than written into the log, so a
  rig emitting a CR cannot break a line in two and a NUL cannot vanish silently.
- **An inbound frame is logged as the decoder received it.** On Kenwood and Yaesu that is one
  byte shorter than the wire, because their splitter consumes the `;` terminator; a CI-V frame
  keeps its whole `FE FE … FD` envelope. Frame granularity is the point — logging raw read
  chunks would tear frames across lines and make them uncorrelatable — and what is being
  checked is what the decoder saw.
- **Undecodable frames are logged too**, with the decode error attached. A rig powering up
  emits noise, and that noise is evidence.

**It costs nothing when off.** The switch is a bool copied onto the connection at dial, tested
before anything is formatted, hex-encoded or allocated. That guard is not decoration: `slog`
evaluates its arguments eagerly, so an unguarded trace would format every frame of every radio
whatever the log level, and the reader goroutine is in the CW timing path (§11.1). Nothing is
logged under the mutex the reader contends for either — the reader traces a frame before it
touches the state cache or looks for a waiter.

Nothing is redacted, because there is nothing to redact: CAT carries frequencies and mode
codes, never credentials. A trace can be pasted into a bug report as it stands.

The questions this is built to answer are already written down: the assumptions marked "worth
revisiting on hardware" at the end of §5.4, §5.5 and §5.6. Does the FT-891 really report `1`
as LSB. Does this Icom actually answer `1A 03`. Does the FTdx9000 answer `ID;` at all, or does
it sit there until the timeout. Each is one session with the trace on.

---

## 7. Locking

Exclusive control is **per radio**, not per instance: one operator can work the IC-7610 while
another works the TS-590SG. Locks are advisory in the sense that they gate the API, not the
serial port.

### Lifecycle

| Step | Endpoint | Behaviour |
|---|---|---|
| Acquire | `POST /radios/{id}/lock` | 128-bit random token, base64url. `201` with token + `expires_at`. `409` if held by someone else |
| Use | any state-changing call | Token in `X-Remoses-Lock` header (preferred) or `remoses_lock_{id}` cookie. **Sliding renewal** — every accepted command resets the TTL |
| Renew | `POST /radios/{id}/lock/renew` | Heartbeat without issuing a command, for a client holding the rig while the operator thinks |
| Inspect | `GET /radios/{id}/lock` | Holder username, `expires_at`, `is_mine` |
| Release | `DELETE /radios/{id}/lock` | Immediate |

Tokens are opaque and compared with `subtle.ConstantTimeCompare`.

`force: true` on acquire steals a held lock, permitted only when `lock.allow_steal` is set.

### What requires a lock

- **Required:** `PATCH /state`, all `PUT /state/*`, `POST /cw`, `DELETE /cw`.
- **Not required:** every `GET`, the WebSocket, `/radios`, `/healthz`.

Requests missing a valid token get `423 Locked` (no lock held / not yours) or `409 Conflict`
(held by another user), with problem+json naming the holder and expiry.

### Expiry is a safety event

When a lock expires or is released while the radio is transmitting, the session **must**:

1. drop PTT / force RX,
2. flush the CW queue and send the rig's CW-abort,
3. publish a `lock` and a `state` event on the WebSocket.

A client that crashes mid-transmission must not leave a carrier up. This is the same code
path as the `tx_timeout` dead-man timer (§12).

> **Leases carry a generation number.** An expiry timer can fire while a renewal is blocked on
> the lock mutex, and would then expire the lease that had already replaced it — dropping PTT
> on a healthy, actively-renewed lock, mid-transmission. Stamping each lease with a generation
> and having the timer verify it before expiring is what prevents that.

---

## 8. REST API

Design-first: `api/openapi.yaml` is the source of truth. Handlers are hand-written against
Go 1.22+ `net/http` `ServeMux` — its pattern routing removes the need for chi, and for a
surface this small a server generator would cost more than it saves.

**Clients are generated, and remoses-cli is one of them.** `make generate` turns the document
into `internal/wire` with oapi-codegen — the types, plus the request plumbing for the two GET
operations `output-options.include-operation-ids` lists — and `internal/client` and
`cmd/remoses-cli` are built on that and nothing else. This is the difference between a spec
that is published and a spec that is used: the monitor reads the same document a third-party
client would, so a field the spec forgets to declare is a field the monitor stops displaying,
and it stops on a developer's machine rather than in somebody's browser.

Restricting the generated client to the read operations is also what keeps "a monitor cannot
change a radio" a property rather than a promise. There is no generated `PatchState` in the
binary to call by accident.

Drift is caught in three places, and each catches something the others cannot:

- **Routes.** A conformance test parses the spec and asserts every documented path and method
  has a registered route, and that no route exists which the spec does not document.
- **Bodies.** The same test pushes real responses — descriptor, state, CW status, lock,
  problem documents — through the generated types with **unknown fields refused**, and checks
  that every member the schema marks `required` is actually there. The first direction
  catches a field the daemon sends and the document does not declare; the second catches a
  promise the daemon does not keep, which matters because a generated client turns a required
  field into a plain value, where a missing one arrives as a zero that reads like a reading.
- **The generated code itself.** `make spec-check`, part of `make check` and of CI,
  regenerates `internal/wire` and fails if it differs from what is checked in. Without it an
  edit to the document that nobody regenerated would leave the two describing different APIs
  with every test passing.

| Method | Path | Lock | Purpose |
|---|---|---|---|
| `GET` | `/radios` | — | List: id, name, backend, connected, capabilities, lock holder |
| `GET` | `/radios/{id}` | — | Descriptor, capabilities, configured limits |
| `GET` | `/radios/{id}/state` | — | Cached snapshot + `age_ms` |
| `PATCH` | `/radios/{id}/state` | ✔ | **Primary control verb** — partial, atomic, ordered |
| `POST` | `/radios/{id}/cw` | ✔ | Enqueue CW text |
| `DELETE` | `/radios/{id}/cw` | ✔ | Abort: flush queue **and** stop the rig |
| `GET` | `/radios/{id}/cw` | — | Queue depth, busy, wpm |
| `POST`/`GET`/`DELETE` | `/radios/{id}/lock`, `/lock/renew` | — | §7 |
| `GET` | `/ws` | — | WebSocket upgrade (§9) |
| `POST` | `/ws-ticket` | — | Short-lived ticket for browser clients |
| `GET` | `/healthz` | — | Unauthenticated liveness |

`PATCH /state` is the **only** control verb, rather than one primary verb plus a pile of
single-value endpoints, because **ordering matters**: on most rigs mode must be set before
frequency (mode selection can change the filter and carrier offset), and "14.025 CW" should
be one transaction, not two racing ones. An earlier draft added `PUT /state/frequency` and
friends as curl-friendly sugar; they were dropped, because `-d '{"frequency":14025000}'` is
no harder to type than `-d '{"hz":14025000}'` and a second way to do the same thing is more
surface to document, test and keep consistent. Each is one route-table line if wanted later.

```http
PATCH /api/v1/radios/ic7610/state
X-Remoses-Lock: 9tQ2… 
{ "mode": "CW", "frequency": 14025000, "power_pct": 40, "filter_slot": 2 }

200 OK
{ "frequency": 14025000, "mode": "CW", "data_mode": false, "filter_slot": 2,
  "power": { "pct": 40.0, "watts": null, "native": 102 },
  "ptt": false,
  "s_meter": { "raw": 78, "scale": 255, "s": 5.5 },
  "cw": { "busy": false, "queued": 0, "wpm": 28 },
  "seq": 4471, "updated_at": "2026-08-04T20:11:04Z", "age_ms": 120 }
```

The same request against `ts590sg` would come back with `"power": {"pct": 40.0,
"watts": 40, "native": 40}` and `"s_meter": {"raw": 9, "scale": 30, …}` — same shape,
honest units.

Apply the write, then **read back from the rig** before responding — rigs silently clamp
things (per-band power limits, unsupported filter slots), and the client should see reality
rather than intent.

Errors are RFC 9457 `application/problem+json` with a `radio_id` extension member.
Distinguish clearly, because a remote operator needs to know whether to retry or go check a
cable:

| Status | Meaning |
|---|---|
| `409` | Lock held by another user |
| `422` | Out of band, or capability unsupported by this rig |
| `423` | No lock held, or token expired/invalid |
| `503` | Radio disconnected |
| `504` | Rig did not answer within the timeout |

---

## 9. WebSocket API

`GET /api/v1/ws` — one connection carries **all** state changes for **all** radios.
Many concurrent clients are supported; the stream is read-only and **needs no lock**.
Optional `?radios=ic7610,ts590sg` filter.

Library: **`github.com/coder/websocket`** (the former `nhooyr.io/websocket`) — small,
context-aware, actively maintained.

### Authentication

- **Programmatic clients** send the normal HTTP Basic `Authorization` header on the upgrade.
- **Browsers** cannot set headers on `WebSocket`, so they first call
  `POST /api/v1/ws-ticket` (Basic auth) and receive a single-use ticket valid ~30 s, passed
  as `/ws?ticket=…`.

### Message envelopes

Server → client, newline-free JSON objects, all carrying `type`:

```jsonc
{ "type":"hello",  "version":"…", "radios":["ic7610","ts590sg"], "server_time":"…" }

// Full snapshot per radio on connect, then deltas.
{ "type":"state",  "radio":"ic7610", "seq":4471, "ts":"…", "state":{ … } }
{ "type":"delta",  "radio":"ic7610", "seq":4472, "ts":"…", "changed":{"frequency":14025300} }

{ "type":"cw",     "radio":"ic7610", "seq":4473, "ts":"…", "busy":true, "queued":12, "wpm":28 }
{ "type":"conn",   "radio":"ts590sg", "seq":4474, "ts":"…", "connected":false, "error":"port closed" }
{ "type":"resync", "radio":"ic7610", "seq":4474, "ts":"…" }  // dropped events; refetch state
```

Client → server is minimal: `{"type":"ping"}` and `{"type":"subscribe","radios":[…]}`.
All control stays on REST, so the WebSocket has no authorisation surface.

**These are schemas, not prose.** `api/openapi.yaml` declares `WSMessage` as a `oneOf` over
the six frames with `discriminator: {propertyName: type}`, and `WSClientMessage` the same for
the two a client may send. OpenAPI has nothing to say about a WebSocket — the operation can
only promise a 101 — so the frames are declared under `components/schemas` and the generator
is told not to prune what no path references. The discriminator is the point: it is the
difference between a generated client offering `AsWSDelta()` on a frame whose type says
`delta`, and one offering a `state` field that is null on five frames out of six.

A delta needs a type of its own, because `State` requires eighteen members and a delta names
only what moved. So the state fields live in `StateFields`, with everything optional, and the
two schemas that use it say what they promise: `State` is `StateFields` plus the required
list, `StateDelta` is `StateFields` as it stands. Sending whole snapshots instead would make
one schema do, and for a stream with few fields or few messages that is the better trade —
but fifty fields per radio at a two-a-second poll is real bandwidth over a link that may be
somebody's phone.

`radio`, `seq` and `ts` are on **every** frame about a radio, discrete `cw` and `conn` events
included, so a gap is detectable without correlating an event against a state message the
rate limiter may be holding back. `hello` is the one frame without them, because it describes
the connection rather than a radio.

Applying a delta is a JSON merge, and deliberately: `changed` uses `State`'s own names, so a
client overwrites the members it carries and leaves the rest. A member present and **null**
means the reading has gone away — the transmit meters, which exist only while the radio is
keyed — and that is what stops the last transmission's SWR sitting on a display for ever.

Those four members are declared `nullable` in the document, and the generator is told to map
that to a three-state type rather than to a pointer. This is the subtle half of the whole
exercise. The prose specified a difference between `{"swr": null}` and `{}`; the schema did
not, so the obvious mapping — `*Meter` with `omitempty` — collapsed both to nil, and a client
generated from the document could not implement the document. The bug was invisible in
remoses-cli, which merges the raw bytes and so never asked the type, and would have surfaced
first in somebody else's client as an SWR reading that never went away.

remoses-cli still merges the bytes, but for the other reason: a field-by-field merge of the
decoded form is fifty lines that fall behind the spec the first time a field is added.

### Backpressure

A slow client must never stall a rig session. One hub subscription fans out into per-client
queues with non-blocking hand-offs, in two lanes:

- **State lane — coalescing.** A `map[radioID]Event` holding only the newest. Being keyed by
  radio it is bounded by the radio count and **cannot overflow**, however long a client
  stalls. Drained at most once per radio per `ws.min_interval` (default 50 ms), because
  spinning the VFO knob with Transceive on produces hundreds of updates a second.
- **Event lane — bounded FIFO, no coalescing.** `cw` and `conn` events are discrete and must
  not be merged. On overflow the lane is dropped and `resync` is flagged.

Every event kind also feeds the state lane, so a client's snapshot and its `seq` stay
continuous even when discrete events are dropped. `seq` is never fabricated or reordered: an
event whose `seq` a newer snapshot already covers is discarded, so the written stream is
strictly increasing per radio.

Increasing, but not contiguous, and that distinction is the contract: coalescing means values
are **skipped**, so a jump from 37 to 41 says a client was spared three updates a later one
superseded, not that anything was lost. What must not be missed silently are the discrete
events, and `resync` is how the server admits it dropped one. Its `seq` is the last version
the connection was actually sent, so it says where the hole starts.

`resync` is itself rate-limited per radio — each one costs the client a REST refetch, so a
burst of drops must not become a burst of refetches.

> **`lock` frames are not implemented yet.** `internal/lock` has no event feed, and the only
> way to synthesise them would be a polling loop, which is worse than the gap. Clients see
> lock state through `GET /radios`. Giving the lock manager an `OnChange` callback alongside
> its existing `SetOnExpire`, and handing it to the hub, is the clean fix.

Keepalive: server pings every `ws.ping_interval`, closes after two missed pongs. This is not
cosmetic — detecting a dead client promptly is what releases its lock and drops PTT.

---

## 10. Authentication

HTTP Basic on every request, over TLS.

- **bcrypt hashes in config, never plaintext.** `remoses passwd` generates them at the
  configured cost.
- **Low cost by choice** (default 8, ≈25 ms). Combined with a **TTL verify-cache** keyed on
  `sha256(user||":"||pass)` (default 60 s), polling clients do not burn CPU on KDF work.
- **Constant-time comparison**, and always run a bcrypt compare — against a dummy hash for
  unknown users — so response timing does not enumerate accounts.
- **Authorisation is per instance.** Any authenticated user may access any radio. There are
  no per-radio scopes and no read-only role in v1.
- **TLS is effectively mandatory.** Basic auth replays the password on every request. remoses
  refuses to start when `listen` is a non-loopback address without `tls` configured, unless
  `server.insecure: true` is set explicitly. Terminating TLS at a reverse proxy is fine —
  bind to loopback in that case.

---

## 11. CW keying

Both methods share one queue, one API, and one status model. Only the sender differs:

```go
type CWSender interface {
    Enqueue(text string) (queued int, err error)
    Abort()
    Status() CWStatus          // busy, queued chars, wpm, est_remaining
    SetSpeed(wpm int) error
    Charset() string
}
```

`catSender` wraps `backend.MorseSender`; `serialKeySender` drives modem control lines.
Capability flags in `GET /radios/{id}` report which is in use, the accepted charset, and the
usable wpm range, so clients can adapt.

Prosign syntax is canonical at the API — `^AR`, `^BT`, `^SK` — and translated per backend
(§5.3). Clients never encode a rig's dialect.

### 11.1 CAT sending (`method: cat`)

The engineering here is the **buffer-refill loop**, since rig buffers are small: 30 characters
on the IC-7610, a fixed 24-character block on Kenwood. The two rigs need genuinely different
loops, because only one of them can be asked how full it is.

Common to both: an unbounded server-side queue, chunked on **word boundaries**, never
mid-character, and never splitting a prosign from its marker.

**Kenwood — closed loop.** `KY;` answers `KY0;` (space available) or `KY1;` (full), and
pushing to a full buffer is a hard error, so the sender is a straightforward feedback loop:

```
for chunk := range chunks {
    for  KY; != "KY0"  { wait ~1 element time }     // backoff, respects ctx cancellation
    write "KY " + pad(chunk, 24) + ";"              // pad spaces are not keyed
}
```

Poll interval should track speed — roughly one dit-time, floored at ~20 ms — rather than a
fixed constant, so the buffer refills promptly at 40 wpm without hammering the port at 15.

**Icom — open loop.** Command `17` has no buffer query, so drain is estimated from
`wpm × characters` (Morse element counting, not just character count, since `PARIS` timing
depends on which characters are queued) and reconciled against the rig's CW-busy indication.
Keep roughly one chunk in flight beyond the one being sent: too little produces audible gaps
between chunks, too much lengthens abort latency. Expose the depth as a config knob and
default it conservatively.

**Abort does two things** in both cases: drop the local queue *and* send the rig's stop
command (`KY0;` / `17` with `FF`), because up to a full buffer may already be inside the
radio and will otherwise keep transmitting.

Note that neither rig needs an explicit `TX;` / `1C 00` before keying — the transceiver keys
itself from the CW buffer. Asserting PTT first is a per-radio config option for stations that
need a sequencer or amplifier lead-in.

### 11.2 Serial keying (`method: serial_key`)

For rigs with no usable CAT CW buffer, remoses generates the Morse itself and keys a serial
control line — DTR or RTS, per config — optionally asserting the other line as PTT with
configurable lead-in and tail.

This is viable **only because remoses runs next to the radio** (§1). Text is buffered
server-side; the network is never in the timing path.

**Capabilities are composed, not taken from the backend.** `cw_method`, `cw_charset` and the
wpm range describe *the keyer that is installed*, which is a configuration choice, where a
backend's `Caps` describes *the radio*. Those disagree exactly here: `civ` reports `cw_method:
cat` because an IC-7610 does have a CAT buffer, but `cw.method: serial_key` on that radio keys
a DTR line and never sends command `17`. Publishing the backend's answer told clients the radio
was keying over CAT, offered them the rig keyer's restricted charset instead of the fuller local
table (which has `*`, and no `^` run-marker), and understated the speed range at both ends —
6–48 against the local keyer's 5–60. `Session.publishCaps` folds the sender's view over the
backend's, and does so on **every** capability refresh: a reconnect re-reads them from the
backend and would otherwise put the wrong answer back.

The CAT sender deliberately declines to override the wpm range. There the rig's own keyer binds,
its range is per model, and the backend already knows it; only the local keyer, which really
does generate the elements, replaces it.

**Confirmed on hardware, on both lines.** An IC-7610 with `USB KEYING (CW)` keyed from its
second USB port, full break-in and no `ptt_line`, sending 21 characters at 20 wpm against an
11220 ms estimate: **DTR took 11428 ms and RTS 11304 ms**, the rig's QSK raising PTT off the key
line either way and `1C 00` reporting it. `key_line` is therefore symmetric in practice, not
merely in the code.

Two things that look like faults and are not. PTT reads true for a moment *after* the queue
drains — that is the rig's own CW delay hanging on, not a control line left asserted; it clears
by itself. And opening a port wired this way produces a brief key-down click, for the reason §6
gives: a real if harmless transmission every time the daemon starts.

**Timing.** A dedicated keyer goroutine with `runtime.LockOSThread()`:

- Element unit is `1200 ms / wpm` (48 ms at 25 wpm). Dit = 1 unit, dah = 3, intra-character
  gap 1, inter-character 3, inter-word 7, adjusted by `weight`.
- Schedule edges against an **absolute start instant** — compute the *n*-th edge time and
  sleep until it — rather than accumulating `time.Sleep(unit)`, which drifts.
- Sleep until ~1 ms before each edge, then spin. Go uses high-resolution waitable timers on
  Windows (since Go 1.16), so this is mostly belt-and-braces, but CW is unforgiving and the
  cost is negligible.
- Measure jitter in tests: assert edge error stays within a few percent of a unit at 30 wpm.

**Port sharing.** The keying line may live on the CAT port or a separate one. Separate is
strongly preferred. When shared, `transport.Port` guards `Write`, `SetDTR`, and `SetRTS` with
a mutex (reads run unguarded in the reader goroutine). Modem-control ioctls are microseconds,
but a blocking CAT write can jitter an element — document this, and warn at startup when a
`serial_key` device equals a CAT device.

**Charset.** Locally generated Morse can support a fuller table than the IC-7610's restricted
set, including arbitrary prosigns. Report the difference through the capability flags rather
than assuming clients know.

### 11.3 CW API

```http
POST /api/v1/radios/ic7610/cw
X-Remoses-Lock: 9tQ2…
{ "text": "CQ TEST DE OH2XYZ ^AR", "wpm": 28, "mode": "append" }

202 Accepted
{ "queued_chars": 21, "position": 1, "est_duration_ms": 8400 }
```

`mode` is `append` or `replace`. Invalid characters get `422` naming the offending character
and the accepted charset. `DELETE /cw` aborts.

---

## 11.4 Transmit metering

Forward power, SWR and ALC, published as `state.power_meter`, `state.swr` and `state.alc` with
`caps.power_meter`, `caps.swr_meter` and `caps.alc_meter` saying which a radio has.

**They are read only while the transmitter is keyed, and absent in receive rather than zero.**
Both halves matter. Polling them in receive would spend two or three transactions per fast tick
to publish zeroes, and a client cannot tell a zero from a real reading of a transmitter working
into a dead load — so a bar drawn from one would be a lie in the direction that hides a fault.
Leaving the last transmission's values behind is worse still: a 3:1 SWR frozen on a display
after the operator stopped reads as something still happening, and the final sample of a
transmission is usually mid-decay rather than representative. `State.Apply` clears all three
whenever PTT is false, so no backend has to remember to; every radio that reports them reports
them only while keyed, which makes it a property of the state rather than of any protocol.

Each backend keeps its own `transmitting` flag, set from whatever it already decodes for PTT —
CI-V `1C`, Kenwood's `IF` and `TX;`/`RX;`, Yaesu's `TX`, rigctld's `get_ptt`. That means a
transmission started at the radio's own PTT switch is metered exactly like one remoses keyed,
and it costs no extra traffic. The reads are ordered so the PTT answer of the same tick is
decoded before the meter reads are chosen; one tick of lag at the very start of a transmission
costs a single sample and is cheaper than serialising every poll behind a PTT read.

**Where they come from, per family:**

| | Power | SWR | ALC |
|---|---|---|---|
| Icom | `15 11` | `15 12` | `15 13` |
| Kenwood | `SM` **while keyed** | `RM` → `RM1` | `RM` → `RM3` |
| Yaesu | `RM5` | `RM6` | `RM4` |
| FT-857/897 | TX status byte | high-SWR **bit** | — |
| rigctld | `RFPOWER_METER` | `SWR` | `ALC` |

Three of those rows have a trap in them.

**Kenwood has no forward-power command.** `SM` is the S-meter in receive and the *RF power
meter* in transmit — one command, two meters, chosen by whether the rig is keyed. This backend
used to file it under `SMeter` either way, which meant every transmission drove the receive
signal bar to full scale and left it there. It now goes to `PowerMeter` while `transmitting` is
set, which is both correct and the only reason `power_meter` works on that family at all.

**Kenwood's `RM` answers three times.** Its reference states it outright — "there are always
three types of responses: SWR, COMP, and ALC" — so one read produces `RM1`, `RM2` and `RM3`.
Each is decoded on its own and the transaction is completed by whichever arrives first; COMP is
decoded far enough to complete a transaction and then dropped, because State has nowhere to put
a compression reading and any of the three may be what a read is waiting on.

**Yaesu's `RM` answer is not the same length on both generations.** The FT-950 generation
answers `RM<meter><nnn>;` and the FT-710 generation appends three more fixed digits. The meter
numbers are identical in both — which is why this is family-wide rather than per model, unusually
for this backend — so reading the meter and the three digits after it and ignoring the rest
handles either without knowing which radio is on the other end.

### Scales, and one number remoses will not invent

`radio.Meter` is a raw deflection and the full-scale value that goes with it, in the radio's own
units: 0-255 on an Icom, meter dots on a Kenwood, a percentage from rigctld. Two per-model
details here are not guessable and were transcribed rather than assumed:

- **An Icom's ALC runs to 120**, not 255 — "0000=Minimum to 0120=Maximum" in both references.
  Published against 255 an ALC at full deflection would read 47%.
- **The IC-9700's power meter reaches 100% at 213** where the IC-7610's reaches 255. Against the
  wrong scale a radio at full power reads 84%.
- **The SWR meter is published against 120, the top of its calibration**, not the 255 its data
  field could hold. That one is a judgement rather than a transcription: the reference calibrates
  `0000` to `0120` and says nothing above, and scaling to 255 would draw a 3:1 SWR — which nobody
  should be transmitting into — at under half a bar. A meter that under-warns is worse than one
  with a short scale, so readings past the documented top pin instead, which is the right shape
  for "worse than the worst marked value".

`state.swr_ratio` is the exception to publishing raw numbers, and it is deliberately narrow. Icom
prints four calibration points for `15 12` — `0000`=1.0, `0048`=1.5, `0080`=2.0, `0120`=3.0 —
so a ratio can be interpolated between them, and that is transcription rather than invention.
Nothing else gets one: the IC-703 names the same command and calibrates nothing, and no Kenwood
or Yaesu reference calibrates its SWR meter at all, so those publish the bar and no figure. Above
the last documented point remoses reports no ratio rather than extrapolating — the curve is
undocumented, an SWR that high is a fault whatever the exact number, and printing "7.4:1" would
be a precise-looking figure about somebody's antenna that remoses made up.

rigctld is the one backend that gets a ratio without a table of its own: Hamlib's `SWR` level is
documented as a real ratio, its rig backend having already done the conversion. Its bar is drawn
against a top of 3.0:1, chosen to match the highest point Icom's own meter names so that the two
render alike, with the exact figure published alongside so nothing is lost when the bar pins.

---

## 11.6 Switching the radio itself

`{"power_switch": "off"}`, `"off_deep"` and `"on"`, gated by `caps.power_switch`. It is `18 00`
and `18 01` on Icom and `PS` on Kenwood and Yaesu.

**This is the one command whose success is indistinguishable from its failure.** A radio told to
switch off stops answering, the next poll times out, and the session tears the link down —
exactly what a pulled cable looks like. So the intent is recorded *before* the command goes out,
and the supervisor logs the disconnection that follows as expected rather than as a fault. Without
that, switching a radio off would produce an error every backoff, at error level, for as long as
the operator left it off: a success reported as an outage.

**Waking is harder, because it has to work in the state where there is no link.** What there
usually is instead is an openable port — an external CI-V interface stays powered, and a radio
whose USB survives its own power switch presents one too — and the supervisor is already looping
on it, dialling and failing to `Init`. So `PowerOn` **arms a request** rather than sending
anything: the supervisor consumes it on its next freshly opened port and sends the wake *before*
`Init`, which is the one moment a sleeping radio can be reached.

Racing the supervisor for the port would be the obvious alternative and is the wrong one. These
are exclusive devices; two dialers produce a wake that fails because the port was busy.

The wake is consumed once rather than retried. A wake that has been sent has been sent, and
repeating it on every attempt would hold a radio somebody switched off at the front panel
permanently on — remoses fighting the operator standing next to it.

### Standby: reachable, and switched off

There is a third connection state, and it was found by switching an IC-7610 off and watching what
remoses made of it. **It does not go silent.** Its CI-V circuit stays alive and answers **NG to
every command**, frequency read included. The link is perfect; the radio is asleep.

Neither of the other two states describes that. `connected: false` sends somebody to check a
cable that is perfectly seated, and `connected: true` leaves an operator staring at a frequency
that has not changed in ten minutes wondering why nothing works. So `state.standby` is its own
flag, published alongside `connected: true`, and `remoses-cli` shows `STANDBY` where it would
otherwise show `CONNECTED`.

The session **stays on the open port** rather than dropping it. Redialling would be wrong twice
over: it reports a healthy link as a fault, and it throws away the very port a wake has to go
through. So `awaitWake` parks there, republishes the state, and retries `Init` every slow poll
interval — noticing within one interval whoever wakes the radio, whether that is a `power_switch`
request or a hand on the front panel.

Commands in that state are refused with `ErrStandby`, a 503 whose message names the remedy:
`the radio is switched off; wake it with {"power_switch":"on"}`. It wraps `ErrDisconnected`, so
anything reasoning about "no usable radio right now" still matches with one check.

**This found a pre-existing bug, and an ironic one.** The failure counter that is supposed to
notice a rig which has stopped answering never fired, because §6 had taught it that *a rejection
means the radio is talking to us* — the fix for an IC-9700 that NGs one optional command in FM.
A radio in standby refuses **everything**, so every poll was excused and the session reported
`connected` over a snapshot going stale by the minute: the exact failure the counter exists to
catch, hidden by the fix for its opposite.

The line is now drawn by tier rather than by error kind. The slow tier carries the optional
values — filter width, data mode, break-in, the tuner — and rigs differ in which they implement,
so a refusal there still does not count. The fast tier is frequency, mode and whichever of PTT
and the S-meter the model has: what a radio must answer to be usable at all. A refusal there is
not a rig declining an extra, it is a rig that cannot do the basics.

### Two kinds of off, and one way in

Kenwood documents both, and the difference is the whole design:

> When turning the power Off by setting the P1 parameter to 0, more current is consumed than if
> you turn the power Off by operating the transceiver panel power switch. However, you can switch
> the power back On without any special procedures, using the PS command.

> When turning the power Off by setting the P1 parameter to 9, the same amount of standby current
> is consumed as if you turned the power Off by operating the transceiver panel power switch. In
> this case … 1) turn the flow control Off. 2) Send dummy data (;). 3) Wait for more than 200 ms.
> 4) Send "PS1;" within 2 seconds of sending the dummy data.

So `off` defaults to the shallow one — standby current in exchange for a wake that is one
command — and `off_deep` is opt-in. A remote station that cannot be woken is a station somebody
has to drive to, and that trade is the operator's to make deliberately.

Waking, by contrast, is **one method**. A caller should not have to know which kind of off a
radio was put into, least of all when it was the front-panel switch that did it — so each backend
tries the cheap wake first and escalates to its family's ritual only if that draws nothing:

| | Cheap wake | Escalation |
|---|---|---|
| Icom | `18 01` behind an `FE` preamble | none documented; the preamble *is* the ritual |
| Kenwood | `PS1;` | dummy `;`, wait >200 ms, `PS1;` inside 2 s |
| Yaesu | `PS1;` | dummy `;`, then `PS1;` inside the documented 1–2 s window |

Icom's preamble is a duration expressed in bytes, so its length is per baud rate — 150 at
115200 down to 7 at 4800 — and a rate between two tabulated ones rounds **up**. Too many `FE`s
cost milliseconds; too few cost a radio that does not wake.

None of the three is verified beyond the probe between the attempts. A radio coming up spends
seconds booting, far longer than any command timeout, so a stricter check would report failure on
a radio that is on its way up. The connection attempt that follows is the verdict.

**What remoses does not claim** is that a wake will work. That depends on wiring, not on the
command: a radio whose CAT arrives over its own USB may take the USB device down with it, leaving
nothing to send the wake-up to. `caps.power_switch` reports the command; the station decides
whether the radio can be reached afterwards.

---

## 11.5 The antenna tuner

`state.tuner` reads `off`, `on` or `tuning`; `{"tuner": "on"}` switches the matching network in
or out of line, and `{"tuner_tune": true}` starts a tuning cycle. `caps.tuner_control` and
`caps.tuner_tune` say which a radio has.

**They are two fields rather than one because a tuning cycle transmits.** The radio keys itself
for a second or two with nobody holding a switch, so `tuner_tune` is treated as a transmit
operation: it needs the lock, it is checked against `limits.bands` — a station that may not
transmit on a band may not tune into it either — and it arms the dead-man timer, so a cycle that
never ends is caught by the interlock that catches a stuck PTT. Keeping it out of the `tuner`
enum also means a client that reads the state and writes it back, which is an ordinary thing to
do, can never key a transmitter by echoing `"tuning"` at the radio it just read it from.

It does not wait for the cycle. The rig decides how long one takes and reports progress in its
own state, which the poller follows — on the fast tier while a cycle runs, because on the slow
one a whole cycle can begin and end between two reads.

| | Command | Off / on | Start a cycle |
|---|---|---|---|
| Icom | `1C 01` | `00` / `01` | `02`, which a read also answers while running |
| Kenwood | `AC` | P2 `0` / `1` | P3 `1` — `AC111` |
| Yaesu | `AC` | P3 `0` / `1` | P3 **`2` or `3`**, per generation |

**The Icom row is per model, and one of the reasons is a safety interlock.** On the IC-718
`1C 01` is *PTT* — its table has no `1C 00` row at all — so a "start tuning" sent there would key
the transmitter and hold it keyed, and nothing in the frame would say so.

The other reason is that plenty of these radios have no tuner, which is why `withTuner` is
applied per entry rather than defaulted in `modern()`. Every reference has now been read:

| Has `1C 01` | Does not |
|---|---|
| IC-703, IC-7300, IC-7300MK2, IC-7600, IC-7610, IC-7700, IC-7760, IC-7850, IC-9100 | IC-905, IC-910H, IC-9700, IC-718, IC-706 family, `generic` |

The split is the obvious one — HF sets have a matching network, the VHF/UHF-and-up sets do not —
except that the IC-9100 has one despite covering VHF and UHF, and words the tune trigger
"Manual tuning selection" where everything else says "Tuning". Same `1C 01 02` either way.

**Having a tuner is not the same as having one on this band.** The IC-9100's covers HF and
50 MHz; on 144 MHz and up the radio rejects the command. remoses does not pre-empt that with a
frequency test — the boundary is the rig's to enforce, and hard-coding one here would be a number
invented rather than transcribed — so the refusal surfaces as an ordinary 422 carrying the
radio's own NG. Worth knowing when reading a support question: a tune failing on 2 m is the radio
saying no.

This shipped for a day as a default of true for every modern Icom, and the IC-9700 caught it:
that radio advertised `tuner_control` and `tuner_tune`, answered NG to the poll every slow tick,
and would have shown an operator a Tune button that could only ever fail. A capability that is
defaulted rather than transcribed is one nobody has checked.

**The Yaesu row differs by generation.** The FT-950's `AC` reads "0: Tuner OFF, 1: Tuner ON,
2: Tuning Start"; the FT-710's reads "0: Tuner OFF (Tuning Stop), 1: Tuner ON, 2: -, 3: Tuning
Start". Being wrong fails safe in both directions — a documented no-op on one, an out-of-range
parameter on the other — so the cost is a tune that does not happen rather than one that happens
unasked.

### What a TS-590S says that its reference does not

Three things, all found by putting one on the air, and two of them make a set fail:

- **An on/off set must send P1 as 0.** `AC010` and `AC000` are accepted; `AC110`, `AC100` and
  `AC101` all answer `?;`. That is the reference's "the setting cannot be performed for RX
  IN/THRU" read strictly: a set may not ask for a receive-tuner state, so it must ask for none.
  Switching the tuner in with `AC010` answers `AC110` — the radio brings the receive tuner in on
  its own. `AC111` keeps its `1` regardless, because that is the form the reference names and the
  one verified transmitting; the rig evidently special-cases it.
- **A set that changes nothing is rejected.** Asking for off while already off answers `?;`. That
  would make an ordinary idempotent request fail the second time — the same PATCH twice, or the
  same button pressed twice — so `SetTuner` reads first and skips a write it does not need. It
  also bit once in the way this protocol's rejections always do: the `?;` from the refused set
  arrived while the read-back was outstanding and failed *that*, which is the late-rejection
  hazard §5.5 already warns about.
- **A finished cycle reports success in the tuner state itself.** Neither the command nor the
  reference has a result code, but the radio answers the question anyway: a cycle that finds a
  match ends with the tuner IN (`AC110`, published as `on`) and one that fails ends with it THRU
  (`AC000`, published as `off`). Confirmed across four frequencies — 3560, 3530 and 3502 kHz all
  matched, 3770 kHz did not, and on that one the rig found the SWR too high and gave up in well
  under a second where a real hunt took two or three. So a client watching `tuner` alone can tell
  a successful tune from a failed one, without a result code existing anywhere.

**An IC-7610 does the same thing**, tested on the same four frequencies: `1C 01` reads `01` after
a cycle that matched and `00` after one that did not, and 3770 kHz failed there too. So "ends on,
matched; ends off, failed" now holds on both families that have been tried. Nothing in either
reference says so. Yaesu is untested.

### What the two radios disagree about, and why PTT wins

They report a cycle completely differently, and following the rig rather than the calendar of
what *ought* to be true is what gets both right:

- A **TS-590S** reports PTT true through the cycle, and its transmit meters carry real readings —
  the SWR visibly falling as the tuner closes on a match, 13 → 5 → 1 of 30 dots.
- An **IC-7610** reports PTT **false** for the whole cycle and answers **zero** to all three
  meters. Its bursts are also shorter than a poll interval, especially where it already knows the
  band: "very quick since it remembers the correct parameters".

So `State.Apply` clears the transmit meters on PTT alone and does *not* treat "tuning" as
transmitting. That was tried, on the grounds that a tuning cycle plainly does key the
transmitter, and reverted the same session: on the IC-7610 it published a zero SWR as a confident
**1.0:1 — the best possible match — at the exact moment the tuner was failing to find one**. The
same reasoning as everywhere else in this backend: a number that looks real and is not is worse
than no number, and the rig's own PTT is the only signal that distinguishes them.

---

## 11.7 The receive front end

Six controls on the way in: the preamplifier, the attenuator, the RF gain, the AGC, and — on
Icom — IP+ and the DIGI-SEL preselector with its shift. They are grouped because an operator
works them together, and they are split across two backend interfaces because four of them are
universal and two belong to one manufacturer:

- `backend.FrontEndController` — `SetPreamp`, `SetAttenuator`, `SetRFGain`, `SetAGC`.
- `backend.PreselectController` — `SetIPPlus`, `SetDigiSel`, `SetDigiSelShift`.

Neither has a read half. All six are ordinary polled values that arrive as patches from the slow
tier, so the session never asks a backend for one.

### Why the attenuator is in dB and the preamp is a count

Two different answers to the same shape of question, and each follows what the radios document.

**The attenuator carries a dB figure**, because the references print one and because the ladders
are not the same set twice: an IC-7610 steps 3 dB at a time to 45, an IC-7850 to 21, an IC-7600
and IC-7700 have 6/12/18, a TS-890S and TS-990S have 6/12/18, and everything smaller has a single
fixed pad. A step index would be a number whose meaning changed with the model — exactly the
mistake `Caps.AttenuatorDB` exists to avoid.

Where a radio's CAT reference documents only ON and OFF — the TS-480, TS-590, FT-891, FT-991A and
FTX-1 — the dB in the table is that radio's published receiver specification rather than something
the command table states. It is a **label on one switch**: the byte on the wire is the same either
way, so a wrong figure mislabels a control rather than mis-setting one. That is recorded in the
model comments so nobody later mistakes it for transcription.

**The preamplifier carries a count of amplifiers**, not of command values, and the IC-9700 is why.
Its `16 02` runs `00` to `03`, but those are the internal preamp and an external one in
combination — `02` is "internal off, external on". Reading that as a ladder would tell a client
that `03` is more gain than `02`. So the IC-9700 reports one preamplifier, and `02` and `03` are
left to the front panel.

### One opcode, five spellings

Command `16 12` is the worst case this backend has met. Every model in the table means something
different by the same byte:

| Radio | `16 12` |
|---|---|
| IC-7610, IC-7760, IC-7300, IC-9700, IC-905, IC-9100 | `01` FAST, `02` MID, `03` SLOW |
| IC-7600 | `00` FAST, `01` MID, `02` SLOW |
| IC-7700 | `00` **OFF**, `01` FAST, `02` MID, `03` SLOW |
| IC-703 | `1` fast, `2` slow |
| IC-910H | `0` slow, `1` fast |

There is no style enum that covers those, so `Model.AGC` is a `map[radio.AGC]byte` and each model
writes its own. One byte out sets a different speed and looks exactly like a success — the failure
mode this document keeps returning to.

Kenwood is nearly as bad in a different way: the AGC **moved commands**. A TS-480 keeps the speed
on `GT`; every radio since keeps a *time constant* there and puts the speed on `GC`. And the
family refuses the command in FM outright — "an error sounds", at the radio — so remoses does not
poll it there. A TS-480 goes further and answers three spaces rather than refusing, which decodes
as "no reading" rather than as a frame to complain about.

Yaesu's `GT` is uniform across models but **does not round-trip**: it accepts `0`–`4` where `4` is
AUTO, and answers `0`–`6` where `4`, `5` and `6` are auto having settled on fast, mid or slow. So
`radio.AGC` carries three read-only values (`auto-fast`, `auto-mid`, `auto-slow`), `AGC.Settable`
excludes them, and a client that echoes one back gets a 422 that names `auto` instead. Flattening
them into `auto` would discard the only report of what the AGC is actually doing.

### An answer that cannot be decoded must still complete the read

Every front-end decoder sets its `Key` **before** it looks at the value. A reading outside what a
model documents then resolves the pending request and publishes nothing.

This is not tidiness. An unmatched reply leaves the read to time out; the failures accumulate; and
the session eventually tears down a link to a radio that is answering perfectly well. The IC-910H
is the concrete case — its own table lists `10`, `20` and `30` for what its specification calls a
single pad, so a reading of `10` is entirely possible against a profile offering `20`. Under the
old rule that would have been a dropped connection; under this one it is a missing value.

### Switching the AGC off is a one-way trip

Found on a TS-590S, and the sharpest thing in this section. **With the AGC off, `GC1` and `GC2`
are both refused and the radio stays off.** A client that switched the AGC off could never switch
it back, and would be told only "command rejected" — the state would sit on `off` for ever with
every attempt to leave it failing.

The reference documents the parameter that gets back out — "3: AGC Off → On (AGC returns to its
Slow/Fast status before turning Off)", "used only for turning AGC On" — and it reads as one option
among four. What it does not say anywhere is that the other two are REFUSED while the AGC is off,
which is the part that makes 3 not an option but the only door. That half came from the radio.

`Model.AGCOnCode` carries it — 3 on the TS-590, 4 on the TS-890S and TS-990S, where the extra
speed pushes it up — and `SetAGC` sends it first when a speed is asked for while the last reading
was `off`. Its own answer is not read: the speed that follows is what the caller asked for, and
reading the intermediate state would report a value nobody requested.

The TS-890S and TS-990S values are transcribed, not tested. Their references describe the
parameter the same way and neither radio has been on the bench, so whether they share the TS-590's
refusal is unknown — if they do not, sending the extra command from `off` is harmless.

It is sent only from `off`. The reference says a 3 while the AGC is on does nothing, but a command
that does nothing is still a command, and this one would otherwise go out on every set.

The TS-480 gets none of this: its AGC is `GT` with three values and no fourth documented. That may
mean it takes a speed directly from off, or it may have the same trap with no way out — nothing
here can tell, and inventing a parameter to send blind is not how it gets settled.

### Two interlocks in no manual

Verified on an IC-9700: **the AGC is pinned to FAST in FM.** All three speeds go in under USB.
In FM `16 12 01` is *accepted* and takes effect, while `02` and `03` both draw an NG — and a read
answers throughout, so the state looks perfectly healthy and only the refusal says anything is
different. Leaving FM restores the speed the previous mode had, so the radio keeps this per mode.

A first pass at this radio recorded it as "the AGC cannot be set in FM", which is what it looks
like from any sequence that does not happen to ask for FAST. The distinction matters: a guard on
the mode would refuse the one speed that works.

Which is why it is not a guard. Kenwood documents this restriction for its own AGC commands; none
of the Icom references here mentions it. So on Icom it is reported from what the radio did rather
than checked before the write: the command still goes out, a model that turns out to allow more is
not fenced off on the strength of one radio, and the reason is appended to the rig's own
rejection, which otherwise says only "command rejected".

The read is left in the poll for the same reason it is safe to: FM answers it. That is the
difference from Kenwood, where the equivalent read draws an audible error tone at the radio and
is skipped in FM entirely.

### An interlock in no manual

Verified on an IC-7610: **with DIGI-SEL engaged, the radio refuses to switch a preamplifier in.**
`16 02 01` and `16 02 02` draw a bare NG while `16 02 00` is accepted and the read works
throughout, so nothing in the exchange says why. With DIGI-SEL off, both preamplifiers select
immediately.

The radio enforces it from the other side too: switching DIGI-SEL **in** while a preamplifier is
selected switches that preamplifier **out** by itself, which the next poll reports as `preamp: 0`.
So the two really are mutually exclusive on this radio, and a client that shows both controls
should expect one to move when the other is touched.

`Rig.digiSel` holds the last `16 4E` reading for one purpose: to add that explanation to the
refusal. remoses does **not** switch the preselector off to make the request succeed — that would
be changing a second control the operator did not ask about, on a receiver they are listening to.
The hint is appended to the radio's own error rather than replacing it.

---

## 11.8 Noise processing, notches and the antenna

The noise blanker, the noise reducer, the two notch filters and — where a radio
has a live one — the antenna selector. `backend.NoiseController` carries the first
four; `backend.AntennaSelector` is separate because no Icom implements it.

### Counts, not switches

`noise_blanker` and `noise_reduction` are 0-or-select rather than booleans,
because a Kenwood has **two of each and they are not two strengths of one**. NR1
is a noise reducer; NR2 is SPAC, whose level parameter is a *following speed* the
reference gives as 2 ms to 20 ms. Publishing them as one switch would flatten two
algorithms into a checkbox.

That radio can also run both blankers at once and answers `NB3` for it. That is a
combination, not a third blanker, so it is remembered but not published — the same
rule the IC-9700's preamp gets, and for the same reason: calling it "3" would
tell a client it is more blanking than 2.

`nr_level` therefore has **no single scale**. The percentage is written against
NR1's 01-10 or NR2's 00-09 depending on which is running, from the last reading.

### Levels are refused while their circuit is off

"When NB is set to OFF, an error occurs." "When the Noise Reduction setting is
OFF, an error occurs." So `NL` and `RL` are asked for only once the radio has
reported the circuit on — on the first slow poll, before anything is known,
neither is asked and the next tick picks them up.

The ordering in `applyNoise` follows from the same fact: each switch is written
before its level, so a single request can turn a blanker on and set its threshold
without the second half failing.

### Two notches, one filter

`notch` and `auto_notch` are separate fields because on Icom and Yaesu they are
separate commands. But **most of these radios can only run one**, and they enforce
it in three different ways:

- **Kenwood** is honest about it: `NT` is one selector — off, auto, manual.
- **Icom** is not. `16 41` and `16 48` are independent commands and no reference
  mentions them interacting, but an IC-7610 switches one off whenever the other
  goes on. Verified in both directions on the radio.
- **Yaesu** has `BP` and `BC` with no documented interaction and no radio on the
  bench to say otherwise, so remoses does not claim exclusivity there.

`Caps.NotchExclusive` carries it, and the session refuses a request setting both.
Before that check, such a request returned 200 having applied whichever the
ordering wrote last — one notch, and no hint that the other had evaporated.

Turning one off where the pair is a single selector needs care in the other
direction too: with a Kenwood in auto, "switch the manual notch off" is *already
true*, so sending `NT0` would switch the automatic notch off as well — cancelling
a control the caller never mentioned. `SetNotch(false)` writes only when the
manual notch is the one running.

### A set that is ignored rather than refused

A TS-590S in CW **ignores** a request for the automatic notch: `NT10` draws no
error and a read still answers `NT20`. The reason is sound — the automatic notch
hunts for tones, and in CW the tone is the wanted signal — but the silence is the
problem. Nothing in the exchange says the request did not happen.

`setNotchSel` therefore verifies its own read-back and returns `ErrUnsupported`
naming what the radio stayed on, plus the reason where the mode makes it known.
This is the third member of a family this document keeps returning to: a command
that succeeds and means something else, a value written but never read back, and
now a value written and quietly discarded.

### One refusal must not starve a tier

Found on an IC-9700 in FM, and it was not a bug in any of the above.

That radio rejects `16 57` — the notch width — in FM, which is correct: FM has no
use for a DSP notch. `readAll` stopped the whole tier at the first failure, so
everything queued **behind** it was skipped on every slow tick. The automatic
notch sat two places back and was therefore never read at all, on a radio that
reports it perfectly well in every other mode. The field simply never appeared.

`readAll` now distinguishes two events that were being treated alike:

- **A rejection** (`ErrRejected`) — the radio is alive and said no. Carry on; the
  first refusal is kept and returned once the run finishes, so the session still
  sees that something was refused.
- **A transport failure** — the link is gone. Stop, because the remaining reads
  would each wait out their own timeout before anybody noticed.

This is a general fix rather than a noise one. Any optional read placed before
another was exposed to it; the notch width was simply the first command
unsupported-in-a-mode to land in the middle of a queue rather than at its end.

### The antenna selector, and the Icom memory that is not one

Kenwood's `AN` and Yaesu's `AN` are live selectors, and remoses offers them where
the parameter layout has been transcribed — the TS-590's three parameters and the
FTdx101's socket count. The TS-890S's `AN` takes four parameters that have not
been read, and the TS-990S has no `AN` row at all.

**Icom's is command `12`, and this section previously said it did not exist.**
That was wrong, and wrong in an instructive way: it argued from what the antenna
*is* on an IC-7610 rather than from the command table, and the radio has both
things. The per-band memory is real — `1A 05 02 76` through `02 87`, one entry
per band range, each carrying the socket and the receive-antenna flag — and
remoses still does not write it, because that is stored configuration. But
alongside it the IC-7610, IC-7600, IC-7700, IC-7760, IC-7850 and IC-9100 all
answer a live `12`, and the IC-7300MK2 answers it for a receive antenna with no
socket to choose.

**The frame is one command carrying two fields**: `12 <socket> [<flag>]`, socket
counting from zero in the *sub-command* and the data byte holding that socket's
receive-antenna flag. Three column layouts print the same bytes — the IC-7600
has no sub-command column at all and one two-byte data field, and the IC-9100
prints the socket with an empty Data column, one byte shorter, because it has no
receive antenna. So `state.rx_antenna` is a property of the selected socket
there rather than an independent switch, and both setters read before they write
because neither field can be set without carrying the other across.

**The read is a bare `12` on every model, and that is a safety property rather
than a style.** Sending `12 00` to read ANT1's flag would, on an IC-9100, be a
complete *set* frame selecting ANT1 — a poll one byte away from moving somebody's
antenna. A test pins it.

Two things remain reported rather than resolved: the flag's meaning is
conditioned by the RX-ANT Connectors item (`1A 05 02 75`), and with `[ANT] SW`
in Auto the socket can move with no command sent at all. remoses reports what
the radio says rather than second-guessing either.

---

## 12. Safety interlocks

This API keys a transmitter over a network, so interlocks are part of the design rather than
a later addition:

- **Dead-man TX timeout** — `limits.tx_timeout` forces RX regardless of client state,
  including when the client vanishes mid-transmission. Shares the lock-expiry path (§7).
- **Lock expiry drops PTT and flushes CW** — see §7.
- **Power clamp** — `limits.max_power_pct` is enforced server-side; a request for 100 gets 80
  *and* a response saying 80.
- **Band limits** — frequency sets outside `limits.bands` are rejected with `422`.
- **Audit log** — one structured `slog` line per state-changing request: user, radio, action,
  old → new, lock token id, result.

---

## 13. Layout and dependencies

```
remoses/
├── cmd/remoses/              main, flags, `passwd` subcommand
├── cmd/remoses-cli/          read-only terminal monitor
├── api/openapi.yaml          source of truth
├── api/codegen.yaml          what `make generate` makes of it
├── docs/DESIGN.md            this file
└── internal/
    ├── config/               load, validate, defaults
    ├── auth/                 basic auth, bcrypt, TTL cache
    ├── lock/                 per-radio lock manager
    ├── api/                  hand-written handlers, problem+json
    ├── wire/                 GENERATED from api/openapi.yaml; do not edit
    ├── client/               transport, auth and errors around internal/wire
    ├── ws/                   hub, per-client queues, coalescing
    ├── rig/                  Manager, Session, State, command queue, poller
    │   ├── backend/          Rig interface + registry
    │   ├── backend/civ/      Icom CI-V
    │   ├── backend/kenwood/  Kenwood / Elecraft / modern Yaesu
    │   └── backend/rigctld/  Hamlib escape hatch
    ├── cw/                   queue, pacing, element generator, Morse table
    └── transport/serial/     go.bug.st/serial wrapper, reconnect, enumeration
```

Dependencies, deliberately few:

| Module | Use |
|---|---|
| `go.bug.st/serial` | Serial I/O, enumeration, modem control lines |
| `goccy/go-yaml` | Config |
| `github.com/coder/websocket` | WebSocket |
| `golang.org/x/crypto/bcrypt` | Password hashing |
| `golang.org/x/term` | Terminal size and password prompt, for remoses-cli |
| `oapi-codegen/runtime` | Small support library the generated client links against |
| `oapi-codegen/nullable` | Three-state optional (absent / null / value) for the fields a delta clears |
| `oapi-codegen/oapi-codegen` | **Tool dependency**: generates `internal/wire`, never linked |

Everything else is stdlib (`net/http`, `log/slog`, `crypto/subtle`). Result is a single static
binary per platform, cross-compiling to linux/amd64, linux/arm64, darwin, and windows.

The generator is a `tool` directive in `go.mod` rather than something to install: `go tool
oapi-codegen` runs the version `go.sum` pins, so two machines generate the same file, and its
own dependency tree — kin-openapi and the rest — is build-time only and reaches neither
binary. `internal/wire` is checked in for the same reason the archives carry the docs: a
build must not need the network or the generator.

---

## 14. Testing strategy

- **Backend codecs** — table-driven tests over recorded frames, including partial reads,
  split frames, CI-V echo, and unsolicited transceive updates.
- **Rig simulators** — an in-process fake `transport.Transport` speaking CI-V and Kenwood CAT,
  enough to run the full session/poller/CW-pacing stack in unit tests with no hardware.
- **Keyer timing** — assert element edge error at 20/25/30 wpm; run in CI but tolerate wider
  bounds on loaded shared runners.
- **Lock semantics** — expiry mid-transmission drops PTT; sliding renewal; steal behaviour.
- **WebSocket backpressure** — a deliberately stalled client must not slow a rig session, and
  must receive `resync` rather than a wedged stream.
- **Contract conformance** — real REST responses and every kind of WebSocket frame are
  decoded into the types `api/openapi.yaml` generates, with unknown members refused and the
  required ones checked for. The point is that these run against the daemon's own output
  rather than against fixtures somebody wrote to match the spec: a fixture agrees with
  whatever it was copied from, including a mistake.

---

## 15. Open items

1. **S-meter calibration tables.** Both rigs report an uncalibrated meter reading (0–30 dots
   on Kenwood, 0–255 on Icom). Converting to S-units needs a per-model lookup table built
   from measurement or community data. Until one exists, `s_meter.s` stays `null` and clients
   get `raw`/`scale` only — which is honest, and enough to draw a bar.
2. **Power Fine.** Kenwood `PC` steps in 5 W unless the rig's Power Fine menu setting is on,
   and the rig rounds *down* silently. Decide whether remoses reads that setting at connect
   and reports the effective step, or simply always reads back the achieved value (the
   read-back-after-write rule in §8 already makes this safe, just occasionally surprising).
3. **VFO model.** v1 addresses the *current* VFO with an optional `?vfo=A|B`. The IC-7610's
   dual receivers deserve a richer model (`main`/`sub`) once the basics work.
4. **Kenwood `TX;` is silent** unless AI is on. Since we enable `AI2` at connect this is
   moot in practice, but the backend should not block waiting for an ack that only arrives
   because of an unrelated setting.
5. Split operation, RIT/XIT, memory channels, antenna tuner — deferred past v1.
6. Audio transport — separate project; the API is shaped so it can be added alongside rather
   than retrofitted.
