package authz

import (
	"slices"
	"strings"
)

// Preset names. A token never stores them: it stores the permissions.
const (
	PresetReadOnly = "read-only"
	PresetOperator = "operator"
	PresetFull     = "full"
)

// Preset is a named set of permissions offered when an automation token is
// made. Changing a preset changes what new tokens are offered, never what an
// existing token holds.
type Preset struct {
	Name        string
	Description string
	Permissions []Permission
}

// operatorExcluded is what an operator token cannot do: manage people,
// roles, other users' tokens, the relay's settings and its backups.
var operatorExcluded = []Permission{
	PermUsersManage, PermRolesManage, PermTokensManageAny,
	PermAutomationTokensManageAny, PermSettingsManage, PermBackupRun,
}

// Presets returns the presets in the order the dashboard offers them, with
// permissions in AllPermissions order. The slices are the caller's own.
func Presets() []Preset {
	var readOnly, operator []Permission
	for _, p := range AllPermissions() {
		if strings.Contains(string(p), ":read") {
			readOnly = append(readOnly, p)
		}
		if !slices.Contains(operatorExcluded, p) {
			operator = append(operator, p)
		}
	}
	return []Preset{
		{Name: PresetReadOnly, Description: "Reads everything that has a read permission (not services or budgets), changes nothing.", Permissions: readOnly},
		{Name: PresetOperator, Description: "Runs services and the AI gateway; no users, roles, settings or backups.", Permissions: operator},
		{Name: PresetFull, Description: "Everything its owner may do.", Permissions: AllPermissions()},
	}
}
