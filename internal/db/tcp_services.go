package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrTCPPortReserved is returned by SetServiceTCPPort when another service
// holds the port.
var ErrTCPPortReserved = errors.New("db: tcp port is reserved by another service")

// SetServiceTCPPort records the public port the relay gave a tcp service, so
// that its next registration gets the same one; port 0 clears it. A port
// belongs to one service at a time: a port another service holds is refused
// with ErrTCPPortReserved and changes nothing. ErrNotFound when no service has
// the id.
func (x *DB) SetServiceTCPPort(ctx context.Context, id string, port int) error {
	var v sql.NullInt64
	if port != 0 {
		v = sql.NullInt64{Int64: int64(port), Valid: true}
	}
	res, err := x.sqlDB.ExecContext(ctx, `UPDATE services SET tcp_port=? WHERE id=?`, v, id)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrTCPPortReserved
		}
		return fmt.Errorf("set service tcp port: %w", err)
	}
	return notFoundIfNoRows(res, "set service tcp port")
}

// ListTCPPortReservations maps every port a service holds to that service's
// id.
func (x *DB) ListTCPPortReservations(ctx context.Context) (map[int]string, error) {
	rows, err := x.sqlDB.QueryContext(ctx, `SELECT tcp_port, id FROM services WHERE tcp_port IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list tcp port reservations: %w", err)
	}
	defer rows.Close()
	out := map[int]string{}
	for rows.Next() {
		var port int
		var id string
		if err := rows.Scan(&port, &id); err != nil {
			return nil, fmt.Errorf("scan tcp port reservation: %w", err)
		}
		out[port] = id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tcp port reservations rows: %w", err)
	}
	return out, nil
}
