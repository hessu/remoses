// Package client is a read-only Go client for the remoses HTTP and WebSocket
// API described by api/openapi.yaml.
//
// Read-only is a property of the package rather than a convention its callers
// are asked to observe: nothing here issues anything but GET, and there is no
// way to reach the lock endpoints at all. A monitor that took a radio's lock
// would lock out the operator actually working that radio, so the restriction
// lives where it cannot be forgotten instead of in a comment on every call
// site.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout bounds a REST request. The endpoints this package calls are
// all served from the session's state cache and never touch a serial port, so a
// request that takes this long means the network or the daemon is in trouble,
// not that a rig is slow.
const DefaultTimeout = 10 * time.Second

// problemLimit bounds how much of an error body is read. Problem documents are
// small; anything larger is a proxy's error page and quoting it in full would
// bury the useful line.
const problemLimit = 8 << 10

// Client talks to one remoses instance as one user.
type Client struct {
	base *url.URL
	user string
	pass string

	// hc carries REST requests and has a timeout. ws has the same transport but
	// no timeout: coder/websocket derives the handshake context from
	// HTTPClient.Timeout and cancels it as soon as the handshake returns, which
	// would tear down the stream the instant it was established.
	hc *http.Client
	ws *http.Client
}

// Option configures a Client.
type Option func(*settings)

type settings struct {
	timeout    time.Duration
	skipVerify bool
	transport  http.RoundTripper
}

// WithTimeout bounds each REST request. It does not apply to the WebSocket
// stream, which is expected to stay open indefinitely.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// WithInsecureTLS disables certificate verification. Stations commonly run
// remoses behind a self-signed certificate, and refusing to talk to one at all
// would push the operator towards plain HTTP, which is worse: Basic auth
// replays the password on every request.
func WithInsecureTLS() Option {
	return func(s *settings) { s.skipVerify = true }
}

// WithTransport overrides the HTTP transport. Tests use it; nothing else should
// need to.
func WithTransport(rt http.RoundTripper) Option {
	return func(s *settings) { s.transport = rt }
}

// New builds a client for the API rooted at baseURL, which must already include
// the instance's base path (see BaseURL and ResolveURL).
func New(baseURL, user, pass string, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("client: bad url %q: %w", baseURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("client: url %q must be http or https", baseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("client: url %q has no host", baseURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawQuery, u.Fragment, u.User = "", "", nil

	set := settings{timeout: DefaultTimeout}
	for _, o := range opts {
		o(&set)
	}

	rt := set.transport
	if rt == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if set.skipVerify {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in, see WithInsecureTLS
		}
		rt = tr
	}

	return &Client{
		base: u,
		user: user,
		pass: pass,
		hc:   &http.Client{Transport: rt, Timeout: set.timeout},
		ws:   &http.Client{Transport: rt},
	}, nil
}

// BaseURL is the API root this client was built for, for error messages.
func (c *Client) BaseURL() string { return c.base.String() }

// User is the authenticated username, for error messages.
func (c *Client) User() string { return c.user }

// Radio fetches the descriptor: name, backend, capabilities, configured limits
// and who holds the lock.
func (c *Client) Radio(ctx context.Context, radioID string) (*Radio, error) {
	var out Radio
	if err := c.get(ctx, "/radios/"+url.PathEscape(radioID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// State fetches the cached state snapshot.
//
// This never fails because a radio is unplugged: the API reports that through
// connected: false and a stale snapshot, which is a state to display rather
// than an error to raise.
func (c *Client) State(ctx context.Context, radioID string) (*State, error) {
	var out State
	if err := c.get(ctx, "/radios/"+url.PathEscape(radioID)+"/state", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// get is the only request path in this package, and it is a GET. Every method
// above goes through it, which is what makes "this client cannot change a
// radio" checkable by reading one function.
func (c *Client) get(ctx context.Context, path string, out any) error {
	u := *c.base
	u.Path += path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", u.String(), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errorFromResponse(u.String(), resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: decoding response: %w", u.String(), err)
	}
	return nil
}

// errorFromResponse turns a non-200 into an APIError, using the problem
// document when the server sent one.
func errorFromResponse(reqURL string, resp *http.Response) error {
	e := &APIError{Status: resp.StatusCode, URL: reqURL}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, problemLimit))
	var p problemDoc
	if json.Unmarshal(body, &p) == nil {
		e.Title, e.Detail = p.Title, p.Detail
	}
	if e.Title == "" {
		e.Title = http.StatusText(resp.StatusCode)
	}
	return e
}

// problemDoc is the RFC 9457 body the API returns for every error.
type problemDoc struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
}
