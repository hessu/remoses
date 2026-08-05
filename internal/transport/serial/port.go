package serial

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/hessu/remoses/internal/transport"
	bugst "go.bug.st/serial"
)

// Port is one open serial device, shared by everything that talks to a radio:
// the session's reader, its CAT writer, and — when the rig is keyed from a modem
// control line — the CW keyer.
type Port struct {
	name string
	p    bugst.Port

	// mu serialises every operation that touches the file descriptor except
	// Read. Modem-control ioctls take microseconds, but a CAT write on a port
	// shared with the keyer can block for the duration of the frame and stretch
	// a CW element (DESIGN.md §11.2) — which is why a separate keying device is
	// strongly preferred, and why nothing slow is done while holding this.
	mu sync.Mutex

	// closed is set outside mu because Read must observe it without ever taking
	// the lock, and because Close must be able to mark the port dead even while
	// a write is in flight.
	closed atomic.Bool
}

var (
	_ transport.Transport    = (*Port)(nil)
	_ transport.ControlLines = (*Port)(nil)
)

func newPort(name string, p bugst.Port) *Port {
	return &Port{name: name, p: p}
}

// Name is the device path the port was opened on, which after a descriptor match
// is not the path in the config file.
func (pt *Port) Name() string { return pt.name }

// Read blocks until at least one byte arrives, the port is closed, or the device
// disappears.
//
// It runs without mu on purpose: it is called only from the session's single
// reader goroutine, and holding the write lock across a read that blocks until
// the radio says something would stall every CAT write and every keyer
// transition in the meantime.
//
// The driver reports a read timeout as (0, nil). That is legal for an io.Reader
// but hostile to bufio, which counts consecutive empty non-error reads and gives
// up with ErrNoProgress, so timeouts are absorbed here rather than passed on.
// They still earn their keep: each expiry is a chance to notice a Close that the
// platform did not deliver to a read already in flight.
func (pt *Port) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		if pt.closed.Load() {
			return 0, deviceErr(pt.name, "read", os.ErrClosed)
		}
		n, err := pt.p.Read(b)
		if err != nil {
			return n, deviceErr(pt.name, "read", err)
		}
		if n > 0 {
			return n, nil
		}
		// (0, nil): read timeout. Loop.
	}
}

// Write sends the whole buffer, serialised against the other writers.
//
// The driver opens the port in blocking mode, so a short write means the write
// was interrupted rather than that the buffer is full; the remainder is retried
// because a truncated CAT frame leaves the rig's parser mid-command, which is
// worse than the delay.
func (pt *Port) Write(b []byte) (int, error) {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.closed.Load() {
		return 0, deviceErr(pt.name, "write", os.ErrClosed)
	}
	written := 0
	for written < len(b) {
		n, err := pt.p.Write(b[written:])
		if n > 0 {
			written += n
		}
		if err != nil {
			return written, deviceErr(pt.name, "write", err)
		}
		if n == 0 {
			return written, deviceErr(pt.name, "write", errors.New("driver accepted no bytes"))
		}
	}
	return written, nil
}

// SetDTR asserts or releases Data Terminal Ready, used for PTT or CW keying.
func (pt *Port) SetDTR(v bool) error {
	return pt.setLine("set DTR", v, pt.p.SetDTR)
}

// SetRTS asserts or releases Request To Send, used for PTT or CW keying.
func (pt *Port) SetRTS(v bool) error {
	return pt.setLine("set RTS", v, pt.p.SetRTS)
}

func (pt *Port) setLine(op string, v bool, set func(bool) error) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if pt.closed.Load() {
		return deviceErr(pt.name, op, os.ErrClosed)
	}
	return deviceErr(pt.name, op, set(v))
}

// Close releases the device. It is idempotent, because both the session and its
// supervisor may reach for it when a radio goes away.
//
// The closed flag is raised before the lock is taken so that a writer blocked in
// the driver stops as soon as it returns, rather than issuing one more write
// against a device that is on its way out.
func (pt *Port) Close() error {
	already := pt.closed.Swap(true)
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if already {
		return nil
	}
	if err := pt.p.Close(); err != nil {
		return fmt.Errorf("serial: %s: close: %w", pt.name, err)
	}
	return nil
}

// codedError is what the driver's *serial.PortError satisfies. The interface is
// matched instead of the concrete type because PortError's code field is
// unexported, so no test outside the driver can build one — and because any
// error that reports a driver error code deserves the same reading.
type codedError interface {
	Code() bugst.PortErrorCode
}

// deviceErr annotates a driver error with the device and operation it came from
// and, when the error means the device itself is gone, tags it with
// transport.ErrDisconnected. The session branches on that sentinel to choose
// between reconnecting and treating the failure as transient, so it is the one
// piece of classification that must not be guesswork.
func deviceErr(name, op string, err error) error {
	if err == nil {
		return nil
	}
	if disconnected(err) {
		return fmt.Errorf("serial: %s: %s: %w: %w", name, op, transport.ErrDisconnected, err)
	}
	return fmt.Errorf("serial: %s: %s: %w", name, op, err)
}

// disconnected reports whether err means the device has gone away — a USB
// adapter unplugged, or a rig switched off — as opposed to a transient failure.
//
// It is matched structurally rather than by message text: the kernel returns
// some of these raw while the driver wraps others, and neither wording is
// stable. The driver's PortError does not implement Unwrap, so an errno it
// wrapped is invisible to errors.Is; its code is checked separately for that
// reason.
func disconnected(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, transport.ErrDisconnected),
		errors.Is(err, os.ErrClosed),
		errors.Is(err, syscall.ENXIO),  // macOS: descriptor of a yanked USB serial
		errors.Is(err, syscall.EIO),    // Linux: same, and a wedged FTDI
		errors.Is(err, syscall.ENODEV), // device node survived the device
		errors.Is(err, syscall.EBADF):  // raced a Close on another goroutine
		return true
	}
	var coded codedError
	if errors.As(err, &coded) {
		switch coded.Code() {
		case bugst.PortClosed, bugst.InvalidSerialPort:
			// PortClosed is also what the unix driver returns when a read sees
			// the zero-length readable state a disconnect leaves behind, so it
			// is the usual way an unplug is reported.
			return true
		}
	}
	return false
}
