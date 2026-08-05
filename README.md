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

**Experimental. No part of this has ever been connected to a real radio.**

That is the single most important thing to know about the project right now.
Every protocol detail below was transcribed from the manufacturers' own CI-V and
CAT reference documentation and is exercised against simulated rigs in the test
suite — but a test suite agreeing with a manual proves only that the code matches
what the manual says. It does not prove the manual is right, that the transcription
is right, or that a particular radio behaves the way its documentation claims.

Concretely, that means:

- **Nothing has been verified on the air.** Treat every "supported" model below as
  "implemented from documentation, awaiting confirmation".
- **It keys transmitters.** The safety interlocks are implemented and tested
  (dead-man TX timeout, lock expiry dropping PTT, power clamping, band limits),
  but they have been tested against fakes, not against a transmitter.
- Expect to find protocol bugs. If you do, the per-model differences are collected
  in [docs/DESIGN.md](docs/DESIGN.md) §5.2–§5.4, which is the place to look first.

Do not leave it running an unattended station yet.

## Supported radios

The **Tested** column marks radios confirmed against real hardware. It is
deliberately empty: see the status note above.

### Icom (`civ` backend)

| Model | `civ.model` | Notes | Tested |
|---|---|---|---|
| IC-718 | `ic-718` | No CAT CW buffer; PTT on `1C 01`; no filter width | — |
| IC-7300 | `ic-7300` | | — |
| IC-7300MK2 | `ic-7300mk2` | | — |
| IC-7600 | `ic-7600` | | — |
| IC-7610 | `ic-7610` | | — |
| IC-7700 | `ic-7700` | | — |
| IC-7760 | `ic-7760` | | — |
| IC-7850 / IC-7851 | `ic-7850` | One profile; identical over CI-V | — |
| IC-905 | `ic-905` | 6-byte frequency field on the 10 GHz band | — |
| IC-910H | `ic-910h` | Own mode-code table; no CAT CW buffer | — |
| IC-9100 | `ic-9100` | | — |
| IC-9700 | `ic-9700` | | — |
| other Icom | `generic` | Requires an explicit `civ.rig_address` | — |

### Kenwood (`kenwood` backend)

| Model | `kenwood.model` | Notes | Tested |
|---|---|---|---|
| TS-590S | `ts590s` | | — |
| TS-590SG | `ts590sg` | | — |

Support for the TS-480, TS-890S and TS-990S is in progress. The TS-890S and
TS-990S are a noticeably different dialect — no `IF;` bulk status, `OM` in place
of `MD`, and data mode folded into the mode code.

### Anything else (`rigctld` backend)

Any rig [Hamlib](https://hamlib.github.io/) supports, by talking to a `rigctld`
process over TCP. remoses speaks the protocol in pure Go and can launch and
supervise the daemon itself, so there is no cgo and no LGPL linking.

Capabilities are read from the running rig at connect, so what works depends on
the Hamlib backend rather than on remoses.

### Yaesu

Not supported yet. Research against the FT-891, FT-991A, FT-710, FTdx10,
FTdx101 and FTX-1 CAT references is under way; the outcome will land in
`docs/yaesu-plan.md`.

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
make          # build into build/
make test     # go test -race ./...
make check    # fmt-check + vet + test, the pre-commit target
make cross    # release binaries for linux, darwin and windows into dist/
```

Single static binary, no cgo. Cross-compiles to linux/amd64, linux/arm64,
linux/arm, darwin/amd64, darwin/arm64 and windows/amd64.

## Configuration

Copy [`remoses.example.yaml`](remoses.example.yaml) and edit. Unknown keys are a
startup error rather than a silent default, and the whole file is validated
before anything opens a port:

```sh
remoses -config remoses.yaml -check
```

`remoses passwd` generates the bcrypt hashes the config file wants.

## Documentation

[docs/DESIGN.md](docs/DESIGN.md) — architecture, the concurrency model, the
locking and safety design, the CW pacing loops, and the per-model protocol
references with the differences between radios spelled out.

[api/openapi.yaml](api/openapi.yaml) — the HTTP API, and the source of truth a
conformance test holds the router to.

## Licence

BSD 3-Clause. See [LICENSE](LICENSE).
