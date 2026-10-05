package aiprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Model is one entry of a provider's model list.
type Model struct {
	ID            string
	DisplayName   string
	ContextLength int64
}

const (
	// MaxModelIDLen bounds a model id, synced or added by hand.
	MaxModelIDLen = 200
	// MaxModels bounds one provider's catalog.
	MaxModels = 5000

	maxModelNameLen = 200
	maxModelsBody   = 8 << 20
	modelsTimeout   = 30 * time.Second
)

// ValidModelID reports whether id can be stored in a model catalog: valid
// UTF-8 of 1..MaxModelIDLen bytes, no control characters, no surrounding
// space. Ids are data; "/" and ":" are common in them.
func ValidModelID(id string) bool {
	if id == "" || len(id) > MaxModelIDLen || !utf8.ValidString(id) || strings.TrimSpace(id) != id {
		return false
	}
	return !strings.ContainsFunc(id, unicode.IsControl)
}

// cleanModelName returns a display name fit to store: a name with control
// characters or invalid UTF-8 is dropped, a long one is cut.
func cleanModelName(name string) string {
	if !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return ""
	}
	name = strings.TrimSpace(name)
	for len(name) > maxModelNameLen {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// FetchModels reads the provider's model list (GET <base>/models, the OpenAI
// list shape). Only ids, names and context lengths are kept; prices in the
// response are ignored on purpose — Burrow never takes prices from the network.
//
// The answer is untrusted: entries without a usable id are skipped, repeated
// ids collapse to the first, and an answer that is too large or lists more
// than MaxModels models is an error. Error texts carry neither the transport's
// error nor anything the upstream sent, so they are safe to show to an admin.
func FetchModels(ctx context.Context, cfg Config, v Vault, rt http.RoundTripper) ([]Model, error) {
	// A nil transport would make the client fall back to
	// http.DefaultTransport: no address guard, environment proxies honoured.
	if isNil(rt) || isNil(v) {
		return nil, fmt.Errorf("fetch models: transport or vault is missing")
	}
	base, err := ValidateBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	header, format := cfg.AuthHeader, cfg.AuthFormat
	if header == "" {
		header = "Authorization"
	}
	if format == "" {
		format = "Bearer {key}"
	}
	if !strings.Contains(format, "{key}") || !validHeaderName(header) {
		return nil, fmt.Errorf("fetch models: auth header or format is invalid")
	}
	for k, val := range cfg.ExtraHeaders {
		if !validHeaderName(k) || !validHeaderValue(val) {
			return nil, fmt.Errorf("fetch models: an extra header is invalid")
		}
	}
	key, ok := v.Get(cfg.CredentialSlot)
	if !ok || key == "" {
		return nil, ErrNotConfigured
	}
	credential := strings.Replace(format, "{key}", key, 1)
	if !validHeaderValue(credential) {
		return nil, fmt.Errorf("fetch models: the credential contains characters not allowed in a header")
	}
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	u := *base
	u.Path = UpstreamPath(base.Path, "/v1/models")
	u.RawPath = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch models: the request could not be built")
	}
	req.Header.Set("Accept", "application/json")
	for k, val := range cfg.ExtraHeaders {
		req.Header.Set(k, val)
	}
	// Last, and with Set: an extra header cannot add to or replace the credential.
	req.Header.Set(header, credential)

	// No redirects: a redirect would resend the credential elsewhere.
	client := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: the provider did not answer")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models: the provider answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody+1))
	if err != nil {
		return nil, fmt.Errorf("fetch models: reading the answer failed")
	}
	if len(raw) > maxModelsBody {
		return nil, fmt.Errorf("fetch models: the answer is larger than %d MiB", maxModelsBody>>20)
	}
	var list struct {
		Data *[]struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil || list.Data == nil {
		return nil, fmt.Errorf("fetch models: the answer is not an OpenAI-style model list")
	}
	out := make([]Model, 0, min(len(*list.Data), MaxModels))
	seen := make(map[string]bool, len(*list.Data))
	for _, m := range *list.Data {
		if !ValidModelID(m.ID) || seen[m.ID] {
			continue
		}
		if len(out) == MaxModels {
			return nil, fmt.Errorf("fetch models: the provider lists more than %d models", MaxModels)
		}
		seen[m.ID] = true
		out = append(out, Model{ID: m.ID, DisplayName: cleanModelName(m.Name), ContextLength: max(m.ContextLength, 0)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
