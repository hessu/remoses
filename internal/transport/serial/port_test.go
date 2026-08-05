package serial

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hessu/remoses/internal/transport"
	bugst "go.bug.st/serial"
)

// codedTestError stands in for the driver's *serial.PortError, whose code field
// is unexported and therefore not constructible from a test.
type codedTestError struct{ code bugst.PortErrorCode }

func (e codedTestError) Error() string             { return fmt.Sprintf("port error %d", e.code) }
func (e codedTestError) Code() bugst.PortErrorCode { return e.code }

func TestDisconnected(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "closed file", err: os.ErrClosed, want: true},
		{name: "wrapped closed file", err: fmt.Errorf("read: %w", os.ErrClosed), want: true},
		{name: "ENXIO", err: syscall.ENXIO, want: true},
		{name: "EIO", err: syscall.EIO, want: true},
		{name: "ENODEV", err: syscall.ENODEV, want: true},
		{name: "EBADF", err: syscall.EBADF, want: true},
		{name: "wrapped errno", err: fmt.Errorf("write: %w", syscall.EIO), want: true},
		{name: "driver PortClosed", err: codedTestError{bugst.PortClosed}, want: true},
		{name: "driver InvalidSerialPort", err: codedTestError{bugst.InvalidSerialPort}, want: true},
		{name: "wrapped driver PortClosed", err: fmt.Errorf("read: %w", codedTestError{bugst.PortClosed}), want: true},
		{name: "already ErrDisconnected", err: transport.ErrDisconnected, want: true},
		{name: "driver PortBusy is not device loss", err: codedTestError{bugst.PortBusy}, want: false},
		{name: "permission denied is not device loss", err: codedTestError{bugst.PermissionDenied}, want: false},
		{name: "EAGAIN is transient", err: syscall.EAGAIN, want: false},
		{name: "EINTR is transient", err: syscall.EINTR, want: false},
		{name: "unrelated error", err: errors.New("boom"), want: false},
		// The message says "closed" but nothing structural does, which is
		// exactly the case string matching would get wrong.
		{name: "text mentioning closed port", err: errors.New("port has been closed"), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := disconnected(tc.err); got != tc.want {
				t.Fatalf("disconnected(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDeviceErr(t *testing.T) {
	t.Run("device loss carries the sentinel and the cause", func(t *testing.T) {
		err := deviceErr("/dev/ttyUSB0", "read", syscall.ENXIO)
		if !errors.Is(err, transport.ErrDisconnected) {
			t.Fatalf("errors.Is(%v, ErrDisconnected) = false", err)
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("underlying errno lost: %v", err)
		}
		if got := err.Error(); !strings.Contains(got, "/dev/ttyUSB0") || !strings.Contains(got, "read") {
			t.Fatalf("error %q does not name the device and operation", got)
		}
	})

	t.Run("transient errors stay transient", func(t *testing.T) {
		err := deviceErr("/dev/ttyUSB0", "write", syscall.EAGAIN)
		if errors.Is(err, transport.ErrDisconnected) {
			t.Fatalf("EAGAIN must not be reported as a disconnect: %v", err)
		}
		if !errors.Is(err, syscall.EAGAIN) {
			t.Fatalf("underlying errno lost: %v", err)
		}
	})

	t.Run("nil stays nil", func(t *testing.T) {
		if err := deviceErr("/dev/ttyUSB0", "set DTR", nil); err != nil {
			t.Fatalf("deviceErr(nil) = %v", err)
		}
	})
}

// fakeDriver is a bugst.Port that needs no hardware. It records concurrency so
// that the serialisation contract can be asserted directly.
type fakeDriver struct {
	mu      sync.Mutex
	reads   []readResult
	writes  []int // bytes accepted per Write call; missing entries mean "all"
	written []byte
	lines   []string
	closed  bool

	busy      atomic.Int32
	overlap   atomic.Bool
	readDelay time.Duration
}

type readResult struct {
	data []byte
	err  error
}

func (f *fakeDriver) enter() func() {
	if f.busy.Add(1) > 1 {
		f.overlap.Store(true)
	}
	return func() { f.busy.Add(-1) }
}

func (f *fakeDriver) Read(p []byte) (int, error) {
	defer f.enter()()
	if f.readDelay > 0 {
		time.Sleep(f.readDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reads) == 0 {
		return 0, codedTestError{bugst.PortClosed}
	}
	r := f.reads[0]
	f.reads = f.reads[1:]
	n := copy(p, r.data)
	return n, r.err
}

func (f *fakeDriver) Write(p []byte) (int, error) {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(p)
	if len(f.writes) > 0 {
		n = f.writes[0]
		f.writes = f.writes[1:]
		if n > len(p) {
			n = len(p)
		}
	}
	f.written = append(f.written, p[:n]...)
	return n, nil
}

func (f *fakeDriver) SetDTR(v bool) error {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf("dtr=%v", v))
	return nil
}

func (f *fakeDriver) SetRTS(v bool) error {
	defer f.enter()()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, fmt.Sprintf("rts=%v", v))
	return nil
}

func (f *fakeDriver) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeDriver) SetMode(*bugst.Mode) error          { return nil }
func (f *fakeDriver) Drain() error                       { return nil }
func (f *fakeDriver) ResetInputBuffer() error            { return nil }
func (f *fakeDriver) ResetOutputBuffer() error           { return nil }
func (f *fakeDriver) SetReadTimeout(time.Duration) error { return nil }
func (f *fakeDriver) Break(time.Duration) error          { return nil }
func (f *fakeDriver) GetModemStatusBits() (*bugst.ModemStatusBits, error) {
	return &bugst.ModemStatusBits{}, nil
}

func TestPortReadAbsorbsTimeouts(t *testing.T) {
	f := &fakeDriver{reads: []readResult{
		{}, // (0, nil): read timeout
		{},
		{data: []byte("FA00014074000;")},
	}}
	p := newPort("/dev/fake", f)

	buf := make([]byte, 64)
	n, err := p.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := string(buf[:n]); got != "FA00014074000;" {
		t.Fatalf("Read = %q", got)
	}
}

// TestPortReadIsBufioSafe is the reason timeouts are absorbed: bufio gives up
// with ErrNoProgress after a run of empty non-error reads.
func TestPortReadIsBufioSafe(t *testing.T) {
	reads := make([]readResult, 0, 200)
	for i := 0; i < 200; i++ {
		reads = append(reads, readResult{})
	}
	reads = append(reads, readResult{data: []byte(";")})
	p := newPort("/dev/fake", &fakeDriver{reads: reads})

	b, err := bufio.NewReader(p).ReadByte()
	if err != nil {
		t.Fatalf("ReadByte: %v", err)
	}
	if b != ';' {
		t.Fatalf("ReadByte = %q", b)
	}
}

func TestPortReadDisconnect(t *testing.T) {
	p := newPort("/dev/fake", &fakeDriver{reads: []readResult{
		{},
		{err: codedTestError{bugst.PortClosed}},
	}})
	_, err := p.Read(make([]byte, 8))
	if !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Read error = %v, want ErrDisconnected", err)
	}
}

func TestPortReadKeepsDataWithError(t *testing.T) {
	p := newPort("/dev/fake", &fakeDriver{reads: []readResult{
		{data: []byte("ab"), err: codedTestError{bugst.PortClosed}},
	}})
	n, err := p.Read(make([]byte, 8))
	if n != 2 {
		t.Fatalf("Read n = %d, want 2 bytes returned alongside the error", n)
	}
	if !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Read error = %v, want ErrDisconnected", err)
	}
}

func TestPortOperationsAfterClose(t *testing.T) {
	f := &fakeDriver{reads: []readResult{{data: []byte("x")}}}
	p := newPort("/dev/fake", f)
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close must be a no-op, got %v", err)
	}
	if !f.closed {
		t.Fatal("driver was not closed")
	}

	if _, err := p.Read(make([]byte, 8)); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Read after Close = %v, want ErrDisconnected", err)
	}
	if _, err := p.Write([]byte("FA;")); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("Write after Close = %v, want ErrDisconnected", err)
	}
	if err := p.SetDTR(true); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("SetDTR after Close = %v, want ErrDisconnected", err)
	}
	if err := p.SetRTS(true); !errors.Is(err, transport.ErrDisconnected) {
		t.Fatalf("SetRTS after Close = %v, want ErrDisconnected", err)
	}
	if len(f.written) != 0 {
		t.Fatalf("Write after Close reached the driver: %q", f.written)
	}
}

func TestPortWriteCompletesShortWrites(t *testing.T) {
	f := &fakeDriver{writes: []int{1, 2}}
	p := newPort("/dev/fake", f)

	n, err := p.Write([]byte("FA;"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 3 {
		t.Fatalf("Write = %d, want 3", n)
	}
	if string(f.written) != "FA;" {
		t.Fatalf("driver saw %q, want %q", f.written, "FA;")
	}
}

func TestPortWriteRefusesToSpin(t *testing.T) {
	// A driver that keeps accepting nothing must produce an error rather than
	// an endless loop in the session's command path.
	f := &fakeDriver{writes: []int{0}}
	p := newPort("/dev/fake", f)
	if _, err := p.Write([]byte("FA;")); err == nil {
		t.Fatal("Write = nil error, want failure when the driver accepts no bytes")
	}
}

func TestPortSetLines(t *testing.T) {
	f := &fakeDriver{}
	p := newPort("/dev/fake", f)
	if err := p.SetDTR(true); err != nil {
		t.Fatalf("SetDTR: %v", err)
	}
	if err := p.SetRTS(false); err != nil {
		t.Fatalf("SetRTS: %v", err)
	}
	if len(f.lines) != 2 || f.lines[0] != "dtr=true" || f.lines[1] != "rts=false" {
		t.Fatalf("driver saw %v", f.lines)
	}
}

// TestPortSerialisesWritesAndLines models the shared-port case from DESIGN.md
// §11.2: the keyer flipping a control line while the CAT writer is busy.
func TestPortSerialisesWritesAndLines(t *testing.T) {
	f := &fakeDriver{}
	p := newPort("/dev/fake", f)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := p.Write([]byte("FA;")); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := p.SetDTR(j%2 == 0); err != nil {
					t.Errorf("SetDTR: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if f.overlap.Load() {
		t.Fatal("writes and control-line changes overlapped on the device")
	}
}

// TestPortReadDoesNotBlockWrites is the reason Read runs unguarded: a reader
// parked on a quiet radio must not hold the port against the keyer.
func TestPortReadDoesNotBlockWrites(t *testing.T) {
	reads := make([]readResult, 0, 64)
	for i := 0; i < 64; i++ {
		reads = append(reads, readResult{})
	}
	f := &fakeDriver{reads: reads, readDelay: time.Millisecond}
	p := newPort("/dev/fake", f)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.Read(make([]byte, 8)) //nolint:errcheck // ends when the fake runs out
	}()

	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 20; i++ {
		if err := p.SetRTS(i%2 == 0); err != nil {
			t.Fatalf("SetRTS while a read was in flight: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("control-line changes were blocked by an in-flight read")
		}
	}
	<-done
}

var _ io.ReadWriteCloser = (*Port)(nil)
