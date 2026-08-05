# remoses

Remote control of amateur radio transceivers over an authenticated HTTP and
WebSocket API.

remoses runs on a machine physically next to the radios, talks to each one over
USB serial (CAT / CI-V), and exposes frequency, mode, filter, power, PTT and CW
sending to network clients. It drives several radios at once, each identified by
a stable id and a human-readable name.

Because remoses is local to the rig, all CW element timing is generated
server-side — network latency affects only how quickly text is queued, never the
quality of the Morse that goes out.

Initial rig support:

| Backend | Rigs |
|---|---|
| `civ` | Icom CI-V — IC-7610 and relatives |
| `kenwood` | Kenwood TS-590S / TS-590SG, and the wider Kenwood-style ASCII CAT family |
| `rigctld` | Anything Hamlib supports, via a `rigctld` process |

Scope for v1 is control only. Audio is a separate concern.

## Status

Under construction. See [docs/DESIGN.md](docs/DESIGN.md) for the full design:
architecture, configuration, protocol references verified against the
manufacturers' documentation, the locking model, and the CW pacing loops.

## Building

```sh
go build ./cmd/remoses
```

Single static binary, no cgo, cross-compiles to linux/amd64, linux/arm64, darwin
and windows.

## Licence

TBD.
