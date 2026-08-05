// Package serial implements transport.Transport over a locally attached serial
// port.
//
// Two things here are less obvious than they look. Devices are addressed by USB
// descriptor rather than by path, because /dev/ttyUSB0 and /dev/tty.usbmodem*
// are not stable across a replug and a reconnect must find the same radio it
// lost (DESIGN.md §6). And one port is shared by several writers: CAT traffic
// and, on rigs keyed from a modem control line, the CW keyer both drive a single
// file descriptor, so writes and line changes are serialised while reads
// deliberately are not (DESIGN.md §11.2).
package serial

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	bugst "go.bug.st/serial"
)

// PortInfo describes one enumerated serial port.
//
// Product is filled in only by builds that can read USB descriptors, and even
// then only when active USB probing is enabled; remoses leaves probing off,
// because probing opens the device and can disturb a rig that is already in use.
// Treat Product as decoration for logs, never as a matching key.
type PortInfo struct {
	Name         string
	IsUSB        bool
	VID          string // as reported by the OS: hex, case not guaranteed
	PID          string
	SerialNumber string
	Product      string
}

// String renders a port the way an operator needs to see it in a diagnostic:
// path first, then whatever identifying descriptors this platform gave us.
func (p PortInfo) String() string {
	if !p.IsUSB && p.VID == "" && p.PID == "" {
		return p.Name
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s [vid=%s pid=%s", p.Name, orUnknown(p.VID), orUnknown(p.PID))
	if p.SerialNumber != "" {
		b.WriteString(" serial=" + p.SerialNumber)
	}
	if p.Product != "" {
		b.WriteString(" " + strconv.Quote(p.Product))
	}
	b.WriteString("]")
	return b.String()
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// listNames is the descriptor-free fallback: it reports device paths and nothing
// else. Used on platforms where detailed enumeration is not implemented, and as
// the whole of List in the cgo-free macOS build.
func listNames() ([]PortInfo, error) {
	names, err := bugst.GetPortsList()
	if err != nil {
		return nil, fmt.Errorf("serial: enumerating ports: %w", err)
	}
	out := make([]PortInfo, 0, len(names))
	for _, n := range names {
		out = append(out, PortInfo{Name: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
