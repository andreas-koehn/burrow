package aigateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw"
)

// A Dialect is the API format an endpoint speaks. It is fixed by the URL
// prefix, before the body is read, and decides how errors and the model list
// look and which paths are inference. A request only ever reaches providers
// whose api_format equals the dialect's name.
type Dialect struct {
	Name       string
	WriteError aigw.ErrorWriter

	inferencePaths map[string]bool // POST paths routed by the body's "model"
	unmeteredPaths map[string]bool // inference paths that produce no usage row
	writeModels    func(w http.ResponseWriter, items []modelItem)
}

// modelItem is one entry of a model list, before it is shaped for a dialect.
type modelItem struct {
	ID          string
	OwnedBy     string
	DisplayName string
}

func (d *Dialect) inference(path string) bool { return d.inferencePaths[strings.TrimRight(path, "/")] }
func (d *Dialect) metered(path string) bool   { return !d.unmeteredPaths[strings.TrimRight(path, "/")] }

// DialectOpenAI is the OpenAI API: Chat Completions, Completions, Embeddings
// and Responses.
var DialectOpenAI = &Dialect{
	Name:       "openai",
	WriteError: WriteError,
	inferencePaths: map[string]bool{
		"/v1/chat/completions": true,
		"/v1/completions":      true,
		"/v1/embeddings":       true,
		"/v1/responses":        true,
	},
	writeModels: writeOpenAIModels,
}

// DialectAnthropic is the Anthropic Messages API. Counting tokens is routed
// by "model" like a message, but it is no inference: it produces no usage row
// and does not run through the chain.
var DialectAnthropic = &Dialect{
	Name:       "anthropic",
	WriteError: WriteAnthropicError,
	inferencePaths: map[string]bool{
		"/v1/messages":              true,
		"/v1/messages/count_tokens": true,
	},
	unmeteredPaths: map[string]bool{"/v1/messages/count_tokens": true},
	writeModels:    writeAnthropicModels,
}

var dialects = map[string]*Dialect{
	DialectOpenAI.Name:    DialectOpenAI,
	DialectAnthropic.Name: DialectAnthropic,
}

// DialectByName returns the dialect with that name.
func DialectByName(name string) (*Dialect, bool) {
	d, ok := dialects[name]
	return d, ok
}

// writeOpenAIModels writes a model list in the OpenAI shape. Ids are stored
// data; the encoder escapes them.
func writeOpenAIModels(w http.ResponseWriter, items []modelItem) {
	type item struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := struct {
		Object string `json:"object"`
		Data   []item `json:"data"`
	}{Object: "list", Data: make([]item, len(items))}
	for i, m := range items {
		out.Data[i] = item{ID: m.ID, Object: "model", OwnedBy: m.OwnedBy}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// writeAnthropicModels writes a model list in the Anthropic shape. The whole
// list is one page.
func writeAnthropicModels(w http.ResponseWriter, items []modelItem) {
	type item struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		CreatedAt   string `json:"created_at"`
	}
	out := struct {
		Data    []item  `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}{Data: make([]item, len(items))}
	for i, m := range items {
		name := m.DisplayName
		if name == "" {
			name = m.ID
		}
		// Burrow does not know when a model was released; the shape requires the field.
		out.Data[i] = item{Type: "model", ID: m.ID, DisplayName: name, CreatedAt: "1970-01-01T00:00:00Z"}
	}
	if len(items) > 0 {
		out.FirstID, out.LastID = &items[0].ID, &items[len(items)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
