package store

import (
	"strings"
	"testing"
)

func TestModelAllowed(t *testing.T) {
	list := []string{"burrow-simple", "zai/*", "openrouter/google/gemini-x"}
	yes := []string{"burrow-simple", "zai/glm-5.1", "zai/a/b", "openrouter/google/gemini-x"}
	no := []string{"burrow-medium", "zai", "zai/", "zaix/glm", "openrouter/google/gemini-y", "openrouter/google", "", "burrow-simple/x"}
	for _, n := range yes {
		if !ModelAllowed(list, n) {
			t.Errorf("ModelAllowed(%q) = false", n)
		}
	}
	for _, n := range no {
		if ModelAllowed(list, n) {
			t.Errorf("ModelAllowed(%q) = true", n)
		}
	}
	if !ModelAllowed(nil, "anything") || !ModelAllowed([]string{}, "zai/x") {
		t.Error("an empty list must allow everything")
	}
}

func TestValidAllowEntry(t *testing.T) {
	good := []string{"burrow-simple", "zai/*", "zai/glm-5.1", "openrouter/google/gemini-x"}
	bad := []string{"", "*", "*/x", "zai/", "/x", "Zai/*", "zai/**", "zai/*/x", "a b", "v1/*", "zai/a b", "zai/a\x00b", strings.Repeat("a", 300)}
	for _, e := range good {
		if !ValidAllowEntry(e) {
			t.Errorf("ValidAllowEntry(%q) = false", e)
		}
	}
	for _, e := range bad {
		if ValidAllowEntry(e) {
			t.Errorf("ValidAllowEntry(%q) = true", e)
		}
	}
}

func TestToolAllowed(t *testing.T) {
	list := []string{"github__*", "jira__create_issue", "fs__read__file"}
	yes := []string{"github__create_issue", "github__a__b", "jira__create_issue", "fs__read__file"}
	no := []string{"github", "github__", "gitlab__create_issue", "jira__create_issues", "jira__", "jira__*", "githubx__a", "fs__read", "", "__x"}
	for _, n := range yes {
		if !ToolAllowed(list, n) {
			t.Errorf("ToolAllowed(%q) = false", n)
		}
	}
	for _, n := range no {
		if ToolAllowed(list, n) {
			t.Errorf("ToolAllowed(%q) = true", n)
		}
	}
	// Unlike models, an empty list allows nothing.
	if ToolAllowed(nil, "github__x") || ToolAllowed([]string{}, "github__x") {
		t.Error("an empty tool list must allow nothing")
	}
}

func TestToolAllowedNative(t *testing.T) {
	list := []string{"github__*", "jira__create_issue"}
	if !ToolAllowedNative(list, "github", "anything") || !ToolAllowedNative(list, "jira", "create_issue") {
		t.Error("allowed pairs refused")
	}
	for _, c := range [][2]string{{"jira", "other"}, {"gitlab", "x"}, {"github", ""}, {"", "x"}, {"jira", "*"}} {
		if ToolAllowedNative(list, c[0], c[1]) {
			t.Errorf("ToolAllowedNative(%q, %q) = true", c[0], c[1])
		}
	}
	if ToolAllowedNative(nil, "github", "x") {
		t.Error("an empty tool list must allow nothing")
	}
}

func TestValidToolEntry(t *testing.T) {
	good := []string{"github__*", "github__create_issue", "a__b", "my-src__x.y/z", "fs__read__file", strings.Repeat("a", 24) + "__t"}
	bad := []string{"", "*", "__x", "github__", "github", "github_create", "GitHub__x", "burrow__x", "v1__*", "gh__a b", "gh__a*", "gh__a\x00b",
		strings.Repeat("a", 25) + "__t", "a_b__t", "gh__" + strings.Repeat("t", 200), "g h__x"}
	for _, e := range good {
		if !ValidToolEntry(e) {
			t.Errorf("ValidToolEntry(%q) = false", e)
		}
	}
	for _, e := range bad {
		if ValidToolEntry(e) {
			t.Errorf("ValidToolEntry(%q) = true", e)
		}
	}
}
