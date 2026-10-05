package db

import (
	"context"
	"fmt"
)

// ListAIProviderModels returns the stored model catalog of a provider ordered
// by model id (never nil).
func (x *DB) ListAIProviderModels(ctx context.Context, slug string) ([]AIProviderModel, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT provider_slug, model_id, display_name, context_length, synced_at
		 FROM ai_provider_models WHERE provider_slug=? ORDER BY model_id`, slug)
	if err != nil {
		return nil, fmt.Errorf("list ai provider models: %w", err)
	}
	defer rows.Close()
	out := make([]AIProviderModel, 0)
	for rows.Next() {
		var m AIProviderModel
		if err := rows.Scan(&m.ProviderSlug, &m.ModelID, &m.DisplayName, &m.ContextLength, &m.SyncedAt); err != nil {
			return nil, fmt.Errorf("list ai provider models: scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceAIProviderModels swaps the whole catalog of a provider in one transaction.
func (x *DB) ReplaceAIProviderModels(ctx context.Context, slug string, models []AIProviderModel) error {
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin replace models tx: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ai_provider_models WHERE provider_slug=?`, slug); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("replace ai provider models: delete: %w", err)
	}
	for _, m := range models {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ai_provider_models(provider_slug, model_id, display_name, context_length) VALUES(?,?,?,?)`,
			slug, m.ModelID, m.DisplayName, m.ContextLength,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("replace ai provider models: insert %q: %w", m.ModelID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace models tx: %w", err)
	}
	return nil
}

// UpsertAIProviderModel inserts or updates one catalog entry.
func (x *DB) UpsertAIProviderModel(ctx context.Context, m AIProviderModel) error {
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO ai_provider_models(provider_slug, model_id, display_name, context_length) VALUES(?,?,?,?)
		 ON CONFLICT(provider_slug, model_id) DO UPDATE SET
		   display_name=excluded.display_name, context_length=excluded.context_length, synced_at=CURRENT_TIMESTAMP`,
		m.ProviderSlug, m.ModelID, m.DisplayName, m.ContextLength)
	if err != nil {
		return fmt.Errorf("upsert ai provider model: %w", err)
	}
	return nil
}

// DeleteAIProviderModel removes one catalog entry, or returns ErrNotFound.
func (x *DB) DeleteAIProviderModel(ctx context.Context, slug, modelID string) error {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM ai_provider_models WHERE provider_slug=? AND model_id=?`, slug, modelID)
	if err != nil {
		return fmt.Errorf("delete ai provider model: %w", err)
	}
	return notFoundIfNoRows(res, "delete ai provider model")
}
