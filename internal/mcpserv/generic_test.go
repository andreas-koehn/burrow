package mcpserv

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/authz"
)

type foundOps struct {
	Operations []struct {
		OperationID string  `json:"operation_id"`
		Tool        *string `json:"tool"`
		Kind        string  `json:"kind"`
	} `json:"operations"`
	Total int    `json:"total"`
	Note  string `json:"note"`
}

func TestGenericToolsAreListedForEveryToken(t *testing.T) {
	hs, _ := newDispatchServer(t, &fakeAPI{}, []string{})
	tools := listedTools(t, connectMCP(t, hs.URL, "", testRevisions[0]))
	for _, name := range []string{"burrow_find_operations", "burrow_describe_operation", "burrow_call_operation"} {
		if tools[name] == nil {
			t.Errorf("a token without permissions does not see %s", name)
		}
	}
	if !tools["burrow_find_operations"].Annotations.ReadOnlyHint || tools["burrow_call_operation"].Annotations.ReadOnlyHint {
		t.Error("annotations of the generic tools are wrong")
	}
}

func TestFindListsOnlyCallableOperations(t *testing.T) {
	hs, sink := newDispatchServer(t, &fakeAPI{}, presetPerms(authz.PresetReadOnly))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	res, err := callTool(t, cs, "burrow_find_operations", map[string]any{"query": "provider"})
	if err != nil || res.IsError {
		t.Fatalf("find: %v %+v", err, res)
	}
	var found foundOps
	if err := json.Unmarshal([]byte(resultText(t, res)), &found); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, op := range found.Operations {
		ids[op.OperationID] = true
		if op.Kind != "read" {
			t.Errorf("read-only token found %s (%s)", op.OperationID, op.Kind)
		}
	}
	if !ids["providersList"] || !ids["providerGet"] || ids["providerCreate"] || ids["providerDelete"] {
		t.Errorf("found = %v", ids)
	}
	if rows := sink.all(); rows[0].SubjectLabel != "burrow_find_operations" || rows[0].Result != "ok" {
		t.Errorf("audit = %+v", rows)
	}

	res, _ = callTool(t, cs, "burrow_find_operations", map[string]any{"query": "", "limit": 3})
	_ = json.Unmarshal([]byte(resultText(t, res)), &found)
	if len(found.Operations) != 3 || found.Total <= 3 || !strings.Contains(found.Note, "Showing 3 of") {
		t.Errorf("limit: %d of %d, note %q", len(found.Operations), found.Total, found.Note)
	}
}

func TestDescribeAndCallHideWhatTheTokenMayNotUse(t *testing.T) {
	api := &fakeAPI{}
	hs, sink := newDispatchServer(t, api, presetPerms(authz.PresetReadOnly))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])
	for _, id := range []string{"providerCreate", "gatewayKeyCreate", "noSuchOperation"} {
		d, _ := callTool(t, cs, "burrow_describe_operation", map[string]any{"operation_id": id})
		c, _ := callTool(t, cs, "burrow_call_operation", map[string]any{"operation_id": id, "arguments": map[string]any{}})
		if !d.IsError || !c.IsError {
			t.Errorf("%s: describe isError=%v call isError=%v", id, d.IsError, c.IsError)
		}
		if resultText(t, d) != resultText(t, c) {
			t.Errorf("%s: describe and call answer differently: %q / %q", id, resultText(t, d), resultText(t, c))
		}
	}
	if api.calls() != 0 {
		t.Fatalf("%d requests reached the API", api.calls())
	}
	rows := sink.all()
	if last := rows[len(rows)-1]; last.SubjectLabel != "burrow_call_operation" || last.Result != "denied" {
		t.Errorf("last audit row = %+v", last)
	}
}

func TestDescribeThenCall(t *testing.T) {
	api := &fakeAPI{reply: `{"ok":true}`}
	hs, sink := newDispatchServer(t, api, presetPerms(authz.PresetOperator))
	cs := connectMCP(t, hs.URL, "", testRevisions[0])

	d, _ := callTool(t, cs, "burrow_describe_operation", map[string]any{"operation_id": "budgetDelete"})
	var desc map[string]any
	if err := json.Unmarshal([]byte(resultText(t, d)), &desc); err != nil {
		t.Fatal(err)
	}
	if desc["kind"] != "delete" || desc["confirm"] != "id" || desc["input_schema"] == nil || desc["tool"] != nil {
		t.Errorf("describe budgetDelete = %v", desc)
	}

	c, _ := callTool(t, cs, "burrow_call_operation", map[string]any{"operation_id": "budgetDelete", "arguments": map[string]any{"id": "b1"}})
	if !c.IsError || api.calls() != 0 {
		t.Errorf("an unconfirmed delete through call: isError=%v calls=%d", c.IsError, api.calls())
	}
	c, _ = callTool(t, cs, "burrow_call_operation", map[string]any{"operation_id": "budgetDelete", "arguments": map[string]any{"id": "b1", "confirm": "b1"}})
	if c.IsError || api.calls() != 1 || api.seen[0].URL.Path != "/api/v1/budgets/b1" {
		t.Errorf("confirmed delete through call: %+v", c)
	}
	rows := sink.all()
	if last := rows[len(rows)-1]; last.SubjectLabel != "burrow_call_operation/budgetDelete" || last.Result != "ok" {
		t.Errorf("last audit row = %+v", last)
	}
}
