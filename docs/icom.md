# Icom radios — the `civ` backend

Sixteen models over CI-V, from the IC-703 to the IC-9700, plus a generic
profile.

An **IC-7610** and an **IC-9700** have been verified on the air; the other
fifteen profiles are implemented from Icom's own documentation and have never
met a radio. See [hardware status](hardware-status.md).

## Configuring one

```yaml
radios:
  - id: ic7610
    name: "Icom IC-7610"
    backend: civ
    port:
      device: /dev/tty.usbmodem14201
      baud: 115200
    civ:
      model: ic-7610
      echo: false
      transceive: true
    poll:
      interval: 500ms
      slow_interval: 5s
    cw:
      enabled: true
      method: cat
      default_wpm: 28
      break_in: semi
    limits:
      max_power_pct: 80
      tx_timeout: 120s
```

### Name your model

`civ.model` is the one setting worth getting right, because **naming the wrong
model does not fail loudly — it changes the wrong setting.** It picks the
default bus address, the modes remoses will offer, and, more importantly, what
remoses will not attempt:

- An **IC-718** has no CAT CW buffer, no filter-width command, and keys PTT on a
  different sub-command from the rest of the family.
- An **IC-910H** puts FM on a different mode code and uses the data-mode
  sub-command for RIT.
- An **IC-703** uses the command that is filter width everywhere else for its
  Set-mode menu, so a filter-width read there would be asking the radio about
  its beeper.
- An **IC-706** of any generation cannot be keyed or have its power set over
  CI-V at all, and numbers its IF filters from zero rather than one.

remoses cannot ask the radio reliably. CI-V command `19 00` answers with the
rig's bus address, which is menu-configurable and shared between models, so it
is used only to **warn** when the answer disagrees with what you configured.

### `rig_address`

Optional. It defaults to the model's factory address, and you only need to set
it if the address was changed in the radio's menu. It is **required** for
`model: generic`, which has no factory default to fall back on.

### `echo` — only for the 13-pin bus

```yaml
civ:
  echo: true
```

Set this **only** when wired to the 13-pin CI-V bus jack, which echoes back
everything remoses sends. A radio reached over its own USB does not echo.

### `transceive` — unsolicited updates

With Transceive enabled in the radio's menu, front-panel changes arrive without
polling: turning the VFO knob produces a stream of broadcasts that remoses folds
into its state cache, so a client watching the WebSocket tracks the knob.

This is a menu setting on the radio. The configuration flag tells remoses to
expect the traffic.

## Supported models

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
| IC-9700 | `ic-9700` | Main's two VFOs and split; sub band present but not readable; no tuner; power switch, front end and CW both ways exercised on the air | **yes** |
| other Icom | `generic` | Requires an explicit `civ.rig_address` | — |

## What your radio can and cannot do

### The IC-706 family cannot be keyed over CI-V at all

None of the three has a transmitter command, so remoses can neither key them nor
tell whether they are keyed, and none has an RF power level either. Both are
front-panel controls, and PTT is a footswitch, the microphone, or a control
line.

`caps.ptt_control` and `caps.power_control` say so, `{"ptt": …}` and
`{"power_w": …}` return 422, and neither is polled. These are the first radios
here to report either as false, so a client that assumed every rig can be keyed
will want to check them.

### CW needs the rig's keyer, or a control line

The IC-703, IC-706 family, IC-718 and IC-910H have **no CAT CW buffer**. Use
`cw.method: serial_key` on those; `cat` is refused at startup with the fix in
the message. Everything else in the table has command `17`, a 30-character
buffer, and takes `cw.method: cat`.

### Break-in is not optional

On an Icom a CW message sent over CAT is transmitted **only** if break-in is on
or the transmitter is already keyed. Otherwise the rig accepts the message,
empties its buffer on schedule and puts nothing on the air. See
[break-in](features.md#break-in-and-why-it-matters-more-than-it-sounds).

An **IC-910H** reports break-in as off/on only, where the rest of the family has
off, semi and full.

### Which Icoms have an antenna tuner

Transcribed per model rather than assumed. The IC-703, IC-7300, IC-7300MK2,
IC-7600, IC-7610, IC-7700, IC-7760, IC-7850 and IC-9100 have one. The IC-905,
IC-910H and IC-9700 have no such row in their references, and neither does the
IC-706 family.

**The IC-718 is the sharp edge.** The command that is the tuner on every other
Icom is *PTT* on that radio — so a "start tuning" sent to one would key the
transmitter and hold it keyed. It reports `tuner_control: false`, and must.

### No Icom gets an antenna selector

They have no live one. On an IC-7610 the antenna is a per-band *memory*, one
entry per band range, so switching it means writing a stored setting rather than
throwing a switch. `caps.antennas` is 0.

## Both VFOs, split and dual watch

**The IC-7610 is the only radio here whose second VFO remoses can reach.** Its
commands `25` and `26` name a VFO in the frame, so both can be read and set
without selecting either. So on an IC-7610 the API exposes both VFOs — each with
its own frequency, mode, data mode and filter — plus **split** (transmit on the
other VFO) and **dual watch** (receive on both at once, with the second
receiver's S-meter streaming while it runs).

**The IC-9700 also has two addressable VFOs, but they mean something else.** Its
`25` and `26` select the *selected and unselected VFO of the Main band*, where
the IC-7610's select the main and sub bands — one opcode, two axes. So on an
IC-9700 remoses exposes Main's two VFOs and split, and `caps.vfo_addressing`
reads `relative`: A is whichever VFO the operator has selected and B is the
other, because nothing in that radio's protocol reports which letter is which.
On an IC-7610 it reads `named` and the labels are stable.

**The IC-9700's sub band is deliberately left alone.** The radio has one and it
receives independently — with **its own VFO A and B**, as Main has; the
front-panel A/B button switches Main's pair. But no command addresses the sub
receiver: the only route is "select the sub band", which would move the
operator's focus and fight whoever is holding the dial. So `caps.sub_receiver`
is true, `caps.sub_receiver_readable` is false, and remoses never switches bands
to read a meter.

The A and B in `caps.vfos` are therefore **Main's two**, and a request naming
`sub` is refused rather than quietly landing on one of them — which is what it
used to do, and is the bug that radio found.

**It also will not put Main on a band the sub receiver is using.** With the sub
on 2 m, a perfectly well-formed frequency set for 144 MHz draws a rejection
while 70 cm and 23 cm are accepted. Nothing in the refusal says why, and nothing
in the protocol reports which band the sub is on, so remoses can only pass the
radio's "no" back.

**Which is what `{"exchange_bands": true}` is for** — `07 B0`, and the only way
to work a band that is sitting on the sub receiver. See
[exchanging the two receivers](features.md#exchanging-the-two-receivers). It is
offered on the IC-9700 alone, because that is the radio whose reference has been
read for the command and the one that needs it.

**One state to be aware of, which remoses cannot currently see.** Touching the
sub frequency field on the radio's own display *selects* that side, and the
operating commands follow it: `state.frequency` and `state.mode` then report the
sub receiver. But `25`/`26` keep addressing Main, so `state.vfo_a` and
`state.vfo_b` go on describing the other receiver. The two halves of the state
disagree about which receiver they are talking about, and nothing remoses reads
today says so — `07 D2` would report it and is not polled. Verified on the radio
by touching the display and watching `frequency` jump to 144.174 while the VFO
pair stayed on 70 cm. Exchanging is unaffected: it leaves the focus where it is.

Other Icoms very likely have `25` and `26` too. They stay off until each radio's
own reference has been read, which is the rule the rest of this page follows.

---

# Protocol notes

Everything below is for somebody reading the code or debugging a frame.
[DESIGN.md §5.4](DESIGN.md) has the full per-model reference.

## The commands remoses uses

| What | Command |
|---|---|
| Preamplifier | `16 02` |
| Attenuator | `11` |
| RF gain | `14 02` |
| AGC | `16 12` |
| IP+ | `16 65` |
| DIGI-SEL, and its shift | `16 4E`, `14 13` |
| Noise blanker, and its level | `16 22`, `14 12` |
| Noise reduction, and its level | `16 40`, `14 06` |
| Manual notch, position, width | `16 48`, `14 0D`, `16 57` |
| Automatic notch | `16 41` |
| Forward power / SWR / ALC | `15 11` / `15 12` / `15 13` |
| Antenna tuner | `1C 01` |
| Power switch | `18 00` / `18 01` |
| Addressed VFOs | `25`, `26` |

## Two per-model traps

**`11` is the attenuator depth in BCD on every Icom but the IC-718**, whose own
table says `01` means 20 dB where the IC-703's says `20` does. Same opcode, same
pad, different byte, and nothing in the frame to tell them apart.

**`16 12` has five spellings that share only the opcode.** The IC-7610 counts
`01` FAST, `02` MID, `03` SLOW; the IC-7600 counts the same three from `00`; the
IC-7700 has four, `00` being AGC OFF; the IC-703 has two, `1` fast and `2` slow;
and the IC-910H has two the other way round, `0` slow and `1` fast. One byte out
sets a different speed and looks like a success.

## Facts that came from the radios, not the manuals

- **With DIGI-SEL engaged an IC-7610 refuses to switch a preamplifier in.**
  `16 02 01` draws a bare NG while `16 02 00` is accepted and the read works
  throughout, so the refusal says nothing about why. remoses adds the reason to
  the 422 and does not switch the preselector off to make the request succeed.
  The radio enforces it from the other side too: switching DIGI-SEL in while a
  preamplifier is selected switches that preamplifier out.
- **An IC-9700 pins the AGC to fast in FM.** All three speeds go in under USB;
  in FM `fast` is *accepted* and takes effect while `mid` and `slow` are
  refused, and a read answers throughout — so the state looks healthy and only
  the refusal says otherwise. Leaving FM restores the speed the previous mode
  had. remoses adds the reason and still sends the command, rather than fencing
  off a mode on the strength of one radio, which is exactly why the one speed
  that works is not refused along with the two that do not.
- **An IC-9700's preamplifier and 10 dB attenuator are mutually exclusive**, and
  the radio enforces it from both sides without saying so: switching the pad in
  switches the preamplifier out, switching the pad out puts it back, and
  switching the preamplifier in takes the pad out. Both commands are accepted —
  it is the *other* control that moves — so a client that writes one and assumes
  the other held its value will be wrong. remoses re-reads both after any
  front-end write.
- **An IC-9700 refuses 2 m on the main receiver while its sub receiver is on
  2 m.** Main and Sub cannot share a band, so a perfectly well-formed `05` for
  144 MHz draws an NG while 70 cm and 23 cm are accepted. Nothing in the frame
  or the refusal says which band the sub is on.
- **The two notches silently switch each other off.** Separate commands on an
  Icom, with nothing in either reference about the interaction. Verified in both
  directions.
- **An IC-9700 in FM refuses the notch width**, which is correct — FM has no use
  for a DSP notch.
- **The IC-706 and MKII answer "read frequency" and "read mode"** even though
  neither appears in their command tables, and the MKII's factory address is
  `4E` rather than the `48` its data-format diagram shows. The first matters
  because remoses fills its state by polling, so a radio it could command but
  never read would never connect; the second is what `civ.rig_address` is for.
- **An IC-7610 in standby does not go silent, it refuses everything.** That is
  what `state.standby` detects, and getting it wrong once left remoses reporting
  a connected radio over a snapshot going stale by the minute.
