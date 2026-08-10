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
exercised once, on the IC-7610, and are shared code; the IC-9700 and the TS-590S
confirmed the protocol surface and CW, not those again. The FT-857D did re-run
reconnect and the interlocks, because its CAT set has no keyer to spend the
session on and because it fails in a way the Icoms do not.

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

## Fourteen bugs no amount of reading the reference would have found

Values written but never read back, so they reported a stale figure for ever;
setters that quietly changed a neighbouring setting, because on CI-V several
settings share one command; capabilities describing the radio's own keyer while
a different one was installed; a config default that addressed every Icom as an
IC-7610; a poll counter that treated a refusal as silence and reconnect-looped a
healthy radio; a `Mode` that could not decode its own output; a serial port
opened with its control lines already high, which one radio answered by saying
nothing whatsoever; a VFO selection accepted as a success by a radio that cannot
address a VFO at all; a data-mode flag carried forward into modes that have no
data spelling, which left one radio with no way out of its packet mode; and a
display drawing `power  0 %` for a radio with no power command, which reads as a
fault rather than as a silence.

All fourteen are fixed, with the measurements and reasoning in
[DESIGN.md](DESIGN.md) §5.4, §5.7, §6 and §11.2.

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
  queue position reported after a `replace` and the lock lease not being
  extended by a long transmission. They are in [DESIGN.md](DESIGN.md); none of
  them keys a transmitter.
- Expect to find protocol bugs. The per-model differences are collected in
  [DESIGN.md](DESIGN.md) §5.2–§5.7, which is the place to look first.

**Do not leave it running an unattended station yet.**
