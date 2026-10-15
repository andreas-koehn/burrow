// generic.go — the three tools that reach every operation: find one,
// describe it, call it. The curated tools cover the common tasks; these keep
// the rest of the API available without 130 tool descriptions in the agent's
// context.

package mcpserv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Names of the generic tools.
const (
	toolFindOperations    = "burrow_find_operations"
	toolDescribeOperation = "burrow_describe_operation"
	toolCallOperation     = "burrow_call_operation"
)

const (
	defaultFindLimit = 25
	maxFindLimit     = 100
)

const (
	findDescription     = "Search the operations of the Burrow API that this token may call, by words in their name, path or summary. Use it when no other tool fits. Then call burrow_describe_operation for the arguments, and burrow_call_operation to run it."
	describeDescription = `Show what an operation does and the arguments it takes, as a JSON Schema. Arguments go to burrow_call_operation exactly in this shape: path and query parameters at the top level, the request body under "body".`
	callDescription     = "Run any operation this token may call. Writing operations accept dry_run: true to check the arguments without changing anything; operations that remove something need confirm. Prefer a dedicated tool when one exists."
)

var (
	findSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Words to look for in the operation's name, path, summary and tags, all of which must match. May be empty to list everything."},
    "kind": {"type": "string", "enum": ["read", "write", "delete"], "description": "Only operations of this kind."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 25, "description": "At most this many operations."}
  },
  "required": ["query"],
  "additionalProperties": false
}`)
	describeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation_id": {"type": "string", "description": "The operation_id from burrow_find_operations."}
  },
  "required": ["operation_id"],
  "additionalProperties": false
}`)
	callSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "operation_id": {"type": "string", "description": "The operation_id from burrow_find_operations."},
    "arguments": {"type": "object", "description": "The arguments as burrow_describe_operation shows them."}
  },
  "required": ["operation_id"],
  "additionalProperties": false
}`)
)

// genericDescriptors are the three tools as Tools() lists them.
func genericDescriptors() []ToolDescriptor {
	return []ToolDescriptor{
		{Name: toolFindOperations, Description: findDescription, ParametersSchema: findSchema, Permissions: []string{}, Kind: "read", Generic: true},
		{Name: toolDescribeOperation, Description: describeDescription, ParametersSchema: describeSchema, Permissions: []string{}, Kind: "read", Generic: true},
		{Name: toolCallOperation, Description: callDescription, ParametersSchema: callSchema, Permissions: []string{}, Kind: "write", Generic: true},
	}
}

type genericTool struct {
	tool    *mcp.Tool
	handler mcp.ToolHandler
}

// genericTools returns the three generic tools for tok.
func (s *Server) genericTools(tok *authedToken) []genericTool {
	f, no := false, false
	yes := true
	find := &mcp.Tool{
		Name: toolFindOperations, Description: findDescription, InputSchema: findSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &f, IdempotentHint: true, OpenWorldHint: &no},
	}
	describe := &mcp.Tool{
		Name: toolDescribeOperation, Description: describeDescription, InputSchema: describeSchema,
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &f, IdempotentHint: true, OpenWorldHint: &no},
	}
	call := &mcp.Tool{
		Name: toolCallOperation, Description: callDescription, InputSchema: callSchema,
		// It can run a delete: a client that asks before destructive tools asks here.
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &yes, OpenWorldHint: &no},
	}
	return []genericTool{
		{find, s.callHandler(tok, toolFindOperations, func(_ context.Context, args map[string]any) (*mcp.CallToolResult, string, string) {
			res, word := s.findOperations(tok, args)
			return res, word, ""
		})},
		{describe, s.callHandler(tok, toolDescribeOperation, func(_ context.Context, args map[string]any) (*mcp.CallToolResult, string, string) {
			res, word := s.describeOperation(tok, args)
			return res, word, ""
		})},
		{call, s.callHandler(tok, toolCallOperation, func(ctx context.Context, args map[string]any) (*mcp.CallToolResult, string, string) {
			return s.callOperation(ctx, tok, args)
		})},
	}
}

// unknownOperation is the answer for an id that does not exist and for one
// the token may not use: the two cannot be told apart.
func unknownOperation(id string) error {
	return fmt.Errorf("unknown operation %s; find it with burrow_find_operations", id)
}

// usable looks op up and checks that tok may call it.
func (s *Server) usable(tok *authedToken, id string) (Operation, bool) {
	op, ok := s.reg.Lookup(id)
	if !ok || !tok.mayUse(op) {
		return Operation{}, false
	}
	return op, true
}

// stringArg reads a required string argument.
func stringArg(args map[string]any, name string) (string, error) {
	v, ok := args[name]
	if !ok {
		return "", fmt.Errorf("invalid argument %s: required", name)
	}
	str, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid argument %s: must be a string", name)
	}
	return str, nil
}

// onlyKeys refuses arguments the tool does not take.
func onlyKeys(args map[string]any, allowed ...string) error {
outer:
	for k := range args {
		for _, a := range allowed {
			if k == a {
				continue outer
			}
		}
		return fmt.Errorf("invalid argument %s: not an argument of this tool", k)
	}
	return nil
}

func (s *Server) findOperations(tok *authedToken, args map[string]any) (*mcp.CallToolResult, string) {
	bad := func(err error) (*mcp.CallToolResult, string) { return errorResult(err), "error" }
	if err := onlyKeys(args, "query", "kind", "limit"); err != nil {
		return bad(err)
	}
	query, err := stringArg(args, "query")
	if err != nil {
		return bad(err)
	}
	kind := ""
	if v, ok := args["kind"]; ok {
		kind, _ = v.(string)
		if kind != "read" && kind != "write" && kind != "delete" {
			return bad(errors.New(`invalid argument kind: must be "read", "write" or "delete"`))
		}
	}
	limit := defaultFindLimit
	if v, ok := args["limit"]; ok {
		n, isNum := v.(float64)
		if !isNum || n != float64(int(n)) || n < 1 || n > maxFindLimit {
			return bad(fmt.Errorf("invalid argument limit: must be an integer from 1 to %d", maxFindLimit))
		}
		limit = int(n)
	}
	terms := strings.Fields(strings.ToLower(query))

	type found struct {
		OperationID string  `json:"operation_id"`
		Tool        *string `json:"tool"`
		Kind        string  `json:"kind"`
		Method      string  `json:"method"`
		Path        string  `json:"path"`
		Summary     string  `json:"summary"`
	}
	ordered := make([]Operation, 0, len(s.reg.Operations()))
	listed := map[string]bool{}
	for _, op := range s.curated {
		ordered = append(ordered, op)
		listed[op.ID] = true
	}
	for _, op := range s.reg.Operations() {
		if !listed[op.ID] {
			ordered = append(ordered, op)
		}
	}
	matches := []found{}
	total := 0
	for _, op := range ordered {
		if !tok.mayUse(op) || (kind != "" && op.Kind() != kind) {
			continue
		}
		_, curated := s.curatedByName[ToolName(op.ID)]
		hay := strings.ToLower(strings.Join(append([]string{op.ID, op.Path, op.Summary}, op.Tags...), " "))
		if curated {
			hay += " " + ToolName(op.ID)
		}
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		total++
		if len(matches) < limit {
			f := found{OperationID: op.ID, Kind: op.Kind(), Method: op.Method, Path: op.Path, Summary: op.Summary}
			if curated {
				name := ToolName(op.ID)
				f.Tool = &name
			}
			matches = append(matches, f)
		}
	}
	out := map[string]any{"operations": matches, "total": total}
	if total > limit {
		out["note"] = fmt.Sprintf("Showing %d of %d; add words to the query or raise limit.", limit, total)
	}
	return jsonResult(out), "ok"
}

func (s *Server) describeOperation(tok *authedToken, args map[string]any) (*mcp.CallToolResult, string) {
	if err := onlyKeys(args, "operation_id"); err != nil {
		return errorResult(err), "error"
	}
	id, err := stringArg(args, "operation_id")
	if err != nil {
		return errorResult(err), "error"
	}
	op, ok := s.usable(tok, id)
	if !ok {
		return errorResult(unknownOperation(id)), "denied"
	}
	perms := []string{}
	for _, p := range op.Access.AnyOf {
		perms = append(perms, string(p))
	}
	out := map[string]any{
		"operation_id": op.ID,
		"tool":         nil,
		"kind":         op.Kind(),
		"method":       op.Method,
		"path":         op.Path,
		"summary":      op.Summary,
		"description":  strings.TrimSpace(op.Description),
		"permissions":  perms,
		"input_schema": op.InputSchema(),
	}
	if _, curated := s.curatedByName[ToolName(op.ID)]; curated {
		out["tool"] = ToolName(op.ID)
	}
	if op.Destructive {
		if n := op.ConfirmValueName(); n != "" {
			out["confirm"] = n
		} else {
			out["confirm"] = op.ID
		}
	}
	return jsonResult(out), "ok"
}

func (s *Server) callOperation(ctx context.Context, tok *authedToken, args map[string]any) (*mcp.CallToolResult, string, string) {
	if err := onlyKeys(args, "operation_id", "arguments"); err != nil {
		return errorResult(err), "error", ""
	}
	id, err := stringArg(args, "operation_id")
	if err != nil {
		return errorResult(err), "error", ""
	}
	op, ok := s.usable(tok, id)
	if !ok {
		return errorResult(unknownOperation(id)), "denied", ""
	}
	callArgs := map[string]any{}
	if v, present := args["arguments"]; present && v != nil {
		m, isObj := v.(map[string]any)
		if !isObj {
			return errorResult(errors.New("invalid argument arguments: must be an object")), "error", toolCallOperation + "/" + op.ID
		}
		callArgs = m
	}
	res, word := s.invoke(ctx, tok, op, callArgs)
	return res, word, toolCallOperation + "/" + op.ID
}
