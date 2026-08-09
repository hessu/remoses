package config

import (
	"strings"
	"testing"
	"time"
)

// validConfig returns a defaulted, valid configuration for mutation by the
// table below.
func validConfig() *Config {
	return &Config{
		Server: Server{Listen: "127.0.0.1:8080", BasePath: "/api/v1"},
		Auth: Auth{
			Realm:      "remoses",
			BcryptCost: 8,
			CacheTTL:   Duration(time.Minute),
			Users:      []User{{Username: "op", PasswordBcrypt: testHash}},
		},
		Lock: Lock{Enabled: true, TTL: Duration(30 * time.Second)},
		WS: WS{
			MinInterval:  Duration(50 * time.Millisecond),
			PingInterval: Duration(30 * time.Second),
			SendQueue:    256,
		},
		Radios: []Radio{{
			ID:      "rig1",
			Name:    "Rig One",
			Backend: BackendCIV,
			Port: Port{
				Device:   "/dev/ttyUSB0",
				Baud:     115200,
				DataBits: 8,
				Parity:   "none",
				StopBits: "1",
				DTR:      "high",
				RTS:      "high",
			},
			CIV:    &CIV{RigAddress: 0x98, ControllerAddress: 0xE0, Transceive: true},
			Poll:   Poll{Interval: Duration(500 * time.Millisecond), SlowInterval: Duration(5 * time.Second)},
			CW:     CW{Enabled: true, Method: "cat", DefaultWPM: 25, ChunksInFlight: 1, BreakIn: "semi"},
			Limits: Limits{MaxPowerPct: 80, TXTimeout: Duration(120 * time.Second)},
		}},
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := Validate(validConfig()); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name:    "no radios",
			mutate:  func(c *Config) { c.Radios = nil },
			wantErr: "at least one radio is required",
		},
		{
			name:    "empty radio id",
			mutate:  func(c *Config) { c.Radios[0].ID = "" },
			wantErr: "radios[0]: id is empty",
		},
		{
			name:    "radio id with uppercase",
			mutate:  func(c *Config) { c.Radios[0].ID = "IC7610" },
			wantErr: `radio "IC7610": id must match`,
		},
		{
			name:    "radio id starting with a dash",
			mutate:  func(c *Config) { c.Radios[0].ID = "-rig" },
			wantErr: "id must match",
		},
		{
			name: "duplicate radio ids",
			mutate: func(c *Config) {
				c.Radios = append(c.Radios, c.Radios[0])
			},
			wantErr: `radio "rig1": duplicate id`,
		},
		{
			name:    "unknown backend",
			mutate:  func(c *Config) { c.Radios[0].Backend = "elecraft" },
			wantErr: `radio "rig1": unknown backend "elecraft", want one of civ, kenwood, yaesu, rigctld`,
		},
		{
			name: "civ without a port",
			mutate: func(c *Config) {
				c.Radios[0].Port.Device = ""
				c.Radios[0].Port.Match = &PortMatch{}
			},
			wantErr: `radio "rig1": port needs device, match (vid/pid/serial), or tcp`,
		},
		{
			name: "port matched by vid only",
			mutate: func(c *Config) {
				c.Radios[0].Port.Device = ""
				c.Radios[0].Port.Match = &PortMatch{VID: "10C4"}
			},
		},
		{
			name:    "zero baud",
			mutate:  func(c *Config) { c.Radios[0].Port.Baud = 0 },
			wantErr: `radio "rig1": port.baud 0 must be positive`,
		},
		{
			name:    "bad parity",
			mutate:  func(c *Config) { c.Radios[0].Port.Parity = "half" },
			wantErr: `port.parity "half"`,
		},
		{
			name:    "bad stop bits",
			mutate:  func(c *Config) { c.Radios[0].Port.StopBits = "3" },
			wantErr: `port.stop_bits "3"`,
		},
		{
			name:    "civ rig address above 255",
			mutate:  func(c *Config) { c.Radios[0].CIV.RigAddress = 256 },
			wantErr: "civ.rig_address 256 is out of range 0..255",
		},
		{
			name:    "civ controller address negative",
			mutate:  func(c *Config) { c.Radios[0].CIV.ControllerAddress = -1 },
			wantErr: "civ.controller_address -1 is out of range 0..255",
		},
		{
			name: "kenwood auto_information 3",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendKenwood
				c.Radios[0].Kenwood = &Kenwood{AutoInformation: 3, BulkPoll: true}
			},
			wantErr: "kenwood.auto_information 3, want 0 (off), 2 or 4",
		},
		{
			name: "kenwood auto_information 4 is fine",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendKenwood
				c.Radios[0].Kenwood = &Kenwood{AutoInformation: 4}
			},
		},
		{
			name: "unknown kenwood model",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendKenwood
				c.Radios[0].Kenwood = &Kenwood{Model: "ts2000", AutoInformation: 2}
			},
			wantErr: `kenwood.model "ts2000", want one of generic, ts480, ts590s, ts590sg, ts890s, ts990s`,
		},
		{
			// The name is hyphenated on the front panel and hyphen-free in the
			// registry; both spellings have to work.
			name: "kenwood model as it is printed on the radio",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendKenwood
				c.Radios[0].Kenwood = &Kenwood{Model: "TS-890S", AutoInformation: 2}
			},
		},
		{
			name: "kenwood model lower case",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendKenwood
				c.Radios[0].Kenwood = &Kenwood{Model: "ts990s", AutoInformation: 2}
			},
		},
		{
			name: "unknown yaesu model",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendYaesu
				c.Radios[0].Yaesu = &Yaesu{Model: "ft-101"}
			},
			wantErr: `yaesu.model "ft-101", want one of generic, ft-710, ft-891, ft-950, ft-991a, ` +
				`ftdx10, ftdx101d, ftdx101mp, ftdx1200, ftdx3000, ftdx5000, ftdx9000, ftx-1, ` +
				`ft-857, ft-857d, ft-897, ft-897d`,
		},
		{
			// The FT-857/FT-897 generation speaks a different protocol under
			// the same backend name; the model is what dispatches, so these
			// names have to validate against yaesu.model like any other.
			name: "yaesu model from the five-byte binary generation",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendYaesu
				c.Radios[0].Yaesu = &Yaesu{Model: "FT-857D"}
			},
		},
		{
			// Yaesu is not consistent about hyphens in its own product names —
			// FT-991A but FTDX10 — so either spelling has to work.
			name: "yaesu model as it is printed on the radio",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendYaesu
				c.Radios[0].Yaesu = &Yaesu{Model: "FT-991A"}
			},
		},
		{
			name: "yaesu model without the hyphen",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendYaesu
				c.Radios[0].Yaesu = &Yaesu{Model: "FTDX-10"}
			},
		},
		{
			name: "yaesu without a port",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendYaesu
				c.Radios[0].Yaesu = &Yaesu{Model: "ft-710"}
				c.Radios[0].Port.Device = ""
			},
			wantErr: `radio "rig1": port needs device, match (vid/pid/serial), or tcp`,
		},
		{
			name: "rigctld without an address",
			mutate: func(c *Config) {
				c.Radios[0].Backend = BackendRigctld
				c.Radios[0].Rigctld = &Rigctld{Spawn: true}
			},
			wantErr: `radio "rig1": backend rigctld requires rigctld.address`,
		},
		{
			name:    "no users",
			mutate:  func(c *Config) { c.Auth.Users = nil },
			wantErr: "at least one user is required",
		},
		{
			name:    "empty username",
			mutate:  func(c *Config) { c.Auth.Users[0].Username = "" },
			wantErr: "auth.users[0]: username is empty",
		},
		{
			name: "duplicate usernames",
			mutate: func(c *Config) {
				c.Auth.Users = append(c.Auth.Users, c.Auth.Users[0])
			},
			wantErr: `duplicate username "op"`,
		},
		{
			name:    "password is not a hash",
			mutate:  func(c *Config) { c.Auth.Users[0].PasswordBcrypt = "hunter2" },
			wantErr: "password_bcrypt is not a bcrypt hash",
		},
		{
			name:    "bcrypt cost too low",
			mutate:  func(c *Config) { c.Auth.BcryptCost = 3 },
			wantErr: "auth.bcrypt_cost 3 is out of range 4..31",
		},
		{
			name:    "bcrypt cost too high",
			mutate:  func(c *Config) { c.Auth.BcryptCost = 32 },
			wantErr: "auth.bcrypt_cost 32 is out of range 4..31",
		},
		{
			name:    "both power limits",
			mutate:  func(c *Config) { c.Radios[0].Limits.MaxPowerW = 80 },
			wantErr: "max_power_pct and limits.max_power_w are mutually exclusive",
		},
		{
			name:    "power percentage above 100",
			mutate:  func(c *Config) { c.Radios[0].Limits.MaxPowerPct = 120 },
			wantErr: "limits.max_power_pct 120 is out of range 0..100",
		},
		{
			name: "negative watts",
			mutate: func(c *Config) {
				c.Radios[0].Limits.MaxPowerPct = 0
				c.Radios[0].Limits.MaxPowerW = -5
			},
			wantErr: "limits.max_power_w -5 must be positive",
		},
		{
			name:    "unknown cw method",
			mutate:  func(c *Config) { c.Radios[0].CW.Method = "winkey" },
			wantErr: `cw.method "winkey", want one of cat, serial_key`,
		},
		{
			name: "disabled cw is not checked",
			mutate: func(c *Config) {
				c.Radios[0].CW.Enabled = false
				c.Radios[0].CW.Method = "winkey"
			},
		},
		{
			name:    "serial_key without a block",
			mutate:  func(c *Config) { c.Radios[0].CW.Method = "serial_key" },
			wantErr: "cw.method serial_key requires a cw.serial_key block",
		},
		{
			name: "bad key line",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{KeyLine: "cts", Weight: 50}
			},
			wantErr: `cw.serial_key.key_line "cts", want one of dtr, rts`,
		},
		{
			name: "bad ptt line",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{KeyLine: "dtr", PTTLine: "dsr", Weight: 50}
			},
			wantErr: `cw.serial_key.ptt_line "dsr"`,
		},
		{
			name: "empty ptt line means full break-in",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{
					Device: "/dev/ttyUSB9", KeyLine: "dtr", Weight: 50,
				}
			},
		},
		{
			name: "serial key without a device",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{KeyLine: "dtr", Weight: 50}
			},
			wantErr: `cw.method serial_key requires cw.serial_key.device`,
		},
		{
			name: "keying device is also the CAT port",
			mutate: func(c *Config) {
				c.Radios[0].Port.Device = "/dev/ttyUSB0"
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{
					Device: "/dev/ttyUSB0", KeyLine: "dtr", Weight: 50,
				}
			},
			wantErr: `is also the CAT port`,
		},
		{
			name: "key and ptt on the same line",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{KeyLine: "dtr", PTTLine: "dtr", Weight: 50}
			},
			wantErr: `cw.serial_key.key_line and ptt_line are both "dtr"`,
		},
		{
			name: "weight out of range",
			mutate: func(c *Config) {
				c.Radios[0].CW.Method = "serial_key"
				c.Radios[0].CW.SerialKey = &SerialKey{KeyLine: "dtr", Weight: 90}
			},
			wantErr: "cw.serial_key.weight 90 is out of range 20..80",
		},
		{
			name:    "tls cert without key",
			mutate:  func(c *Config) { c.Server.TLS = &TLS{CertFile: "/etc/remoses/cert.pem"} },
			wantErr: "cert_file and key_file must both be set or both be empty",
		},
		{
			name:    "tls key without cert",
			mutate:  func(c *Config) { c.Server.TLS = &TLS{KeyFile: "/etc/remoses/key.pem"} },
			wantErr: "cert_file and key_file must both be set or both be empty",
		},
		{
			name:    "listen is not host:port",
			mutate:  func(c *Config) { c.Server.Listen = "8080" },
			wantErr: `server.listen "8080" is not host:port`,
		},
		{
			name:    "wildcard bind without tls",
			mutate:  func(c *Config) { c.Server.Listen = "0.0.0.0:8080" },
			wantErr: "Basic auth replays the password on every request",
		},
		{
			name:    "bare port bind without tls",
			mutate:  func(c *Config) { c.Server.Listen = ":8080" },
			wantErr: "server.insecure: true to override",
		},
		{
			name:    "routable bind without tls",
			mutate:  func(c *Config) { c.Server.Listen = "192.0.2.10:8080" },
			wantErr: "is not a loopback address and tls is not configured",
		},
		{
			name: "wildcard bind with tls",
			mutate: func(c *Config) {
				c.Server.Listen = "0.0.0.0:8080"
				c.Server.TLS = &TLS{CertFile: "/etc/remoses/cert.pem", KeyFile: "/etc/remoses/key.pem"}
			},
		},
		{
			name: "wildcard bind with the insecure escape hatch",
			mutate: func(c *Config) {
				c.Server.Listen = "0.0.0.0:8080"
				c.Server.Insecure = true
			},
		},
		{
			name:   "ipv6 loopback needs no tls",
			mutate: func(c *Config) { c.Server.Listen = "[::1]:8080" },
		},
		{
			name:   "localhost needs no tls",
			mutate: func(c *Config) { c.Server.Listen = "localhost:8080" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := Validate(c)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tt.wantErr == "":
			case err == nil:
				t.Fatalf("Validate passed, want error containing %q", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Validate error =\n%v\nwant it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// An operator far from the radio site should learn about every mistake in one
// run, not one per restart.
func TestValidateReportsEveryError(t *testing.T) {
	c := validConfig()
	c.Auth.BcryptCost = 99
	c.Auth.Users[0].PasswordBcrypt = "not-a-hash"
	c.Radios[0].ID = "Rig One"
	c.Radios[0].Backend = "nope"
	c.Radios = append(c.Radios, Radio{
		ID:      "rig2",
		Backend: BackendKenwood,
		Port:    Port{Baud: -1, Parity: "none", StopBits: "1"},
		Kenwood: &Kenwood{AutoInformation: 7},
	})

	err := Validate(c)
	if err == nil {
		t.Fatal("Validate passed")
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("Validate did not return a joined error: %T", err)
	}
	if n := len(joined.Unwrap()); n < 6 {
		t.Errorf("Validate returned %d errors, want at least 6:\n%v", n, err)
	}

	for _, want := range []string{
		"auth.bcrypt_cost 99",
		"password_bcrypt is not a bcrypt hash",
		`radio "Rig One": id must match`,
		`unknown backend "nope"`,
		`radio "rig2": port needs device, match (vid/pid/serial), or tcp`,
		`radio "rig2": port.baud -1`,
		"kenwood.auto_information 7",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q from:\n%v", want, err)
		}
	}
}
