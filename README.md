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

**Experimental. Only a handful of the radios below have ever been connected —
but every one of those has been put through everything it can do.**

Everything else was transcribed from the manufacturers' own reference
documentation and is exercised against simulated rigs in the test suite. That
proves the code matches what the manual says. It does not prove the manual is
right, or that a particular radio behaves the way its documentation claims.
Between them they turned up **bugs no amount of reading the reference would
have** — including CW that was accepted, queued, drained on schedule and never
transmitted, which only an operator listening could have caught.

**[docs/hardware-status.md](docs/hardware-status.md)** — what was exercised on
each radio, what the bugs were, and what this still does not tell you. Read it
before pointing remoses at a transmitter.

Do not leave it running an unattended station yet.

### If you have one of the untested radios

`remoses test-run` exercises everything your radio says it can do, puts the
radio back as it found it, and writes a report to send back. A minute of your
time is worth more to this project than any amount of re-reading the
manufacturer's reference.

```sh
remoses test-run                                   # receive only; never keys the radio
remoses test-run -tx-freq 28030000                 # ...and test transmitting there
remoses test-run -tx-freq 28030000 -tx-power-pct 25 -cw-text "TEST DE N0CALL"
```

| Flag | Default | What it does |
|---|---|---|
| `-config PATH` | `remoses.yaml` | Which configuration file to read. |
| `-radio ID` | the only one | Which configured radio to test. Required if the file has more than one. |
| `-tx-freq HZ` | *none* | **The transmit switch.** Without it the run never keys the radio. With it, the run may key the transmitter, check the transmit meters, send a short CW message and run one antenna-tuner cycle. Pick a frequency you are licensed and equipped to use. |
| `-tx-power-pct N` | `10` | Power for the transmit tests, percent. |
| `-cw-text TEXT` | `TEST TEST DE REMOSES` | What the CW test sends. **Put your own callsign here** — `N0CALL` in these examples is a placeholder, not something to transmit. |
| `-test-power-switch` | off | Also switch the radio off over CAT and wake it again. Off by default: whether a wake works is a wiring question, and a radio that will not wake needs somebody standing at it. |
| `-notes TEXT` | *none* | Free text about your station, copied into the report header. |
| `-out PATH` | `remoses-selftest-<radio>-<time>.jsonl` | Where to write the report. |
| `-log-level LEVEL` | `info` | Terminal chatter. The CAT trace goes in the report regardless. |

The report is JSON Lines — a header with your radio's capabilities, one line per
step with the request, the read-back and **the CAT frames that went over the
wire**, and a summary. A few tens of kilobytes, with nothing secret in it.

**Send it to <remoses-logs@he.fi>**, and say whether the CW was audible if you
ran the transmit tests — that is the one thing the file cannot record.

**[docs/test-run.md](docs/test-run.md)** has the detail, including how to read a
report and what it deliberately does not cover.

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
| TS-590SG | `ts590sg` | **yes** |
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

Exercised against a Hamlib 4.7 daemon with an IC-7610 behind it, CW on the air
included. It implements a subset — frequency, mode, filter width, power, PTT,
the transmit meters and CW — and refuses the rest whatever Hamlib reports, so
prefer a native backend where your radio has one.

## Connections

A radio can be reached three ways, all sharing the same supervised
dial/backoff/reconnect path:

- a **local serial port**, by device path or — preferably — by USB VID/PID and
  serial number, which survives a replug;
- a **serial port over the network**, `port.tcp: host:port`, as published by
  ser2net or a hardware terminal server;
- **`rigctld`** over TCP.

## Installing

**Linux, Raspberry Pi and macOS:**

```sh
curl -fsSL https://raw.githubusercontent.com/hessu/remoses/main/install.sh | sh
```

**Windows**, in PowerShell — `irm` and `iex` are built in, nothing to install
first:

```powershell
irm https://raw.githubusercontent.com/hessu/remoses/main/install.ps1 | iex
```

Both work out which build your machine needs, **verify the download against the
release's `SHA256SUMS`**, and install two binaries. The Windows one installs
per-user and needs no administrator. Neither leaves anything running.

To read the script before running it, which is the right instinct:

```sh
curl -fsSLO https://raw.githubusercontent.com/hessu/remoses/main/install.sh
less install.sh && sh install.sh
```

Add `--systemd` on Linux to also create a service that starts at boot — a
`remoses` user in the `dialout` group, a configuration in `/etc/remoses`, and a
unit that is installed but **not started**, because the example configuration
still has example passwords in it. See [docs/install.md](docs/install.md) for
the options, and for doing it by hand instead.

## Downloads

Every tagged release carries prebuilt archives, published automatically from
the tag. Each contains both binaries, the example configuration, the user guide
and the licence; `SHA256SUMS` beside them covers the lot.

| Archive | For |
|---|---|
| `linux-amd64` | An ordinary Linux PC or server. |
| `linux-arm64` | Raspberry Pi 3, 4, 5 or Zero 2 W on **64-bit** Raspberry Pi OS. |
| `linux-armv7` | Raspberry Pi 2, or a 3/4/5 running a **32-bit** OS. |
| `linux-armv6` | Raspberry Pi 1, Zero and Zero W. |
| `windows-amd64` | A Windows shack PC. |
| `windows-arm64` | Windows on ARM. |
| `darwin-arm64`, `darwin-amd64` | Apple silicon and Intel Macs. |

**The two 32-bit Pi builds are not interchangeable.** An armv7 binary on a Pi
Zero dies with an illegal instruction, which is a miserable thing to diagnose at
a remote site. If you are unsure, `uname -m` reports `aarch64` for 64-bit,
`armv7l` for a Pi 2 and up on a 32-bit OS, and `armv6l` for a Pi 1 or Zero.

Nothing needs installing beside the binary: one static executable per command,
no cgo, no runtime.

## Building

```sh
make          # build remoses and remoses-cli into build/
make test     # go test -race ./...
make check    # fmt-check + vet + test, the pre-commit target
make cross    # cross-compiled binaries into dist/
make release  # ...packaged into per-platform archives with checksums
```

`make release` is exactly what the tag build runs, so a release can be
reproduced on a laptop, and the platform list lives in the Makefile alone.

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
