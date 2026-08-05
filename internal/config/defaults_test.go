package config

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	c, err := Parse([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := c.Radio("rig1")

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"server.listen", c.Server.Listen, DefaultListen},
		{"server.base_path", c.Server.BasePath, DefaultBasePath},
		{"auth.realm", c.Auth.Realm, DefaultRealm},
		{"auth.bcrypt_cost", c.Auth.BcryptCost, DefaultBcryptCost},
		{"auth.cache_ttl", c.Auth.CacheTTL.D(), 60 * time.Second},
		{"lock.enabled", c.Lock.Enabled, true},
		{"lock.ttl", c.Lock.TTL.D(), 30 * time.Second},
		{"lock.allow_steal", c.Lock.AllowSteal, false},
		{"ws.min_interval", c.WS.MinInterval.D(), 50 * time.Millisecond},
		{"ws.ping_interval", c.WS.PingInterval.D(), 30 * time.Second},
		{"ws.send_queue", c.WS.SendQueue, 256},
		{"poll.interval", r.Poll.Interval.D(), 500 * time.Millisecond},
		{"poll.slow_interval", r.Poll.SlowInterval.D(), 5 * time.Second},
		{"port.baud", r.Port.Baud, 115200},
		{"port.data_bits", r.Port.DataBits, 8},
		{"port.parity", r.Port.Parity, "none"},
		{"port.stop_bits", r.Port.StopBits, "1"},
		{"cw.default_wpm", r.CW.DefaultWPM, 25},
		{"cw.chunks_in_flight", r.CW.ChunksInFlight, 1},
		{"limits.tx_timeout", r.Limits.TXTimeout.D(), 120 * time.Second},
		{"civ.rig_address", r.CIV.RigAddress, 0x98},
		{"civ.controller_address", r.CIV.ControllerAddress, 0xE0},
		{"civ.transceive", r.CIV.Transceive, true},
		{"name falls back to id", r.Name, "rig1"},
	}
	for _, ch := range checks {
		if ch.got != ch.want {
			t.Errorf("%s = %v, want %v", ch.field, ch.got, ch.want)
		}
	}

	if r.Kenwood != nil {
		t.Errorf("civ radio got a kenwood block: %+v", r.Kenwood)
	}
}

func TestApplyDefaultsKenwood(t *testing.T) {
	c, err := Parse([]byte(`
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: rig1
    backend: kenwood
    port: { device: COM7 }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	k := c.Radio("rig1").Kenwood
	if k == nil {
		t.Fatal("kenwood block not materialised")
	}
	if k.AutoInformation != 2 {
		t.Errorf("auto_information = %d, want 2", k.AutoInformation)
	}
	if !k.BulkPoll {
		t.Error("bulk_poll = false, want true")
	}
	if c.Radio("rig1").CIV != nil {
		t.Error("kenwood radio got a civ block")
	}
}

func TestApplyDefaultsRigctldLeavesSerialAlone(t *testing.T) {
	c, err := Parse([]byte(`
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: rig1
    backend: rigctld
    rigctld: { address: "127.0.0.1:4532" }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Radio("rig1").Port; got.Baud != 0 || got.Parity != "" {
		t.Errorf("rigctld radio got serial defaults: %+v", got)
	}
}

// Several knobs default to a non-zero value while zero is itself a legal
// setting. An explicit zero must survive defaulting, or "turn this off" would
// be impossible to express.
func TestExplicitZerosSurviveDefaults(t *testing.T) {
	c, err := Parse([]byte(`
auth:
  cache_ttl: 0s
  users: [{username: op, password_bcrypt: "` + testHash + `"}]
lock:
  enabled: false
radios:
  - id: icom
    backend: civ
    port: { device: /dev/ttyUSB0 }
    civ: { rig_address: 0, controller_address: 0, transceive: false }
    limits: { tx_timeout: 0s }
  - id: kenwood
    backend: kenwood
    port: { device: COM7 }
    kenwood: { auto_information: 0, bulk_poll: false }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Auth.CacheTTL != 0 {
		t.Errorf("cache_ttl = %v, want 0", c.Auth.CacheTTL)
	}
	if c.Lock.Enabled {
		t.Error("lock.enabled = true, want false")
	}

	icom := c.Radio("icom")
	if icom.CIV.RigAddress != 0 || icom.CIV.ControllerAddress != 0 {
		t.Errorf("civ addresses = %d/%d, want 0/0",
			icom.CIV.RigAddress, icom.CIV.ControllerAddress)
	}
	if icom.CIV.Transceive {
		t.Error("civ.transceive = true, want false")
	}
	if icom.Limits.TXTimeout != 0 {
		t.Errorf("tx_timeout = %v, want 0", icom.Limits.TXTimeout)
	}

	kw := c.Radio("kenwood")
	if kw.Kenwood.AutoInformation != 0 {
		t.Errorf("auto_information = %d, want 0", kw.Kenwood.AutoInformation)
	}
	if kw.Kenwood.BulkPoll {
		t.Error("bulk_poll = true, want false")
	}
}

func TestSerialKeyWeightDefault(t *testing.T) {
	c, err := Parse([]byte(`
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: rig1
    backend: rigctld
    rigctld: { address: "127.0.0.1:4532" }
    cw:
      enabled: true
      method: serial_key
      serial_key: { device: /dev/ttyUSB2, key_line: dtr, ptt_line: rts }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Radio("rig1").CW.SerialKey.Weight; got != 50 {
		t.Errorf("serial_key.weight = %d, want 50", got)
	}
}
