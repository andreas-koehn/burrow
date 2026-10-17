package guardrails

import "testing"

func TestEffective(t *testing.T) {
	custom := []CustomPattern{{ID: "ticket", Pattern: "T-1", Direction: DirectionBoth}}
	globalOn := Settings{Enabled: true, Action: ActionRefuse403, Answers: AnswerSettings{Enabled: true, Action: AnswerActionBlock}, CustomPatterns: custom}
	globalOff := Settings{Enabled: false, Action: ActionLogOnly, Answers: AnswerSettings{Enabled: true, Action: AnswerActionFlag}, CustomPatterns: custom}
	ownOn := &Settings{Enabled: true, Action: ActionRefuseSafe, Answers: AnswerSettings{Enabled: false}, CustomPatterns: []CustomPattern{{ID: "own"}}}
	ownOff := &Settings{Enabled: false, Action: ActionLogOnly}

	// No section: the global settings, as they are.
	if got := Effective(nil, globalOn); got == nil || !got.Enabled || got.Action != ActionRefuse403 || got.Answers.Action != AnswerActionBlock || len(got.CustomPatterns) != 1 {
		t.Fatalf("no section, global on: %+v", got)
	}
	// No section and the request patterns off: answer inspection still runs
	// on its own switch, the request tier stays off.
	if got := Effective(nil, globalOff); got == nil || got.Enabled || !got.Answers.Enabled || got.Answers.Action != AnswerActionFlag || len(got.CustomPatterns) != 1 {
		t.Fatalf("no section, global off: %+v", got)
	}
	// Everything off: none (the service keeps its path through the relay).
	if got := Effective(nil, Settings{Enabled: false, Action: ActionLogOnly}); got != nil {
		t.Fatalf("no section, all off: %+v", got)
	}
	// A section that is on overrides enabled and action; answers and custom
	// patterns stay global.
	got := Effective(ownOn, globalOn)
	if !got.Enabled || got.Action != ActionRefuseSafe || !got.Answers.Enabled || got.Answers.Action != AnswerActionBlock || len(got.CustomPatterns) != 1 || got.CustomPatterns[0].ID != "ticket" {
		t.Fatalf("own on: %+v", got)
	}
	if got := Effective(ownOn, globalOff); !got.Enabled || got.Action != ActionRefuseSafe || got.Answers.Action != AnswerActionFlag {
		t.Fatalf("own on, global off: %+v", got)
	}
	// A section that is off counts as no override: the global settings.
	if got := Effective(ownOff, globalOn); !got.Enabled || got.Action != ActionRefuse403 || !got.Answers.Enabled {
		t.Fatalf("own off, global on: %+v", got)
	}
	// Both off: the request tier off, the answers on their own switch.
	if got := Effective(ownOff, globalOff); got == nil || got.Enabled || !got.Answers.Enabled || len(got.CustomPatterns) != 1 {
		t.Fatalf("own off, global off: %+v", got)
	}
	// The result is a copy: changing it leaves the inputs alone.
	got = Effective(nil, globalOn)
	got.Enabled = false
	if !globalOn.Enabled {
		t.Fatal("Effective returned the global settings themselves")
	}
}
