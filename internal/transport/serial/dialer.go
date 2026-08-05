package serial

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/config"
	"github.com/hessu/remoses/internal/transport"
	bugst "go.bug.st/serial"
)

// defaultReadTimeout bounds a single driver-level read. It is not a protocol
// timeout — Port.Read hides expiries from callers — it only sets how long a read
// already in flight can outlive a Close on platforms where closing the handle
// does not interrupt it.
const defaultReadTimeout = 200 * time.Millisecond

// Dialer opens one configured serial port. It does not retry: the session's
// supervisor owns backoff.
type Dialer struct {
	// ReadTimeout overrides defaultReadTimeout. Zero means the default.
	ReadTimeout time.Duration

	device string
	match  *config.PortMatch
	mode   *bugst.Mode
}

var _ transport.Dialer = (*Dialer)(nil)

// NewDialer validates a port configuration up front, so that a bad baud rate or
// a misspelled parity fails at startup rather than at the first reconnect.
func NewDialer(p config.Port) (*Dialer, error) {
	mode, err := newMode(p)
	if err != nil {
		return nil, err
	}
	device := strings.TrimSpace(p.Device)
	if device == "" && p.Match == nil {
		return nil, fmt.Errorf("serial: port needs a device path or a vid/pid match")
	}
	d := &Dialer{ReadTimeout: defaultReadTimeout, device: device, mode: mode}
	if p.Match != nil {
		m := *p.Match
		if describeMatch(m) == "" {
			return nil, fmt.Errorf("serial: port match sets none of vid, pid or serial")
		}
		d.match = &m
	}
	return d, nil
}

// Dial resolves the configured port to a device path and opens it.
//
// The port is opened non-blocking by the driver, so ctx cannot be observed
// during the open itself; it is checked on either side, which is enough for a
// cancelled reconnect not to leave a port open.
func (d *Dialer) Dial(ctx context.Context) (transport.Transport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := d.resolve()
	if err != nil {
		return nil, err
	}
	p, err := bugst.Open(name, d.mode)
	if err != nil {
		return nil, deviceErr(name, "open", err)
	}
	timeout := d.ReadTimeout
	if timeout <= 0 {
		timeout = defaultReadTimeout
	}
	if err := p.SetReadTimeout(timeout); err != nil {
		p.Close()
		return nil, deviceErr(name, "set read timeout", err)
	}
	if err := ctx.Err(); err != nil {
		p.Close()
		return nil, err
	}
	return newPort(name, p), nil
}

// Describe names the dial target for logs, in the terms the operator configured
// it: by descriptor when matching, by path otherwise.
func (d *Dialer) Describe() string {
	target := d.device
	if d.match != nil {
		target = describeMatch(*d.match)
		if d.device != "" {
			target += " or " + d.device
		}
	}
	return fmt.Sprintf("%s @%d", target, d.mode.BaudRate)
}

// resolve turns the configuration into a device path.
//
// Matching wins when it succeeds, because the configured path is by definition
// stale after a replug. The path is the fallback for the cases matching cannot
// serve: a non-USB port, a build without descriptor enumeration, or an adapter
// whose descriptors the OS does not expose.
func (d *Dialer) resolve() (string, error) {
	if d.match == nil {
		return d.device, nil
	}
	ports, err := List()
	if err != nil {
		if d.device != "" {
			return d.device, nil
		}
		return "", fmt.Errorf("serial: looking for %s: %w", describeMatch(*d.match), err)
	}
	if p, ok := matchPort(ports, *d.match); ok {
		return p.Name, nil
	}
	if d.device != "" {
		return d.device, nil
	}
	return "", fmt.Errorf("serial: no port matches %s; saw %s%s",
		describeMatch(*d.match), describePorts(ports), enumerationNote)
}

// matchPort picks the port whose USB descriptors satisfy m.
//
// Only the fields the operator set are compared, so vid/pid alone identifies the
// single adapter of its kind on the machine while serial disambiguates two of
// them. When several ports still match — two identical adapters and no serial
// configured — the lowest device name wins, so that a restart lands on the same
// radio instead of following enumeration order.
func matchPort(ports []PortInfo, m config.PortMatch) (PortInfo, bool) {
	vid, pid := normHexID(m.VID), normHexID(m.PID)
	serial := strings.TrimSpace(m.Serial)
	if vid == "" && pid == "" && serial == "" {
		return PortInfo{}, false
	}
	var best PortInfo
	found := false
	for _, p := range ports {
		if vid != "" && normHexID(p.VID) != vid {
			continue
		}
		if pid != "" && normHexID(p.PID) != pid {
			continue
		}
		// Serial numbers are compared case-insensitively: adapters report them
		// in whatever case their EEPROM holds, and operators retype them.
		if serial != "" && !strings.EqualFold(strings.TrimSpace(p.SerialNumber), serial) {
			continue
		}
		if !found || p.Name < best.Name {
			best, found = p, true
		}
	}
	return best, found
}

// normHexID canonicalises a USB vendor or product id for comparison. Config
// files are hand-written and carry "0x10C4", "10C4" or "10c4" for the same
// vendor, while each OS reports whichever case and zero-padding it prefers.
func normHexID(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return ""
	}
	if t := strings.TrimLeft(s, "0"); t != "" {
		return t
	}
	return "0"
}

// describeMatch renders the search criteria. It returns "" for a match that
// constrains nothing, which NewDialer rejects.
func describeMatch(m config.PortMatch) string {
	var parts []string
	if v := strings.TrimSpace(m.VID); v != "" {
		parts = append(parts, "vid="+v)
	}
	if v := strings.TrimSpace(m.PID); v != "" {
		parts = append(parts, "pid="+v)
	}
	if v := strings.TrimSpace(m.Serial); v != "" {
		parts = append(parts, "serial="+v)
	}
	return strings.Join(parts, " ")
}

// describePorts lists what was actually on the machine, so a wrong VID can be
// diagnosed from the log line alone.
func describePorts(ports []PortInfo) string {
	if len(ports) == 0 {
		return "no serial ports at all"
	}
	names := make([]string, 0, len(ports))
	for _, p := range ports {
		names = append(names, p.String())
	}
	return strings.Join(names, ", ")
}
