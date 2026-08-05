//go:build darwin && !cgo

package serial

// enumerationNote explains a failed match in this build before the operator goes
// looking for a wrong VID that is not there.
const enumerationNote = " (this macOS build has no cgo, so USB descriptors are unavailable and vid/pid matching cannot succeed; configure port.device)"

// List enumerates serial ports by name only.
//
// Reading USB descriptors on macOS goes through IOKit, which the driver reaches
// through cgo; this build does not have it. Ports therefore come back as bare
// paths, VID/PID matching never succeeds, and a radio configured with a match
// must also carry a device path. Everything else — opening, reading, keying —
// works unchanged, which is the point of keeping this build alive.
func List() ([]PortInfo, error) {
	return listNames()
}
