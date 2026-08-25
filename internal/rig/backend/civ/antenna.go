package civ

import (
	"context"
	"fmt"

	"github.com/hessu/remoses/internal/rig/backend"
)

// Command 12: which antenna socket the radio is using, and whether the separate
// receive-only input is switched in.
//
// This backend asserted for a long time that no Icom had an antenna selector at
// all — that the antenna here is a per-band MEMORY in the Set menu, so switching
// it would mean writing a stored setting, which remoses does not do. The
// memories are real: an IC-7610 keeps one per band range at 1A 05 02 76 through
// 02 87. They were never the only route. Six of the radios in the model table
// have a live command 12 as well, and the radio has both.
//
// # One frame, printed three ways
//
// The operands are the same everywhere:
//
//	12 <socket> [<RX-ANT flag>]
//
// with the socket counting from zero. What differs is which column each manual
// puts them in, and that is the whole reason this looked harder than it is:
//
//   - The IC-7610, IC-7760, IC-7700 and IC-7850 print the socket in the SUB-
//     COMMAND column and the flag in the data column — "12 00 00 or 01
//     Select/read ANT1 selection (00=RX ANT OFF, 01=RX ANT ON)", and the same
//     for 01 / ANT2 (IC-7610 CI-V Reference Guide p. 3; IC-7760 Ref. Guide p. 3;
//     IC-7700 printed p. 14-3; IC-7850 printed p. 18-3).
//   - The IC-7600 has no sub-command column at all and prints one two-byte data
//     field with four rows: "0000 Send/read ANT1 selection (RX ANT OFF)", "0001
//     ...(RX ANT ON)", "0100 ...ANT2...(RX ANT OFF)", "0101 ...(RX ANT ON)"
//     (printed p. 160). Byte for byte that is the same frame as above.
//   - The IC-9100 prints the socket and leaves the data column EMPTY: "12 00
//     Send/read ANT1 selection", "12 01 Send/read ANT2 selection" (printed
//     p. 184). One byte shorter, and no receive antenna to switch.
//
// So the socket is never the data byte, and a SetAntenna that wrote n there
// would set ANT1's receive-antenna flag instead of selecting socket n. That is
// what Model.Antennas, Model.RXAntenna and antennaFrame between them keep
// straight.
//
// # Reading it back
//
// The read is a bare 12, with neither operand. The IC-7300MK2's guide states the
// convention outright — "Commands with an asterisk (*) allow you to read or
// write setting values. To read the current settings, send the command without
// any subcommand or data" (p. 4) — and command 12 carries that asterisk on the
// IC-7610, IC-7760 and IC-7300MK2, the dagger that means the same on the
// IC-7850, and the words "Send/read" or "Select/read" in the other three tables.
//
// A bare 12 is also the ONLY safe read. Sending "12 00" to ask what ANT1's flag
// is would, on an IC-9100, be a complete set frame: select ANT1. A poll must
// never be one byte away from moving somebody's antenna.
//
// # Why the setters read before they write
//
// Both fields ride in one frame and there is no encoding for "leave the other
// one alone". Selecting a socket therefore has to carry the receive-antenna flag
// across, and switching the receive antenna has to carry the socket across, or
// each would silently reset the other. The round trip is what SetFilterWidth and
// SetFilterSlot already pay for the same reason; a setter can afford it, and
// this is the direction where being one slow-poll stale would put a wrong value
// into the radio rather than merely report one.
//
// # Two things the manuals condition, which remoses reports rather than resolves
//
// The flag's meaning depends on a Set-mode item. The IC-7610's footnote against
// command 12 reads: "If the Antenna Type is set to 'RX-I/O,' command '01 (RX ANT
// ON)' is invalid and '00 (RX ANT OFF)' is always returned" (p. 9), the item
// being 1A 05 02 75, "TYPE SET > RX-ANT Connectors". The IC-7760 prints the same
// footnote. remoses neither reads nor writes that item — it is persistent
// configuration — so on a radio wired that way a request to switch the receive
// antenna on is accepted and reads back off. The state is truthful; the
// explanation is in the radio's own menu.
//
// And the socket can move with no command sent. 1A 05 02 89 is "Send the Antenna
// selection mode ([ANT] SW) (00=OFF, 01=Manual, 02=Auto)" (IC-7610 p. 8), and in
// Auto the radio picks per band from those band memories. So state.antenna can
// change under a client that touched nothing but the frequency, which is a fact
// about the radio rather than a race here.

// antennaFrame builds a command 12 for this model.
//
// socket counts from zero, as the wire does. The flag byte is appended only
// where the model's table prints one: on the IC-9100 a trailing 00 would be a
// parameter its parser is not expecting, the same trap as the IC-703's data-mode
// command.
func (r *Rig) antennaFrame(socket int, rx bool) []byte {
	if !r.model.RXAntenna {
		return r.frame(cmdAntenna, byte(socket))
	}
	return r.frame(cmdAntenna, byte(socket), onOff(rx))
}

// antennaState reads command 12 back: the selected socket counting from 1, and
// that socket's receive-antenna flag.
//
// Either may be absent — the IC-9100 reports no flag and the IC-7300MK2 no
// socket — so the caller checks what it needs rather than this returning an
// error for a field the radio was never going to send.
func (r *Rig) antennaState(ctx context.Context, c backend.Conn) (socket int, rx bool, err error) {
	u, err := r.read(ctx, c, KeyAntenna, r.frame(cmdAntenna))
	if err != nil {
		return 0, false, err
	}
	if u.Patch.Antenna != nil {
		socket = *u.Patch.Antenna
	}
	if u.Patch.RXAntenna != nil {
		rx = *u.Patch.RXAntenna
	}
	return socket, rx, nil
}

// SetAntenna selects an antenna socket, counting from 1.
func (r *Rig) SetAntenna(ctx context.Context, c backend.Conn, n int) error {
	if r.model.Antennas == 0 {
		return fmt.Errorf("civ: the %s has no antenna selection command; on this radio "+
			"the antenna is a per-band memory in the Set menu, which remoses does not "+
			"write: %w", r.model.Label, backend.ErrUnsupported)
	}
	if n < 1 || n > r.model.Antennas {
		return fmt.Errorf("civ: the %s has antenna sockets 1 to %d, not %d: %w",
			r.model.Label, r.model.Antennas, n, backend.ErrUnsupported)
	}
	rx := false
	if r.model.RXAntenna {
		// The flag shares the frame, so it has to be read and carried across or
		// changing the socket would switch the receive antenna out with it.
		_, wasOn, err := r.antennaState(ctx, c)
		if err != nil {
			return err
		}
		rx = wasOn
		if n > r.model.RXAntennaSockets {
			// ANT4 on the IC-7700 and IC-7850, whose row prints "00" and "fix":
			// there is no receive antenna behind that socket to carry across.
			// Sending 01 would draw an NG on the antenna change the operator DID
			// ask for, so the flag goes off with the socket — which is what the
			// radio has anyway once ANT4 is selected.
			rx = false
		}
	}
	return r.set(ctx, c, "antenna", r.antennaFrame(n-1, rx))
}

// SetRXAntenna switches the separate receive-only input in or out.
func (r *Rig) SetRXAntenna(ctx context.Context, c backend.Conn, on bool) error {
	if !r.model.RXAntenna {
		return fmt.Errorf("civ: the %s has no receive antenna command: %w",
			r.model.Label, backend.ErrUnsupported)
	}
	// On a radio with sockets to choose between, the flag belongs to the one
	// currently selected and the frame carries both — so the socket is read and
	// written back unchanged. Where there is no selection (the IC-7300MK2) the
	// first byte is the fixed 00 its single row prints.
	socket := 0
	if r.model.Antennas > 0 {
		cur, _, err := r.antennaState(ctx, c)
		if err != nil {
			return err
		}
		if cur < 1 {
			return fmt.Errorf("civ: the %s did not report which antenna socket is "+
				"selected, and the receive antenna cannot be set without it",
				r.model.Label)
		}
		if on && cur > r.model.RXAntennaSockets {
			return fmt.Errorf("civ: the %s has no receive antenna on ANT%d; its table "+
				"fixes that socket's flag at RX ANT OFF, so select ANT1 to ANT%d "+
				"first: %w", r.model.Label, cur, r.model.RXAntennaSockets,
				backend.ErrUnsupported)
		}
		socket = cur - 1
	}
	return r.set(ctx, c, "receive antenna", r.antennaFrame(socket, on))
}

// decodeAntenna reads a command 12 answer.
//
// The key is set as soon as the model says this radio has the command, before
// the bytes are looked at, for the reason the attenuator decoder gives: an
// answer this backend cannot make sense of must still resolve the read that
// provoked it, or the poll fails and the failures eventually tear down a link to
// a radio that is answering perfectly well.
func (r *Rig) decodeAntenna(u *backend.Update, body []byte) {
	if r.model.Antennas == 0 && !r.model.RXAntenna {
		return
	}
	if len(body) < 1 {
		return
	}
	u.Key = KeyAntenna
	// The socket, where there is one to publish. A byte past the end of this
	// radio's sockets is a frame misread rather than a socket to report — and on
	// the IC-7300MK2 the same byte is a fixed sub-command with no socket in it.
	if n := int(body[0]) + 1; n <= r.model.Antennas {
		u.Patch.Antenna = &n
	}
	if r.model.RXAntenna && len(body) >= 2 {
		on := body[1] != 0x00
		u.Patch.RXAntenna = &on
	}
}

// antennaReads is this model's command 12 query for the slow poll, if it has one.
//
// A single bare read covers both fields, which is the one place this command is
// cheaper than the front end around it.
func (r *Rig) antennaReads() []request {
	if r.model.Antennas == 0 && !r.model.RXAntenna {
		return nil
	}
	return []request{{KeyAntenna, r.frame(cmdAntenna)}}
}
