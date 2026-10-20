package audit

import (
	"context"
	"encoding/json"
	"testing"
)

// Round 2: a row written while a management tool call is replayed through
// the REST API names the credential that made the call, as the
// mcp.tool.call row does: token_id for an automation token, oauth_grant_id
// for an OAuth grant. The caller's own payload keys win, and a row written
// outside such a call is left as it is.
func TestAppend_ReplayedCallCarriesTheCredential(t *testing.T) {
	x := newTestDB(t)
	l := newTestLogger(t, x)
	for _, c := range []struct {
		name    string
		ctx     context.Context
		payload string
		want    map[string]any
	}{
		{"automation token", WithCredential(context.Background(), Credential{TokenID: "tok1"}), `{"role":"user"}`, map[string]any{"role": "user", "token_id": "tok1"}},
		{"oauth grant", WithCredential(context.Background(), Credential{OAuthGrantID: "g1"}), ``, map[string]any{"oauth_grant_id": "g1"}},
		{"own key wins", WithCredential(context.Background(), Credential{TokenID: "tok1"}), `{"token_id":"other"}`, map[string]any{"token_id": "other"}},
		{"no call", context.Background(), `{"role":"user"}`, map[string]any{"role": "user"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := Event{Action: ActionUserCreate, SubjectID: c.name, Result: "ok"}
			if c.payload != "" {
				e.Payload = json.RawMessage(c.payload)
			}
			if err := l.Append(c.ctx, e); err != nil {
				t.Fatal(err)
			}
			var raw string
			if err := x.DB().QueryRow(`SELECT payload FROM audit_events WHERE subject_id = ?`, c.name).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("payload %s, want %v", raw, c.want)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Fatalf("payload %s, want %v", raw, c.want)
				}
			}
		})
	}
	if ok, bad, err := l.Verify(context.Background(), "", ""); err != nil || !ok {
		t.Fatalf("chain after the rows: ok=%v first bad %q err %v", ok, bad, err)
	}
}
