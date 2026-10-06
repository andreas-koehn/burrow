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
