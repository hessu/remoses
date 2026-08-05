package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Backend names. Validation compares against this literal list rather than the
// backend registry: config is below rig/backend in the dependency graph, and
// importing the registry to ask it would be a cycle.
const (
	BackendCIV     = "civ"
	BackendKenwood = "kenwood"
	BackendRigctld = "rigctld"
)

var backends = []string{BackendCIV, BackendKenwood, BackendRigctld}

// CIVModels are the accepted values of civ.model.
//
// Duplicated from the civ backend's own registry rather than imported, for the
// same reason as `backends`: config sits below rig/backend in the dependency
// graph and asking the registry would be a cycle. The backend has a test that
// fails if the two ever drift, which is the direction that can import both.
var CIVModels = []string{
	"generic",
	"ic-718", "ic-7300", "ic-7300mk2", "ic-7600", "ic-7610", "ic-7700",
	"ic-7760", "ic-7850", "ic-905", "ic-910h", "ic-9100", "ic-9700",
}

// idRe keeps radio ids usable unescaped in a URL path and in a cookie name.
var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

var (
	parities  = []string{"none", "odd", "even", "mark", "space"}
	stopBits  = []string{"1", "1.5", "2"}
	cwMethods = []string{"cat", "serial_key"}
	lines     = []string{"dtr", "rts"}
)

// Validate checks a defaulted Config.
//
// It reports every problem it finds, joined, rather than stopping at the first.
// This file is hand-edited by an operator who may be a long way from the radio
// site, and a boot loop that reveals one mistake per restart is hostile.
func Validate(c *Config) error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	validateServer(c, add)
	validateAuth(c, add)
	validateRadios(c, add)

	return errors.Join(errs...)
}

type addFunc func(format string, a ...any)

func validateServer(c *Config, add addFunc) {
	tls := c.Server.TLS
	if tls != nil && (tls.CertFile == "") != (tls.KeyFile == "") {
		add("server.tls: cert_file and key_file must both be set or both be empty")
	}
	haveTLS := tls != nil && tls.CertFile != "" && tls.KeyFile != ""

	host, _, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		add("server.listen %q is not host:port: %v", c.Server.Listen, err)
		return
	}
	if !haveTLS && !c.Server.Insecure && !isLoopback(host) {
		add("server.listen %q is not a loopback address and tls is not configured: "+
			"Basic auth replays the password on every request, so remoses refuses to "+
			"expose it in cleartext. Configure server.tls, bind to 127.0.0.1 behind a "+
			"TLS-terminating proxy, or set server.insecure: true to override",
			c.Server.Listen)
	}
}

// isLoopback reports whether binding host reaches only this machine. An empty
// or unspecified host is the wildcard, which is the case the TLS rule exists
// for.
func isLoopback(host string) bool {
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validateAuth(c *Config, add addFunc) {
	if c.Auth.BcryptCost < bcrypt.MinCost || c.Auth.BcryptCost > bcrypt.MaxCost {
		add("auth.bcrypt_cost %d is out of range %d..%d",
			c.Auth.BcryptCost, bcrypt.MinCost, bcrypt.MaxCost)
	}

	if len(c.Auth.Users) == 0 {
		add("auth.users: at least one user is required")
	}
	seen := make(map[string]bool, len(c.Auth.Users))
	for i, u := range c.Auth.Users {
		switch {
		case u.Username == "":
			add("auth.users[%d]: username is empty", i)
		case seen[u.Username]:
			add("auth.users[%d]: duplicate username %q", i, u.Username)
		default:
			seen[u.Username] = true
		}
		if _, err := bcrypt.Cost([]byte(u.PasswordBcrypt)); err != nil {
			add("auth.users[%d] (%s): password_bcrypt is not a bcrypt hash: %v "+
				"(generate one with: remoses passwd)", i, u.Username, err)
		}
	}
}

func validateRadios(c *Config, add addFunc) {
	if len(c.Radios) == 0 {
		add("radios: at least one radio is required")
	}
	seen := make(map[string]bool, len(c.Radios))
	for i := range c.Radios {
		r := &c.Radios[i]
		label := radioLabel(r.ID, i)

		switch {
		case r.ID == "":
			add("%s: id is empty", label)
		case !idRe.MatchString(r.ID):
			add("%s: id must match %s", label, idRe)
		case seen[r.ID]:
			add("%s: duplicate id", label)
		default:
			seen[r.ID] = true
		}

		if !slices.Contains(backends, r.Backend) {
			add("%s: unknown backend %q, want one of %s",
				label, r.Backend, strings.Join(backends, ", "))
		}

		switch r.Backend {
		case BackendCIV:
			validatePort(&r.Port, label, add)
			validateCIV(r.CIV, label, add)
		case BackendKenwood:
			validatePort(&r.Port, label, add)
			validateKenwood(r.Kenwood, label, add)
		case BackendRigctld:
			if r.Rigctld == nil || r.Rigctld.Address == "" {
				add("%s: backend rigctld requires rigctld.address", label)
			}
		}

		validateLimits(&r.Limits, label, add)
		validateCW(&r.CW, label, add)

		// Local keying toggles DTR or RTS on a port of its own, independent of
		// how the rig is controlled — keying a rigctld- or TCP-controlled radio
		// through a local adapter is a perfectly reasonable station. What it
		// always needs is a local device to key: neither a socket nor Hamlib
		// carries modem control lines.
		if r.CW.Enabled && r.CW.Method == "serial_key" && r.CW.SerialKey != nil {
			switch {
			case r.CW.SerialKey.Device == "":
				add("%s: cw.method serial_key requires cw.serial_key.device, "+
					"a local serial port whose DTR/RTS can be keyed", label)
			case r.Port.Device != "" && r.CW.SerialKey.Device == r.Port.Device:
				add("%s: cw.serial_key.device %q is also the CAT port; "+
					"keying needs a separate device", label, r.CW.SerialKey.Device)
			}
		}
	}
}

func radioLabel(id string, i int) string {
	if id == "" {
		return fmt.Sprintf("radios[%d]", i)
	}
	return fmt.Sprintf("radio %q", id)
}

func validatePort(p *Port, label string, add addFunc) {
	if p.Networked() {
		// A networked port carries bytes only; the local serial settings below
		// belong to the terminal server, so flagging them here would be noise.
		if p.Device != "" || p.Match.set() {
			add("%s: port.tcp cannot be combined with device or match", label)
		}
		if _, _, err := net.SplitHostPort(p.TCP); err != nil {
			add("%s: port.tcp %q must be host:port", label, p.TCP)
		}
		return
	}
	if p.Device == "" && !p.Match.set() {
		add("%s: port needs device, match (vid/pid/serial), or tcp", label)
	}
	if p.Baud <= 0 {
		add("%s: port.baud %d must be positive", label, p.Baud)
	}
	if !slices.Contains(parities, p.Parity) {
		add("%s: port.parity %q, want one of %s",
			label, p.Parity, strings.Join(parities, ", "))
	}
	if !slices.Contains(stopBits, p.StopBits) {
		add("%s: port.stop_bits %q, want one of %s",
			label, p.StopBits, strings.Join(stopBits, ", "))
	}
}

// set reports whether the match block names anything to match on. An empty
// block is as good as absent.
func (m *PortMatch) set() bool {
	return m != nil && (m.VID != "" || m.PID != "" || m.Serial != "")
}

func validateCIV(civ *CIV, label string, add addFunc) {
	if civ == nil {
		return
	}
	if civ.Model != "" && !slices.Contains(CIVModels, strings.ToLower(civ.Model)) {
		add("%s: civ.model %q, want one of %s",
			label, civ.Model, strings.Join(CIVModels, ", "))
	}
	if civ.RigAddress < 0 || civ.RigAddress > 255 {
		add("%s: civ.rig_address %d is out of range 0..255", label, civ.RigAddress)
	}
	if civ.ControllerAddress < 0 || civ.ControllerAddress > 255 {
		add("%s: civ.controller_address %d is out of range 0..255", label, civ.ControllerAddress)
	}
}

func validateKenwood(k *Kenwood, label string, add addFunc) {
	if k == nil {
		return
	}
	if k.AutoInformation != 0 && k.AutoInformation != 2 && k.AutoInformation != 4 {
		add("%s: kenwood.auto_information %d, want 0 (off), 2 or 4",
			label, k.AutoInformation)
	}
}

func validateLimits(l *Limits, label string, add addFunc) {
	if l.MaxPowerPct != 0 && l.MaxPowerW != 0 {
		add("%s: limits.max_power_pct and limits.max_power_w are mutually exclusive", label)
	}
	if l.MaxPowerPct < 0 || l.MaxPowerPct > 100 {
		add("%s: limits.max_power_pct %g is out of range 0..100", label, l.MaxPowerPct)
	}
	if l.MaxPowerW < 0 {
		add("%s: limits.max_power_w %g must be positive", label, l.MaxPowerW)
	}
}

func validateCW(cw *CW, label string, add addFunc) {
	if !cw.Enabled {
		return
	}
	if !slices.Contains(cwMethods, cw.Method) {
		add("%s: cw.method %q, want one of %s",
			label, cw.Method, strings.Join(cwMethods, ", "))
		return
	}
	if cw.Method != "serial_key" {
		return
	}

	sk := cw.SerialKey
	if sk == nil {
		add("%s: cw.method serial_key requires a cw.serial_key block", label)
		return
	}
	if !slices.Contains(lines, sk.KeyLine) {
		add("%s: cw.serial_key.key_line %q, want one of %s",
			label, sk.KeyLine, strings.Join(lines, ", "))
	}
	if sk.PTTLine != "" && !slices.Contains(lines, sk.PTTLine) {
		add("%s: cw.serial_key.ptt_line %q, want one of %s or empty for full break-in",
			label, sk.PTTLine, strings.Join(lines, ", "))
	}
	if sk.KeyLine != "" && sk.KeyLine == sk.PTTLine {
		add("%s: cw.serial_key.key_line and ptt_line are both %q", label, sk.KeyLine)
	}
	if sk.Weight < 20 || sk.Weight > 80 {
		add("%s: cw.serial_key.weight %d is out of range 20..80", label, sk.Weight)
	}
}
