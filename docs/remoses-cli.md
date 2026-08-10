# remoses-cli

A **read-only** terminal monitor for one radio. It fetches the current state
over REST so there is something on screen at once, then follows the WebSocket
stream.

It never issues a `PATCH`, `POST` or `DELETE` and **never takes a lock** — a
monitor that took the lock would lock out the operator actually working the
radio.

```sh
remoses-cli ic7610                       # watch the local instance
remoses-cli -url https://radio.example.net ic7610
remoses-cli -once ic7610                 # print the state once and exit
remoses-cli ic7610 | tee ic7610.log      # timestamped lines instead of a redraw
```

## The display

```
remoses  ic7610 - IC-7610 - civ                                      CONNECTED
------------------------------------------------------------------------------

  14.025.000 MHz    CW                                                PTT   RX

  S   ██████████████████░░  S9+21 dB  230/255

  passband  500 Hz   filter 2                                power  40 %  40 W
  cw        idle  queued 0  28 wpm                               lock   oh2abc

  seq 4471   updated 0.0 s ago                                    stream  live
```

**The meter block swaps while the radio is transmitting**, the way the radio's
own meter does:

```
  14.025.000 MHz    CW                                          PTT   >> TX <<

  PWR ███████████▎░░░░░░░░  143/255  56 %
  SWR ██████▍░░░░░░░░░░░░░  1.4:1
  ALC ████████████░░░░░░░░  72/120  60 %

  passband  500 Hz   filter 2                                power  40 %  40 W
  cw        sending  queued 14  28 wpm  ~4.3 s                   lock   oh2abc
```

They swap rather than stack because during a transmission the S reading is not
merely uninteresting, it is wrong: on a Kenwood the command that reports it is
reporting the power meter instead, so what is left is whatever the last receive
poll saw.

## Nothing is drawn for a radio that cannot report it

**Only the meters the radio actually reports get a line** — an FT-857 has power
and a high-SWR bit and no ALC, so it gets two lines rather than three.

**The same applies to the row above them.** `passband`, `filter` and `power` are
drawn only where the radio has the command behind each, and the row goes
entirely when it has none of the three — as on an FT-857, whose CAT set reads
neither a bandwidth nor an output power.

A zero there would not be a small reading, it would be the absence of one, and
`power  0 %` beside a radio putting out ten watts reads as a fault.

## Piped output

On a terminal the display is redrawn in place. Anywhere else it becomes one
timestamped `key=value` line per change, so `| tee log` produces something
readable:

```
2026-08-10T17:06:03.229+03:00 ft857 status connected=true freq=28030000 mode=CW …
```

The same omission rule applies, and it matters more here: a log is read
afterwards by somebody who cannot ask what the zero meant. `pwr_raw=`,
`swr_raw=`, `swr=` and `alc_raw=` are present only while transmitting;
`passband=`, `filter=`, `power_pct=` and `power_w=` only where the radio has
those commands.

Meter-only changes are throttled (`-interval`, default 1s) — the S meter moves
on every poll, and twenty lines a second of nothing else is not a log.

## Where it connects

With no `-url`, the server address is read from the daemon's own configuration
file (`remoses.yaml` by default, `-config` to point elsewhere):
`server.listen` and `server.base_path` give the URL, and whether `server.tls` is
set decides `http` or `https`.

A wildcard bind such as `0.0.0.0:8080` is not an address a client can dial, so
it resolves to loopback. If the default configuration file is not there, the
daemon's built-in defaults are assumed — `http://127.0.0.1:8080/api/v1`.

`-url` overrides all of it, and fills in `/api/v1` when the URL has no path of
its own.

## Credentials

The username comes from `-user` or `$REMOSES_USER`. The password comes from
`$REMOSES_PASSWORD`, `-password-file` (`-` for stdin), or a prompt with echo
off.

**There is deliberately no password flag**: a password on the command line is
visible in the process table and lands in shell history. The configuration file
is no help either — it holds bcrypt hashes, not passwords.

## What it does about failure

- **A radio that is unplugged is shown as such**, with the last known state and
  how old it is, because that is a normal condition rather than an error.
- **A dropped stream reconnects** with exponential backoff and says so while it
  is trying.
- **A `resync`** — the server's way of saying this client fell behind —
  refetches the state over REST rather than guessing what was missed.
- **Bad credentials and an unknown radio id** are reported and are not retried.
