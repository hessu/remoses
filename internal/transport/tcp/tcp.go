// Package tcp provides a TCP transport.
//
// It exists for the rigctld backend, which talks to a separate Hamlib daemon
// over a socket rather than to a serial port. The session layer is written
// against transport.Transport precisely so that this is a drop-in alternative
// and the supervisor's dial/backoff/reconnect logic applies unchanged.
package tcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hessu/remoses/internal/transport"
)

// Dialer opens TCP connections to a fixed address.
type Dialer struct {
	Address string
	// Timeout bounds a single connection attempt. The session supervisor owns
	// retry and backoff, so this only needs to stop one attempt hanging.
	Timeout time.Duration
}

// NewDialer returns a dialer for addr, which must be host:port.
func NewDialer(addr string) (*Dialer, error) {
	if addr == "" {
		return nil, errors.New("tcp: no address configured")
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("tcp: address %q must be host:port: %w", addr, err)
	}
	return &Dialer{Address: addr, Timeout: 5 * time.Second}, nil
}

func (d *Dialer) Describe() string { return "tcp " + d.Address }

func (d *Dialer) Dial(ctx context.Context) (transport.Transport, error) {
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var nd net.Dialer
	c, err := nd.DialContext(dctx, "tcp", d.Address)
	if err != nil {
		// A refused or unreachable daemon is the same situation as an unplugged
		// USB adapter: nothing to talk to, retry later. Reporting it as
		// ErrDisconnected keeps the supervisor's handling uniform.
		return nil, fmt.Errorf("tcp %s: %w: %w", d.Address, transport.ErrDisconnected, err)
	}
	if t, ok := c.(*net.TCPConn); ok {
		// rigctld transactions are small and latency matters for the poll loop,
		// so do not wait to coalesce them.
		_ = t.SetNoDelay(true)
		_ = t.SetKeepAlive(true)
		_ = t.SetKeepAlivePeriod(30 * time.Second)
	}
	return &conn{c: c}, nil
}

// conn adapts net.Conn to transport.Transport.
//
// It does not implement transport.ControlLines: a socket has no modem control
// lines, so serial keying is unavailable over rigctld and the CW layer must use
// the CAT path.
type conn struct {
	mu     sync.Mutex
	c      net.Conn
	closed bool
}

func (t *conn) Read(p []byte) (int, error) {
	n, err := t.c.Read(p)
	if err != nil {
		return n, t.classify(err)
	}
	return n, nil
}

func (t *conn) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n, err := t.c.Write(p)
	if err != nil {
		return n, t.classify(err)
	}
	return n, nil
}

func (t *conn) Close() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	return t.c.Close()
}

// classify maps a peer that has gone away onto ErrDisconnected, so the session
// redials instead of treating it as a transient read error.
func (t *conn) classify(err error) error {
	if err == nil {
		return nil
	}
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()

	if closed || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("%w: %w", transport.ErrDisconnected, err)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return err // a deadline, not a dead peer
	}
	return fmt.Errorf("%w: %w", transport.ErrDisconnected, err)
}
