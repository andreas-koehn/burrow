package api

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"gopkg.in/yaml.v3"
)

// openapiYAML is the hand-written OpenAPI v3 doc, embedded at build time.
// The canonical source lives at docs/openapi.yaml; a byte-identical mirror
// is committed at internal/api/openapi.yaml because go:embed cannot reach
// outside the package directory. TestOpenAPI_EmbedFresh keeps the two
// copies in sync.
//
//go:embed openapi.yaml
var openapiYAML []byte

// OpenAPIDocument returns the embedded OpenAPI document. The MCP server
// builds its tools from it.
func OpenAPIDocument() []byte { return openapiYAML }

// GetOpenAPIYAML serves the embedded OpenAPI YAML at
// GET /api/v1/openapi.yaml. Public (no auth) so SDK code-generators can
// curl the spec without bootstrapping a session.
func (d Deps) GetOpenAPIYAML(w http.ResponseWriter, _ *http.Request) {
	// "application/yaml" is the IANA-registered media type (RFC 9512);
	// generators (openapi-generator-cli, redocly) all accept it.
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(openapiYAML)
}

// GetOpenAPIJSON serves the same doc converted to JSON at request time
// at GET /api/v1/openapi.json. Conversion goes yaml.v3 → any →
// encoding/json — no new module dependency, and the source-of-truth
// stays the hand-written YAML.
//
// We convert per-request rather than once at startup because the
// embed.FS payload is small (~30 KB), the conversion is microseconds,
// and tests that rebuild the embed don't need a separate cache-bust path.
func (d Deps) GetOpenAPIJSON(w http.ResponseWriter, _ *http.Request) {
	var doc any
	if err := yaml.Unmarshal(openapiYAML, &doc); err != nil {
		// Embedded YAML is verified at test time (TestOpenAPI_RouteCoverage
		// already unmarshals it). A failure here would mean a corrupt
		// binary — we still respond with a clean JSON error rather than
		// a panic.
		writeErr(w, http.StatusInternalServerError, "openapi yaml unmarshal failed")
		return
	}
	// yaml.v3 unmarshals mappings into map[string]any by default (not
	// map[interface{}]interface{} like yaml.v2), so encoding/json can
	// marshal the value directly with no key-type coercion.
	body, err := json.Marshal(doc)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "openapi json marshal failed")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
