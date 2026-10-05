package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/version"
)

// DiscoveryPath is where a relay answers discovery, on its dashboard origin.
const DiscoveryPath = "/api/v1/client/discovery"

// DiscoveryTimeout is how long one discovery request may take, from the
// connection to the last byte of the answer.
const DiscoveryTimeout = 10 * time.Second

// discoveryTimeout is DiscoveryTimeout; tests shorten it.
var discoveryTimeout = DiscoveryTimeout

// maxDiscoveryBody is the largest answer that is read. A discovery document
// is a few hundred bytes.
const maxDiscoveryBody = 64 << 10

// Discovery is what a relay says about itself before the client has a token.
type Discovery struct {
	Control          string // host:port of the control endpoint, checked with ValidateControl
	Version          string // the relay's version
	MinClientVersion string // "" when the relay sets no minimum
	ProtocolVersion  int
	// Date is the relay's clock from the Date header, zero without one. It is
	// set whenever the relay answered, also next to an error.
	Date time.Time
}

// ErrNoDiscovery says the relay answered 404: it is older than discovery.
var ErrNoDiscovery = errors.New("the relay has no discovery endpoint")

// ErrNotARelay says the address answered, but not with a discovery document:
// a proxy's error page, another web site, or an answer that names no usable
// control endpoint.
var ErrNotARelay = errors.New("the address does not look like a Burrow relay")

// DiscoveryStatusError is an answer with a status that is neither 200 nor 404,
// redirects included: discovery never follows one.
type DiscoveryStatusError struct {
	Status int
	// RedirectHost is the host a redirect points at, "" otherwise. It comes
	// from the answer and is for display only.
	RedirectHost string
}

func (e *DiscoveryStatusError) Error() string {
	if e.RedirectHost != "" {
		return "the address answered discovery with a redirect to " + e.RedirectHost + " (status " + strconv.Itoa(e.Status) + ")"
	}
	return "the address answered discovery with status " + strconv.Itoa(e.Status)
}

// Discover asks the relay at relayURL (https://host[:port], the dashboard
// address) for its discovery document.
//
// The request goes to that origin over HTTPS and nowhere else: another scheme
// is refused before anything is sent, and a redirect is reported as a
// *DiscoveryStatusError instead of being followed. It carries no token. It
// ends after DiscoveryTimeout at the latest, and at most 64 KiB of the answer
// are read. The control endpoint in the answer is checked with ValidateControl
// before it is returned.
//
// httpClient supplies the transport (and with it the TLS settings); it is not
// modified. The errors are ErrNoDiscovery for a 404, ErrNotARelay for an
// answer that is not a discovery document, a *DiscoveryStatusError for another
// status, and the transport's own error (a certificate problem among them)
// when no answer arrived.
func Discover(ctx context.Context, httpClient *http.Client, relayURL string) (Discovery, error) {
	u, err := url.Parse(strings.TrimSpace(relayURL))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return Discovery{}, errors.New("the relay address must be https://host or https://host:port")
	}
	target := url.URL{Scheme: "https", Host: u.Host, Path: DiscoveryPath}

	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return Discovery{}, errors.New("the relay address must be https://host or https://host:port")
	}
	req.Header.Set("User-Agent", "burrow/"+version.Version)
	req.Header.Set("Accept", "application/json")

	hc := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if httpClient != nil {
		hc.Transport = httpClient.Transport
	}
	resp, err := hc.Do(req)
	if err != nil {
		// Without the *url.Error around it: that repeats the URL, which the
		// caller names itself.
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			err = ue.Err
		}
		return Discovery{}, err
	}
	defer resp.Body.Close()

	var d Discovery
	if t, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		d.Date = t
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return d, ErrNoDiscovery
	case resp.StatusCode != http.StatusOK:
		se := &DiscoveryStatusError{Status: resp.StatusCode}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			se.RedirectHost = redirectHost(resp.Header.Get("Location"))
		}
		return d, se
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		return d, fmt.Errorf("%w: its answer could not be read", ErrNotARelay)
	}
	if len(raw) > maxDiscoveryBody {
		return d, fmt.Errorf("%w: its answer is too large", ErrNotARelay)
	}
	var doc struct {
		Control          string `json:"control"`
		Version          string `json:"version"`
		MinClientVersion string `json:"min_client_version"`
		ProtocolVersion  int    `json:"protocol_version"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&doc); err != nil {
		return d, fmt.Errorf("%w: its answer is not a discovery document", ErrNotARelay)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return d, fmt.Errorf("%w: its answer is not a discovery document", ErrNotARelay)
	}
	// What the answer holds is not repeated in the error: it is not trusted.
	if err := ValidateControl(doc.Control); err != nil {
		return d, fmt.Errorf("%w: its answer names no usable control endpoint", ErrNotARelay)
	}
	d.Control = doc.Control
	d.Version = printable(doc.Version, 64)
	d.MinClientVersion = printable(doc.MinClientVersion, 64)
	d.ProtocolVersion = doc.ProtocolVersion
	return d, nil
}

// printable keeps a value from the relay fit for a terminal: printable ASCII
// without spaces, at most max characters; anything else gives "".
func printable(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c <= ' ' || c > '~' {
			return ""
		}
	}
	return s
}

// redirectHost is the host of a Location header when it is a plain host name
// or address, "" otherwise.
func redirectHost(location string) string {
	u, err := url.Parse(location)
	if err != nil || u.Host == "" {
		return ""
	}
	if validHost(u.Hostname()) != nil {
		return ""
	}
	return u.Host
}

// ValidateControl checks that s is a control endpoint a client can connect
// to: host:port with a host name, an IPv4 address or a bracketed IPv6 address,
// and a port from 1 to 65535. Nothing else may be in it — no scheme, path,
// credentials, space or control character. The error does not repeat s.
func ValidateControl(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return errors.New("a control endpoint must be host:port")
	}
	if len(port) > 5 {
		return errors.New("the port of a control endpoint must be a number between 1 and 65535")
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return errors.New("the port of a control endpoint must be a number between 1 and 65535")
		}
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("the port of a control endpoint must be a number between 1 and 65535")
	}
	if strings.HasPrefix(s, "[") {
		// Brackets are for an IPv6 address and nothing else.
		if ip := net.ParseIP(host); ip == nil || !strings.Contains(host, ":") {
			return errors.New("the host of a control endpoint is not valid")
		}
		return nil
	}
	return validHost(host)
}

// validHost accepts an IP address or a DNS host name in ASCII.
func validHost(host string) error {
	bad := errors.New("the host of a control endpoint is not valid")
	if host == "" || len(host) > 253 {
		return bad
	}
	if net.ParseIP(host) != nil {
		return nil
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return bad
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return bad
			}
		}
	}
	return nil
}

// SameEndpoint reports whether two host:port values name the same control
// endpoint: the host without regard to case or a trailing dot, the same port.
func SameEndpoint(a, b string) bool { return sameEndpoint(a, b) }

// SameHost reports whether a control endpoint (host:port) is on the host of a
// relay address (https://host[:port]).
func SameHost(control, relayURL string) bool {
	ch, _, err := net.SplitHostPort(control)
	if err != nil {
		return false
	}
	u, err := url.Parse(relayURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	norm := func(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }
	return norm(ch) == norm(u.Hostname())
}
