# Research notes

**Working notes, not documentation.** These are transcriptions and open
questions gathered while reading the manufacturer CAT references, kept because
the reading is expensive and the answers are needed again by later work. They
are not written for an operator and they are not maintained in step with the
code — `docs/features.md` and `docs/DESIGN.md` are.

Each file cites its source document and page for every claim, and says plainly
where a fact could not be established. Where a note contradicts the code, the
code is what ships and the note is a lead to follow, not an authority.

| File | Covers |
|---|---|
| [tx-audio-icom.md](tx-audio-icom.md) | CI-V: mic gain `14 0B`, compressor `16 44`, compressor level `14 0E`; VOX `16 46`; antenna command `12`; Set-mode connector items |
| [tx-audio-kenwood.md](tx-audio-kenwood.md) | `MG`, `PR`/`PR0`, `PL`; VOX `VX`/`VG`/`VD` per model; `MS` transmission audio route |
| [tx-audio-yaesu.md](tx-audio-yaesu.md) | `MG`, `PR`, `PL` per model; VOX and anti-VOX `AV`; `EX` connector items |

They were written during the transmit-audio phase (mic gain and speech
processor) and deliberately reach past it, because the manuals were open: the
VOX, Data VOX and transmit-input material is for later phases.

## Findings that are about existing code, not about this phase

Reading the references turned up several places where the shipped per-model
tables disagree with the manuals. **None of these have been acted on.** They are
listed in the per-family files with citations, and the notable ones are:

- **`caps.antennas: 0` is wrong on six Icoms.** The IC-7610, IC-7760, IC-7700,
  IC-7850, IC-9100 and IC-7600 have CI-V command `12`. The code's stated reason
  — that an Icom's antenna is only a per-band memory — does not hold for these.
  It is intricate enough to need its own change: the socket is the sub-command
  while the data byte is that socket's RX-ANT flag, the IC-7600 has no
  sub-command at all, and the flag's meaning is conditioned by a Set-mode item.
- **The IC-706 and IC-706MKII have no command `16`**, yet `mkiiFamily` claims
  their noise blanker, preamp and attenuator from the MKIIG's table.
- **The IC-910H has a command `14`** and an RF gain remoses does not offer.
- **Nine wrong Icom `POScale` values** — only the IC-7610 and IC-9700 are right.
- **Yaesu:** the FTdx9000's command list has no `AI`, `RM`, `PS`, `RA` or `NA`
  row though the profile sets `HasAI: true`; the FTX-1 has no `NB` or `NR`
  though `noiseReads()` polls both; `NL` is 000-255 on the FT-950 and FTdx9000
  rather than 000-010; and `AN` exists on four more models than `model.go`
  claims, including a real RX-antenna selector that `SetRXAntenna` currently
  refuses as nonexistent.
