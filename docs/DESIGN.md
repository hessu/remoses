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
  listen: "0.0.0.0:8080"
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
    - username: oh7lzb
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

### 5.1 Backends

| Backend | Covers | Notes |
|---|---|---|
| `civ` | All Icom | Binary `FE FE <to> <from> cmd [sub] [data] FD`; BCD frequencies |
| `kenwood` | Kenwood, Elecraft, modern Yaesu, Flex CAT | ASCII, `;`-terminated; per-model quirk table |
| `rigctld` | Everything Hamlib supports | Pure-Go TCP client; optionally spawns `rigctld` as a child process and supervises it |

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
  `1A 05 0112`, but `1A 05` writes the operator's *persistent* Set-mode configuration, which
  survives power-off. remoses will not permanently reconfigure somebody's radio as a side
  effect of connecting, so it only reads. Without it the Icom is poll-only while the Kenwood
  gets free push updates.
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
every Icom; what differs per model is the factory bus address, which operating modes exist,
and — on one radio — the width of the frequency field. `civ.model` names the radio so remoses
can publish honest capabilities and default the address correctly.

Verified against each model's own CI-V reference guide:

| `civ.model` | Address | Modes beyond the common set | Frequency field |
|---|---|---|---|
| `ic-7610` | `0x98` | PSK, PSK-R | 5 bytes |
| `ic-7300mk2` | `0xB6` | — | 5 bytes |
| `ic-7760` | `0xB2` | PSK, PSK-R | 5 bytes |
| `ic-9700` | `0xA2` | DV, DD | 5 bytes |
| `ic-905` | `0xAC` | DV, DD, ATV | 5 bytes, **6 on the 10 GHz band** |
| `generic` | none | — | 5 bytes |

The common set is LSB, USB, AM, CW, CW-R, FM, FSK (the rig calls it RTTY) and FSK-R.

Everything else remoses uses is identical across all five: `03`/`05` frequency, `04`/`06`
mode, `14 0A` RF power, `15 02` S-meter, `1A 03` filter width, `1C 00` PTT, `17` send CW
(30 characters), `19 00` read ID. Mode *codes* are shared too — `0x03` is CW on every Icom —
so there is one code table and each model records only which subset it accepts. `generic` is
the escape hatch for an Icom without a profile; it has no factory address, so `rig_address`
must be given rather than guessed.

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

**Poll** at two rates: `interval` for volatile values (frequency, mode, PTT, S-meter) and
`slow_interval` for stable ones (power, filter). With transceive/AI enabled the poller is
mostly a safety net against missed pushes.

On Kenwood the fast poll is a single `IF;` — one 38-character reply carrying frequency,
RIT/XIT, TX/RX state, mode and split — plus `SM0;`. The exception is Data mode, where `IF;`
does not answer, so the poller checks `DA;` and falls back to discrete `FA;` / `MD;` / `RI;`
queries. On Icom the fast poll is `03`, `04`, `1C 00`, `15 02`.

**Disconnect** — USB serial dies when a cable is pulled or the rig is switched off. Treat the
port as a supervised resource: on read error, close, mark `connected: false`, publish a WS
event, and retry with exponential backoff (capped ~30 s). Re-open **by VID/PID + serial**
where configured, not by device path — `/dev/ttyUSB0` is not stable across replug.

A dead port is not the only failure mode. A rig switched off behind a powered USB adapter
leaves the port open and readable while answering nothing, so the session also counts
consecutive poll failures and tears the connection down after five. Without that it would
poll into the void indefinitely and keep reporting a stale snapshot as live.

**But a rig that refuses a command is not a broken link.** Rigs differ in which of the
optional commands they implement, and they decline in protocol-specific ways: an Icom
acknowledges an unimplemented command with `FB` instead of returning data, a Kenwood answers
`?;`. Only transport loss and outright silence are treated as fatal — anything else means the
radio replied, which proves it is still there. Concretely:

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
surface this small a code generator would cost more than it saves. Drift is caught instead by
a conformance test that parses the spec and asserts every documented path and method has a
registered route, and that no route exists which the spec does not document.

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

{ "type":"cw",     "radio":"ic7610", "busy":true, "queued":12, "wpm":28 }
{ "type":"conn",   "radio":"ts590sg", "connected":false, "error":"port closed" }
{ "type":"resync", "radio":"ic7610" }        // client dropped events; refetch state
```

Client → server is minimal: `{"type":"ping"}` and `{"type":"subscribe","radios":[…]}`.
All control stays on REST, so the WebSocket has no authorisation surface.

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
strictly increasing per radio and clients can detect gaps independently.

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
├── api/openapi.yaml          source of truth
├── docs/DESIGN.md            this file
└── internal/
    ├── config/               load, validate, defaults
    ├── auth/                 basic auth, bcrypt, TTL cache
    ├── lock/                 per-radio lock manager
    ├── api/                  generated server, handlers, problem+json
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
| `oapi-codegen/oapi-codegen` | Build-time server generation |

Everything else is stdlib (`net/http`, `log/slog`, `crypto/subtle`). Result is a single static
binary per platform, cross-compiling to linux/amd64, linux/arm64, darwin, and windows.

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
