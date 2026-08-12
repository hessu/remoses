# Anything else — the `rigctld` backend

The escape hatch: any rig [Hamlib](https://hamlib.github.io/) supports, reached
by talking to a `rigctld` process over TCP.

remoses speaks that protocol in pure Go, so there is **no cgo and no LGPL
linking** — the single static binary stays single and static, and Hamlib stays a
separate process you can upgrade, restart or debug on its own.

**Verified against a Hamlib 4.7 daemon** driving an IC-7610 over the LAN:
frequency, all ten modes it offers, the filter width, transmit power, PTT, the
transmit meters, and CW **heard on the air** through Hamlib's own keyer. See
[hardware status](hardware-status.md). Only that one rig, and only through one
Hamlib backend — what works for you depends on yours.

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
See [transmit meters](features.md#ptt-and-the-transmit-meters). Confirmed on the
air: a continuous 1.03, 1.04, 1.07, 1.29, 1.36 through a transmission, where the
same radio on its native backend answers from a four-point table.

**What this backend does not implement is a bigger list than what Hamlib
lacks.** Frequency, mode, filter width, power, PTT, the three transmit meters
and CW are the surface; the receive front end, the noise blankers and notches,
the antenna tuner, split, the second VFO and the power switch are all refused
whatever Hamlib says about them. On an IC-7610 that is most of the radio — the
native `civ` backend reaches all of it. `caps` tells a client the truth either
way, and the refusals name what is missing.

**A power limit in watts may not be enforceable.** Hamlib reports power as a
fraction with no watt meaning on many rigs, and `limits.max_power_w` needs a
full-scale wattage to clamp against — so on such a radio it is silently no limit
at all. remoses warns at connect and names `limits.max_power_pct`, which is what
to use. It cannot warn at startup, for the same reason it cannot check anything
else here: the capabilities do not exist until the daemon answers.

## Hamlib caches, so a write reads back stale

remoses reads the radio after every write so the response reflects what actually
happened rather than what was asked for. Through rigctld that guarantee has a
hole in it: **Hamlib answers reads from its own cache**, 1000 ms by default on
the daemon tested (`\get_cache` reports it), and remoses reads back faster than
that.

What it looks like: set the mode to AM and the response carries the *previous*
mode's filter width, then the next poll — a second later — agrees with the radio
exactly. The state cache is right within a poll; only the immediate reply to the
write is behind.

Nothing here works around it. remoses could tell the daemon to stop caching, but
`rigctld` serves every client from one rig instance, so that would be reaching
into a process this station may be sharing with something else. Watch the next
poll rather than the write's own answer, and be aware that a client which writes
and immediately believes the response can be a second out of date.

## When to prefer a native backend

If your radio is in the [Icom](icom.md), [Kenwood](kenwood.md) or
[Yaesu](yaesu.md) tables, use that backend instead. The native backends know
per-model facts that Hamlib's generic handling cannot express and that this
project has spent a lot of care on — which mode codes a particular radio
actually has, which sub-command is PTT on an IC-718, that a TS-590's break-in
lives on the VOX command, that an FT-857 must never be asked for a filter width.

The rigctld backend is for the rigs that have no profile here.
