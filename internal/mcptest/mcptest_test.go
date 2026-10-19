package mcptest_test

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/mcptest"
)

func connect(t *testing.T, url string, hdr http.Header) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	hc := &http.Client{Transport: headerRT{hdr}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type headerRT struct{ h http.Header }

func (r headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range r.h {
		req.Header[k] = v
	}
	return http.DefaultTransport.RoundTrip(req)
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	return cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
}

func firstText(r *mcp.CallToolResult) string {
	return r.Content[0].(*mcp.TextContent).Text
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	m := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		m[tl.Name] = tl
	}
	return m
}

func TestToolsBehave(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		up := mcptest.NewServer(t, mcptest.Opts{Stateful: stateful})
		cs := connect(t, up.URL, nil)

		tools := toolNames(t, cs)
		if len(tools) != len(mcptest.DefaultTools()) {
			t.Fatalf("stateful=%v: %d tools", stateful, len(tools))
		}
		if tools["echo"].Annotations == nil || !tools["echo"].Annotations.ReadOnlyHint {
			t.Errorf("echo not read-only")
		}
		if tools["mutate"].Annotations != nil && tools["mutate"].Annotations.ReadOnlyHint {
			t.Errorf("mutate is read-only")
		}

		r, err := call(t, cs, "echo", map[string]any{"text": "hi"})
		if err != nil || firstText(r) != "echo: hi" {
			t.Fatalf("echo: %v %v", r, err)
		}
		r, err = call(t, cs, "fail", map[string]any{"message": "nope"})
		if err != nil || !r.IsError || firstText(r) != "nope" {
			t.Fatalf("fail: %+v %v", r, err)
		}
		r, err = call(t, cs, "big", map[string]any{"bytes": 1000})
		if err != nil || len(firstText(r)) != 1000 {
			t.Fatalf("big: %v", err)
		}
		r, err = call(t, cs, "big", nil)
		if err != nil || len(firstText(r)) != mcptest.DefaultBigBytes {
			t.Fatalf("big default: %v", err)
		}
		start := time.Now()
		if r, err = call(t, cs, "slow", map[string]any{"ms": 150}); err != nil || firstText(r) != "slept 150ms" {
			t.Fatalf("slow: %v", err)
		}
		if time.Since(start) < 150*time.Millisecond {
			t.Errorf("slow returned early")
		}
	}
}

func TestSlowHonoursCancel(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	cs := connect(t, up.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "slow", Arguments: map[string]any{"ms": 10000}})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("expected quick cancellation, got %v after %v", err, time.Since(start))
	}
}

func TestRecordsCredentialAndCalls(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{Stateful: true})
	cs := connect(t, up.URL, http.Header{"X-Api-Key": {"sekret"}})
	if _, err := call(t, cs, "echo", map[string]any{"text": "a"}); err != nil {
		t.Fatal(err)
	}
	calls := up.CallsTo("echo")
	if len(calls) != 1 || calls[0].Header.Get("X-Api-Key") != "sekret" || calls[0].SessionID == "" ||
		!strings.Contains(string(calls[0].Args), `"a"`) {
		t.Fatalf("calls = %+v", up.Calls())
	}
	var sawInit, sawCall bool
	for _, r := range up.Requests() {
		if r.Header.Get("X-Api-Key") != "sekret" {
			t.Errorf("request without credential: %+v", r)
		}
		sawInit = sawInit || r.RPC == "initialize"
		sawCall = sawCall || r.RPC == "tools/call"
	}
	if !sawInit || !sawCall {
		t.Errorf("rpc methods not recorded: %+v", up.Requests())
	}
	up.ResetRecords()
	if len(up.Calls()) != 0 || len(up.Requests()) != 0 {
		t.Error("ResetRecords")
	}
}

func TestAuthorize(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{Authorize: func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer ok"
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: up.URL, DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err == nil {
		t.Fatal("connected without credential")
	}
	connect(t, up.URL, http.Header{"Authorization": {"Bearer ok"}})
}

func TestStatelessKeepsNoSession(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	cs := connect(t, up.URL, nil)
	if _, err := call(t, cs, "echo", nil); err != nil {
		t.Fatal(err)
	}
	if id := up.Calls()[0].SessionID; id != "" {
		t.Errorf("stateless session id %q", id)
	}
	up.Restart() // no effect on a stateless upstream
	if _, err := call(t, cs, "echo", nil); err != nil {
		t.Fatalf("call after restart: %v", err)
	}
}

func TestStatefulSessionLostAfterRestart(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{Stateful: true})
	cs := connect(t, up.URL, nil)
	if _, err := call(t, cs, "echo", nil); err != nil {
		t.Fatal(err)
	}
	up.Restart()
	_, err := call(t, cs, "echo", nil)
	if !errors.Is(err, mcp.ErrSessionMissing) {
		t.Fatalf("err = %v, want ErrSessionMissing", err)
	}
	// A fresh session works against the same URL.
	cs2 := connect(t, up.URL, nil)
	if _, err := call(t, cs2, "echo", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSetToolsAndChangeDescription(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		up := mcptest.NewServer(t, mcptest.Opts{Stateful: stateful, Tools: []mcptest.ToolSpec{mcptest.Echo("a"), mcptest.Echo("b")}})
		cs := connect(t, up.URL, nil)
		if got := toolNames(t, cs); len(got) != 2 {
			t.Fatalf("initial %d", len(got))
		}
		if !up.ChangeDescription("a", "new words") || up.ChangeDescription("zz", "x") {
			t.Fatal("ChangeDescription found/missing")
		}
		got := toolNames(t, cs)
		if got["a"].Description != "new words" || got["b"].Description == "new words" {
			t.Errorf("descriptions: %q / %q", got["a"].Description, got["b"].Description)
		}
		up.SetTools(mcptest.Echo("b"), mcptest.Fail("c").ReadOnly())
		got = toolNames(t, cs)
		if _, ok := got["a"]; ok || got["c"] == nil || !got["c"].Annotations.ReadOnlyHint || len(got) != 2 {
			t.Errorf("after SetTools: %v", got)
		}
		if _, err := call(t, cs, "a", nil); err == nil {
			t.Errorf("removed tool still callable")
		}
		if r, err := call(t, cs, "c", nil); err != nil || !r.IsError {
			t.Errorf("c: %v %v", r, err)
		}
	}
}

func TestStdioCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := mcptest.StdioCommand(t, "--name", "stdio-mock")
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if n := cs.InitializeResult().ServerInfo.Name; n != "stdio-mock" {
		t.Errorf("name %q", n)
	}
	if got := toolNames(t, cs); len(got) != len(mcptest.DefaultTools()) {
		t.Errorf("%d tools", len(got))
	}
	if r, err := call(t, cs, "echo", map[string]any{"text": "x"}); err != nil || firstText(r) != "echo: x" {
		t.Errorf("echo: %v %v", r, err)
	}
}

func TestStdioCrashAfter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := mcptest.StdioCommand(t, "--crash-after", "2")
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if _, err := call(t, cs, "echo", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := call(t, cs, "echo", nil); err == nil {
		t.Fatal("second call succeeded, want crash")
	}
	_ = cs.Wait() // the SDK reaps the child; reading ProcessState after it is safe
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
		t.Errorf("exit = %v", cmd.ProcessState)
	}
}

func TestSetDown(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	cs := connect(t, up.URL, nil)
	up.SetDown(true)
	resp, err := http.Post(up.URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", resp.StatusCode)
	}
	if _, err := call(t, cs, "echo", nil); err == nil {
		t.Error("call succeeded while down")
	}
	up.SetDown(false)
	if r, err := call(t, cs, "echo", map[string]any{"text": "back"}); err != nil || firstText(r) != "echo: back" {
		t.Errorf("after recovery: %v %v", r, err)
	}
}

func TestNoTools(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{NoTools: true})
	cs := connect(t, up.URL, nil)
	if got := toolNames(t, cs); len(got) != 0 {
		t.Errorf("tools: %v", got)
	}
	up.SetTools(mcptest.Echo("e"))
	if got := toolNames(t, cs); len(got) != 1 {
		t.Errorf("after SetTools: %v", got)
	}
}

func TestOversizedBodyRefused(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{})
	resp, err := http.Post(up.URL, "application/json", strings.NewReader(strings.Repeat("a", 4<<20+1)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", resp.StatusCode)
	}
}

func TestSessionHeaderOnTheWire(t *testing.T) {
	for _, stateful := range []bool{false, true} {
		up := mcptest.NewServer(t, mcptest.Opts{Stateful: stateful})
		body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
		req, _ := http.NewRequest(http.MethodPost, up.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if got := resp.Header.Get("Mcp-Session-Id") != ""; got != stateful {
			t.Errorf("stateful=%v: Mcp-Session-Id present = %v (status %d)", stateful, got, resp.StatusCode)
		}
	}
}

func TestRestartLeaksNoGoroutines(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{Stateful: true})
	settle := func() int {
		time.Sleep(200 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	base := settle()
	for range 20 {
		cs := connect(t, up.URL, nil)
		if _, err := call(t, cs, "echo", nil); err != nil {
			t.Fatal(err)
		}
		up.Restart()
		_ = cs.Close()
	}
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	if after := settle(); after > base+10 {
		t.Errorf("goroutines grew from %d to %d over 20 restarts", base, after)
	}
}

func TestConcurrentMutationsRace(t *testing.T) {
	up := mcptest.NewServer(t, mcptest.Opts{Stateful: true, Tools: []mcptest.ToolSpec{mcptest.Echo("a"), mcptest.Echo("b")}})
	done := make(chan struct{})
	for i := range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := range 20 {
				switch i % 3 {
				case 0:
					up.ChangeDescription("a", strings.Repeat("d", j))
				case 1:
					up.SetTools(mcptest.Echo("a"), mcptest.Echo("b"))
				default:
					up.Restart()
				}
			}
		}()
	}
	for range 4 {
		<-done
	}
	cs := connect(t, up.URL, nil)
	if got := toolNames(t, cs); len(got) != 2 {
		t.Errorf("tools: %v", got)
	}
}
