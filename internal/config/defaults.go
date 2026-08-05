package config

import "time"

// Defaults. They are chosen for a single-operator station sitting next to its
// radios, not for an internet-facing service: loopback bind, cheap bcrypt, and
// polling intervals fast enough that a VFO knob feels live.
const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultBasePath = "/api/v1"

	DefaultRealm = "remoses"
	// DefaultBcryptCost is well below bcrypt's own default of 10. Basic auth
	// means every polling request pays the KDF unless it hits the verify cache,
	// and the threat model here is a LAN, not a leaked password database.
	DefaultBcryptCost = 8
	DefaultCacheTTL   = 60 * time.Second

	DefaultLockTTL = 30 * time.Second

	DefaultWSMinInterval  = 50 * time.Millisecond
	DefaultWSPingInterval = 30 * time.Second
	DefaultWSSendQueue    = 256

	DefaultPollInterval     = 500 * time.Millisecond
	DefaultPollSlowInterval = 5 * time.Second

	DefaultBaud     = 115200
	DefaultDataBits = 8
	DefaultParity   = "none"
	DefaultStopBits = "1"

	DefaultCWWPM            = 25
	DefaultCWChunksInFlight = 1
	// DefaultSerialKeyWeight is neutral dit/dah weighting.
	DefaultSerialKeyWeight = 50

	DefaultTXTimeout = 120 * time.Second

	// DefaultCIVRigAddress is the IC-7610's factory address; DefaultCIVControllerAddress
	// is the conventional address for a PC on the CI-V bus.
	DefaultCIVRigAddress        = 0x98
	DefaultCIVControllerAddress = 0xE0

	// DefaultKenwoodAutoInformation selects AI2, which pushes state changes and
	// reverts to off at rig power-down rather than permanently altering the
	// operator's settings.
	DefaultKenwoodAutoInformation = 2

	// DefaultYaesuAutoInformation enables AI1 for the same reason, Yaesu
	// spelling it 0/1 where Kenwood has 0/2/4.
	DefaultYaesuAutoInformation = true
)

// applyDefaults fills unset fields. p tells it which zero values were written
// deliberately; see the presence type.
func applyDefaults(c *Config, p *presence) {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Server.BasePath == "" {
		c.Server.BasePath = DefaultBasePath
	}

	if c.Auth.Realm == "" {
		c.Auth.Realm = DefaultRealm
	}
	if c.Auth.BcryptCost == 0 {
		c.Auth.BcryptCost = DefaultBcryptCost
	}
	if p.Auth.CacheTTL == nil {
		c.Auth.CacheTTL = Duration(DefaultCacheTTL)
	}

	if p.Lock.Enabled == nil {
		c.Lock.Enabled = true
	}
	if c.Lock.TTL == 0 {
		c.Lock.TTL = Duration(DefaultLockTTL)
	}

	if c.WS.MinInterval == 0 {
		c.WS.MinInterval = Duration(DefaultWSMinInterval)
	}
	if c.WS.PingInterval == 0 {
		c.WS.PingInterval = Duration(DefaultWSPingInterval)
	}
	if c.WS.SendQueue == 0 {
		c.WS.SendQueue = DefaultWSSendQueue
	}

	for i := range c.Radios {
		applyRadioDefaults(&c.Radios[i], p.radio(i))
	}
}

func applyRadioDefaults(r *Radio, p presenceRadio) {
	if r.Name == "" {
		r.Name = r.ID
	}

	if r.Poll.Interval == 0 {
		r.Poll.Interval = Duration(DefaultPollInterval)
	}
	if r.Poll.SlowInterval == 0 {
		r.Poll.SlowInterval = Duration(DefaultPollSlowInterval)
	}

	// A rigctld radio reaches its rig over TCP, so serial settings would be
	// noise rather than a default.
	if r.Backend != BackendRigctld {
		if r.Port.Baud == 0 {
			r.Port.Baud = DefaultBaud
		}
		if r.Port.DataBits == 0 {
			r.Port.DataBits = DefaultDataBits
		}
		if r.Port.Parity == "" {
			r.Port.Parity = DefaultParity
		}
		if r.Port.StopBits == "" {
			r.Port.StopBits = DefaultStopBits
		}
	}

	if r.CW.DefaultWPM == 0 {
		r.CW.DefaultWPM = DefaultCWWPM
	}
	if r.CW.ChunksInFlight == 0 {
		r.CW.ChunksInFlight = DefaultCWChunksInFlight
	}
	if r.CW.SerialKey != nil && r.CW.SerialKey.Weight == 0 {
		r.CW.SerialKey.Weight = DefaultSerialKeyWeight
	}

	if p.Limits.TXTimeout == nil {
		r.Limits.TXTimeout = Duration(DefaultTXTimeout)
	}

	// The protocol blocks are defaulted only for the backend that reads them,
	// so a stray civ: block on a Kenwood stays visibly wrong rather than being
	// quietly filled in. Each backend requires its block, so an absent one is
	// materialised here.
	switch r.Backend {
	case BackendCIV:
		if r.CIV == nil {
			r.CIV = &CIV{}
		}
		if p.CIV == nil || p.CIV.RigAddress == nil {
			r.CIV.RigAddress = DefaultCIVRigAddress
		}
		if p.CIV == nil || p.CIV.ControllerAddress == nil {
			r.CIV.ControllerAddress = DefaultCIVControllerAddress
		}
		if p.CIV == nil || p.CIV.Transceive == nil {
			r.CIV.Transceive = true
		}
	case BackendKenwood:
		if r.Kenwood == nil {
			r.Kenwood = &Kenwood{}
		}
		if p.Kenwood == nil || p.Kenwood.AutoInformation == nil {
			r.Kenwood.AutoInformation = DefaultKenwoodAutoInformation
		}
		if p.Kenwood == nil || p.Kenwood.BulkPoll == nil {
			r.Kenwood.BulkPoll = true
		}
	case BackendYaesu:
		if r.Yaesu == nil {
			r.Yaesu = &Yaesu{}
		}
		if p.Yaesu == nil || p.Yaesu.AutoInformation == nil {
			r.Yaesu.AutoInformation = DefaultYaesuAutoInformation
		}
	}
}
