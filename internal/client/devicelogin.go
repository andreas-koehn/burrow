package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/version"
)

// Where a relay answers the browser-approved sign-in, on its dashboard origin,
// and the page a person approves it on.
const (
	DeviceLoginStartPath = "/api/v1/client/login/start"
	DeviceLoginPollPath  = "/api/v1/client/login/poll"
	DeviceLoginPagePath  = "/link"
)

// deviceRequestTimeout is how long one request of the sign-in may take, from
// the connection to the last byte of the answer. Tests shorten it.
var deviceRequestTimeout = 15 * time.Second

const (
	// maxDeviceBody is the largest answer that is read.
	maxDeviceBody = 64 << 10
	// pollMargin is added to the relay's interval: a poll that arrives a
	// moment early is answered with slow_down.
	pollMargin = 500 * time.Millisecond
	// slowDownStep is what slow_down adds to the interval, as the device
	// grant (RFC 8628) says.
	slowDownStep = 5 * time.Second
	// The relay's timing is taken within these bounds.
	defaultPollInterval = 5 * time.Second
	minPollInterval     = time.Second
	maxPollInterval     = time.Minute
	defaultLoginLife    = 10 * time.Minute
	maxLoginLife        = time.Hour
	// maxPollFailures is how many polls in a row may fail before the wait
	// gives up.
	maxPollFailures = 3
)

var (
	// ErrNoBrowserLogin says the relay has no sign-in endpoints: it is older
	// than browser sign-in.
	ErrNoBrowserLogin = errors.New("this relay does not support browser sign-in")
	// ErrLoginDenied says the request was denied in the dashboard.
	ErrLoginDenied = errors.New("the sign-in was denied in the dashboard")
	// ErrLoginExpired says the request's life ended before it was approved.
	ErrLoginExpired = errors.New("the sign-in code expired")
	// ErrLoginBusy says the relay holds as many pending requests as it allows.
	ErrLoginBusy = errors.New("the relay has too many pending sign-ins; try again in a minute")
	// ErrNotASignIn says the relay answered with something that is not what
	// the sign-in endpoints answer.
	ErrNotASignIn = errors.New("the relay's answer is not a sign-in answer")
)

// DeviceStatusError is an answer of a sign-in endpoint with a status that has
// no meaning there, redirects included: they are never followed.
type DeviceStatusError struct{ Status int }

func (e *DeviceStatusError) Error() string {
	return "the relay answered the sign-in with status " + strconv.Itoa(e.Status)
}

// DeviceLogin signs a machine in through the browser: Start opens a request
// on the relay, a person approves it in the dashboard, Wait collects the
// token.
type DeviceLogin struct {
	// HTTP supplies the transport (and with it the TLS settings). One that
	// keeps connections alive saves a handshake per poll.
	HTTP *http.Client
	// Relay is the dashboard address, https://host[:port]. Nothing is sent to
	// another origin or without TLS.
	Relay string
	// Meta is what the approval page shows about this machine. TokenName is
	// sent only when it is set.
	Meta struct{ Hostname, OS, Arch, ClientVersion, TokenName string }
	// Sleep waits between two polls; nil waits on a timer. It returns the
	// context's error when the context ends first.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DeviceStart is an open sign-in request.
//
// DeviceCode is a secret like the token: it goes into the body of the polls
// and nowhere else. String, GoString and LogValue leave it out.
type DeviceStart struct {
	DeviceCode string
	// UserCode is what the person compares, as the relay formats it.
	UserCode string
	// VerificationURL is the page to open. It is always on the relay's own
	// origin.
	VerificationURL string
	// OwnURL says that the relay named a page that is not on its own origin
	// and VerificationURL is the relay's own /link instead. A relay that names
	// no page gets the same address without this being set.
	OwnURL              bool
	ExpiresIn, Interval time.Duration
}

func (s DeviceStart) String() string {
	return "DeviceStart{UserCode:" + s.UserCode + " DeviceCode:[redacted]}"
}

// GoString keeps %#v from printing the device code.
func (s DeviceStart) GoString() string { return s.String() }

// LogValue keeps slog from printing the device code.
func (s DeviceStart) LogValue() slog.Value { return slog.StringValue(s.String()) }

// DeviceToken is what an approved request gives, once. Token is the secret;
// String, GoString and LogValue leave it out. TokenName and Email are text
// fit for a terminal, or empty.
type DeviceToken struct{ Token, TokenName, Email string }

func (t DeviceToken) String() string {
	return "DeviceToken{TokenName:" + t.TokenName + " Token:[redacted]}"
}

// GoString keeps %#v from printing the token.
func (t DeviceToken) GoString() string { return t.String() }

// LogValue keeps slog from printing the token.
func (t DeviceToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

// origin checks the relay address and returns it as https://host[:port].
func (d DeviceLogin) origin() (string, error) {
	u, err := url.Parse(strings.TrimSpace(d.Relay))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("the relay address must be https://host or https://host:port")
	}
	return "https://" + u.Host, nil
}

// post sends one JSON request to the relay and returns the status and, for a
// 200, the answer. Redirects are not followed: the body may hold the device
// code. The errors do not repeat the request.
func (d DeviceLogin) post(ctx context.Context, target string, body []byte) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, deviceRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, errors.New("the relay address must be https://host or https://host:port")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "burrow/"+version.Version)

	hc := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if d.HTTP != nil {
		hc.Transport = d.HTTP.Transport
	}
	// ended names why the request stopped when a context did it.
	ended := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case rctx.Err() != nil:
			return rctx.Err()
		}
		// Without the *url.Error around it: that repeats the URL.
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			return ue.Err
		}
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, ended(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Only the status counts. What comes with it is read to its end, up to
		// the bound, so that the next poll can use the same connection.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDeviceBody))
		return resp.StatusCode, nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDeviceBody+1))
	if err != nil {
		return 0, nil, ended(err)
	}
	if len(raw) > maxDeviceBody {
		return 0, nil, ErrNotASignIn
	}
	return resp.StatusCode, raw, nil
}

// decodeOne reads raw as one JSON object and nothing after it.
func decodeOne(raw []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(into); err != nil {
		return ErrNotASignIn
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ErrNotASignIn
	}
	return nil
}

// Start opens a sign-in request. It answers ErrNoBrowserLogin for a relay
// without the endpoints (404), ErrLoginBusy when the relay holds too many
// pending requests (429), a *DeviceStatusError for another status,
// ErrNotASignIn for an answer that is not a request, and the transport's own
// error when no answer arrived.
func (d DeviceLogin) Start(ctx context.Context) (DeviceStart, error) {
	origin, err := d.origin()
	if err != nil {
		return DeviceStart{}, err
	}
	body, err := json.Marshal(struct {
		Hostname      string `json:"hostname"`
		OS            string `json:"os"`
		Arch          string `json:"arch"`
		ClientVersion string `json:"client_version"`
		TokenName     string `json:"token_name,omitempty"`
	}{d.Meta.Hostname, d.Meta.OS, d.Meta.Arch, d.Meta.ClientVersion, d.Meta.TokenName})
	if err != nil {
		return DeviceStart{}, err
	}
	status, raw, err := d.post(ctx, origin+DeviceLoginStartPath, body)
	switch {
	case err != nil:
		return DeviceStart{}, err
	case status == http.StatusNotFound || status == http.StatusMethodNotAllowed:
		return DeviceStart{}, ErrNoBrowserLogin
	case status == http.StatusTooManyRequests:
		return DeviceStart{}, ErrLoginBusy
	case status != http.StatusOK:
		return DeviceStart{}, &DeviceStatusError{Status: status}
	}
	var doc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := decodeOne(raw, &doc); err != nil {
		return DeviceStart{}, err
	}
	// Nothing of the answer is repeated in an error: it is not trusted.
	if doc.DeviceCode == "" || printable(doc.DeviceCode, 512) != doc.DeviceCode || !validUserCode(doc.UserCode) {
		return DeviceStart{}, ErrNotASignIn
	}
	s := DeviceStart{
		DeviceCode:      doc.DeviceCode,
		UserCode:        doc.UserCode,
		VerificationURL: doc.VerificationURL,
		ExpiresIn:       boundSeconds(doc.ExpiresIn, defaultLoginLife, time.Second, maxLoginLife),
		Interval:        boundSeconds(doc.Interval, defaultPollInterval, minPollInterval, maxPollInterval),
	}
	if !onOrigin(s.VerificationURL, origin) {
		s.OwnURL = strings.TrimSpace(s.VerificationURL) != ""
		s.VerificationURL = origin + DeviceLoginPagePath + "?code=" + s.UserCode
	}
	return s, nil
}

// boundSeconds turns a number of seconds from the relay into a duration
// between low and high; def when the relay gave none.
func boundSeconds(n int64, def, low, high time.Duration) time.Duration {
	switch {
	case n <= 0:
		return def
	case n > int64(high/time.Second):
		return high
	}
	return max(time.Duration(n)*time.Second, low)
}

// validUserCode accepts what a relay shows as a code: letters, digits and
// hyphens, 4 to 32 of them. It goes onto a terminal and into a URL.
func validUserCode(s string) bool {
	if len(s) < 4 || len(s) > 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// onOrigin reports whether raw is a plain https URL on origin
// (https://host[:port]): same host, same port, no credentials, no fragment,
// and made of nothing but letters, digits and -._~:/?=&%[]. It is shown on a
// terminal and handed to the program that opens the browser.
func onOrigin(raw, origin string) bool {
	if raw == "" || len(raw) > 512 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && strings.IndexByte("-._~:/?=&%[]", c) < 0 {
			return false
		}
	}
	u, err := url.Parse(raw)
	o, oerr := url.Parse(origin)
	if err != nil || oerr != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Fragment != "" || u.Hostname() == "" {
		return false
	}
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		return "443"
	}
	return strings.EqualFold(u.Hostname(), o.Hostname()) && port(u) == port(o)
}

// displayText keeps a value from the relay fit for a terminal: printable
// characters, at most max bytes; anything else gives "".
func displayText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max || !utf8.ValidString(s) {
		return ""
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return ""
		}
	}
	return s
}

// sleepContext waits for d or until ctx ends, whichever is first.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Wait polls the relay until the request is decided and returns the token of
// an approved one.
//
// It waits a little longer than the relay's interval between two polls and
// longer still after each slow_down. It ends with ErrLoginDenied (403),
// ErrLoginExpired (410, or the request's life is over; then without asking
// again), ErrNotASignIn for a 200 that holds no token, the context's error as
// soon as the context ends, and the last error after three polls in a row
// that brought no answer the sign-in knows.
func (d DeviceLogin) Wait(ctx context.Context, s DeviceStart) (DeviceToken, error) {
	origin, err := d.origin()
	if err != nil {
		return DeviceToken{}, err
	}
	sleep := d.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	body, err := json.Marshal(struct {
		DeviceCode string `json:"device_code"`
	}{s.DeviceCode})
	if err != nil {
		return DeviceToken{}, err
	}
	interval := min(max(s.Interval, minPollInterval), maxPollInterval)
	var elapsed time.Duration
	failures := 0
	for {
		wait := interval + pollMargin
		if elapsed+wait >= s.ExpiresIn {
			// The next poll would come after the request's end.
			if rest := s.ExpiresIn - elapsed; rest > 0 {
				if err := sleep(ctx, rest); err != nil {
					return DeviceToken{}, err
				}
			}
			return DeviceToken{}, ErrLoginExpired
		}
		if err := sleep(ctx, wait); err != nil {
			return DeviceToken{}, err
		}
		began := time.Now()
		status, raw, err := d.post(ctx, origin+DeviceLoginPollPath, body)
		elapsed += wait + time.Since(began)
		if ctx.Err() != nil {
			return DeviceToken{}, ctx.Err()
		}
		switch {
		case errors.Is(err, ErrNotASignIn):
			return DeviceToken{}, err
		case err != nil:
			failures++
		case status == http.StatusOK:
			return deviceToken(raw)
		case status == http.StatusAccepted:
			failures = 0
		case status == http.StatusTooManyRequests:
			failures = 0
			interval = min(interval+slowDownStep, maxPollInterval)
		case status == http.StatusForbidden:
			return DeviceToken{}, ErrLoginDenied
		case status == http.StatusGone:
			return DeviceToken{}, ErrLoginExpired
		default:
			failures++
			err = &DeviceStatusError{Status: status}
		}
		if failures >= maxPollFailures {
			return DeviceToken{}, err
		}
	}
}

// deviceToken reads the answer of an approved request.
func deviceToken(raw []byte) (DeviceToken, error) {
	var doc struct {
		Token     string `json:"token"`
		TokenName string `json:"token_name"`
		Email     string `json:"email"`
	}
	if err := decodeOne(raw, &doc); err != nil {
		return DeviceToken{}, err
	}
	// The rule `login --token` applies to a pasted token.
	if !ValidToken(doc.Token) {
		return DeviceToken{}, ErrNotASignIn
	}
	return DeviceToken{
		Token:     doc.Token,
		TokenName: displayText(doc.TokenName, 120),
		Email:     displayText(doc.Email, 254),
	}, nil
}
