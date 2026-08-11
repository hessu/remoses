# Hardware status

**Four radios out of the thirty-seven remoses supports have ever been connected
— but all four have been put through everything they can do.**

Every protocol detail in this project was transcribed from the manufacturers'
own CI-V and CAT reference documentation and is exercised against simulated rigs
in the test suite. That proves the code matches what the manual says. It does
not prove the manual is right, that the transcription is right, or that a
particular radio behaves the way its documentation claims — which is why the
four radios that have been on the wire matter more than the thirty-three that
have not.

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

## The four

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

**One thing that is still not right, found while settling that design.**
Touching the sub frequency field on the radio's own display selects that side,
and the operating commands follow it — `state.frequency` reports the sub
receiver — while `25`/`26` keep addressing Main, so `state.vfo_a` and
`state.vfo_b` describe the other one. The published state then disagrees with
itself about which receiver it is describing, from one touch of the screen and
with remoses having done nothing. `07 D2` reports which band is selected and is
not polled. Recorded rather than fixed.

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

It found six bugs in the process: one that stopped it connecting at all, the
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

## The interlocks were fired for real

Not against fakes: band limits refusing an out-of-band tune, power clamping, the
dead-man `tx_timeout` forcing receive mid-transmission, and lock expiry cutting
a live CW transmission inside a character.

CW pacing was measured at **61 ms of drift over 18.3 seconds** on a rig whose
buffer cannot be queried, so the timing is dead reckoning.

## Fifteen bugs no amount of reading the reference would have found

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
fault rather than as a silence; and a request naming one receiver that was
applied to another, because the check that a VFO is addressable is not the same
question as whether it is the VFO that was asked for.

All fifteen are fixed, with the measurements and reasoning in
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

**Expect the other thirty-three models to be hiding something similar.**

## What this still does not tell you

- **Four radios have been verified**, two Icoms, a Kenwood and a Yaesu. Treat
  every other model as "implemented from documentation, awaiting confirmation".
  The twelve **ASCII** Yaesu profiles — a different protocol from the FT-857D
  that was tested — the remaining fourteen Icom profiles and the other four
  Kenwood profiles have never seen a radio, and neither has the rigctld backend.
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
  queue position reported after a `replace`, the lock lease not being extended
  by a long transmission, and an IC-9700 whose published state splits across two
  receivers when the operator selects the sub band at the radio. They are in
  [DESIGN.md](DESIGN.md) and on the [Icom page](icom.md); none of them keys a
  transmitter.
- Expect to find protocol bugs. The per-model differences are collected in
  [DESIGN.md](DESIGN.md) §5.2–§5.7, which is the place to look first.

**Do not leave it running an unattended station yet.**
