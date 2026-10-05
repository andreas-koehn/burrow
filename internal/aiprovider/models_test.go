package aiprovider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFetchModels(t *testing.T) {
	var gotAuth, gotPath, gotExtra string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotExtra = r.Header.Get("Authorization"), r.URL.Path, r.Header.Get("X-Title")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"google/gemini-x","name":"Gemini X","context_length":1000000,"pricing":{"prompt":"0.000001"}},
			{"id":"glm-5.1"},
			{"id":"glm-5.1","name":"duplicate"},
			{"id":""},
			{"id":"bad\u0000id"},
			{"id":"line\nbreak"},
			{"id":" padded "},
			{"id":"neg","context_length":-5},
			{"name":"no id"}
		]}`))
	}))
	defer srv.Close()

	cfg := Config{Slug: "p", BaseURL: srv.URL + "/api/v1", CredentialSlot: "S", ExtraHeaders: map[string]string{"X-Title": "Burrow"}}
	models, err := FetchModels(context.Background(), cfg, mapVault{"S": "sk-up"}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer sk-up" || gotPath != "/api/v1/models" || gotExtra != "Burrow" {
		t.Fatalf("auth %q path %q extra %q", gotAuth, gotPath, gotExtra)
	}
	want := []Model{{ID: "glm-5.1"}, {ID: "google/gemini-x", DisplayName: "Gemini X", ContextLength: 1000000}, {ID: "neg"}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %+v, want %+v (sorted by id; malformed and repeated entries dropped)", models, want)
	}
}

// An extra header can neither replace nor add to the credential.
func TestFetchModels_CredentialWinsOverExtraHeader(t *testing.T) {
	var got []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	cfg := Config{Slug: "p", BaseURL: srv.URL, CredentialSlot: "S", ExtraHeaders: map[string]string{"authorization": "Bearer other"}}
	if _, err := FetchModels(context.Background(), cfg, mapVault{"S": "sk-up"}, srv.Client().Transport); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "Bearer sk-up" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestFetchModels_NamesAreBounded(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":%q},{"id":"ok","name":%q},{"id":"ctl","name":"a\u0007b"}]}`,
			strings.Repeat("x", MaxModelIDLen+1), strings.Repeat("n", 5000))
	}))
	defer srv.Close()
	models, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: srv.URL, CredentialSlot: "S"}, mapVault{"S": "sk-up"}, srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "ctl" || models[0].DisplayName != "" || models[1].ID != "ok" || len(models[1].DisplayName) != maxModelNameLen {
		t.Fatalf("models = %d entries, first %+v", len(models), models[0])
	}
}

func TestFetchModels_Failures(t *testing.T) {
	var elsewhere bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere = true }))
	defer other.Close()

	tooMany := func(w http.ResponseWriter, _ *http.Request) {
		var b bytes.Buffer
		b.WriteString(`{"data":[`)
		for i := 0; i <= MaxModels; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"id":"m%d"}`, i)
		}
		b.WriteString(`]}`)
		_, _ = w.Write(b.Bytes())
	}
	cases := map[string]http.HandlerFunc{
		"upstream 401": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`key sk-up rejected`))
		},
		"not json":    func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`<html>sk-up`)) },
		"no data key": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) },
		"huge body": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxModelsBody+10))
		},
		"too many models": tooMany,
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/models", http.StatusFound)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewTLSServer(h)
			defer srv.Close()
			_, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: srv.URL, CredentialSlot: "S"}, mapVault{"S": "sk-up"}, srv.Client().Transport)
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "sk-up") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Fatalf("error leaks the credential, the upstream body or its address: %v", err)
			}
		})
	}
	if elsewhere {
		t.Fatal("a redirect was followed: the credential left for another host")
	}

	// A transport failure is reported without the transport's own text.
	dead := httptest.NewTLSServer(http.NotFoundHandler())
	deadURL, rt := dead.URL, dead.Client().Transport
	dead.Close()
	if _, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: deadURL, CredentialSlot: "S"}, mapVault{"S": "sk-up"}, rt); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("dead upstream err = %v", err)
	}

	if _, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: "https://x/v1", CredentialSlot: "NONE"}, mapVault{}, http.DefaultTransport); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("missing slot err = %v", err)
	}
	if _, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": ""}, http.DefaultTransport); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty slot err = %v", err)
	}
	if _, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: "http://x/v1", CredentialSlot: "S"}, mapVault{"S": "sk-up"}, http.DefaultTransport); !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("http base URL err = %v", err)
	}
	// Never the default transport: it has no address guard.
	if _, err := FetchModels(context.Background(), Config{Slug: "p", BaseURL: "https://x/v1", CredentialSlot: "S"}, mapVault{"S": "sk-up"}, nil); err == nil {
		t.Fatal("a nil transport must be refused")
	}
	bad := Config{Slug: "p", BaseURL: "https://x/v1", CredentialSlot: "S", ExtraHeaders: map[string]string{"X-A": "v\r\nX-B: w"}}
	if _, err := FetchModels(context.Background(), bad, mapVault{"S": "sk-up"}, http.DefaultTransport); err == nil {
		t.Fatal("a header value with CR/LF must be refused")
	}
}

// The guarded transport refuses a loopback upstream for the model sync too:
// not even a TCP connection is opened.
func TestFetchModels_GuardedTransportRefusesLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	cfg := Config{Slug: "p", BaseURL: "https://" + ln.Addr().String() + "/v1", CredentialSlot: "S"}
	rt := NewTransport(false)
	defer rt.CloseIdleConnections()
	if _, err := FetchModels(context.Background(), cfg, mapVault{"S": "sk-up"}, rt); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("err = %v", err)
	}
	// With the guard lifted the same call does connect (and fails on TLS).
	open := NewTransport(true)
	defer open.CloseIdleConnections()
	if n := accepted.Load(); n != 0 {
		t.Fatalf("the guarded transport opened %d connections to loopback", n)
	}
	_, _ = FetchModels(context.Background(), cfg, mapVault{"S": "sk-up"}, open)
	deadline := time.Now().Add(2 * time.Second)
	for accepted.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if accepted.Load() == 0 {
		t.Fatal("control: with private upstreams allowed the listener should have been reached")
	}
}

func TestValidModelID(t *testing.T) {
	for id, want := range map[string]bool{
		"glm-5.1": true, "google/gemini-x": true, "qwen2.5:0.5b": true, "a b": true,
		"": false, " a": false, "a ": false, "a\nb": false, "a\x00": false, "a\x7f": false, "\xff": false,
		strings.Repeat("x", MaxModelIDLen): true, strings.Repeat("x", MaxModelIDLen+1): false,
	} {
		if got := ValidModelID(id); got != want {
			t.Errorf("ValidModelID(%q) = %v, want %v", id, got, want)
		}
	}
}
