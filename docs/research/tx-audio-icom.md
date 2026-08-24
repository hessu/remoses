# civ backend — transmit audio, and material for later phases

Transcribed from the manufacturer references in `/Users/hessu/src/cw/remoses-manuals/`.
Every claim cites the document and the page. Where a manual is silent, or prints
something that does not match its siblings, that is said outright rather than
smoothed over — the differences are the useful part.

All fifteen Icom profiles are covered. Page numbers are the **printed** folio;
where the PDF page differs it is given as `PDF n / printed m`.

---

## 1. The three commands, per model

| Model | `14 0B` mic gain | `16 44` compressor | `14 0E` comp level | Source |
|---|---|---|---|---|
| IC-7610 | yes | yes | yes, 0-255→0-10 | CI-V Ref. Guide pp. 3, 4 |
| IC-9700 | yes | yes | yes, 0-255→0-10 | CI-V Ref. Guide pp. 4, 5 |
| IC-7760 | yes | yes | yes, 0-255→0-10 | CI-V Ref. Guide pp. 3, 4 |
| IC-905 | yes | yes | yes, 0-255→0-10 | CI-V Ref. Guide pp. 3, 4 |
| IC-7300MK2 | yes | yes | yes, 0-255→0-10 | CI-V Ref. Guide pp. 5, 6 |
| IC-7300 | yes | yes | yes, 0-255→0-10 | Manual §19, pp. 19-3, 19-4 |
| IC-7600 | yes | yes | yes, 0-255→0-10 | Manual §12, pp. 160, 161 |
| IC-7700 | yes | yes | yes, 0-255→0-10 | Manual §14, pp. 14-3, 14-4 |
| IC-7850/7851 | yes | yes | yes, 0-255→0-10 | Manual §18, pp. 18-3, 18-4 |
| IC-9100 | yes | yes | yes, 0-255→0-10 | Manual §18, pp. 184, 185 |
| **IC-910H** | yes | yes | yes, **0-255→0-100%** | Manual §13, p. 79 |
| IC-718 | yes | yes | **ABSENT** | Advanced Manual §5, p. 5-3 |
| IC-703 | yes | yes | present, **field is 0-10** | Manual §11, p. 72 |
| IC-706MKIIG | **no cmd 14** | yes (`COMP setting`) | **no cmd 14** | Manual §6, p. 46 |
| IC-706, IC-706MKII | **no cmd 14** | **no cmd 16 either** | — | §6, pp. 39 and 41 |

Verbatim, so nothing rests on paraphrase:

- IC-7610 p. 3: `0B` `00 00 ~ 02 55` **"Send/read MIC gain (00 00=min. ~ 02 55=max.)"**; `0E` **"Send/read the COMP level (00 00=0 ~ 02 55=10)"**. p. 4: `16 44` `00 or 01` **"Set the Speech compressor (00=OFF, 01=ON)"**.
- IC-9700 p. 4: `0B` **"Send/read MIC gain (0000=Minimum to 0255=Maximum)"**; `0E` **"Send/read the COMP level (0000=0 to 0255=10)"**. p. 5: `16 44` **"Send/read the Speech compressor (00=OFF, 01=ON)"**.
- IC-7760 and IC-905 p. 4: identical wording to each other and to the IC-9700.
- IC-7300MK2 p. 5: `0B` **"Sets or reads the microphone gain. (00 00=Minimum ~ 02 55=Maximum)"**; `0E` **"Sets or reads the Speech Compressor level. (00 00=0 ~ 02 55=10)"**. p. 6: `16 44` **"Sets or reads the ON/OFF status of the Speech Compressor function. (00=OFF, 01=ON)"**.
- IC-7300 p. 19-3: `0B` **"Send/read [MIC] position (00 00=max. CCW, 02 55=max. CW)"**; `0E` **"Send/read the COMP level (00 00=0 to 02 55=10)"**; `16 44` **"Speech compressor *(00=OFF, 01=ON)"**.
- IC-7600 p. 160: `0B` **"Send/read [MIC GAIN] level (0000=max. CCW, 0255=max. CW)"**; `0E` **"Send/read COMP level (0000=0, 0255=10)"**. p. 161: `16 44` `00`/`01` **"Speech compressor OFF" / "Speech compressor ON"**.
- IC-7700 p. 14-3: `0B` **"Send/read [MIC GAIN] level (0000=max. CCW, 0255=max. CW)"**; `0E` **"Send/read [COMP] level (0000=0, 0255=10)"**. p. 14-4: `16 44` as the IC-7600.
- IC-7850 p. 18-3: `0B` **"Send/read [MIC] position (0000=max. CCW, 0255=max. CW)"**; `0E` **"Send/read COMP level (0000=0, 0255=10)"**.
- IC-9100 p. 184: `0B` **"Send/read [MIC GAIN] position (0000=max. CCW to 0255=max. CW)"**; `0E` **"Send/read COMP level (0000=0 to 0255=10)"**. p. 185: `16 44` **"Send/read Speech compressor OFF / ON"**.
- IC-910H p. 79: `0B` **"[MIC GAIN] level setting (0=max. CCW; 128=center; 255=max. CW)."**; `0E` **"Set mic. compressor level (0=0%; 255=100%)."**; `16 44` **"Set mic. compressor (0=OFF; 1=ON)."**
- IC-718 p. 5-3: `0B` **"Send/read the MIC gain (00 00=Minimum ~ 02 55=Maximum)"**; `16 44` **"Read/send the Compressor function (00=OFF, 01=ON)"**. Its 14 group is 01, 02, 03, 06, 09, 0A, 0B, 0C, 0F — it jumps **0C to 0F**, so there is no 0E.
- IC-703 p. 72: `0B` **"Microphone gain setting (0=mini. to 255=max.)"**; `0E` **"COMP Level setting (0=0 to 10=10)"**; `16 44` **"Speech compressor (0=OFF; 1=ON)"**.
- IC-706MKIIG p. 46: `16 44` **"COMP setting"**, its own labelled row. That table has **no Data column at all**, so it never states 00=OFF/01=ON for anything in the 16 group.

### `14 0E` has three incompatible spellings

This is the finding that shaped the implementation:

1. **0-255 carrying the radio's 0-10 compressor scale** — ten radios, everything modern plus the IC-9100.
2. **0-255 carrying a plain 0-100%** — the IC-910H alone.
3. **The field itself is 0 to 10** — the IC-703 alone: `"COMP Level setting (0=0 to 10=10)"`, printed beside `"RF power setting (0=mini. to 255=max.)"` in the same table, so the difference is deliberate.

`setLevel` writes 0-255 for the whole family, which is right for (1) and (2) and
wrong for (3): a request for 100% would send 255 to a radio whose maximum is 10 —
an out-of-range level, not a refusal. So **the IC-703's ProcLevel is not
claimed**, with the reason in its model entry. Fixing it properly means a
per-model level scale, which is one radio's worth of machinery for a profile that
has never met a radio.

`generic` gets the mic gain and the switch but **not** the level, for the same
reason: all thirteen tables with a command 14 put the mic gain on `0B` over
0-255, and all fourteen with a command 16 put the compressor on `44` as a
two-value switch, with no model spelling either differently — but `14 0E` has the
three spellings above and nothing on the bus says which generation is answering.

### One assumption, named

The **IC-706MKIIG's data byte** for `16 44` is assumed to be 00/01. Its table has
no Data column, so the values come from the rest of the family. This is the same
reading, from the same page, that the existing `16 47` break-in profile already
rests on, and an on/off is the cheapest place to be wrong: two values, both in
every other table, and a byte the radio does not know draws an NG.

---

## 2. Corrections to things the repo asserts — three are live bugs

### 2.1 "No Icom has an antenna selector" is **false**. Command 12 exists on six.

`civ/civ.go` asserted this and `DESIGN.md:2618-2630` / `docs/icom.md:145-150` say
it in prose. It is wrong, and the command is not even spelled the same way twice:

| Model | Shape of command 12 | Source |
|---|---|---|
| IC-7610 | `12 00` ANT1, `12 01` ANT2; data `00`/`01` = **RX ANT off/on** | Ref. Guide p. 3 |
| IC-7760 | `12 00`–`12 03`, ANT1–ANT4; same data meaning | Ref. Guide p. 3 |
| IC-7700 | `12 00`–`12 03`, ANT1–ANT4; ANT4 row is `00` only, "fix" | p. 14-3 |
| IC-7850 | `12 00`–`12 03`, ANT1–ANT4; ANT4 `00` only | p. 18-3 |
| IC-9100 | `12 00` ANT1, `12 01` ANT2 — **Data column empty**, a bare selector | p. 184 |
| IC-7600 | **no sub-command at all**; data is four digits: `0000` ANT1/RX-off, `0001` ANT1/RX-on, `0100` ANT2/RX-off, `0101` ANT2/RX-on | p. 160 |
| IC-7300MK2 | `12 00` `00/01` — "**the ON/OFF status of a receiving antenna**", not a selector | p. 4 |
| IC-9700, IC-905, IC-7300, IC-718, IC-703, IC-910H, IC-706 family | **no command 12** | — |

**The IC-7610 answer the user asked for: yes, it has one.** Verbatim from p. 3:
`12* 00*¹ 00 or 01 Select/read ANT1 selection (00=RX ANT OFF, 01=RX ANT ON)`,
and the same for `01*¹` / ANT2.

Two things make this harder than it looks, and are why I corrected the comment
but left `Antennas: 0`:

- **The socket is the sub-command; the data byte is that socket's receive-antenna
  flag.** A naive `SetAntenna(n)` writing n into the data byte would set ANT1's
  RX-ANT flag instead of selecting socket n. The IC-7600 does not even have a
  sub-command — it packs both into a four-digit data field — so this needs a
  per-model encoder, exactly like the attenuator.
- **The data byte's meaning is conditioned by a Set-mode item.** Footnote *1
  (IC-7610 p. 9): *"If the Antenna Type is set to 'RX-I/O,' command '01 (RX ANT
  ON)' is invalid and '00 (RX ANT OFF)' is always returned."* The item is
  `1A 05 02 75` *"TYPE SET > RX-ANT Connectors (00=Connect a receive antenna,
  01=Connect an external device)"* (p. 8). Related: `16 53` *"Set the ANT-RX I/O
  (00=OFF, 01=ON)"*, which on the IC-7850 is three-valued (OFF / "A" / "B", p. 18-4).

Also worth knowing before publishing a "current antenna": `1A 05 02 89` is *"Send
the Antenna selection mode ([ANT] SW) (00=OFF, 01=Manual, 02=Auto)"* (IC-7610
p. 8). In Auto the radio picks per band from its own memories, so the value would
move under a client with no command sent.

The per-band ANTENNA MEMORY the old comment described **also exists** — IC-7610
`1A 05 02 76` through `02 87`, one per band range (p. 8). The radio has both. The
old comment was right about the memories and wrong that they were the only route.

### 2.2 The IC-706 and IC-706MKII have no command 16 at all

`mkiiFamily` sets `NoiseBlanker: true` (16 22), `Preamp: 1` (16 02) and
`Attenuator: []int{20}` (11) for all three, under a comment saying the two
earlier tables "cannot be read at all".

Both tables are perfectly legible — IC-706 p. 39, IC-706MKII p. 41 — and show a
command set running **05 to 10 and stopping**: no 00–04, no 11, no 15, no 16, no
19. So those three capabilities are claimed for those two radios on the strength
of the *MKIIG's* table, which is the same footing as the 03/04 reads DESIGN.md
records them answering without printing. **Comment corrected, behaviour left
alone** — three capabilities across two models is its own change.

The MKIIG's 16 group was also mis-transcribed: it is **02, 12, 22, 42, 43, 44, 46
and 47** (preamp, AGC, NB, TONE, TSQL, COMP, VOX, BK-IN), each its own labelled
row. The old comment omitted `16 47`. Corrected.

### 2.3 The IC-910H does have a command 14, and an RF gain remoses does not offer

The entry says *"No 14 group in its table at all, so no RF gain."* Its 14 group is
**01, 02, 03, 04, 06, 09, 0A, 0B, 0C, 0E, 0F** (p. 79) — which is also why that
same entry could already set `Power: true` (14 0A) and `MaxWPM: 60` (14 0C)
without anyone noticing the contradiction.

`14 02` is `"[RF GAIN] level setting (0=max. CCW; 255=max. CW)."` and `RFGain` is
false on that profile, so **the radio has a receiver gain control on the bus that
remoses does not offer**. Comment corrected and the gap recorded in the entry;
the flag is not flipped, because that is the front end's change with the front
end's test.

### 2.4 Four PO meter scales are wrong

`modern()` gives every radio `POScale: 255`. DESIGN.md already records why this
matters ("against the wrong scale a radio at full power reads 84%"):

| Model | `15 11` as printed | Should be | Source |
|---|---|---|---|
| IC-7300 | `00 00=0%, 01 43=50%, 02 13=100%` | 213 | p. 19-3 |
| IC-7300MK2 | `00 00=0% ~ 01 43=50% ~ 02 13=100%` | 213 | p. 5 |
| IC-905 | `00 00=0% ~ 01 43=50% ~ 02 13=100%` | 213 | p. 4 |
| IC-7600 | `0000=0%, 0143=50%, 0213=100%` | 213 | p. 160 |
| IC-9100 | `0000=0%, 0141=50%, 0215=100%` | 215 | p. 185 |
| IC-7760 | `00 00=0W ~ 01 43=100W ~ 02 12=200W` | 212 | p. 4 |
| IC-7700 | `0000=0 W, 0143=100 W, 0212=200 W` | 212 | p. 14-4 |
| IC-7850 | `0000=0 W, 0143=100 W, 0213=200 W` | 213 | p. 18-4 |
| IC-718 | `00 00=no transmission, 02 31=100 W (approximate)` | 231 | p. 5-3 |

Only the IC-7610 (255) and IC-9700 (213) are currently right. Note the IC-7700
and IC-7850 differ by one digit for the same nominal 200 W, and that those two
plus the IC-7760 and IC-718 print a **watt** figure — `15 11` on those is
arguably watt-accurate, which `PowerWattAccurate: false` denies family-wide.

Not changed: that is the transmit-meter feature.

---

## 3. Phase 2 — VOX and Data VOX

**VOX is `16 46` on every radio here whose 16 group exists** — fourteen of the
fifteen profiles, the IC-706 and IC-706MKII being the exceptions (no command 16).
Worded "VOX function" on the modern sets and the IC-703/IC-718, "Set VOX
(0=OFF; 1=ON)" on the IC-910H (p. 79), "VOX setting" on the IC-706MKIIG (p. 46,
no data column). It is one row below the compressor in all of them, so
implementing the switch is the same shape as `SetProc`.

**The gains are `14 16` and `14 17` on the modern sets** — "VOX gain" and "Anti
VOX gain", `0000-0255` = 0-100% — on the IC-7610, IC-9700, IC-7760, IC-905,
IC-7300, IC-7300MK2 (worded "VOX sensitivity level" / "ANTI VOX sensitivity
level", with the note *"Higher values make the VOX function less sensitive to the
audio"*), IC-7700 and IC-7850. **The other five keep them elsewhere, and no two
agree:**

| Model | VOX gain | Anti-VOX | VOX delay | Voice delay |
|---|---|---|---|---|
| IC-7610 | `14 16` | `14 17` | `1A 05 02 92` | `1A 05 02 93` |
| IC-7300 | `14 16` | `14 17` | `1A 05 01 91` | `1A 05 01 92` |
| IC-7700 | `14 16` | `14 17` | `1A 05 0182` | `1A 05 0183` |
| IC-7850 | `14 16` | `14 17` | `1A 05 0309` | `1A 05 0310` |
| IC-7600 | `1A 05 0165` | `1A 05 0166` | `1A 05 0167` | `1A 05 0168` |
| IC-9100 | `1A 05 0125` | `1A 05 0126` | `1A 05 0127` | `1A 05 0128` |
| IC-718 | `1A 01 01` `(00 00=1 ~ 02 51=99, 02 55=H)` | `1A 01 03` | `1A 01 02` | — |
| IC-703 | `1A 0309` | `1A 0310` | `1A 0311` | — |
| IC-910H | `1A 02` `+level data` | `1A 04` | `1A 03` | — |

Delay is `00 to 20` = 0.0–2.0 s wherever it appears; voice delay is
`(00=OFF, 01=Short, 02=Mid, 03=Long)`.

**Two collision hazards in that table.** `1A 05 0309`/`0310` are the IC-7850's
*VOX delay and voice delay* while `1A 0309`/`0310` are the IC-703's *VOX gain and
anti-VOX gain* — different commands, near-identical numbers. And the IC-910H's
`1A 02`/`1A 03`/`1A 04` are in the range that is memory contents, band stacking
and keyer memory on every modern set.

**Data VOX: no CI-V row exists on any Icom checked.** Verified by full-document
text search on the IC-7300, IC-7600, IC-7700 and IC-7850 manuals, and by reading
the IC-7610's complete `1A 05` list (items `0001`–`0310`, printed pp. 4-8) and the
IC-9100's set-mode chapter. The IC-718's `1A 01` group runs 01–31 with no such
item; the IC-703's set mode is 43 named items with none; the IC-910H's VOX set
mode has three items and no Data VOX. **A Data VOX field would have nothing to
write on Icom** — if it is wanted for another manufacturer, `caps` has to say so
per family.

**Three things to settle before a VOX phase writes a byte:**

1. VOX keys the transmitter with no command following it. Every other transmit
   path in remoses is interlocked — `tuner_tune` needs the lock, is checked
   against `limits.bands` and arms the dead-man timer (`DESIGN.md:2284-2290`) —
   and none of that can catch a VOX tripping on room noise. Switching it on for
   an operator who cannot see the radio is the `cw.break_in` decision again
   (`DESIGN.md:852-858`).
2. Do not share a code path with Kenwood's `VX`, which **is** break-in in CW on a
   TS-590 and VOX otherwise (`DESIGN.md:1019-1033`), and which remoses already
   writes in CW there. On Icom the two are separate commands (`16 46`, `16 47`),
   so the abstraction must keep them apart or a VOX request would change break-in
   and the CW guard would start consulting a VOX flag.
3. There is no `radio.State` field and no `Caps` flag for VOX yet.

---

## 4. Phase 3 — transmit input / connector selection

**It exists on Icom, it is a Set-mode item, and that means remoses may read it
but must not write it** — the rule at `DESIGN.md:347-380`, "would this still be
changed after remoses is gone?".

Item numbers and value sets are **completely different per model**. Nothing can
be shared:

| Model | Items | Values |
|---|---|---|
| IC-7610 | `1A 05 00 91` DATA OFF, `00 92`/`00 93`/`00 94` DATA1/2/3 | `00=MIC, 01=ACC, 02=MIC,ACC, 03=USB, 04=MIC,USB, 05=LAN` |
| IC-9700 | `1A 05 0115` DATA OFF, `0116` DATA | same six |
| IC-7300 | `1A 05 00 66` DATA OFF, `00 67` DATA | `00=MIC, 01=ACC, 02=MIC/ACC, 03=USB, 04=MIC/USB` — **no LAN** |
| IC-7600 | `1A 05 0030` DATA OFF, `0031`/`0032`/`0033` DATA1/2/3 | `00=MIC, 01=ACC, 02=both MIC and ACC, 03=USB` — **no MIC/USB, no LAN** |
| IC-7700 | `1A 05 0032` DATA OFF, `0033`/`0034`/`0035` DATA1/2/3 | `00=MIC, 01=ACC, 02=MIC/ACC, 03=S/P DIF, 04=LAN` |
| IC-7850 | `1A 05 0063` DATA OFF, `0064`/`0065`/`0066` DATA1/2/3 | **eleven** values, `00=MIC` … `10=MIC/USB`, ACC-A and ACC-B separate |
| IC-9100 | `1A 05 0056` DATA OFF, `0057` DATA | `00=MIC, 01=ACC, 02=MIC+ACC, 03=USB` |
| IC-718, IC-703, IC-910H, IC-706 family | none | — |

Sources: IC-7610 p. 5, IC-9700 p. 7, IC-7300 p. 19-5, IC-7600 pp. 161–162,
IC-7700 p. 14-5, IC-7850 p. 18-5, IC-9100 p. 187.

Matching input **levels** are separate items again: IC-7610 `00 88` ACC / `00 89`
USB / `00 90` LAN (p. 5); IC-9700 `0112`/`0113`/`0114` (p. 7); IC-7300 `00 64`
ACC / `00 65` USB (p. 19-5); IC-7600 `0029` USB-B (p. 161); IC-7700 `0030` ACC,
`0031` S/P DIF, `0192` LAN (pp. 14-5, 14-8); IC-7850 `0058`–`0062` (p. 18-5);
IC-9100 `0054` USB (p. 187).

**A printing inconsistency to handle if these are ever read:** on the IC-7700 all
four connector items give the Data column as `00 to 03` while the description
enumerates five values up to `04=LAN` (p. 14-5). The IC-7850's `00 to 10` is
self-consistent.

**And one live behaviour worth ten minutes on the bench.** `civ/civ.go:802-804`
already notes that `SetMode` with data on sends the DATA**1** flag, since `1A 06`
takes one. On an IC-7610 that means the radio uses whatever `1A 05 00 92` says
the DATA1 modulation input is — so an operator whose USB audio path is configured
on DATA2 or DATA3 has their **effective modulation source changed by an ordinary
data-mode write**. remoses writes no persistent item, so nothing is altered after
it disconnects, but the audio path moves while it is connected. This is the "on
CI-V several settings share a command" hazard of `DESIGN.md:709-732`, and the
IC-7610 is on the bench.

---

## 5. Smaller findings, recorded in passing

- **The compressor moves the transmit passband.** `16 58` (SSB transmit
  bandwidth) takes "one of following values ... depending on the 'COMP' status
  (ON or OFF)" — three separate Set-mode items for WIDE/MID/NAR (IC-7610 p. 4,
  items `1A 05 00 15`/`00 16`/`00 17`; IC-7300MK2 p. 6, items `00 14`/`00 15`/
  `00 16`). So toggling the compressor changes the occupied bandwidth at the
  radio's own choosing. Recorded in `txaudio.go`; remoses neither models `16 58`
  nor tries to hold it still.
- **There is a COMP meter, `15 14`**, which `radio.State` has nowhere to put —
  the same position it is in for Kenwood's `RM2` COMP reading
  (`DESIGN.md:2130-2134`). Calibrations differ and want transcribing per model if
  it is ever added: IC-7610 `(0 dB, 0130=15 dB, 0241=30 dB)`; IC-9700
  `(…, 0210=25.5 dB)`; IC-905 `(…, 0210=25.5 dB)`; IC-7300MK2 `(…, 0210=30 dB)`;
  IC-9100 `(0120=15 dB, 0240=30 dB)`; IC-7300, IC-7600, IC-7700, IC-7850
  `(0130=15 dB, 0241=30 dB)`. Absent on the IC-718, IC-703, IC-910H and IC-706
  family.
- **`16 45` (monitor function) is absent on the IC-718 and the IC-910H** — both
  tables jump 44 → 46 — and present on everything else with a 16 group. Another
  reminder that the group is contiguous nowhere.
- **The IC-9700 has `16 59`**, *"Send/read the sub band (the Dualwatch function)
  (00=OFF, 01=ON)"* (p. 5), and the IC-9100 has the same row (p. 185).
  `civ/model.go` says the IC-9700 has no dual watch because `07 C0/C1/C2` is not
  in its table — true, but `16 59` may be the same capability under a different
  command. Worth a look; not touched.
- **`14 0F` is the break-in delay everywhere and means three different things**:
  `(00 00=2.0d to 02 55=13.0d)` dots on the IC-7300/IC-7610/IC-9100,
  `(0000=max. CCW, 0255=max. CW)` on the IC-7600, `(20=2.0d to 130=13.0d)` on the
  IC-703, and `(0=2.0 sec; 255=13.0 sec.)` on the IC-910H — which calls dots
  seconds. remoses does not send it; if it ever does, that is a per-model scale.
- **`14 0C` is not wpm everywhere.** `(00 00=6wpm, 02 55=48wpm)` on the IC-7300,
  but `(0000=max. CCW, 0255=max. CW)` on the IC-7600 and IC-9100 — the same
  keyer-speed command with no wpm mapping printed. The existing `MaxWPM` field
  assumes the mapping family-wide.
- **`14 0B`'s label is not stable even where the sub-command is**: "MIC gain",
  "[MIC GAIN] level", "[MIC] position", "[MIC GAIN] position", "microphone gain",
  "the MIC gain". Same command throughout, which is the reassuring part.
