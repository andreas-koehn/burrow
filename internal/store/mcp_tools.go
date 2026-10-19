package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/db"
)

// Tools of tool sources (MCP gateway): filled by the sync (internal/mcpgw),
// approved and overridden by admins through the API.

var (
	// ErrMCPToolNotFound: the source has no tool of that name.
	ErrMCPToolNotFound = errors.New("store: tool not found")
	// ErrMCPToolNotPending: the tool has no definition waiting for approval.
	ErrMCPToolNotPending = errors.New("store: tool is not waiting for approval")
	// ErrMCPToolChanged: the tool's definition changed since the approver read it.
	ErrMCPToolChanged = errors.New("store: tool definition changed since it was read")
	// ErrInvalidMCPTool is wrapped with the reason an override was refused;
	// the reason is safe to show to an admin.
	ErrInvalidMCPTool = errors.New("store: invalid tool override")
)

// MaxToolDescriptionOverride is the longest description override, in bytes.
const MaxToolDescriptionOverride = 8 << 10

// MaxAggregatedToolName is the longest name a tool may have on the
// aggregated endpoint /mcp, where it is named <slug>__<name> (D11).
const MaxAggregatedToolName = 64

var toolRenameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func invalidTool(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidMCPTool, reason) }

// ValidToolRename checks a rename override of a tool of the source slug: it
// must work on both endpoints, so it is made of A-Z, a-z, 0-9, _ and - and
// <slug>__<rename> is at most 64 characters (D11). The reason names the rule.
func ValidToolRename(slug, rename string) error {
	if !toolRenameRe.MatchString(rename) {
		return invalidTool("rename may contain only A-Z, a-z, 0-9, _ and -")
	}
	if max := MaxAggregatedToolName - len(slug) - 2; len(rename) > max {
		return invalidTool(fmt.Sprintf("rename must be at most %d characters for this source (%s__<name> is at most %d)", max, slug, MaxAggregatedToolName))
	}
	return nil
}

func validDescriptionOverride(s string) error {
	if len(s) > MaxToolDescriptionOverride || !utf8.ValidString(s) ||
		strings.ContainsFunc(s, func(r rune) bool { return r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f) }) {
		return invalidTool("description_override must be at most 8192 bytes of text without control characters other than tab and newline")
	}
	return nil
}

func (s *Store) sourceExists(ctx context.Context, slug string) error {
	if _, err := s.q.GetMCPSource(ctx, slug); errors.Is(err, db.ErrNotFound) {
		return ErrMCPSourceNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// ListMCPTools returns the tools of a source ordered by name, removed ones
// included. ErrMCPSourceNotFound.
func (s *Store) ListMCPTools(ctx context.Context, slug string) ([]db.MCPTool, error) {
	if err := s.sourceExists(ctx, slug); err != nil {
		return nil, err
	}
	return s.q.ListMCPTools(ctx, slug)
}

// MCPTool returns one tool. ErrMCPToolNotFound.
func (s *Store) MCPTool(ctx context.Context, slug, name string) (db.MCPTool, error) {
	t, err := s.q.GetMCPTool(ctx, slug, name)
	if errors.Is(err, db.ErrNotFound) {
		return db.MCPTool{}, ErrMCPToolNotFound
	}
	return t, err
}

// ApplyMCPToolSync stores what a sync read (see db.ApplyMCPToolSync).
// ErrMCPSourceNotFound.
func (s *Store) ApplyMCPToolSync(ctx context.Context, slug string, seen []db.MCPToolSeen, at time.Time, warning string) (db.MCPSyncCounts, error) {
	c, err := s.q.ApplyMCPToolSync(ctx, slug, seen, at, warning)
	if errors.Is(err, db.ErrNotFound) {
		return c, ErrMCPSourceNotFound
	}
	return c, err
}

// RecordMCPSyncError records a failed sync on the source. ErrMCPSourceNotFound.
func (s *Store) RecordMCPSyncError(ctx context.Context, slug string, at time.Time, msg string) error {
	if err := s.q.SetMCPSourceSyncError(ctx, slug, at, msg); errors.Is(err, db.ErrNotFound) {
		return ErrMCPSourceNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// ApproveMCPTool approves the pending definition of a tool. A non-empty hash
// must be the tool's current hash (the definition the approver saw).
// ErrMCPToolNotFound, ErrMCPToolNotPending, ErrMCPToolChanged.
func (s *Store) ApproveMCPTool(ctx context.Context, slug, name, hash string) (db.MCPTool, error) {
	t, err := s.q.ApproveMCPTool(ctx, slug, name, hash, time.Now().UTC())
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.MCPTool{}, ErrMCPToolNotFound
	case errors.Is(err, db.ErrMCPToolNotPending):
		return t, ErrMCPToolNotPending
	case errors.Is(err, db.ErrMCPToolChanged):
		return t, ErrMCPToolChanged
	}
	return t, err
}

// ApproveAllMCPTools approves the pending tools of a source (with hashes
// non-nil: those it names, at the hash given) and returns their names.
// ErrMCPSourceNotFound.
func (s *Store) ApproveAllMCPTools(ctx context.Context, slug string, hashes map[string]string) ([]string, error) {
	if err := s.sourceExists(ctx, slug); err != nil {
		return nil, err
	}
	return s.q.ApproveAllMCPTools(ctx, slug, hashes, time.Now().UTC())
}

// MCPToolOverridesUpdate holds the overrides PUT changes: a nil field keeps
// its value, "" clears Rename and DescriptionOverride. ReadOnlySet with a nil
// ReadOnly goes back to the upstream's hint.
type MCPToolOverridesUpdate struct {
	Hidden              *bool
	Rename              *string
	DescriptionOverride *string
	ReadOnlySet         bool
	ReadOnly            *bool
}

// UpdateMCPToolOverrides changes a tool's overrides. A rename must be valid
// (ValidToolRename) and must not give the tool the name another tool of the
// source is served under; a rename equal to the upstream name clears it.
// ErrMCPToolNotFound, ErrInvalidMCPTool, ErrMCPSourceBusy.
func (s *Store) UpdateMCPToolOverrides(ctx context.Context, slug, name string, u MCPToolOverridesUpdate) (db.MCPTool, error) {
	if u.Rename != nil && *u.Rename != "" && *u.Rename != name {
		if err := ValidToolRename(slug, *u.Rename); err != nil {
			return db.MCPTool{}, err
		}
	}
	if u.DescriptionOverride != nil {
		if err := validDescriptionOverride(*u.DescriptionOverride); err != nil {
			return db.MCPTool{}, err
		}
	}
	t, err := s.q.ModifyMCPToolOverrides(ctx, slug, name, time.Now().UTC(), func(cur db.MCPTool) (db.MCPToolOverrides, error) {
		o := db.MCPToolOverrides{Hidden: cur.Hidden, Rename: cur.Rename, DescriptionOverride: cur.DescriptionOverride, ReadOnly: cur.ReadOnly}
		if u.Hidden != nil {
			o.Hidden = *u.Hidden
		}
		if u.Rename != nil {
			o.Rename = *u.Rename
			if o.Rename == name {
				o.Rename = ""
			}
		}
		if u.DescriptionOverride != nil {
			o.DescriptionOverride = *u.DescriptionOverride
		}
		if u.ReadOnlySet {
			o.ReadOnly = u.ReadOnly
		}
		if o.Rename != "" && o.Rename != cur.Rename {
			if err := s.renameFree(ctx, slug, name, o.Rename); err != nil {
				return o, err
			}
		}
		return o, nil
	})
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.MCPTool{}, ErrMCPToolNotFound
	case errors.Is(err, db.ErrMCPSourceBusy):
		return db.MCPTool{}, ErrMCPSourceBusy
	}
	return t, err
}

// renameFree refuses a rename that is the served name of another tool of the
// source (its rename, or its upstream name when it has none).
func (s *Store) renameFree(ctx context.Context, slug, name, rename string) error {
	tools, err := s.q.ListMCPTools(ctx, slug)
	if err != nil {
		return err
	}
	for _, t := range tools {
		if t.Name == name {
			continue
		}
		served := t.Name
		if t.Rename != "" {
			served = t.Rename
		}
		if served == rename || t.Name == rename {
			return invalidTool(fmt.Sprintf("rename %q is already the name of the tool %q", rename, t.Name))
		}
	}
	return nil
}
