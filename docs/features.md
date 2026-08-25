# Controlling a radio

What remoses can do with a rig once it is connected, and how each control
behaves. Which of these your radio has is a per-model fact: every one is gated
by a capability the API publishes, and the backend pages say which radios have
what — [Icom](icom.md), [Kenwood](kenwood.md), [Yaesu](yaesu.md),
[rigctld](rigctld.md).

**Read `caps` rather than inferring from the model name.** Everything below has
a matching capability field, and a client that checks it will not offer a power
slider to a radio that has no power command.

## Frequency, mode and filters

The ordinary controls: `frequency`, `mode`, `data_mode`, `passband_hz` and
`filter_slot`, all settable in one `PATCH` and applied in a rig-safe order —
**mode first**, because selecting a mode can change the filter and the carrier
offset on many radios and would silently undo a frequency set applied before it.

That ordering is not theoretical. An FT-857D moves its displayed frequency by
the CW pitch offset when you select CW, so `{"mode": "CW"}` alone lands the dial
600 Hz away while `{"frequency": F, "mode": "CW"}` lands on `F`.

**After every write remoses reads back from the radio** and answers with what it
actually did. Rigs clamp power per band, refuse filter slots the current mode
does not have, and round frequencies to their tuning step, all without saying
so.

## VFOs and split

Most CAT protocols only offer "the VFO the radio is on", and remoses controls
that rather than labelling it A or B. Where a radio can address its VFOs by
name, the API exposes both — each with its own frequency, and on some radios its
own mode and filter — plus **split** and, on one radio, **dual watch**.

`caps.vfos`, `caps.split`, `caps.dual_watch` and `caps.per_vfo_mode` say which.
`caps.vfo_addressing` distinguishes `named`, where A and B are stable labels,
from `relative`, where A is whichever VFO the operator has selected and B is the
other — because on some radios nothing in the protocol reports which letter is
which, and remoses will not guess.

**Memory mode is not modelled — but getting out of it is.**
`{"vfo_mode": true}` returns the radio to VFO operation, which is what an
operator stuck on a memory channel actually needs; a rig left there refuses the
per-VFO commands and its readings go stale.

## Exchanging the two receivers

`{"exchange_bands": true}`, gated by `caps.band_exchange`, swaps a radio's two
receivers so that the band the sub one was holding becomes the band everything
else operates.

**It exists for a band you otherwise cannot reach at all.** An IC-9700 refuses
to put its main receiver on a band its sub receiver is using — leave 2 m on the
sub side and a frequency set for 144 MHz is simply rejected — and no command
addresses the sub receiver. Exchanging is the only route in.

Like `vfo_mode` and `tuner_tune` it is an **action**: true only, with nothing to
read back, so `false` is a 422. And it is **applied before everything else in
the same request**, so this works in one call:

```json
{"exchange_bands": true, "frequency": 144300000, "mode": "USB"}
```

**Every field changes at once.** Frequency, mode, both filters and both VFOs now
describe a different band, so remoses re-reads the radio in full before
answering and a client holding state should replace it rather than merge.

Note that the *other* receiver has its own VFO A and B, and after an exchange
`state.vfo_a` and `state.vfo_b` are that pair rather than the one they were.

## Which receiver you are looking at

`state.selected_band` reads `main` or `sub` on a two-receiver radio that reports
it, and it is worth reading because **it says which receiver the operating
fields describe**.

On an IC-9700 the operator can select either side at the radio — touching a
frequency field on the display is enough — and `state.frequency` and
`state.mode` follow that selection. `state.vfo_a` and `state.vfo_b` do not: they
are the *main* receiver's pair either way, because that radio's per-VFO commands
only address Main. So with `selected_band: "sub"` the two halves of the state
are describing two different receivers, which is correct and would be baffling
unlabelled.

The sub receiver has its own VFO A and B too. They are perfectly usable at the
radio, and remoses cannot reach them — the same limitation
`caps.sub_receiver_readable` reports. An exchange swaps both pairs along with
everything else.

**remoses never sets the selection**, only reads it. Moving which receiver an
operator is looking at is not something a daemon should do behind them; where a
band has to be reached, `exchange_bands` is the answer.

The field is absent on a radio with one receiver, or with two that cannot be
asked. It can also trail the frequency by one poll on a radio that pushes
frequency changes, since the push carries the new number but not the fact that
the receiver changed under it.

## PTT and the transmit meters

`{"ptt": true}` keys the radio, where the radio has a command for it. A few do
not — the IC-706 family has no transmitter command at all — and there
`caps.ptt_control` is false and the request returns 422 rather than appearing to
work.

**Forward power, SWR and ALC are polled only while the transmitter is keyed**
and published as `state.power_meter`, `state.swr` and `state.alc`, with
`caps.power_meter`, `caps.swr_meter` and `caps.alc_meter` saying which exist.

**In receive the three fields are absent rather than zero.** A bar drawn at 0
cannot be told from a real reading into a dead load, and a 3:1 SWR left on the
display after a transmission ends reads as a fault that is still happening.

Each meter carries its own `scale`, and a client should compare `raw` against it
rather than assume a range: an Icom's ALC runs to 120 of a possible 255, an
IC-9700's power meter reaches 100% at 213 where an IC-7610's reaches 255, and an
FT-857's meters are four bits.

**SWR is also published as a number** in `state.swr_ratio`, but only where the
radio's own documentation calibrates its meter. Icom prints four points and gets
a figure; Kenwood and Yaesu print none and get only the bar. Above the top
calibrated point remoses stops rather than extrapolating — the curve is
undocumented, an SWR that high is a fault either way, and "7.4:1" would be a
number invented about your antenna. A rigctld radio is the exception: Hamlib
reports a true ratio, so that one arrives calibrated.

## Transmit audio: gain and the speech processor

`state.tx_audio_gain`, `state.proc` and `state.proc_level`, all settable in the same
`PATCH` as everything else, gated by `caps.tx_audio_gain_control`,
`caps.proc_control` and `caps.proc_level_control`.

**`tx_audio_gain` is the gain the radio's own gain command reaches, not the
microphone socket.** Every manufacturer names the control after a microphone,
and on a rig being worked over USB it is very often not the microphone that it
adjusts. Which connector feeds transmit audio — mic, USB, ACC, LAN — is a menu
item on most of these radios and remoses reads it on none of them, so a client
labelling this "USB input gain" is making a promise the protocol does not
support.

**On the newer Yaesus it is sharper than unknown routing: the radio holds
several gains at once.** An FT-710 has MIC GAIN alongside per-mode `USB MOD
GAIN` and `REAR MOD GAIN`; an FT-891 has eight per-mode gains; the FTdx1200 and
FTdx3000 each keep a separate `DATA MIC GAIN`. Which of them `MG` writes is not
stated in any of their manuals. So on an FT-710 running a USB digital mode, this
control may be moving something that is not in circuit — worth knowing before
you reach for it to fix your audio.

A TS-890S and TS-990S are the other side of it: they answer `MS`, a live
selector for the transmission audio route. remoses does not model it yet, so
the caveat applies to every radio — but on those two it is a choice rather than
a limitation.

It is a percentage, for the same reason `rf_gain` is: the counts underneath are
0-255 on Icom and 0-100 on Kenwood, and a number that means a different thing on
two radios is a number nobody can draw.

**The processor's switch is applied before its level** in a request carrying
both, because a radio that refuses the level while the processor is off would
otherwise reject `{"proc": true, "proc_level": 60}` — which is the obvious thing
for a client to send. This is the same ordering the noise blanker needs and for
the same reason.

`proc_level` is reported whether or not the processor is in, where the radio
will answer: the setting is remembered, and a client redrawing the control wants
the value it will return to.

**Nothing here transmits**, but all of it decides what the next over sounds
like. `remoses test-run` moves the gain only a few percent either side of
wherever the operator left it and puts it back as its own recorded step, rather
than sweeping a range and leaving somebody's audio wound up.

## CW

Two ways to send, and which one a radio can use is a fact about the radio rather
than a preference.

**`method: cat`** hands the text to the rig's own keyer buffer. The rig
generates the timing, which is the best possible answer where it exists.

**`method: serial_key`** generates the Morse in remoses and keys a DTR or RTS
line. This is viable only because remoses runs next to the radio: the network is
never in the timing path.

```yaml
cw:
  enabled: true
  method: serial_key
  default_wpm: 25
  serial_key:
    device: /dev/ttyUSB1   # a separate port from CAT is required
    key_line: dtr          # dtr | rts
    ptt_line: rts          # omit for full break-in
    ptt_lead_ms: 15
    ptt_tail_ms: 150
    weight: 50             # dit/dah weighting, %; 20..80
```

The keying port must be a **different device** from the CAT port. Sharing one
works at the OS level, but the session owns its transport privately and redials
it on disconnect, so the keyer would be holding a port that may be closed
underneath it.

**remoses refuses at startup**, not at the first message, if the configuration
names a method the radio does not have — `cw.method: cat` on a radio with no
keyer buffer fails with the fix in the message.

Text is in a rig-neutral canonical form: a prosign is `^` followed by the
letters keyed with no inter-character spacing, such as `^AR` or `^SK`. remoses
translates that into whatever the target rig speaks.

## Break-in, and why it matters more than it sounds

**With break-in off, a CW message sent over CAT is accepted, the rig's buffer
drains on schedule, and nothing is transmitted.** remoses reports a success and
you hear silence. Only an operator listening can catch it, and it has happened
twice in testing on two different manufacturers.

So remoses reads the setting (`state.break_in`, `caps.break_in_control`), lets
you change it (`{"break_in": "semi"}`), and **never lets CW be queued that would
go nowhere**. What it does about it is `cw.break_in`:

- **`semi`** (the default) or **`full`** switch break-in on before sending,
  because a remote operator cannot reach the front panel.
- **`manual`** writes nothing and returns a 422 naming the fix, for a station
  that sequences its own transmit path.

Semi is the default because full is QSK and will clock your relays between
elements — that is a choice to make deliberately.

**Not every radio says which kind of break-in it has.** Some report only off and
on. There `state.break_in` reads `on` rather than guessing, and setting `semi`
or `full` is still accepted; both mean its single "on". Radios whose reference
has not been read for the command are never blocked by the check.

## The antenna tuner

`state.tuner` reads `off`, `on` or `tuning`. `{"tuner": "on"}` switches the
matching network in or out of line, and `{"tuner_tune": true}` runs a tuning
cycle. `caps.tuner_control` and `caps.tuner_tune` say which a radio has.

**Starting a cycle transmits**, so it is gated like any other transmit path: it
needs the lock, it is refused outside `limits.bands`, and it arms the dead-man
timer. That is also why it is a separate field from `tuner` rather than a third
value of it — a client reading the state and writing it back must never key a
radio by echoing `"tuning"` at it.

The call returns as soon as the radio accepts the command; watch `state.tuner`,
which remoses follows at the fast poll rate while a cycle runs.

**The tuner state itself says whether a cycle succeeded**: a match ends on `on`,
a failure ends on `off`. That is in no manual and came from the radios, both of
the two that have been tried.

Note that having a tuner is not the same as having one on the current band — an
IC-9100's covers HF and 50 MHz and the radio rejects a tune on 144 MHz and up.

## The receive front end

`state.preamp`, `state.attenuator_db`, `state.rf_gain` and `state.agc`, plus
Icom's `state.ip_plus` and `state.digi_sel` with its `digi_sel_shift`. Each is
absent where the radio has no such command, and the matching capability —
`preamp_levels`, `attenuator_db`, `rf_gain_control`, `agc_settings`,
`ip_plus_control`, `digi_sel_control`, `digi_sel_shift_control` — says what to
offer.

**The attenuator is in dB, not in steps**, because no two of these radios step
it the same way: an IC-7610 goes 3 dB at a time to 45, an IC-7850 to 21, an
IC-7600 and IC-7700 have 6/12/18, and everything smaller has one fixed pad.
`caps.attenuator_db` lists what this radio has.

**`preamp_levels` counts amplifiers rather than command values.** An IC-9700's
preamp command runs `00` to `03`, but those are an internal and an external
preamp in combination rather than four stages of gain, so it reports one.

Radios enforce interlocks here that no manual mentions — an IC-7610 refuses a
preamplifier while DIGI-SEL is engaged, an IC-9700 refuses any AGC change in FM.
remoses reports the refusal with the reason attached and does **not** switch a
second control off to make your request succeed, because that would be changing
something on a receiver somebody is listening to.

## Noise blankers, noise reduction and notches

`state.noise_blanker` and `state.noise_reduction` with their levels,
`state.notch` with a position and (on Icom) a width, and `state.auto_notch`.

**The two notches cannot both run**, and the radios enforce it without always
saying so. A Kenwood is honest — one selector, off/auto/manual — but an Icom has
them as separate commands that silently switch each other off, which no
reference mentions. `caps.notch_exclusive` says so and a request for both is
refused, rather than applying one and leaving the other to vanish.

**Levels can be refused while their circuit is off**, in as many words in the
Kenwood reference, so a request applies each switch before its level.

## The antenna selector

`state.antenna` and `state.rx_antenna` where the radio has a live selector,
counted from 1, with `caps.antennas` and `caps.rx_antenna_control`.

**On Icom the two fields are one frame**, and it is worth knowing before drawing
them as separate controls: command `12` puts the socket in the sub-command and
that socket's receive-antenna flag in the data byte, so `rx_antenna` is a
property *of the selected socket* rather than an independent switch, and
changing the socket can change it. The IC-7610, IC-7600, IC-7700, IC-7760,
IC-7850 and IC-9100 have it; the IC-7300MK2 has the receive antenna with no
socket to choose.

Those radios *also* keep a per-band antenna **memory** in their Set menu, which
is a different thing and is stored configuration. remoses works the live switch
and leaves the memory alone — so an antenna that changes by itself on a band
change is the memory doing its job, not remoses.

The two capabilities are independent, so read both rather than inferring one
from the other. Kenwood's `AN` and the FTdx101's selector are offered too.

## Switching the radio itself off and on

`{"power_switch": "off"}` and `"on"`, gated by `caps.power_switch`. It must be
the only field in the request: a patch that switched the radio off and set a
frequency has no sensible ordering, since one of the two is always addressed to
a radio that is not listening.

**`off` is the recoverable off.** Where a radio offers a choice — a Kenwood does
— the default draws more standby current and wakes on a single command, while
`off_deep` matches the front-panel switch and needs a longer wake sequence. A
remote station that cannot be woken is one somebody has to drive to, so the
deeper sleep is opt-in.

**Waking works with no link, which is the state it is for.** remoses arms the
wake and sends it on its next connection attempt, on the freshly opened port
before it tries to talk to the radio — the one moment a sleeping rig can be
reached. Expect several seconds of booting; watch `connected`.

**Whether a wake works at all is a wiring question, not a command one.** An
external CI-V interface stays powered and listening. A radio whose CAT arrives
over its own USB may take the USB device down with it and leave nothing to send
the wake-up to, so try it with somebody near the radio before relying on it.

**A radio that is switched off is reachable, and remoses says so** — on the
radios that answer at all. `state.standby` is a third condition published
alongside `connected: true`, and `remoses-cli` shows `STANDBY` in place of
`CONNECTED`. It is set however the radio came to be off, a `power_switch`
request or the front panel button, because the detection is simply that the
radio answers every command with a rejection. The session holds the port open
and watches for it to wake rather than dropping a link that is perfectly
healthy, and commands meanwhile answer 503 with the remedy in the message.

**Some radios go silent instead**, an FT-857 among them: nothing answers, and
the USB adapter stays present. There is no standby to detect and remoses reports
`connected: false`, which is the honest answer — and it means remote waking is
not available on such a radio in any form.

## Safety interlocks

Four, and all four have been fired against real hardware rather than fakes:

- **Band limits** refuse a tune outside `limits.bands`.
- **Power clamping** applies `limits.max_power_*` server-side, and the response
  says what the radio was actually set to.
- **The dead-man `tx_timeout`** forces receive regardless of what the client is
  doing.
- **Lock expiry drops PTT and flushes the CW queue.** A client that dies
  mid-over does not leave a carrier up.

**One failure mode is untested and unfixable here:** pulling the cable *while
transmitting*. The rig stays keyed and remoses cannot reach it, because the link
it would send the unkey over is the one that vanished. Only the radio's own
time-out ends that, so set one.
