package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
	"github.com/ankoehn/burrow/internal/db"
)

// Browser-approved client sign-in, in the shape of the OAuth device
// authorization grant (RFC 8628).
//
// Two codes with different jobs. The device code is 32 random bytes that only
// the client holds; the relay keeps its hash and hands the token to whoever
// presents the code. The user code is short enough to type; it names the
// request for a signed-in dashboard user, who approves or denies it. The user
// code alone never yields a token.
//
// The token is minted when the approved request is collected, not when it is
// approved: the request row is deleted and the token row inserted in one
// transaction (db.CollectClientLogin), so the token exists once, is handed
// out once, and its plaintext is never stored.

const (
	// clientLoginTTL is how long a sign-in request lives.
	clientLoginTTL = 10 * time.Minute
	// clientLoginInterval is the shortest time between two polls of one
	// device code.
	clientLoginInterval = 2 * time.Second
	// clientLoginMaxPending caps the pending requests, so that the
	// unauthenticated start endpoint cannot fill the table.
	clientLoginMaxPending = 20
	// clientLoginMaxPendingPerIP caps the pending requests of one source: an
	// IPv4 address, or the /64 of an IPv6 address (db.ClientLoginSourceKey).
	// Without it one source starting a request every thirty seconds would
	// hold all twenty places and turn the sign-in off for everybody.
	clientLoginMaxPendingPerIP = 5

	// userCodeAlphabet has no I, L, O, 0 or 1: nothing that reads as
	// something else. 31^8 codes (about 2^39.6) against at most twenty live
	// requests and a rate-limited lookup.
	userCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	userCodeLen      = 8

	maxLoginHostname  = 253
	maxLoginMetaField = 64
	maxTokenNameLen   = 120
)

var (
	// ErrLoginNotFound: no such sign-in request, or it has expired, or it was
	// collected already. The three are not told apart.
	ErrLoginNotFound = errors.New("store: sign-in request not found or expired")
	// ErrLoginTooMany: the cap on pending requests is reached.
	ErrLoginTooMany = errors.New("store: too many pending sign-in requests")
	// ErrLoginPending: the request waits for a decision.
	ErrLoginPending = errors.New("store: sign-in request is still pending")
	// ErrLoginDenied: the request was denied.
	ErrLoginDenied = errors.New("store: sign-in request was denied")
	// ErrLoginSlowDown: the device code was polled less than the interval ago.
	ErrLoginSlowDown = errors.New("store: polling too fast")
	// ErrLoginConflict: the request was approved or denied already.
	ErrLoginConflict = errors.New("store: sign-in request was already decided")
	// ErrLoginTokenName: the token name is empty, too long or holds control
	// characters.
	ErrLoginTokenName = errors.New("store: token name must be 1 to 120 characters without control characters")
)

// LoginStart is what a client gets for starting a sign-in request. DeviceCode
// is shown here once and stored only as a hash.
type LoginStart struct {
	DeviceCode, UserCode string
	ExpiresIn, Interval  int // seconds
}

// LoginMeta is what a client says about itself when it starts a request, plus
// the address it called from. All of it except SourceIP is untrusted text.
type LoginMeta struct {
	Hostname, OS, Arch, ClientVersion, SourceIP string
	TokenName                                   string // the client's suggestion (--name); may be empty
}

// LoginRequestView is a sign-in request as a dashboard user sees it. It holds
// neither the device code nor a token.
type LoginRequestView struct {
	UserCode, Hostname, OS, Arch, ClientVersion, SourceIP, Status string
	SuggestedTokenName                                            string // the client's --name, else the hostname
	CreatedAt, ExpiresAt                                          time.Time
	Age                                                           time.Duration // by the relay's clock
}

// LoginResult is the outcome of a collected request: the token (the only
// place its plaintext ever is), its name and the owner's email.
type LoginResult struct{ Token, TokenName, Email string }

// SetClientLoginClock replaces the clock of the sign-in requests. For tests;
// call it before the store is shared.
func (s *Store) SetClientLoginClock(now func() time.Time) { s.clientLoginNow = now }

func (s *Store) loginNow() time.Time {
	if s.clientLoginNow != nil {
		return s.clientLoginNow().UTC()
	}
	return time.Now().UTC()
}

// StartClientLogin opens a sign-in request for a client and returns its two
// codes. ErrLoginTooMany when the cap on pending requests is reached.
func (s *Store) StartClientLogin(ctx context.Context, m LoginMeta) (LoginStart, error) {
	now := s.loginNow()
	// Drop what has expired, so the table stays as small as the cap says.
	// The cap itself counts live rows only and does not depend on this.
	_, _ = s.q.DeleteExpiredClientLogins(ctx, now)

	row := db.ClientLoginRequest{
		Hostname:      cleanLoginText(m.Hostname, maxLoginHostname),
		OS:            cleanLoginText(m.OS, maxLoginMetaField),
		Arch:          cleanLoginText(m.Arch, maxLoginMetaField),
		ClientVersion: cleanLoginText(m.ClientVersion, maxLoginMetaField),
		SourceIP:      cleanLoginText(m.SourceIP, maxLoginMetaField),
		TokenName:     cleanLoginText(m.TokenName, maxTokenNameLen),
		CreatedAt:     now,
		ExpiresAt:     now.Add(clientLoginTTL),
	}
	// A user code that is taken is drawn again; with 31^8 codes and twenty
	// live requests that does not happen in practice.
	for attempt := 0; attempt < 5; attempt++ {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return LoginStart{}, err
		}
		deviceCode := base64.RawURLEncoding.EncodeToString(raw)
		userCode, err := newUserCode()
		if err != nil {
			return LoginStart{}, err
		}
		row.DeviceCodeHash = auth.HashToken(deviceCode)
		row.UserCode = userCode
		inserted, err := s.q.InsertClientLogin(ctx, row, clientLoginMaxPending, clientLoginMaxPendingPerIP)
		if db.IsClientLoginDuplicate(err) {
			continue
		}
		if err != nil {
			return LoginStart{}, err
		}
		if !inserted {
			return LoginStart{}, ErrLoginTooMany
		}
		// Not audited: the caller is anonymous, and the audit log is a hash
		// chain that is never compacted. Approval, denial and the token mint
		// are the audited steps.
		return LoginStart{
			DeviceCode: deviceCode,
			UserCode:   formatUserCode(userCode),
			ExpiresIn:  int(clientLoginTTL / time.Second),
			Interval:   int(clientLoginInterval / time.Second),
		}, nil
	}
	return LoginStart{}, errors.New("store: could not draw a free user code")
}

// GetClientLogin returns the request a user code names. The code is accepted
// with or without the dash, in any case, with spaces around or inside it.
// ErrLoginNotFound for an unknown, malformed or expired code.
func (s *Store) GetClientLogin(ctx context.Context, userCode string) (LoginRequestView, error) {
	code, ok := normalizeUserCode(userCode)
	if !ok {
		return LoginRequestView{}, ErrLoginNotFound
	}
	return s.clientLoginView(ctx, code, s.loginNow())
}

func (s *Store) clientLoginView(ctx context.Context, code string, now time.Time) (LoginRequestView, error) {
	row, err := s.q.GetClientLoginByUserCode(ctx, code, now)
	if errors.Is(err, db.ErrNotFound) {
		return LoginRequestView{}, ErrLoginNotFound
	}
	if err != nil {
		return LoginRequestView{}, err
	}
	return loginView(row, now), nil
}

// ApproveClientLogin approves a pending request in the name of approverID;
// the client token minted when the request is collected will belong to that
// user and carry tokenName. Nothing is minted here.
//
// ErrLoginTokenName for a bad name, ErrLoginNotFound for an unknown or
// expired code, ErrLoginConflict when the request was decided already (it
// stays as it was decided).
func (s *Store) ApproveClientLogin(ctx context.Context, userCode, approverID, tokenName string) (LoginRequestView, error) {
	name, ok := cleanTokenName(tokenName)
	if !ok {
		return LoginRequestView{}, ErrLoginTokenName
	}
	if approverID == "" {
		return LoginRequestView{}, ErrForbidden
	}
	return s.decideClientLogin(ctx, userCode, db.ClientLoginApproved, approverID, approverID, name)
}

// DenyClientLogin denies a pending request. Errors as ApproveClientLogin.
func (s *Store) DenyClientLogin(ctx context.Context, userCode, userID string) (LoginRequestView, error) {
	return s.decideClientLogin(ctx, userCode, db.ClientLoginDenied, userID, "", "")
}

func (s *Store) decideClientLogin(ctx context.Context, userCode, status, actorID, approvedBy, tokenName string) (LoginRequestView, error) {
	code, ok := normalizeUserCode(userCode)
	if !ok {
		return LoginRequestView{}, ErrLoginNotFound
	}
	now := s.loginNow()
	// Read the request first: what the audit row and the answer say about it
	// (hostname, source address, …) never changes, and once the decision is
	// written a poll may remove the row at any moment — a denial is reported
	// once, an approval is collected. Nothing below depends on reading it
	// again after a decision that took effect.
	row, err := s.q.GetClientLoginByUserCode(ctx, code, now)
	if errors.Is(err, db.ErrNotFound) {
		return LoginRequestView{}, ErrLoginNotFound
	}
	if err != nil {
		return LoginRequestView{}, err
	}
	// The statement carries the state check (pending, not expired), so of
	// two decisions one wins whatever was read above.
	decided, err := s.q.DecideClientLogin(ctx, code, status, approvedBy, tokenName, now)
	if err != nil {
		return LoginRequestView{}, err
	}
	if !decided {
		// Decided by someone else, or gone since the read.
		if _, err := s.q.GetClientLoginByUserCode(ctx, code, now); errors.Is(err, db.ErrNotFound) {
			return LoginRequestView{}, ErrLoginNotFound
		} else if err != nil {
			return LoginRequestView{}, err
		}
		return LoginRequestView{}, ErrLoginConflict
	}
	if s.clientLoginAfterDecide != nil {
		s.clientLoginAfterDecide()
	}
	row.Status = status
	if tokenName != "" {
		row.TokenName = tokenName
	}
	action := audit.ActionClientLoginApproved
	if status == db.ClientLoginDenied {
		action = audit.ActionClientLoginDenied
	}
	s.emitAudit(ctx, action, func(e *audit.Event) {
		if e.ActorID == "" {
			e.ActorID = actorID
		}
		e.SubjectLabel = row.Hostname
		e.Payload = loginAuditPayload(row, tokenName)
	})
	return loginView(row, now), nil
}

// PollClientLogin is the client asking for the outcome of its request.
//
//   - ErrLoginNotFound: unknown device code, expired, or collected already.
//   - ErrLoginSlowDown: polled less than the interval ago. Nothing else
//     happens, whatever the state of the request.
//   - ErrLoginPending: no decision yet.
//   - ErrLoginDenied: denied; the request is removed, so this is said once.
//   - nil: approved. The token is minted for the approver and returned, and
//     the request is removed, in one transaction; every later poll gets
//     ErrLoginNotFound.
func (s *Store) PollClientLogin(ctx context.Context, deviceCode string) (LoginResult, error) {
	// A device code is 43 characters; anything far from that is not one, and
	// is not worth a hash and a query.
	if deviceCode == "" || len(deviceCode) > 128 {
		return LoginResult{}, ErrLoginNotFound
	}
	now := s.loginNow()
	hash := auth.HashToken(deviceCode)
	row, err := s.q.GetClientLoginByDeviceHash(ctx, hash)
	if errors.Is(err, db.ErrNotFound) {
		return LoginResult{}, ErrLoginNotFound
	}
	if err != nil {
		return LoginResult{}, err
	}
	if !row.ExpiresAt.After(now) {
		_ = s.q.DeleteClientLogin(ctx, hash)
		return LoginResult{}, ErrLoginNotFound
	}

	allowed, err := s.q.TouchClientLoginPoll(ctx, hash, now, now.Add(-clientLoginInterval))
	if err != nil {
		return LoginResult{}, err
	}
	if !allowed {
		return LoginResult{}, ErrLoginSlowDown
	}

	switch row.Status {
	case db.ClientLoginPending:
		return LoginResult{}, ErrLoginPending
	case db.ClientLoginDenied:
		_ = s.q.DeleteClientLogin(ctx, hash)
		return LoginResult{}, ErrLoginDenied
	case db.ClientLoginApproved:
		// below
	default:
		return LoginResult{}, ErrLoginNotFound
	}

	// The token goes to the approver as that user is now: one who was deleted
	// takes the request along (ON DELETE CASCADE), one who was suspended
	// since gets no token.
	approver, err := s.q.GetUserByID(ctx, row.ApprovedBy)
	if err == nil && approver.Status == "suspended" {
		err = db.ErrNotFound
	}
	if errors.Is(err, db.ErrNotFound) {
		_ = s.q.DeleteClientLogin(ctx, hash)
		return LoginResult{}, ErrLoginNotFound
	}
	if err != nil {
		return LoginResult{}, err
	}

	pt, ct, err := newClientToken(approver.ID, row.TokenName)
	if err != nil {
		return LoginResult{}, err
	}
	collected, err := s.q.CollectClientLogin(ctx, hash, now, ct)
	if err != nil {
		return LoginResult{}, err
	}
	if !collected {
		// Another poll collected it, or it went away in between.
		return LoginResult{}, ErrLoginNotFound
	}
	s.emitAudit(ctx, audit.ActionTokenMint, func(e *audit.Event) {
		// The poll is anonymous; the token's owner is the approver.
		e.ActorID = approver.ID
		e.ActorEmail = approver.Email
		e.SubjectID = ct.ID
		e.SubjectLabel = ct.Name
		e.Payload = loginAuditPayload(row, "")
	})
	return LoginResult{Token: pt, TokenName: ct.Name, Email: approver.Email}, nil
}

// SweepClientLogins removes the expired sign-in requests and reports how
// many. The retention job runs the same delete.
func (s *Store) SweepClientLogins(ctx context.Context) (int, error) {
	return s.q.DeleteExpiredClientLogins(ctx, s.loginNow())
}

func loginView(r db.ClientLoginRequest, now time.Time) LoginRequestView {
	suggested := r.TokenName
	if suggested == "" {
		suggested = cleanLoginText(r.Hostname, maxTokenNameLen)
	}
	age := now.Sub(r.CreatedAt)
	if age < 0 {
		age = 0
	}
	return LoginRequestView{
		UserCode: formatUserCode(r.UserCode), Hostname: r.Hostname, OS: r.OS, Arch: r.Arch,
		ClientVersion: r.ClientVersion, SourceIP: r.SourceIP, Status: r.Status,
		SuggestedTokenName: suggested,
		CreatedAt:          r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(),
		Age: age,
	}
}

// loginAuditPayload is what an audit row says about a sign-in request: what
// the approver saw, and never a code or a token.
func loginAuditPayload(r db.ClientLoginRequest, tokenName string) []byte {
	m := map[string]string{
		"hostname":       r.Hostname,
		"source_ip":      r.SourceIP,
		"os":             r.OS,
		"arch":           r.Arch,
		"client_version": r.ClientVersion,
	}
	if tokenName != "" {
		m["token_name"] = tokenName
	}
	return audit.MustJSON(m)
}

// newUserCode draws eight characters of userCodeAlphabet from crypto/rand,
// without modulo bias.
func newUserCode() (string, error) {
	const n = len(userCodeAlphabet) // 31
	const limit = 256 - (256 % n)   // 248: bytes below it map evenly
	out := make([]byte, 0, userCodeLen)
	buf := make([]byte, 2*userCodeLen)
	for len(out) < userCodeLen {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < limit && len(out) < userCodeLen {
				out = append(out, userCodeAlphabet[int(b)%n])
			}
		}
	}
	return string(out), nil
}

// formatUserCode shows a stored user code as XXXX-XXXX.
func formatUserCode(code string) string {
	if len(code) != userCodeLen {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// normalizeUserCode turns what a person typed into the stored form: upper
// case, without dashes and spaces. ok is false for anything that cannot be a
// user code, so that such input never reaches the database.
func normalizeUserCode(in string) (code string, ok bool) {
	if len(in) > 64 {
		return "", false
	}
	out := make([]byte, 0, userCodeLen)
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case c == '-' || c == ' ' || c == '\t':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		}
		if strings.IndexByte(userCodeAlphabet, c) < 0 || len(out) == userCodeLen {
			return "", false
		}
		out = append(out, c)
	}
	if len(out) != userCodeLen {
		return "", false
	}
	return string(out), true
}

// cleanLoginText makes a client-supplied string safe to store and to show:
// valid UTF-8, no control or format characters (escape sequences, line
// breaks, direction overrides, zero-width characters), trimmed, at most max
// characters.
func cleanLoginText(in string, max int) string {
	// Bound the work before looking at the content.
	if len(in) > 4*max+64 {
		in = in[:4*max+64]
	}
	in = strings.ToValidUTF8(in, "")
	var b strings.Builder
	for _, r := range in {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) > max {
		out = strings.TrimSpace(string([]rune(out)[:max]))
	}
	return out
}

// cleanTokenName checks the name a token is approved with: 1 to 120
// characters after trimming, valid UTF-8, no control or format characters.
// Unlike the client's own strings it is refused, not repaired: a signed-in
// user typed it.
func cleanTokenName(in string) (string, bool) {
	name := strings.TrimSpace(in)
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxTokenNameLen {
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
	}
	return name, true
}
