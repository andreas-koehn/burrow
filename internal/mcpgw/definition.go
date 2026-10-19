package mcpgw

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/db"
)

// definitionKeys are the fields of an upstream tool that are pinned (D9):
// the hash covers them, and they are all that is stored and later served.
// Anything else the upstream sends (_meta, icons, …) is dropped, so nothing
// unpinned can reach a client.
var definitionKeys = []string{"name", "title", "description", "inputSchema", "outputSchema", "annotations"}

// MaxDefinitionBytes caps one tool's canonical definition; a larger tool is
// skipped by the sync.
const MaxDefinitionBytes = 64 << 10

// toolNameRe is the set of upstream tool names the gateway accepts: the
// characters MCP recommends for tool names, at most 128 of them. A name is
// stored, shown and used in API paths, so anything else is skipped.
var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// ValidToolName reports whether an upstream tool name is accepted.
func ValidToolName(name string) bool { return toolNameRe.MatchString(name) }

var errDefinitionTooLarge = errors.New("definition too large")

// Canonical returns the canonical JSON of the pinned fields of an upstream
// tool and its sha256 (hex). Canonical JSON is compact with object keys
// sorted at every level, so the same definition always hashes the same.
func Canonical(t *mcp.Tool) (definition, hash string, err error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return "", "", fmt.Errorf("marshal tool: %w", err)
	}
	return canonicalJSON(raw)
}

func canonicalJSON(raw []byte) (definition, hash string, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // numbers keep their text: no float rounding between syncs
	var all map[string]any
	if err := dec.Decode(&all); err != nil {
		return "", "", fmt.Errorf("decode tool: %w", err)
	}
	pinned := make(map[string]any, len(definitionKeys))
	for _, k := range definitionKeys {
		if v, ok := all[k]; ok && v != nil {
			pinned[k] = v
		}
	}
	out, err := json.Marshal(pinned) // map keys are sorted by encoding/json
	if err != nil {
		return "", "", fmt.Errorf("encode tool: %w", err)
	}
	if len(out) > MaxDefinitionBytes {
		return "", "", errDefinitionTooLarge
	}
	sum := sha256.Sum256(out)
	return string(out), hex.EncodeToString(sum[:]), nil
}

// Definition is a stored (canonical) tool definition, decoded.
type Definition struct {
	Name         string          `json:"name"`
	Title        string          `json:"title,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	Annotations  json.RawMessage `json:"annotations,omitempty"`
}

// ParseDefinition decodes a stored definition.
func ParseDefinition(s string) (Definition, error) {
	var d Definition
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		return Definition{}, fmt.Errorf("decode tool definition: %w", err)
	}
	return d, nil
}

// ReadOnlyHint returns the upstream's readOnlyHint annotation (false when
// absent or not a boolean).
func (d Definition) ReadOnlyHint() bool {
	var a struct {
		ReadOnlyHint *bool `json:"readOnlyHint"`
	}
	if len(d.Annotations) == 0 || json.Unmarshal(d.Annotations, &a) != nil || a.ReadOnlyHint == nil {
		return false
	}
	return *a.ReadOnlyHint
}

// Tool is a tool as the gateway serves it: the approved definition with the
// operator's overrides applied.
type Tool struct {
	Source string
	// Native is the upstream's name: the name CallTool sends upstream.
	Native string
	// Name is the name served on /mcp/<source>: the rename override, or Native.
	Name         string
	Title        string
	Description  string // the description override, or the approved description
	InputSchema  json.RawMessage
	OutputSchema json.RawMessage
	Annotations  json.RawMessage
	// ReadOnly is the operator's classification (D8): the override, or the
	// upstream's readOnlyHint.
	ReadOnly bool
}

// Served returns the tool as the gateway serves it, and false when it must
// not be served: not approved (pending or removed), hidden, or its approved
// definition cannot be read. Only the approved definition is ever used, never
// the current one.
func Served(t db.MCPTool) (Tool, bool) {
	if t.Status != db.MCPToolApproved || t.Hidden || t.ApprovedDefinition == "" {
		return Tool{}, false
	}
	d, err := ParseDefinition(t.ApprovedDefinition)
	if err != nil || d.Name != t.Name {
		return Tool{}, false
	}
	out := Tool{
		Source: t.SourceSlug, Native: t.Name, Name: t.Name,
		Title: d.Title, Description: d.Description,
		InputSchema: d.InputSchema, OutputSchema: d.OutputSchema, Annotations: d.Annotations,
		ReadOnly: d.ReadOnlyHint(),
	}
	if t.Rename != "" {
		out.Name = t.Rename
	}
	if t.DescriptionOverride != "" {
		out.Description = t.DescriptionOverride
	}
	if t.ReadOnly != nil {
		out.ReadOnly = *t.ReadOnly
	}
	return out, true
}

// GuardrailHit is one guardrail pattern that matched a field of a tool's
// definition during a sync (D9). Stored as JSON in mcp_tools.guardrail_hits.
type GuardrailHit struct {
	Field   string `json:"field"` // "title", "description", "input_schema", "output_schema" or "annotations"
	Pattern string `json:"pattern"`
}

// ParseGuardrailHits decodes mcp_tools.guardrail_hits (never nil).
func ParseGuardrailHits(s string) []GuardrailHit {
	out := []GuardrailHit{}
	_ = json.Unmarshal([]byte(s), &out)
	if out == nil {
		out = []GuardrailHit{}
	}
	return out
}
