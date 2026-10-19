package store

import (
	"regexp"
	"strings"
)

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

// maxToolEntry bounds one tool allow-list entry: a source slug (at most 24
// characters), "__" and a tool name.
const maxToolEntry = 160

// toolSourceRe is a tool source's slug: 1-24 characters of [a-z0-9-].
var toolSourceRe = regexp.MustCompile(`^[a-z0-9-]{1,24}$`)

// ValidToolSourceSlug reports whether s can name a tool source: 1-24
// characters of [a-z0-9-], not one of the reserved "burrow" and "v1".
func ValidToolSourceSlug(s string) bool {
	return toolSourceRe.MatchString(s) && s != "burrow" && s != "v1"
}

// ValidToolEntry reports whether entry is a well-formed tool allow-list entry:
// "<source>__<tool>" or "<source>__*". A source slug has no underscore, so the
// first "__" ends it and the tool name after it may contain more.
func ValidToolEntry(entry string) bool {
	if len(entry) > maxToolEntry {
		return false
	}
	source, tool, ok := strings.Cut(entry, "__")
	if !ok || !ValidToolSourceSlug(source) || tool == "" {
		return false
	}
	if tool == "*" {
		return true
	}
	return !strings.ContainsAny(tool, "* ") && !hasControl(tool)
}

// ToolAllowed reports whether a key restricted to allowed may call the tool
// name, given as "<source>__<tool>" (the name on the aggregated /mcp
// endpoint). Entries are exact names or "<source>__*". Unlike ModelAllowed an
// empty list allows nothing.
func ToolAllowed(allowed []string, name string) bool {
	source, tool, ok := strings.Cut(name, "__")
	if !ok {
		return false
	}
	return ToolAllowedNative(allowed, source, tool)
}

// ToolAllowedNative is ToolAllowed for a source and a tool name given apart,
// as on /mcp/<source> where tools keep their native names.
func ToolAllowedNative(allowed []string, source, tool string) bool {
	// source must be a slug: one holding "__" could match an entry of
	// another source ("a__b" + "c" against "a__b__c").
	if !ValidToolSourceSlug(source) || tool == "" || tool == "*" {
		return false
	}
	for _, e := range allowed {
		if e == source+"__"+tool || e == source+"__*" {
			return true
		}
	}
	return false
}
