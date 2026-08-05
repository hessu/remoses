package tcp

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/transport"
)

func TestNewDialerRejectsBadAddresses(t *testing.T) {
	for _, addr := range []string{"", "no-port", "1.2.3.4"} {
		if _, err := NewDialer(addr); err == nil {
			t.Errorf("NewDialer(%q) = nil error, want a complaint about host:port", addr)
		}
	}
	if _, err := NewDialer("127.0.0.1:4532"); err != nil {
		t.Errorf("NewDialer(valid): %v", err)
	}
}

// listen starts a server that echoes what it is given, and returns its address
// plus a func that drops the connection.
func listen(t *testing.T) (addr string, hangup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	conns := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		conns <- c
		_, _ = io.Copy(c, c)
	}()

	return ln.Addr().String(), func() {
		select {
		case c := <-conns:
			_ = c.Close()
		case <-time.After(time.Second):
			t.Error("no connection to hang up")
		}
	}
}

func TestRoundTrip(t *testing.T) {
	addr, _ := listen(t)
	d, err := NewDialer(addr)
	if err != nil {
		t.Fatal(err)
	}
	if d.Describe() == "" {
		t.Error("Describe is empty; it appears in operator-facing logs")
	}

	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	want := "+f\n"
	if _, err := c.Write([]byte(want)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if string(buf) != want {
		t.Errorf("read %q, want %q", buf, want)
	}
}

// A peer that goes away must look like an unplugged cable, so the session
// supervisor redials instead of treating it as a transient read error.
func TestPeerHangupIsDisconnected(t *testing.T) {
	addr, hangup := listen(t)
	d, _ := NewDialer(addr)
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("priming read: %v", err)
	}

	hangup()
	if _, err := io.ReadFull(c, buf); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("read after hangup: %v, want ErrDisconnected", err)
	}
}

func TestRefusedConnectionIsDisconnected(t *testing.T) {
	// Bind then close, so the port is almost certainly free and refusing.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	d, _ := NewDialer(addr)
	if _, err := d.Dial(context.Background()); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Dial to a refusing port: %v, want ErrDisconnected", err)
	}
}

func TestLocalCloseIsDisconnected(t *testing.T) {
	addr, _ := listen(t)
	d, _ := NewDialer(addr)
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, transport.ErrDisconnected) {
		t.Errorf("read after close: %v, want ErrDisconnected", err)
	}
}

// A socket has no modem control lines, so serial keying cannot work over
// rigctld and the CW layer must take the CAT path.
func TestNoControlLines(t *testing.T) {
	addr, _ := listen(t)
	d, _ := NewDialer(addr)
	c, err := d.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.(transport.ControlLines); ok {
		t.Error("tcp conn claims modem control lines")
	}
}
