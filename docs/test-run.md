# `remoses test-run` — check a radio and file a report

remoses carries profiles for radios nobody here can plug in, transcribed from
manufacturers' references. **Every radio that has actually been connected found
bugs** — values written but never read back, commands that silently changed a
neighbouring setting, capabilities describing a different radio — and none of
them were visible from the documentation.

The untested profiles are not better than the tested ones were. They are only
unexamined. If you have one of these radios, this is how you can find out, in
about a minute, and send back something worth acting on.

```sh
remoses test-run -config remoses.yaml
```

It exercises everything your radio says it can do, puts the radio back as it
found it, and writes a file.

**Send the file to <remoses-logs@he.fi>.** That is the whole point of it: a
report from a radio nobody here owns is worth more than any amount of
re-reading the manufacturer's reference.

Two things to put in the message, because the file cannot know them:

- **whether the CW was audible**, if you ran the transmit tests — "accepted,
  queued, drained on schedule, and never transmitted" is the worst bug this
  project has met, twice, and only a listener catches it;
- **whether anything sounded wrong** at the radio — relays chattering, the rig
  muting, a tuner hunting when it should not have.

## It does not transmit unless you say so

With no `-tx-freq`, the run never keys the transmitter. It reads, sets receive
controls, and puts them back.

```sh
remoses test-run -tx-freq 28030000 -cw-text "TEST DE N0CALL"
```

Give it a frequency and it will additionally key the radio briefly, check the
transmit meters, send a short CW message, and — where the radio has an antenna
tuner — run one tuning cycle. **Pick a frequency you are licensed and equipped
to transmit on.** Power is held low (`-tx-power-pct`, default 10) and every
transmission is short.

**Put your own callsign in `-cw-text`.** `N0CALL` here is a placeholder that
happens to be reserved for exactly this purpose; transmitting it identifies
nobody, and transmitting somebody else's identifies the wrong station.

Two checks are deliberately skipped without `-tx-freq`, and the report says so:
the ones that would key the transmitter *if a capability turned out to be
wrong*. Wrong capabilities are exactly what this looks for, so it will not
gamble a carrier on one being right.

## The power switch is separate

```sh
remoses test-run -test-power-switch
```

Off by default. It switches the radio off over CAT and wakes it again — and
**whether a wake works is a wiring question, not a command one**. A radio whose
CAT arrives over its own USB may take the USB down with it and leave nothing to
send the wake-up to. Use this only if you can reach the radio.

## It puts the radio back

Frequency, mode, filter, power and the receive controls are captured before
anything is written and restored at the end — including when the run fails
partway, and when you interrupt it with Ctrl-C. The report says whether the
restore worked, and the tool says so loudly on the terminal if it did not.

It is still somebody's radio. Run it when you are at the station, not from the
other side of the country.

## What comes out

JSON Lines: one self-contained JSON object per line.

- **A header** — remoses version, backend, configured model, how the radio is
  reached, the full capability set, and the state as found.
- **One line per step** — what was requested, what was expected, what came
  back, a verdict, and **the CAT frames that went over the wire for that step**.
- **A summary** — the counts, the failures by name, and whether the radio was
  restored.

The wire trace is the part that makes a report worth having. Decoded state hides
exactly the mistakes worth finding — a mode table read off the wrong column, a
frequency field a digit short, a command the radio does not really implement —
and every one of those is obvious in the hex.

A full run is a few tens of kilobytes — small enough to attach to a mail to
<remoses-logs@he.fi>. Nothing in it is a secret: CAT carries frequencies and
mode codes, never credentials, and the file contains no part of your
configuration beyond the radio's own description. It is worth a look before you
send it, and it should confirm that.

Add `-notes "IC-7300, CT-17 interface, Hamlib not involved"` and it goes in the
header.

## Reading a report

The verdicts are deliberately conservative, because a report that cries wolf
gets ignored and most people will only ever run this once.

| Verdict | Meaning |
|---|---|
| `pass` | Did what was asked, and the read-back agrees. |
| `fail` | Unambiguous. Used only for: a write the radio accepted that changed nothing; a control the radio *denies* that was accepted anyway; a control it advertises that could not be exercised at all. |
| `refused` | The request was rejected, and rejection was the right answer. |
| `skipped` | No such control, or the run was not authorised to try it. |
| `info` | It happened and is written down; nothing claims it was right. |

**`info` is not "fine".** A radio that landed somewhere other than where it was
asked, a transmit meter that stayed at zero, a mode that came back as a
different mode — all of those are `info`, because each has an innocent
explanation and a guilty one, and the wire trace beside it is what tells them
apart. If you are diagnosing a report, read the `info` lines.

Two things the file can never tell you, and which are worth asking the operator:

- **Whether the CW was audible.** The run can prove the radio keyed and the
  queue drained. Only somebody listening can say a signal went out — and "CW
  accepted, queued, drained on schedule, and never transmitted" is the worst bug
  this project has met, twice, on two manufacturers.
- **Whether anything sounded wrong.** Relays chattering, the rig muting, an
  antenna tuner hunting when it should not.

## What it does not cover

- **Reconnect.** Pulling the cable and plugging it back in is not something a
  program can do to itself.
- **The safety interlocks.** Band limits, the dead-man `tx_timeout` and lock
  expiry are configuration-dependent and would mean transmitting for as long as
  the timer, so they stay a job for somebody who owns the radio.
- **Anything above the session.** The HTTP API, the WebSocket and the locking
  are shared code, exercised elsewhere; this drives the radio through the same
  session the daemon uses and stops there.
