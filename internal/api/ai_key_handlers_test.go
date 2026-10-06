package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/store"
)

// testGatewayKey is the plaintext the fake store hands out: the real format,
// so a leak of it anywhere is easy to search for.
const testGatewayKey = "bgw_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// fakeKeyStore implements AIGatewayKeyStore over a slice with the store's
// ownership rule: an admin sees and revokes every key, anyone else their own.
type fakeKeyStore struct {
	rows []store.GatewayKey
	err  error

	creates, revokes       int
	lastUser, lastName     string
	lastAllowed            []string
	listCaller, listRole   string
	revokeCaller, revokeID string
	revokeRole             string
}

func (f *fakeKeyStore) CreateGatewayKey(_ context.Context, userID, name string, allowed []string) (store.GatewayKey, string, error) {
	f.creates++
	f.lastUser, f.lastName, f.lastAllowed = userID, name, allowed
	if f.err != nil {
		return store.GatewayKey{}, "", f.err
	}
	k := store.GatewayKey{
		ID: fmt.Sprintf("key-%d", f.creates), Name: name, KeyPrefix: testGatewayKey[:8], UserID: userID,
		AllowedModels: append([]string{}, allowed...), CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	f.rows = append(f.rows, k)
	return k, testGatewayKey, nil
}

func (f *fakeKeyStore) ListGatewayKeys(_ context.Context, callerID, callerRole string) ([]store.GatewayKey, error) {
	f.listCaller, f.listRole = callerID, callerRole
	out := []store.GatewayKey{}
	for _, k := range f.rows {
		if callerRole == "admin" || k.UserID == callerID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (f *fakeKeyStore) RevokeGatewayKey(_ context.Context, callerID, callerRole, id string) error {
	f.revokes++
	f.revokeCaller, f.revokeRole, f.revokeID = callerID, callerRole, id
	for i, k := range f.rows {
		if k.ID == id && (callerRole == "admin" || k.UserID == callerID) {
			now := time.Unix(1_700_000_100, 0).UTC()
			f.rows[i].RevokedAt = &now
			return nil
		}
	}
	return store.ErrKeyNotFound
}

type keyFixture struct {
	ks  *fakeKeyStore
	aud *stubAuditAppender
	d   Deps
}

func newKeyFixture(role string) *keyFixture {
	f := &keyFixture{ks: &fakeKeyStore{}, aud: &stubAuditAppender{}}
	f.d = Deps{
		Users:         &fakeUserStore{role: role},
		AIGatewayKeys: f.ks,
		AuditAppender: f.aud,
		AuthDomain:    "burrow.example.com",
		Log:           discardLog(),
	}
	return f
}

func (f *keyFixture) serve(t *testing.T) *authClient {
	t.Helper()
	srv, c := newAIProviderServer(t, f.d)
	t.Cleanup(srv.Close)
	return c
}

// noPlaintext fails when the key appears anywhere it must not: an audit
// event or the given response body.
func (f *keyFixture) noPlaintext(t *testing.T, body string) {
	t.Helper()
	secret := testGatewayKey[8:]
	if strings.Contains(body, secret) {
		t.Errorf("plaintext key in a response: %s", body)
	}
	for _, ev := range f.aud.events {
		if strings.Contains(string(ev.Payload)+ev.SubjectID+ev.SubjectLabel, secret) {
			t.Errorf("plaintext key in audit event %+v payload %s", ev, ev.Payload)
		}
	}
}

func TestPostKey(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		f := newKeyFixture("user")
		c := f.serve(t)
		resp := c.post(t, "/api/v1/ai/keys", map[string]any{"name": "laptop", "allowed_models": []string{"burrow-simple", "zai/*"}})
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
		body := wantStatus(t, resp, http.StatusCreated)
		var got map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if key, _ := got["key"].(string); key != testGatewayKey {
			t.Errorf("key = %v", got["key"])
		}
		for _, k := range []string{"id", "name", "key_prefix", "user_id", "allowed_models", "last_used", "created_at", "revoked_at"} {
			if _, ok := got[k]; !ok {
				t.Errorf("field %q missing in %s", k, body)
			}
		}
		if got["key_prefix"] != "bgw_AAAA" || got["user_id"] != "u-self" || got["last_used"] != nil || got["revoked_at"] != nil {
			t.Errorf("response = %s", body)
		}
		if f.ks.lastUser != "u-self" || f.ks.lastName != "laptop" || fmt.Sprint(f.ks.lastAllowed) != "[burrow-simple zai/*]" {
			t.Errorf("store received user %q name %q allowed %v", f.ks.lastUser, f.ks.lastName, f.ks.lastAllowed)
		}

		if len(f.aud.events) != 1 || f.aud.events[0].Action != "ai_gateway_key.create" || f.aud.events[0].SubjectID != "key-1" {
			t.Fatalf("audit = %+v", f.aud.events)
		}
		payload := auditPayload(t, f.aud.events[0])
		if payload["name"] != "laptop" || payload["key_prefix"] != "bgw_AAAA" || fmt.Sprint(payload["allowed_models"]) != "[burrow-simple zai/*]" {
			t.Errorf("audit payload = %v", payload)
		}

		// Never again: not in the list, not in the audit log.
		list := wantStatus(t, c.get(t, "/api/v1/ai/keys"), http.StatusOK)
		f.noPlaintext(t, list)
		if strings.Contains(list, `"key"`) || strings.Contains(strings.ToLower(list), "hash") {
			t.Errorf("list carries a key or a hash field: %s", list)
		}
	})

	t.Run("no allow-list is an empty array", func(t *testing.T) {
		f := newKeyFixture("user")
		body := wantStatus(t, f.serve(t).post(t, "/api/v1/ai/keys", map[string]any{"name": "all"}), http.StatusCreated)
		if !strings.Contains(body, `"allowed_models":[]`) {
			t.Errorf("body = %s", body)
		}
	})

	t.Run("store refuses", func(t *testing.T) {
		f := newKeyFixture("user")
		f.ks.err = fmt.Errorf("%w: at most 64 allowed models", store.ErrInvalidKey)
		resp := f.serve(t).post(t, "/api/v1/ai/keys", map[string]any{"name": "laptop"})
		body := wantStatus(t, resp, http.StatusBadRequest)
		var e map[string]string
		_ = json.Unmarshal([]byte(body), &e)
		if e["error"] != "at most 64 allowed models" {
			t.Errorf("error = %q", e["error"])
		}
		if len(f.aud.events) != 0 {
			t.Errorf("a refused create was audited")
		}
	})

	t.Run("refused before the store", func(t *testing.T) {
		many := make([]string, 65)
		for i := range many {
			many[i] = fmt.Sprintf("model-%d", i)
		}
		for name, body := range map[string]any{
			"no name":          map[string]any{"allowed_models": []string{"a/*"}},
			"blank name":       map[string]any{"name": "   "},
			"long name":        map[string]any{"name": strings.Repeat("n", 121)},
			"control in name":  map[string]any{"name": "a\nb"},
			"wildcard only":    map[string]any{"name": "k", "allowed_models": []string{"*"}},
			"partial wildcard": map[string]any{"name": "k", "allowed_models": []string{"zai/glm-*"}},
			"empty entry":      map[string]any{"name": "k", "allowed_models": []string{""}},
			"bad provider":     map[string]any{"name": "k", "allowed_models": []string{"Z AI/x"}},
			"long entry":       map[string]any{"name": "k", "allowed_models": []string{"zai/" + strings.Repeat("m", 300)}},
			"too many entries": map[string]any{"name": "k", "allowed_models": many},
			"unknown field":    map[string]any{"name": "k", "user_id": "someone-else"},
			"not json":         "nope",
			"body over 8 KiB":  map[string]any{"name": "k", "allowed_models": []string{"zai/" + strings.Repeat("m", 9<<10)}},
		} {
			t.Run(name, func(t *testing.T) {
				f := newKeyFixture("user")
				wantStatus(t, f.serve(t).post(t, "/api/v1/ai/keys", body), http.StatusBadRequest)
				if f.ks.creates != 0 {
					t.Error("the store was called")
				}
			})
		}
	})

	t.Run("no store", func(t *testing.T) {
		f := newKeyFixture("user")
		f.d.AIGatewayKeys = nil
		wantStatus(t, f.serve(t).post(t, "/api/v1/ai/keys", map[string]any{"name": "k"}), http.StatusServiceUnavailable)
	})
}

func TestListKeys(t *testing.T) {
	last := time.Unix(1_700_000_050, 0).UTC()
	rows := []store.GatewayKey{
		{ID: "k-mine", Name: "mine", KeyPrefix: "bgw_AAAA", UserID: "u-self", AllowedModels: []string{"zai/*"}, LastUsed: &last},
		{ID: "k-other", Name: "theirs", KeyPrefix: "bgw_BBBB", UserID: "u-other", AllowedModels: []string{}},
	}

	t.Run("user sees own keys", func(t *testing.T) {
		f := newKeyFixture("user")
		f.ks.rows = rows
		body := wantStatus(t, f.serve(t).get(t, "/api/v1/ai/keys"), http.StatusOK)
		if f.ks.listCaller != "u-self" || f.ks.listRole != "user" {
			t.Errorf("store called with %q / %q", f.ks.listCaller, f.ks.listRole)
		}
		var got []map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil || len(got) != 1 || got[0]["id"] != "k-mine" {
			t.Fatalf("list = %s (%v)", body, err)
		}
		if _, has := got[0]["key"]; has {
			t.Errorf("list entry has a key field: %s", body)
		}
		if strings.Contains(strings.ToLower(body), "hash") || strings.Contains(body, "k-other") {
			t.Errorf("list = %s", body)
		}
	})

	t.Run("admin sees all", func(t *testing.T) {
		f := newKeyFixture("admin")
		f.ks.rows = rows
		body := wantStatus(t, f.serve(t).get(t, "/api/v1/ai/keys"), http.StatusOK)
		if f.ks.listRole != "admin" || !strings.Contains(body, "k-other") {
			t.Errorf("role %q list %s", f.ks.listRole, body)
		}
	})

	t.Run("empty list is []", func(t *testing.T) {
		f := newKeyFixture("user")
		if body := strings.TrimSpace(wantStatus(t, f.serve(t).get(t, "/api/v1/ai/keys"), http.StatusOK)); body != "[]" {
			t.Errorf("body = %s", body)
		}
		f.d.AIGatewayKeys = nil
		if body := strings.TrimSpace(wantStatus(t, f.serve(t).get(t, "/api/v1/ai/keys"), http.StatusOK)); body != "[]" {
			t.Errorf("nil store: body = %s", body)
		}
	})
}

func TestDeleteKey(t *testing.T) {
	rows := func() []store.GatewayKey {
		return []store.GatewayKey{
			{ID: "k-mine", Name: "mine", KeyPrefix: "bgw_AAAA", UserID: "u-self"},
			{ID: "k-other", Name: "theirs", KeyPrefix: "bgw_BBBB", UserID: "u-other"},
		}
	}

	t.Run("own key", func(t *testing.T) {
		f := newKeyFixture("user")
		f.ks.rows = rows()
		c := f.serve(t)
		wantStatus(t, c.delete(t, "/api/v1/ai/keys/k-mine"), http.StatusNoContent)
		if f.ks.revokeCaller != "u-self" || f.ks.revokeRole != "user" || f.ks.revokeID != "k-mine" {
			t.Errorf("store called with %q / %q / %q", f.ks.revokeCaller, f.ks.revokeRole, f.ks.revokeID)
		}
		if len(f.aud.events) != 1 || f.aud.events[0].Action != "ai_gateway_key.revoke" || f.aud.events[0].SubjectID != "k-mine" {
			t.Fatalf("audit = %+v", f.aud.events)
		}
	})

	// Another user's key and a key that does not exist give the same answer,
	// so ids cannot be probed.
	t.Run("another user's key is a missing key", func(t *testing.T) {
		f := newKeyFixture("user")
		f.ks.rows = rows()
		c := f.serve(t)
		other := wantStatus(t, c.delete(t, "/api/v1/ai/keys/k-other"), http.StatusNotFound)
		missing := wantStatus(t, c.delete(t, "/api/v1/ai/keys/k-nope"), http.StatusNotFound)
		if other != missing || !strings.Contains(other, "key not found") {
			t.Errorf("answers differ: %q vs %q", other, missing)
		}
		if f.ks.rows[1].RevokedAt != nil || len(f.aud.events) != 0 {
			t.Errorf("the key was revoked or the refusal audited")
		}
	})

	t.Run("admin revokes any", func(t *testing.T) {
		f := newKeyFixture("admin")
		f.ks.rows = rows()
		wantStatus(t, f.serve(t).delete(t, "/api/v1/ai/keys/k-other"), http.StatusNoContent)
		if f.ks.rows[1].RevokedAt == nil {
			t.Error("not revoked")
		}
	})

	t.Run("overlong id", func(t *testing.T) {
		f := newKeyFixture("admin")
		wantStatus(t, f.serve(t).delete(t, "/api/v1/ai/keys/"+strings.Repeat("a", 200)), http.StatusNotFound)
		if f.ks.revokes != 0 {
			t.Error("the store was called")
		}
	})
}

// An automation bearer token is accepted on the key routes as it is on
// /tokens and the service API keys; the key belongs to the token's user.
func TestKeyRoutes_BearerToken(t *testing.T) {
	f := newKeyFixture("user")
	auto := newFakeAutomationStore()
	f.d.Automation, f.d.Bearer = auto, auto
	_, bearer, err := auto.MintAutomationToken(context.Background(), "u-self", "user", "ci", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewRouter(f.d))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ai/keys", strings.NewReader(`{"name":"ci"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, resp, http.StatusCreated)
	if f.ks.lastUser != "u-self" {
		t.Errorf("key owner = %q", f.ks.lastUser)
	}
}

func TestGetGatewayInfo(t *testing.T) {
	f := newKeyFixture("user")
	body := wantStatus(t, f.serve(t).get(t, "/api/v1/ai/gateway"), http.StatusOK)
	want := `{"endpoints":[{"dialect":"openai","base_url":"https://burrow.example.com/openai/v1"},{"dialect":"anthropic","base_url":"https://burrow.example.com/anthropic"}]}`
	if strings.TrimSpace(body) != want {
		t.Errorf("body = %s\nwant   %s", body, want)
	}

	f.d.AuthDomain = ""
	body = wantStatus(t, f.serve(t).get(t, "/api/v1/ai/gateway"), http.StatusOK)
	want = `{"endpoints":[{"dialect":"openai","base_url":""},{"dialect":"anthropic","base_url":""}]}`
	if strings.TrimSpace(body) != want {
		t.Errorf("no auth domain: body = %s", body)
	}
}
