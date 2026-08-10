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

**Experimental. Four radios out of the thirty-seven below have ever been connected —
but all four have been put through everything they can do.**

Every protocol detail below was transcribed from the manufacturers' own CI-V and
CAT reference documentation and is exercised against simulated rigs in the test
suite. That proves the code matches what the manual says. It does not prove the
manual is right, that the transcription is right, or that a particular radio
behaves the way its documentation claims — which is why the four radios that have
been on the wire matter more than the thirty-three that have not.

### What has been verified on hardware

An **IC-7610**, over its native USB, on the air. Everything remoses implements
for it was exercised: connect and reconnect, the full poll, all ten modes, all
three filter slots and the filter-width ladder, transmit power, PTT, Icom
Transceive push updates tracking the VFO knob, the REST and WebSocket APIs, the
lock lifecycle including steal and expiry, both its VFOs with split and dual
watch, and CW both ways — the rig's own CAT keyer and locally generated Morse
keying DTR and RTS. Later, its **antenna tuner**: switched in and out, and four
tuning cycles run for real on 80 m, three that found a match and one that could
not. Later still, its **power switch** — off to standby, woken again over the bus
with nobody at the radio — and the whole **receive front end**: both preamplifiers,
the 3 dB attenuator ladder, RF gain, AGC, IP+ and DIGI-SEL with its shift. That
last one turned up an interlock in no manual: with DIGI-SEL engaged the radio
refuses to switch a preamplifier in.

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
on 28.030 MHz, both semi and full break-in. Later, the **antenna tuner**: switched
in and out, and four tuning cycles run for real on 80 m — three that found a match
and one that could not. Later still, its **receive front end**: preamplifier,
attenuator, RF gain and all three AGC states, then the **noise blanker, noise
reduction, both notches and the antenna selector**. It found six bugs in the
process: one that stopped it connecting at all, the break-in gap again, two in
the tuner command, one that made switching the AGC off a trip with no way back,
and one where a request the radio silently ignored was reported as a success.

An **FT-857D**, the first Yaesu of either generation and the first radio of the
FT-857/FT-897 family — a **completely different CAT protocol** from every other
Yaesu, five binary bytes with no framing of any kind, and until now written
entirely from a manual. Everything that protocol has: the packed-BCD frequency
field across its whole 100 kHz – 470 MHz range, every mode including **WFM read
back but correctly refused as a set**, PTT and the transmit power meter under a
**10 W carrier on 80 m and 10 m**, the undocumented acknowledgement byte that the
whole framing design rests on, disconnect and reconnect through a power cycle,
and the interlocks again — band limits, the dead-man `tx_timeout`, and a lapsed
lock dropping PTT. Its CW could not be tried: this radio has no CAT keyer at all,
and the `serial_key` path needs a second serial port that was not there.

**The safety interlocks were fired for real**, not against fakes: band limits
refusing an out-of-band tune, power clamping, the dead-man `tx_timeout` forcing
receive mid-transmission, and lock expiry cutting a live CW transmission inside a
character. CW pacing was measured at **61 ms of drift over 18.3 seconds** on a rig
whose buffer cannot be queried, so the timing is dead reckoning.

**Between them they found fourteen bugs that no amount of reading the reference
would have.** Values written but never read back, so they reported a stale
figure for ever; setters that quietly changed a neighbouring setting, because on
CI-V several settings share one command; capabilities describing the radio's own
keyer while a different one was installed; a config default that addressed every
Icom as an IC-7610; a poll counter that treated a refusal as silence and
reconnect-looped a healthy radio; a `Mode` that could not decode its own output;
a serial port opened with its control lines already high, which one radio
answered by saying nothing whatsoever; a VFO selection accepted as a success by a
radio that cannot address a VFO at all; a data-mode flag carried forward into
modes that have no data spelling, which left one radio with no way out of its
packet mode; and a display drawing `power  0 %` for a radio with no power command,
which reads as a fault rather than as a silence. All fourteen are fixed, with the
measurements and reasoning in [docs/DESIGN.md](docs/DESIGN.md) §5.4, §5.7, §6 and
§11.2.

The worst of them was invisible from this side: **CW accepted, queued, drained
on schedule — and never transmitted**, because the radio's break-in was off. Only
an operator listening could have caught it — and it happened again, on the
TS-590S, after it had already been found and fixed on the IC-9700, because the
Kenwood backend had no notion of break-in at all. **Expect the other thirty-three
models to be hiding something similar.**

### What that still does not tell you

- **Four radios have been verified**, two Icoms, a Kenwood and a Yaesu. Treat
  every other model below as "implemented from documentation, awaiting
  confirmation". The twelve **ASCII** Yaesu profiles — which are a different
  protocol from the FT-857D that was tested — the remaining fourteen Icom profiles
  and the other four Kenwood profiles have never seen a radio.
- **CW has never been sent by a Yaesu of any kind.** Every Yaesu here keys a
  control line rather than using a CAT keyer, and that path is confirmed on an
  IC-7610 and is not Yaesu-specific — but no Yaesu has run it.
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

The **Tested** column marks radios confirmed against real hardware. Four entries
are filled in: see the status note above.

"Tested" there means every CAT feature remoses implements **for that radio** was
exercised on the air — frequency, all its modes, filters, power, PTT, whatever
VFO and split it has, and CW actually heard — not that the radio was seen to
connect. The FT-857D is the one entry with an exception against it, and it is a
property of the radio rather than a gap: that generation has no CAT keyer, so its
CW is locally generated on a second serial port, and there was not one to use.

It does not mean each radio re-ran the whole daemon. The parts that are not
radio-specific — locking, the WebSocket, the safety interlocks, reconnect —
were exercised once, on the IC-7610, and are shared code; the IC-9700 and the
TS-590S confirmed the protocol surface and CW, not those again. The FT-857D did
re-run reconnect and the interlocks, because its CAT set has no keyer to spend
the session on and because it fails in a way the Icoms do not: switched off it
goes **silent** where an IC-7610 answers every command with a rejection.

### Icom (`civ` backend)

| Model | `civ.model` | Notes | Tested |
|---|---|---|---|
| IC-703 | `ic-703` | 10 W; no CAT CW buffer; no filter width (`1A 03` is the Set-mode menu); data mode on `1A 04` | — |
| IC-706 | `ic-706` | **No PTT and no power over CI-V**; no meter; no CW buffer; WFM | — |
| IC-706MKII | `ic-706mkii` | As the IC-706; address `4E` | — |
| IC-706MKIIG | `ic-706mkiig` | As above, plus an S-meter and CW break-in | — |
| IC-718 | `ic-718` | No CAT CW buffer; PTT on `1C 01`; no filter width | — |
| IC-7300 | `ic-7300` | | — |
| IC-7300MK2 | `ic-7300mk2` | | — |
| IC-7600 | `ic-7600` | | — |
| IC-7610 | `ic-7610` | Both VFOs, split and dual watch; every implemented feature exercised on the air, interlocks included | **yes** |
| IC-7700 | `ic-7700` | | — |
| IC-7760 | `ic-7760` | | — |
| IC-7850 / IC-7851 | `ic-7850` | One profile; identical over CI-V | — |
| IC-905 | `ic-905` | 6-byte frequency field on the 10 GHz band | — |
| IC-910H | `ic-910h` | Own mode-code table; no CAT CW buffer; break-in is off/on only | — |
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

**Not every radio says which kind of break-in.** An IC-910H's command is "0=OFF, 1=ON" where the
rest of its family has off, semi and full, so `state.break_in` reads `on` there rather than
guessing — the difference is audible, full being QSK. Setting `semi` or `full` on such a radio is
still accepted; both mean its single "on".

This is not Icom-specific; it caught a Kenwood too. remoses reads the setting
(`state.break_in`, `caps.break_in_control`), lets you change it (`{"break_in": "semi"}`), and
**never lets CW be queued that would go nowhere**. What it does about it is `cw.break_in`:
`semi` (the default) or `full` switch break-in on before sending, because a remote operator
cannot reach the front panel; `manual` writes nothing and returns a 422 naming the fix, for a
station that sequences its own transmit path. Semi is the default because full is QSK and will
clock your relays between elements — that is a choice to make deliberately. Radios whose
reference has not been read for the command are never blocked by the check.

**Transmit metering: forward power, SWR and ALC.** Where a radio reports them, remoses polls
them **only while the transmitter is keyed** and publishes them as `state.power_meter`,
`state.swr` and `state.alc`, with `caps.power_meter`, `caps.swr_meter` and `caps.alc_meter`
saying which exist. In receive the three fields are **absent rather than zero**, because a bar
drawn at 0 cannot be told from a real reading into a dead load — and because a 3:1 SWR left on
the display after a transmission ends reads as a fault that is still happening.

The Icom command set puts each on its own read: `15 11`, `15 12` and `15 13`. Two details there
are not guessable — an Icom's ALC runs to **120** of a possible 255, and the IC-9700's power
meter reaches 100% at **213** where the IC-7610's reaches 255 — so `scale` is per meter and per
model, and a client should compare `raw` against it rather than assume a range.

**SWR is also published as a number** in `state.swr_ratio`, but only where the radio's own
documentation calibrates its meter: Icom prints four points (`0`, `48`, `80`, `120` → 1.0, 1.5,
2.0, 3.0) and gets a figure, Kenwood and Yaesu print none and get only the bar. Above the top
calibrated point remoses stops rather than extrapolating — the curve is undocumented, an SWR that
high is a fault either way, and "7.4:1" would be a number invented about your antenna. A rigctld
radio is the exception: Hamlib reports a true ratio, so that one arrives calibrated.

**The radio itself can be switched off and on.** `{"power_switch": "off"}` and `"on"`, gated by
`caps.power_switch` — `18 00`/`18 01` on Icom, `PS` on Kenwood and Yaesu. It must be the only
field in the request: a patch that switched the radio off and set a frequency has no sensible
ordering, since one of the two is always addressed to a radio that is not listening.

Two things about it are worth knowing before using it remotely.

**`off` is the recoverable off.** Where a radio offers a choice — a Kenwood does — the default
draws more standby current and wakes on a single command, while `off_deep` matches the
front-panel switch and needs a longer wake sequence. A remote station that cannot be woken is one
somebody has to drive to, so the deeper sleep is opt-in.

**Waking works with no link, which is the state it is for.** remoses arms the wake and sends it
on its next connection attempt, on the freshly opened port before it tries to talk to the radio —
the one moment a sleeping rig can be reached. `on` is a single method whatever the radio needs:
the backend tries the cheap wake first and escalates to its family's ritual (a dummy byte, a
wait, a retry inside a timing window) only if that draws nothing. Expect several seconds of
booting; watch `connected`.

**Whether a wake works at all is a wiring question, not a command one.** An external CI-V
interface stays powered and listening. A radio whose CAT arrives over its own USB may take the
USB device down with it and leave nothing to send the wake-up to — so try it with somebody near
the radio before relying on it. On an **IC-7610 it works**: switched off over CAT and woken again
over the same link, with nobody touching the radio.

**A radio that is switched off is reachable, and remoses says so.** `state.standby` is a third
condition published alongside `connected: true`, and `remoses-cli` shows `STANDBY` in place of
`CONNECTED`. It is set however the radio came to be off — a `power_switch` request or the front
panel button — because the detection is simply that the radio answers every command with a
rejection. The session holds the port open and watches for it to wake rather than dropping a
link that is perfectly healthy, and commands meanwhile answer 503 with the remedy in the message.

That last part fixed a bug worth knowing about if you run an older build: an IC-7610 in standby
does not go silent, it refuses everything — so the "has this rig stopped answering" counter
excused every poll and left remoses reporting a connected radio over a snapshot going stale by
the minute.

**The antenna tuner can be switched and tuned.** `state.tuner` reads `off`, `on` or `tuning`;
`{"tuner": "on"}` switches the matching network in or out of line, and `{"tuner_tune": true}`
runs a tuning cycle. `caps.tuner_control` and `caps.tuner_tune` say which a radio has. It is
`1C 01` on Icom, `AC` on Kenwood and Yaesu.

**Starting a cycle transmits**, so it is gated like any other transmit path: it needs the lock,
it is refused outside `limits.bands`, and it arms the dead-man timer. That is also why it is a
separate field from `tuner` rather than a third value of it — a client reading the state and
writing it back must never key a radio by echoing `"tuning"` at it. The call returns as soon as
the radio accepts the command; watch `state.tuner` for the cycle, which remoses follows at the
fast poll rate while it runs.

**Tested on the air on both a TS-590S and an IC-7610**, four frequencies each on 80 m — three
that matched and one whose SWR was too high for the antenna.

The most useful thing they reported is not in either manual: **the tuner state itself says
whether a cycle succeeded.** A match ends on `on`, a failure ends on `off`. Both radios do it,
and no command reports it any other way.

They disagree about everything else, which is why remoses follows the radio rather than assuming.
A TS-590S reports PTT true through a cycle and returns real meter readings — the SWR visibly
falling as the tuner closes in. An IC-7610 reports PTT false throughout and answers zero to every
meter, and its cycles are shorter than a poll where it already knows the band. So the transmit
meters follow the rig's own PTT and nothing else: treating "tuning" as transmitting was tried and
reverted, because on the IC-7610 it published a zero SWR as a confident 1.0:1 — a perfect match —
at the moment the tuner was failing to find one.

The TS-590S also refuses two things its reference does not mention: an on/off set must send `P1`
as `0`, and **a set that changes nothing is refused as well** — so remoses reads before writing,
or the same request sent twice would fail the second time.

**Which Icoms have one is transcribed per model, not assumed.** Every reference has been read:
the IC-703, IC-7300, IC-7300MK2, IC-7600, IC-7610, IC-7700, IC-7760, IC-7850 and IC-9100 have
`1C 01`; the IC-905, IC-910H and IC-9700 have no such row, and neither does the IC-706 family.
Note that having a tuner is not the same as having one on the current band — the IC-9100's
covers HF and 50 MHz and the radio itself rejects a tune on 144 MHz and up.

**The IC-718 is the sharp edge.** There `1C 01` is *PTT*, not the tuner — so a "start tuning"
sent to one would key the transmitter and hold it keyed. It reports `tuner_control: false`, and
must.

**The IC-706 family cannot be keyed over CI-V at all.** None of the three has a transmitter
command — no `1C` at any sub-command — so remoses can neither key them nor tell whether they are
keyed, and none has an RF power level either. Both are front-panel controls, and PTT is a
footswitch, the microphone, or a control line. `caps.ptt_control` and `caps.power_control` say so,
`{"ptt": …}` and `{"power_w": …}` return 422, and neither is polled. These are the first radios
here to report either as false, so a client that assumed every rig can be keyed will want to
check them.

They are also the oldest radios in the table, and their manuals are correspondingly thin — the
original IC-706's whole command list is one narrow column. Two facts in those profiles come from
how the radios behave rather than from what the tables print: the IC-706 and MKII do answer "read
frequency" and "read mode" even though neither appears in their command tables, and the MKII's
factory address is `4E` rather than the `48` its data-format diagram shows. The first matters
because remoses fills its state by polling — a radio it could command but never read would never
connect — and the second is what `civ.rig_address` is for.

**Memory mode is not modelled — but getting out of it is.** `{"vfo_mode": true}` returns the
radio to VFO operation, which is what an operator stuck on a memory channel actually needs; a rig
left there refuses the per-VFO commands and its readings go stale.

**The receive front end is readable and settable.** `state.preamp`, `state.attenuator_db`,
`state.rf_gain` and `state.agc`, plus Icom's `state.ip_plus` and `state.digi_sel` with its
`digi_sel_shift`. Each is absent where the radio has no such command, and the matching capability
— `preamp_levels`, `attenuator_db`, `rf_gain_control`, `agc_settings`, `ip_plus_control`,
`digi_sel_control`, `digi_sel_shift_control` — says what to offer. On Icom they are `16 02`, `11`,
`14 02`, `16 12`, `16 65` and `16 4E` with `14 13`.

**The attenuator is in dB, not in steps**, because no two of these radios step it the same way: an
IC-7610 goes 3 dB at a time to 45, an IC-7850 to 21, an IC-7600 and IC-7700 have 6/12/18, and
everything smaller has one fixed pad. `caps.attenuator_db` lists them.

Two traps are per model and worth knowing:

- **`11` is the depth in BCD on every Icom but the IC-718**, whose own table says `01` means 20 dB
  where the IC-703's says `20` does. Same opcode, same pad, different byte, and nothing in the
  frame to tell them apart.
- **`16 12` has five spellings that share only the opcode.** The IC-7610 counts `01` FAST, `02`
  MID, `03` SLOW; the IC-7600 counts the same three from `00`; the IC-7700 has four, `00` being
  AGC OFF; the IC-703 has two, `1` fast and `2` slow; and the IC-910H has two the other way round,
  `0` slow and `1` fast. One byte out sets a different speed and looks like a success.

**Tested on an IC-9700 too**, which has five of the seven front-end controls plus the whole noise
and notch group: one preamplifier, a 10 dB pad, RF gain, AGC and IP+, and correctly no DIGI-SEL.
It turned up a restriction of its own, also unmentioned in any Icom reference here — **the AGC
cannot be set in FM.** All three speeds go in under USB and
every one draws a rejection in FM, while a read still answers `fast`, so the state looks healthy
and only the refusal says otherwise. remoses adds the reason and still sends the command, rather
than fencing off a mode on the strength of one radio.

**Tested on an IC-7610**, which has all seven controls, and which produced a finding that is in no
manual: **with DIGI-SEL engaged the radio refuses to switch a preamplifier in.** `16 02 01` draws
a bare NG while `16 02 00` is accepted and the read works throughout, so the refusal says nothing
about why. remoses adds the reason to the 422 and does not switch the preselector off to make the
request succeed — that would be changing a second control on a receiver somebody is listening to.
The radio enforces it from the other side as well: switching DIGI-SEL in while a preamplifier is
selected switches that preamplifier out, and the next poll reports it.

**The noise processing and the notches are there too.** `state.noise_blanker` and
`state.noise_reduction` with their levels, `state.notch` with a position and (on Icom) a width,
and `state.auto_notch`. On Icom they are `16 22`/`14 12`, `16 40`/`14 06`, `16 48`/`14 0D`/`16 57`
and `16 41`.

**The two notches cannot both run**, and the radios enforce it without saying so. A Kenwood is
honest — `NT` is one selector, off/auto/manual — but on an IC-7610 they are *separate commands*
that silently switch each other off, which no reference mentions. Verified on the radio in both
directions. `caps.notch_exclusive` says so and a request for both is refused, rather than applying
one and leaving the other to vanish.

**An IC-9700 in FM refuses `16 57`**, the notch width, which is correct — FM has no use for a DSP
notch. That exposed a general bug worth knowing about if you run an older build: **one refused
read used to stop the whole slow poll**, so everything queued behind it was skipped on every tick.
The automatic notch sat two places back and never appeared at all. A rejection now carries on to
the next read; only a transport failure stops the run.

**No Icom gets an antenna selector**, and that is about the radios. They have no live one: on an
IC-7610 the antenna is a per-band *memory* (`1A 05 02 76`–`02 87`, one entry per band range), so
switching it means writing a stored setting rather than throwing a switch. `caps.antennas` is 0.
Kenwood's `AN` and the FTdx101's are live selectors and are offered.

`preamp_levels` counts amplifiers rather than command values, which the IC-9700 is the reason for:
its `16 02` runs `00` to `03`, but those are the internal preamp and an external one in
combination — `02` is "internal off, external on", not a third stage of gain. It reports one, and
`02` and `03` are left to the front panel.

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

**The receive front end differs by generation more than anything else here**, and three of the
differences would put a syntax error on the wire rather than a wrong value. `PA` takes one digit
and answers two — the second is documented "always 0" — and `RA` takes two and answers four on the
TS-480 and TS-590, one and one on the TS-890S, and a band selector plus a digit on the TS-990S.

**`RG` counts to 100 on a TS-480 and to 255 on everything after it**, which is the same knob
reported on two scales a factor of two and a half apart. That is why the API publishes a
percentage and each model states its own ceiling.

**And the AGC moved commands.** A TS-480 keeps the speed on `GT` (`000` off, `001` fast, `002`
slow); every radio since keeps a *time constant* there and the speed on `GC`. Sending one radio's
form to the other sets the wrong thing entirely. The speeds differ too: a TS-590 has off, slow and
fast with no middle, a TS-890S and TS-990S have all three.

remoses does not read the AGC in FM on any of them. Every reference carries the same note — "this
command cannot be performed in FM mode (an error sounds)" — and the error sounds *at the radio*, so
polling anyway would beep at whoever is listening once per slow tick. A TS-480 makes it worse by
answering three spaces instead of refusing, which is decoded as "no reading" rather than as a
frame to complain about.

**The noise blanker and reducer are counts, not switches**: this family has NB1 and NB2, NR1 and
NR2, and they are different circuits rather than two strengths of one. NR2 is SPAC, whose level is
a *following speed* of 2 ms to 20 ms where NR1's is an effective level of 01 to 10 — so `nr_level`
is a percentage of whichever range is running. Both radios can also run both blankers at once and
answer `NB3` for it; that is a combination rather than a third blanker, so it is not published.

**The levels are refused while their circuit is off** — "an error occurs", in as many words — so
`NL` and `RL` are only asked for once the radio has reported the blanker or reducer on, and a
request applies each switch before its level.

**A TS-590S in CW ignores a request for the automatic notch.** No error, no change: `NT10` is
accepted and a read still answers `NT20`. The reason is sound — the automatic notch hunts for
tones, and in CW the tone is the signal you are listening to — but nothing in the exchange says the
request did not happen. remoses checks the read-back and answers 422 naming the mode, rather than
reporting a success that did not occur.

**Switching the AGC off is a one-way trip unless you know the trick**, which a TS-590S demonstrated
on the air: with the AGC off, `GC1` and `GC2` are *both* refused and the radio stays off. A client
that switched it off could never switch it back and would be told only "command rejected". The
manual documents the parameter that gets back out — "3: AGC Off → On (AGC returns to its Slow/Fast
status before turning Off)" — as one option among four; that the other two are *refused* from off,
making it the only door, is not in there and came from the radio. remoses sends it first when a
speed is asked for while the AGC is off, and only then. The TS-480 has no such value documented
and gets none of this.

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
| FT-857D | `ft-857d` | Frequency, every mode, PTT and the transmit meters exercised on the air; CW untried, as it needs a second serial port | **yes** |
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
refuses to set one. It is also the one radio here with **no attenuator command** — no `RA` row at
all — while keeping `PA`, `RG` and `GT`.

The receive front end is otherwise uniform across the modern family: `PA` for the preamplifier
(IPO, AMP 1, AMP 2 — the FT-891 has only IPO and AMP), `RA` for the attenuator, `RG` for the gain
and `GT` for the AGC. The FT-891, FT-991A and FTX-1 have a single pad where the FTdx sets step
6/12/18 dB.

**`GT` does not round-trip, and that is not a fault.** It *accepts* 0 to 4, where 4 is AUTO, and
*answers* 0 to 6, where 4, 5 and 6 are auto having settled on fast, mid or slow. So setting `auto`
and reading back `auto-mid` means the radio is telling you what auto currently is. remoses
publishes the resolved value rather than flattening the three into the one that was written, and
refuses a request for an `auto-*` reading — there is no way to tell a radio "be automatic, and
also be mid".

None of these radios can key arbitrary CW text over CAT — `KY` plays a stored keyer memory, and
remoses will not overwrite the operator's saved messages to send one — so CW on a Yaesu means
`cw.method: serial_key`, keying DTR or RTS. Every modern model supports it through its
`PC KEYING` menu item; the FT-857/FT-897 have no CW-over-CAT command at all, so it is the only
option there too.

That path is the one part of Yaesu CW support that has been confirmed on hardware, because it is
not Yaesu-specific: `serial_key` generates the Morse locally and keys a control line, and it was
tested on both DTR and RTS against an IC-7610's USB keying. **No Yaesu has ever run it**, and no
Yaesu has yet put CW on the air through remoses.

The CAT side has now been exercised on **one** Yaesu, an FT-857D — which is the binary protocol
at the bottom of this section rather than the ASCII dialect the twelve models above it speak.
None of those twelve has seen a radio.

**The FT-857 and FT-897 are more limited than the rest**, and it is the radios rather than
remoses: their seventeen-command CAT set has no way to set or read **transmit power**, no
**filter width or filter selection**, no **push updates** (so a front-panel knob movement shows
up at the next poll rather than immediately), and no **model identification**. Its one VFO
command is a blind toggle that reports nothing, so remoses controls whichever VFO the radio is
on and does not offer A and B by name. Frequency, mode, PTT, the S-meter, the transmit power
meter and the high-SWR warning all work, and all of that has now been seen on an FT-857D.

Nothing in the wire format differs across the four, so in principle what that radio confirmed
holds for the other three — but only the FT-857D has been on a wire, and the **Tested** column
says so rather than crediting a radio nobody plugged in.

**Three things that radio does are worth knowing before driving one remotely**, and none is in
any manual.

**A mode change moves the frequency.** Selecting CW adds the radio's CW pitch offset to the
displayed frequency — +600 Hz on the one tested — and DIG subtracts its own shift, −2120 Hz. So
`{"mode": "CW"}` on its own leaves the dial somewhere else. Sending frequency and mode together
is unaffected: remoses applies mode first for exactly this reason, and the frequency write puts
the offset back.

**Keying it over CAT in CW transmits nothing.** The radio goes into transmit — `ptt` reads true,
the relay closes — and the power meter stays at zero, because in CW the transmitter needs the key
line. `serial_key` is therefore not just the only way to *send* CW on these radios, it is the only
way to get RF out of them in CW at all; a CAT tune-up needs FM or AM. The useful side of this is
that CW is a safe mode for testing a remote setup, since nothing reaches the antenna.

**Switched off, it goes silent rather than refusing.** An Icom in standby answers every command
with a rejection, which remoses reports as `standby` on a live link. This one just stops
answering while its USB adapter stays present, so it is reported as **disconnected** — which is
the honest answer, and means `power_switch`-style remote waking is not available here in any
form. It reconnects by itself when the radio comes back.

They also need their serial port set up by hand: **4800 bps** out of the box (menu 019
`CAT RATE`, which also offers 9600 and 38400) and **two stop bits**, where remoses defaults to
115200 and one. Menu 020 `CAT/LIN/TUN` has to be on `CAT` as well, or the rear-panel jack is
driving a linear amplifier instead. See `remoses.example.yaml`. The radio tested was on 38400,
which is worth setting: at 4800 the fast poll's three commands take a visible slice of a 500 ms
tick.

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

  14.025.000 MHz    CW                                                PTT   RX

  S   ██████████████████░░  S9+21 dB  230/255

  passband  500 Hz   filter 2                                power  40 %  40 W
  cw        idle  queued 0  28 wpm                               lock   oh2abc

  seq 4471   updated 0.0 s ago                                    stream  live
```

**The meter block swaps while the radio is transmitting**, the way the radio's own meter does:

```
  14.025.000 MHz    CW                                          PTT   >> TX <<

  PWR ███████████▎░░░░░░░░  143/255  56 %
  SWR ██████▍░░░░░░░░░░░░░  1.4:1
  ALC ████████████░░░░░░░░  72/120  60 %

  passband  500 Hz   filter 2                                power  40 %  40 W
  cw        sending  queued 14  28 wpm  ~4.3 s                   lock   oh2abc
```

They swap rather than stack because during a transmission the S reading is not merely
uninteresting, it is wrong: on a Kenwood the command that reports it is reporting the power meter
instead, so what is left is whatever the last receive poll saw. Only the meters the radio actually
reports get a line — an FT-857 has power and a high-SWR bit and no ALC — and the piped form
carries the same readings as `pwr_raw=`, `swr_raw=`, `swr=` and `alc_raw=` fields, present only
while they exist.

**The same rule applies to the row above them.** `passband`, `filter` and `power` are drawn only
where the radio has the command behind each, and the row goes entirely when it has none of the
three — as on an FT-857, whose CAT set reads neither a bandwidth nor an output power. A zero
there would not be a small reading, it would be the absence of one, and `power  0 %` beside a
radio putting out ten watts reads as a fault. The piped form omits `passband=`, `filter=`,
`power_pct=` and `power_w=` for the same reason, which matters more in a log: it is read
afterwards by somebody who cannot ask what the zero meant.

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
