package aigw

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestApplySystemText(t *testing.T) {
	cases := []struct {
		name, path, body, text string
		want                   string // the relevant part after decoding, as JSON
		applied                bool
	}{
		{"chat without system", "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "Answer in German.",
			`[{"role":"system","content":"Answer in German."},{"role":"user","content":"hi"}]`, true},
		{"chat keeps the app's system second", "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]}`, "Answer in German.",
			`[{"role":"system","content":"Answer in German."},{"role":"system","content":"Be brief."},{"role":"user","content":"hi"}]`, true},
		{"chat with developer message", "/v1/chat/completions",
			`{"model":"m","messages":[{"role":"developer","content":"x"}]}`, "T",
			`[{"role":"system","content":"T"},{"role":"developer","content":"x"}]`, true},
		{"responses without instructions", "/v1/responses", `{"model":"m","input":"hi"}`, "T", `"T"`, true},
		{"responses with instructions", "/v1/responses", `{"model":"m","input":"hi","instructions":"Be brief."}`, "T", `"T\n\nBe brief."`, true},
		{"messages without system", "/v1/messages", `{"model":"m","max_tokens":5,"messages":[]}`, "T", `"T"`, true},
		{"messages with string system", "/v1/messages", `{"model":"m","system":"Be brief.","messages":[]}`, "T", `"T\n\nBe brief."`, true},
		{"messages with block system", "/v1/messages",
			`{"model":"m","system":[{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}],"messages":[]}`, "T",
			`[{"type":"text","text":"T"},{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}]`, true},
		{"responses with empty instructions", "/v1/responses", `{"model":"m","input":"hi","instructions":""}`, "T", `"T"`, true},
		{"messages with empty system", "/v1/messages", `{"model":"m","system":"","messages":[]}`, "T", `"T"`, true},
		{"embeddings untouched", "/v1/embeddings", `{"model":"m","input":"hi"}`, "T", "", false},
		{"count_tokens untouched", "/v1/messages/count_tokens", `{"model":"m","messages":[]}`, "T", "", false},
		{"empty text", "/v1/chat/completions", `{"model":"m","messages":[]}`, "", "", false},
		{"not an object", "/v1/chat/completions", `[1,2]`, "T", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, applied := ApplySystemText(c.path, []byte(c.body), c.text)
			if applied != c.applied {
				t.Fatalf("applied = %v, want %v", applied, c.applied)
			}
			if !applied {
				if string(out) != c.body {
					t.Fatalf("unapplied body changed: %s", out)
				}
				return
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("output is not an object: %v", err)
			}
			key := map[string]string{"/v1/chat/completions": "messages", "/v1/responses": "instructions", "/v1/messages": "system"}[c.path]
			var got, want any
			if err := json.Unmarshal(obj[key], &got); err != nil {
				t.Fatalf("decode %s: %v", key, err)
			}
			if err := json.Unmarshal([]byte(c.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %s, want %s", key, obj[key], c.want)
			}
			if string(obj["model"]) != `"m"` {
				t.Fatalf("model changed: %s", obj["model"])
			}
		})
	}

	t.Run("unrelated values keep their bytes", func(t *testing.T) {
		body := `{"model":"m","temperature":0.70,"metadata":{"a":"<b>&"},"messages":[{"role":"user","content":"hi"}]}`
		out, applied := ApplySystemText("/v1/chat/completions", []byte(body), "T <&>")
		if !applied {
			t.Fatal("not applied")
		}
		for _, frag := range []string{`"temperature":0.70`, `"metadata":{"a":"<b>&"}`, `"content":"T <&>"`} {
			if !bytes.Contains(out, []byte(frag)) {
				t.Errorf("output lacks %s: %s", frag, out)
			}
		}
	})
	t.Run("trailing slash", func(t *testing.T) {
		if _, applied := ApplySystemText("/v1/responses/", []byte(`{"input":"x"}`), "T"); !applied {
			t.Fatal("not applied")
		}
	})
	t.Run("odd shapes are left alone", func(t *testing.T) {
		for _, c := range []struct{ path, body string }{
			{"/v1/chat/completions", `{"model":"m"}`},
			{"/v1/chat/completions", `{"messages":"x"}`},
			{"/v1/responses", `{"instructions":5}`},
			{"/v1/messages", `{"system":7}`},
		} {
			if out, applied := ApplySystemText(c.path, []byte(c.body), "T"); applied || string(out) != c.body {
				t.Errorf("%s %s: applied=%v out=%s", c.path, c.body, applied, out)
			}
		}
	})
}

func TestJoinSystemText(t *testing.T) {
	for _, c := range []struct{ m, k, want string }{{"", "", ""}, {"M", "", "M"}, {"", "K", "K"}, {"M", "K", "M\n\nK"}} {
		if got := JoinSystemText(c.m, c.k); got != c.want {
			t.Errorf("Join(%q,%q) = %q, want %q", c.m, c.k, got, c.want)
		}
	}
}

func TestSystemTextContext(t *testing.T) {
	if SystemTextFrom(context.Background()) != "" {
		t.Fatal("empty context has text")
	}
	if got := SystemTextFrom(WithSystemText(context.Background(), "x")); got != "x" {
		t.Fatalf("got %q", got)
	}
}
