package config

import (
	"strings"
	"testing"
)

// A serial port published over the network by a terminal server (ser2net and
// friends) is reachable by every serial backend, not just rigctld.
func TestNetworkedPort(t *testing.T) {
	tests := []struct {
		name    string
		port    string
		wantErr string
	}{
		{
			name: "tcp instead of a local device",
			port: `tcp: "192.168.1.50:4001"`,
		},
		{
			name:    "tcp combined with device",
			port:    `tcp: "192.168.1.50:4001", device: /dev/ttyUSB0`,
			wantErr: "cannot be combined with device or match",
		},
		{
			name:    "tcp without a port",
			port:    `tcp: "192.168.1.50"`,
			wantErr: "must be host:port",
		},
		{
			name:    "neither device nor tcp",
			port:    `baud: 9600`,
			wantErr: "port needs device, match (vid/pid/serial), or tcp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(`
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: remote
    backend: kenwood
    port: { ` + tc.port + ` }
`))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Parse: %v", err)
			case tc.wantErr == "":
				if p := c.Radio("remote").Port; !p.Networked() || p.TCP != "192.168.1.50:4001" {
					t.Errorf("port = %+v, want networked 192.168.1.50:4001", p)
				}
			case err == nil:
				t.Fatalf("Parse succeeded, want error containing %q", tc.wantErr)
			case !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// Baud, parity and stop bits belong to the terminal server on a networked port,
// so remoses must not insist on them or complain about them.
func TestNetworkedPortIgnoresLineSettings(t *testing.T) {
	if _, err := Parse([]byte(`
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: remote
    backend: civ
    port: { tcp: "ser2net.local:4001" }
    civ: { rig_address: 0x98 }
`)); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
