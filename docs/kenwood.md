# Kenwood radios — the `kenwood` backend

The TS-480 to the TS-990S, plus a generic profile.

A **TS-590S** has been verified on the air. The other profiles are implemented
from Kenwood's own documentation and have never met a radio. See
[hardware status](hardware-status.md).

The same ASCII dialect is spoken by several Elecraft and modern Yaesu radios,
which is what `generic` is for — with one deliberate gap, noted under break-in
below.

## Configuring one

```yaml
radios:
  - id: ts590sg
    name: "Kenwood TS-590SG"
    backend: kenwood
    port:
      device: /dev/ttyUSB0
      baud: 115200
    kenwood:
      model: ts590sg
      auto_information: 2
      bulk_poll: true
    poll:
      interval: 500ms
      slow_interval: 5s
    cw:
      enabled: true
      method: cat
      default_wpm: 25
      break_in: semi
    limits:
      max_power_w: 80
      tx_timeout: 120s
```

### Name your model

It matters here, and **naming the wrong one does not fail loudly.** The TS-890S
and TS-990S are a noticeably different dialect from the rest: `OM` where the
others use `MD`, data mode folded into the mode code, and no `IF;` bulk status —
and with it no way to poll PTT at all, so on those two it arrives only through
auto-information pushes.

The break-in command differs across all five as well; see below.

### `auto_information` — unsolicited updates

`AI2` pushes state changes as they happen, so a front-panel change appears
without waiting for a poll. It **reverts to off at rig power-down**, so remoses
does not permanently alter the operator's menu settings. `0` disables it.

### `bulk_poll`

`IF;` answers with one 38-character reply covering frequency, RIT/XIT, TX/RX,
mode and split — one exchange instead of several. The poller falls back to
discrete queries in Data mode, where `IF;` does not answer.

Not available on the TS-890S and TS-990S, which have no `IF;`.

### If the radio answers nothing at all

**A TS-590S needs its control lines raised, not merely set high.** Opening the
port with DTR and RTS already asserted produced a radio that answered nothing
whatsoever — correct speed, correct device, well-formed frames going out, not
one byte coming back.

remoses now opens **every** port with both lines low and drives them to their
configured state immediately afterwards, so a port configured `high` gets a
low-to-high transition. The defaults are right for a CAT port; see
[`port.dtr` / `port.rts`](configuration.md#a-local-serial-port).

## Supported models

| Model | `kenwood.model` | Notes | Tested |
|---|---|---|---|
| TS-480 | `ts480` | No DATA mode, no filter selection; break-in on `VX` is **inferred**, see below | — |
| TS-590S | `ts590s` | Frequency, modes, filters, power, PTT, break-in and CW exercised on the air | **yes** |
| TS-590SG | `ts590sg` | Same command set as the S; the two differ only in `ID` | — |
| TS-890S | `ts890s` | PTT cannot be polled, only pushed | — |
| TS-990S | `ts990s` | 200 W; PTT cannot be polled, only pushed; **`FA`/`FB` are Main/Sub bands, not VFOs** | — |
| other Kenwood | `generic` | TS-590 shape, but no break-in — see below | — |

**The TS-590SG shares the S's command set entirely.** Their reference is one
document, which marks every command remoses uses "[TS-590S / TS-590SG common]";
the two differ only in the `ID` they answer with, so everything verified on the
S holds for the SG. A test asserts the two profiles stay identical, because a
fix applied to one and not the other would stay invisible until somebody put an
SG on the air.

## Both VFOs, and split

`FA` and `FB` read and set VFO A and VFO B directly without selecting either, so
a client can park a frequency on B while the operator works A. `FR` and `FT`
then say which is received and which transmitted, and **that relationship is
split** on this protocol — there is no split flag to write.

What the family does not offer is a per-VFO **mode**: `MD` applies to whichever
VFO is selected and nothing addresses the other one's, so `caps.per_vfo_mode` is
false and `state.vfo_a` / `state.vfo_b` carry frequencies only. Reaching the
other VFO's mode would mean selecting it, sending `MD` and selecting back —
moving the operator's radio under them, and leaving it wrong if the sequence
failed halfway.

**Except on the TS-990S, where `FA` and `FB` are not VFOs at all.** Its reference
names them "Main Band Frequency" and "Sub Band Frequency": two receivers, each
with its own VFO A and B underneath. That is the same opcode pointing at a
different axis, so remoses reports `caps.vfos: ["current"]` there and refuses to
address A or B rather than calling that radio's Sub band "VFO B".

## Break-in, and the VOX trap

**Break-in is spelled four different ways across these five radios**, and it is
not cosmetic: with break-in off, a `KY` message is accepted, the rig's buffer
drains on schedule and **nothing is transmitted**.

- **TS-990S** — `BI` takes off, semi and full directly.
- **TS-890S** — `BI` has only off and on, and the `SD` delay decides which kind:
  0 ms *is* full break-in.
- **TS-590S and SG** — no break-in command at all. They use `VX`, which sets
  VOX, "except that when transmitting the VX command in CW mode, the Break-in
  function is set and read, rather than the VOX function". One command with two
  meanings, chosen by the mode the radio happens to be in — so remoses only
  touches it in CW, and refuses a break-in request in any other mode rather than
  switching voice VOX on behind you.
- **TS-480** — treated like the TS-590, and **that one is an inference.** Its
  reference documents `VX` as VOX and says nothing about CW. But it has `SD`,
  the CW break-in delay, so break-in exists on the radio; it has no `BI`; and
  across this family those two facts move together. If an operator reports VOX
  switching on when they send CW, this is the assumption that was wrong.
- **`generic`** gets **no** break-in, and that is a refusal rather than a gap.
  The inference above is one about Kenwood; it says nothing about the Elecrafts
  and modern Yaesus that also speak this dialect, and break-in is the one
  command in the profile that *writes* on the strength of a guess. Being wrong
  there leaves VOX switched on — invisibly, since remoses only writes `VX` in
  CW, so it surfaces later when the operator moves to SSB and the radio starts
  keying on room noise. **Name your model to get the check.**

## The receive front end

It **differs by generation more than anything else here**, and three of the
differences would put a syntax error on the wire rather than a wrong value:

- `PA` takes one digit and answers two — the second is documented "always 0".
- `RA` takes two and answers four on the TS-480 and TS-590, one and one on the
  TS-890S, and a band selector plus a digit on the TS-990S.
- **`RG` counts to 100 on a TS-480 and to 255 on everything after it**, the same
  knob on two scales a factor of two and a half apart. That is why the API
  publishes a percentage and each model states its own ceiling.

**The AGC moved commands.** A TS-480 keeps the speed on `GT` (`000` off, `001`
fast, `002` slow); every radio since keeps a *time constant* there and the speed
on `GC`. Sending one radio's form to the other sets the wrong thing entirely.
The speeds differ too: a TS-590 has off, slow and fast with no middle, a TS-890S
and TS-990S have all three.

**remoses does not read the AGC in FM on any of them.** Every reference carries
the same note — "this command cannot be performed in FM mode (an error sounds)"
— and the error sounds *at the radio*, so polling anyway would beep at whoever
is listening once per slow tick.

## Noise blankers and reduction

**They are counts, not switches.** This family has NB1 and NB2, NR1 and NR2, and
they are different circuits rather than two strengths of one. NR2 is SPAC, whose
level is a *following speed* of 2 ms to 20 ms where NR1's is an effective level
of 01 to 10 — so `nr_level` is a percentage of whichever range is running.

Both radios can also run both blankers at once and answer `NB3` for it; that is
a combination rather than a third blanker, so it is not published.

**The levels are refused while their circuit is off** — "an error occurs", in as
many words — so `NL` and `RL` are only asked for once the radio has reported the
blanker or reducer on, and a request applies each switch before its level.

---

# Protocol notes

Everything below is for somebody reading the code or debugging a frame.
[DESIGN.md §5.5](DESIGN.md) has the full per-model reference.

## Facts that came from the radio, not the manual

A TS-590S found a batch of bugs on its first session. These were the ones that
are things the radio does and its reference does not describe:

- **Switching the AGC off is a one-way trip unless you know the trick.** With
  the AGC off, `GC1` and `GC2` are *both* refused and the radio stays off. A
  client that switched it off could never switch it back and would be told only
  "command rejected". The manual documents the parameter that gets back out —
  "3: AGC Off → On (AGC returns to its Slow/Fast status before turning Off)" —
  as one option among four; that the other two are *refused* from off, making it
  the only door, is not in there and came from the radio. remoses sends it first
  when a speed is asked for while the AGC is off, and only then. The TS-480 has
  no such value documented and gets none of this.
- **A TS-590S in CW ignores a request for the automatic notch.** No error, no
  change: `NT10` is accepted and a read still answers `NT20`. The reason is
  sound — the automatic notch hunts for tones, and in CW the tone is the signal
  you are listening to — but nothing in the exchange says the request did not
  happen. remoses checks the read-back and answers 422 naming the mode, rather
  than reporting a success that did not occur.
- **The tuner refuses two things its reference does not mention.** An on/off set
  must send `P1` as `0`, and **a set that changes nothing is refused as well** —
  so remoses reads before writing, or the same request sent twice would fail the
  second time.

A TS-480 also **answers three spaces instead of refusing** an AGC read in FM,
which is decoded as "no reading" rather than as a frame to complain about.
