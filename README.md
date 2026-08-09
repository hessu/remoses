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

**Experimental. Two radios out of the thirty-three below have ever been connected —
but both have been put through everything they can do.**

Every protocol detail below was transcribed from the manufacturers' own CI-V and
CAT reference documentation and is exercised against simulated rigs in the test
suite. That proves the code matches what the manual says. It does not prove the
manual is right, that the transcription is right, or that a particular radio
behaves the way its documentation claims — which is why the two radios that have
been on the wire matter more than the thirty-one that have not.

### What has been verified on hardware

An **IC-7610**, over its native USB, on the air. Everything remoses implements
for it was exercised: connect and reconnect, the full poll, all ten modes, all
three filter slots and the filter-width ladder, transmit power, PTT, Icom
Transceive push updates tracking the VFO knob, the REST and WebSocket APIs, the
lock lifecycle including steal and expiry, both its VFOs with split and dual
watch, and CW both ways — the rig's own CAT keyer and locally generated Morse
keying DTR and RTS.

An **IC-9700**, on 2 m and 70 cm with real antennas. Its CAT surface end to end:
Main's two VFOs addressed without disturbing each other, split, every mode it has
including **DV**, band limits, explicit PTT, leaving memory mode, CW break-in,
and CW keyed and **heard on the air**. Not a re-run of the whole daemon — locking,
the WebSocket and the interlocks are shared code already exercised on the
IC-7610 — but everything CI-V-specific to that radio.

A **TS-590S** on its built-in USB — the first non-Icom radio to be tried, and the
first test of the Kenwood backend against anything but its own unit tests.
Frequency, mode, transmit power, the filter width and both IF filter slots,
explicit PTT, CW break-in in all three states, and CW **heard on the air** at 5 W
on 28.030 MHz, both semi and full break-in. It found two bugs in the process: one
that stopped it connecting at all, and the break-in gap again.

**The safety interlocks were fired for real**, not against fakes: band limits
refusing an out-of-band tune, power clamping, the dead-man `tx_timeout` forcing
receive mid-transmission, and lock expiry cutting a live CW transmission inside a
character. CW pacing was measured at **61 ms of drift over 18.3 seconds** on a rig
whose buffer cannot be queried, so the timing is dead reckoning.

**Between them they found eleven bugs that no amount of reading the reference
would have.** Values written but never read back, so they reported a stale
figure for ever; setters that quietly changed a neighbouring setting, because on
CI-V several settings share one command; capabilities describing the radio's own
keyer while a different one was installed; a config default that addressed every
Icom as an IC-7610; a poll counter that treated a refusal as silence and
reconnect-looped a healthy radio; a `Mode` that could not decode its own output;
and a serial port opened with its control lines already high, which one radio
answered by saying nothing whatsoever. All eleven are fixed, with the
measurements and reasoning in [docs/DESIGN.md](docs/DESIGN.md) §5.4, §6 and §11.2.

The worst of them was invisible from this side: **CW accepted, queued, drained
on schedule — and never transmitted**, because the radio's break-in was off. Only
an operator listening could have caught it — and it happened again, on the
TS-590S, after it had already been found and fixed on the IC-9700, because the
Kenwood backend had no notion of break-in at all. **Expect the other thirty
models to be hiding something similar.**

### What that still does not tell you

- **Three radios have been verified**, two Icoms and one Kenwood. Treat every
  other model below as "implemented from documentation, awaiting confirmation".
  The Yaesu backends, the remaining ten Icom profiles and the other four Kenwood
  profiles have never seen a radio.
- **`limits.bands` gates tuning, not transmitting.** There is no transmit-only
  band limit, so it cannot express "receive anywhere, transmit only here" — which
  is what you want if a band has no antenna on it.
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

The **Tested** column marks radios confirmed against real hardware. Three entries
are filled in: see the status note above.

"Tested" there means every CAT feature remoses implements **for that radio** was
exercised on the air — frequency, all its modes, filters, power, PTT, whatever
VFO and split it has, and CW actually heard — not that the radio was seen to
connect.

It does not mean each radio re-ran the whole daemon. The parts that are not
radio-specific — locking, the WebSocket, the safety interlocks, reconnect —
were exercised once, on the IC-7610, and are shared code; the IC-9700 and the
TS-590S confirmed the protocol surface and CW, not those again.

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
| IC-9700 | `ic-9700` | Main's two VFOs and split; sub band present but not readable; CW break-in | **yes** |
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

**The IC-9700 also has two addressable VFOs, but they mean something else.** Its commands `25`
and `26` select the *selected and unselected VFO of the Main band*, where the IC-7610's select the
main and sub bands — one opcode, two axes. So on an IC-9700 remoses exposes Main's two VFOs and
split, and `caps.vfo_addressing` reads `relative`: A is whichever VFO the operator has selected
and B is the other, because nothing in that radio's protocol reports which letter is which and
remoses will not guess. (On an IC-7610 it reads `named` and the labels are stable.)

**Its sub band is deliberately left alone.** The radio has one and it receives independently, but
no command addresses it — the only route is "select the sub band", which would move the
operator's focus and fight whoever is holding the dial. So `caps.sub_receiver` is true and
`caps.sub_receiver_readable` is false, and remoses never switches bands to read a meter.

**CW break-in is controllable, and it matters more than it sounds.** On an Icom a CW message sent
over CAT is transmitted only if break-in is on or the transmitter is already keyed — otherwise
the rig accepts the message, empties its buffer on schedule and puts nothing on the air.

This is not Icom-specific; it caught a Kenwood too. remoses reads the setting
(`state.break_in`, `caps.break_in_control`), lets you change it (`{"break_in": "semi"}`), and
**never lets CW be queued that would go nowhere**. What it does about it is `cw.break_in`:
`semi` (the default) or `full` switch break-in on before sending, because a remote operator
cannot reach the front panel; `manual` writes nothing and returns a 422 naming the fix, for a
station that sequences its own transmit path. Semi is the default because full is QSK and will
clock your relays between elements — that is a choice to make deliberately. Radios whose
reference has not been read for the command are never blocked by the check.

**Memory mode is not modelled — but getting out of it is.** `{"vfo_mode": true}` returns the
radio to VFO operation, which is what an operator stuck on a memory channel actually needs; a rig
left there refuses the per-VFO commands and its readings go stale.

### Kenwood (`kenwood` backend)

| Model | `kenwood.model` | Notes | Tested |
|---|---|---|---|
| TS-480 | `ts480` | No DATA mode, no filter selection; break-in on `VX` is **inferred**, see below | — |
| TS-590S | `ts590s` | Frequency, modes, filters, power, PTT, break-in and CW exercised on the air | **yes** |
| TS-590SG | `ts590sg` | Same command set as the S; the two differ only in `ID` | — |
| TS-890S | `ts890s` | PTT cannot be polled, only pushed | — |
| TS-990S | `ts990s` | 200 W; PTT cannot be polled, only pushed; **`FA`/`FB` are Main/Sub bands, not VFOs** | — |
| other Kenwood | `generic` | TS-590 shape, but no break-in — see below | — |

**The TS-590SG shares the S's command set entirely.** Their reference is one document, which
marks every command remoses uses "[TS-590S / TS-590SG common]"; the two differ only in the `ID`
they answer with, so everything verified on the S holds for the SG. A test asserts the profiles
stay identical, because a fix applied to one and not the other would stay invisible until
somebody put an SG on the air.

The TS-890S and TS-990S are a noticeably different dialect: `OM` in place of
`MD`, data mode folded into the mode code, no `IF;` bulk status — and with it no
way to poll PTT at all, so it arrives only through auto-information pushes.

**Both VFOs are addressable, and split with them.** `FA` and `FB` read and set
VFO A and VFO B directly without selecting either, so a client can park a
frequency on B while the operator works A; `FR` and `FT` then say which is
received and which transmitted, and that relationship *is* split on this
protocol — there is no split flag to write. What the family does not offer is a
per-VFO **mode**: `MD` applies to whichever VFO is selected and nothing
addresses the other one's, so `caps.per_vfo_mode` is false and `state.vfo_a` /
`state.vfo_b` carry frequencies only. Reaching the other VFO's mode would mean
selecting it, sending `MD` and selecting back — moving the operator's radio
under them, and leaving it wrong if the sequence failed halfway.

**Except on the TS-990S, where `FA` and `FB` are not VFOs at all.** Its
reference names them "Main Band Frequency" and "Sub Band Frequency": two
receivers, each with its own VFO A and B underneath. That is the same opcode
pointing at a different axis, exactly as the IC-7610's `25`/`26` differ from the
IC-9700's — so remoses reports `caps.vfos: ["current"]` there and refuses to
address A or B, rather than calling that radio's Sub band "VFO B".

**CW break-in is spelled four different ways across these five radios**, and it
is not a cosmetic difference: with break-in off, a `KY` message is accepted, the
rig's buffer drains on schedule and nothing is transmitted. The TS-990S has a
`BI` command taking off, semi and full directly. The TS-890S has `BI` with only
off and on, and the `SD` delay decides which kind — 0 ms *is* full break-in. The
TS-590S and SG have no break-in command at all: they use `VX`, which sets VOX,
"except that when transmitting the VX command in CW mode, the Break-in function
is set and read, rather than the VOX function" — one command with two meanings,
chosen by the mode the radio happens to be in, so remoses only touches it in CW
and refuses a break-in request in any other mode rather than switching voice VOX
on behind you.

**The TS-480 is treated the same way, and that one is an inference.** Its reference documents
`VX` as the VOX function and says nothing about CW. But it has `SD`, the CW break-in *delay*, so
break-in exists on the radio; it has no `BI`; and across this family those two facts move
together — the TS-890S and TS-990S, which do have `BI`, both fence `VX` off with "cannot be set
in modes other than SSB/AM/FM", while the TS-590, which does not, overloads it. The TS-480 has
neither a `BI` nor that fence, so the silence reads as an omission. If an operator reports VOX
switching on when they send CW, this is the assumption that was wrong.

`generic` gets **no** break-in, and that is a refusal rather than a gap. The inference above is
one about Kenwood; it says nothing about the Elecrafts and modern Yaesus that also speak this
dialect, and break-in is the one command in the profile that *writes* on the strength of a guess.
Being wrong there leaves VOX switched on — invisibly, since remoses only writes `VX` in CW, so it
surfaces later when the operator moves to SSB and the radio starts keying on room noise. Name
your model to get the check.

**The TS-590S needed its control lines raised, not merely set high.** Opening the
port with DTR and RTS already asserted produced a radio that answered nothing
whatsoever — correct speed, correct device, well-formed frames going out, not one
byte coming back. remoses now opens every port with both lines low and drives
them to their configured state immediately afterwards, so a port configured high
gets a low-to-high transition. See `port.dtr` / `port.rts` below.

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
