package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// CheckRequest reads a Messages request body the way Anthropic's server
// does, and refuses what that server answers with a 400. It is the yardstick
// of EncodeRequest: the self-checks that release a pair and the tests judge
// what the encoder writes with it.
//
// It holds, and returns an error otherwise:
//   - the body is a JSON object of the fields EncodeRequest writes ("model",
//     "max_tokens", "system", "messages", "tools", "tool_choice",
//     "temperature", "top_p", "stop_sequences", "stream") and no other;
//   - "model" is a string that is not empty and "max_tokens" a whole number
//     above 0; "system", when present, is a string with more than white
//     space;
//   - "messages" is not empty, begins with a user message, and its roles
//     alternate between user and assistant;
//   - the content of a message is a list of blocks that is not empty: text
//     with more than white space; image with a base64 source of a known
//     media type or an http(s) url source; tool_use (in an assistant
//     message) with an id of the characters [a-zA-Z0-9_-] that is its own
//     within the message, a name and an input object; tool_result (in a
//     user message) whose content is a string or a list of text and image
//     blocks. No other block type;
//   - every tool_use is answered: the user message after an assistant
//     message holds one tool_result for each of its ids, before any other
//     block, and a tool_result stands nowhere else and for no id twice;
//   - a tool has a name and an "input_schema" that is an object whose "type"
//     is "object";
//   - "tool_choice" stands only next to tools, is of type auto, any, none,
//     or tool with the name of one of them;
//   - "temperature" and "top_p" lie between 0 and 1 and do not stand
//     together; no stop sequence is white space only.
func CheckRequest(body []byte) error {
	var top map[string]json.RawMessage
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(body, &top) != nil {
		return errors.New("the body is not a JSON object")
	}
	for key := range top {
		switch key {
		case "model", "max_tokens", "system", "messages", "tools", "tool_choice", "temperature", "top_p", "stop_sequences", "stream":
		default:
			return fmt.Errorf("a field the Messages API does not know: %q", key)
		}
	}
	var req struct {
		Model     string   `json:"model"`
		MaxTokens *float64 `json:"max_tokens"`
		System    *string  `json:"system"`
		Messages  []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
		Temperature *float64 `json:"temperature"`
		TopP        *float64 `json:"top_p"`
		Stop        []string `json:"stop_sequences"`
		Stream      *bool    `json:"stream"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New("a field of the wrong type")
	}
	switch {
	case req.Model == "":
		return errors.New("no model")
	case req.MaxTokens == nil || *req.MaxTokens < 1 || *req.MaxTokens != math.Trunc(*req.MaxTokens):
		return errors.New("max_tokens is not a whole number above 0")
	case req.System != nil && strings.TrimSpace(*req.System) == "":
		return errors.New("an empty system prompt")
	case len(req.Messages) == 0:
		return errors.New("no messages")
	case req.Messages[0].Role != "user":
		return errors.New("the first message is not the user's")
	}
	var waiting map[string]bool // the tool_use ids of the assistant message before
	for i, m := range req.Messages {
		fail := func(what string) error { return fmt.Errorf("messages[%d] (%s): %s", i, m.Role, what) }
		if m.Role != "user" && m.Role != "assistant" {
			return fail("is of no known role")
		}
		if i > 0 && req.Messages[i-1].Role == m.Role {
			return fail("follows a message of the same role")
		}
		if len(m.Content) == 0 {
			return fail("has no content")
		}
		answered := waiting
		waiting = nil
		ids := map[string]bool{}
		other := false // a block that is no tool_result was read
		for _, raw := range m.Content {
			var b struct {
				Type      string          `json:"type"`
				Text      *string         `json:"text"`
				Source    json.RawMessage `json:"source"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				Content   json.RawMessage `json:"content"`
			}
			if json.Unmarshal(raw, &b) != nil {
				return fail("has a block that is no object")
			}
			if b.Type != "tool_result" {
				other = true
			}
			switch b.Type {
			case "text":
				if b.Text == nil || strings.TrimSpace(*b.Text) == "" {
					return fail("has a text block without text")
				}
			case "image":
				if m.Role != "user" || !soundImage(b.Source) {
					return fail("has an image the API does not take")
				}
			case "tool_use":
				if m.Role != "assistant" || !soundToolID(b.ID) || ids[b.ID] || b.Name == "" || ir.CheckObject(b.Input) != nil {
					return fail("has a tool_use without an id of its own, a name, or an input object")
				}
				ids[b.ID] = true
			case "tool_result":
				switch {
				case m.Role != "user":
					return fail("has a tool_result")
				case other:
					return fail("has a tool_result after other content")
				case !answered[b.ToolUseID]:
					return fail("has a tool_result for no tool_use of the message before")
				case !soundResult(b.Content):
					return fail("has a tool_result whose content the API does not take")
				}
				delete(answered, b.ToolUseID)
			default:
				return fail(fmt.Sprintf("has a block of type %q", b.Type))
			}
		}
		if len(answered) > 0 {
			return fail("does not answer every tool_use of the message before")
		}
		if len(ids) > 0 {
			waiting = ids
		}
	}
	if len(waiting) > 0 {
		return errors.New("the last message has tool_use blocks without an answer")
	}
	names := map[string]bool{}
	for i, t := range req.Tools {
		var schema struct {
			Type json.RawMessage `json:"type"`
		}
		if t.Name == "" || ir.CheckObject(t.Schema) != nil || json.Unmarshal(t.Schema, &schema) != nil || string(schema.Type) != `"object"` {
			return fmt.Errorf("tools[%d]: no name, or an input_schema that is no object schema", i)
		}
		names[t.Name] = true
	}
	if c := req.ToolChoice; c != nil {
		switch {
		case len(req.Tools) == 0:
			return errors.New("tool_choice without tools")
		case c.Type == "tool" && !names[c.Name]:
			return errors.New("tool_choice names no tool of the request")
		case c.Type != "auto" && c.Type != "any" && c.Type != "none" && c.Type != "tool":
			return errors.New("tool_choice of an unknown type")
		}
	}
	for name, v := range map[string]*float64{"temperature": req.Temperature, "top_p": req.TopP} {
		if v != nil && (*v < 0 || *v > 1) {
			return fmt.Errorf("%s is not between 0 and 1", name)
		}
	}
	if req.Temperature != nil && req.TopP != nil {
		return errors.New("temperature and top_p stand together")
	}
	for _, s := range req.Stop {
		if strings.TrimSpace(s) == "" {
			return errors.New("a stop sequence of white space only")
		}
	}
	return nil
}

func soundToolID(id string) bool {
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return id != ""
}

func soundImage(raw json.RawMessage) bool {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(raw, &src) != nil {
		return false
	}
	switch src.Type {
	case "base64":
		return imageTypes[src.MediaType] && src.Data != ""
	case "url":
		return strings.HasPrefix(src.URL, "https://") || strings.HasPrefix(src.URL, "http://")
	}
	return false
}

// soundResult reports whether a tool_result's content is a string, or a list
// of text and image blocks that is not empty.
func soundResult(raw json.RawMessage) bool {
	if _, ok := asString(raw); ok {
		return true
	}
	var blocks []struct {
		Type   string          `json:"type"`
		Text   *string         `json:"text"`
		Source json.RawMessage `json:"source"`
	}
	if json.Unmarshal(raw, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text == nil || strings.TrimSpace(*b.Text) == "" {
				return false
			}
		case "image":
			if !soundImage(b.Source) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
