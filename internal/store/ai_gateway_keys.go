package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/authz"
	"github.com/ankoehn/burrow/internal/db"
)

// GatewayKey is what callers see of an AI gateway key. It never carries the
// key or its hash.
type GatewayKey struct {
	ID            string
	Name          string
	KeyPrefix     string // first 8 characters of the key, for display
	UserID        string
	AllowedModels []string // never nil; empty = every model
	SystemText    string   // put in front of the app's system content; empty = none
	AllowedTools  []string // never nil; "<source>__<tool>" and "<source>__*"; empty = no tools
	ToolsReadOnly bool     // only tools the operator classed read-only
	LastUsed      *time.Time
	CreatedAt     time.Time
	RevokedAt     *time.Time
}

var (
	// ErrInvalidKey is wrapped with the reason a gateway key was refused at
	// creation; the reason is safe to show to the caller.
	ErrInvalidKey = errors.New("store: invalid gateway key")
	// ErrKeyNotFound: no gateway key has that id, or it is not the caller's.
	ErrKeyNotFound = errors.New("store: gateway key not found")
)

const (
	gatewayKeyPrefix     = "bgw_"
	gatewayKeyLen        = len(gatewayKeyPrefix) + 43 // 32 bytes in unpadded base64url
	gatewayKeyShown      = 8                          // characters kept for display
	maxGatewayKeyName    = 120
	maxGatewayKeyAllowed = 64
	maxGatewayKeyTools   = 64
)

const msgSystemTextLong = "system_text is longer than 8192 bytes"

func invalidKey(reason string) error { return fmt.Errorf("%w: %s", ErrInvalidKey, reason) }

// newGatewayKey returns a fresh plaintext key: the prefix plus 32 random bytes.
func newGatewayKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return gatewayKeyPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func toGatewayKey(k db.AIGatewayKey) GatewayKey {
	return GatewayKey{
		ID: k.ID, Name: k.Name, KeyPrefix: k.KeyPrefix, UserID: k.UserID,
		AllowedModels: append([]string{}, k.AllowedModels...), SystemText: k.SystemText,
		AllowedTools: append([]string{}, k.AllowedTools...), ToolsReadOnly: k.ToolsReadOnly,
		LastUsed: k.LastUsed, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt,
	}
}

// checkAllowedModels validates a model allow-list and returns it without
// duplicates, never nil.
func checkAllowedModels(allowed []string) ([]string, error) {
	if len(allowed) > maxGatewayKeyAllowed {
		return nil, invalidKey("at most 64 allowed models")
	}
	entries := make([]string, 0, len(allowed))
	seen := make(map[string]bool, len(allowed))
	for _, e := range allowed {
		if !ValidAllowEntry(e) {
			return nil, invalidKey(`an allowed model must be a model name, "<provider>/<model>" or "<provider>/*"`)
		}
		if !seen[e] {
			seen[e] = true
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// checkAllowedTools validates a tool allow-list and returns it without
// duplicates, never nil.
func checkAllowedTools(allowed []string) ([]string, error) {
	if len(allowed) > maxGatewayKeyTools {
		return nil, invalidKey("at most 64 allowed tools")
	}
	entries := make([]string, 0, len(allowed))
	seen := make(map[string]bool, len(allowed))
	for _, e := range allowed {
		if !ValidToolEntry(e) {
			return nil, invalidKey(`an allowed tool must be "<source>__<tool>" or "<source>__*"`)
		}
		if !seen[e] {
			seen[e] = true
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// NewGatewayKey is what CreateGatewayKeyFrom makes a key of.
type NewGatewayKey struct {
	Name string
	// AllowedModels restricts the key to those models (see ValidAllowEntry);
	// empty means all.
	AllowedModels []string
	// AllowedTools lists the tools the key may call (see ValidToolEntry);
	// empty means none.
	AllowedTools  []string
	ToolsReadOnly bool
	// SystemText is put in front of the system content of the key's requests.
	SystemText string
}

// CreateGatewayKey makes a gateway key owned by userID that may call no tools.
// The plaintext is returned here and nowhere else: the database keeps its
// hash. allowed restricts the key to those models (see ValidAllowEntry); empty
// means all. systemText is put in front of the system content of the key's
// requests.
func (s *Store) CreateGatewayKey(ctx context.Context, userID, name string, allowed []string, systemText string) (GatewayKey, string, error) {
	return s.CreateGatewayKeyFrom(ctx, userID, NewGatewayKey{Name: name, AllowedModels: allowed, SystemText: systemText})
}

// CreateGatewayKeyFrom is CreateGatewayKey with the tool allow-list and the
// read-only switch.
func (s *Store) CreateGatewayKeyFrom(ctx context.Context, userID string, in NewGatewayKey) (GatewayKey, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > maxGatewayKeyName || hasControl(name) {
		return GatewayKey{}, "", invalidKey("name must be 1-120 characters without control characters")
	}
	if len([]byte(in.SystemText)) > MaxSystemTextBytes {
		return GatewayKey{}, "", invalidKey(msgSystemTextLong)
	}
	models, err := checkAllowedModels(in.AllowedModels)
	if err != nil {
		return GatewayKey{}, "", err
	}
	tools, err := checkAllowedTools(in.AllowedTools)
	if err != nil {
		return GatewayKey{}, "", err
	}
	plaintext, err := newGatewayKey()
	if err != nil {
		return GatewayKey{}, "", fmt.Errorf("generate gateway key: %w", err)
	}
	id := uuid.NewString()
	if err := s.q.CreateAIGatewayKey(ctx, db.AIGatewayKey{
		ID: id, Name: name, KeyHash: auth.HashToken(plaintext), KeyPrefix: plaintext[:gatewayKeyShown],
		UserID: userID, AllowedModels: models, SystemText: in.SystemText,
		AllowedTools: tools, ToolsReadOnly: in.ToolsReadOnly,
	}); err != nil {
		return GatewayKey{}, "", err
	}
	stored, err := s.q.GetAIGatewayKey(ctx, id)
	if err != nil {
		return GatewayKey{}, "", err
	}
	return toGatewayKey(stored), plaintext, nil
}

// ListGatewayKeys returns the keys the caller may see, newest first: every
// user's when all is set (the caller holds ai:read:any or ai:configure:any,
// which the handler decides), otherwise the caller's own. Never nil.
func (s *Store) ListGatewayKeys(ctx context.Context, callerID string, all bool) ([]GatewayKey, error) {
	owner := callerID
	if all {
		owner = "" // every user
	} else if owner == "" {
		return []GatewayKey{}, nil // "" would mean every user to the query
	}
	rows, err := s.q.ListAIGatewayKeys(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]GatewayKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, toGatewayKey(r))
	}
	return out, nil
}

// RevokeGatewayKey revokes the key at once. Its owner may, and so may a
// caller who manages every key (anyKey: ai:configure:any, decided by the handler);
// for anyone else the key looks like one that does not exist. Revoking a
// revoked key is not an error.
func (s *Store) RevokeGatewayKey(ctx context.Context, callerID string, anyKey bool, id string) error {
	k, err := s.q.GetAIGatewayKey(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return ErrKeyNotFound
	}
	if err != nil {
		return err
	}
	if !anyKey && (callerID == "" || k.UserID != callerID) {
		return ErrKeyNotFound
	}
	if err := s.q.RevokeAIGatewayKey(ctx, id); errors.Is(err, db.ErrNotFound) {
		return ErrKeyNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// GatewayKeyUpdate is a change to a key. A nil field leaves that field as it
// is; a non-nil allow-list replaces the list (an empty one clears it).
type GatewayKeyUpdate struct {
	Name          *string
	SystemText    *string
	AllowedModels *[]string
	AllowedTools  *[]string
	ToolsReadOnly *bool
}

// UpdateGatewayKey applies u to a key. Its owner may, and so may a caller who
// manages every key (anyKey, as for RevokeGatewayKey); for anyone else the key
// looks like one that does not exist. A revoked key can still be changed.
func (s *Store) UpdateGatewayKey(ctx context.Context, callerID string, anyKey bool, id string, u GatewayKeyUpdate) (GatewayKey, error) {
	k, err := s.q.GetAIGatewayKey(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return GatewayKey{}, ErrKeyNotFound
	}
	if err != nil {
		return GatewayKey{}, err
	}
	// The owner of a key never changes: the check above holds for the
	// update below.
	if !anyKey && (callerID == "" || k.UserID != callerID) {
		return GatewayKey{}, ErrKeyNotFound
	}
	// Only the fields the update sets are written, in one statement: an
	// update of other fields running at the same time is not undone.
	var p db.AIGatewayKeyPatch
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if name == "" || len(name) > maxGatewayKeyName || hasControl(name) {
			return GatewayKey{}, invalidKey("name must be 1-120 characters without control characters")
		}
		p.Name = &name
	}
	if u.SystemText != nil {
		if len([]byte(*u.SystemText)) > MaxSystemTextBytes {
			return GatewayKey{}, invalidKey(msgSystemTextLong)
		}
		p.SystemText = u.SystemText
	}
	if u.AllowedModels != nil {
		models, err := checkAllowedModels(*u.AllowedModels)
		if err != nil {
			return GatewayKey{}, err
		}
		p.AllowedModels = &models
	}
	if u.AllowedTools != nil {
		tools, err := checkAllowedTools(*u.AllowedTools)
		if err != nil {
			return GatewayKey{}, err
		}
		p.AllowedTools = &tools
	}
	p.ToolsReadOnly = u.ToolsReadOnly
	if err := s.q.UpdateAIGatewayKey(ctx, id, p); errors.Is(err, db.ErrNotFound) {
		return GatewayKey{}, ErrKeyNotFound
	} else if err != nil {
		return GatewayKey{}, err
	}
	k, err = s.q.GetAIGatewayKey(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return GatewayKey{}, ErrKeyNotFound
	}
	if err != nil {
		return GatewayKey{}, err
	}
	return toGatewayKey(k), nil
}

// RevokeGatewayKeyByID revokes the key at once, for the system itself: the
// cost engine calls it when the gateway_key budget budgetID with action
// disable_key is exceeded. There is no caller to check; it must not be
// reachable from a request.
//
// revoked says whether this call changed the key. A key that was revoked
// before, by a person or by another budget an instant earlier, is left as it
// is and nothing is recorded: the update is conditional on the key being
// active, so of several revokes at once exactly one changes it. That one is
// audited as ai_gateway_key.revoke with actor "system" and the budget that
// caused it; the event names the key by id and name only.
func (s *Store) RevokeGatewayKeyByID(ctx context.Context, id, budgetID string) (revoked bool, err error) {
	changed, err := s.q.RevokeAIGatewayKeyIfActive(ctx, id)
	if err != nil {
		return false, err
	}
	k, err := s.q.GetAIGatewayKey(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		return false, ErrKeyNotFound
	}
	if !changed {
		return false, err
	}
	// The key is revoked whether or not its row could be read again; only
	// its name for the audit event depends on that.
	s.emitAudit(ctx, audit.ActionAIGatewayKeyRevoke, func(e *audit.Event) {
		e.ActorID, e.ActorEmail = "system", ""
		e.SubjectID, e.SubjectLabel = id, k.Name
		e.Payload = audit.MustJSON(map[string]string{"reason": "budget_exceeded", "budget_id": budgetID})
	})
	return true, nil
}

// ValidateGatewayKey looks the presented key up by its hash. ok is false for
// a key that is malformed, unknown or revoked, and for one whose owner is
// gone, suspended, or no longer holds ai:configure:own or ai:configure:any:
// such a key looks like an unknown one. Nothing is cached: every call reads
// the key and its owner, so a revoked key or a suspended owner fails on the
// next request. The presented value never appears in an error.
func (s *Store) ValidateGatewayKey(ctx context.Context, presented string) (GatewayKey, bool, error) {
	if len(presented) != gatewayKeyLen || !strings.HasPrefix(presented, gatewayKeyPrefix) {
		return GatewayKey{}, false, nil
	}
	k, err := s.q.GetAIGatewayKeyByHash(ctx, auth.HashToken(presented))
	if errors.Is(err, db.ErrNotFound) {
		return GatewayKey{}, false, nil
	}
	if err != nil {
		return GatewayKey{}, false, err
	}
	if k.RevokedAt != nil {
		return GatewayKey{}, false, nil
	}
	owner, err := s.q.GetUserByID(ctx, k.UserID)
	if errors.Is(err, db.ErrNotFound) {
		return GatewayKey{}, false, nil
	}
	if err != nil {
		return GatewayKey{}, false, err
	}
	mayConfigure := authz.Can(owner.Role, authz.PermAIConfigureOwn) || authz.Can(owner.Role, authz.PermAIConfigureAny)
	if owner.Status == "suspended" || !mayConfigure {
		return GatewayKey{}, false, nil
	}
	_ = s.q.TouchAIGatewayKey(ctx, k.ID) // best effort; a failed touch does not fail the request
	return toGatewayKey(k), true, nil
}
