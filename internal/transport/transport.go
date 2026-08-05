// Package transport abstracts the byte pipe to a radio.
//
// Serial ports are the only implementation that matters today, but the rigctld
// backend speaks TCP, so the session layer is written against this interface
// rather than against a serial port directly.
package transport

import (
	"context"
	"errors"
	"io"
)

// ErrDisconnected is returned by a Transport whose underlying device has gone
// away — typically a USB serial adapter that was unplugged, or a rig that was
// switched off. The session treats it as a signal to close and start the
// reconnect loop rather than as a transient read error.
var ErrDisconnected = errors.New("transport: disconnected")

// Transport is a bidirectional byte pipe to one radio.
//
// Read is called only from the session's reader goroutine. Write may be called
// concurrently with Read and must be safe against it; implementations serialise
// writes internally.
type Transport interface {
	io.ReadWriteCloser
}

// ControlLines is implemented by transports with RS-232 modem control outputs.
//
// remoses uses these for two things: hardware PTT, and CW keying on rigs with
// no usable CAT CW buffer. Implementations must serialise these against Write,
// because on a shared port both touch the same file descriptor.
type ControlLines interface {
	SetDTR(bool) error
	SetRTS(bool) error
}

// Dialer opens a transport. A failed dial is retried by the session's
// supervisor with backoff, so implementations should not retry internally.
type Dialer interface {
	// Dial opens the device. The returned Transport is owned by the caller.
	Dial(ctx context.Context) (Transport, error)
	// Describe returns a short human-readable target, for logs and errors —
	// e.g. "/dev/tty.usbmodem14201 @115200" or "127.0.0.1:4532".
	Describe() string
}
