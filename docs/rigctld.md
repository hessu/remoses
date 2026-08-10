# Anything else — the `rigctld` backend

The escape hatch: any rig [Hamlib](https://hamlib.github.io/) supports, reached
by talking to a `rigctld` process over TCP.

remoses speaks that protocol in pure Go, so there is **no cgo and no LGPL
linking** — the single static binary stays single and static, and Hamlib stays a
separate process you can upgrade, restart or debug on its own.

Never tried against a real radio. See [hardware status](hardware-status.md).

## Configuring one

The only required field is the address:

```yaml
radios:
  - id: ft857
    name: "Yaesu FT-857 (hamlib)"
    backend: rigctld
    rigctld:
      address: "127.0.0.1:4532"
    cw:
      enabled: true
      method: serial_key
      default_wpm: 22
      serial_key:
        device: /dev/ttyUSB2
        key_line: dtr
        ptt_line: rts
    limits:
      max_power_pct: 100
      tx_timeout: 120s
```

That form assumes you are running `rigctld` yourself.

## Letting remoses run rigctld

```yaml
    rigctld:
      address: "127.0.0.1:4532"
      spawn: true
      model: 1035
      device: /dev/ttyUSB1
```

With `spawn: true` remoses launches the daemon at startup and supervises it. The
`model` is Hamlib's own numeric rig id — `rigctl --list` prints them — and
`device` is the serial port Hamlib should open.

The session dials it like any other TCP target, and the supervisor's backoff
means a daemon that takes a moment to bind is simply picked up on the next
attempt rather than failing the start.

## What works depends on Hamlib, not on remoses

**Capabilities are read from the running rig at connect.** remoses asks Hamlib
what the radio can do and publishes that as `caps`, so the answer to "does my
radio support X here" is whatever the Hamlib backend for it implements.

One consequence worth knowing: **SWR arrives calibrated.** Hamlib reports a true
ratio rather than a meter deflection, so `state.swr_ratio` on a rigctld radio is
a real number across the whole range — where a native Icom backend stops at the
top point its manufacturer calibrated, and Kenwood and Yaesu get only a bar.
See [transmit meters](features.md#ptt-and-the-transmit-meters).

## When to prefer a native backend

If your radio is in the [Icom](icom.md), [Kenwood](kenwood.md) or
[Yaesu](yaesu.md) tables, use that backend instead. The native backends know
per-model facts that Hamlib's generic handling cannot express and that this
project has spent a lot of care on — which mode codes a particular radio
actually has, which sub-command is PTT on an IC-718, that a TS-590's break-in
lives on the VOX command, that an FT-857 must never be asked for a filter width.

The rigctld backend is for the rigs that have no profile here.
