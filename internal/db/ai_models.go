package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AIModel is a synthetic model: a name that resolves to an ordered list of
// provider targets per dialect.
type AIModel struct {
	Name                string
	Description         string
	Enabled             bool
	FallbackOnRateLimit bool
	AttemptTimeoutS     int             // seconds until the first response byte of one attempt
	TotalTimeoutS       int             // seconds across all attempts until a response starts
	Targets             []AIModelTarget // ordered by Dialect, then Position; never nil after a read
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// AIModelTarget is one provider/model a synthetic model can be sent to.
type AIModelTarget struct {
	Dialect      string // "openai" or "anthropic"; equals the provider's api_format
	Position     int    // order within the dialect
	ProviderSlug string
	TargetModel  string
}

// ErrDuplicateModel is returned when a model name is already in use.
var ErrDuplicateModel = errors.New("db: model name already in use")

// ErrProviderInUse is returned, wrapped with the model names, when a delete
// would remove a provider that a synthetic model still targets.
var ErrProviderInUse = errors.New("provider is used by a model")

const aiModelCols = `name, description, enabled, fallback_on_rate_limit, attempt_timeout_s, total_timeout_s, created_at, updated_at`

func scanAIModel(row interface{ Scan(...any) error }) (AIModel, error) {
	var m AIModel
	err := row.Scan(&m.Name, &m.Description, (*intBool)(&m.Enabled), (*intBool)(&m.FallbackOnRateLimit),
		&m.AttemptTimeoutS, &m.TotalTimeoutS, &m.CreatedAt, &m.UpdatedAt)
	m.Targets = []AIModelTarget{}
	return m, err
}

// insertAIModelTargets writes targets with positions renumbered 0..n-1 per dialect.
func insertAIModelTargets(ctx context.Context, tx *sql.Tx, name string, targets []AIModelTarget) error {
	next := map[string]int{}
	for _, t := range targets {
		pos := next[t.Dialect]
		next[t.Dialect] = pos + 1
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ai_model_targets(model_name, dialect, position, provider_slug, target_model) VALUES(?,?,?,?,?)`,
			name, t.Dialect, pos, t.ProviderSlug, t.TargetModel); err != nil {
			return fmt.Errorf("insert model target: %w", err)
		}
	}
	return nil
}

// CreateAIModel inserts the model and its targets in one transaction.
func (x *DB) CreateAIModel(ctx context.Context, m AIModel) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin create model tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ai_models(name, description, enabled, fallback_on_rate_limit, attempt_timeout_s, total_timeout_s)
		 VALUES(?,?,?,?,?,?)`,
		m.Name, m.Description, boolToInt(m.Enabled), boolToInt(m.FallbackOnRateLimit), m.AttemptTimeoutS, m.TotalTimeoutS); err != nil {
		_ = tx.Rollback()
		if isDuplicateServiceErr(err) {
			return ErrDuplicateModel
		}
		return fmt.Errorf("create ai model: %w", err)
	}
	if err := insertAIModelTargets(ctx, tx, m.Name, m.Targets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit create model tx: %w", err)
	}
	return nil
}

func loadAIModelTargets(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, where string, args ...any) (map[string][]AIModelTarget, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT model_name, dialect, position, provider_slug, target_model FROM ai_model_targets `+where+
			` ORDER BY model_name, dialect, position`, args...)
	if err != nil {
		return nil, fmt.Errorf("load model targets: %w", err)
	}
	defer rows.Close()
	out := map[string][]AIModelTarget{}
	for rows.Next() {
		var name string
		var t AIModelTarget
		if err := rows.Scan(&name, &t.Dialect, &t.Position, &t.ProviderSlug, &t.TargetModel); err != nil {
			return nil, fmt.Errorf("load model targets: scan: %w", err)
		}
		out[name] = append(out[name], t)
	}
	return out, rows.Err()
}

// GetAIModel returns the model with its targets, or ErrNotFound.
func (x *DB) GetAIModel(ctx context.Context, name string) (AIModel, error) {
	m, err := scanAIModel(x.sqlDB.QueryRowContext(ctx, `SELECT `+aiModelCols+` FROM ai_models WHERE name=?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return AIModel{}, ErrNotFound
	}
	if err != nil {
		return AIModel{}, fmt.Errorf("get ai model: %w", err)
	}
	ts, err := loadAIModelTargets(ctx, x.sqlDB, `WHERE model_name=?`, name)
	if err != nil {
		return AIModel{}, err
	}
	if t := ts[name]; t != nil {
		m.Targets = t
	}
	return m, nil
}

// ListAIModels returns every model ordered by name, with targets.
func (x *DB) ListAIModels(ctx context.Context) ([]AIModel, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT `+aiModelCols+` FROM ai_models ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list ai models: %w", err)
	}
	out := make([]AIModel, 0)
	for rows.Next() {
		m, err := scanAIModel(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("list ai models: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list ai models: %w", err)
	}
	rows.Close()
	ts, err := loadAIModelTargets(ctx, x.sqlDB, ``)
	if err != nil {
		return nil, err
	}
	for i := range out {
		if t := ts[out[i].Name]; t != nil {
			out[i].Targets = t
		}
	}
	return out, nil
}

// UpdateAIModel replaces the model's fields and targets; m.Name may differ
// from name (rename). ErrNotFound, ErrDuplicateModel.
func (x *DB) UpdateAIModel(ctx context.Context, name string, m AIModel) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update model tx: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE ai_models SET name=?, description=?, enabled=?, fallback_on_rate_limit=?, attempt_timeout_s=?, total_timeout_s=?, updated_at=CURRENT_TIMESTAMP
		  WHERE name=?`,
		m.Name, m.Description, boolToInt(m.Enabled), boolToInt(m.FallbackOnRateLimit), m.AttemptTimeoutS, m.TotalTimeoutS, name)
	if err != nil {
		_ = tx.Rollback()
		if isDuplicateServiceErr(err) {
			return ErrDuplicateModel
		}
		return fmt.Errorf("update ai model: %w", err)
	}
	if err := notFoundIfNoRows(res, "update ai model"); err != nil {
		_ = tx.Rollback()
		return err
	}
	// Foreign keys are on, so a rename has already moved the target rows to m.Name.
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_model_targets WHERE model_name=?`, m.Name); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("update ai model: clear targets: %w", err)
	}
	if err := insertAIModelTargets(ctx, tx, m.Name, m.Targets); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit update model tx: %w", err)
	}
	return nil
}

// DeleteAIModel removes the model; its targets go by cascade.
func (x *DB) DeleteAIModel(ctx context.Context, name string) error {
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM ai_models WHERE name=?`, name)
	if err != nil {
		return fmt.Errorf("delete ai model: %w", err)
	}
	return notFoundIfNoRows(res, "delete ai model")
}

// modelNamesUsing returns the sorted names of the models with a target on one
// of the providers the filter selects (never nil). The filter is a constant
// of this package; t is the target row, p its provider, s the provider's service.
func (x *DB) modelNamesUsing(ctx context.Context, filter string, arg string) ([]string, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT DISTINCT t.model_name
		   FROM ai_model_targets t
		   JOIN ai_providers p ON p.slug = t.provider_slug
		   JOIN services s ON s.id = p.service_id `+filter+`
		  ORDER BY t.model_name`, arg)
	if err != nil {
		return nil, fmt.Errorf("list model names by provider: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list model names by provider: scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListAIModelNamesByProvider returns the names of models with a target on the
// provider, sorted (never nil).
func (x *DB) ListAIModelNamesByProvider(ctx context.Context, slug string) ([]string, error) {
	return x.modelNamesUsing(ctx, `WHERE t.provider_slug=?`, slug)
}

// providerInUseOr explains a failed delete. ai_model_targets.provider_slug
// has no ON DELETE, so deleting a provider, its service or the service's
// owner fails on the foreign key while a model targets the provider. The
// models are read after the failure, not before the delete: a check made
// first could be overtaken by a model created in between. When the filter
// finds models, the result wraps ErrProviderInUse with their names; any other
// failure, and nil, come back as they are.
func (x *DB) providerInUseOr(ctx context.Context, deleteErr error, filter, arg string) error {
	if deleteErr == nil || errors.Is(deleteErr, ErrNotFound) {
		return deleteErr
	}
	names, err := x.modelNamesUsing(ctx, filter, arg)
	if err != nil || len(names) == 0 {
		return deleteErr
	}
	return fmt.Errorf("%w: %s", ErrProviderInUse, strings.Join(names, ", "))
}
