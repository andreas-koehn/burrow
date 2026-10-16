package audit

import (
	"slices"
	"testing"
)

// guardrail.flagged is a known, sampled action.
func TestGuardrailFlaggedIsListed(t *testing.T) {
	if !slices.Contains(AllActions, ActionGuardrailFlagged) {
		t.Fatal("guardrail.flagged is not in AllActions")
	}
	if !IsAggregated(ActionGuardrailFlagged) {
		t.Fatal("guardrail.flagged is not aggregated")
	}
}
