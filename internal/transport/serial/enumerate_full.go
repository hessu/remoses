//go:build !darwin || cgo

package serial

import (
	"errors"
	"fmt"

	bugst "go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// enumerationNote is empty in this build: descriptors are available, so a failed
// match is the operator's configuration and not a missing capability.
const enumerationNote = ""

// List enumerates serial ports together with their USB descriptors.
//
// No active-probe filters are passed to the driver. Probing opens the device to
// read its string descriptors, which can disturb a rig that is already talking
// to something else; the cost is that PortInfo.Product usually comes back empty.
// VID, PID and serial number come from the OS without probing, and those are
// what matching relies on.
func List() ([]PortInfo, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		// Some platforms compile the enumerator but cannot implement it. A list
		// of bare paths still lets a device-path configuration work.
		var coded codedError
		if errors.As(err, &coded) && coded.Code() == bugst.FunctionNotImplemented {
			return listNames()
		}
		return nil, fmt.Errorf("serial: enumerating ports: %w", err)
	}
	out := make([]PortInfo, 0, len(details))
	for _, d := range details {
		if d == nil {
			continue
		}
		out = append(out, PortInfo{
			Name:         d.Name,
			IsUSB:        d.IsUSB,
			VID:          d.VID,
			PID:          d.PID,
			SerialNumber: d.SerialNumber,
			Product:      d.Product,
		})
	}
	return out, nil
}
