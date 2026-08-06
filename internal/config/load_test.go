package config

import (
	"strings"
	"testing"
)

// testHash is a real bcrypt hash, cost 8, of the password "changeme".
const testHash = "$2a$08$boRz/m7HqlHYSduBcNDLOOoJQoEut/wmkD.Mq98XiDINpdOiQ61iC"

// minimalYAML is the smallest document that loads: everything else is defaulted.
const minimalYAML = `
auth:
  users:
    - username: op
      password_bcrypt: "` + testHash + `"
radios:
  - id: rig1
    backend: civ
    port: { device: /dev/ttyUSB0 }
`

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "minimal",
			yaml: minimalYAML,
		},
		{
			name: "unknown top-level key",
			yaml: minimalYAML + "\nnonsense: 1\n",
			// A typo'd key must not silently leave a default in place.
			wantErr: `unknown field "nonsense"`,
		},
		{
			name:    "unknown nested key",
			yaml:    "server:\n  lissen: \"127.0.0.1:8080\"\n" + minimalYAML,
			wantErr: `unknown field "lissen"`,
		},
		{
			name:    "unknown radio key",
			yaml:    minimalYAML + "    baud: 9600\n",
			wantErr: `unknown field "baud"`,
		},
		{
			// stop_bits is a string because of "1.5", but an operator will write
			// it unquoted; the decoder coerces the scalar rather than refusing.
			name: "unquoted stop_bits",
			yaml: `
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: rig1
    backend: civ
    port: { device: /dev/ttyUSB0, stop_bits: 1.5 }
`,
		},
		{
			name:    "duplicate key",
			yaml:    minimalYAML + "    port: { device: /dev/ttyUSB1 }\n",
			wantErr: `mapping key "port" already defined`,
		},
		{
			name:    "malformed yaml",
			yaml:    "server: {\n",
			wantErr: "config:",
		},
		{
			name:    "duration without a unit",
			yaml:    "lock:\n  ttl: 30\n" + minimalYAML,
			wantErr: "missing unit in duration",
		},
		{
			name:    "band without a range",
			yaml:    minimalYAML + "    limits: { bands: [\"14MHz\"] }\n",
			wantErr: "is not a low-high range",
		},
		{
			name:    "validation failure surfaces",
			yaml:    "radios: []\nauth: { users: [] }\n",
			wantErr: "at least one radio is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(tt.yaml))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Parse: %v", err)
			case tt.wantErr == "":
				if c == nil {
					t.Fatal("Parse returned nil config and nil error")
				}
			case err == nil:
				t.Fatalf("Parse succeeded, want error containing %q", tt.wantErr)
			case !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("Parse error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseDecodesTypedScalars(t *testing.T) {
	c, err := Parse([]byte(minimalYAML + `    limits:
      bands: ["1.8-2.0MHz", "14000-14350kHz"]
      tx_timeout: 90s
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := c.Radio("rig1")
	if r == nil {
		t.Fatal("radio rig1 missing")
	}
	if got := r.Limits.TXTimeout.String(); got != "1m30s" {
		t.Errorf("tx_timeout = %s, want 1m30s", got)
	}
	want := []Band{{LowHz: 1_800_000, HighHz: 2_000_000}, {LowHz: 14_000_000, HighHz: 14_350_000}}
	if len(r.Limits.Bands) != len(want) {
		t.Fatalf("bands = %v, want %v", r.Limits.Bands, want)
	}
	for i := range want {
		if r.Limits.Bands[i] != want[i] {
			t.Errorf("bands[%d] = %v, want %v", i, r.Limits.Bands[i], want[i])
		}
	}
}

func TestLoadExampleConfig(t *testing.T) {
	c, err := Load("../../remoses.example.yaml")
	if err != nil {
		t.Fatalf("the shipped example must load and validate: %v", err)
	}
	for _, id := range []string{"ic7610", "ts590sg", "ft710", "ft857"} {
		if c.Radio(id) == nil {
			t.Errorf("example is missing radio %q", id)
		}
	}
	// The two Yaesu entries share a backend name and differ only in the model,
	// which is the whole point of the dispatch — and the example is where an
	// operator sees that for the first time.
	for _, id := range []string{"ft710", "ft857"} {
		if got := c.Radio(id).Backend; got != BackendYaesu {
			t.Errorf("example radio %q has backend %q, want %q", id, got, BackendYaesu)
		}
	}
	if got := c.Radio("ft857").Yaesu.Model; got != "ft-857d" {
		t.Errorf("example radio ft857 names model %q; without a model it would be built as a "+
			"modern Yaesu, which that radio cannot speak", got)
	}
	if len(c.Auth.Users) != 2 {
		t.Errorf("example has %d users, want 2", len(c.Auth.Users))
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir() + "/nope.yaml"); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

// twoRadioYAML has one radio tracing its CAT traffic and one not, which is the
// arrangement the -debug-wire flag has to interact correctly with.
const twoRadioYAML = `
auth: { users: [{username: op, password_bcrypt: "` + testHash + `"}] }
radios:
  - id: rig1
    backend: civ
    port: { device: /dev/ttyUSB0 }
    debug_wire: true
  - id: rig2
    backend: kenwood
    port: { device: /dev/ttyUSB1 }
`

func TestDebugWireFromConfig(t *testing.T) {
	c, err := Parse([]byte(twoRadioYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.Radio("rig1").DebugWire {
		t.Error("debug_wire: true did not reach the radio")
	}
	if c.Radio("rig2").DebugWire {
		t.Error("debug_wire defaulted to on")
	}
}

func TestApplyWireDebugFromTheCommandLine(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    map[string]bool
		wantErr string
	}{
		{
			// The flag turns a radio on; it never turns one off, so the rig
			// already tracing in the file keeps tracing.
			name: "one radio by id",
			spec: "rig2",
			want: map[string]bool{"rig1": true, "rig2": true},
		},
		{
			name: "empty spec changes nothing",
			spec: "",
			want: map[string]bool{"rig1": true, "rig2": false},
		},
		{
			name: "all",
			spec: WireDebugAll,
			want: map[string]bool{"rig1": true, "rig2": true},
		},
		{
			name: "list tolerates spaces and case",
			spec: " rig2 , RIG2 ",
			want: map[string]bool{"rig1": true, "rig2": true},
		},
		{
			// Silence is what a trace aimed at the wrong radio looks like, so a
			// name that matches nothing has to be reported rather than ignored.
			name:    "unknown radio is refused",
			spec:    "rig3",
			wantErr: "unknown radio rig3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse([]byte(twoRadioYAML))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			err = c.ApplyWireDebug(tt.spec)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ApplyWireDebug(%q) = %v, want an error containing %q",
						tt.spec, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyWireDebug(%q): %v", tt.spec, err)
			}
			for id, want := range tt.want {
				if got := c.Radio(id).DebugWire; got != want {
					t.Errorf("radio %s debug_wire = %v, want %v", id, got, want)
				}
			}
		})
	}
}

// The error names the radios that do exist: whoever mistyped an id at two in
// the morning needs the right one, not a lecture.
func TestApplyWireDebugErrorListsConfiguredRadios(t *testing.T) {
	c, err := Parse([]byte(twoRadioYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	err = c.ApplyWireDebug("ic7610")
	if err == nil {
		t.Fatal("ApplyWireDebug with an unknown radio succeeded")
	}
	for _, want := range []string{"rig1", "rig2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the configured radio %s", err, want)
		}
	}
}
