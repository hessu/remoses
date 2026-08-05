package client

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/hessu/remoses/internal/config"
)

// BaseURL derives the API root of the instance a configuration file describes.
//
// Three things come out of the file. server.tls decides the scheme, exactly as
// it decides ListenAndServeTLS in the daemon. server.listen gives the
// authority — but it is a *bind* address, and a bind address is not always a
// reachable one: 0.0.0.0 and :: mean "every interface", which is not somewhere
// a client can connect. Those are rewritten to the matching loopback, which is
// the right guess because this path exists for the case where the monitor and
// the daemon are on the same machine; anything else is what -url is for.
// server.base_path is appended, because the routes hang off it.
func BaseURL(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("client: nil configuration")
	}

	listen := cfg.Server.Listen
	if listen == "" {
		listen = config.DefaultListen
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("client: server.listen %q is not host:port: %w", listen, err)
	}
	host = reachableHost(host)
	if port == "" {
		return "", fmt.Errorf("client: server.listen %q has no port", listen)
	}

	scheme := "http"
	if cfg.Server.TLS != nil {
		scheme = "https"
	}

	base := cfg.Server.BasePath
	if base == "" {
		base = config.DefaultBasePath
	}

	u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: normalizePath(base)}
	return u.String(), nil
}

// reachableHost maps a bind address onto an address that can be dialled.
func reachableHost(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	}
	// An unspecified address written some other way — "0:0:0:0:0:0:0:0" — is
	// still a wildcard and still not connectable.
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		if ip.To4() != nil {
			return "127.0.0.1"
		}
		return "::1"
	}
	return host
}

// ResolveURL normalises a user-supplied -url into an API root.
//
// A bare scheme and host is the common case — nobody wants to type the base
// path — so an empty path is filled in with the daemon's default. A path that
// was actually written is left alone, because an instance behind a reverse
// proxy can live anywhere.
//
// Credentials in the URL are refused rather than used: a password there would
// sit in shell history and in the process table, which is the whole reason this
// tool has no -password flag.
func ResolveURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("client: empty url")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("client: bad url %q: %w", raw, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("client: url %q must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("client: url %q has no host", raw)
	}
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			return "", fmt.Errorf("client: url %q carries a password; "+
				"pass it in REMOSES_PASSWORD, -password-file or at the prompt instead, "+
				"so it stays out of shell history and the process table", u.Redacted())
		}
	}
	u.User = nil
	u.RawQuery, u.Fragment = "", ""

	u.Path = normalizePath(u.Path)
	if u.Path == "" {
		u.Path = config.DefaultBasePath
	}
	return u.String(), nil
}

// UserFromURL returns the username written into a URL, if any. The username is
// not a secret, so taking it from there is a convenience rather than a leak.
func UserFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return ""
	}
	return u.User.Username()
}

// normalizePath gives a base path a leading slash and no trailing one, so that
// it concatenates with a route without producing "//" or a relative path.
func normalizePath(p string) string {
	p = strings.TrimSuffix(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}
