# Configuration

remoses is configured by one YAML file. Copy
[`remoses.example.yaml`](../remoses.example.yaml), edit it, and check it before
you run anything:

```sh
remoses -config remoses.yaml -check
```

**Unknown keys are a startup error, not a silent default**, so a typo is
reported with its line and column rather than quietly ignored. The whole file is
validated before anything opens a port.

Radio-specific settings — which `civ.model` to name, what an FT-857 needs in its
menus — are on the backend pages: [Icom](icom.md), [Kenwood](kenwood.md),
[Yaesu](yaesu.md), [rigctld](rigctld.md).

## The server

```yaml
server:
  listen: "0.0.0.0:8080"
  base_path: /api/v1
  tls:
    cert_file: /etc/remoses/cert.pem
    key_file: /etc/remoses/key.pem
```

**Anything but loopback needs TLS.** HTTP Basic replays the password on every
request, and there is a request per poll. Terminating TLS at a reverse proxy
instead is fine: drop the `tls` block and bind to `127.0.0.1`. If you genuinely
mean cleartext on a real address, `server.insecure: true` says so deliberately.

## Users and passwords

```yaml
auth:
  realm: remoses
  bcrypt_cost: 8
  cache_ttl: 60s
  users:
    - username: n0call
      password_bcrypt: "$2a$08$..."
```

Generate the hashes with `remoses passwd`, which reads the password from the
terminal rather than from an argument so it does not land in shell history or
the process table. The file holds **hashes, not passwords** — which is also why
`remoses-cli` cannot read your password out of it.

`bcrypt_cost` is deliberately low. This is a trusted station, and every polling
request pays the KDF unless it hits `cache_ttl`.

## Exclusive control

```yaml
lock:
  enabled: true
  ttl: 30s
  allow_steal: false
```

The lock is **per radio**, so one operator can work the IC-7610 while another
works the TS-590SG. It is a sliding lease: every accepted command resets it.

**Expiry is a safety event, not a cleanup.** A client that dies mid-over loses
the lock, and losing it drops PTT and flushes the CW queue — see
[Controlling a radio](features.md#safety-interlocks).

## The event stream

```yaml
ws:
  min_interval: 50ms
  ping_interval: 30s
  send_queue: 256
```

Spinning a VFO knob on a radio with push updates can emit hundreds of state
changes a second, so they are coalesced to at most one per radio per
`min_interval`.

## Reaching a radio

A radio can be reached three ways, all sharing the same supervised
dial/backoff/reconnect path:

### A local serial port

```yaml
port:
  device: /dev/ttyUSB0
  baud: 115200
  data_bits: 8
  parity: none
  stop_bits: "1"
  dtr: high
  rts: high
```

**Prefer matching on the USB descriptor to naming a device.** `/dev/ttyUSB0` is
not stable across a replug or a reboot with two adapters plugged in:

```yaml
port:
  match:
    vid: "10C4"
    pid: "EA60"
    serial: "IC7610-001"
  baud: 115200
```

`match` wins over `device` when both are given. On macOS this needs a cgo
build to read USB descriptors; the released macOS binaries do not have one, and
say so in the error when a match fails.

**`dtr` and `rts` are the resting state of the control lines**, `high` or `low`,
both defaulting to high on a CAT port. The port is always *opened* with them low
and driven a moment later, so `high` produces a low-to-high transition — which
is what a TS-590S needs before it will answer at all.

Set them `low` only if that port's DTR or RTS is wired to a key or PTT
interface, where asserting one transmits.

### A serial port over the network

```yaml
port:
  tcp: "192.168.1.50:4001"
```

As published by ser2net, a Digi or Moxa device server, or an ESP32 bridge. It is
mutually exclusive with `device` and `match`, and carries **bytes only** — plain
TCP, not RFC 2217. So the line settings belong to the terminal server and are
ignored here, and there are no modem control lines, which rules out
`cw.method: serial_key` on that port. Keying such a rig needs its own local
`cw.serial_key.device`.

### rigctld

See [the rigctld page](rigctld.md).

## Polling

```yaml
poll:
  interval: 500ms      # frequency, mode, PTT, S-meter
  slow_interval: 5s    # power, filter — these rarely move
```

Two tiers, because reading everything at the fast rate would fill the port with
traffic about settings that move once an hour. Radios with push updates report
front-panel changes between polls; radios without do not, and there the fast
tier is the only source of state.

## Limits

```yaml
limits:
  max_power_pct: 80        # or max_power_w: 80 — not both
  tx_timeout: 120s
  bands:
    - 1.8-2.0MHz
    - 3.5-3.8MHz
    - 14.0-14.35MHz
```

**`tx_timeout` is a dead-man timer** that forces receive regardless of what the
client is doing. Set one.

**`bands` gates tuning, not transmitting.** There is no transmit-only band
limit, so it cannot express "receive anywhere, transmit only here" — which is
what you want if a band has no antenna on it.

**Which power field to use depends on the radio.** Icom reports power on a
relative 0–255 scale with no watt meaning, so those take `max_power_pct`;
Kenwood and most Yaesus are in watts. The two Yaesus whose `PC` is an
uncalibrated index need `max_power_pct` as well. A radio with no power command
at all — the IC-706 family, the FT-857 generation — should have neither, since a
limit there is one remoses could not enforce.

## CW

```yaml
cw:
  enabled: true
  method: cat            # or serial_key
  default_wpm: 25
  break_in: semi
```

Which method a radio can use is a fact about the radio, and remoses refuses at
startup rather than at the first message if the configuration asks for one it
does not have. See [Controlling a radio](features.md#cw) for the full picture,
including the `serial_key` block.

## Tracing the wire

```yaml
debug_wire: true
```

Logs the exact bytes to and from one radio: hex always, plus a readable
rendering for the ASCII dialects. **This is the thing to turn on the first time
a rig misbehaves**, because the ordinary logs show decoded state, which is the
one layer where a wrong assumption about a radio is invisible.

Off by default and per radio because it is noisy — polling alone puts a few
frames a second on the wire per rig. The lines come out at debug level, so a
one-off needs no edit to the file:

```sh
remoses -debug-wire=ic7610 -log-level=debug
remoses -debug-wire=all -log-level=debug
```

The flag only ever turns tracing on, so it cannot countermand the file.
