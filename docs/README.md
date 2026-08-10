# remoses user guide

Start here if you are setting remoses up in front of a radio.

## Setting up

- **[Configuration](configuration.md)** — the configuration file end to end:
  the server, users and passwords, how a radio's port is described, poll rates,
  band and power limits, and the dead-man timer.
- **[Controlling a radio](features.md)** — what remoses can do with a rig once
  it is connected, and how each control behaves: modes and filters, PTT and the
  transmit meters, CW, break-in, the antenna tuner, the receive front end, and
  switching the radio itself off and on.
- **[remoses-cli](remoses-cli.md)** — the read-only terminal monitor.

## Your radio

One page per backend. Each opens with how to configure that manufacturer's
radios and what the models differ in, and keeps the protocol detail for the end.

- **[Icom](icom.md)** — `civ`, seventeen profiles from the IC-703 to the IC-9700.
- **[Kenwood](kenwood.md)** — `kenwood`, the TS-480 through the TS-990S.
- **[Yaesu](yaesu.md)** — `yaesu`, which covers **two** unrelated CAT protocols:
  the modern ASCII one and the FT-857/FT-897 binary one.
- **[Anything else](rigctld.md)** — `rigctld`, for any rig Hamlib supports.

## Before you trust it

- **[Hardware status](hardware-status.md)** — which radios have actually been
  connected, what was exercised on each, and the bugs they found. Four of the
  thirty-seven models have been on a wire; everything else is implemented from
  the manufacturer's documentation and has never met a radio.

## Reference

- **[DESIGN.md](DESIGN.md)** — architecture, the concurrency model, the locking
  and safety design, the CW pacing loops, and the per-model protocol references
  with every difference between radios spelled out. This is the developer
  document; the pages above link into its sections rather than repeating them.
- **[api/openapi.yaml](../api/openapi.yaml)** — the HTTP API, and the source of
  truth a conformance test holds the router to.
