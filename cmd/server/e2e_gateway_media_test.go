// cmd/server/e2e_gateway_media_test.go
//
// Images, speech, transcription, audio translation and rerank through the
// relay's gateway, to an in-process stand-in that serves the answers of
// test/harness/mockoai. Same wiring as e2e_gateway_tunnel_test.go.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/db"
)

// mediaMock answers like mockoai does and records the paths and the model it
// was asked for.
type mediaMock struct{ paths, models []string }

func (m *mediaMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.paths = append(m.paths, r.URL.Path)
	switch r.URL.Path {
	case "/v1/audio/transcriptions", "/v1/audio/translations":
		if err := r.ParseMultipartForm(8 << 20); err != nil || r.FormValue("model") == "" {
			http.Error(w, "form needs model and file", http.StatusBadRequest)
			return
		}
		if _, _, err := r.FormFile("file"); err != nil {
			http.Error(w, "form needs model and file", http.StatusBadRequest)
			return
		}
		m.models = append(m.models, r.FormValue("model"))
		gwJSON(w, 200, map[string]string{"text": "hello from mockoai"})
		return
	}
	var body struct{ Model string }
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	m.models = append(m.models, body.Model)
	switch r.URL.Path {
	case "/v1/images/generations":
		gwJSON(w, 200, map[string]any{"created": 1700000000, "data": []map[string]string{{"b64_json": "aGk="}}})
	case "/v1/audio/speech":
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("ID3\x04\x00mock"))
	case "/v1/rerank":
		gwJSON(w, 200, map[string]any{"results": []map[string]any{{"index": 0, "relevance_score": 0.9}}, "usage": map[string]int{"total_tokens": 12}})
	default:
		http.NotFound(w, r)
	}
}

func TestE2E_GatewayMediaEndpoints(t *testing.T) {
	tunnels := &gwTunnels{}
	e := bootGatewayE2EWith(t, tunnels)
	ctx := context.Background()
	wrapped := db.Wrap(e.sqldb)
	admin, err := wrapped.GetUserByEmail(ctx, e.adminEmail)
	if err != nil {
		t.Fatalf("get admin: %v", err)
	}
	mock := &mediaMock{}
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	if err := wrapped.CreateService(ctx, db.Service{ID: "svc-media", UserID: admin.ID, Name: "media", Type: "http", AccessMode: "api_key"}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	tunnels.connect("svc-media", srv.Listener.Addr().String())
	if code, body := e.admin(t, "POST", "/api/v1/ai/providers", map[string]any{"slug": "media", "name": "media", "kind": "tunnel", "service_id": "svc-media"}); code != http.StatusCreated {
		t.Fatalf("create provider: %d %s", code, body)
	}
	if code, body := e.admin(t, "POST", "/api/v1/ai/models", map[string]any{"name": "media-model", "targets": []map[string]string{
		{"dialect": "openai", "provider": "media", "model": "target-x"},
	}}); code != http.StatusCreated {
		t.Fatalf("create model: %d %s", code, body)
	}
	code, body := e.admin(t, "POST", "/api/v1/ai/keys", map[string]any{"name": "all"})
	var key struct{ Key string }
	if err := json.Unmarshal(body, &key); code != http.StatusCreated || err != nil || key.Key == "" {
		t.Fatalf("create key: %d %s", code, body)
	}

	form := func() (string, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		_, _ = fw.Write([]byte("RIFF\r\n--not-the-boundary\x00\x01\x02"))
		_ = mw.WriteField("model", "media-model")
		_ = mw.Close()
		return buf.String(), mw.FormDataContentType()
	}
	formBody, formType := form()
	cases := []struct {
		path, body, contentType, wantBody string
		tokensIn                          int
	}{
		{"/v1/images/generations", `{"model":"media-model","prompt":"a cat"}`, "application/json", `"b64_json":"aGk="`, 0},
		{"/v1/audio/speech", `{"model":"media-model","input":"hi","voice":"alloy"}`, "application/json", "ID3", 0},
		{"/v1/audio/transcriptions", formBody, formType, `"text":"hello from mockoai"`, 0},
		{"/v1/audio/translations", formBody, formType, `"text":"hello from mockoai"`, 0},
		{"/v1/rerank", `{"model":"media-model","query":"q","documents":["a"]}`, "application/json", `"relevance_score":0.9`, 12},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			before := e.usageCount(t)
			r := e.call(t, "POST", "/openai/v1"+strings.TrimPrefix(c.path, "/v1"), c.body, "Authorization", "Bearer "+key.Key, "Content-Type", c.contentType)
			if r.Status != 200 || !strings.Contains(string(r.Body), c.wantBody) {
				t.Fatalf("status %d body %s", r.Status, r.Body)
			}
			if got := mock.paths[len(mock.paths)-1]; got != c.path {
				t.Fatalf("upstream path = %s", got)
			}
			if got := mock.models[len(mock.models)-1]; got != "target-x" {
				t.Fatalf("upstream model = %q", got)
			}
			u := e.usageFor(t, r.Header.Get("Burrow-Request-Id"))
			if u.Provider != "media" || u.Requested != "media-model" || u.Target != "target-x" || u.Status != 200 || u.TokensIn != c.tokensIn || u.TokensOut != 0 {
				t.Fatalf("usage row %+v", u)
			}
			if after := e.usageCount(t); after != before+1 {
				t.Fatalf("usage rows %d -> %d, want one more", before, after)
			}
		})
	}

	t.Run("a form without model is refused", func(t *testing.T) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", "a.wav")
		_, _ = fw.Write([]byte("x"))
		_ = mw.Close()
		n := len(mock.paths)
		r := e.call(t, "POST", "/openai/v1/audio/transcriptions", buf.String(), "Authorization", "Bearer "+key.Key, "Content-Type", mw.FormDataContentType())
		if r.Status != 400 || r.code() != "model_required" || len(mock.paths) != n {
			t.Fatalf("status %d code %q", r.Status, r.code())
		}
	})
}
