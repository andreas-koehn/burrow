package store

import "strings"

// maxAllowEntry bounds one allow-list entry: a provider slug, "/" and a
// model id of the length the model catalog accepts, with room to spare.
const maxAllowEntry = 264

// ValidAllowEntry reports whether entry is a well-formed allow-list entry: a
// synthetic model name, "<provider>/<model>", or "<provider>/*".
func ValidAllowEntry(entry string) bool {
	if len(entry) > maxAllowEntry {
		return false
	}
	provider, model, direct := strings.Cut(entry, "/")
	if !direct {
		return ValidModelName(entry)
	}
	if !ValidProviderSlug(provider) || model == "" {
		return false
	}
	if model == "*" {
		return true
	}
	return !strings.ContainsAny(model, "* ") && !hasControl(model)
}

// ModelAllowed reports whether a key restricted to allowed may use name. name
// is a synthetic model name or "<provider>/<model>". An empty list allows all.
func ModelAllowed(allowed []string, name string) bool {
	if len(allowed) == 0 {
		return true
	}
	if name == "" {
		return false
	}
	provider, model, direct := strings.Cut(name, "/")
	for _, e := range allowed {
		if e == name {
			return true
		}
		if direct && model != "" && e == provider+"/*" {
			return true
		}
	}
	return false
}
