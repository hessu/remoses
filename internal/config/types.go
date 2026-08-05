// Package config defines and loads the remoses YAML configuration.
//
// This file holds the type declarations only, so that every other package can
// depend on a stable shape. Loading, defaulting and validation live alongside
// it in load.go / defaults.go / validate.go.
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Config is the whole configuration file.
type Config struct {
	Server Server  `yaml:"server"`
	Auth   Auth    `yaml:"auth"`
	Lock   Lock    `yaml:"lock"`
	WS     WS      `yaml:"ws"`
	Radios []Radio `yaml:"radios"`
}

// Radio returns the radio with the given id, or nil.
func (c *Config) Radio(id string) *Radio {
	for i := range c.Radios {
		if c.Radios[i].ID == id {
			return &c.Radios[i]
		}
	}
	return nil
}

type Server struct {
	Listen   string `yaml:"listen"`
	BasePath string `yaml:"base_path"`
	TLS      *TLS   `yaml:"tls"`
	// Insecure permits binding a non-loopback address without TLS. Basic auth
	// replays the password on every request, so this must be opted into.
	Insecure bool `yaml:"insecure"`
}

type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Auth struct {
	Realm string `yaml:"realm"`
	// BcryptCost is deliberately low by default: this is a local, trusted
	// station rather than an internet-facing account system, and every polling
	// request pays the KDF unless it hits CacheTTL.
	BcryptCost int      `yaml:"bcrypt_cost"`
	CacheTTL   Duration `yaml:"cache_ttl"`
	Users      []User   `yaml:"users"`
}

// User is an account. Authorisation is per instance: any authenticated user may
// use any radio, so there are no scopes here.
type User struct {
	Username       string `yaml:"username"`
	PasswordBcrypt string `yaml:"password_bcrypt"`
}

type Lock struct {
	Enabled bool     `yaml:"enabled"`
	TTL     Duration `yaml:"ttl"`
	// AllowSteal permits force=true on acquire to take a lock held by someone
	// else.
	AllowSteal bool `yaml:"allow_steal"`
}

type WS struct {
	// MinInterval is the per-radio coalescing floor for state events. Spinning
	// a VFO knob with Transceive on can produce hundreds of updates a second.
	MinInterval  Duration `yaml:"min_interval"`
	PingInterval Duration `yaml:"ping_interval"`
	SendQueue    int      `yaml:"send_queue"`
}

// Radio is one transceiver.
type Radio struct {
	ID      string `yaml:"id"`   // stable, URL-safe
	Name    string `yaml:"name"` // human readable
	Backend string `yaml:"backend"`

	Port    Port     `yaml:"port"`
	CIV     *CIV     `yaml:"civ"`
	Kenwood *Kenwood `yaml:"kenwood"`
	Yaesu   *Yaesu   `yaml:"yaesu"`
	Rigctld *Rigctld `yaml:"rigctld"`

	Poll   Poll   `yaml:"poll"`
	CW     CW     `yaml:"cw"`
	Limits Limits `yaml:"limits"`
}

// Port describes where a radio's control interface lives: a local serial
// device, or a serial port published over the network by a terminal server such
// as ser2net.
type Port struct {
	// Device is the OS path. Prefer Match: device paths are not stable across
	// replug on Linux or macOS.
	Device string     `yaml:"device"`
	Match  *PortMatch `yaml:"match"`

	// TCP is a host:port serving this radio's serial stream, mutually exclusive
	// with Device and Match. It carries bytes only — plain TCP, not RFC 2217 —
	// so line settings are configured at the terminal server and the modem
	// control lines are unavailable, which rules out serial CW keying and
	// hardware PTT on such a radio.
	TCP string `yaml:"tcp"`

	Baud     int    `yaml:"baud"`
	DataBits int    `yaml:"data_bits"`
	Parity   string `yaml:"parity"` // none | odd | even | mark | space
	StopBits string `yaml:"stop_bits"`
}

// Networked reports whether this port is reached over TCP rather than a local
// serial device.
func (p Port) Networked() bool { return p.TCP != "" }

// PortMatch identifies a USB serial device by its descriptors rather than by
// the path the OS happened to assign it.
type PortMatch struct {
	VID    string `yaml:"vid"`
	PID    string `yaml:"pid"`
	Serial string `yaml:"serial"`
}

// CIV configures the Icom backend.
type CIV struct {
	// Model names the radio, which sets the default bus address and the set of
	// operating modes remoses will offer for it. CI-V opcodes are shared across
	// the family, but which modes a radio has is not: an IC-9700 has DV and DD
	// where an IC-7610 has PSK. Naming the model also gives later work a place
	// to hang the differences that do need separate code paths.
	//
	// The rig cannot reliably be asked: command 19 00 reports its bus address,
	// which is menu-configurable and shared between models, so remoses uses it
	// only to warn about a mismatch. See civ.Rig.checkIdentity.
	Model             string `yaml:"model"`
	RigAddress        int    `yaml:"rig_address"`        // overrides the model default
	ControllerAddress int    `yaml:"controller_address"` // conventionally 0xE0
	// Echo is true when wired to the 13-pin CI-V bus jack, which echoes back
	// everything we transmit. USB connections do not echo.
	Echo bool `yaml:"echo"`
	// Transceive enables unsolicited state broadcasts from the rig.
	Transceive bool `yaml:"transceive"`
}

// Kenwood configures the Kenwood/Elecraft ASCII CAT backend.
//
// Yaesu is not one of them, despite sharing the framing. Its FA field is two
// digits shorter, its mode command takes a receiver selector, its IF answer has
// a different layout and no TX/RX flag at all, and TX; — which keys a Kenwood —
// is the PTT *read* on a Yaesu. See the yaesu backend.
type Kenwood struct {
	Model string `yaml:"model"` // ts590s | ts590sg | ...
	// AutoInformation: 0 off, 2 on (self-clears at rig power-off), 4 on with
	// backup (needs TS-590S firmware >= 2.00).
	AutoInformation int `yaml:"auto_information"`
	// BulkPoll uses IF; — one 38-character reply carrying frequency, RIT/XIT,
	// TX/RX, mode and split — instead of four separate queries. IF; does not
	// answer in Data mode, so the poller falls back automatically.
	BulkPoll bool `yaml:"bulk_poll"`
}

// Yaesu configures the Yaesu ASCII CAT backend.
type Yaesu struct {
	// Model names the radio, and it matters more here than on any other
	// backend: the mode-code tables are per model rather than per family. The
	// code E is PSK on an FT-710 and C4FM on an FT-991A, so the wrong name
	// reports the wrong mode instead of failing. The FTX-1 also has a wider IF
	// answer and a power command with a head selector.
	Model string `yaml:"model"`
	// AutoInformation enables AI1, the rig's push updates. Yaesu has no AI2:
	// the parameter is 0 or 1 only. Like Kenwood's AI2 it reverts to off when
	// the rig is switched off, so it does not permanently alter the operator's
	// settings. On the FTdx10 it works only over the USB CAT port.
	AutoInformation bool `yaml:"auto_information"`
}

// Rigctld configures the Hamlib escape-hatch backend.
type Rigctld struct {
	Address string `yaml:"address"`
	// Spawn makes remoses launch and supervise rigctld itself.
	Spawn  bool   `yaml:"spawn"`
	Model  int    `yaml:"model"`
	Device string `yaml:"device"`
}

type Poll struct {
	Interval     Duration `yaml:"interval"`
	SlowInterval Duration `yaml:"slow_interval"`
}

// CW configures Morse sending for one radio.
type CW struct {
	Enabled bool `yaml:"enabled"`
	// Method is "cat" (rig-side buffer) or "serial_key" (locally generated,
	// keyed on a modem control line).
	Method     string `yaml:"method"`
	DefaultWPM int    `yaml:"default_wpm"`
	// ChunksInFlight is how many chunks beyond the one being sent may sit in
	// the rig's buffer. Too few gives audible gaps between chunks; too many
	// lengthens abort latency.
	ChunksInFlight int        `yaml:"chunks_in_flight"`
	SerialKey      *SerialKey `yaml:"serial_key"`
}

// SerialKey configures locally generated keying on RS-232 control lines.
type SerialKey struct {
	// Device may be the CAT port or a separate one. Separate is strongly
	// preferred: a blocking CAT write on a shared port can jitter an element.
	Device  string `yaml:"device"`
	KeyLine string `yaml:"key_line"` // dtr | rts
	PTTLine string `yaml:"ptt_line"` // dtr | rts; empty for full break-in

	PTTLeadMS int `yaml:"ptt_lead_ms"`
	PTTTailMS int `yaml:"ptt_tail_ms"`
	// Weight is dit/dah weighting as a percentage; 50 is neutral.
	Weight int `yaml:"weight"`
}

// Limits are the transmit safety interlocks.
type Limits struct {
	// MaxPowerPct and MaxPowerW are mutually exclusive; use whichever matches
	// the rig's native scale.
	MaxPowerPct float64 `yaml:"max_power_pct"`
	MaxPowerW   float64 `yaml:"max_power_w"`
	// TXTimeout forces RX regardless of client state. It also fires when a lock
	// expires mid-transmission.
	TXTimeout Duration `yaml:"tx_timeout"`
	Bands     []Band   `yaml:"bands"`
}

// AllowsFrequency reports whether hz falls inside a configured band. An empty
// band list means no restriction.
func (l Limits) AllowsFrequency(hz uint64) bool {
	if len(l.Bands) == 0 {
		return true
	}
	for _, b := range l.Bands {
		if hz >= b.LowHz && hz <= b.HighHz {
			return true
		}
	}
	return false
}

// Duration is a time.Duration that unmarshals from a YAML string such as
// "500ms" or "30s".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("config: bad duration %q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Band is an inclusive frequency range in Hz.
type Band struct {
	LowHz  uint64
	HighHz uint64
}

func (b Band) String() string {
	return fmt.Sprintf("%.6f-%.6fMHz", float64(b.LowHz)/1e6, float64(b.HighHz)/1e6)
}

// UnmarshalText parses forms like "1.8-2.0MHz", "14000-14350kHz" or
// "144000000-146000000". The unit suffix, if present, applies to both bounds.
func (b *Band) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		return fmt.Errorf("config: empty band")
	}

	mult := 1.0
	lower := strings.ToLower(s)
	switch {
	case strings.HasSuffix(lower, "ghz"):
		mult, s = 1e9, s[:len(s)-3]
	case strings.HasSuffix(lower, "mhz"):
		mult, s = 1e6, s[:len(s)-3]
	case strings.HasSuffix(lower, "khz"):
		mult, s = 1e3, s[:len(s)-3]
	case strings.HasSuffix(lower, "hz"):
		mult, s = 1, s[:len(s)-2]
	}

	lo, hi, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return fmt.Errorf("config: band %q is not a low-high range", text)
	}
	parse := func(part string) (uint64, error) {
		f, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return 0, fmt.Errorf("config: bad frequency %q in band %q", part, text)
		}
		if f < 0 || f*mult > math.MaxUint64 {
			return 0, fmt.Errorf("config: frequency %q out of range in band %q", part, text)
		}
		return uint64(math.Round(f * mult)), nil
	}
	l, err := parse(lo)
	if err != nil {
		return err
	}
	h, err := parse(hi)
	if err != nil {
		return err
	}
	if l > h {
		return fmt.Errorf("config: band %q has low above high", text)
	}
	b.LowHz, b.HighHz = l, h
	return nil
}

func (b Band) MarshalText() ([]byte, error) { return []byte(b.String()), nil }
