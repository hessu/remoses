# Kenwood backend — notes from the transmit-audio phase

What the repository's own material does and does not corroborate, for the phases
after this one. Everything below is cited to a file and line in
`/Users/hessu/src/cw/remoses`. Where the repo is silent it says so; nothing here
is filled in from recollection.

> **Read §7 onwards first.** Sections 1-6 were written when the manufacturer
> references were not available and record only what the *repository* contained.
> The references in `/Users/hessu/src/cw/remoses-manuals/` have since been read.
> **§1 and §5 are superseded** — `MG`, `PR`/`PR0` and `PL` are all transcribed and
> implemented, and the two-value `PL` question is settled (§7). §2's list of
> "silences a VOX phase has to close" is largely closed by §8. §4's claim that
> transmit-input selection is a menu item on every Kenwood is **wrong for the
> TS-890S and TS-990S**, which have a live `MS` command (§10). §3 is superseded by
> §9. Everything in §7-§10 is cited to a document and a *printed* page number (the
> `– N –` in the page footer, not the PDF page index).

---

## 1. The headline for this phase: MG / PR / PL are not in this repository

**Searched exhaustively:** all of `docs/` (including `DESIGN.md`, read end to end
by hand as well), all of `internal/`, `api/openapi.yaml`, `README.md`,
`remoses.example.yaml`, and git history.

| Command | Occurrences attributed to Kenwood | Verdict |
|---|---|---|
| `MG` (mic gain) | **none** | not corroborated |
| `PR` (speech processor) | **none** — zero occurrences as a command token anywhere | not corroborated |
| `PL` (processor level) | **none** | not corroborated |

The only literal `MG`/`PL` in the tree is **Yaesu**, `docs/DESIGN.md:1176`,
inside `### 5.6 Yaesu models`:

> **`PC` is not always watts.** On the FTdx5000 and FTdx9000 the manuals give the
> same three digits as `000`–`255` where every other model gives a watt range, in
> the same manuals that give `MG`, `PL`, `SQ` and `RG` as `000`–`255` too — this
> is those radios' level scale, not a typo.

That is **FTdx5000 / FTdx9000 only**, three digits `000`–`255`, one value for
`PL` and not two — and it is a Yaesu fact. `docs/DESIGN.md:409-415` is explicit
that Yaesu is not a `kenwood` model despite the shared framing, and that two
command letters mean *different things* between the two dialects (`TX;` keys a
Kenwood and is the PTT read on a Yaesu; `KY` streams text on one and plays a
stored keyer memory on the other). So this line is evidence about Yaesu and
about nothing else.

`PR` false positives to ignore when grepping: `PRE 1`/`PRE 2` in
`internal/rig/backend/kenwood/model.go:99` and `:548` (the **preamplifier**,
command `PA`), plus `LGPL`, `prosign`, `processing`.

### The two-value `PL` question

The task asked whether `PL` carries an input level and an output level in one
frame on some models of this family. **This repository says nothing about it,
in either direction.** No doc, comment, test or commit describes a `PL` of any
shape on a Kenwood. So there was no basis on which to *decide* how to map two
fields onto the single 0-100 % `proc_level` the API publishes, and no mapping
was written.

### The one Kenwood-scale claim that does exist, and why it is not enough

`internal/rig/backend/backend.go:366-368` and `internal/radio/types.go:652-653`
both assert "0-255 on Icom, 0-100 on Kenwood" for the transmit gain. That is an
unsourced assertion in an interface comment added in the same uncommitted change
that created `TXAudioController`; it names no model, no command, no digit count
and no reference. It is not a transcription and was not treated as one.

### What was implemented instead

`internal/rig/backend/kenwood/txaudio.go` — `TXAudioController` implemented for
the family, all three methods refusing with `backend.ErrUnsupported`, naming the
model and stating the reason. `Caps()` in `kenwood.go` sets `MicGainControl`,
`ProcControl` and `ProcLevelControl` to `false` explicitly, with the reasoning
next to them. No read path, no decoders, no command constants — see §5.

---

## 2. VOX: `VX`, and how it collides with break-in per model

### Corroborated

**`docs/DESIGN.md:1012-1017`** is the load-bearing table. Its last column is the
`VX` fence, not a `BI` column:

| Model | Command | Values | Semi vs full | `VX` in CW |
|---|---|---|---|---|
| TS-990S | `BI` | `0` off, `1` semi, `2` full | direct | fenced off: "cannot be set in modes other than SSB/AM/FM" |
| TS-890S | `BI` | `0` off, `1` on | `SD` delay: `0000` ms **is** full | fenced off, same wording |
| TS-590S / SG | **`VX`** | `0` off, `1` on | `SD` delay, as above | **is** break-in, stated outright |
| TS-480 | **`VX`**, inferred | `0` off, `1` on | `SD` delay, as above | not documented either way |

So the fence runs **both ways** and each direction is transcribed:

- **TS-890S / TS-990S** — `VX` (i.e. VOX) "cannot be set in modes other than
  SSB/AM/FM". Repeated at `internal/rig/backend/kenwood/model.go:503-506`. These
  two have a real `BI`, so VOX and break-in are separate commands there and a
  VOX phase is comparatively safe on them — subject to §2's unknowns.
- **TS-890S / TS-990S**, the mirror: `BI` "can only be performed in CW mode" and
  returns 0 in any other — `internal/rig/backend/kenwood/kenwood.go:601-604`.
- **TS-590S / SG** — the trap, quoted from the *TS-590S/TS-590SG PC Control
  Command Reference Guide* (B5A-0316-00, identified at `docs/DESIGN.md:420-421`):
  "when transmitting the VX command in CW mode, the Break-in function is set and
  read, rather than the VOX function". Appears at `docs/DESIGN.md:1019-1021`,
  `docs/kenwood.md:124-129`, `internal/rig/backend/kenwood/model.go:214-219` and
  paraphrased at `remoses.example.yaml:189-192`.
- **TS-480** — its reference documents `VX` as "the VOX function status" and is
  silent about CW; `internal/rig/backend/kenwood/model.go:496-498`. remoses's use
  of `VX` for break-in there is an **explicit inference**, argued at
  `model.go:496-510` and `docs/DESIGN.md:1027-1038`, and listed as an open
  hardware question at `docs/DESIGN.md:1064-1066`. This is the only place in the
  tree where a Kenwood reference is quoted describing `VX` as VOX in its own
  right — and it is the model whose CW behaviour is unknown.
- **`generic`** abstains from break-in entirely, deliberately, because this
  dialect is also spoken by Elecraft and modern Yaesu whose `VX` semantics the
  repo says it knows nothing about — `model.go:465-483`, `docs/kenwood.md:135-141`.

**Parameter shape:** one digit, `VX0` / `VX1`. From the table above plus the
frames actually emitted, `internal/rig/backend/kenwood/breakin.go:128` and the
expectations in `breakin_test.go:27-41`.

### Not corroborated — silence a VOX phase has to close first

- **`VG` (VOX gain): zero occurrences anywhere in the repo.** No parameter shape,
  no model attribution, nothing in git history.
- **`VD` (VOX delay): zero occurrences anywhere in the repo.** Same.
- **Whether a VOX write in CW clobbers the stored break-in setting on a TS-590,
  or a break-in write clobbers the stored VOX setting.** Completely silent, and
  this is the sharpest unknown: on that radio the two are one command, so if the
  radio keeps one register rather than two, setting VOX in SSB and then entering
  CW may report a break-in state nobody chose. The CW guard
  (`Session.EnsureCWWillTransmit`) consumes that value.
- **Whether `VX` is *readable* on the TS-890S/TS-990S in CW.** Their fence says
  "cannot be **set**" outside SSB/AM/FM; the repo never says whether a read is
  refused too.
- **Whether the TS-590's overload applies in CW-R, FSK/FSK-R or the data modes.**
  `isCW()` at `breakin.go:196-201` counts `ModeCW` and `ModeCWR` only; the
  comment there is remoses's own reasoning, not a quote. FSK is therefore treated
  as VOX territory today.
- **The read-answer width of `VX`.** The decoder takes `arg[0]` only
  (`codec.go:293`), so a longer VOX answer form would go unnoticed.

### Existing guards a VOX phase will collide with — do not weaken them

- `breakin.go:75-90` — `SetBreakIn` refuses outside CW on the `VX` style.
- `breakin.go:152-164` — `breakInRead()` returns `""` outside CW, so `VX;` is not
  polled there.
- `codec.go:280-297` — inbound `VX` frames are decoded on every model but
  **dropped when not in CW**; today a `VX` push in USB is silently discarded,
  which is exactly what a VOX feature would need to change.
- Tests that will fail if `VX` becomes writable outside CW:
  `breakin_test.go:124-133` and `:153-156`.
- Self-test tooling: `internal/selftest/steps.go:49-53` and `:570-574`.

**Nothing in this phase touched any of them.** `txaudio.go` states in its header
comment that VOX is transmit audio in an operator's mental model and is *not*
this control, and `TestTransmitAudioDoesNotReachForVX` asserts from this feature's
side that a `VX` frame outside CW still publishes no break-in and still completes
its transaction.

---

## 3. Data VOX

**Zero occurrences anywhere in the repo.** The concept is never named, for any
manufacturer. The nearest adjacent thing is Icom's DATA1/DATA2/DATA3, described
as differing *only* in which modulation input they use and explicitly not modelled
(`internal/rig/backend/civ/dualvfo.go:54-57`, `civ/civ.go:783-785`), and a list of
IC-706MKIIG sub-commands "including VOX, none of which remoses models"
(`civ/model.go:858-861`).

---

## 4. Transmit input / connector selection (mic vs USB vs ACC2)

### Corroborated

`docs/DESIGN.md:431`, the Kenwood column of the §5.2 table (TS-590S/SG common,
sourced to B5A-0316-00):

> | PTT | `1C 00` — `00`=RX, `01`=TX | `TX;` (`TX0`=SEND/mic, `TX1`=DATA SEND via ACC2/USB, `TX2`=TX tune; bare `TX;` means `TX0`) / `RX;` |

The code-side statement and the standing policy,
`internal/rig/backend/kenwood/kenwood.go:741-744`:

> TX; without a parameter means TX0, SEND, the microphone input. TX1 (DATA SEND,
> ACC2/USB) is not selected automatically even in Data mode: it changes which
> input modulates the transmitter, and a plain PTT flag carries no such intention.

`SetPTT` sends bare `TX;` / `RX;`; **remoses never sends `TX1` or `TX2`.** The
decoder treats any `TX` frame as PTT-on regardless of parameter
(`codec.go:343-350`), and `codec_test.go:305,309` exercises `TX0` and `TX2`
pushes.

*Beware:* every other `TX0`/`TX1` in the tree is **Yaesu**, where the meaning is
inverted — `TX;` read, `TX1;` key, `TX0;` unkey
(`internal/rig/backend/yaesu/yaesu.go:25-27`, `docs/DESIGN.md:1100`, called out
at `docs/DESIGN.md:412`).

### Not corroborated

- **There is no Kenwood `EX` extended-menu command anywhere in this repo.** `EX`
  appears only as Yaesu's menu command (`docs/DESIGN.md:364`, `:1250-1251`,
  `yaesu/model.go:403`).
- **No command or menu item that selects the modulation source is modelled for
  any family**, and that is a stated design position rather than a gap. Four
  places say the same thing: `internal/radio/types.go:644-650`,
  `internal/rig/backend/backend.go:356-361`, `internal/radio/types.go:1301-1303`,
  `api/openapi.yaml:984-990`. The summary: which connector feeds the modulator is
  a menu setting on all three families, remoses does not read it, so the field is
  "the transmit gain" and no more.
- Hamlib's equivalent split is noted at
  `internal/rig/backend/rigctld/rigctld.go:496-498` (a plain PTT flag does not
  carry which input modulates the transmitter).

### The policy a connector-selection phase has to argue against

`docs/DESIGN.md:347` — **"Rule: the operator's radio is not ours to reconfigure"**,
with the table of menu commands remoses declines to write at `:364`. If transmit
input selection turns out to live in a persistent menu item on Kenwood, that rule
applies and the honest outcome is to document the gap rather than write it. Note
also that VOX, transmit-input selection and mic gain are **not** in §15 Open items
(`docs/DESIGN.md:2719-2736`) — none of this is on the roadmap in that document.

---

## 5. Why no read/poll/decode path was added in this phase

The task asked for one "following how the noise fields flow". It was not written,
and the reason is the same one that gated the capabilities false:

- **There is nothing to poll.** `pollSlow` builds its read list from profile
  capabilities; with all three false there is no request to add, and adding a
  request means committing a command letter to the wire.
- **A decoder is the same guess in reverse.** `decodeNL`/`decodeRL` turn an
  answer into a percentage with `scaleFrom(n, lo, hi)`, and `lo`/`hi` are
  transcribed per command (and, for `RL`, per *reducer* — see `nrLevelRange`).
  Without a transcribed range there is no percentage to compute, only a plausible
  one.
- **The unhandled case is already correct.** `Decode`'s switch falls through to
  `KeyUnsolicited` with an empty patch for any frame it does not recognise
  (`codec.go:139-146`), so an `MG`/`PR`/`PL` push arriving on AI2 is ignored
  quietly rather than mis-decoded. That is the right treatment for a frame this
  backend cannot read.

**What turning one on will take**, in order: a row in that model's own PC Control
Command Reference Guide giving the command letters, the parameter width, the
range and — for a level — how many fields the frame carries and in what order;
then a `Model` field, a request constant, a reply key, a decoder scaled against
that range, a line in `pollSlow`, and the `Caps` flag. `TestTXAudioCapsAreFalseOnEveryModel`
is placed so that it has to be edited deliberately at that point.

**Three per-model hazards that are already documented and will apply:**

- **Scales differ across the family.** `RG` counts `000-100` on a TS-480 and
  `000-255` on everything after it — `frontend.go:29-30`, carried as
  `Model.RFGainMax` (`model.go:113-117`, `:390`, `:514-516`). Assume a transmit
  gain can do the same.
- **The set width and the answer width differ.** `PA` takes one digit and answers
  two; `RA` takes two and answers four on the TS-480/TS-590 — `frontend.go:19-22`.
  Parsing an answer with the setter's width reads padding as the value.
- **The TS-990S puts a band selector in front of these commands**
  (`Model.Banded`, `frontend.go:42-47`), and the TS-890S/TS-990S are a different
  dialect generally (`model.go:942-966` region of DESIGN, `docs/kenwood.md:41-49`).
  A transmit-audio command transcribed from a TS-590 reference must not be
  assumed to have the same shape there.

---

## 6. Files changed in this phase

- `internal/rig/backend/kenwood/txaudio.go` — new. `TXAudioController` for the
  family; all three methods refuse with `backend.ErrUnsupported`.
- `internal/rig/backend/kenwood/kenwood.go` — `Caps()` gains
  `MicGainControl` / `ProcControl` / `ProcLevelControl`, all `false`, with the
  reasoning beside them.
- `internal/rig/backend/kenwood/txaudio_test.go` — new. Refusal on every model
  with nothing on the wire; caps false on every model; no `MG`/`PR`/`PL` request
  emitted across `Init` and both poll tiers on any model; and the `VX` guard
  checked from this feature's side.

Nothing outside `internal/rig/backend/kenwood/` was touched.
`go build`, `go vet` and `go test` on that package alone are clean.

---
---

# Part two — transcribed from the manufacturers' references

Documents, all in `/Users/hessu/src/cw/remoses-manuals/`:

| Short name used below | File | Identity |
|---|---|---|
| **590/r3** | `ts590_g_pc_command_en_rev3.pdf` | TS-590S / TS-590SG PC Control Command Reference Guide, JVCKENWOOD, dated January/30/2019 |
| **590/older** | `ts_590_g_pc_command_e.pdf` | earlier revision of the same document |
| **480** | `ts_480_pc.pdf` | TS-480 PC Control Command |
| **890** | `ts890_pc_command_en_rev1.pdf` | TS-890S PC Control Command Reference Guide, rev 1 |
| **990** | `ts990_pc_command_en_rev2.pdf` | TS-990S PC Control Command Reference Guide, rev 2 |

Page numbers are the **printed** page (the `– N –` footer). Quotation marks mean
the manual's exact words.

---

## 7. Transmit audio — `MG`, `PR`/`PR0`, `PL` (this phase; supersedes §1 and §5)

### `MG` — microphone gain

| Model | Frame | Digits | Range | Notes in the reference |
|---|---|---|---|---|
| TS-480 | `MGnnn;` / read `MG;` | 3 | `000 (min.) ~ 100 (max.)` | none | 480 p13 |
| TS-590S/SG | `MGnnn;` / read `MG;` | 3 | `000 ~ 100 (in steps of 1)`, "an entered value of 101 or higher results in 100 being entered" | "Sets and reads the microphone gain **for SSB and AM mode**." "Configure the FM mode microphone gain using the menu. (Refer to the EX command.)" | 590/r3 p17 |
| TS-890S | `MGnnn;` / read `MG;` | 3 | `000 ~ 100` | "Configure the FM mode microphone gain using the Advanced menu." | 890 p46 |
| TS-990S | `MGnnn;` / read `MG;` | 3 | **`000 ~ 255 (in steps of 1)`** | "Configure the FM mode microphone gain using the menu." | 990 p39 |

**Cross-check:** 590/older p17 gives the same frame and range but does **not**
carry the "for SSB and AM mode" sentence or the FM redirection — rev 3 added
both. The FM gain is a menu item: **TS-590S menu 047**, **TS-590SG menu 053**,
"Mic gain for FM", three positions `1 / 2 / 3` (590/r3 p8 and p10).

**No mode or state restriction is stated for `MG` in any of the four**, so it is
polled and written in every mode. What comes back in FM is a real stored setting
that is simply not the one modulating the radio at that moment.

**The TS-990S's 255 is not a typo** — it matches that radio's `AG` (000-255),
`RG` (000-255) and `ML` (000-255), where the TS-590's `ML` is 001-020. This is
the `RG` trap on a second control.

### `PR` / `PR0` — speech processor switch

| Model | Frame | Values | Cite |
|---|---|---|---|
| TS-480 | `PRn;` read `PR;` answer `PRn;` | `0` OFF, `1` ON | 480 p16 |
| TS-590S/SG | `PRn;` read `PR;` answer `PRn;` | `0` OFF, `1` ON | 590/r3 p21, 590/older p21 |
| TS-890S | **`PR0n;`** read `PR0;` answer `PR0n;` | `0` OFF, `1` ON | 890 p54 |
| TS-990S | **`PR0n;`** read `PR0;` answer `PR0n;` | `0` OFF, `1` ON | 990 p46 |

**The collision, and it is the sharpest one in this family:** on the TS-890S and
TS-990S there is also **`PR1` = "Speech Processor Effect Type", `0: Soft, 1: Hard`**
(890 p54, 990 p47). So `PR1;` — the frame that switches a TS-590's processor
**on** — is on a TS-890S the well-formed **read** of an unrelated setting. The
radio answers `PR1n;`, nothing is rejected, and the processor stays off. The
TS-590 has the same setting as a **menu item** instead: "Effective change of
Speech Processor", `SOFT / HARD`, **TS-590S menu 029**, **TS-590SG menu 035**
(590/r3 p8, p10).

No mode restriction is stated for the switch on any model.

### `PL` — speech processor input/output level: **it carries TWO values**

Settled, on all four references:

```
Set     P L  P1 P1 P1  P2 P2 P2  ;
Read    P L  ;
Answer  P L  P1 P1 P1  P2 P2 P2  ;
        P1 = Input level      P2 = Output level
```

| Model | Range of each field | Cite |
|---|---|---|
| TS-480 | `000 (min.) ~ 100 (max.)` | 480 p15 |
| TS-590S/SG | `000 (minimum) ~ 100 (maximum)`, "entering a value of 101 or higher results in 100 being entered" | 590/r3 p21, 590/older p21 |
| TS-890S | `000 (minimum) ~ 100 (maximum)` | 890 p53 |
| TS-990S | **`000 (minimum) ~ 255 (maximum)`** | 990 p46 |

**Decision recorded in `txaudio.go`:** `proc_level` maps to **P1, the input
level** — it is the field that decides how hard the processor works, it is the
one that does nothing while the processor is off, and the output level is a
make-up gain into the modulator, which is the job `MicGain` already has. **P2 is
preserved, not discarded:** the set form carries both fields, so `SetProcLevel`
reads `PL;` first and writes the radio's own output level back unchanged, and
refuses rather than invent one if nothing readable came back.

### The state-refusal question, answered in the negative

The two neighbouring level commands state their refusal outright — `NL` "When NB
is set to OFF, an error occurs" (590/r3 p20) and `RL` "When the Noise Reduction
setting is OFF, an error occurs" (590/r3 p23). **`PL` carries no such note in any
of the four references, and neither does `MG`.** So neither is gated on the
processor's state and both are polled unconditionally. That is a transcribed
negative, not an untested assumption — but it is a negative, so if a bench
TS-590S ever answers `?;` to `PL;` with the processor off, this is the sentence
that was wrong.

---

## 8. VOX — `VX`, `VG`/`VG0`, `VD`, anti-VOX (`VG1`)

### The two dialects are structurally different, not just spelt differently

**TS-480 and TS-590S/SG — one global VOX gain and one global delay:**

| Cmd | Frame | Parameter | Cite |
|---|---|---|---|
| `VX` | `VXn;` read `VX;` answer `VXn;` | `0` VOX OFF, `1` VOX ON | 480 p23, 590/r3 p32 |
| `VG` | `VGnnn;` read `VG;` | VOX gain `000 ~ 009` (590: "in steps of 1"; "an entered value of 010 or higher results in 09 being entered") | 480 p22, 590/r3 p30 |
| `VD` | `VDnnnn;` read `VD;` | VOX delay `0000 ~ 3000 ms (in steps of 150)`; 590 adds "3001 or higher results in 3000", "a value that does not match the 150 ms step will be rounded down" | 480 p22, 590/r3 p30 |
| anti-VOX | **does not exist** | — | see below |

**TS-890S and TS-990S — every VOX parameter is PER INPUT:**

| Cmd | Frame | P1 (input type) | P2 | Cite |
|---|---|---|---|---|
| `VX` | `VXn;` read `VX;` | — | `0` OFF, `1` ON | 890 p68, 990 p62 |
| `VG0` | `VG0 P1 P2P2P2;` read `VG0 P1;` | `0` Microphone, `1` ACC 2, `2` USB-Audio, `3` LAN (890) / Optical (990) | VOX gain `000 ~ 020`; `999` = restore initial (set only) | 890 p68 |
| `VG0` (990) | as above | `0` Microphone, `1` ACC2, `2` USB-Audio, `3` Optical | **`000 ~ 255` for the Microphone input, `000 ~ 020` for any other input**; `999` initial | 990 p61 |
| `VG1` | `VG1 P1 P2P2P2;` read `VG1 P1;` | same input list | **Anti-VOX Level** `000 ~ 020`; `999` initial | 890 p68, 990 p61 |
| `VD` | `VD P1 P2P2P2;` read `VD P1;` | same input list | VOX delay `000 ~ 020`, **value × 150 ms**; `999` initial | 890 p67, 990 p61 |

Consequences a VOX phase has to design around:

- The API can publish **one** `vox_gain` / `vox_delay`, but these two radios hold
  **four of each**. Either the phase picks an input and says so (the way
  `proc_level` picks `PL`'s input field), or it reads `MS` (§10) to find which
  input is actually feeding the modulator.
- **`VG0`'s range on the TS-990S depends on `P1`** — 255 for the mic, 20 for
  everything else. A single per-model ceiling will not do; it has to be per input.
- The delay units are not comparable across the family: **4 digits of
  milliseconds** on the TS-480/TS-590, **3 digits of 150 ms steps** on the
  TS-890S/TS-990S. Same command letters, different quantity.
- **Manual typo to expect:** 990 p61 prints `VG1`'s *Answer* row as `V G 0 P1 …`.
  The command block is titled `VG1` / "Anti-VOX Level" and the Set and Read rows
  say `VG1`, so the answer is `VG1`; the `0` is an error in the document.

### Anti-VOX per model — a definite negative for the older pair

- **TS-890S / TS-990S:** `VG1`, above.
- **TS-480:** no anti-VOX command. Its `V` section is `VD, VG, VR, VV, VX` and
  nothing else (480 pp22-23).
- **TS-590S / TS-590SG:** **no anti-VOX command and no anti-VOX menu item.** The
  `V` section is `VD, VG, VR, VS0-VS4, VV, VX` (590/r3 pp30-32), and both complete
  EX menu lists were read end to end — TS-590S menus `000-087` (590/r3 pp7-9) and
  TS-590SG menus `000-099` (590/r3 pp9-11). Neither contains an anti-VOX entry.

### The mode fences, both directions, now fully transcribed

- **TS-590S/SG `VX`:** "When transmitting the VX command in CW mode, the Break-in
  function is set and read, rather than the VOX function." (590/r3 p32) — the
  quote the repo already carries, confirmed in the newest revision.
- **TS-480 `VX`:** "Sets or reads the VOX function status", `0`/`1`, **and not one
  word about CW** (480 p23). The repo's inference at `model.go` is exactly as
  described there: silence, not denial.
- **TS-890S `VX`:** "This command cannot be set in modes other than SSB/FM/AM."
  "When reading this command in a mode other than SSB/FM/AM, 0 is returned."
  (890 p68)
- **TS-990S `VX`:** "This command cannot be set in modes other than SSB/AM/FM."
  "When reading this command in a mode other than SSB/AM/FM, '0' is returned."
  (990 p62)
- **TS-890S `BI`:** `BIn;`, `0` Break-in OFF, `1` Break-in ON. "Settings can only
  be performed in CW mode." "'0' is respond when reading in any mode other than
  CW mode." (890 p6)
- **TS-990S `BI`:** `BIn;`, `0` Break-in Off, `1` Semi Break-in, `2` Full Break-in.
  "Settings can only be performed in CW mode." "'0' is returned when reading in
  any mode other than CW mode." (990 p6)

This **closes** §2's open item "whether `VX` is readable on the TS-890S/TS-990S in
CW": it is readable everywhere and simply **answers `0`** outside SSB/AM/FM. That
is a trap of its own — a `VX;` read in CW on those two returns a confident `0`
that means "not applicable", not "VOX is off".

### **The big open question: does writing break-in in CW clobber the stored VOX setting on a TS-590?**

**The references do not answer it.** 590/r3 p32 gives `VX` one parameter table
and one sentence about CW, and says nothing about how many registers sit behind
it. 590/older says the same. So this remains **unsettled from documents**.

**Transcribed evidence that they are TWO registers, not one:**

1. **The delays are already two separate commands with incompatible ranges.**
   `VD` is the VOX delay, `0000 ~ 3000 ms in steps of 150` (590/r3 p30). `SD` is
   "the CW break-in time delay", `0000` = full break-in, `0050 ~ 1000 ms in steps
   of 50` (590/r3 p24). A firmware that kept one shared on/off flag while keeping
   two independent, differently-quantised delays would be a strange design.
2. **The next generation split the same behaviour into two commands and kept both
   readable, each masked by mode.** `BI` answers `0` outside CW; `VX` answers `0`
   outside SSB/AM/FM (citations above). That is precisely how two separate
   registers behave under a mode mask — and the TS-590's single `VX` reads as the
   *union* of those two commands under one name, dispatched by mode.
3. No sentence anywhere in the family suggests a shared register.

**Verdict for now: two registers is the strong reading, but it is an inference,
and `breakin.go`'s CW guard should not be rewritten on it.**

**A TS-590S is on the bench, and this is a one-minute experiment.** Run it before
the VOX phase starts:

```
MD2;  VX1;  VX;      -> in USB, switch VOX on and confirm VX1
MD3;  VX;            -> in CW: does it answer the BREAK-IN state, or 1?
                        (if break-in was off it should answer VX0 — that alone
                        shows the read is dispatched, not shared)
      VX1;  VX;      -> turn break-in on in CW, confirm VX1
      SD;            -> and confirm the delay command that goes with it
MD2;  VX;            -> back in USB: still VX1?  If yes, VOX survived a
                        break-in write and the two registers are separate.
                        If it now mirrors whatever was written in CW, they
                        are one register and the CW guard has a real problem.
      VX0;  MD3; VX; -> the mirror: switch VOX off in USB, is break-in still on?
```

Record the answer here. It is the last thing standing between remoses and a VOX
feature on the TS-590.

---

## 9. Data VOX per model (supersedes §3)

- **TS-480:** nothing. No Data VOX command and no menu route documented in this
  reference.
- **TS-590S:** **menu items only, via `EX`** (590/r3 p9):
  `069` DATA VOX `OFF/ON`; `070` DATA VOX delay `0, 5, 10, 15, 20, 25, 30, 35,
  40, 45 … up to 100 (steps of 5)`; `071` DATA VOX gain for USB audio input `0~9`;
  `072` DATA VOX gain for ACC2 terminal input `0~9`.
- **TS-590SG:** the same four, renumbered (590/r3 p11): `076` DATA VOX,
  `077` DATA VOX delay, `078` DATA VOX gain for USB audio input, `079` DATA VOX
  gain for ACC2 terminal input.
- **TS-890S / TS-990S:** **there is no separate Data VOX**, and that is not a gap
  — `VD`, `VG0` and `VG1` already take the input as `P1`, so "data VOX" there is
  the same command with `P1` = ACC 2 / USB-Audio / LAN (890) or Optical (990).
  See §8.

Note for the design position at `docs/DESIGN.md:347` ("the operator's radio is not
ours to reconfigure"): on the TS-590 pair, Data VOX is reachable only by writing a
**persistent menu item**, which is exactly what that rule declines to do. On the
TS-890S/TS-990S it is an ordinary command.

---

## 10. Transmit input / connector selection (corrects §4)

### `TX` selects the transmission *route*, on every model

- **TS-480** (480 p21): `TXn;` — `0` "Normal (SEND) transmission using MIC input",
  `1` "DTS transmission using ANI input", `2` "TX Tune transmission".
- **TS-590S/SG** (590/r3 p29): `TXn;` — `0` "SEND (normal transmission using the
  MIC input)", `1` "DATA SEND (ACC2/ USB input)", `2` "TX Tune". "If no P1
  parameter is specified, it is set to 0 (SEND)."
- **TS-890S** (890 p66): `0` "Transmission by SEND/PTT", `1` "Transmission by DATA
  SEND/PKS", `2` "TX TUNE".
- **TS-990S** (990 p60): `0` "SEND/PTT (normal transmission using the MIC input)",
  `1` "DATA SEND/PKS (ACC2/ USB input)", `2` "TX TUNE".

This confirms the repo's existing statement and policy at
`kenwood.go` (`SetPTT` sends bare `TX;` = `TX0`, never `TX1`/`TX2`).

### **§4 is wrong for the newest two: `MS` is a live connector-selection command**

**TS-890S — `MS`, "Transmission Audio Entry Sound Generator Selection"** (890 p48):

```
Set     M S P1 P2 P3 ;      Read  M S P1 ;      Answer  M S P1 P2 P3 ;
P1 (Transmission means)  0: SEND/PTT      1: DATA SEND (PF)
P2 (Front)               0: OFF           1: Microphone
P3 (Rear)                0: OFF   1: ACC 2   2: USB Audio   3: LAN
• "P2 and P3 cannot be OFF at the same time."
• "When both P2 and P3 are set to 9 with the setting command, P1 is set to the
   initial value."
```

**TS-990S — `MS`, same title** (990 p41):

```
Set     M S P1 P2 P3 P4 P5 ;    Read  M S P1 ;   Answer  M S P1 P2 P3 P4 P5 ;
P1  0: SS signal of SEND/PTT/REMOTE/ACC2 connector
    1: PKS signal of DATA SEND/ACC2 connector
P2  Microphone input transmission   OFF / ON
P3  ACC2 input transmission         OFF / ON
P4  USB-Audio input transmission    OFF / ON
P5  Optical input transmission      OFF / ON
• "ACC2 input (P3) and USB-Audio input (P4) cannot both be ON at the same time."
• "P2 ~ P5 cannot all be OFF at the same time."
• "The transmission sound source is appointed by P1 if P2 ~ P5 are all set to 9
   and they are returned to their initial settings."
```

So on these two radios remoses **could** read which connector feeds the
modulator, per transmission route, with an ordinary read command. That directly
contradicts the assumption behind four places in the tree — `internal/radio/types.go`,
`internal/rig/backend/backend.go`, `internal/radio/types.go` (Caps) and
`api/openapi.yaml` — which all say the connector "is a menu setting on all three
families" and is therefore not modelled, and which is the stated reason `mic_gain`
is documented as "the transmit gain and no more".

**That reason still holds for the TS-480 and TS-590**, where it genuinely is a
menu item:

- **TS-590S menu 063** "DATA modulation line", `ACC2 / USB` (590/r3 p9).
- **TS-590SG menu 069** "DATA modulation line", `ACC2 / USB`, **plus TS-590SG-only
  menu 070** "Audio source of SEND/PTT transmission for data mode", `FRONT / REAR`
  (590/r3 pp10-11).
- Related level menus, if a phase ever wants them: TS-590S `064/065` USB audio
  input/output level `0~9`, `066/067` ACC2 terminal AF input/output level `0~9`;
  the TS-590SG numbers are `071/072` and `073/074` (590/r3 pp9, 11).
- **TS-480:** nothing at all; the route is chosen by `TX0` vs `TX1` and the
  connector is a hardware matter.

**What a connector-selection phase should therefore say:** the capability is
per model, exactly like everything else in `Model` — a real, readable, writable
command on the TS-890S and TS-990S, and a persistent menu item (so, under
`docs/DESIGN.md:347`, not remoses's to write) on the TS-480 and TS-590. The blanket
sentence in the four API-facing comments needs qualifying before either of the
newer radios is claimed.

---

## 11. Files changed in the transmit-audio phase (final)

All inside `internal/rig/backend/kenwood/`:

- `txaudio.go` — rewritten. The transcription above as a file comment, `reqMG` /
  `reqPL`, `txAudioReads`, the three setters (`SetMicGain`, `SetProc`,
  `SetProcLevel`) and the three decoders (`decodeMG`, `decodePR`, `decodePL`).
- `model.go` — `Model.MicGainMax`, `Model.ProcCmd`, `Model.ProcLevelMax`;
  `procReq` / `procSet` / `procSwitchChar`; values in `md()`, `om()` and the
  TS-990S entry; a note in the `generic` entry saying why these three are claimed
  where break-in is not.
- `codec.go` — `keyMG` / `keyPR` / `keyPL` and their decode cases.
- `kenwood.go` — `Rig.procOut`; `Caps` computes the three from the profile;
  `pollSlow` appends `txAudioReads()`.
- `conn_test.go` — default `MG` / `PR` / `PR0` / `PL` answers for the shared harness.
- `txaudio_test.go` — rewritten: per-model scaling, the `PR` vs `PR0` spelling,
  output-level preservation, the decoders, caps following the profile, the
  "claimed ⇒ polled, unclaimed ⇒ never on the wire" invariant, and the `VX` fence
  from this side.
- `kenwood_test.go`, `model_test.go` — slow-poll expectations extended with
  `MG;`, `PR;`/`PR0;` and `PL;`.

`go build`, `go vet`, `go test` and `gofmt -l` on that package alone are clean.
