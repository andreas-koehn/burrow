// Package store composes the db layer + auth into the interfaces the server
// and (later) the HTTP API depend on.
package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/mailer"
)

// ErrUnauthorized is returned when a client token does not match.
var ErrUnauthorized = errors.New("store: unauthorized")

// ErrInvalidCredentials is returned by ChangePassword when the current password is wrong.
var ErrInvalidCredentials = errors.New("store: invalid credentials")

// ErrPasswordTooShort is returned when a new password is shorter than minPasswordLen.
var ErrPasswordTooShort = errors.New("store: password too short (minimum 8 characters)")

// ErrEmailConflict is returned by CreateUser when the email already exists.
var ErrEmailConflict = errors.New("store: email already in use")

// ErrInvalidRole is returned by CreateUser when the role is not 'admin' or 'user'.
var ErrInvalidRole = errors.New("store: role must be 'admin' or 'user'")

// ErrInvalidStatus is returned by SetUserStatus for a status other than
// 'active' or 'suspended'.
var ErrInvalidStatus = errors.New("store: status must be 'active' or 'suspended'")

// ErrInvalidAccessMode is returned by SetTunnelAccessMode for a value other
// than 'open', 'api_key', 'burrow_login', or 'mtls'.
var ErrInvalidAccessMode = errors.New("store: access_mode must be 'open', 'api_key', 'burrow_login', or 'mtls'")

// ErrMTLSCARequired is returned by SetServiceAccessMode when the caller asks
// for access_mode='mtls' but does not supply a non-empty CA PEM. Maps to 400.
var ErrMTLSCARequired = errors.New("store: mtls access mode requires a non-empty mtls_ca_pem")

// ErrInvalidMTLSCAPEM is returned when the supplied mtls_ca_pem does not
// contain at least one parseable CERTIFICATE block. Maps to 400.
var ErrInvalidMTLSCAPEM = errors.New("store: invalid CA PEM")

// ErrSMTPUnconfigured is returned by SendTestEmail when no smtp.host is set.
var ErrSMTPUnconfigured = errors.New("store: smtp is not configured")

// ErrForbidden is returned when the caller lacks permission for the requested
// operation (maps to HTTP 403 in the API layer).
var ErrForbidden = errors.New("store: forbidden")

// ErrServiceNotHTTP is returned by SetServiceAccessMode when mode is not
// "open" and the service type is not "http" (maps to HTTP 409).
var ErrServiceNotHTTP = errors.New("store: api_key and burrow_login require an http service")

// ErrNameRequired is returned by CreateAPIKey when the name argument is empty
// (maps to HTTP 400 in the API layer).
var ErrNameRequired = errors.New("store: name is required")

// ErrUnknownRole is returned by SetAccessPolicy when a role is not a built-in
// authz role (maps to HTTP 400 in the API layer).
var ErrUnknownRole = errors.New("store: unknown role")

// minPasswordLen is the minimum required password length.
const minPasswordLen = 8

// sessionTTL is the lifetime of a browser session.
const sessionTTL = 7 * 24 * time.Hour

// SaveTunnelArg is the subset of a tunnel the store persists.
// The server adapter in cmd/server converts *server.Tunnel to this type,
// keeping store free of any import of internal/server.
type SaveTunnelArg struct {
	ID, Name, Type, LocalAddr string
	RemotePort                int
}

// AuditAppender is the narrow audit-append surface the store may use to
// chain mutation calls onto the audit hash chain. *audit.Logger satisfies
// it. Defined here (rather than imported from internal/audit) so the
// store package does not pull in internal/audit just to hold an optional
// callback (and so the cmd/server wiring layer can pass nil before the
// logger has been built without forcing an import cycle).
type AuditAppender interface {
	// The argument type is deliberately `any` so the store package can
	// stay free of any audit-package import. Callers in cmd/server pass
	// audit.Event values; future store-side audit emissions construct the
	// same shape via reflection-free type assertions at the audit layer.
	// v0.4.0 ships the field unused on the store hot path (no store
	// methods append yet); the setter exists so Task 25 wiring is complete.
	Append(ctx context.Context, e any) error
}

// Store is the DB-backed implementation of the server/API dependencies.
// The caller retains ownership of the underlying *sql.DB (Store has no Close).
type Store struct {
	q            *db.DB
	smtpPassword string // injected from BURROW_SMTP_PASSWORD(_FILE); never persisted

	// auditLogger is the optional chain logger the store would consult on
	// future audit-emitting mutations (Task 25 wires it via SetAuditLogger).
	// v0.4.0 does not emit any audit events from the store package itself
	// — the api/handlers own that responsibility — but the field is kept
	// so future store-side mutations can append without an additional
	// constructor change. Nil-safe (no caller calls .Append on it today).
	auditLogger AuditAppender

	// clientLoginNow is the clock of the client sign-in requests (their
	// expiry and poll interval). Nil means time.Now; tests inject one with
	// SetClientLoginClock.
	clientLoginNow func() time.Time
}

// New builds a Store over an open, migrated *sql.DB.
func New(d *sql.DB) *Store { return &Store{q: db.Wrap(d)} }

// SetSMTPPassword injects the SMTP secret (from config). Called by cmd/server.
func (s *Store) SetSMTPPassword(pw string) { s.smtpPassword = pw }

// SetAuditLogger installs the optional audit appender used by future
// store-side audit emissions. Idempotent + concurrency-safe to call once
// at startup; not safe to mutate after the store has been handed out to
// goroutines (Task 25 wires it during cmd/server's serve bootstrap,
// before any HTTP handler runs).
func (s *Store) SetAuditLogger(a AuditAppender) { s.auditLogger = a }

// emitAudit is the nil-safe, panic-safe trampoline every store mutation
// uses to append a typed audit row. Mutations call it AFTER their DB write
// succeeds: a failing mutation never produces a misleading audit row, and a
// failing audit append never breaks the mutation (the audit chain itself is
// best-effort on writes; auditors detect gaps offline). actor metadata is
// pulled from ctx (set by api middleware via audit.WithLogContext).
//
// The defer/recover catches any panic from a misbehaving logger so that the
// mutation's RunE / handler-level error path stays untouched. The audit
// append return value is intentionally discarded — there is no caller that
// could meaningfully react to it from inside a mutation.
func (s *Store) emitAudit(ctx context.Context, action string, fill func(*audit.Event)) {
	if s == nil || s.auditLogger == nil {
		return
	}
	defer func() {
		// Defensive: never let a logger bug propagate into the mutation
		// caller. Spec G's audit chain is best-effort on writes.
		_ = recover()
	}()
	lc := audit.LogContextFrom(ctx)
	ev := audit.Event{
		ActorID:    lc.ActorID,
		ActorEmail: lc.ActorEmail,
		Action:     action,
		Result:     "ok",
		SourceIP:   lc.SourceIP,
		UserAgent:  lc.UserAgent,
		RequestID:  lc.RequestID,
	}
	if fill != nil {
		fill(&ev)
	}
	_ = s.auditLogger.Append(ctx, ev)
}

// SeedAdmin ensures the bootstrap admin user exists.
// It is a no-op when email or password is empty (unset BURROW_ADMIN_* — safe).
// Uses INSERT ... ON CONFLICT(email) DO NOTHING so the operation is
// idempotent and race-proof: the UNIQUE(email) constraint in the schema
// guarantees atomicity — two concurrent callers both succeed without
// duplication (one inserts, the other gets 0 rows affected and returns nil).
// B17: eliminates the prior check-then-insert pattern.
func (s *Store) SeedAdmin(ctx context.Context, email, password string) error {
	if email == "" || password == "" {
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.q.DB().ExecContext(ctx,
		`INSERT INTO users(id, email, password_hash, role)
		 VALUES(?,?,?,'admin')
		 ON CONFLICT(email) DO NOTHING`,
		uuid.NewString(), email, hash,
	)
	return err
}

// VerifyUserPassword checks whether the given password matches the stored hash
// for the user identified by email. Returns (false, nil) for unknown emails
// so that login of an unknown email is a normal "wrong creds", not a 500.
func (s *Store) VerifyUserPassword(ctx context.Context, email, password string) (bool, error) {
	u, err := s.q.GetUserByEmail(ctx, email)
	if errors.Is(err, db.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return auth.VerifyPassword(password, u.PasswordHash)
}

// GetUserByEmail returns the user with the given email, or db.ErrNotFound.
func (s *Store) GetUserByEmail(ctx context.Context, email string) (db.User, error) {
	return s.q.GetUserByEmail(ctx, email)
}

// GetUserByID returns the user with the given ID, or db.ErrNotFound.
func (s *Store) GetUserByID(ctx context.Context, id string) (db.User, error) {
	return s.q.GetUserByID(ctx, id)
}

// IssueClientToken generates a new client token for the given user and name,
// persists the hash, and returns the plaintext (shown once).
func (s *Store) IssueClientToken(ctx context.Context, userID, name string) (plaintext string, err error) {
	pt, ct, err := newClientToken(userID, name)
	if err != nil {
		return "", err
	}
	if err := s.q.CreateClientToken(ctx, ct); err != nil {
		return "", err
	}
	s.emitAudit(ctx, audit.ActionTokenMint, func(e *audit.Event) {
		e.SubjectID = ct.ID
		e.SubjectLabel = name
	})
	return pt, nil
}

// newClientToken generates a client token for the user: the plaintext (shown
// once) and the row to persist, which holds its hash only. Every client token
// is made here, whether POST /tokens or an approved sign-in request asks.
func newClientToken(userID, name string) (plaintext string, ct db.ClientToken, err error) {
	pt, hash, err := auth.GenerateClientToken()
	if err != nil {
		return "", db.ClientToken{}, err
	}
	return pt, db.ClientToken{
		ID:        uuid.NewString(),
		UserID:    userID,
		Name:      name,
		TokenHash: hash,
	}, nil
}

// ListClientTokens returns all client tokens belonging to the given user.
func (s *Store) ListClientTokens(ctx context.Context, userID string) ([]db.ClientToken, error) {
	return s.q.ListClientTokensByUser(ctx, userID)
}

// RevokeClientToken deletes the token with the given id, scoped to owning user.
func (s *Store) RevokeClientToken(ctx context.Context, id, userID string) error {
	if err := s.q.DeleteClientToken(ctx, id, userID); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionTokenRevoke, func(e *audit.Event) {
		e.SubjectID = id
	})
	return nil
}

// Authenticate validates a plaintext client token and returns the owning user's ID.
// Returns ErrUnauthorized for unknown or invalid tokens.
// The last-used timestamp update is best-effort and does not fail authentication.
func (s *Store) Authenticate(ctx context.Context, plaintext string) (userID string, err error) {
	hash := auth.HashToken(plaintext)
	ct, err := s.q.GetClientTokenByHash(ctx, hash)
	if errors.Is(err, db.ErrNotFound) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	// Best-effort: update last_used without failing auth on error.
	_ = s.q.TouchClientTokenLastUsed(ctx, ct.ID)
	return ct.UserID, nil
}

// AuthenticateNamed is like Authenticate but also returns the client-token's
// name (used by the server to label clients in the overview). Returns
// ErrUnauthorized for unknown/invalid tokens.
func (s *Store) AuthenticateNamed(ctx context.Context, plaintext string) (userID, tokenName string, err error) {
	hash := auth.HashToken(plaintext)
	ct, err := s.q.GetClientTokenByHash(ctx, hash)
	if errors.Is(err, db.ErrNotFound) {
		return "", "", ErrUnauthorized
	}
	if err != nil {
		return "", "", err
	}
	_ = s.q.TouchClientTokenLastUsed(ctx, ct.ID)
	return ct.UserID, ct.Name, nil
}

// SaveTunnel upserts a tunnel row belonging to the given user.
func (s *Store) SaveTunnel(ctx context.Context, userID string, t *SaveTunnelArg) error {
	return s.q.UpsertTunnel(ctx, db.Tunnel{
		ID:         t.ID,
		UserID:     userID,
		Name:       t.Name,
		Type:       t.Type,
		RemotePort: t.RemotePort,
		LocalAddr:  t.LocalAddr,
	})
}

// MarkTunnelSeen updates the last_seen timestamp for the given tunnel ID.
func (s *Store) MarkTunnelSeen(ctx context.Context, tunnelID string) error {
	return s.q.TouchTunnelLastSeen(ctx, tunnelID)
}

// CreateSession creates a new browser session for the given user, returning
// the session ID. The session expires after 7 days.
func (s *Store) CreateSession(ctx context.Context, userID, ua, ip string) (id string, err error) {
	id = uuid.NewString()
	if err := s.q.CreateSession(ctx, db.Session{
		ID:        id,
		UserID:    userID,
		ExpiresAt: time.Now().UTC().Add(sessionTTL),
		UserAgent: ua,
		IP:        ip,
	}); err != nil {
		return "", err
	}
	// session.create is unusual: the actor *is* the new session's user (a
	// just-completed login). The api/Login handler has not yet stuffed an
	// audit.LogContext, so we fill ActorID/SourceIP/UserAgent from the
	// arguments we already have. SubjectID = sessionID (the new session).
	s.emitAudit(ctx, audit.ActionSessionCreate, func(e *audit.Event) {
		e.SubjectID = id
		if e.ActorID == "" {
			e.ActorID = userID
		}
		if e.SourceIP == "" {
			e.SourceIP = ip
		}
		if e.UserAgent == "" {
			e.UserAgent = ua
		}
	})
	return id, nil
}

// ValidateSession looks up a session by ID and returns the owning user ID.
// Returns ErrUnauthorized if the session does not exist or has expired.
// Expired sessions are deleted best-effort before returning.
func (s *Store) ValidateSession(ctx context.Context, id string) (userID string, err error) {
	sess, err := s.q.GetSession(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return "", ErrUnauthorized
	}
	if err != nil {
		return "", err
	}
	if time.Now().UTC().After(sess.ExpiresAt) {
		_ = s.q.DeleteSession(ctx, id)
		return "", ErrUnauthorized
	}
	return sess.UserID, nil
}

// DeleteSession removes the session with the given ID.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	if err := s.q.DeleteSession(ctx, id); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionSessionDelete, func(e *audit.Event) {
		e.SubjectID = id
	})
	return nil
}

// ChangePassword verifies currentPassword, enforces minimum length on newPassword,
// re-hashes, and persists. Returns ErrInvalidCredentials on wrong current password,
// ErrPasswordTooShort on a short new password. Sessions are NOT rotated (MVP decision).
func (s *Store) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	u, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return err
	}
	ok, err := auth.VerifyPassword(currentPassword, u.PasswordHash)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if len(newPassword) < minPasswordLen {
		return ErrPasswordTooShort
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.q.UpdateUserPassword(ctx, userID, hash)
}

// UpdateUserPassword is a thin wrapper that updates the password hash directly.
// Callers that need to enforce business rules (current password check, min-length)
// should use ChangePassword instead.
func (s *Store) UpdateUserPassword(ctx context.Context, userID, newHash string) error {
	return s.q.UpdateUserPassword(ctx, userID, newHash)
}

// ListUsersPage returns a filtered, paginated page plus the full filtered total.
func (s *Store) ListUsersPage(ctx context.Context, q string, limit, offset int) ([]db.User, int, error) {
	return s.q.ListUsersPage(ctx, q, limit, offset)
}

// UpdateUserRole validates and sets a user's role.
func (s *Store) UpdateUserRole(ctx context.Context, id, role string) error {
	if role != "admin" && role != "user" {
		return ErrInvalidRole
	}
	if err := s.q.UpdateUserRole(ctx, id, role); err != nil {
		return err
	}
	var email string
	if u, err := s.q.GetUserByID(ctx, id); err == nil {
		email = u.Email
	}
	s.emitAudit(ctx, audit.ActionUserUpdate, func(e *audit.Event) {
		e.SubjectID = id
		e.SubjectLabel = email
		e.Payload = audit.MustJSON(map[string]string{"role": role})
	})
	return nil
}

// SetUserStatus validates and sets a user's status. Suspending also revokes
// every existing session for that user (so suspension takes effect immediately
// without adding a per-request user lookup to RequireSession).
func (s *Store) SetUserStatus(ctx context.Context, id, status string) error {
	if status != "active" && status != "suspended" {
		return ErrInvalidStatus
	}
	if err := s.q.UpdateUserStatus(ctx, id, status); err != nil {
		return err
	}
	if status == "suspended" {
		if _, err := s.q.DeleteSessionsByUser(ctx, id); err != nil {
			return err
		}
	}
	var email string
	if u, err := s.q.GetUserByID(ctx, id); err == nil {
		email = u.Email
	}
	action := audit.ActionUserReactivate
	if status == "suspended" {
		action = audit.ActionUserSuspend
	}
	s.emitAudit(ctx, action, func(e *audit.Event) {
		e.SubjectID = id
		e.SubjectLabel = email
	})
	return nil
}

// TouchUserLastLogin stamps the user's last_login (best-effort at the call site).
func (s *Store) TouchUserLastLogin(ctx context.Context, id string) error {
	return s.q.TouchUserLastLogin(ctx, id)
}

// RoleDetail is a role row enriched with its permission set. For builtin
// roles (admin/user) the Permissions slice comes from the code-defined
// authz table; for non-builtin (v0.4.0) roles it comes from the DB row's
// permissions JSON column.
type RoleDetail struct {
	Name               string
	Description        string
	CreatedAt          time.Time
	Permissions        []string
	Builtin            bool
	DefaultForNewUsers bool
}

// ErrRoleBuiltin is returned by UpdateRole / DeleteRole when the caller
// targets a built-in role (admin/user). Mapped to HTTP 409 by the API.
var ErrRoleBuiltin = errors.New("store: role is built-in")

// ErrUnknownPermission is returned by CreateRole / UpdateRole when the
// permissions slice contains a key that is not in authz.AllPermissions().
// The handler surfaces the offending key in the 400 response body.
type ErrUnknownPermission struct{ Key string }

// Error implements error.
func (e ErrUnknownPermission) Error() string {
	return "store: unknown permission " + strconv.Quote(e.Key)
}

// ErrRoleExists is returned by CreateRole on a duplicate name.
var ErrRoleExists = errors.New("store: role already exists")

// ListRoles returns every roles row, builtin first then custom (alphabetical
// within each group — the underlying db.ListRoles orders by name, so the
// "admin" row will naturally precede "user" and any custom role whose name
// starts after "u" sorts last). The shape preserves the v0.4.0 columns.
func (s *Store) ListRoles(ctx context.Context) ([]db.Role, error) {
	return s.q.ListRoles(ctx)
}

// GetRole returns a role's DB row plus its effective permission set. For
// builtin roles the permission list is the hard-coded authz catalog (the DB
// row's permissions JSON is intentionally left empty for admin/user by
// migration 0005); for custom roles the list is the DB row itself.
func (s *Store) GetRole(ctx context.Context, name string) (RoleDetail, error) {
	r, err := s.q.GetRole(ctx, name)
	if err != nil {
		return RoleDetail{}, err
	}
	d := RoleDetail{
		Name:               r.Name,
		Description:        r.Description,
		CreatedAt:          r.CreatedAt,
		Builtin:            r.Builtin,
		DefaultForNewUsers: r.DefaultForNewUsers,
	}
	if r.Builtin {
		if ar, ok := authz.Get(name); ok {
			for _, p := range ar.Permissions {
				d.Permissions = append(d.Permissions, string(p))
			}
		}
	} else {
		d.Permissions = append(d.Permissions, r.Permissions...)
	}
	return d, nil
}

// CreateRole inserts a new non-builtin role. The permissions slice is
// validated against authz.AllPermissions(); the first unknown key returns
// ErrUnknownPermission so the handler can surface "unknown permission <key>".
// If defaultForNewUsers is true the prior default is cleared in the same DB
// transaction (single-default invariant). After a successful insert the
// process-wide authz custom-roles cache is refreshed so the new role's
// permissions become visible to every Can() lookup.
func (s *Store) CreateRole(ctx context.Context, name, description string, permissions []string, defaultForNewUsers bool) error {
	if err := validateRolePermissions(permissions); err != nil {
		return err
	}
	if err := s.q.CreateRole(ctx, name, description, permissions, defaultForNewUsers); err != nil {
		if errors.Is(err, db.ErrRoleExists) {
			return ErrRoleExists
		}
		return err
	}
	s.emitAudit(ctx, audit.ActionRoleCreate, func(e *audit.Event) {
		e.SubjectLabel = name
		e.Payload = audit.MustJSON(map[string]any{
			"permissions":           permissions,
			"default_for_new_users": defaultForNewUsers,
		})
	})
	return s.refreshRolesCache(ctx)
}

// RoleUpdate is the optional-field patch shape; see db.RoleUpdate.
type RoleUpdate = db.RoleUpdate

// UpdateRole patches a non-builtin role. Returns ErrRoleBuiltin (→409) when
// the target row is admin/user, ErrUnknownPermission on a stray perm key,
// or db.ErrNotFound when no row matches. Refreshes the authz custom-roles
// cache on success so subsequent Can() lookups see the new permission set.
func (s *Store) UpdateRole(ctx context.Context, name string, u RoleUpdate) error {
	if u.Permissions != nil {
		if err := validateRolePermissions(*u.Permissions); err != nil {
			return err
		}
	}
	if err := s.q.UpdateRole(ctx, name, u); err != nil {
		if errors.Is(err, db.ErrRoleBuiltin) {
			return ErrRoleBuiltin
		}
		return err
	}
	s.emitAudit(ctx, audit.ActionRoleUpdate, func(e *audit.Event) {
		e.SubjectLabel = name
		payload := map[string]any{}
		if u.Permissions != nil {
			payload["permissions"] = *u.Permissions
		}
		if u.Description != nil {
			payload["description"] = *u.Description
		}
		if u.DefaultForNewUsers != nil {
			payload["default_for_new_users"] = *u.DefaultForNewUsers
		}
		e.Payload = audit.MustJSON(payload)
	})
	return s.refreshRolesCache(ctx)
}

// DeleteRole removes a non-builtin role in one transaction. Users on the
// deleted role are re-assigned to the current default-for-new-users role.
// The list of affected user IDs is returned so the caller (handler) can
// log audit events once the v0.4.0 Task 25 wiring lands; in this stage the
// handler simply discards the list (TODO Task 25). Returns ErrRoleBuiltin
// or db.ErrNotFound as appropriate. Refreshes the authz cache on success.
func (s *Store) DeleteRole(ctx context.Context, name string) (affectedUserIDs []string, err error) {
	fallback, err := s.q.DefaultRoleName(ctx)
	if err != nil {
		// No default role configured — fall back to the builtin "user" so
		// the cascade is guaranteed to land on a valid row. (Migration 0005
		// leaves both builtins with default_for_new_users=0; the API forces
		// callers to nominate a default via UpdateRole / CreateRole. Until
		// then we keep the safe default rather than failing the DELETE.)
		if errors.Is(err, db.ErrNotFound) {
			fallback = "user"
		} else {
			return nil, err
		}
	}
	// Defensive guard: refuse to fall back to the role we're about to drop
	// (would cascade users onto a deleted row). This can only happen if the
	// caller is dropping the current default role; we route them to "user"
	// in that case. Task 25 will add an explicit "set a new default before
	// deleting the current one" handler error if we want stricter behavior.
	if fallback == name {
		fallback = "user"
	}
	affected, err := s.q.DeleteRole(ctx, name, fallback)
	if err != nil {
		if errors.Is(err, db.ErrRoleBuiltin) {
			return nil, ErrRoleBuiltin
		}
		return nil, err
	}
	s.emitAudit(ctx, audit.ActionRoleDelete, func(e *audit.Event) {
		e.SubjectLabel = name
		e.Payload = audit.MustJSON(map[string]any{
			"fallback":       fallback,
			"affected_users": len(affected),
		})
	})
	if err := s.refreshRolesCache(ctx); err != nil {
		return nil, err
	}
	return affected, nil
}

// RefreshRolesCache repopulates the process-wide authz custom-roles cache
// from the current DB state. Exported so cmd/server can call it once at
// startup (Task 25 wires it); after that every store-side mutation calls
// the unexported helper directly. Builtin rows are skipped — their
// permissions live in the hardcoded authz table.
func (s *Store) RefreshRolesCache(ctx context.Context) error {
	return s.refreshRolesCache(ctx)
}

func (s *Store) refreshRolesCache(ctx context.Context) error {
	rows, err := s.q.ListRoles(ctx)
	if err != nil {
		return err
	}
	custom := make(map[string][]authz.Permission, len(rows))
	for _, r := range rows {
		if r.Builtin {
			continue
		}
		perms := make([]authz.Permission, 0, len(r.Permissions))
		for _, k := range r.Permissions {
			perms = append(perms, authz.Permission(k))
		}
		custom[r.Name] = perms
	}
	authz.SetRoles(custom)
	return nil
}

// validateRolePermissions checks every supplied key against the closed
// authz.AllPermissions() catalog and returns ErrUnknownPermission for the
// first stray. The check is O(n*m) but n,m are tiny so the linear scan
// keeps the dependency surface minimal (no need for an in-memory set).
func validateRolePermissions(perms []string) error {
	if len(perms) == 0 {
		return nil
	}
	allowed := authz.AllPermissions()
	for _, k := range perms {
		ok := false
		for _, a := range allowed {
			if string(a) == k {
				ok = true
				break
			}
		}
		if !ok {
			return ErrUnknownPermission{Key: k}
		}
	}
	return nil
}

// ListSessions returns the user's sessions (newest first).
func (s *Store) ListSessions(ctx context.Context, userID string) ([]db.Session, error) {
	return s.q.ListSessionsByUser(ctx, userID)
}

// RevokeSession deletes one of the user's sessions (scoped).
func (s *Store) RevokeSession(ctx context.Context, id, userID string) error {
	if err := s.q.DeleteSessionForUser(ctx, id, userID); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionSessionDelete, func(e *audit.Event) {
		e.SubjectID = id
	})
	return nil
}

// RevokeOtherSessions signs the user out everywhere except keepID.
func (s *Store) RevokeOtherSessions(ctx context.Context, userID, keepID string) (int64, error) {
	n, err := s.q.DeleteSessionsByUserExcept(ctx, userID, keepID)
	if err != nil {
		return 0, err
	}
	s.emitAudit(ctx, audit.ActionSessionRevokeOthers, func(e *audit.Event) {
		e.SubjectID = userID
		e.Payload = audit.MustJSON(map[string]int64{"revoked": n})
	})
	return n, nil
}

// GetSettings returns the settings table as a flat map.
func (s *Store) GetSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.GetAllSettings(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m, nil
}

// SaveSettings upserts the given non-secret settings.
func (s *Store) SaveSettings(ctx context.Context, kv map[string]string) error {
	return s.q.SetSettings(ctx, kv)
}

// GetTunnel returns one persisted tunnel row, or db.ErrNotFound.
func (s *Store) GetTunnel(ctx context.Context, id string) (db.Tunnel, error) {
	return s.q.GetTunnel(ctx, id)
}

// SetTunnelAccessMode validates the enum and sets a tunnel's access mode
// scoped to its owner. api_key/burrow_login are accepted and persisted but
// have no runtime effect in v0.2.0 (inert until HTTP tunnels land in v0.3).
func (s *Store) SetTunnelAccessMode(ctx context.Context, id, userID, mode string) error {
	switch mode {
	case "open", "api_key", "burrow_login":
	default:
		return ErrInvalidAccessMode
	}
	return s.q.SetTunnelAccessMode(ctx, id, userID, mode)
}

// FlushTunnelTotals adds the given byte deltas to a tunnel's persisted counters.
func (s *Store) FlushTunnelTotals(ctx context.Context, id string, addIn, addOut int64) error {
	return s.q.FlushTunnelTotals(ctx, id, addIn, addOut)
}

// SMTPConfigFromSettings builds a mailer.Config from the settings table plus
// the injected secret. ok=false means SMTP is unconfigured (no host).
func (s *Store) SMTPConfigFromSettings(ctx context.Context) (cfg mailer.Config, ok bool, err error) {
	m, err := s.GetSettings(ctx)
	if err != nil {
		return mailer.Config{}, false, err
	}
	host := m["smtp.host"]
	if host == "" {
		return mailer.Config{}, false, nil
	}
	port := 0
	for _, r := range m["smtp.port"] {
		if r < '0' || r > '9' {
			port = 0
			break
		}
		port = port*10 + int(r-'0')
	}
	tlsMode := mailer.TLSMode(m["smtp.tls"])
	if tlsMode == "" {
		tlsMode = mailer.TLSSTARTTLS
	}
	return mailer.Config{
		Host:     host,
		Port:     port,
		Username: m["smtp.username"],
		Password: s.smtpPassword,
		From:     m["smtp.from"],
		TLS:      tlsMode,
	}, true, nil
}

// SendTestEmail builds the SMTP config from settings (+ injected secret) and
// sends a test message to `to`. Returns ErrSMTPUnconfigured when no host is set.
func (s *Store) SendTestEmail(ctx context.Context, to string) error {
	cfg, ok, err := s.SMTPConfigFromSettings(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return ErrSMTPUnconfigured
	}
	return mailer.SendTest(ctx, cfg, to)
}

// CreateUser creates a new user account. Returns ErrEmailConflict on duplicate email,
// ErrPasswordTooShort on a short password, ErrInvalidRole on an unknown role.
func (s *Store) CreateUser(ctx context.Context, email, password, role string) (db.User, error) {
	if role != "admin" && role != "user" {
		return db.User{}, ErrInvalidRole
	}
	if len(password) < minPasswordLen {
		return db.User{}, ErrPasswordTooShort
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return db.User{}, err
	}
	u := db.User{
		ID:           uuid.NewString(),
		Email:        email,
		PasswordHash: hash,
		Role:         role,
	}
	if err := s.q.CreateUser(ctx, u); err != nil {
		// modernc sqlite wraps the underlying error; a UNIQUE constraint violation
		// message contains "UNIQUE constraint failed".
		if isUniqueViolation(err) {
			return db.User{}, ErrEmailConflict
		}
		return db.User{}, err
	}
	s.emitAudit(ctx, audit.ActionUserCreate, func(e *audit.Event) {
		e.SubjectID = u.ID
		e.SubjectLabel = u.Email
		e.Payload = audit.MustJSON(map[string]string{"role": u.Role})
	})
	return u, nil
}

// DeleteUser removes the user and all associated sessions/tokens/tunnels (ON DELETE CASCADE).
// Returns db.ErrNotFound if no such user exists.
func (s *Store) DeleteUser(ctx context.Context, id string) error {
	// Capture the email BEFORE the cascade so the audit row is still useful.
	var email string
	if u, err := s.q.GetUserByID(ctx, id); err == nil {
		email = u.Email
	}
	if err := s.q.DeleteUser(ctx, id); err != nil {
		return err
	}
	s.emitAudit(ctx, audit.ActionUserDelete, func(e *audit.Event) {
		e.SubjectID = id
		e.SubjectLabel = email
	})
	return nil
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint violation.
// modernc/sqlite wraps driver errors as *sqlite.Error or plain fmt.Errorf strings;
// the canonical way to detect them is to inspect the error message string.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return containsStr(err.Error(), "UNIQUE constraint failed")
}

// containsStr is a small helper so we don't import strings in the store package.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && findStr(s, sub)
}

func findStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
