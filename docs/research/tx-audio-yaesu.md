# Yaesu ASCII CAT — transmit audio, VOX and transmit-input notes

Written while implementing `backend.TXAudioController` for
`internal/rig/backend/yaesu/`. Everything below is what the **repository itself**
contains, with file:line. Where I had to say "not corroborated", that means the
repo does not say it — not that the radio lacks it.

The search covered `docs/` (including the gitignored `docs/DESIGN.md~` backup),
`internal/`, `api/`, `cmd/`, `README.md`, `remoses.example.yaml` and git history.
There is **no vendor / reference / testdata / manual directory anywhere in the
repo, and no `.txt`, `.pdf` or `.csv` file** — so `docs/DESIGN.md` and the Go
comments are the whole of the transcribed CAT material.

---

## 1. This phase: what I could and could not corroborate

### `MG` — microphone / transmit gain: **partially corroborated, not enough to send**

The only occurrence in the whole repository is one sentence:

`docs/DESIGN.md:1174-1179`

> **`PC` is not always watts.** On the FTdx5000 and FTdx9000 the manuals give the
> same three digits as `000`–`255` where every other model gives a watt range, in
> the same manuals that give `MG`, `PL`, `SQ` and `RG` as `000`–`255` too — this
> is those radios' level scale, not a typo.

(Duplicated verbatim in the editor backup `docs/DESIGN.md~:738`.)

What that establishes:

- `MG` exists **on the FTdx5000 and FTdx9000**, per those two manuals.
- Its **value field is three digits, `000`–`255`** on those two radios.

What it does **not** establish, and this is what stopped me:

- **The frame layout.** `RG` is in the same list with the same `000`–`255`, and
  `RG` on the wire is `RG0<nnn>;` — three digits *behind a main-receiver
  selector* (`internal/rig/backend/yaesu/frontend.go:27-35, 142`). So the note is
  describing a *parameter*, not a frame, and `MG<nnn>;` and `MG0<nnn>;` are
  equally consistent with everything written down here.
- **Whether the other ten models have `MG` at all**, or what range they use if
  they do. The `docs/DESIGN.md:1081-1095` per-model table has columns for `ID`,
  `FA`, `FA` range, `IF`, modes, `SH` form and `PC` — **no level-command
  columns**.

Cost of guessing wrong, which is why I did not: a malformed set answers with
silence and burns the session's whole per-command timeout (this family documents
no error response — `docs/DESIGN.md:1131-1135`); worse, a read at the wrong
offset turns `MG0128` into `4.7%` on a rig sitting at half gain — a confident
wrong number, which is the `docs/DESIGN.md:5.4` failure this backend is shaped
around.

**To unblock:** one line of one manual — the `MG` row of the FTdx5000's or
FTdx9000's CAT reference (frame *and* parameter), plus the same row from any
FTdx101-generation manual to learn whether it is family-wide. Turning it on is
then a `Model` field, a decoder, and a slow-tier read in exactly the shape
`frontend.go` already uses for `RG`.

### `PR` — speech processor switch: **ZERO hits repo-wide**

Searched `\bPR\b`, `` `PR` ``, `"PR"`, `PR0`, `PR1`, `PR;` across docs, source,
API spec, config and git history. **Nothing, in any form.** The `yaesu` codec key
table (`internal/rig/backend/yaesu/codec.go:13-41`) is, verbatim and complete:
`FA FB MD PC SM RM AC PA RA RG GT NB NL NR RL BP BC AN PS SH NA IF TX ID AI`,
plus `keyBusy = "?"`.

So the processor's switch is not "false on the models that lack it" — it is false
because this project has never transcribed it for any model.

### Processor level: **`PL` named, never identified; and the `EX` route is closed by policy**

- `PL` appears **only** in the same `docs/DESIGN.md:1176` sentence, in the list
  "`MG`, `PL`, `SQ` and `RG` as `000`–`255`". Nothing anywhere says `PL` *is* the
  speech processor level — that expansion is outside knowledge, not repo
  material. (The Go identifier `ProcLevel` is common in the tree but is never
  tied to the literal string `PL`.)
- If the level is an `EX` menu item instead, that route is **closed by an
  existing rule, not merely uncorroborated**:
  - `docs/DESIGN.md:360-364` — the "what remoses declines to write" table lists
    `Yaesu EX | Menu items, e.g. 090 AMS TX MODE`.
  - `docs/DESIGN.md:1249-1252` and `internal/rig/backend/yaesu/model.go:401-406`
    — "remoses does not write `EX` for the same reason it does not write `KM`".
  - `internal/rig/backend/yaesu/yaesu_test.go:46-79` (`TestNeverWritesKeyerMemory`)
    asserts on **every model** that no request begins `KM`, `KY` or `EX`.
  - **090 (AMS TX MODE, FT-991A) is the only specific `EX` item number anywhere
    in the repo.** There is no table of per-model `EX` numbers to work from, so
    an `EX` processor level would be a guessed index *and* a policy violation.

### What I shipped

`internal/rig/backend/yaesu/txaudio.go` — `TXAudioController` implemented, all
three methods refusing with `backend.ErrUnsupported` and naming the model;
`Caps.MicGainControl` / `ProcControl` / `ProcLevelControl` written out as
explicit `false` in `yaesu.go` with the reason; tests in `txaudio_test.go`
including a durable guard that a model whose caps say "no transmit audio" is
never *asked* about `MG`/`PR`/`PL` on any poll tier.

---

## 2. Next phase: VOX and Data VOX

**Nothing about Yaesu VOX exists in this repository.**

- `VX` has **zero hits** inside `internal/rig/backend/yaesu/` or
  `internal/rig/backend/yaesubin/`.
- The phrase **"Data VOX" has zero hits repo-wide**, in any spelling.
- Every `VX` in the tree is **Kenwood**: `internal/rig/backend/kenwood/codec.go:31`
  (`keyVX backend.Key = "VX"`), `kenwood/breakin.go:66,77-78,158`,
  `kenwood/model.go:215-217,474,497,510`, `kenwood/kenwood.go:603`.

The Kenwood material is worth reading before doing Yaesu VOX, because it is where
this project already thought about the VOX/break-in collision:

`docs/DESIGN.md:1012-1036` (table + prose), mirrored in `docs/kenwood.md:115-142`:

> The TS-590 row is the trap. That radio has no break-in command at all: `VX`
> sets VOX, and its reference adds that "when transmitting the VX command in CW
> mode, the Break-in function is set and read, rather than the VOX function". One
> command, two meanings, selected by the mode the radio happens to be in.

…and the TS-480 row is explicitly an **inference** with the asymmetry argument
written out (`docs/DESIGN.md:1027-1038`), which is the template this project uses
when it does decide to act on an uncorroborated command.

Two consequences for a Yaesu VOX phase:

1. **Do not assume Yaesu's VOX is `VX` because Kenwood's is.** The package doc at
   `internal/rig/backend/yaesu/yaesu.go:9-34` exists precisely because Yaesu and
   Kenwood share framing and disagree on fields, and because two shared letters
   (`KY`, `TX`) mean *different things* on the two makes. `TX` is the worked
   example: a set on Kenwood, a read on Yaesu.
2. **Yaesu break-in is currently unmodelled too.** `Caps.CWMethod` is `CWNone` on
   every Yaesu model (`yaesu.go:319-323`), so there is no CW guard here that a
   VOX/break-in reading would feed — unlike Kenwood, where the reading decides
   whether Morse will actually be transmitted. That lowers the stakes but also
   removes the justification Kenwood had for acting on an inference.
3. A "Data VOX" control, if it exists on these radios, is the kind of thing this
   family keeps in the `EX` menu — see the `EX` policy above.

---

## 3. Next phase: transmit input / connector selection (mic vs USB vs ACC vs DATA)

**Zero hits** for `MOD source`, `MODSRC`, `modulation source`, `DATA IN`,
`DATA-IN`, `DATAIN`, `MIC SELECT`. There is no Yaesu menu number for it anywhere
in the repo.

But the project has already **decided not to model it**, in four places, and a
future phase should start from that decision rather than reopen it:

- `internal/radio/types.go:641-661` (`State.MicGain` doc):
  > MicGain is the gain on whatever input the radio is currently taking transmit
  > audio from, and is NOT specifically the microphone socket despite every
  > manufacturer naming it after one. Which connector feeds the modulator — mic,
  > USB, ACC, LAN — is a menu setting on all three families, and remoses does not
  > read it […] A client that promises the operator it is adjusting the USB input
  > may be adjusting the microphone.
- `internal/radio/types.go:1294-1306` (`Caps` doc): "None of them says which
  connector the gain belongs to."
- `internal/rig/backend/backend.go:348-364` (interface doc): "What this interface
  deliberately does NOT model is which connector the audio comes from."
- `api/openapi.yaml:973-1005` and `:1535-1550` — the same statement is already
  **published to clients**.

So implementing connector selection is an API-surface change (new state field,
new cap, new patch field, spec + generated `internal/wire/wire.gen.go`), not just
a backend one. Note the canonical connector list in the existing prose is
**"mic, USB, ACC, LAN"** — it does not mention DATA.

The only Yaesu "rear panel" material in the repo is unrelated to audio routing:

- `docs/yaesu.md:107-115` and `docs/DESIGN.md:1443-1447` — menu **019 CAT RATE**
  and menu **020 CAT/LIN/TUN** (the rear jack drives a linear amplifier unless
  set to `CAT`). FT-857/897 generation.
- `internal/rig/backend/yaesubin/yaesubin.go:247-251` — "Both radios have a KEY
  jack and a rear-panel data port".
- Icom's DATA1/2/3 modes "differ only in which modulation input they use, which
  is a station wiring choice remoses does not model" —
  `internal/rig/backend/civ/dualvfo.go:54-58`, `civ/civ.go:782-786`. That is the
  closest precedent in the tree for declining connector selection.

---

## 4. Cross-cutting facts worth carrying forward

- **No backend implements `TXAudioController` yet.** As of this work the only
  references are the interface itself (`backend.go:365-377`) and the session
  plumbing (`internal/rig/session.go:896-958`). `validateTXAudio` checks
  `Caps` **before** dispatch, so a false cap refuses at the session and the
  backend's own refusal is only a second line of defence.
- **There is a `tx-audio` selftest group already written**
  (`internal/selftest/steps.go:551-610`, registered at `:30`). It drives
  `mic_gain`, `proc` and `proc_level` purely through the patch API and skips with
  `"no transmit gain command"` / `"no processor level command"` when the caps are
  false — so the yaesu backend will simply report those skips, which is the
  correct outcome today.
- **The transmit audio chain is not documented in `docs/` at all.** `grep -i` for
  "processor", "speech", "mic gain" or "proc" over every `.md` file returns
  nothing — it exists only in Go comments and `api/openapi.yaml`. `docs/features.md`
  and `docs/yaesu.md` will both need a section when a backend can actually work
  it, and `docs/DESIGN.md` has no §5.x for it either.
- **Yaesu refusals are silent.** `docs/DESIGN.md:1131-1135` — no manual documents
  an error, NAK or busy response, so an unimplemented command, an out-of-range
  parameter and a mode the rig will not take all cost a full per-command timeout.
  The one exception is the undocumented `?;` busy answer, treated as transient
  (`docs/DESIGN.md:1140-1157`). This is why every capability here is recorded per
  model rather than probed, and why nothing speculative may be sent.

---

# PART TWO — written from the manufacturer CAT manuals

Everything above this line was written from repository material alone. Everything
below is transcribed from the PDFs in `/Users/hessu/src/cw/remoses-manuals/`.

**Page numbers are the PRINTED page shown in the page footer**, not the PDF page
index. In every one of these documents the PDF page is the printed page plus one
(the cover is unnumbered).

**Read this distinction throughout:** a row marked *transcribed* means I opened
the Control Command Table entry and counted its cells. A row marked *index only*
means I saw the command listed in that model's Control Command List with its
Set/Read/Ans. flags, but did **not** open its table entry. Index-only rows are
evidence the command exists and nothing more — not its frame, not its range.

---

## 5. `MG`, `PR`, `PL` — the transmit audio chain, all twelve transcribed

Section 1 above is superseded. Every claim in this section is a transcribed
table entry.

### 5.1 `MG` — the frame question, settled

**`MG<nnn>;` on all twelve. There is no receiver selector.** Read is a bare
`MG;`; the answer is `MG<nnn>`.

The FTdx5000 is what settles it rather than a majority vote. That radio has a
real second receiver and *does* put a MAIN/SUB selector in front of the digits on
`AG`, `GT`, `BC`, `AN`, `IS`, `NL`, `RL` and `RG` — and still prints MIC GAIN as
six cells, `M G P1 P1 P1 ;` with `"P1  000 - 255"` (FTdx5000 Series CAT Operation
Reference 1907-D, p.12). The FTdx101 repeats the contrast: `RG` is
`R G P1 P2 P2 P2 ;` with `"P1 0: MAIN BAND / 1: SUB BAND"` (p.20) while `MG` is
`M G P1 P1 P1 ;` (p.15). There is one transmitter, so there is nothing for a
selector to choose.

So the `MG0<nnn>;` reading that blocked the previous phase is **wrong**, and the
`RG` parallel that suggested it is a coincidence of parameter width.

### 5.2 The per-model table

| Model | `MG` range | `PL` range | `PR` shape | `PR` state values | Pages (MG / PL / PR) |
|---|---|---|---|---|---|
| FT-950 | **000 - 255** | 000 - 100 | 1 param | `0`=OFF, `1`=proc ON, `2`=Parametric Mic EQ ON | 11 / 13 / 13 |
| FTdx1200 | 000 - 100 | 000 - 100 | 2 param | `1`=OFF, `2`=ON | 11 / 13 / 13 |
| FTdx3000 | 000 - 100 | 000 - 100 | 2 param | `1`=OFF, `2`=ON | 11 / 13 / 13 |
| FTdx5000 | **000 - 255** | **000 - 255** | 1 param | `0`=OFF, `1`=proc ON, `2`=Mic EQ ON | 12 / 14 / 14 |
| FTdx9000 | **000 - 255** | **000 - 255** | **misprinted** | `0`=OFF, `1`=ON | 6 / 8 / 8 |
| FT-991A | 000 - 100 | 000 - 100 | 2 param | `1`=OFF, `2`=ON | 11 / 14 / 14 |
| FT-891 | 000 - 100 | 000 - 100 | 2 param | **`0`=OFF, `1`=ON** | 12 / 14 / 14 |
| FT-710 | 000 - 100 | **001 - 100**, `000`="OFF" | 2 param | `1`=OFF, `2`=ON | 15 / 18 / 18 |
| FTdx10 | 000 - 100 | 000 - 100 | 2 param | `1`=OFF, `2`=ON | 15 / 18 / 18 |
| FTdx101D/MP | 000 - 100 | 000 -100 | 2 param | `1`=OFF, `2`=ON | 15 / 18 / 18 |
| FTX-1 | 000 - 100 | **`000`="OFF", 001 -100** | 2 param | `1`=OFF, `2`=ON | 19 / 22 / 22 |

Documents: FT-950 CAT Operation Reference Book; FTDX1200_CAT_OM_ENG;
FTDX3000_CAT_OM_ENG_2006-D; FTDX5000_CAT_OM_ENG_1907-D; FTDX9000_CAT_MANUAL
(titled "FTDX9000 Operating Manual" in its own footer); FT-991A_CAT_OM_ENG_1711-D;
FT-891_CAT_OM_ENG_1909-C; FT-710_CAT_OM_ENG_2306-C; FTDX10_CAT_OM_ENG_2308-F;
FTDX101MP_D_CAT_OM_ENG_2308-L; FTX-1_CAT_OM_ENG_2508-C.

**Note that the splits do not follow the FA/FB generation boundary.** The
FTdx1200 and FTdx3000 have the FT-950's eight-digit frequency field and the
FTdx101's transmit audio. This is why every field is per model.

### 5.3 `PR`'s two shapes, and the parametric microphone equalizer

Every one of these radios has a parametric mic equalizer alongside the
compressor, and `PR` is the command that reaches both.

*Single-parameter* (FT-950 p.13, FTdx5000 p.14): `PR<n>;`, read `PR;`.
FT-950 legend verbatim: `"0: Speech Processor "OFF" / 1: Speech Processor "ON" /
2: Parametric Microphone Equalizer "ON""`. The FTdx5000 prints the same with
`"Microphone Equalizer"`. So value `2` means the compressor is *not* in circuit.

*Two-parameter* (the other eight): `PR<sel><state>;`, read `PR<sel>;`, where
`"P1  0: Speech Processor / 1: Parametric Microphone Equalizer"` and P2 is the
state.

**The FT-891 is the only model whose P2 is `0`/`1`.** Verified directly, twice, on
FT-891 CAT Operation Reference Book 1909-C p.14: `"P2  0: "OFF" / 1: "ON""`. The
other seven all print `"P2  1: "OFF" / 2: "ON""` (FT-710 p.18, FT-991A p.14,
FTdx10 p.18, FTdx101 p.18, FTX-1 p.22, FTdx1200 p.13, FTdx3000 p.13). This is the
one place in the family where a wrong value **acts** rather than timing out:
`PR01;` is "off" on an FTdx101 and "on" on an FT-891.

The FTdx3000's P1 legend is printed self-inconsistently — `"0: Speech Processor
"OFF"" / "1: Parametric Microphone Equalizer "ON""`, carrying states on what is
plainly the selector, while P2 carries states again (p.13). Nothing depends on
resolving it: both readings agree that `0` addresses the compressor.

### 5.4 The FTdx9000's `PR` is a misprint — the one blocked capability

FTDX9000_CAT_MANUAL p.8. The block is headed **`PR`** and titled **"RF Speech
Processor Status"**, and the command list on p.2 lists `PR  RF Speech Processor
Status` with Set/Read/Ans all `O`. But all three of its rows — Set, Read and
Answer — spell the command **`P C`**, the letters of "TX Power Level", whose own
block sits directly above it on the same page with an incompatible three-digit
parameter. Its legend is `"P1  0: RF Speech Processor "OFF" / 1: RF Speech
Processor "ON""`.

One of those two statements is wrong and the document does not say which, so the
frame is **not transcribed** and `SetProc` refuses on that model alone. Its `MG`
(p.6) and `PL` (p.8) rows are clean and are enabled.

**To unblock:** one `PR0;` / `PR1;` against a real FTdx9000. `PR` is also the
fail-safe guess if anyone wants to try it — a wrong `PR` costs a timeout, whereas
`PC<n>;` is a malformed *power* command.

---

## 6. VOX — the rows, per model

**`VX` is right for Yaesu too**, but it is `VX` alone: no receiver selector, and
the table title is "VOX STATUS" where the index says "VOX". Yaesu's VOX has no
break-in overlap of the kind that makes Kenwood's `VX` dangerous — Yaesu keeps
CW break-in on a separate `BI` command (and `SD` for its delay), so the TS-590
trap described in §2 above **does not apply to this family**.

### 6.1 Transcribed rows

| | `VX` switch | `VG` gain | `VD` delay |
|---|---|---|---|
| FT-710 (p.22) | `V X P1 ;` `"0: VOX "OFF" / 1: VOX "ON""` | `V G P1 P1 P1 ;` `"000 - 100"` | see 6.2 |
| FT-891 (p.18) | `V X P1 ;` same | `V G P1 P1 P1 ;` `"000 - 100"` | `V D P1 P1 P1 P1 ;` `"0030 - 3000 msec (10 msec multiples)"` |
| FT-991A (p.18 / p.17) | `V X P1 ;` same | `V G P1 P1 P1 ;` `"000 - 100"` | `V D P1 P1 P1 P1 ;` `"0030 - 3000 msec (10 msec multiples)"` |
| FTdx101 (p.23) | `V X P1 ;` same | `V G P1 P1 P1 ;` `"000 - 100"` | see 6.2 |
| FTdx1200 (p.17) | `V X P1 ;` same | `V G P1 P1 P1 ;` `"000 - 100"` | `V D P1 P1 P1 P1 ;` `"0030 - 3000 msec (10 msec multiples)"` |
| FTdx3000 (p.17) | `V X P1 ;` same | `V G P1 P1 P1 ;` `"000 - 100"` | `V D P1 P1 P1 P1 ;` `"0030 - 3000 mS (10 mS multiples)"` |

*Index only* — `VX`, `VG` and `VD` are all listed with Set/Read/Ans `O`, rows not
opened: **FT-950** (p.3), **FTdx5000** (p.3), **FTdx9000** (p.2, which lists
`VD VOX Delay Time`, `VG VOX Gain`, `VX VOX Status`), **FTdx10** (p.5),
**FTX-1** (p.5). Do not encode those five without opening their entries.

### 6.2 `VD` is the one to be careful with — two manuals contradict themselves

On the FT-891, FT-991A, FTdx1200 and FTdx3000, `VD` is four digits carrying raw
milliseconds, `0030 - 3000`. Clean.

On the **FT-710 (p.22)** and **FTdx101 (p.23)** the frame renders as four `P1`
cells but the legend enumerates **two-digit index codes**: `"00: 30 msec  01: 50
msec  02: 100 msec  03: 150 msec  04: 200 msec  05: 250 msec  06: 300 msec - 33:
3000 msec"`. Those cannot both be right. Supporting evidence that the *legend* is
the newer text and the frame diagram is stale: the sibling `SD` (CW BREAK-IN
DELAY TIME) on FT-710 p.19 / FTdx101 p.21 carries an identically-styled `00`–`33`
legend with only **two** `P1` cells. The step wording is also wrong in both
(`"06 - 33: 10 msec multiples"` where 06=300 to 33=3000 over 27 steps is 100
msec/step; `SD` prints "100 msec steps" for the same span). **Verify `VD`'s width
on hardware before encoding it for those two models.**

### 6.3 Anti-VOX — a CAT command on three models, `EX` on the rest

`AV  ANTI VOX LEVEL` **exists as a two-letter command** on:

- **FT-710** p.7 — `A V P1 P1 P1 ;`, read `A V ;`, `"P1  001-100: ANTI VOX LEVEL"`
- **FTdx101** p.6 — `A V P1 P1 P1 ;`, read `A V ;`, `"P1  001-100: ANTI VOX LEVEL"`
- **FTdx10** — index only (p.5 lists `AV  ANTI VOX LEVEL`), row not opened

Note the range starts at **001**, not 000, on both transcribed models. On the
FTdx101 anti-VOX is *not* an `EX` item at all — it is CAT-only plus a front-panel
knob assignment (p.11, `CS DIAL` value `06: ANTI VOX`).

**No `AV` row in the index** on: FT-891 (p.3), FT-991A (p.3), FTdx1200 (p.3),
FTdx3000 (p.3), FTX-1 (p.5), FT-950 (p.3), FTdx5000 (p.3), FTdx9000 (p.2). On
those it is `EX` only:

| Model | `EX` item | Printed values |
|---|---|---|
| FT-950 | **117** VOX ANTI-TRIP GAIN | `"000 ~ 100"` (p.8) |
| FT-891 | **1619** ANTI VOX GAIN; **1622** ANTI DVOX GAIN | `"0 - 100 (P2= 000 - 100)"`, 3 digits (p.9) |
| FT-991A | **145** ANTI VOX GAIN; **148** ANTI DVOX GAIN | `"000 ~ 100"`, 3 digits (p.9) |
| FTdx1200 | **183** TX GNRL ANTI VOX GAIN | `"000 ~ 100"`, 3 digits (p.9) |
| FTdx3000 | **183** TX GNRL ANTI VOX GAIN | `"0 ~ 100"`, digit count not printed (p.9) |
| FTdx5000 | **176** ANTI VOX GAIN | `"000~100"`, 3 digits (p.9) |

The FTX-1's TX GENERAL block (p.12) has no anti-VOX item at all; nor does the
FTdx10's (p.12). Not found is not the same as absent — neither manual's full menu
chart was read end to end for this.

---

## 7. Data VOX — no model has a separate command

**No two-letter DATA VOX command exists on any of the twelve.** The mechanism is
the same everywhere: one `EX` item selects which input VOX listens to, and `VG` /
`VD` then address whichever set is selected.

The FT-710 (p.22) and FTdx101 (p.23) say so explicitly in a note on `VD`:

> "VD command has different parameters to be changed according to the setting of
> Menu item [OPERATION SETTING] → [TX GENERAL] → [VOX SELECT]. "MIC": VOX DELAY
> "DATA": DATA VOX DELAY"

The FT-991A carries the same note against its menu **142** (p.17). The FT-891 has
**no** such note on `VG`/`VD` (p.18), so which of its two sets those commands
reach is **not stated in its manual** — worth knowing before anyone builds a UI
on it.

### 7.1 The VOX source selector, per model

| Model | `EX` address | Printed values |
|---|---|---|
| FT-950 | **114** VOX OPERATION | `"0: MIC INPUT   1: DATA INPUT"` (p.8) |
| FT-891 | **1616** VOX SELECT | `"0: MIC   1: DATA"` (p.9) |
| FT-991A | **142** VOX SELECT | `"0: MIC   1: DATA"` (p.9) |
| FTdx1200 | **180** TX GNRL VOX SELECT | `"0: MIC   1: DATA"` (p.9) |
| FTdx3000 | **180** TX GNRL VOX SELECT | `"0: MIC   1: DATA"` (p.8) |
| FTdx5000 | **175** VOX SELECT | `"0: MIC, 1: DATA"` (p.9) |
| FTdx10 | **03 / 04 / 05** VOX SELECT | `"0: MIC   1: DATA"` (p.12) |
| FTdx101 | **03 / 04 / 05** VOX SELECT | `"0: MIC   1: DATA"` (p.12) |
| FT-710 | **03 / 04 / 05** VOX SELECT | `"0: MIC   1: USB   2: REAR (RTTY/Data Jack)"` (p.12) |
| FTX-1 | **03 / 05 / 10** VOX SELECT | `"0: MIC   1: USB   2: Bluetooth"` (p.12) |

The FT-710 and FTX-1 are worth noticing: three sources, not two, and the FTX-1
lists **Bluetooth** — the first radio in this registry with a wireless transmit
input.

### 7.2 Separate DATA VOX gain/delay items where they exist

- FT-891 p.9: **1620** DATA VOX GAIN `"0 - 100"`, **1621** DATA VOX DELAY `"30 - 3000 msec"`
- FT-991A p.9: **146** DATA VOX GAIN `"000 ~ 100"`, **147** DATA VOX DELAY `"30 ~ 3000 msec"`
- FTdx1200 p.8: **079** DATA VOX GAIN, **080** DATA VOX DELAY
- FTdx3000 p.7: **078** DATA VOX GAIN, **079** DATA VOX DELAY
- FTdx10 p.12 / FTdx101 p.12: **03/04/06** DATA VOX GAIN `"0 ~ 100"` — and there
  is **no** DATA VOX DELAY item on either; `VD` covers it via VOX SELECT. On the
  FTdx101 there is also no mic-side VOX GAIN or VOX DELAY `EX` item at all, so
  `VG` and `VD` are the *only* way to reach them.
- FT-710: no DATA VOX items in the menu chart; `VD`'s note says the one command
  covers both.

### 7.3 `EX` frame differs by generation — three incompatible addressing schemes

- **FT-950 / FTdx1200 / FTdx3000 / FT-991A**: `EX<nnn><param>;`, one 3-digit menu
  number. FT-991A `"P1 : 001 - 153"` (p.7); FTdx1200 `"P1 : 001-196"` (p.7).
- **FT-891**: `EX<nnnn><param>;`, a **4-digit** number, `"P1 : 0101 - 1803"` (p.7).
- **FT-710 / FTdx10 / FTdx101 / FTX-1**: three 2-digit groups,
  `EX<gg><gg><gg><param>;`. FTdx101 `"P1 : 01 - 05  P2 : 01 - 07  P3 : 01 - 23"` (p.9).

Relevant only if the `EX` policy is ever revisited — but it means "the EX item
number" is not a single concept across this backend.

---

## 8. Transmit input / modulation source selection

**Confirmed: no model in this registry has a two-letter CAT command for it.** It
is an `EX` menu item on all twelve, and it is **per mode** on every one of them —
SSB, AM, FM and DATA each have their own selector. So §3 above stands: the `EX`
policy (`docs/DESIGN.md:360-364`, `:1249-1252`, `TestNeverWritesKeyerMemory`) is
what closes this route, not a gap in what has been read.

| Model | Items (mode: `EX` address) | Printed values |
|---|---|---|
| FT-891 (p.8) | SSB **1105**, AM **0605**, FM **0901**, DATA **0809** MIC SELECT / DATA IN SELECT | `"0: MIC   1: REAR"` |
| FT-991A (p.8) | SSB **106**+**109**, AM **045**+**048**, FM **074**+**077**, DATA **070**+**072** | MIC SELECT `"0: MIC  1: REAR"`; PORT SELECT `"0: DATA  1: USB"` on 048/109 but `"1: DATA  2: USB"` on 072/077 — **inconsistently coded in the manual** |
| FTdx1200 (pp.7-8) | SSB **103**, AM **055**, FM **086** MIC SEL | `"0: FRONT   1: DATA"` — no USB option on this radio |
| FTdx3000 (p.7) | SSB **103**, AM **052**, FM **085** MIC SEL; DATA **074** DATA IN SELECT | `"0: FRONT  1: DATA  2: USB"`; DATA IN SELECT `"0: DATA  1: USB"` |
| FTdx101 (p.10) | SSB **01/01/11**+**01/01/12**, AM **01/02/11**+**01/02/13**, FM **01/03/10**+**01/03/12**, DATA **01/04/13**+**01/04/14** | MOD SOURCE `"0: MIC  1: REAR"`; REAR SELECT `"0: DATA  1: USB"` — consistently coded |
| FT-710 (pp.10-12) | SSB **01/01/14**, AM **01/02/14**, FM **01/03/13**, DATA **01/04/14** MOD SOURCE; plus **06/0x/15** per preset | `"0: MIC  1: USB  2: REAR (RTTY/Data Jack)  3: AUTO"` — one item, four sources, and an AUTO |

*Not read*: FT-950, FTdx5000, FTdx9000, FTdx10, FTX-1. For the FT-950 the `EX`
table I did open covers items 073–118 and contains no MIC SELECT, so if it has
one it is numbered below 073.

### 8.1 The fact that matters most for `State.MicGain`

`internal/radio/types.go:641-661` already warns that `MicGain` is "the gain on
whatever input the radio is currently taking transmit audio from". These manuals
show that is **worse than the warning implies on the newer radios**: `MG` and the
USB/rear gains are *different settings*, not one setting applied to a routed
input.

- **FT-710** (pp.10-12): alongside `MOD SOURCE` there are separate `USB MOD GAIN`
  and `REAR MOD GAIN` items, both `"000 - 100"`, 3 digits, **per mode**. So on an
  FT-710 running USB digital modes, `MG` moves a control that is not in circuit.
- **FTdx1200** p.8 / **FTdx3000** p.7: `DATA MIC GAIN`, `"MCVR/FIX(0 ~ 100)
  (P2 = 1000: MCVR, 0000 ~ 0100: FIX(0 ~ 100))"`, 4 digits — a separate DATA-mode
  gain with its own "follow the front-panel knob" value.
- **FT-891** p.9: eight separate per-mode gains — `1607 SSB MIC GAIN`,
  `1608 AM MIC GAIN`, `1609 FM MIC GAIN`, `1610 DATA MIC GAIN`, and
  `1611 SSB DATA GAIN`, `1612 AM DATA GAIN`, `1613 FM DATA GAIN`,
  `1614 DATA DATA GAIN`, all `"0 - 100"`.

Which of those `MG` actually writes is **not stated in any of these manuals**. If
`docs/features.md` or the OpenAPI prose is ever expanded, this is the sharp
version of the caveat: on a Yaesu it is not merely that remoses does not know
which connector the gain belongs to — the radio holds several gains at once and
the manual does not say which one `MG` reaches.

### 8.2 Adjacent transmit-audio commands deliberately not implemented

- **`AO  AMC OUTPUT LEVEL`** — `A O P1 P1 P1 ;`, `"001-100"` (FT-710 p.6,
  FTdx101 p.6, FTX-1 p.6). Index only on FTdx10 (p.5). **Absent** from FT-891,
  FT-991A, FTdx1200, FTdx3000 (checked in each index). On the FTdx101 there is an
  `EX` item **03/03/01 PROC TYPE** `"0: COMP  1: AMC"` (p.12) that decides whether
  the compressor or the AMC is in circuit, which conditions what `PL` and `AO`
  each do. The FTX-1's TX AUDIO block (p.12) has no PROC TYPE item.
- **`ML  MONITOR LEVEL`** — present on all twelve. Two frames: `M L P1 P2 P2 P2 ;`
  with `"P1 0: MONI ON/OFF, 1: MONI Level"` on almost everything, but plain
  `M L P1 P1 P1 ;` `"000 - 255"` on the FTdx9000 (p.6).
- The **parametric mic EQ** (`PRMTRC EQ1/2/3 FREQ|LEVEL|BWTH`) and the
  **processor EQ** (`P PRMTRC EQ…`) are `EX`-only on every model — e.g. FTdx10
  p.12 `03/03/02`–`03/03/19`, FT-950 p.8 items 091–108, FTdx5000 p.9 items
  158–169. Nine `EX` writes per band, three bands. Out of reach by policy and
  probably not worth reaching.

---

## 9. Incidental discrepancies found in the manuals — NOT part of this phase

These are outside the transmit audio chain and I did not touch them. Each is a
place where the current `internal/rig/backend/yaesu/` code disagrees with a
manual I had open. They want checking before anyone relies on them.

1. **`NL` full scale is not 000-010 everywhere.** `noise.go` uses
   `nbLevelMax = 10`, which matches the FTdx10 (p.17, `"P2  000 - 010"`) and the
   FTX-1 (p.21, `"001 - 010: (NB Level)"`). But the **FT-950 prints `"P2  000 -
   255"`** (p.12) and so does the **FTdx9000** (p.7). On those two, remoses can
   only reach the bottom 4% of the blanker threshold.
2. **The FTdx9000's `BP` has no sub-command parameter.** It is
   `B P P1 P1 P1 ;` with `"000: Manual NOTCH "OFF" / 001 - 300: NOTCH Frequency
   (x10 Hz)"` (p.3) — one parameter carrying both, where every other model has
   the `P2` 0/1 selector `noise.go` assumes. `BP00<nnn>;` and `BP01<nnn>;` are
   both malformed there. (Note also `001 - 300`, not `001 - 320`.)
3. **The FTdx9000's command list is much smaller than the profile assumes**
   (p.2). It has **no `AI` row — and no AI column in the table at all**, unlike
   every other manual here; no `RM`, no `PS`, no `EX`, no `RA`, no `NA`, no `ID`,
   no `MS`, no `RS`, no `BI`, no `KR`, no `KP`. The profile currently sets
   `HasAI: true` and `Caps` reports `PowerMeter`, `SWRMeter`, `ALCMeter` and
   `PowerSwitch` true family-wide. If `AI` really is absent, that radio is
   poll-only and `Init` is writing a command it does not have.
4. **`AN` is not FTdx101-only.** `model.go` says "No other command list read for
   this backend has an AN row, the FTdx9000's included." All four of these have
   one: **FT-950** p.4 (`"P2 1: ANT "1", 2: ANT "2""`), **FTdx5000** p.4 (four
   antennas plus `"5: ANT "RX""` and `"P4  0: ANT "RX" "OFF", 1: ANT "RX" "ON""`),
   **FTdx9000** p.3 (`"1: Antenna "1"…4: Antenna "4", 5: Antenna "RX""`), and
   FTdx101. The FTX-1 and FT-891 indexes genuinely have none.
5. **A receive antenna selector does exist**, which `SetRXAntenna` currently
   refuses on every model saying no command list has one: the FTdx5000's `AN` P4
   is exactly an ANT RX on/off (p.4), and the FTdx9000's `AN` P2 value 5 is
   Antenna "RX" (p.3).
6. **`NB` has a third value on two models.** FT-950 p.12 and FTdx9000 p.7 both
   print `"2: Noise Blanker (Wide) "ON""`. `SetNoiseBlanker` rejects anything
   above 1 and `decodeNB` ignores a 2, so a rig in wide-blanker mode reads back
   as having no blanker at all.
7. **The FTX-1 has no `NB` and no `NR` command.** Its index (p.5) lists `NL`
   (NOISE BLANKER LEVEL, whose `000` *is* OFF) and `RL` (NOISE REDUCTION (DNR)
   LEVEL), with no separate switches. `noiseReads()` sends it `NB0;` and `NR0;`
   every slow poll, which would be two per-command timeouts per tick.
8. The FTdx5000's `PA` has **four** values, `"0: IPO 1, 1: AMP 1, 2: AMP 2, 3:
   IPO 2"` (p.14), where the profile models `Preamp: 2` plus IPO.

Items 3 and 7 are the two with a running cost (a timeout every poll tick);
items 1, 2 and 6 are wrong-reading bugs of the kind §5.4 of DESIGN.md is about.
