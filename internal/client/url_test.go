package client

import (
	"strings"
	"testing"

	"github.com/hessu/remoses/internal/config"
)

func TestBaseURLFromConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string
	}{
		{
			name: "empty config uses the daemon defaults",
			want: "http://127.0.0.1:8080/api/v1",
		},
		{
			name: "loopback without tls is http",
			cfg:  config.Config{Server: config.Server{Listen: "127.0.0.1:8080", BasePath: "/api/v1"}},
			want: "http://127.0.0.1:8080/api/v1",
		},
		{
			name: "tls configured makes it https",
			cfg: config.Config{Server: config.Server{
				Listen:   "radio.example.net:8443",
				BasePath: "/api/v1",
				TLS:      &config.TLS{CertFile: "cert.pem", KeyFile: "key.pem"},
			}},
			want: "https://radio.example.net:8443/api/v1",
		},
		{
			name: "wildcard bind resolves to loopback",
			cfg:  config.Config{Server: config.Server{Listen: "0.0.0.0:9000"}},
			want: "http://127.0.0.1:9000/api/v1",
		},
		{
			name: "v6 wildcard resolves to v6 loopback",
			cfg:  config.Config{Server: config.Server{Listen: "[::]:8080"}},
			want: "http://[::1]:8080/api/v1",
		},
		{
			name: "port-only bind resolves to loopback",
			cfg:  config.Config{Server: config.Server{Listen: ":8080"}},
			want: "http://127.0.0.1:8080/api/v1",
		},
		{
			name: "base path without a leading slash",
			cfg:  config.Config{Server: config.Server{Listen: "127.0.0.1:8080", BasePath: "remoses/api/v1"}},
			want: "http://127.0.0.1:8080/remoses/api/v1",
		},
		{
			name: "base path at the root",
			cfg:  config.Config{Server: config.Server{Listen: "127.0.0.1:8080", BasePath: "/"}},
			want: "http://127.0.0.1:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BaseURL(&tt.cfg)
			if err != nil {
				t.Fatalf("BaseURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("BaseURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBaseURLRejectsUnusableListen(t *testing.T) {
	if _, err := BaseURL(&config.Config{Server: config.Server{Listen: "not-a-host-port"}}); err == nil {
		t.Fatal("expected an error for a listen address with no port")
	}
	if _, err := BaseURL(nil); err == nil {
		t.Fatal("expected an error for a nil configuration")
	}
}

func TestResolveURL(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"https://radio.example.net", "https://radio.example.net/api/v1"},
		{"https://radio.example.net/", "https://radio.example.net/api/v1"},
		{"http://host:8080/remoses/api/v1", "http://host:8080/remoses/api/v1"},
		{"http://host:8080/remoses/api/v1/", "http://host:8080/remoses/api/v1"},
		{"host:8080", "http://host:8080/api/v1"},
		{"wss://host/api/v1", "https://host/api/v1"},
		{"ws://host", "http://host/api/v1"},
		{"https://oh2xyz@host", "https://host/api/v1"},
		{"https://host/api/v1?radios=x#frag", "https://host/api/v1"},
	}
	for _, tt := range tests {
		got, err := ResolveURL(tt.in)
		if err != nil {
			t.Errorf("ResolveURL(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ResolveURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A password in the URL would be readable in the process table and in shell
// history, which is the whole reason this tool has no -password flag; accepting
// it here would reopen the hole from the other side.
func TestResolveURLRefusesEmbeddedPassword(t *testing.T) {
	_, err := ResolveURL("https://oh2xyz:hunter2@host/api/v1")
	if err == nil {
		t.Fatal("expected an error for a URL carrying a password")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error message leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "REMOSES_PASSWORD") {
		t.Errorf("error message does not say what to do instead: %v", err)
	}
}

func TestResolveURLRejectsBadInput(t *testing.T) {
	for _, in := range []string{"", "   ", "ftp://host/api", "http://"} {
		if _, err := ResolveURL(in); err == nil {
			t.Errorf("ResolveURL(%q): expected an error", in)
		}
	}
}

func TestUserFromURL(t *testing.T) {
	if got := UserFromURL("https://oh2xyz@host/api/v1"); got != "oh2xyz" {
		t.Errorf("UserFromURL = %q, want oh2xyz", got)
	}
	if got := UserFromURL("https://host/api/v1"); got != "" {
		t.Errorf("UserFromURL = %q, want empty", got)
	}
}
