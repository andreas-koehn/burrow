package credinject_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/ankoehn/burrow/internal/credinject"
)

// stubVault is an in-test Vault backed by a plain map.
type stubVault struct{ m map[string]string }

func (s stubVault) Get(slot string) (string, bool) { v, ok := s.m[slot]; return v, ok }
func (s stubVault) Slots() []string                { return nil }

// stubStore is an in-test Store that returns a fixed Binding (or none).
type stubStore struct {
	bind   credinject.Binding
	bound  bool // true → bind is valid; false → unbound
	putErr error
	delErr error
	// for inspection
	lastPut credinject.Binding
	lastDel string
}

func (s *stubStore) GetBinding(_ context.Context, _ string) (credinject.Binding, bool, error) {
	return s.bind, s.bound, nil
}
func (s *stubStore) PutBinding(_ context.Context, b credinject.Binding) error {
	s.lastPut = b
	return s.putErr
}
func (s *stubStore) DeleteBinding(_ context.Context, serviceID string) error {
	s.lastDel = serviceID
	return s.delErr
}

func TestInjectorStripsVisitorAuthAndAppliesHeaderFormat(t *testing.T) {
	v := stubVault{map[string]string{"OPENAI": "sk-real"}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc1",
			Slot:         "OPENAI",
			HeaderName:   "Authorization",
			HeaderFormat: "Bearer {key}",
		},
		bound: true,
	}
	i := credinject.New(v, s, slog.Default())
	r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer visitor-burrow-key")
	ok, err := i.Apply(context.Background(), "svc1", r)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("got %q; want Bearer sk-real", got)
	}
}

func TestInjectorUnboundPassThrough(t *testing.T) {
	v := stubVault{map[string]string{"OPENAI": "sk-real"}}
	s := &stubStore{bound: false}
	i := credinject.New(v, s, slog.Default())
	var injectFired bool
	i.OnInject = func(_, _ string) { injectFired = true }
	r, _ := http.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer visitor-key")
	ok, err := i.Apply(context.Background(), "svc-none", r)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("unbound service should return false")
	}
	// Header must be untouched.
	if got := r.Header.Get("Authorization"); got != "Bearer visitor-key" {
		t.Errorf("pass-through should leave header unchanged; got %q", got)
	}
	if injectFired {
		t.Error("OnInject must NOT fire on unbound path")
	}
}

func TestInjectorMissingSlotEnvFiresMissCounter(t *testing.T) {
	// Vault has no key for OPENAI.
	v := stubVault{map[string]string{}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc2",
			Slot:         "OPENAI",
			HeaderName:   "Authorization",
			HeaderFormat: "Bearer {key}",
		},
		bound: true,
	}
	var missFired bool
	var injectFired bool
	i := credinject.New(v, s, slog.Default())
	i.OnMiss = func(serviceID string) { missFired = true }
	i.OnInject = func(_, _ string) { injectFired = true }

	r, _ := http.NewRequest("POST", "/v1/chat", nil)
	ok, err := i.Apply(context.Background(), "svc2", r)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("missing slot should return false (pass-through)")
	}
	if !missFired {
		t.Error("OnMiss hook should have been called")
	}
	if injectFired {
		t.Error("OnInject must NOT fire on missing-slot path")
	}
}

func TestInjectorSuccessFiresInjectCounter(t *testing.T) {
	v := stubVault{map[string]string{"OPENAI": "sk-real"}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc5",
			Slot:         "OPENAI",
			HeaderName:   "Authorization",
			HeaderFormat: "Bearer {key}",
		},
		bound: true,
	}
	type call struct{ serviceID, slot string }
	var calls []call
	i := credinject.New(v, s, slog.Default())
	i.OnInject = func(serviceID, slot string) {
		calls = append(calls, call{serviceID, slot})
	}

	r, _ := http.NewRequest("POST", "/v1/chat/completions", nil)
	ok, err := i.Apply(context.Background(), "svc5", r)
	if err != nil || !ok {
		t.Fatalf("Apply returned (%v, %v); want (true, nil)", ok, err)
	}
	if len(calls) != 1 {
		t.Fatalf("OnInject called %d times; want 1", len(calls))
	}
	if calls[0].serviceID != "svc5" || calls[0].slot != "OPENAI" {
		t.Errorf("OnInject args = (%q, %q); want (svc5, OPENAI)", calls[0].serviceID, calls[0].slot)
	}
}

func TestInjectorHeaderFormatCustomTemplate(t *testing.T) {
	v := stubVault{map[string]string{"ANTHROPIC": "sk-ant-1"}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc3",
			Slot:         "ANTHROPIC",
			HeaderName:   "x-api-key",
			HeaderFormat: "{key}",
		},
		bound: true,
	}
	i := credinject.New(v, s, slog.Default())
	r, _ := http.NewRequest("POST", "/v1/messages", nil)
	ok, err := i.Apply(context.Background(), "svc3", r)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got := r.Header.Get("x-api-key"); got != "sk-ant-1" {
		t.Errorf("got %q; want sk-ant-1", got)
	}
}

func TestInjectorOnlyFirstKeyPlaceholderReplaced(t *testing.T) {
	// Format has {key} twice; only the first should be replaced.
	v := stubVault{map[string]string{"SLOT": "myval"}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc4",
			Slot:         "SLOT",
			HeaderName:   "X-Custom",
			HeaderFormat: "{key}+{key}",
		},
		bound: true,
	}
	i := credinject.New(v, s, slog.Default())
	r, _ := http.NewRequest("GET", "/", nil)
	ok, err := i.Apply(context.Background(), "svc4", r)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if got := r.Header.Get("X-Custom"); got != "myval+{key}" {
		t.Errorf("got %q; want myval+{key} (only first occurrence replaced)", got)
	}
}

// A binding whose slot is missing from the vault sends no credential at all:
// the visitor's own value for the bound header must not stand in for the one
// the operator configured. Other headers are left alone.
func TestInjectorMissingSlotStripsVisitorCredential(t *testing.T) {
	v := stubVault{map[string]string{}}
	s := &stubStore{
		bind: credinject.Binding{
			ServiceID:    "svc2",
			Slot:         "OPENAI",
			HeaderName:   "x-api-key",
			HeaderFormat: "{key}",
		},
		bound: true,
	}
	missed := 0
	i := credinject.New(v, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	i.OnMiss = func(string) { missed++ }

	r, _ := http.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Api-Key", "visitor-key")
	r.Header.Add("X-Api-Key", "visitor-key-2")
	r.Header.Set("Content-Type", "application/json")
	ok, err := i.Apply(context.Background(), "svc2", r)
	if err != nil || ok {
		t.Fatalf("Apply = %v, %v; want false, nil", ok, err)
	}
	if got := r.Header.Values("X-Api-Key"); len(got) != 0 {
		t.Fatalf("the visitor's credential reached the upstream request: %q", got)
	}
	if r.Header.Get("Content-Type") != "application/json" || missed != 1 {
		t.Fatalf("headers %v, OnMiss fired %d times", r.Header, missed)
	}
}
