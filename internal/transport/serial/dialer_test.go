package serial

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
)

var testPorts = []PortInfo{
	{Name: "/dev/ttyUSB0", IsUSB: true, VID: "10c4", PID: "ea60", SerialNumber: "IC7610-001"},
	{Name: "/dev/ttyUSB1", IsUSB: true, VID: "0403", PID: "6001", SerialNumber: "A700eXyZ"},
	{Name: "/dev/ttyUSB2", IsUSB: true, VID: "10C4", PID: "EA60", SerialNumber: "TS590SG-002"},
	{Name: "/dev/ttyS0"},
}

func TestMatchPort(t *testing.T) {
	tests := []struct {
		name  string
		match config.PortMatch
		ports []PortInfo
		want  string // "" means no match expected
	}{
		{
			name:  "vid and pid, case-insensitive against config",
			match: config.PortMatch{VID: "0403", PID: "6001"},
			ports: testPorts,
			want:  "/dev/ttyUSB1",
		},
		{
			name:  "config upper case matches lower case OS report",
			match: config.PortMatch{VID: "0403", PID: "6001", Serial: "a700exyz"},
			ports: testPorts,
			want:  "/dev/ttyUSB1",
		},
		{
			name:  "0x prefix tolerated",
			match: config.PortMatch{VID: "0x0403", PID: "0X6001"},
			ports: testPorts,
			want:  "/dev/ttyUSB1",
		},
		{
			name:  "leading zeros tolerated",
			match: config.PortMatch{VID: "403", PID: "6001"},
			ports: testPorts,
			want:  "/dev/ttyUSB1",
		},
		{
			name:  "serial picks between identical adapters",
			match: config.PortMatch{VID: "10C4", PID: "EA60", Serial: "TS590SG-002"},
			ports: testPorts,
			want:  "/dev/ttyUSB2",
		},
		{
			name:  "serial is case-insensitive and space tolerant",
			match: config.PortMatch{VID: "10C4", PID: "EA60", Serial: " ts590sg-002 "},
			ports: testPorts,
			want:  "/dev/ttyUSB2",
		},
		{
			name:  "ambiguous match resolves to lowest device name",
			match: config.PortMatch{VID: "10C4", PID: "EA60"},
			ports: testPorts,
			want:  "/dev/ttyUSB0",
		},
		{
			name:  "ambiguous match is independent of enumeration order",
			match: config.PortMatch{VID: "10C4", PID: "EA60"},
			ports: []PortInfo{testPorts[2], testPorts[1], testPorts[0]},
			want:  "/dev/ttyUSB0",
		},
		{
			name:  "serial alone is enough",
			match: config.PortMatch{Serial: "IC7610-001"},
			ports: testPorts,
			want:  "/dev/ttyUSB0",
		},
		{
			name:  "vid alone is enough when unique",
			match: config.PortMatch{VID: "0403"},
			ports: testPorts,
			want:  "/dev/ttyUSB1",
		},
		{
			name:  "wrong vid does not match",
			match: config.PortMatch{VID: "1a86", PID: "7523"},
			ports: testPorts,
			want:  "",
		},
		{
			name:  "right vid with wrong serial does not match",
			match: config.PortMatch{VID: "10C4", PID: "EA60", Serial: "IC7610-999"},
			ports: testPorts,
			want:  "",
		},
		{
			name:  "empty match never matches anything",
			match: config.PortMatch{},
			ports: testPorts,
			want:  "",
		},
		{
			name:  "descriptor-free enumeration cannot match",
			match: config.PortMatch{VID: "10C4", PID: "EA60"},
			ports: []PortInfo{{Name: "/dev/tty.usbserial-1420"}},
			want:  "",
		},
		{
			name:  "no ports at all",
			match: config.PortMatch{VID: "10C4"},
			ports: nil,
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchPort(tc.ports, tc.match)
			if ok != (tc.want != "") {
				t.Fatalf("matchPort ok = %v (%q), want match %q", ok, got.Name, tc.want)
			}
			if ok && got.Name != tc.want {
				t.Fatalf("matchPort = %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestNormHexID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"10C4", "10c4"},
		{"10c4", "10c4"},
		{"0x10C4", "10c4"},
		{"0X10c4", "10c4"},
		{" 0403 ", "403"},
		{"403", "403"},
		{"", ""},
		{"0x", ""},
		{"0000", "0"},
		{"0x0000", "0"},
	}
	for _, tc := range tests {
		if got := normHexID(tc.in); got != tc.want {
			t.Errorf("normHexID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNewDialer(t *testing.T) {
	t.Run("needs a device or a match", func(t *testing.T) {
		if _, err := NewDialer(config.Port{Baud: 9600}); err == nil {
			t.Fatal("expected an error for a port with neither device nor match")
		}
	})

	t.Run("rejects a match that constrains nothing", func(t *testing.T) {
		if _, err := NewDialer(config.Port{Baud: 9600, Match: &config.PortMatch{}}); err == nil {
			t.Fatal("expected an error for an empty match block")
		}
	})

	t.Run("rejects bad framing at startup", func(t *testing.T) {
		if _, err := NewDialer(config.Port{Device: "/dev/null", Baud: 9600, Parity: "nope"}); err == nil {
			t.Fatal("expected an error for an unknown parity")
		}
	})

	t.Run("copies the match so later config edits cannot alias it", func(t *testing.T) {
		m := config.PortMatch{VID: "10C4", PID: "EA60"}
		d, err := NewDialer(config.Port{Baud: 115200, Match: &m})
		if err != nil {
			t.Fatalf("NewDialer: %v", err)
		}
		m.VID = "FFFF"
		if d.match.VID != "10C4" {
			t.Fatalf("dialer match aliased the caller's struct: %+v", d.match)
		}
	})
}

func TestDialerDescribe(t *testing.T) {
	tests := []struct {
		name string
		port config.Port
		want []string
	}{
		{
			name: "device only",
			port: config.Port{Device: "/dev/tty.usbmodem14201", Baud: 115200},
			want: []string{"/dev/tty.usbmodem14201", "@115200"},
		},
		{
			name: "match with device fallback",
			port: config.Port{
				Device: "/dev/ttyUSB0",
				Baud:   38400,
				Match:  &config.PortMatch{VID: "10C4", PID: "EA60", Serial: "IC7610-001"},
			},
			want: []string{"vid=10C4", "pid=EA60", "serial=IC7610-001", "/dev/ttyUSB0", "@38400"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewDialer(tc.port)
			if err != nil {
				t.Fatalf("NewDialer: %v", err)
			}
			got := d.Describe()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("Describe() = %q, missing %q", got, want)
				}
			}
		})
	}
}

// TestResolveFallsBackToDevice exercises the path an operator hits on a machine
// where the adapter exposes no descriptors: the match cannot succeed, and the
// configured path has to carry the dial.
func TestResolveFallsBackToDevice(t *testing.T) {
	d, err := NewDialer(config.Port{
		Device: "/dev/ttyUSB9",
		Baud:   9600,
		Match:  &config.PortMatch{VID: "dead", PID: "beef"},
	})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	name, err := d.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "/dev/ttyUSB9" {
		t.Fatalf("resolve = %q, want the configured device path", name)
	}
}

// TestResolveErrorNamesSearchAndResult keeps the failure message diagnosable:
// what was looked for, and what was on the machine instead.
func TestResolveErrorNamesSearchAndResult(t *testing.T) {
	d, err := NewDialer(config.Port{Baud: 9600, Match: &config.PortMatch{VID: "dead", PID: "beef"}})
	if err != nil {
		t.Fatalf("NewDialer: %v", err)
	}
	_, err = d.resolve()
	if err == nil {
		t.Skip("a real port on this machine matches vid=dead pid=beef, which is implausible but not our business")
	}
	msg := err.Error()
	for _, want := range []string{"vid=dead", "pid=beef"} {
		if !strings.Contains(msg, want) {
			t.Errorf("resolve error %q does not name %q", msg, want)
		}
	}
	if _, listErr := List(); listErr == nil && !strings.Contains(msg, "saw ") {
		t.Errorf("resolve error %q does not report what was enumerated", msg)
	}
}

func TestDescribePorts(t *testing.T) {
	if got := describePorts(nil); !strings.Contains(got, "no serial ports") {
		t.Errorf("describePorts(nil) = %q", got)
	}
	got := describePorts(testPorts)
	for _, want := range []string{"/dev/ttyUSB0", "vid=10c4", "pid=ea60", "serial=IC7610-001", "/dev/ttyS0"} {
		if !strings.Contains(got, want) {
			t.Errorf("describePorts() = %q, missing %q", got, want)
		}
	}
}
