# Yaesu radios — the `yaesu` backend

One backend name, **two completely unrelated CAT protocols**:

- the **modern ASCII dialect** — `FA014025000;`, two letters and a semicolon —
  spoken by the FT-950, FT-891, FT-991A, FT-710, FTX-1 and the FTdx family;
- the **FT-857/FT-897 binary protocol** — five fixed bytes with the opcode last,
  packed-BCD frequencies, seventeen commands in total, and no terminator or
  framing of any kind.

`backend: yaesu` is correct for both. remoses works out which one your radio
speaks from `yaesu.model`, so you never have to know which generation it belongs
to — but you do have to name it.

An **FT-857D** has been verified on the air, which is the binary protocol. None
of the ASCII profiles has ever met a radio. See
[hardware status](hardware-status.md).

## Naming the model is not optional

**This matters more here than on any other backend**, for three separate
reasons.

**It picks the protocol.** An FT-857 left unnamed will not work at all, because
an unnamed Yaesu means the modern dialect, which these radios do not speak.

**The mode-code tables are per radio, not per family.** Code `E` is PSK on five
models, C4FM on the FT-991A, and nothing at all on the other six — so the wrong
name reports the wrong mode rather than failing.

**Five models are an eight-digit-frequency generation.** `FA14025000;` where the
newer seven take `FA014025000;`, with an `IF` answer a byte shorter to match.
Naming one as the other puts a malformed command on the wire.

## Configuring a modern Yaesu

```yaml
radios:
  - id: ft710
    name: "Yaesu FT-710"
    backend: yaesu
    port:
      device: /dev/ttyUSB0
      baud: 38400          # the FT-710 and FTX-1 default to this on CAT-1 and CAT-3
    yaesu:
      model: ft-710
      auto_information: true
    poll:
      interval: 500ms
      slow_interval: 5s
    cw:
      enabled: true
      method: serial_key   # see below — no Yaesu has a usable CAT keyer
      default_wpm: 25
      serial_key:
        device: /dev/ttyUSB1
        key_line: dtr
        ptt_line: rts
        ptt_lead_ms: 15
        ptt_tail_ms: 150
    limits:
      max_power_w: 80
      tx_timeout: 120s
```

**`auto_information`** pushes state changes so a front-panel move appears
without waiting for a poll. It reverts to off at rig power-down, so it does not
permanently alter the operator's menu settings. Yaesu has no AI2 — the parameter
is on or off. On an **FTdx10 it works only over the USB CAT port**, so a rig
reached through the RS-232C jack or a serial-to-TCP bridge is poll-only whatever
this says.

## Configuring an FT-857, FT-857D, FT-897 or FT-897D

These need three things no other radio here does, and all three are on the radio
rather than in the file.

```yaml
radios:
  - id: ft857
    name: "Yaesu FT-857D"
    backend: yaesu
    port:
      device: /dev/ttyUSB0
      baud: 38400        # menu 019 CAT RATE. Factory default is 4800.
      stop_bits: "2"     # the manuals specify two; remoses defaults to one
    yaesu:
      model: ft-857d     # REQUIRED — this is what selects the binary protocol
      auto_information: false   # ignored; this generation has none
    poll:
      interval: 500ms
      slow_interval: 5s  # nothing for the slow tier to read, see below
    cw:
      enabled: true
      method: serial_key         # the only option; there is no CAT keyer
      default_wpm: 25
      serial_key:
        device: /dev/ttyUSB1     # must be a different port from CAT
        key_line: dtr
        ptt_line: rts
        ptt_lead_ms: 15
        ptt_tail_ms: 150
    limits:
      tx_timeout: 120s   # no max_power_*: there is no power command to clamp
```

**On the radio:**

1. **Menu 019 `CAT RATE`** — 4800 out of the box, and 9600 and 38400 are
   offered. Raising it is worth doing: at 4800 the fast poll's three commands
   take a visible slice of a 500 ms tick.
2. **Menu 020 `CAT/LIN/TUN` must be `CAT`**, or the rear-panel jack is driving a
   linear amplifier rather than talking to a computer.
3. **Two stop bits.** The manuals specify one start bit, eight data bits, no
   parity and two stop bits; remoses defaults to one, so this must be set.

**No `max_power_*` here.** These radios have no CAT command for transmit power
in either direction, so there is nothing for remoses to clamp — `RF POWER SET`
on the front panel is the only control, and a limit written here would be one
remoses could not enforce.

**The slow poll tier is empty.** Power, filter width and filter slot are what it
carries on every other radio, and this CAT set has no command for any of the
three.

## Supported models

### The modern ASCII dialect

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

**Two report transmit power as an uncalibrated `000`–`255` index rather than
watts** — the FTdx5000 and FTdx9000 — so remoses shows a percentage and refuses
a request in watts on them. Use `max_power_pct` there.

**The FTdx9000 has no `ID` command**, so remoses cannot cross-check that the
configuration names the right radio; it says which command set it is using and
carries on. Its `SH` is the position of the WIDTH knob rather than a bandwidth
in Hz, so remoses reports no filter width for it and refuses to set one. It is
also the one radio here with **no attenuator command** at all, while keeping the
preamplifier, RF gain and AGC.

### The FT-857 / FT-897 binary protocol

| Model | `yaesu.model` | Notes | Tested |
|---|---|---|---|
| FT-857 | `ft-857` | | — |
| FT-857D | `ft-857d` | Frequency, every mode, PTT and the transmit meters exercised on the air; CW untried, as it needs a second serial port | **yes** |
| FT-897 | `ft-897` | | — |
| FT-897D | `ft-897d` | | — |

Nothing in the wire format differs across the four, so in principle what the
FT-857D confirmed holds for the other three — but only the FT-857D has been on a
wire, and the **Tested** column says so rather than crediting a radio nobody
plugged in.

## CW on a Yaesu means keying a control line

**No Yaesu here can key arbitrary CW text over CAT.** On the modern radios `KY`
plays a *stored keyer memory*, and remoses will not overwrite the operator's
saved messages to send one message. The FT-857/FT-897 have no CW-over-CAT
command of any kind.

So CW on any Yaesu is `cw.method: serial_key`, keying DTR or RTS on a second
serial port. Every modern model supports it through its **`PC KEYING` menu
item** — set that to RTS or DTR to match your `key_line`.

That path is confirmed on hardware, on both DTR and RTS, but against an IC-7610
— it is not Yaesu-specific code. **No Yaesu has ever run it**, and no Yaesu has
yet put CW on the air through remoses.

## The receive front end

Uniform across the modern family: `PA` for the preamplifier (IPO, AMP 1, AMP 2 —
the FT-891 has only IPO and AMP), `RA` for the attenuator, `RG` for the gain and
`GT` for the AGC. The FT-891, FT-991A and FTX-1 have a single pad where the FTdx
sets step 6/12/18 dB.

**`GT` does not round-trip, and that is not a fault.** It *accepts* 0 to 4,
where 4 is AUTO, and *answers* 0 to 6, where 4, 5 and 6 are auto having settled
on fast, mid or slow. So setting `auto` and reading back `auto-mid` means the
radio is telling you what auto currently is. remoses publishes the resolved
value rather than flattening the three into the one that was written, and
refuses a request for an `auto-*` reading — there is no way to tell a radio "be
automatic, and also be mid".

The FT-857 generation has none of this: no preamplifier, attenuator, RF gain or
AGC command exists in its seventeen.

## What the FT-857 and FT-897 cannot do

It is the radios rather than remoses. Their seventeen-command CAT set has:

- **no transmit power**, in either direction — `RF POWER SET` is a front-panel
  menu item;
- **no filter width and no filter selection** — the optional YF-122 filters are
  chosen with the front panel's `[B]` and `[C]` keys;
- **no CW over CAT**, as above;
- **no push updates** — a front-panel knob movement is invisible until the next
  poll;
- **no VFO by name** — the one VFO command is a blind toggle that swaps A and B
  and reports nothing, so remoses controls whichever VFO the radio is on and
  refuses A and B rather than tuning one and labelling it the other;
- **no model identification** — there is no `ID` command in this generation, so
  the configured model is all remoses will ever know.

Frequency, mode, PTT, the S-meter, the transmit power meter and the high-SWR
warning all work, and all of that has been seen on an FT-857D.

## Three things an FT-857D does that no manual mentions

Worth knowing before driving one remotely.

**A mode change moves the frequency.** Selecting CW adds the radio's CW pitch
offset to the displayed frequency — +600 Hz on the one tested — and DIG
subtracts its own shift, −2120 Hz. So `{"mode": "CW"}` on its own leaves the
dial somewhere else. Sending frequency and mode together is unaffected: remoses
applies mode first for exactly this reason, and the frequency write puts the
offset back.

**Keying it over CAT in CW transmits nothing.** The radio goes into transmit —
`ptt` reads true, the relay closes — and the power meter stays at zero, because
in CW the transmitter needs the key line. `serial_key` is therefore not just the
only way to *send* CW on these radios, it is the only way to get RF out of them
in CW at all; a CAT tune-up needs FM or AM. The useful side of this is that CW
is a safe mode for testing a remote setup, since nothing reaches the antenna.

**Switched off, it goes silent rather than refusing.** An Icom in standby
answers every command with a rejection, which remoses reports as `standby` on a
live link. This one just stops answering while its USB adapter stays present, so
it is reported as **disconnected** — the honest answer, and it means
`power_switch`-style remote waking is not available here in any form. It
reconnects by itself when the radio comes back.

---

# Protocol notes

Everything below is for somebody reading the code or debugging a frame.
[DESIGN.md §5.6](DESIGN.md) covers the ASCII dialect and
[§5.7](DESIGN.md) the binary one.

## The binary protocol in one table

| | ASCII Yaesu | FT-857 / FT-897 |
|---|---|---|
| Frame | `FA014250000;` | `43 97 00 00 01` — **five bytes, always** |
| Command position | first | **last** |
| Frequency | decimal digits in Hz | **packed BCD in units of 10 Hz** |
| Mode | a character, `2` = USB | a byte, `02` = **CW** |
| Answer framing | `;` terminates | **nothing terminates anything** |
| Commands | hundreds | **seventeen** |
| Push updates | `AI1;` | **none at all** |

**There is no framing, and that shapes the whole backend.** An answer has no
terminator, no length, no opcode, no checksum and nothing that identifies it.
Answers are one byte or five, and the two are not distinguishable by content —
an acknowledgement of `00` and the leading digit pair of a status answer on the
1.8 MHz band are the same byte. The only thing that knows where a frame ends is
the command that provoked it, which is why this is the one backend implementing
`backend.ReplyFramer`.

**Every command is acknowledged, and no manual says so.** The command chart
lists a reply for the three reads only, but the radios answer everything else
with a single byte, and remoses waits for it. That is framing rather than
thoroughness: on a delimiter-free stream an unconsumed byte becomes the first
byte of the next answer and offsets everything after it permanently. Confirmed
on an FT-857D, which answered `00` to every set it was given.

**A slipped stream cannot resynchronise in place, so it is made to fail
instead.** The status answer's four BCD bytes are eight nibbles that must all be
0–9, so a frame offset by even one byte fails that test about forty-nine times
in fifty. The decoder reports it bad, five consecutive poll failures tear the
connection down, and the reconnect starts a clean stream.

**`06` is WFM, and it is read-only.** The status table has it and the mode-set
table does not, so WFM is the one mode that can appear in `state.mode` and never
in `caps.modes`. Confirmed in both directions on hardware: tuning to 100 MHz put
the radio into WFM by its own band memory and remoses read it back, while
`{"mode": "WFM"}` is refused.

**`0A` is DIG and `0C` is PKT.** DIG is one mode for the same reason C4FM is one
mode on an FT-991A: which digital mode it actually is — RTTY, PSK31, USER, each
in two sidebands — is a menu item no CAT command reads. PKT is packet on FM and
maps to FM with the data flag.
