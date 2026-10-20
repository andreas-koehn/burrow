package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The states of a tool (mcp_tools.status). Only an approved tool is served.
const (
	MCPToolApproved = "approved"
	MCPToolPending  = "pending"
	MCPToolRemoved  = "removed"
)

// ErrMCPToolNotPending is returned by ApproveMCPTool for a tool that has no
// definition waiting for approval (already approved, or removed upstream).
var ErrMCPToolNotPending = errors.New("db: tool is not waiting for approval")

// ErrMCPToolNameTaken is wrapped (with the other tool's name) when a tool
// would be served under a name another tool of the source already has.
var ErrMCPToolNameTaken = errors.New("db: tool name already taken in the source")

// ErrMCPToolChanged is returned by ApproveMCPTool when the tool's current
// definition is no longer the one the approver saw (its hash differs).
var ErrMCPToolChanged = errors.New("db: tool definition changed since it was read")

// MCPTool is a row of the mcp_tools table: one tool of a tool source as the
// last sync saw it, with what an admin approved and the operator's overrides.
type MCPTool struct {
	SourceSlug string
	Name       string // the upstream's own name

	// CurrentHash and Definition are the upstream's definition at the last
	// sync; ApprovedHash and ApprovedDefinition are what an admin approved
	// ("" before the first approval). Definitions are canonical JSON.
	ApprovedHash       string
	CurrentHash        string
	Definition         string
	ApprovedDefinition string
	Status             string // MCPToolApproved, MCPToolPending or MCPToolRemoved

	// Overrides.
	Hidden              bool
	Rename              string // "" = the upstream name
	DescriptionOverride string // "" = the approved description
	ReadOnly            *bool  // nil = the upstream's readOnlyHint

	// GuardrailHits is a JSON array of the guardrail pattern hits in the
	// current definition: [{"field":"description","pattern":"<id>"}, …].
	GuardrailHits string
	UpdatedAt     time.Time
}

// MCPToolSeen is one tool as a sync read it from the upstream.
type MCPToolSeen struct {
	Name          string
	Hash          string // sha256 of Definition
	Definition    string // canonical JSON
	GuardrailHits string // JSON array
}

// MCPSyncCounts tells what a sync changed.
type MCPSyncCounts struct {
	Added   int // tools seen for the first time (or again after removal)
	Changed int // tools whose definition differs from the last sync's
	Removed int // tools the upstream no longer lists
}

const mcpToolCols = `source_slug, name, approved_hash, current_hash, definition, approved_definition, status, hidden, rename, description_override, read_only, guardrail_hits, updated_at`

func scanMCPTool(row interface{ Scan(...any) error }) (MCPTool, error) {
	var t MCPTool
	var ro sql.NullInt64
	err := row.Scan(&t.SourceSlug, &t.Name, &t.ApprovedHash, &t.CurrentHash, &t.Definition, &t.ApprovedDefinition, &t.Status,
		(*intBool)(&t.Hidden), &t.Rename, &t.DescriptionOverride, &ro, &t.GuardrailHits, &t.UpdatedAt)
	if err != nil {
		return t, err
	}
	if ro.Valid {
		b := ro.Int64 != 0
		t.ReadOnly = &b
	}
	return t, nil
}

// ListMCPTools returns the tools of a source ordered by name (never nil),
// removed ones included.
func (x *DB) ListMCPTools(ctx context.Context, slug string) ([]MCPTool, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT `+mcpToolCols+` FROM mcp_tools WHERE source_slug=? ORDER BY name`, slug)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defer rows.Close()
	out := make([]MCPTool, 0)
	for rows.Next() {
		t, err := scanMCPTool(rows)
		if err != nil {
			return nil, fmt.Errorf("list tools: scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListMCPToolsForSlugs returns the tools of the sources named by slugs, in
// one query, by source slug and ordered by name within each. A slug with no
// tools (or no source) has no entry.
func (x *DB) ListMCPToolsForSlugs(ctx context.Context, slugs []string) (map[string][]MCPTool, error) {
	out := map[string][]MCPTool{}
	if len(slugs) == 0 {
		return out, nil
	}
	args := make([]any, len(slugs))
	for i, s := range slugs {
		args[i] = s
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(slugs)), ",")
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+mcpToolCols+` FROM mcp_tools WHERE source_slug IN (`+marks+`) ORDER BY source_slug, name`, args...)
	if err != nil {
		return nil, fmt.Errorf("list tools: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanMCPTool(rows)
		if err != nil {
			return nil, fmt.Errorf("list tools: scan: %w", err)
		}
		out[t.SourceSlug] = append(out[t.SourceSlug], t)
	}
	return out, rows.Err()
}

// GetMCPTool returns one tool, or ErrNotFound.
func (x *DB) GetMCPTool(ctx context.Context, slug, name string) (MCPTool, error) {
	t, err := scanMCPTool(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+mcpToolCols+` FROM mcp_tools WHERE source_slug=? AND name=?`, slug, name))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPTool{}, ErrNotFound
	}
	if err != nil {
		return MCPTool{}, fmt.Errorf("get tool: %w", err)
	}
	return t, nil
}

// ApplyMCPToolSync stores what a sync read from the upstream, in one
// transaction, and records the sync as successful at `at` (last_sync_error
// becomes warning, "" for a clean sync):
//
//   - a tool seen for the first time is inserted as pending;
//   - a tool whose hash equals its approved hash is (or becomes again)
//     approved; any other hash makes it pending, and the approved definition
//     is kept for the side-by-side view;
//   - a tool the upstream no longer lists becomes removed (its approval and
//     overrides are kept, so it is served again only if it comes back
//     unchanged).
//
// Each tool's status is decided inside its own UPDATE from the row's
// approved hash, so an approval that lands during the sync is never lost or
// applied to a definition the approver did not see. Callers serialise syncs
// of one source. ErrNotFound when the source does not exist.
func (x *DB) ApplyMCPToolSync(ctx context.Context, slug string, seen []MCPToolSeen, at time.Time, warning string) (MCPSyncCounts, error) {
	var c MCPSyncCounts
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return c, fmt.Errorf("begin tool sync tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE mcp_sources SET last_sync_at=?, last_sync_error=? WHERE slug=?`, at, warning, slug)
	if err != nil {
		return c, fmt.Errorf("tool sync: source: %w", err)
	}
	if err := notFoundIfNoRows(res, "tool sync: source"); err != nil {
		return c, err
	}

	type prev struct{ hash, status string }
	old := map[string]prev{}
	rows, err := tx.QueryContext(ctx, `SELECT name, current_hash, status FROM mcp_tools WHERE source_slug=?`, slug)
	if err != nil {
		return c, fmt.Errorf("tool sync: read: %w", err)
	}
	for rows.Next() {
		var n string
		var p prev
		if err := rows.Scan(&n, &p.hash, &p.status); err != nil {
			_ = rows.Close()
			return c, fmt.Errorf("tool sync: scan: %w", err)
		}
		old[n] = p
	}
	if err := rows.Close(); err != nil {
		return c, fmt.Errorf("tool sync: read: %w", err)
	}

	listed := make(map[string]bool, len(seen))
	for _, s := range seen {
		listed[s.Name] = true
		p, known := old[s.Name]
		if !known {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO mcp_tools(source_slug, name, current_hash, definition, status, guardrail_hits, updated_at) VALUES(?,?,?,?,?,?,?)`,
				slug, s.Name, s.Hash, s.Definition, MCPToolPending, s.GuardrailHits, at); err != nil {
				return c, fmt.Errorf("tool sync: insert: %w", err)
			}
			c.Added++
			continue
		}
		switch {
		case p.status == MCPToolRemoved:
			c.Added++
		case p.hash != s.Hash:
			c.Changed++
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mcp_tools SET
			   updated_at = CASE WHEN current_hash<>? OR status=? THEN ? ELSE updated_at END,
			   status = CASE WHEN approved_hash=? THEN ? ELSE ? END,
			   current_hash=?, definition=?, guardrail_hits=?
			 WHERE source_slug=? AND name=?`,
			s.Hash, MCPToolRemoved, at,
			s.Hash, MCPToolApproved, MCPToolPending,
			s.Hash, s.Definition, s.GuardrailHits,
			slug, s.Name); err != nil {
			return c, fmt.Errorf("tool sync: update: %w", err)
		}
	}
	for n, p := range old {
		if listed[n] || p.status == MCPToolRemoved {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE mcp_tools SET status=?, updated_at=? WHERE source_slug=? AND name=?`, MCPToolRemoved, at, slug, n); err != nil {
			return c, fmt.Errorf("tool sync: remove: %w", err)
		}
		c.Removed++
	}
	if err := tx.Commit(); err != nil {
		return c, fmt.Errorf("commit tool sync tx: %w", err)
	}
	return c, nil
}

// SetMCPSourceSyncError records a failed sync; the tools stay as they are.
// ErrNotFound when the source does not exist.
func (x *DB) SetMCPSourceSyncError(ctx context.Context, slug string, at time.Time, msg string) error {
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE mcp_sources SET last_sync_at=?, last_sync_error=? WHERE slug=?`, at, msg, slug)
	if err != nil {
		return fmt.Errorf("set tool sync error: %w", err)
	}
	return notFoundIfNoRows(res, "set tool sync error")
}

// lockSource serialises the writes that check served names across a source's
// tools: the no-op UPDATE takes the source row's lock (Postgres) or the
// write lock (SQLite) until the transaction ends. ErrNotFound.
func lockSource(ctx context.Context, tx *sql.Tx, slug string) error {
	res, err := tx.ExecContext(ctx, `UPDATE mcp_sources SET updated_at=updated_at WHERE slug=?`, slug)
	if err != nil {
		return fmt.Errorf("lock tool source: %w", err)
	}
	return notFoundIfNoRows(res, "lock tool source")
}

// servedNameTaken returns the native name of another tool of the source
// that is served under served (approved, not removed), or "".
func servedNameTaken(ctx context.Context, tx *sql.Tx, slug, name, served string) (string, error) {
	var other string
	err := tx.QueryRowContext(ctx,
		`SELECT name FROM mcp_tools WHERE source_slug=? AND name<>? AND status=?
		   AND (CASE WHEN rename<>'' THEN rename ELSE name END)=? LIMIT 1`,
		slug, name, MCPToolApproved, served).Scan(&other)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("check served name: %w", err)
	}
	return other, nil
}

func getMCPToolTx(ctx context.Context, tx *sql.Tx, slug, name string) (MCPTool, error) {
	t, err := scanMCPTool(tx.QueryRowContext(ctx, `SELECT `+mcpToolCols+` FROM mcp_tools WHERE source_slug=? AND name=?`, slug, name))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPTool{}, ErrNotFound
	}
	if err != nil {
		return MCPTool{}, fmt.Errorf("get tool: %w", err)
	}
	return t, nil
}

// approveTx approves one pending tool at hash inside tx: ErrMCPToolNotPending,
// ErrMCPToolChanged, or ErrMCPToolNameTaken when another approved tool is
// served under the name this one would be served under.
func approveTx(ctx context.Context, tx *sql.Tx, slug, name, hash string, at time.Time) error {
	t, err := getMCPToolTx(ctx, tx, slug, name)
	if err != nil {
		return err
	}
	if t.Status != MCPToolPending {
		return ErrMCPToolNotPending
	}
	if t.CurrentHash != hash {
		return ErrMCPToolChanged
	}
	served := t.Name
	if t.Rename != "" {
		served = t.Rename
	}
	if other, err := servedNameTaken(ctx, tx, slug, name, served); err != nil {
		return err
	} else if other != "" {
		return fmt.Errorf("%w: %q is already served under the name %q", ErrMCPToolNameTaken, other, served)
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE mcp_tools SET approved_hash=current_hash, approved_definition=definition, status=?, updated_at=?
		  WHERE source_slug=? AND name=? AND status=? AND current_hash=?`,
		MCPToolApproved, at, slug, name, MCPToolPending, hash)
	if err != nil {
		return fmt.Errorf("approve tool: %w", err)
	}
	return nil
}

// ApproveMCPTool approves the current definition of a pending tool; hash
// must equal the tool's current hash (the definition the approver saw).
// ErrNotFound, ErrMCPToolNotPending, ErrMCPToolChanged, ErrMCPToolNameTaken.
func (x *DB) ApproveMCPTool(ctx context.Context, slug, name, hash string, at time.Time) (MCPTool, error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return MCPTool{}, fmt.Errorf("begin approve tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSource(ctx, tx, slug); err != nil {
		return MCPTool{}, err
	}
	if err := approveTx(ctx, tx, slug, name, hash, at); err != nil {
		return MCPTool{}, err
	}
	if err := tx.Commit(); err != nil {
		return MCPTool{}, fmt.Errorf("commit approve tx: %w", err)
	}
	return x.GetMCPTool(ctx, slug, name)
}

// ApproveAllMCPTools approves the pending tools hashes names, each only
// while its current hash is the one given, and returns the names it
// approved and those it left because another approved tool is served under
// their name (both sorted).
func (x *DB) ApproveAllMCPTools(ctx context.Context, slug string, hashes map[string]string, at time.Time) (approved, conflicts []string, err error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin approve-all tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSource(ctx, tx, slug); err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(hashes))
	for n := range hashes {
		names = append(names, n)
	}
	sort.Strings(names)
	approved, conflicts = []string{}, []string{}
	for _, n := range names {
		switch err := approveTx(ctx, tx, slug, n, hashes[n], at); {
		case err == nil:
			approved = append(approved, n)
		case errors.Is(err, ErrMCPToolNameTaken):
			conflicts = append(conflicts, n)
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrMCPToolNotPending), errors.Is(err, ErrMCPToolChanged):
		default:
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit approve-all tx: %w", err)
	}
	return approved, conflicts, nil
}

// MCPToolOverrides are the operator's settings of one tool.
type MCPToolOverrides struct {
	Hidden              bool
	Rename              string
	DescriptionOverride string
	ReadOnly            *bool
}

// ModifyMCPToolOverrides reads the tool, hands it to modify and stores the
// overrides it returns, in one transaction that holds the source's lock: a
// new rename is refused with ErrMCPToolNameTaken when another tool of the
// source (not removed) is named or served under it, and no concurrent write
// can slip in between the check and the update. modify's error is returned
// as is. ErrNotFound.
func (x *DB) ModifyMCPToolOverrides(ctx context.Context, slug, name string, at time.Time, modify func(MCPTool) (MCPToolOverrides, error)) (MCPTool, error) {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return MCPTool{}, fmt.Errorf("begin tool overrides tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSource(ctx, tx, slug); err != nil {
		return MCPTool{}, err
	}
	old, err := getMCPToolTx(ctx, tx, slug, name)
	if err != nil {
		return MCPTool{}, err
	}
	o, err := modify(old)
	if err != nil {
		return MCPTool{}, err
	}
	if o.Rename != "" && o.Rename != old.Rename {
		var other string
		err := tx.QueryRowContext(ctx,
			`SELECT name FROM mcp_tools WHERE source_slug=? AND name<>? AND status<>?
			   AND (name=? OR rename=?) LIMIT 1`,
			slug, name, MCPToolRemoved, o.Rename, o.Rename).Scan(&other)
		switch {
		case err == nil:
			return MCPTool{}, fmt.Errorf("%w: rename %q is already the name of the tool %q", ErrMCPToolNameTaken, o.Rename, other)
		case !errors.Is(err, sql.ErrNoRows):
			return MCPTool{}, fmt.Errorf("check rename: %w", err)
		}
	}
	var ro any
	if o.ReadOnly != nil {
		ro = boolToInt(*o.ReadOnly)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE mcp_tools SET hidden=?, rename=?, description_override=?, read_only=?, updated_at=? WHERE source_slug=? AND name=?`,
		boolToInt(o.Hidden), o.Rename, o.DescriptionOverride, ro, at, slug, name); err != nil {
		return MCPTool{}, fmt.Errorf("modify tool overrides: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MCPTool{}, fmt.Errorf("commit tool overrides tx: %w", err)
	}
	return x.GetMCPTool(ctx, slug, name)
}
