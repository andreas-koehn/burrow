package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ankoehn/burrow/internal/aigw/translate/ir"
)

// CheckRequest reads a Chat Completions request body the way a strict
// OpenAI-compatible server does, and refuses a conversation such a server
// answers with a 400. It is the yardstick for what the caller-side request
// decoders hand to EncodeRequest; the tests judge their output with it.
//
// It holds, and returns an error otherwise:
//   - "messages" is a list that is not empty, of the roles system,
//     developer, user, assistant and tool;
//   - a user message has content: a string that is not empty, or a list
//     that is not empty;
//   - an assistant message has a content string that is not empty, or tool
//     calls; a tool call has an id of its own within the message, a function
//     name, and arguments that are the text of one JSON object;
//   - no assistant message follows another directly;
//   - every tool call is answered: the messages after an assistant message
//     with tool calls are tool messages, one for each of its ids, before
//     anything else and before the end;
//   - a tool message stands nowhere else, names one of those ids, and no id
//     is answered twice.
func CheckRequest(body []byte) error {
	var req struct {
		Messages []struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"tool_call_id"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string  `json:"name"`
					Arguments *string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New("the body is not a JSON object with messages")
	}
	if len(req.Messages) == 0 {
		return errors.New("no messages")
	}
	empty := func(raw json.RawMessage) bool {
		switch string(bytes.TrimSpace(raw)) {
		case "", "null", `""`, "[]":
			return true
		}
		return false
	}
	var waiting map[string]bool // the calls of the last assistant message that have no answer yet
	last := ""
	for i, m := range req.Messages {
		fail := func(what string) error { return fmt.Errorf("messages[%d] (%s): %s", i, m.Role, what) }
		if m.Role != "tool" && len(waiting) > 0 {
			return fail("stands before every tool call of the assistant message was answered")
		}
		switch m.Role {
		case "system", "developer":
		case "user":
			if empty(m.Content) {
				return fail("has no content")
			}
		case "assistant":
			if last == "assistant" {
				return fail("follows an assistant message")
			}
			if empty(m.Content) && len(m.ToolCalls) == 0 {
				return fail("has neither content nor tool calls")
			}
			waiting = map[string]bool{}
			for _, c := range m.ToolCalls {
				if c.ID == "" || waiting[c.ID] || c.Function.Name == "" || c.Function.Arguments == nil || ir.CheckObject([]byte(*c.Function.Arguments)) != nil {
					return fail("has a tool call without an id of its own, a name, or arguments that are a JSON object")
				}
				waiting[c.ID] = true
			}
		case "tool":
			if !waiting[m.CallID] {
				return fail("answers no tool call of the assistant message before it")
			}
			delete(waiting, m.CallID)
		default:
			return fail("is of no known role")
		}
		last = m.Role
	}
	if len(waiting) > 0 {
		return errors.New("the last assistant message has tool calls without an answer")
	}
	return nil
}

// CheckPairing is the part of CheckRequest that a request decoder's pairing
// of tool calls and tool results answers for, whatever a caller wrote into
// its messages: every tool message follows the assistant message that holds
// its call, every call is answered there, and no assistant message follows
// another directly.
func CheckPairing(body []byte) error {
	var req struct {
		Messages []struct {
			Role      string `json:"role"`
			CallID    string `json:"tool_call_id"`
			ToolCalls []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return errors.New("the body is not a JSON object with messages")
	}
	var waiting map[string]bool
	last := ""
	for i, m := range req.Messages {
		switch {
		case m.Role == "tool":
			if !waiting[m.CallID] {
				return fmt.Errorf("messages[%d]: a tool message for no waiting call", i)
			}
			delete(waiting, m.CallID)
		case len(waiting) > 0:
			return fmt.Errorf("messages[%d]: %s before every call was answered", i, m.Role)
		case m.Role == "assistant":
			if last == "assistant" {
				return fmt.Errorf("messages[%d]: two assistant messages in a row", i)
			}
			waiting = map[string]bool{}
			for _, c := range m.ToolCalls {
				if waiting[c.ID] {
					return fmt.Errorf("messages[%d]: one call id twice", i)
				}
				waiting[c.ID] = true
			}
		}
		last = m.Role
	}
	if len(waiting) > 0 {
		return errors.New("calls without an answer at the end")
	}
	return nil
}
