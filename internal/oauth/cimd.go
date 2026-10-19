package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/aiprovider"
)

// Client ID Metadata Document fetching (spec D25).
const (
	cimdTimeout  = 5 * time.Second
	cimdMaxBytes = 64 << 10
	cimdMinTTL   = 5 * time.Minute
	cimdMaxTTL   = 24 * time.Hour
	cimdMaxCache = 1000
)

// clientDoc is the part of a client metadata document Burrow reads.
type clientDoc struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

type cimdEntry struct {
	doc     clientDoc
	expires time.Time
}

type cimdFetcher struct {
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	cache map[string]cimdEntry
}

// newCIMDClient is the default document client: public addresses only, even
// with BURROW_AI_ALLOW_PRIVATE_UPSTREAMS set (the URL comes from whoever
// starts an authorization), 5 s for everything, redirects not followed.
func newCIMDClient() *http.Client {
	tr := aiprovider.NewTransport(false)
	tr.ResponseHeaderTimeout = cimdTimeout
	return &http.Client{
		Transport: tr,
		Timeout:   cimdTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newCIMDFetcher(client *http.Client, now func() time.Time) *cimdFetcher {
	if client == nil {
		client = newCIMDClient()
	}
	// The policy holds whatever client does the transport: no redirects,
	// 5 s at most.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if c.Timeout <= 0 || c.Timeout > cimdTimeout {
		c.Timeout = cimdTimeout
	}
	return &cimdFetcher{client: &c, now: now, cache: map[string]cimdEntry{}}
}

// IsMetadataClientID reports whether a client_id is the URL of a client
// metadata document (and not the id of a pre-registered client).
func IsMetadataClientID(id string) bool { return strings.HasPrefix(id, "https://") }

// errCIMD is a client metadata document Burrow refuses; the message is shown
// on the error page (it names no secret).
type errCIMD struct{ msg string }

func (e errCIMD) Error() string { return "client metadata document: " + e.msg }

func cimdErr(format string, a ...any) error { return errCIMD{fmt.Sprintf(format, a...)} }

// validCIMDURL checks the client_id URL itself: https, a path other than "/",
// no query, fragment, credentials or dot segments.
func validCIMDURL(id string) error {
	u, err := aiprovider.ValidateBaseURL(id)
	if err != nil {
		return cimdErr("client_id is not a usable https URL")
	}
	if u.Path == "" || u.Path == "/" {
		return cimdErr("client_id must have a path")
	}
	return nil
}

// fetch returns the document of the client_id URL, from the cache while it
// is fresh.
func (f *cimdFetcher) fetch(ctx context.Context, id string) (clientDoc, error) {
	if err := validCIMDURL(id); err != nil {
		return clientDoc{}, err
	}
	now := f.now()
	f.mu.Lock()
	if e, ok := f.cache[id]; ok && now.Before(e.expires) {
		f.mu.Unlock()
		return e.doc, nil
	}
	f.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, cimdTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return clientDoc{}, cimdErr("client_id is not a usable https URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		if errors.Is(err, aiprovider.ErrBlockedAddress) {
			return clientDoc{}, cimdErr("the client_id host is not a public address")
		}
		return clientDoc{}, cimdErr("could not be fetched")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return clientDoc{}, cimdErr("fetch answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return clientDoc{}, cimdErr("could not be read")
	}
	if len(body) > cimdMaxBytes {
		return clientDoc{}, cimdErr("larger than 64 KiB")
	}
	var doc clientDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return clientDoc{}, cimdErr("not valid JSON")
	}
	if doc.ClientID != id {
		return clientDoc{}, cimdErr("its client_id does not equal the URL it was fetched from")
	}
	doc.ClientName = strings.TrimSpace(doc.ClientName)
	if doc.ClientName == "" || len(doc.ClientName) > 200 {
		return clientDoc{}, cimdErr("client_name is missing")
	}
	if len(doc.RedirectURIs) == 0 || len(doc.RedirectURIs) > 32 {
		return clientDoc{}, cimdErr("redirect_uris is missing")
	}
	for _, u := range doc.RedirectURIs {
		if !ValidRedirectURI(u) {
			return clientDoc{}, cimdErr("a redirect URI is not https, a loopback http URL or an app scheme")
		}
	}
	if m := doc.TokenEndpointAuthMethod; m != "" && m != "none" {
		return clientDoc{}, cimdErr("token_endpoint_auth_method %q is not supported (public clients only)", m)
	}

	ttl := cacheTTL(resp.Header, now)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cache) >= cimdMaxCache {
		for k, e := range f.cache {
			if !now.Before(e.expires) {
				delete(f.cache, k)
			}
		}
		if len(f.cache) >= cimdMaxCache {
			f.cache = map[string]cimdEntry{}
		}
	}
	f.cache[id] = cimdEntry{doc: doc, expires: now.Add(ttl)}
	return doc, nil
}

// cacheTTL is how long a document may be kept: max-age or Expires, held
// within 5 minutes and 24 hours (no-store and no-cache count as 0, so 5
// minutes).
func cacheTTL(h http.Header, now time.Time) time.Duration {
	ttl := time.Duration(0)
	cc := strings.ToLower(h.Get("Cache-Control"))
	found := false
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if v, ok := strings.CutPrefix(part, "max-age="); ok {
			if n, err := strconv.Atoi(strings.Trim(v, `"`)); err == nil && n >= 0 {
				ttl, found = time.Duration(n)*time.Second, true
			}
		}
		if part == "no-store" || part == "no-cache" {
			ttl, found = 0, true
			break
		}
	}
	if !found {
		if exp, err := http.ParseTime(h.Get("Expires")); err == nil {
			ttl = exp.Sub(now)
		}
	}
	return min(max(ttl, cimdMinTTL), cimdMaxTTL)
}
