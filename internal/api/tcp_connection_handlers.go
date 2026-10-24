package api

// tcp_connection_handlers.go — the live connections of a tcp service (native
// tcp, P04): the visitors of its public port and the forward streams of
// signed-in clients, with their bytes so far, and closing one of them.

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/server"
	"github.com/ankoehn/burrow/internal/store"
)

// ServiceConnections is the relay as the API reads and closes the live
// connections of tcp services. *server.Server satisfies it.
type ServiceConnections interface {
	ServiceConnections(serviceID string) []server.ConnView
	CloseServiceConnection(serviceID, connID string) bool
}

// ForwardRechecker is the relay as the API asks it to authorize the live
// forward streams again: of a service's other users, or of one user. Each
// ends the forwards the authorizer refuses now. *server.Server satisfies it.
type ForwardRechecker interface {
	RecheckForwards(ctx context.Context, serviceID string) int
	RecheckUserForwards(ctx context.Context, userID string) int
}

// recheckServiceForwards ends the live forwards to the service that its
// policy no longer allows.
func (d Deps) recheckServiceForwards(ctx context.Context, serviceID string) {
	if d.Forwards == nil {
		return
	}
	if n := d.Forwards.RecheckForwards(context.WithoutCancel(ctx), serviceID); n > 0 {
		d.Log.Info("forwards ended: no longer allowed", "service_id", serviceID, "forwards", n)
	}
}

// recheckUserForwards ends the live forwards of the user that their role or
// status no longer allows.
func (d Deps) recheckUserForwards(ctx context.Context, userID string) {
	if d.Forwards == nil {
		return
	}
	if n := d.Forwards.RecheckUserForwards(context.WithoutCancel(ctx), userID); n > 0 {
		d.Log.Info("forwards ended: no longer allowed", "user_id", userID, "forwards", n)
	}
}

// serviceConnectionResp is one live connection of a tcp service. kind is
// "visitor" (of the public port) or "forward" (burrow forward); user_id,
// user_email, token_name and session_id name the one who forwards and are
// "" for a visitor. bytes_in is what reached the service, bytes_out what it
// sent back.
type serviceConnectionResp struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	SourceIP  string    `json:"source_ip"`
	UserID    string    `json:"user_id"`
	UserEmail string    `json:"user_email"`
	TokenName string    `json:"token_name"`
	SessionID string    `json:"session_id"`
	Since     time.Time `json:"since"`
	BytesIn   uint64    `json:"bytes_in"`
	BytesOut  uint64    `json:"bytes_out"`
	// Protocol and SNI are what its first bytes were so far (see the
	// connection log); "unknown" until they tell.
	Protocol string `json:"protocol"`
	SNI      string `json:"sni"`
}

// tcpServiceForConnections gates a connections route as the service routes
// are gated (services:configure, the owner for :own) and returns the
// service, which must be a tcp service. false: the answer is written.
func (d Deps) tcpServiceForConnections(w http.ResponseWriter, r *http.Request) (store.ServiceDetail, bool) {
	role, err := d.actingRole(r, authz.PermServicesConfigureOwn, authz.PermServicesConfigureAny)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return store.ServiceDetail{}, false
	}
	det, err := d.Services.GetService(r.Context(), userID(r.Context()), role, chi.URLParam(r, "serviceID"))
	if err != nil {
		if !mapServiceErr(w, err, "service not found") {
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return store.ServiceDetail{}, false
	}
	if det.Type != "tcp" {
		writeErr(w, http.StatusConflict, "live connections are those of a tcp service")
		return store.ServiceDetail{}, false
	}
	return det, true
}

// ListServiceConnections handles GET /api/v1/services/{serviceID}/connections:
// the connections the relay carries for the tcp service now, the oldest
// first; [] when no client serves it.
func (d Deps) ListServiceConnections(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.tcpServiceForConnections(w, r)
	if !ok {
		return
	}
	var conns []server.ConnView
	if d.Connections != nil {
		conns = d.Connections.ServiceConnections(svc.ID)
	}
	slices.SortFunc(conns, func(a, b server.ConnView) int { return a.Since.Compare(b.Since) })
	emails := map[string]string{}
	out := make([]serviceConnectionResp, 0, len(conns))
	for _, c := range conns {
		email, known := emails[c.UserID]
		if !known && c.UserID != "" && d.Users != nil {
			if u, err := d.Users.GetUserByID(r.Context(), c.UserID); err == nil {
				email = u.Email
			}
			emails[c.UserID] = email
		}
		out = append(out, serviceConnectionResp{
			ID: c.ID, Kind: c.Kind, SourceIP: c.SourceIP, UserID: c.UserID, UserEmail: email,
			TokenName: c.TokenName, SessionID: c.SessionID, Since: c.Since,
			BytesIn: c.BytesIn, BytesOut: c.BytesOut, Protocol: c.Protocol, SNI: c.SNI,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteServiceConnection handles DELETE
// /api/v1/services/{serviceID}/connections/{connID}: the connection is closed
// at once (a visitor's socket, or a forward stream; the client that forwards
// stays signed in). It is audited. 404 when the service has no live
// connection of that id.
func (d Deps) DeleteServiceConnection(w http.ResponseWriter, r *http.Request) {
	svc, ok := d.tcpServiceForConnections(w, r)
	if !ok {
		return
	}
	connID := chi.URLParam(r, "connID")
	var conn *server.ConnView
	if d.Connections != nil {
		for _, c := range d.Connections.ServiceConnections(svc.ID) {
			if c.ID == connID {
				conn = &c
				break
			}
		}
	}
	if conn == nil || !d.Connections.CloseServiceConnection(svc.ID, connID) {
		writeErr(w, http.StatusNotFound, "connection not found")
		return
	}
	if d.AuditAppender != nil {
		lc := audit.LogContextFrom(r.Context())
		if err := d.AuditAppender.Append(r.Context(), audit.Event{
			ActorID: lc.ActorID, ActorEmail: lc.ActorEmail, Action: audit.ActionServiceConnectionClose,
			SubjectID: svc.ID, SubjectLabel: svc.Name, Result: "ok",
			SourceIP: lc.SourceIP, UserAgent: lc.UserAgent, RequestID: lc.RequestID,
			Payload: audit.MustJSON(map[string]any{
				"connection_id": conn.ID, "kind": conn.Kind, "source_ip": conn.SourceIP,
				"user_id": conn.UserID, "token_name": conn.TokenName, "since": conn.Since,
				"bytes_in": conn.BytesIn, "bytes_out": conn.BytesOut,
			}),
		}); err != nil {
			d.warn("audit write failed", "action", audit.ActionServiceConnectionClose, "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
