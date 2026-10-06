package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/auth"
)

// loginClock is the injected clock of the sign-in tests.
type loginClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLoginClock() *loginClock {
	return &loginClock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *loginClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *loginClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// auditSink records what the store appends to the audit log.
type auditSink struct {
	mu sync.Mutex
	ev []audit.Event
}

func (a *auditSink) Append(_ context.Context, e any) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ev = append(a.ev, e.(audit.Event))
	return nil
}

func (a *auditSink) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.ev))
	for _, e := range a.ev {
		out = append(out, e.Action)
	}
	return out
}

func (a *auditSink) text() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var b strings.Builder
	for _, e := range a.ev {
		b.WriteString(strings.Join([]string{e.ActorID, e.ActorEmail, e.Action, e.SubjectID,
			e.SubjectLabel, e.SourceIP, e.UserAgent, e.RequestID, string(e.Payload)}, "|"))
		b.WriteString("\n")
	}
	return b.String()
}

// loginFixture is a store with an injected clock and one user.
func loginFixture(t *testing.T) (*Store, *sql.DB, *loginClock, string) {
	t.Helper()
	s, d := newStoreWithDB(t)
	clk := newLoginClock()
	s.SetClientLoginClock(clk.now)
	return s, d, clk, loginUser(t, s, "u1@x")
}

func loginUser(t *testing.T, s *Store, email string) string {
	t.Helper()
	u, err := s.CreateUser(context.Background(), email, "password1", "user")
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u.ID
}

var loginMeta = LoginMeta{Hostname: "laptop.local", OS: "linux", Arch: "amd64", ClientVersion: "0.6.0", SourceIP: "203.0.113.7"}

func mustStart(t *testing.T, s *Store) LoginStart {
	t.Helper()
	st, err := s.StartClientLogin(context.Background(), loginMeta)
	if err != nil {
		t.Fatalf("StartClientLogin: %v", err)
	}
	return st
}

func tokenCount(t *testing.T, s *Store, userID string) int {
	t.Helper()
	ts, err := s.ListClientTokens(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return len(ts)
}

func loginRows(t *testing.T, d *sql.DB) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM client_login_requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestClientLogin_Lifecycle(t *testing.T) {
	ctx := context.Background()
	s, d, clk, u1 := loginFixture(t)

	st := mustStart(t, s)
	if st.ExpiresIn != 600 || st.Interval != 2 {
		t.Fatalf("expires_in=%d interval=%d, want 600 and 2", st.ExpiresIn, st.Interval)
	}
	v, err := s.GetClientLogin(ctx, st.UserCode)
	if err != nil {
		t.Fatalf("GetClientLogin: %v", err)
	}
	if v.Status != "pending" || v.Hostname != "laptop.local" || v.OS != "linux" || v.Arch != "amd64" ||
		v.ClientVersion != "0.6.0" || v.SourceIP != "203.0.113.7" || v.UserCode != st.UserCode {
		t.Fatalf("view = %+v", v)
	}
	if v.SuggestedTokenName != "laptop.local" {
		t.Fatalf("suggested name %q, want the hostname", v.SuggestedTokenName)
	}
	if !v.CreatedAt.Equal(clk.now()) || !v.ExpiresAt.Equal(clk.now().Add(10*time.Minute)) {
		t.Fatalf("created %v expires %v", v.CreatedAt, v.ExpiresAt)
	}

	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("poll while pending: %v, want ErrLoginPending", err)
	}
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("a token exists before approval")
	}

	av, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "  laptop ")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if av.Status != "approved" {
		t.Fatalf("status after approve = %q", av.Status)
	}
	// Approval mints nothing: the token appears at the first poll after it.
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("approval minted a token; it must be minted at the poll")
	}

	clk.add(3 * time.Second)
	res, err := s.PollClientLogin(ctx, st.DeviceCode)
	if err != nil {
		t.Fatalf("poll after approval: %v", err)
	}
	if res.TokenName != "laptop" || res.Email != "u1@x" {
		t.Fatalf("result name=%q email=%q", res.TokenName, res.Email)
	}
	uid, name, err := s.AuthenticateNamed(ctx, res.Token)
	if err != nil || uid != u1 || name != "laptop" {
		t.Fatalf("token authenticates as %q/%q (%v), want u1/laptop", uid, name, err)
	}
	if loginRows(t, d) != 0 {
		t.Fatal("the request row survived its collection")
	}

	clk.add(3 * time.Second)
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("second poll: %v, want ErrLoginNotFound", err)
	}
	if n := tokenCount(t, s, u1); n != 1 {
		t.Fatalf("%d tokens, want exactly 1", n)
	}
}

func TestClientLogin_TokenNameSuggestion(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := loginFixture(t)
	m := loginMeta
	m.TokenName = "build-box"
	st, err := s.StartClientLogin(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.GetClientLogin(ctx, st.UserCode)
	if err != nil || v.SuggestedTokenName != "build-box" {
		t.Fatalf("suggested = %q (%v), want build-box", v.SuggestedTokenName, err)
	}
}

func TestClientLogin_ConcurrentPollsMintOnce(t *testing.T) {
	ctx := context.Background()
	s, _, _, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]LoginResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.PollClientLogin(ctx, st.DeviceCode)
		}(i)
	}
	close(start)
	wg.Wait()

	got := 0
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil:
			got++
			if results[i].Token == "" {
				t.Fatal("a successful poll carried no token")
			}
		case errors.Is(errs[i], ErrLoginNotFound), errors.Is(errs[i], ErrLoginSlowDown):
			if results[i].Token != "" {
				t.Fatal("a failed poll carried a token")
			}
		default:
			t.Fatalf("poll %d: unexpected error %v", i, errs[i])
		}
	}
	if got != 1 {
		t.Fatalf("%d polls received a token, want exactly 1", got)
	}
	if c := tokenCount(t, s, u1); c != 1 {
		t.Fatalf("%d tokens exist, want exactly 1", c)
	}
}

func TestClientLogin_Deny(t *testing.T) {
	ctx := context.Background()
	s, d, clk, u1 := loginFixture(t)
	st := mustStart(t, s)
	v, err := s.DenyClientLogin(ctx, st.UserCode, u1)
	if err != nil || v.Status != "denied" {
		t.Fatalf("deny: %+v %v", v, err)
	}
	// A denied request cannot be approved afterwards.
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); !errors.Is(err, ErrLoginConflict) {
		t.Fatalf("approve after deny: %v, want ErrLoginConflict", err)
	}
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginDenied) {
		t.Fatalf("poll after deny: %v, want ErrLoginDenied", err)
	}
	if loginRows(t, d) != 0 {
		t.Fatal("the denied row survived the poll that reported it")
	}
	clk.add(3 * time.Second)
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("poll after the denial was reported: %v, want ErrLoginNotFound", err)
	}
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("a denied request minted a token")
	}
}

func TestClientLogin_Expiry(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)

	pending := mustStart(t, s)
	approved := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, approved.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}

	// One second before the end the request is still there.
	clk.add(10*time.Minute - time.Second)
	if _, err := s.GetClientLogin(ctx, pending.UserCode); err != nil {
		t.Fatalf("get just before expiry: %v", err)
	}

	clk.add(2 * time.Second) // 10 min + 1 s
	if _, err := s.GetClientLogin(ctx, pending.UserCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.ApproveClientLogin(ctx, pending.UserCode, u1, "x"); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.DenyClientLogin(ctx, pending.UserCode, u1); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("deny: %v", err)
	}
	if _, err := s.PollClientLogin(ctx, pending.DeviceCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("poll pending: %v", err)
	}
	// An approval that was never collected dies with the request.
	if _, err := s.PollClientLogin(ctx, approved.DeviceCode); !errors.Is(err, ErrLoginNotFound) {
		t.Fatalf("poll approved-but-expired: %v", err)
	}
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("an expired request minted a token")
	}
}

func TestClientLogin_ApproveTwice(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	u2 := loginUser(t, s, "u2@x")
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u2, "second"); !errors.Is(err, ErrLoginConflict) {
		t.Fatalf("second approve: %v, want ErrLoginConflict", err)
	}
	if _, err := s.DenyClientLogin(ctx, st.UserCode, u2); !errors.Is(err, ErrLoginConflict) {
		t.Fatalf("deny after approve: %v, want ErrLoginConflict", err)
	}
	clk.add(3 * time.Second)
	res, err := s.PollClientLogin(ctx, st.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	uid, name, _ := s.AuthenticateNamed(ctx, res.Token)
	if uid != u1 || name != "first" || res.Email != "u1@x" {
		t.Fatalf("token belongs to %q/%q (%s), want the first approver", uid, name, res.Email)
	}
	if tokenCount(t, s, u2) != 0 {
		t.Fatal("the second approver owns a token")
	}
}

func TestClientLogin_UserCodes(t *testing.T) {
	ctx := context.Background()
	s, _, clk, _ := loginFixture(t)
	re := regexp.MustCompile(`^[A-HJKMNP-Z2-9]{4}-[A-HJKMNP-Z2-9]{4}$`)
	seen := map[string]bool{}
	devices := map[string]bool{}
	var last LoginStart
	for i := 0; i < 1000; i++ {
		if i%20 == 0 && i > 0 {
			clk.add(11 * time.Minute)
			if _, err := s.SweepClientLogins(ctx); err != nil {
				t.Fatal(err)
			}
		}
		st := mustStart(t, s)
		if !re.MatchString(st.UserCode) {
			t.Fatalf("user code %q has the wrong shape", st.UserCode)
		}
		if seen[st.UserCode] || devices[st.DeviceCode] {
			t.Fatalf("code repeated after %d starts", i)
		}
		seen[st.UserCode], devices[st.DeviceCode] = true, true
		// 32 random bytes, base64url without padding.
		if len(st.DeviceCode) != 43 {
			t.Fatalf("device code has %d characters, want 43", len(st.DeviceCode))
		}
		last = st
	}

	plain := strings.ReplaceAll(last.UserCode, "-", "")
	for _, in := range []string{
		strings.ToLower(last.UserCode),
		plain,
		" " + strings.ToLower(plain[:4]) + " " + strings.ToLower(plain[4:]) + " ",
	} {
		v, err := s.GetClientLogin(ctx, in)
		if err != nil || v.UserCode != last.UserCode {
			t.Fatalf("lookup %q: %+v %v", in, v, err)
		}
	}
	for _, in := range []string{"", "ABCD", plain + "A", "ABCD-EFG!", "%", "ABCD_EFGH", plain[:7] + "0"} {
		if _, err := s.GetClientLogin(ctx, in); !errors.Is(err, ErrLoginNotFound) {
			t.Fatalf("lookup %q: %v, want ErrLoginNotFound", in, err)
		}
	}
}

// The user code alone opens nothing: it is not a device code.
func TestClientLogin_UserCodeIsNotADeviceCode(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{st.UserCode, strings.ReplaceAll(st.UserCode, "-", ""), auth.HashToken(st.DeviceCode), ""} {
		clk.add(3 * time.Second)
		if res, err := s.PollClientLogin(ctx, guess); !errors.Is(err, ErrLoginNotFound) || res.Token != "" {
			t.Fatalf("poll with %q: %v", guess, err)
		}
	}
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("a token was minted without the device code")
	}
}

func TestClientLogin_RowHoldsNoSecret(t *testing.T) {
	ctx := context.Background()
	s, d, _, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query(`SELECT * FROM client_login_requests`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	n := 0
	for rows.Next() {
		n++
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			var text string
			switch x := v.(type) {
			case string:
				text = x
			case []byte:
				text = string(x)
			}
			if strings.Contains(text, st.DeviceCode) {
				t.Fatalf("column %s holds the device code", cols[i])
			}
			if strings.HasPrefix(text, "bur_") {
				t.Fatalf("column %s holds a token", cols[i])
			}
			if cols[i] == "device_code_hash" && text != auth.HashToken(st.DeviceCode) {
				t.Fatalf("device_code_hash is not the hash of the device code")
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestClientLogin_PendingCap(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	var all []LoginStart
	for i := 0; i < 20; i++ {
		all = append(all, mustStart(t, s))
	}
	if _, err := s.StartClientLogin(ctx, loginMeta); !errors.Is(err, ErrLoginTooMany) {
		t.Fatalf("21st start: %v, want ErrLoginTooMany", err)
	}

	// One collected request frees a place.
	if _, err := s.ApproveClientLogin(ctx, all[0].UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PollClientLogin(ctx, all[0].DeviceCode); err != nil {
		t.Fatal(err)
	}
	mustStart(t, s)
	if _, err := s.StartClientLogin(ctx, loginMeta); !errors.Is(err, ErrLoginTooMany) {
		t.Fatalf("start at the cap again: %v, want ErrLoginTooMany", err)
	}

	// Expiry frees them all, without a sweep.
	clk.add(10*time.Minute + time.Second)
	mustStart(t, s)
}

func TestClientLogin_SlowDown(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginPending) {
		t.Fatalf("first poll: %v", err)
	}
	clk.add(1900 * time.Millisecond)
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginSlowDown) {
		t.Fatalf("poll after 1.9 s: %v, want ErrLoginSlowDown", err)
	}
	// Polling too fast does not hand out the token either.
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	if res, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginSlowDown) || res.Token != "" {
		t.Fatalf("fast poll after approval: %v", err)
	}
	if tokenCount(t, s, u1) != 0 {
		t.Fatal("a too-fast poll minted a token")
	}
	clk.add(100 * time.Millisecond) // 2 s after the first poll
	if _, err := s.PollClientLogin(ctx, st.DeviceCode); err != nil {
		t.Fatalf("poll after 2 s: %v", err)
	}
}

func TestClientLogin_Sweep(t *testing.T) {
	ctx := context.Background()
	s, d, clk, _ := loginFixture(t)
	mustStart(t, s)
	mustStart(t, s)
	clk.add(6 * time.Minute)
	live := mustStart(t, s)
	clk.add(5 * time.Minute) // the first two are 11 minutes old, the third 5
	n, err := s.SweepClientLogins(ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep removed %d (%v), want 2", n, err)
	}
	if loginRows(t, d) != 1 {
		t.Fatalf("%d rows left, want 1", loginRows(t, d))
	}
	if _, err := s.GetClientLogin(ctx, live.UserCode); err != nil {
		t.Fatalf("the live request was swept: %v", err)
	}
	if n, _ := s.SweepClientLogins(ctx); n != 0 {
		t.Fatalf("second sweep removed %d", n)
	}
}

func TestClientLogin_MetadataIsSanitised(t *testing.T) {
	ctx := context.Background()
	s, _, _, _ := loginFixture(t)
	st, err := s.StartClientLogin(ctx, LoginMeta{
		Hostname:      "\x1b[31m" + strings.Repeat("h", 500),
		OS:            "li\nnux\x00" + strings.Repeat("o", 100),
		Arch:          "amd‮64​",
		ClientVersion: "0.6.0\r\n\xff",
		SourceIP:      "203.0.113.7",
		TokenName:     "\tmy\x07 box" + strings.Repeat("n", 200),
	})
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.GetClientLogin(ctx, st.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(v.Hostname)) != 253 || !strings.HasPrefix(v.Hostname, "[31mhhh") {
		t.Fatalf("hostname has %d characters: %.20q", len([]rune(v.Hostname)), v.Hostname)
	}
	if len([]rune(v.OS)) != 64 || !strings.HasPrefix(v.OS, "linuxooo") {
		t.Fatalf("os = %q", v.OS)
	}
	if v.Arch != "amd64" {
		t.Fatalf("arch = %q, want the direction override and the zero-width space gone", v.Arch)
	}
	if v.ClientVersion != "0.6.0" {
		t.Fatalf("client version = %q", v.ClientVersion)
	}
	if len([]rune(v.SuggestedTokenName)) != 120 || !strings.HasPrefix(v.SuggestedTokenName, "my box") {
		t.Fatalf("suggested name = %.20q (%d)", v.SuggestedTokenName, len([]rune(v.SuggestedTokenName)))
	}
	for _, f := range []string{v.Hostname, v.OS, v.Arch, v.ClientVersion, v.SuggestedTokenName} {
		for _, r := range f {
			if r < 0x20 || r == 0x7f || r == '�' {
				t.Fatalf("control character %U in %q", r, f)
			}
		}
	}
}

func TestClientLogin_TokenNameRules(t *testing.T) {
	ctx := context.Background()
	s, _, _, u1 := loginFixture(t)
	st := mustStart(t, s)
	for _, bad := range []string{"", "   ", "a\nb", "a\x00b", "a‮b", strings.Repeat("x", 121), "\xff\xfe"} {
		if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, bad); !errors.Is(err, ErrLoginTokenName) {
			t.Fatalf("approve with name %q: %v, want ErrLoginTokenName", bad, err)
		}
	}
	if v, err := s.GetClientLogin(ctx, st.UserCode); err != nil || v.Status != "pending" {
		t.Fatalf("a refused name changed the request: %+v %v", v, err)
	}
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, strings.Repeat("x", 120)); err != nil {
		t.Fatalf("120 characters must be accepted: %v", err)
	}
}

func TestClientLogin_ApproverDeletedBeforePoll(t *testing.T) {
	ctx := context.Background()
	s, d, clk, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, u1); err != nil {
		t.Fatal(err)
	}
	clk.add(3 * time.Second)
	if res, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginNotFound) || res.Token != "" {
		t.Fatalf("poll after the approver was deleted: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM client_tokens`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d tokens exist (%v), want none", n, err)
	}
}

func TestClientLogin_ApproverSuspendedBeforePoll(t *testing.T) {
	ctx := context.Background()
	s, d, clk, u1 := loginFixture(t)
	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserStatus(ctx, u1, "suspended"); err != nil {
		t.Fatal(err)
	}
	clk.add(3 * time.Second)
	if res, err := s.PollClientLogin(ctx, st.DeviceCode); !errors.Is(err, ErrLoginNotFound) || res.Token != "" {
		t.Fatalf("poll after the approver was suspended: %v", err)
	}
	if tokenCount(t, s, u1) != 0 || loginRows(t, d) != 0 {
		t.Fatal("a suspended approver's request must be dropped without a token")
	}
}

func TestClientLogin_Audit(t *testing.T) {
	ctx := context.Background()
	s, _, clk, u1 := loginFixture(t)
	sink := &auditSink{}
	s.SetAuditLogger(sink)

	st := mustStart(t, s)
	if _, err := s.ApproveClientLogin(ctx, st.UserCode, u1, "laptop"); err != nil {
		t.Fatal(err)
	}
	clk.add(3 * time.Second)
	res, err := s.PollClientLogin(ctx, st.DeviceCode)
	if err != nil {
		t.Fatal(err)
	}
	denied := mustStart(t, s)
	if _, err := s.DenyClientLogin(ctx, denied.UserCode, u1); err != nil {
		t.Fatal(err)
	}

	want := []string{"client.login.started", "client.login.approved", "token.mint", "client.login.started", "client.login.denied"}
	if got := sink.actions(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions %v, want %v", got, want)
	}
	text := sink.text()
	for _, secret := range []string{
		st.DeviceCode, auth.HashToken(st.DeviceCode), st.UserCode, strings.ReplaceAll(st.UserCode, "-", ""),
		denied.DeviceCode, denied.UserCode, res.Token, auth.HashToken(res.Token),
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("the audit log holds a code or a token:\n%s", text)
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.ev {
		if !strings.Contains(string(e.Payload), `"hostname":"laptop.local"`) ||
			!strings.Contains(string(e.Payload), `"source_ip":"203.0.113.7"`) {
			t.Fatalf("%s lacks the request's hostname and source IP: %s", e.Action, e.Payload)
		}
		switch e.Action {
		case "client.login.approved", "client.login.denied", "token.mint":
			if e.ActorID != u1 {
				t.Fatalf("%s actor = %q, want the approver", e.Action, e.ActorID)
			}
		}
	}
}
