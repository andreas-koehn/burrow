package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TCPPortHold is a service's hold on a port.
type TCPPortHold struct {
	ServiceID, UserID string
	Pinned            bool
	LastSeen          *time.Time
}

// ErrTCPPortReserved is returned by SetServiceTCPPort when another service
// holds the port.
var ErrTCPPortReserved = errors.New("db: tcp port is reserved by another service")

// SetServiceTCPPort records the public port the relay gave a tcp service, so
// that its next registration gets the same one, and whether the client asked
// for it (pinned) or the relay chose it; port 0 clears both. A port belongs
// to one service at a time: a port another service holds is refused with
// ErrTCPPortReserved and changes nothing, unless that service is takeFrom and
// holds the port automatically (not pinned): then it loses the port in the
// same transaction. Which holds may be taken (an offline service's) is the
// caller's decision. ErrNotFound when no service has the id.
func (x *DB) SetServiceTCPPort(ctx context.Context, id string, port int, pinned bool, takeFrom string) error {
	var v sql.NullInt64
	if port != 0 {
		v = sql.NullInt64{Int64: int64(port), Valid: true}
	} else {
		pinned = false
	}
	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("set service tcp port: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if takeFrom != "" && takeFrom != id {
		res, err := tx.ExecContext(ctx,
			`UPDATE services SET tcp_port=NULL, tcp_port_pinned=? WHERE id=? AND tcp_port=? AND tcp_port_pinned=?`,
			0, takeFrom, port, 0)
		if err != nil {
			return fmt.Errorf("set service tcp port: take over: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("set service tcp port: take over rows: %w", err)
		} else if n == 0 {
			return ErrTCPPortReserved
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE services SET tcp_port=?, tcp_port_pinned=? WHERE id=?`, v, boolToInt(pinned), id)
	if err != nil {
		if isDuplicateServiceErr(err) {
			return ErrTCPPortReserved
		}
		return fmt.Errorf("set service tcp port: %w", err)
	}
	if err := notFoundIfNoRows(res, "set service tcp port"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		if isDuplicateServiceErr(err) {
			return ErrTCPPortReserved
		}
		return fmt.Errorf("set service tcp port: commit: %w", err)
	}
	return nil
}

// ListTCPPortReservations maps every port a service holds to the hold: the
// service, its owner, whether the hold is pinned, and when a client last
// served the service (nil: never).
func (x *DB) ListTCPPortReservations(ctx context.Context) (map[int]TCPPortHold, error) {
	rows, err := x.sqlDB.QueryContext(ctx,
		`SELECT tcp_port, id, user_id, tcp_port_pinned, last_seen FROM services WHERE tcp_port IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list tcp port reservations: %w", err)
	}
	defer rows.Close()
	out := map[int]TCPPortHold{}
	for rows.Next() {
		var port int
		var h TCPPortHold
		var seen sql.NullTime
		if err := rows.Scan(&port, &h.ServiceID, &h.UserID, (*intBool)(&h.Pinned), &seen); err != nil {
			return nil, fmt.Errorf("scan tcp port reservation: %w", err)
		}
		if seen.Valid {
			h.LastSeen = &seen.Time
		}
		out[port] = h
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tcp port reservations rows: %w", err)
	}
	return out, nil
}
