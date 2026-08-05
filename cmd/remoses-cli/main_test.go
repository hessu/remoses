package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/client"
)

// A real bcrypt hash, cost 8. It is here to make the fixture configurations
// load, and it is also the point of one of the tests below: what the file holds
// is a hash, so no password can be recovered from it.
const testHash = "$2a$08$boRz/m7HqlHYSduBcNDLOOoJQoEut/wmkD.Mq98XiDINpdOiQ61iC"

const radiosYAML = `
radios:
  - id: ic7610
    backend: civ
    port: { device: /dev/ttyUSB0 }
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "remoses.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestResolveBaseFromConfigFile(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: "127.0.0.1:8080"
  base_path: /api/v1
auth:
  users:
    - username: op
      password_bcrypt: "`+testHash+`"
`+radiosYAML)

	got, err := resolveBase("", path, true)
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if want := "http://127.0.0.1:8080/api/v1"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
}

// server.tls being set is what makes the daemon serve TLS, so it is what
// decides the scheme here too.
func TestResolveBaseUsesHTTPSWhenTLSIsConfigured(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: "0.0.0.0:8443"
  base_path: /api/v1
  tls:
    cert_file: /etc/remoses/cert.pem
    key_file: /etc/remoses/key.pem
auth:
  users:
    - username: op
      password_bcrypt: "`+testHash+`"
`+radiosYAML)

	got, err := resolveBase("", path, true)
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	// The wildcard bind is not an address a client can dial; loopback is the
	// right guess for a monitor reading the local daemon's own configuration.
	if want := "https://127.0.0.1:8443/api/v1"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
}

// Not being run from the daemon's directory is normal, and the built-in
// defaults are a better answer there than a refusal.
func TestResolveBaseFallsBackWhenTheDefaultConfigIsAbsent(t *testing.T) {
	t.Setenv(envConfig, "")
	missing := filepath.Join(t.TempDir(), "remoses.yaml")

	got, err := resolveBase("", missing, false)
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if want := "http://127.0.0.1:8080/api/v1"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
}

// A path the operator typed is different: silently ignoring it would hide a
// typo behind a connection to the wrong instance.
func TestResolveBaseFailsOnAMissingNamedConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if _, err := resolveBase("", missing, true); err == nil {
		t.Fatal("expected an error for a named configuration file that is absent")
	}
}

func TestResolveBaseURLFlagWins(t *testing.T) {
	path := writeConfig(t, `
auth: { users: [{username: op, password_bcrypt: "`+testHash+`"}] }
`+radiosYAML)

	got, err := resolveBase("https://radio.example.net", path, true)
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if want := "https://radio.example.net/api/v1"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
}

func TestResolveBaseFromEnvironment(t *testing.T) {
	t.Setenv(envURL, "https://radio.example.net:8443/remoses/api/v1")

	got, err := resolveBase("", filepath.Join(t.TempDir(), "absent.yaml"), false)
	if err != nil {
		t.Fatalf("resolveBase: %v", err)
	}
	if want := "https://radio.example.net:8443/remoses/api/v1"; got != want {
		t.Errorf("base = %q, want %q", got, want)
	}
}

func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv(envUser, "oh2xyz")
	t.Setenv(envPassword, "hunter2")

	user, pass, err := credentials("", "", "")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if user != "oh2xyz" || pass != "hunter2" {
		t.Errorf("credentials = %q/%q", user, pass)
	}
}

func TestCredentialsFlagBeatsEnvironment(t *testing.T) {
	t.Setenv(envUser, "fromenv")
	t.Setenv(envPassword, "hunter2")

	user, _, err := credentials("fromflag", "", "")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if user != "fromflag" {
		t.Errorf("user = %q, want fromflag", user)
	}
}

func TestCredentialsFromURLUsername(t *testing.T) {
	t.Setenv(envUser, "")
	t.Setenv(envPassword, "hunter2")

	user, _, err := credentials("", "", "oh2abc")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if user != "oh2abc" {
		t.Errorf("user = %q, want oh2abc", user)
	}
}

// A file is the scriptable way to hold a secret in something with permissions
// on it, which the environment of a process does not have.
func TestCredentialsFromPasswordFile(t *testing.T) {
	t.Setenv(envUser, "oh2xyz")
	t.Setenv(envPassword, "from-the-environment")

	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("hunter2\n# a comment underneath\n"), 0o600); err != nil {
		t.Fatalf("writing password file: %v", err)
	}

	_, pass, err := credentials("", path, "")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if pass != "hunter2" {
		t.Errorf("password = %q, want hunter2", pass)
	}
}

func TestCredentialsPasswordFileMustExist(t *testing.T) {
	t.Setenv(envUser, "oh2xyz")
	if _, _, err := credentials("", filepath.Join(t.TempDir(), "absent"), ""); err == nil {
		t.Fatal("expected an error for a password file that is not there")
	}
}

// The configuration holds bcrypt hashes, so an operator who reaches for it to
// find the password needs telling why that will not work.
func TestDescribeExplainsAuthFailure(t *testing.T) {
	err := &client.APIError{Status: 401, Title: "Unauthorized", URL: "http://x/api/v1"}
	msg := describe(err, "http://x/api/v1", "oh2xyz", "ic7610").Error()

	for _, want := range []string{"authentication failed", "oh2xyz", "bcrypt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message is missing %q: %s", want, msg)
		}
	}
}

func TestDescribeExplainsAnUnknownRadio(t *testing.T) {
	err := &client.APIError{Status: 404, Title: "Not Found", URL: "http://x/api/v1"}
	msg := describe(err, "http://x/api/v1", "oh2xyz", "nosuch").Error()
	if !strings.Contains(msg, "nosuch") || !strings.Contains(msg, "no radio") {
		t.Errorf("message = %s", msg)
	}
}

func TestDescribePassesOtherErrorsThrough(t *testing.T) {
	err := &client.APIError{Status: 502, Title: "Bad Gateway", URL: "http://x/api/v1"}
	if got := describe(err, "b", "u", "r"); got != error(err) {
		t.Errorf("describe rewrote an error it should not have: %v", got)
	}
}

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-version"}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "remoses-cli") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestExactlyOneRadioIsRequired(t *testing.T) {
	for _, args := range [][]string{{}, {"a", "b"}} {
		var out, errOut bytes.Buffer
		err := run(args, &out, &errOut)
		if err == nil {
			t.Errorf("run(%v): expected an error", args)
			continue
		}
		if !strings.Contains(err.Error(), "radio id") {
			t.Errorf("run(%v): unhelpful error %v", args, err)
		}
		if !strings.Contains(errOut.String(), "usage:") {
			t.Errorf("run(%v): no usage was printed", args)
		}
	}
}

func TestUsageDocumentsTheCredentialRules(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"-h"}, &out, &errOut); err == nil {
		t.Fatal("expected flag.ErrHelp")
	}
	help := errOut.String()
	for _, want := range []string{"REMOSES_PASSWORD", "password-file", "shell history", "-once", "-url"} {
		if !strings.Contains(help, want) {
			t.Errorf("usage is missing %q:\n%s", want, help)
		}
	}
}
