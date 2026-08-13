// Package client is a read-only Go client for the remoses HTTP and WebSocket
// API described by api/openapi.yaml.
//
// The wire types and the request plumbing are not written here: they are
// generated from that document into internal/wire, and this package is the
// transport, the authentication and the error handling around them. That is
// what makes remoses-cli a proof rather than a demonstration — it reads the
// same document a third-party client would, so a field the spec forgets to
// declare is a field the monitor stops displaying.
//
// Read-only is a property of the package rather than a convention its callers
// are asked to observe: the generated client contains the two GET operations
// and nothing else — see include-operation-ids in api/codegen.yaml — so there
// is no PATCH here to call by accident, and no way to reach the lock endpoints
// at all. A monitor that took a radio's lock would lock out the operator
// actually working that radio.
package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hessu/remoses/internal/wire"
)

// DefaultTimeout bounds a REST request. The endpoints this package calls are
// all served from the session's state cache and never touch a serial port, so a
// request that takes this long means the network or the daemon is in trouble,
// not that a rig is slow.
const DefaultTimeout = 10 * time.Second

// responseLimit bounds a response body.
//
// It lives in the transport rather than at the point of use because the
// generated client reads a body into memory in full before anything looks at
// the status. A state snapshot is a couple of kilobytes; anything approaching
// this is a proxy's error page or a peer that is not the daemon, and neither is
// worth an unbounded allocation.
const responseLimit = 1 << 20

// problemLimit bounds how much of an error body is quoted back. Problem
// documents are small; anything larger would bury the useful line.
const problemLimit = 8 << 10

// radioIDRe is the daemon's own rule for a radio id — see internal/config,
// where the comment on it says the point: an id has to be usable unescaped in a
// URL path.
//
// This package holds its arguments to the same rule because the generated
// request builder takes it at its word. It interpolates a path parameter
// without escaping and resolves the result as a relative reference, so an id
// carrying `../` would not be a 404 for a radio nobody has; it would be a
// request to some other endpoint. Checking here turns a typo back into an error
// message about a typo.
var radioIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func checkRadioID(id string) error {
	if !radioIDRe.MatchString(id) {
		return fmt.Errorf(
			"client: %q is not a radio id: ids are lower-case letters, digits, _ and -", id)
	}
	return nil
}

// Client talks to one remoses instance as one user.
type Client struct {
	base *url.URL
	user string
	pass string

	// api carries REST requests and has a timeout. ws has the same transport
	// but no timeout: coder/websocket derives the handshake context from
	// HTTPClient.Timeout and cancels it as soon as the handshake returns, which
	// would tear down the stream the instant it was established.
	api *wire.ClientWithResponses
	ws  *http.Client
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

	c := &Client{
		base: u,
		user: user,
		pass: pass,
		ws:   &http.Client{Transport: rt},
	}

	// The generated client is given a Doer rather than being asked to build
	// requests itself, so the timeout, the transport and the credentials are
	// still this package's business.
	api, err := wire.NewClientWithResponses(u.String(),
		wire.WithHTTPClient(&http.Client{
			Transport: boundedTransport{rt: rt},
			Timeout:   set.timeout,
		}),
		wire.WithRequestEditorFn(c.authorize))
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	c.api = api
	return c, nil
}

// authorize is the one request editor: Basic auth on every call, over whatever
// scheme the base URL named.
func (c *Client) authorize(_ context.Context, req *http.Request) error {
	req.SetBasicAuth(c.user, c.pass)
	req.Header.Set("Accept", "application/json")
	return nil
}

// BaseURL is the API root this client was built for, for error messages.
func (c *Client) BaseURL() string { return c.base.String() }

// User is the authenticated username, for error messages.
func (c *Client) User() string { return c.user }

// Radio fetches the descriptor: name, backend, capabilities, configured limits
// and who holds the lock.
func (c *Client) Radio(ctx context.Context, radioID string) (*wire.Radio, error) {
	if err := checkRadioID(radioID); err != nil {
		return nil, err
	}
	resp, err := c.api.GetRadioWithResponse(ctx, radioID)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", c.describeURL("/radios/", radioID), err)
	}
	if resp.JSON200 == nil {
		return nil, errorFromResponse(c.describeURL("/radios/", radioID), resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// State fetches the cached state snapshot.
//
// This never fails because a radio is unplugged: the API reports that through
// connected: false and a stale snapshot, which is a state to display rather
// than an error to raise.
func (c *Client) State(ctx context.Context, radioID string) (*wire.State, error) {
	if err := checkRadioID(radioID); err != nil {
		return nil, err
	}
	resp, err := c.api.GetStateWithResponse(ctx, radioID)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", c.describeURL("/radios/", radioID, "/state"), err)
	}
	if resp.JSON200 == nil {
		return nil, errorFromResponse(c.describeURL("/radios/", radioID, "/state"),
			resp.HTTPResponse, resp.Body)
	}
	return resp.JSON200, nil
}

// Age is how old a snapshot was when the server answered.
//
// The server's own measurement is the honest one: comparing updated_at against
// a local clock would report whatever skew exists between the two machines as
// staleness. A snapshot that arrived over the stream carries no age, because it
// was published as it was taken, and reads as zero.
func Age(st *wire.State) time.Duration {
	if st == nil || st.AgeMS == nil {
		return 0
	}
	if *st.AgeMS < 0 {
		return 0
	}
	return time.Duration(*st.AgeMS) * time.Millisecond
}

// IsStale reports the server's own verdict on the snapshot, which is worth more
// than an age: what counts as too old depends on the radio's poll interval, and
// only the daemon knows that.
func IsStale(st *wire.State) bool { return st != nil && st.Stale != nil && *st.Stale }

// describeURL renders a request URL for an error message. The generated client
// builds the real one; this only has to name the same thing.
func (c *Client) describeURL(parts ...string) string {
	u := *c.base
	u.Path += strings.Join(parts, "")
	return u.String()
}

// errorFromResponse turns a non-200 into an APIError, using the problem
// document when the server sent one.
func errorFromResponse(reqURL string, resp *http.Response, body []byte) error {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	e := &APIError{Status: status, URL: reqURL}

	if len(body) > problemLimit {
		body = body[:problemLimit]
	}
	var p wire.Problem
	if json.Unmarshal(body, &p) == nil {
		e.Title, e.Detail = p.Title, valueOr(p.Detail)
	}
	if e.Title == "" {
		e.Title = http.StatusText(status)
	}
	return e
}

func valueOr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// boundedTransport caps how much of a response body reaches the decoder. See
// responseLimit.
type boundedTransport struct{ rt http.RoundTripper }

func (b boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.rt.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(resp.Body, responseLimit), resp.Body}
	return resp, nil
}
