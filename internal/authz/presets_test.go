package authz

import (
	"slices"
	"strings"
	"testing"
)

func TestPresets(t *testing.T) {
	ps := Presets()
	if len(ps) != 3 || ps[0].Name != PresetReadOnly || ps[1].Name != PresetOperator || ps[2].Name != PresetFull {
		t.Fatalf("presets = %+v, want read-only, operator, full", ps)
	}
	all := AllPermissions()
	for _, p := range ps {
		if p.Description == "" {
			t.Errorf("%s has no description", p.Name)
		}
		seen := map[Permission]bool{}
		for _, perm := range p.Permissions {
			if !slices.Contains(all, perm) {
				t.Errorf("%s names unknown permission %s", p.Name, perm)
			}
			if seen[perm] {
				t.Errorf("%s names %s twice", p.Name, perm)
			}
			seen[perm] = true
		}
	}

	readOnly := ps[0].Permissions
	for _, perm := range all {
		want := strings.Contains(string(perm), ":read")
		if got := slices.Contains(readOnly, perm); got != want {
			t.Errorf("read-only has %s = %v, want %v", perm, got, want)
		}
	}
	for _, perm := range []Permission{PermMetricsRead, PermMcpToolsRead, PermInspectorReadAny, PermAuditRead} {
		if !slices.Contains(readOnly, perm) {
			t.Errorf("read-only lacks %s", perm)
		}
	}

	excluded := []Permission{PermUsersManage, PermRolesManage, PermTokensManageAny,
		PermAutomationTokensManageAny, PermSettingsManage, PermBackupRun}
	operator := ps[1].Permissions
	for _, perm := range all {
		if got, want := slices.Contains(operator, perm), !slices.Contains(excluded, perm); got != want {
			t.Errorf("operator has %s = %v, want %v", perm, got, want)
		}
	}

	if !slices.Equal(ps[2].Permissions, all) {
		t.Errorf("full = %v, want every permission in AllPermissions order", ps[2].Permissions)
	}

	// Callers get copies: changing one does not change the next call.
	ps[0].Permissions[0] = "x"
	if Presets()[0].Permissions[0] == "x" {
		t.Error("Presets returns shared slices")
	}
}
