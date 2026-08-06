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

**Experimental. One radio out of the thirty-three below has ever been connected — but
that one has now been put through everything.**

Every protocol detail below was transcribed from the manufacturers' own CI-V and
CAT reference documentation and is exercised against simulated rigs in the test
suite. That proves the code matches what the manual says. It does not prove the
manual is right, that the transcription is right, or that a particular radio
behaves the way its documentation claims — which is why the one radio that has
been on the wire matters more than the thirty-two that have not.

### What has been verified on hardware

An **IC-7610**, over its native USB, on the air. Everything remoses implements
for it was exercised: connect and reconnect, the full poll, all ten modes, all
three filter slots and the filter-width ladder, transmit power, PTT, Icom
Transceive push updates tracking the VFO knob, the REST and WebSocket APIs, the
lock lifecycle including steal and expiry, and CW both ways — the rig's own CAT
keyer and locally generated Morse keying DTR and RTS.

**The safety interlocks were fired for real**, not against fakes: band limits
refusing an out-of-band tune, power clamping, the dead-man `tx_timeout` forcing
receive mid-transmission, and lock expiry cutting a live CW transmission inside a
character. CW pacing was measured at **61 ms of drift over 18.3 seconds** on a rig
whose buffer cannot be queried, so the timing is dead reckoning.

**It found five bugs that no amount of reading the reference would have.** Two
values remoses wrote but never read back, so both reported a stale figure for
ever; two setters that quietly changed a neighbouring setting, because on CI-V
several settings share one command; and capabilities that described the radio's
own keyer while a different one was installed. All five are fixed, with the
measurements and reasoning in [docs/DESIGN.md](docs/DESIGN.md) §5.4 and §11.2.
**Expect the other thirty-two models to be hiding something similar.**

### What that still does not tell you

- **Only the IC-7610 has been verified.** Treat every other model below as
  "implemented from documentation, awaiting confirmation". The Yaesu, Kenwood and
  remaining Icom backends have never seen a radio.
- **One failure mode is untested and unfixable here:** pulling the cable *while
  transmitting*. The rig stays keyed and remoses cannot reach it — the link it
  would send the unkey over is the one that vanished. Only the radio's own
  time-out ends that, so set one.
- **A few known rough edges are recorded rather than fixed**, including the CW
  queue position reported after a `replace` and the lock lease not being extended
  by a long transmission. They are in `DESIGN.md`; none of them keys a
  transmitter.
- Expect to find protocol bugs. The per-model differences are collected in
  [docs/DESIGN.md](docs/DESIGN.md) §5.2–§5.7, which is the place to look first.

Do not leave it running an unattended station yet.

## Supported radios

The **Tested** column marks radios confirmed against real hardware. Exactly one
entry is filled in: see the status note above.

"Tested" there means every feature remoses implements for that radio was
exercised on the air, not that the radio was seen to connect.

### Icom (`civ` backend)

| Model | `civ.model` | Notes | Tested |
|---|---|---|---|
| IC-718 | `ic-718` | No CAT CW buffer; PTT on `1C 01`; no filter width | — |
| IC-7300 | `ic-7300` | | — |
| IC-7300MK2 | `ic-7300mk2` | | — |
| IC-7600 | `ic-7600` | | — |
| IC-7610 | `ic-7610` | Both VFOs, split and dual watch; every implemented feature exercised on the air, interlocks included | **yes** |
| IC-7700 | `ic-7700` | | — |
| IC-7760 | `ic-7760` | | — |
| IC-7850 / IC-7851 | `ic-7850` | One profile; identical over CI-V | — |
| IC-905 | `ic-905` | 6-byte frequency field on the 10 GHz band | — |
| IC-910H | `ic-910h` | Own mode-code table; no CAT CW buffer | — |
| IC-9100 | `ic-9100` | | — |
| IC-9700 | `ic-9700` | | — |
| other Icom | `generic` | Requires an explicit `civ.rig_address` | — |

**The IC-7610 is the only radio here whose second VFO remoses can reach.** Its commands `25` and
`26` name a VFO in the frame, so both can be read and set without selecting either; everywhere
else the protocol only offers "the VFO the radio is on", and remoses controls that rather than
labelling it A or B. So on an IC-7610 the API exposes both VFOs — each with its own frequency,
mode, data mode and filter — plus **split** (transmit on the other VFO) and **dual watch**
(receive on both at once, with the second receiver's S-meter streaming while it runs). Clients
should read `caps.vfos`, `caps.split`, `caps.dual_watch` and `caps.per_vfo_mode` rather than
inferring any of it from the model name.

Other Icoms very likely have `25` and `26` too. They stay off until each radio's own reference
has been read, which is the same rule the rest of this table follows.

**The IC-9700's second receiver is a different shape and is not supported yet.** It has two
receivers, each with its own VFO A/B and memory mode, and transmits only on Main — so its split
means "the other VFO of Main" where the IC-7610's means "the sub receiver". Fitting both needs a
receivers-and-VFOs model; see [docs/DESIGN.md](docs/DESIGN.md) §5.4.

### Kenwood (`kenwood` backend)

| Model | `kenwood.model` | Notes | Tested |
|---|---|---|---|
| TS-480 | `ts480` | No DATA mode, no filter selection | — |
| TS-590S | `ts590s` | | — |
| TS-590SG | `ts590sg` | | — |
| TS-890S | `ts890s` | PTT cannot be polled, only pushed | — |
| TS-990S | `ts990s` | 200 W; PTT cannot be polled, only pushed | — |
| other Kenwood | `generic` | TS-590 shape | — |

The TS-890S and TS-990S are a noticeably different dialect: `OM` in place of
`MD`, data mode folded into the mode code, no `IF;` bulk status — and with it no
way to poll PTT at all, so it arrives only through auto-information pushes.

### Yaesu (`yaesu` backend)

| Model | `yaesu.model` | Notes | Tested |
|---|---|---|---|
| FT-950 | `ft-950` | 8-digit frequency, 27-byte `IF`; no PSK | — |
| FTdx1200 | `ftdx1200` | 8-digit; two `ID` values; no PSK, no DATA-FM, no AM-N | — |
| FTdx3000 | `ftdx3000` | 8-digit; no PSK | — |
| FTdx5000 | `ftdx5000` | 8-digit; `PC` is a `000`–`255` index, **not watts** | — |
| FTdx9000 | `ftdx9000` | 8-digit; **no `ID`** — cannot be identified; `SH` is not a bandwidth; `PC` is an index | — |
| FT-891 | `ft-891` | No PSK and no DATA-FM; narrow flag inside `SH` | — |
| FT-991A | `ft-991a` | Mode code `E` is **C4FM**, not PSK; six-byte `SH` | — |
| FT-710 | `ft-710` | | — |
| FTdx10 | `ftdx10` | Push updates work only over the USB CAT port | — |
| FTdx101D | `ftdx101d` | | — |
| FTdx101MP | `ftdx101mp` | 200 W | — |
| FTX-1 | `ftx-1` | 30-byte `IF`; `PC` names the head; C4FM-DN and C4FM-VW | — |
| other Yaesu | `generic` | FTdx101 shape | — |
| FT-857 | `ft-857` | **Binary CAT** — a different protocol; see below | — |
| FT-857D | `ft-857d` | | — |
| FT-897 | `ft-897` | | — |
| FT-897D | `ft-897d` | | — |

Naming the model matters more here than on any other backend, for two separate reasons.

The first is that it chooses the **protocol**. The last four radios above speak a CAT system
that has nothing in common with the others but the manufacturer: five fixed binary bytes with
the opcode last, packed-BCD frequencies, seventeen commands in total, and no terminator or
framing of any kind. `backend: yaesu` is still correct for them — remoses dispatches on the
model name, so you never have to know which generation your radio belongs to — but an FT-857
left unnamed will not work, because an unnamed Yaesu means the modern dialect.

The second is that within the modern family the mode-code tables are per radio rather than per
family: `E` selects PSK on five of them, C4FM on the FT-991A, and nothing at all on the other
six, so the wrong name reports the wrong mode instead of failing. The five older radios are
also an **eight-digit** generation — `FA14025000;` where the newer seven take `FA014025000;` —
and their `IF` answer is a byte shorter to match, so a wrong name there produces a malformed
command rather than an error.

Two of the older radios report **transmit power as an uncalibrated `000`–`255` index rather
than watts**, so remoses shows a percentage and refuses a request in watts on them: the FTdx5000
and the FTdx9000.

The **FTdx9000 has no `ID` command**, so remoses cannot cross-check that the configuration names
the right radio — it says which command set it is using and carries on. Its `SH` is the position
of the WIDTH knob rather than a bandwidth in Hz, so remoses reports no filter width for it and
refuses to set one.

None of these radios can key arbitrary CW text over CAT — `KY` plays a stored keyer memory, and
remoses will not overwrite the operator's saved messages to send one — so CW on a Yaesu means
`cw.method: serial_key`, keying DTR or RTS. Every modern model supports it through its
`PC KEYING` menu item; the FT-857/FT-897 have no CW-over-CAT command at all, so it is the only
option there too.

That path is the one part of Yaesu CW support that has been confirmed on hardware, because it is
not Yaesu-specific: `serial_key` generates the Morse locally and keys a control line, and it was
tested on both DTR and RTS against an IC-7610's USB keying. What has never been exercised on a
Yaesu is the CAT side — the frames in the tables above.

**The FT-857 and FT-897 are more limited than the rest**, and it is the radios rather than
remoses: their seventeen-command CAT set has no way to set or read **transmit power**, no
**filter width or filter selection**, no **push updates** (so a front-panel knob movement shows
up at the next poll rather than immediately), and no **model identification**. Its one VFO
command is a blind toggle that reports nothing, so remoses controls whichever VFO the radio is
on and does not offer A and B by name. Frequency, mode, PTT, the S-meter, the transmit power
meter and the high-SWR warning all work.

They also need their serial port set up by hand: **4800 bps** out of the box (menu 019
`CAT RATE`, which also offers 9600 and 38400) and **two stop bits**, where remoses defaults to
115200 and one. Menu 020 `CAT/LIN/TUN` has to be on `CAT` as well, or the rear-panel jack is
driving a linear amplifier instead. See `remoses.example.yaml`.

### Anything else (`rigctld` backend)

Any rig [Hamlib](https://hamlib.github.io/) supports, by talking to a `rigctld`
process over TCP. remoses speaks the protocol in pure Go and can launch and
supervise the daemon itself, so there is no cgo and no LGPL linking.

Capabilities are read from the running rig at connect, so what works depends on
the Hamlib backend rather than on remoses.

## Connections

A radio can be reached three ways, all sharing the same supervised
dial/backoff/reconnect path:

- a **local serial port**, by device path or — preferably — by USB VID/PID and
  serial number, which survives a replug;
- a **serial port over the network**, `port.tcp: host:port`, as published by
  ser2net or a hardware terminal server;
- **`rigctld`** over TCP.

## remoses-cli

`remoses-cli` is a **read-only** terminal monitor for one radio. It fetches the
current state over REST so there is something on screen at once, then follows
the WebSocket stream. It never issues a `PATCH`, `POST` or `DELETE` and never
takes a lock — a monitor that took the lock would lock out the operator actually
working the radio.

```sh
remoses-cli ic7610                       # watch the local instance
remoses-cli -url https://radio.example.net ic7610
remoses-cli -once ic7610                 # print the state once and exit
remoses-cli ic7610 | tee ic7610.log      # timestamped lines instead of a redraw
```

```
remoses  ic7610 - IC-7610 - civ                                      CONNECTED
------------------------------------------------------------------------------

  14.025.000 MHz    CW                                          PTT   >> TX <<

  S ██████████████████░░  S9+21 dB  230/255

  passband  500 Hz   filter 2                                power  40 %  40 W
  cw        sending  queued 14  28 wpm  ~4.3 s                   lock   oh2abc

  seq 4471   updated 0.0 s ago                                    stream  live
```

**Where it connects.** With no `-url`, the server address is read from the
daemon's own configuration file (`remoses.yaml` by default, `-config` to point
elsewhere): `server.listen` and `server.base_path` give the URL, and whether
`server.tls` is set decides `http` or `https`. A wildcard bind such as
`0.0.0.0:8080` is not an address a client can dial, so it resolves to loopback.
If the default configuration file is not there, the daemon's built-in defaults
are assumed — `http://127.0.0.1:8080/api/v1`. `-url` overrides all of it, and
fills in `/api/v1` when the URL has no path of its own.

**Credentials.** The username comes from `-user` or `$REMOSES_USER`, the password
from `$REMOSES_PASSWORD`, `-password-file` (`-` for stdin) or a prompt with echo
off. There is deliberately **no password flag**: a password on the command line
is visible in the process table and lands in shell history. The configuration
file is no help either — it holds bcrypt hashes, not passwords.

**What it does about failure.** A radio that is unplugged is shown as such, with
the last known state and how old it is, because that is a normal state rather
than an error. A dropped stream reconnects with exponential backoff and says so
while it is trying. A `resync` — the server's way of saying this client fell
behind — refetches the state over REST rather than guessing what was missed.
Bad credentials and an unknown radio id are reported and are not retried.

**Output adapts.** On a terminal the display is redrawn in place; anywhere else
it becomes one timestamped `key=value` line per change, so `| tee log` produces
something readable. Meter-only changes are throttled there (`-interval`,
default 1s) — the S meter moves on every poll, and twenty lines a second of
nothing else is not a log.

## Building

```sh
make          # build remoses and remoses-cli into build/
make test     # go test -race ./...
make check    # fmt-check + vet + test, the pre-commit target
make cross    # release binaries for linux, darwin and windows into dist/
```

Single static binary per command, no cgo. Cross-compiles to linux/amd64,
linux/arm64, linux/arm, darwin/amd64, darwin/arm64 and windows/amd64.

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
