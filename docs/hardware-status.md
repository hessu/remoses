# Hardware status

**Only a handful of the radios remoses supports have ever been connected — but
every one of those has been put through everything it can do.**

Every protocol detail in this project was transcribed from the manufacturers'
own CI-V and CAT reference documentation and is exercised against simulated rigs
in the test suite. That proves the code matches what the manual says. It does
not prove the manual is right, that the transcription is right, or that a
particular radio behaves the way its documentation claims — which is why the
radios that have been on the wire matter more than the ones that have not.

## What "tested" means in the model tables

Every CAT feature remoses implements **for that radio** was exercised on the air
— frequency, all its modes, filters, power, PTT, whatever VFO and split it has,
and CW actually heard — not that the radio was seen to connect.

The FT-857D is the one entry with an exception against it, and it is a property
of the radio rather than a gap: that generation has no CAT keyer, so its CW is
locally generated on a second serial port, and there was not one to use.

It does not mean each radio re-ran the whole daemon. The parts that are not
radio-specific — locking, the WebSocket, the safety interlocks, reconnect — were
exercised once, on the IC-7610, and are shared code; the TS-590S confirmed the
protocol surface and CW, not those again. The FT-857D did re-run reconnect and
the interlocks, because its CAT set has no keyer to spend the session on and
because it fails in a way the Icoms do not. The IC-9700 re-ran reconnect too, as
a side effect of switching it off and waking it again.

## The radios that have been connected

### IC-7610

Over its native USB, on the air. Everything remoses implements for it was
exercised: connect and reconnect, the full poll, all ten modes, all three filter
slots and the filter-width ladder, transmit power, PTT, Icom Transceive push
updates tracking the VFO knob, the REST and WebSocket APIs, the lock lifecycle
including steal and expiry, both its VFOs with split and dual watch, and CW both
ways — the rig's own CAT keyer and locally generated Morse keying DTR and RTS.

Later, its **antenna tuner**: switched in and out, and four tuning cycles run
for real on 80 m, three that found a match and one that could not.

Later still, its **power switch** — off to standby, woken again over the bus
with nobody at the radio — and the whole **receive front end**: both
preamplifiers, the 3 dB attenuator ladder, RF gain, AGC, IP+ and DIGI-SEL with
its shift. That last one turned up an interlock in no manual: with DIGI-SEL
engaged the radio refuses to switch a preamplifier in.

### IC-9700

On 2 m and 70 cm with real antennas. Its CAT surface end to end: Main's two VFOs
addressed without disturbing each other, split, every mode it has including
**DV**, band limits, explicit PTT, leaving memory mode, CW break-in, and CW
keyed and **heard on the air**.

Not a re-run of the whole daemon — locking, the WebSocket and the interlocks are
shared code already exercised on the IC-7610 — but everything CI-V-specific to
that radio.

**A later session finished the job**, over the radio's own USB, and it is now
the most completely exercised radio here after the IC-7610. What that session
added:

- **The power switch, end to end.** Switched off over CAT, correctly reported as
  `standby: true` on a link that stayed up, a command meanwhile answered 503
  naming the remedy, and then **woken over the bus with nobody at the radio** —
  152 `FE` dummy bytes and `18 01`, `FB` back, a few seconds of booting, and the
  session back on the frequency it was left on. **Its USB survives standby**,
  which is the thing that decides whether remote waking is possible at all: the
  CP2102N inside the radio stays enumerated with the rig switched off, so the
  wake has somewhere to go. That is not true of every radio and cannot be
  assumed from this one.
- **The whole receive front end**: the preamplifier, the 10 dB pad, RF gain, all
  three AGC speeds and IP+, with a second preamplifier stage, a 20 dB pad and
  DIGI-SEL correctly refused as things this radio does not have.
- **The whole noise and notch group**: blanker and level, reducer and level, the
  manual notch with its position and all three widths, the automatic notch, the
  exclusivity between the two enforced in both directions, and a request for
  both at once refused.
- **The transmit meters under real RF.** Forward power 48/213, SWR 1.11:1 and
  ALC to 119/120 on 70 cm at 10 % — and absent, not zero, the moment the
  transmission ended.
- **CW both ways, both heard on the air.** The rig's own CAT keyer, and then
  locally generated Morse **keying DTR on the radio's second USB serial port** —
  the first time `serial_key` has run against anything but an IC-7610, and the
  first time it has keyed a port the radio itself provides.
- **Transceive push updates**, 377 of them in twenty seconds, tracking the VFO
  knob in 10 Hz steps between polls.
- Every mode again including **DD**, which needs 1.2 GHz; the filter slots with
  their per-slot widths; transmit power in percent, with watts correctly refused
  on a radio that has no watt scale.

One gap in that session: **nothing was transmitted on 23 cm**, because there is
no antenna for it. Tuning to 1.2 GHz and setting DD there was exercised; keying
it was not.

**2 m could not be tuned either — and that turned into a feature.** The sub
receiver was sitting on 2 m, and the radio will not put Main on a band Sub is
using, so the band was unreachable from the network with nothing in the API able
to move it. `{"exchange_bands": true}` (`07 B0`) is the fix, and it was written,
tested and verified in the same session: 2 m refused, exchanged, 144.300 tuned,
exchanged back, 70 cm restored. The combined form —
`{"exchange_bands": true, "frequency": 144300000}` — brings the band over and
tunes it in one call.

The exchange also made the two-receiver structure visible: after swapping,
`vfo_a` and `vfo_b` read 144.174 and 144.173 where they had read 433.950 and
432.500. Each receiver has its own pair, and remoses's A and B are whichever
receiver is on Main.

**And a second finding, from settling that design, which is now also fixed.**
Touching the sub frequency field on the radio's display selects that side, and
the operating commands follow it — `state.frequency` reports the sub receiver —
while `25`/`26` keep addressing Main, so `state.vfo_a` and `state.vfo_b`
describe the other one. The published state disagreed with itself about which
receiver it was describing, from one touch of the screen, with remoses having
done nothing and nothing it read saying so.

`state.selected_band` now carries it, from `07 D2`, read and never written.

Two things that took hardware to get right. **The transceive stream does not
carry the selection** — the obvious first guess, and a capture of the touch
disproved it: selecting a band broadcasts the ordinary frequency and mode pair,
the new operating frequency with no band tag, which is exactly why
`state.frequency` had always followed correctly while nothing knew why.

And **the read belongs on the fast tier**, which is not where a value only a
finger can change looks like it belongs. On the slow tier the label trailed the
frequency by up to five seconds, and both directions of that were watched on the
radio — a 2 m frequency marked "main", then a 70 cm one marked "sub" — before it
moved. A window of about half a second remains and cannot be closed by polling,
because the frequency is pushed and the selection is asked for.

**Three things the radio does that no manual here mentions**, all found in that
session:

- **The AGC is pinned to FAST in FM** — not unsettable there, which is what
  remoses had recorded from the first session. `fast` is accepted and takes
  effect; `mid` and `slow` draw an NG, and leaving FM restores the speed the
  previous mode had. The earlier reading is what any sequence that never happens
  to ask for `fast` will produce. It matters because a guard on the mode would
  refuse the one speed that works — and remoses does not have one, which is the
  only reason `fast` in FM works today.
- **The preamplifier and the 10 dB attenuator are mutually exclusive**, enforced
  from both sides in silence: setting either one moves the other, and both
  commands are accepted. A client that writes one and assumes the other held
  will be wrong.
- **2 m is refused on the main receiver while the sub receiver is on 2 m.** Main
  and Sub cannot share a band, so a well-formed `05` for 144 MHz draws an NG
  while 70 cm and 23 cm go in. Nothing in the refusal says why, and nothing in
  the protocol reports what band the sub is on — `caps.sub_receiver_readable` is
  false for the same reason.

It also found **one bug**, the same shape as the one an FT-857D found the day
before: a VFO named in a request, accepted, and quietly applied to a different
one. `{"vfo": "sub"}` was applied to VFO B and `{"vfo": "main"}` to VFO A on a
radio whose `caps.vfos` lists neither — so a client asking to tune the second
receiver got a 200 and a *main-band* VFO moved under it instead.

The FT-857D's fix was not enough for it. That radio has no dual-VFO commands at
all, so checking for them caught the case; an IC-9700 *has* them, and the check
passed on a VFO the radio cannot reach. Both receivers on that radio have their
own VFO A and B — the front-panel A/B button switches the Main receiver's pair —
and `25`/`26` reach only Main's, which is exactly what `caps.vfos` says. A named
VFO is now checked against that list as well, and refused whatever the backend
could physically address. Fixed and re-verified on the radio.

### TS-590S

On its built-in USB, the first non-Icom radio to be tried and the first test of
the Kenwood backend against anything but its own unit tests. Frequency, mode,
transmit power, the filter width and both IF filter slots, explicit PTT, CW
break-in in all three states, and CW **heard on the air** at 5 W on 28.030 MHz,
both semi and full break-in.

Later, the **antenna tuner**: switched in and out, and four tuning cycles run
for real on 80 m — three that found a match and one that could not.

Later still, its **receive front end**: preamplifier, attenuator, RF gain and
all three AGC states, then the **noise blanker, noise reduction, both notches
and the antenna selector**.

It found bugs in the process: one that stopped it connecting at all, the
break-in gap again, two in the tuner command, one that made switching the AGC
off a trip with no way back, and one where a request the radio silently ignored
was reported as a success.

### FT-857D

The first Yaesu of either generation and the first radio of the FT-857/FT-897
family — a **completely different CAT protocol** from every other Yaesu, five
binary bytes with no framing of any kind, and until then written entirely from a
manual.

Everything that protocol has: the packed-BCD frequency field across its whole
100 kHz – 470 MHz range, every mode including **WFM read back but correctly
refused as a set**, PTT and the transmit power meter under a **10 W carrier on
80 m and 10 m**, the undocumented acknowledgement byte that the whole framing
design rests on, disconnect and reconnect through a power cycle, and the
interlocks again — band limits, the dead-man `tx_timeout`, and a lapsed lock
dropping PTT.

Its CW could not be tried: this radio has no CAT keyer at all, and the
`serial_key` path needs a second serial port that was not there.

### TS-590SG — the first radio verified through `remoses test-run`

On the bench here, like the others, but driven entirely by the self-test rather
than by hand. So it is two results at once: the radio, and the tool that is
meant to let people who are not here produce the same evidence.

Six runs over ninety minutes, on Linux over a CP2102 at 57600. The last three
are identical step for step: **52 pass, 7 refused, 6 skipped, 6 info, nothing
failed, and the radio put back as it was found.** Frequency, every mode, both
filter slots and the width ladder, transmit power, PTT with the transmit meters,
both VFOs, the receive front end, the noise blankers and reducers and both
notches, the antenna selector, break-in in all three states, a tuning cycle that
found a match on 28 MHz — and **CW confirmed audible on RF by the operator**,
which is the one thing no report can record.

That completes what the TS-590SG needed. Its command set is the TS-590S's, which
was verified here on the air, and a test asserts the two profiles stay identical.

**It found two bugs, and the pair of them is an argument for the whole idea.**

- **A transmit/receive changeover is not instant, and the read-back beat it.**
  Two of the first three runs reported "the radio is still transmitting" after
  the unkey and one did not — decided entirely by whether a poll tick happened
  to land inside the step and read a second time. The session now waits for the
  transmit flag to agree, bounded, before asking. A wrong answer that depends on
  scheduling is worse than a slow one, and this is the wrong answer that matters
  most: a client told the transmitter is still up when it is not, or the
  dead-man path appearing to fail at the one job it exists for.
- **A refused set was reported as a refused read.** A Kenwood rejection is a bare
  "?" that names nothing, and a refused *set* is answered late enough to arrive
  while the read-back is outstanding — so the report said `NR;: rejected`, of a
  plain read that had been answering all session. It cost the first reader of
  that file a wrong turn, which is exactly the cost this project cannot afford
  in a file written by somebody who is not here to be asked.

**And it found three faults in the test tool** — which is the other half of what
this run was for, because every report anybody else ever sends depends on it. Mode steps were named with control characters
rather than mode names — `radio.Mode` is a `uint8`, so `string(m)` yields the
rune with that code point — so every report so far was unreadable exactly where
it was most interesting. The mode sweep left the radio wherever the capability
list happened to end, so break-in and the automatic notch were tested in modes
that cannot have them. And a rejection by the radio was scored as a finding
rather than as the correct answer.

The last of those is worth stating as a rule the tool now follows: **a control
must be exercised in a mode that has it.** Break-in on this radio is set with
`VX`, which addresses VOX in anything but CW; `FW` is not the filter-width
command in SSB at all. Tested in the wrong mode, both produce refusals that
describe the test rather than the radio — and in the first reports, six of them.

### IC-7610 behind Hamlib rigctld

The same radio as the first entry, reached the other way: a Hamlib 4.7 daemon on
the shack LAN, `backend: rigctld`, over TCP. **The first test of that backend
against anything but its own unit tests**, and the point of it is the path
rather than the radio — remoses's own view of an IC-7610 is already known, so
anything that differs here is Hamlib or this backend.

Frequency across three bands, every one of the ten modes Hamlib lists for it
with `DV` correctly refused as one it does not, the filter width, transmit power
in percent, PTT with the transmit meters appearing only while keyed, and **CW
heard on the air at 28.030 MHz** through Hamlib's own keyer at 25% and at full
power — the queue draining in 20-character chunks, and an abort clearing it
mid-message and dropping PTT within a second.

**`swr_ratio` really does arrive calibrated**, which this project had claimed
for a long time without having checked. A continuous 1.03, 1.04, 1.07 at low
power and 1.29, 1.36 at full, where the same radio on its native backend answers
from a four-point table and stops extrapolating past the top one.

Everything this backend does not implement was refused cleanly and named: the
receive front end, the noise and notch group, the tuner, split, the second VFO,
the power switch, filter slots, and power in watts. On an IC-7610 that is most
of the radio, which is the argument for the native backend where one exists.

**It found two bugs in the first minute, both from the same root**: this backend
learns what the rig can do at connect, and remoses assumed a backend could
answer that before dialling.

- **`cw.method: cat` was refused for every rigctld radio.** The daemon checked
  the CW capability at startup, before the first connection, where this
  backend's capabilities are an honest placeholder saying "none". The rig had a
  keyer and said so a second later. All of `morse.go` was unreachable by
  configuration.
- **`limits.max_power_w` was silently no limit at all.** Hamlib reports power as
  a fraction with no watt meaning here, and a watt limit needs a full-scale
  wattage to clamp against, so it could never bind — on the one setting an
  operator writes down specifically to be safe. It now warns at connect and
  names `max_power_pct`.

**One thing that cannot be fixed from here.** Hamlib answers reads from its own
cache — 1000 ms on this daemon — and remoses reads back after a write faster
than that, so the reply to a write can carry the value from before it. Set AM
and the response holds the previous mode's filter width; the next poll agrees
with the radio. remoses could tell the daemon to stop caching, but `rigctld`
serves every client from one rig instance and that is not this process's to
change.

## The interlocks were fired for real

Not against fakes: band limits refusing an out-of-band tune, power clamping, the
dead-man `tx_timeout` forcing receive mid-transmission, and lock expiry cutting
a live CW transmission inside a character.

CW pacing was measured at **61 ms of drift over 18.3 seconds** on a rig whose
buffer cannot be queried, so the timing is dead reckoning.

## Bugs no amount of reading the reference would have found

Values written but never read back, so they reported a stale figure for ever;
setters that quietly changed a neighbouring setting, because on CI-V several
settings share one command; capabilities describing the radio's own keyer while
a different one was installed; a config default that addressed every Icom as an
IC-7610; a poll counter that treated a refusal as silence and reconnect-looped a
healthy radio; a `Mode` that could not decode its own output; a serial port
opened with its control lines already high, which one radio answered by saying
nothing whatsoever; a VFO selection accepted as a success by a radio that cannot
address a VFO at all; a data-mode flag carried forward into modes that have no
data spelling, which left one radio with no way out of its packet mode; a
display drawing `power  0 %` for a radio with no power command, which reads as a
fault rather than as a silence; a request naming one receiver that was applied
to another, because the check that a VFO is addressable is not the same question
as whether it is the VFO that was asked for; and a state whose halves described
two different receivers with nothing published that said which was which.

All of them are fixed, with the measurements and reasoning in
[DESIGN.md](DESIGN.md) §5.4, §5.7, §6 and §11.2.

**Two of them were the same bug found twice**, a day apart, on radios at
opposite ends of the range — a VFO named in a request and quietly applied to a
different one. The first fix was written against a radio with no dual-VFO
commands and did not cover a radio that has them. That is the argument for
connecting the next radio rather than reasoning about it.

**The worst of them was invisible from this side: CW accepted, queued, drained
on schedule — and never transmitted**, because the radio's break-in was off.
Only an operator listening could have caught it — and it happened again, on the
TS-590S, after it had already been found and fixed on the IC-9700, because the
Kenwood backend had no notion of break-in at all.

**Expect the untested models to be hiding something similar.**

## What this still does not tell you

- **What has been verified is two Icoms, two Kenwoods and a Yaesu**, named
  above, plus one of those Icoms through Hamlib. Treat every other model as
  "implemented from documentation, awaiting confirmation". The **ASCII** Yaesu
  profiles — a different protocol from the FT-857D that was tested — the
  remaining Icom profiles and the other Kenwood profiles have never seen a
  radio.
- **No radio has yet been verified by anybody but its maintainer.** The
  TS-590SG was driven entirely by `remoses test-run`, which is the shape a
  report from a stranger would take and is why that command exists — but it was
  still this bench, this cabling and somebody who knows what the answers ought
  to be. The first report from a radio nobody here owns will be the real test of
  it.
- **The rigctld backend has met one Hamlib backend, not Hamlib.** What it can do
  for a given rig is whatever that rig's Hamlib backend implements, and the one
  radio tried through it was an IC-7610 — which is also the radio remoses knows
  best natively, so it was the easiest possible case.
- **CW has never been sent by a Yaesu of any kind.** Every Yaesu keys a control
  line rather than using a CAT keyer, and that path is confirmed on an IC-7610
  and is not Yaesu-specific — but no Yaesu has run it.
- **`limits.bands` gates tuning, not transmitting.** There is no transmit-only
  band limit, so it cannot express "receive anywhere, transmit only here".
- **One failure mode is untested and unfixable here:** pulling the cable *while
  transmitting*. The rig stays keyed and remoses cannot reach it — the link it
  would send the unkey over is the one that vanished. Only the radio's own
  time-out ends that, so set one.
- **A few known rough edges are recorded rather than fixed**, including the CW
  queue position reported after a `replace` and the lock lease not being
  extended by a long transmission. They are in [DESIGN.md](DESIGN.md); none of
  them keys a transmitter.
- Expect to find protocol bugs. The per-model differences are collected in
  [DESIGN.md](DESIGN.md) §5.2–§5.7, which is the place to look first.

**Do not leave it running an unattended station yet.**
