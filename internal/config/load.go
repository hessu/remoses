package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/goccy/go-yaml"
)

// WireDebugAll is the -debug-wire value selecting every configured radio.
const WireDebugAll = "all"

// ApplyWireDebug turns on CAT wire logging for the radios named in spec: a
// comma-separated list of radio ids, or the single word "all".
//
// It only ever enables. The command line is what somebody reaches for at two in
// the morning with a misbehaving rig in front of them, so the file must not be
// able to countermand it; leaving debug_wire on permanently is what the file is
// for. An empty spec changes nothing.
//
// An id that names no configured radio is an error rather than a no-op: the
// flag would otherwise be silent about a typo, and silence is exactly what it
// looks like when the trace is aimed at the wrong radio.
func (c *Config) ApplyWireDebug(spec string) error {
	var unknown []string
	for _, name := range strings.Split(spec, ",") {
		name = strings.TrimSpace(name)
		switch {
		case name == "":
			continue
		case strings.EqualFold(name, WireDebugAll):
			for i := range c.Radios {
				c.Radios[i].DebugWire = true
			}
		default:
			// Matched case-insensitively. Validation constrains ids to
			// lower-case, so folding cannot make two radios ambiguous, and
			// rejecting "IC7610" for its capitals would be a papercut at
			// exactly the moment this flag gets typed.
			r := c.radioFold(name)
			if r == nil {
				unknown = append(unknown, name)
				continue
			}
			r.DebugWire = true
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	return fmt.Errorf("unknown radio %s (configured: %s)",
		strings.Join(unknown, ", "), strings.Join(c.RadioIDs(), ", "))
}

// radioFold finds a radio by id, ignoring case. Only the command line needs
// this; everywhere else an id comes from the file or a URL path and must match
// exactly.
func (c *Config) radioFold(id string) *Radio {
	for i := range c.Radios {
		if strings.EqualFold(c.Radios[i].ID, id) {
			return &c.Radios[i]
		}
	}
	return nil
}

// Load reads, defaults and validates the configuration file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes, defaults and validates a configuration document.
//
// Decoding is strict: an unrecognised key is an error rather than a silently
// ignored one. This file is hand-edited, and a typo'd "lissen:" that quietly
// left the daemon on its default address would be worse than a refusal to
// start.
func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalWithOptions(b, &c, yaml.DisallowUnknownField()); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var p presence
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	applyDefaults(&c, &p)

	if err := Validate(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// presence records which optional keys the document actually contained.
//
// It exists because several knobs default to a non-zero value while zero is
// itself a meaningful setting: "lock: {enabled: false}", "auto_information: 0"
// (AI off), "cache_ttl: 0s" (verify cache disabled). Config alone cannot tell
// those apart from an absent key, so a second, lenient pass over the same bytes
// records what was actually written.
//
// Only keys carrying that ambiguity appear here. Everywhere else the zero value
// is never a legal setting, so zero already means "unset".
type presence struct {
	Auth   presenceAuth    `yaml:"auth"`
	Lock   presenceLock    `yaml:"lock"`
	Radios []presenceRadio `yaml:"radios"`
}

type presenceAuth struct {
	CacheTTL *Duration `yaml:"cache_ttl"`
}

type presenceLock struct {
	Enabled *bool `yaml:"enabled"`
}

type presenceRadio struct {
	CIV     *presenceCIV     `yaml:"civ"`
	Kenwood *presenceKenwood `yaml:"kenwood"`
	Yaesu   *presenceYaesu   `yaml:"yaesu"`
	Limits  presenceLimits   `yaml:"limits"`
}

type presenceCIV struct {
	RigAddress        *int  `yaml:"rig_address"`
	ControllerAddress *int  `yaml:"controller_address"`
	Transceive        *bool `yaml:"transceive"`
}

type presenceKenwood struct {
	AutoInformation *int  `yaml:"auto_information"`
	BulkPoll        *bool `yaml:"bulk_poll"`
}

type presenceYaesu struct {
	AutoInformation *bool `yaml:"auto_information"`
}

type presenceLimits struct {
	TXTimeout *Duration `yaml:"tx_timeout"`
}

// radio returns the presence record for radio index i. Both decodes walk the
// same document so the indices line up; the guard covers a Config assembled in
// code rather than parsed.
func (p *presence) radio(i int) presenceRadio {
	if i < len(p.Radios) {
		return p.Radios[i]
	}
	return presenceRadio{}
}
