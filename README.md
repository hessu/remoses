# remoses

Remote control of amateur radio transceivers over an authenticated HTTP and
WebSocket API.

remoses runs on a machine physically next to the radios, talks to each one over
USB serial (CAT / CI-V) or a serial port published over the network, and exposes
frequency, mode, filter, power, PTT and CW sending to network clients. It drives
several radios at once, each identified by a stable id and a human-readable name.

Because remoses is local to the rig, all CW element timing is generated
server-side — network latency affects only how quickly text is queued, never the
quality of the Morse that goes out.

Scope for v1 is control only. Audio is a separate concern.

## Status

**Experimental. Four radios out of the thirty-seven below have ever been
connected — but all four have been put through everything they can do.**

Everything else was transcribed from the manufacturers' own reference
documentation and is exercised against simulated rigs in the test suite. That
proves the code matches what the manual says. It does not prove the manual is
right, or that a particular radio behaves the way its documentation claims.
Between them the four found **fifteen bugs no amount of reading the reference
would have** — including CW that was accepted, queued, drained on schedule and
never transmitted, which only an operator listening could have caught.

**[docs/hardware-status.md](docs/hardware-status.md)** — what was exercised on
each radio, what the bugs were, and what this still does not tell you. Read it
before pointing remoses at a transmitter.

Do not leave it running an unattended station yet.

## Supported radios

**Tested** marks radios confirmed against real hardware: every CAT feature
remoses implements *for that radio*, exercised on the air. Treat everything else
as "implemented from documentation, awaiting confirmation".

Each backend page has the same table with per-model notes, plus how to configure
that manufacturer's radios.

### [Icom](docs/icom.md) — `civ`

| Model | `civ.model` | Tested |
|---|---|---|
| IC-703 | `ic-703` | — |
| IC-706 | `ic-706` | — |
| IC-706MKII | `ic-706mkii` | — |
| IC-706MKIIG | `ic-706mkiig` | — |
| IC-718 | `ic-718` | — |
| IC-7300 | `ic-7300` | — |
| IC-7300MK2 | `ic-7300mk2` | — |
| IC-7600 | `ic-7600` | — |
| IC-7610 | `ic-7610` | **yes** |
| IC-7700 | `ic-7700` | — |
| IC-7760 | `ic-7760` | — |
| IC-7850 / IC-7851 | `ic-7850` | — |
| IC-905 | `ic-905` | — |
| IC-910H | `ic-910h` | — |
| IC-9100 | `ic-9100` | — |
| IC-9700 | `ic-9700` | **yes** |
| other Icom | `generic` | — |

### [Kenwood](docs/kenwood.md) — `kenwood`

| Model | `kenwood.model` | Tested |
|---|---|---|
| TS-480 | `ts480` | — |
| TS-590S | `ts590s` | **yes** |
| TS-590SG | `ts590sg` | — |
| TS-890S | `ts890s` | — |
| TS-990S | `ts990s` | — |
| other Kenwood | `generic` | — |

### [Yaesu](docs/yaesu.md) — `yaesu`

One backend name, **two unrelated CAT protocols**. remoses picks between them
from the model name, so naming your model is mandatory here.

| Model | `yaesu.model` | Protocol | Tested |
|---|---|---|---|
| FT-950 | `ft-950` | ASCII | — |
| FTdx1200 | `ftdx1200` | ASCII | — |
| FTdx3000 | `ftdx3000` | ASCII | — |
| FTdx5000 | `ftdx5000` | ASCII | — |
| FTdx9000 | `ftdx9000` | ASCII | — |
| FT-891 | `ft-891` | ASCII | — |
| FT-991A | `ft-991a` | ASCII | — |
| FT-710 | `ft-710` | ASCII | — |
| FTdx10 | `ftdx10` | ASCII | — |
| FTdx101D | `ftdx101d` | ASCII | — |
| FTdx101MP | `ftdx101mp` | ASCII | — |
| FTX-1 | `ftx-1` | ASCII | — |
| other Yaesu | `generic` | ASCII | — |
| FT-857 | `ft-857` | **binary** | — |
| FT-857D | `ft-857d` | **binary** | **yes** |
| FT-897 | `ft-897` | **binary** | — |
| FT-897D | `ft-897d` | **binary** | — |

### [Anything else](docs/rigctld.md) — `rigctld`

Any rig [Hamlib](https://hamlib.github.io/) supports, by talking to a `rigctld`
process over TCP. remoses speaks the protocol in pure Go and can launch and
supervise the daemon itself, so there is no cgo and no LGPL linking.

## Connections

A radio can be reached three ways, all sharing the same supervised
dial/backoff/reconnect path:

- a **local serial port**, by device path or — preferably — by USB VID/PID and
  serial number, which survives a replug;
- a **serial port over the network**, `port.tcp: host:port`, as published by
  ser2net or a hardware terminal server;
- **`rigctld`** over TCP.

## Building

```sh
make          # build remoses and remoses-cli into build/
make test     # go test -race ./...
make check    # fmt-check + vet + test, the pre-commit target
make cross    # release binaries for linux, darwin and windows into dist/
```

Single static binary per command, no cgo. Cross-compiles to linux/amd64,
linux/arm64, linux/arm, darwin/amd64, darwin/arm64 and windows/amd64.

## Getting started

Copy [`remoses.example.yaml`](remoses.example.yaml) and edit it. Unknown keys
are a startup error rather than a silent default, and the whole file is
validated before anything opens a port:

```sh
remoses passwd                          # generate a bcrypt hash for the file
remoses -config remoses.yaml -check     # validate without opening a port
remoses -config remoses.yaml            # run
```

Then watch it:

```sh
remoses-cli ic7610
```

[docs/configuration.md](docs/configuration.md) covers the file in full, and the
backend page for your radio covers what that manufacturer needs.

## Documentation

**[docs/](docs/README.md) — the user guide.**

- [Configuration](docs/configuration.md) — the file, ports, limits, users.
- [Controlling a radio](docs/features.md) — modes, PTT, meters, CW, break-in,
  the tuner, the front end, and the safety interlocks.
- [remoses-cli](docs/remoses-cli.md) — the read-only terminal monitor.
- Your radio: [Icom](docs/icom.md) · [Kenwood](docs/kenwood.md) ·
  [Yaesu](docs/yaesu.md) · [rigctld](docs/rigctld.md).
- [Hardware status](docs/hardware-status.md) — what has actually been tried.

**Reference.**

- [docs/DESIGN.md](docs/DESIGN.md) — architecture, the concurrency model, the
  locking and safety design, the CW pacing loops, and the per-model protocol
  references with the differences between radios spelled out.
- [api/openapi.yaml](api/openapi.yaml) — the HTTP API, and the source of truth a
  conformance test holds the router to.

## Licence

BSD 3-Clause. See [LICENSE](LICENSE).
