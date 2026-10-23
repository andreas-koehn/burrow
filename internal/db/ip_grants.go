package db

// ip_grants.go — just-in-time grants of tcp services in mode ip (table
// service_ip_grants, migration 0040): an address or range that may connect
// until expires_at, next to the permanent lists of service_ip_geo.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// IPGrant is one just-in-time grant. CreatedBy is "" when the user who made
// it was deleted.
type IPGrant struct {
	ID        string
	ServiceID string
	CIDR      string
	ExpiresAt time.Time
	Note      string
	CreatedBy string
	CreatedAt time.Time
}

const ipGrantCols = `id, service_id, cidr, expires_at, note, COALESCE(created_by,''), created_at`

func scanIPGrant(sc interface{ Scan(...any) error }) (IPGrant, error) {
	var g IPGrant
	if err := sc.Scan(&g.ID, &g.ServiceID, &g.CIDR, &g.ExpiresAt, &g.Note, &g.CreatedBy, &g.CreatedAt); err != nil {
		return IPGrant{}, err
	}
	g.ExpiresAt, g.CreatedAt = g.ExpiresAt.UTC(), g.CreatedAt.UTC()
	return g, nil
}

// CreateIPGrant inserts a grant. CreatedAt is now when it is zero; an empty
// CreatedBy is stored as NULL.
func (x *DB) CreateIPGrant(ctx context.Context, g IPGrant) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	var by sql.NullString
	if g.CreatedBy != "" {
		by = sql.NullString{String: g.CreatedBy, Valid: true}
	}
	_, err := x.sqlDB.ExecContext(ctx,
		`INSERT INTO service_ip_grants(id, service_id, cidr, expires_at, note, created_by, created_at) VALUES(?,?,?,?,?,?,?)`,
		g.ID, g.ServiceID, g.CIDR, g.ExpiresAt.UTC(), g.Note, by, g.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("create ip grant: %w", err)
	}
	return nil
}

// ListIPGrants returns the grants of the service that have not expired at
// now, the first to expire first.
func (x *DB) ListIPGrants(ctx context.Context, serviceID string, now time.Time) ([]IPGrant, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT `+ipGrantCols+` FROM service_ip_grants WHERE service_id=? AND expires_at > ? ORDER BY expires_at, id`,
		serviceID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list ip grants: %w", err)
	}
	defer rows.Close()
	out := []IPGrant{}
	for rows.Next() {
		g, err := scanIPGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("scan ip grant: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ip grants rows: %w", err)
	}
	return out, nil
}

// DeleteIPGrant removes the grant of the service and returns it; ErrNotFound
// when the service has no grant of that id.
func (x *DB) DeleteIPGrant(ctx context.Context, serviceID, id string) (IPGrant, error) {
	g, err := scanIPGrant(x.sqlDB.QueryRowContext(ctx,
		`SELECT `+ipGrantCols+` FROM service_ip_grants WHERE service_id=? AND id=?`, serviceID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return IPGrant{}, ErrNotFound
	}
	if err != nil {
		return IPGrant{}, fmt.Errorf("get ip grant: %w", err)
	}
	res, err := x.sqlDB.ExecContext(ctx, `DELETE FROM service_ip_grants WHERE service_id=? AND id=?`, serviceID, id)
	if err != nil {
		return IPGrant{}, fmt.Errorf("delete ip grant: %w", err)
	}
	if err := notFoundIfNoRows(res, "delete ip grant"); err != nil {
		return IPGrant{}, err
	}
	return g, nil
}

// PruneIPGrants deletes the grants of the service that expired at or before
// now, and returns how many.
func (x *DB) PruneIPGrants(ctx context.Context, serviceID string, now time.Time) (int64, error) {
	res, err := x.sqlDB.ExecContext(ctx,
		`DELETE FROM service_ip_grants WHERE service_id=? AND expires_at <= ?`, serviceID, now.UTC())
	if err != nil {
		return 0, fmt.Errorf("prune ip grants: %w", err)
	}
	return res.RowsAffected()
}

// SetServiceMaxConnections sets the cap on connected visitors of a tcp
// service; 0 clears it (the relay's default). ErrNotFound when no service has
// the id.
func (x *DB) SetServiceMaxConnections(ctx context.Context, id string, n int) error {
	var v sql.NullInt64
	if n > 0 {
		v = sql.NullInt64{Int64: int64(n), Valid: true}
	}
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE services SET max_connections=? WHERE id=?`, v, id)
	if err != nil {
		return fmt.Errorf("set service max connections: %w", err)
	}
	return notFoundIfNoRows(res, "set service max connections")
}
