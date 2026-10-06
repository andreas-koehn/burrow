package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestClientLogin_SQLite(t *testing.T) {
	checkClientLogin(t, testDB(t), "u-login-sqlite")
}

// checkClientLogin exercises the client_login_requests queries. It runs
// against SQLite here and against a live Postgres in the postgres-tagged
// test, so every statement is proven on both engines.
func checkClientLogin(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(context.Background(), userID) })
	if _, err := x.DB().ExecContext(ctx, `DELETE FROM client_login_requests`); err != nil {
		t.Fatalf("clear table: %v", err)
	}
	t.Cleanup(func() { _, _ = x.DB().ExecContext(context.Background(), `DELETE FROM client_login_requests`) })

	t0 := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	mk := func(i int) ClientLoginRequest {
		return ClientLoginRequest{
			DeviceCodeHash: fmt.Sprintf("hash-%s-%d", userID, i),
			UserCode:       fmt.Sprintf("CODE%04d", i),
			Hostname:       "laptop", OS: "linux", Arch: "amd64", ClientVersion: "0.6.0",
			SourceIP: "203.0.113.7", TokenName: "suggested",
			CreatedAt: t0, ExpiresAt: t0.Add(10 * time.Minute),
		}
	}
	insert := func(i int) ClientLoginRequest {
		t.Helper()
		r := mk(i)
		ok, err := x.InsertClientLogin(ctx, r, 20)
		if err != nil || !ok {
			t.Fatalf("insert %d: ok=%v err=%v", i, ok, err)
		}
		return r
	}

	// Insert and read back, by both keys.
	r1 := insert(1)
	got, err := x.GetClientLoginByUserCode(ctx, r1.UserCode, t0)
	if err != nil {
		t.Fatalf("get by user code: %v", err)
	}
	if got.DeviceCodeHash != r1.DeviceCodeHash || got.Status != ClientLoginPending || got.Hostname != "laptop" ||
		got.OS != "linux" || got.Arch != "amd64" || got.ClientVersion != "0.6.0" || got.SourceIP != "203.0.113.7" ||
		got.ApprovedBy != "" || got.TokenName != "suggested" || got.LastPollAt != nil ||
		!got.CreatedAt.Equal(t0) || !got.ExpiresAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("row = %+v", got)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, r1.DeviceCodeHash); err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown hash: %v", err)
	}

	// A taken user code is a duplicate, not a silent overwrite.
	dup := mk(2)
	dup.UserCode = r1.UserCode
	if _, err := x.InsertClientLogin(ctx, dup, 20); !IsClientLoginDuplicate(err) {
		t.Fatalf("duplicate user code: %v", err)
	}

	// Expiry is exclusive at the last moment.
	if _, err := x.GetClientLoginByUserCode(ctx, r1.UserCode, t0.Add(10*time.Minute-time.Second)); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	if _, err := x.GetClientLoginByUserCode(ctx, r1.UserCode, t0.Add(10*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("at expiry: %v", err)
	}

	// The poll gate: once, then not again until notAfter has passed.
	if ok, err := x.TouchClientLoginPoll(ctx, r1.DeviceCodeHash, t0, t0.Add(-2*time.Second)); err != nil || !ok {
		t.Fatalf("first touch: %v %v", ok, err)
	}
	t1 := t0.Add(1900 * time.Millisecond)
	if ok, err := x.TouchClientLoginPoll(ctx, r1.DeviceCodeHash, t1, t1.Add(-2*time.Second)); err != nil || ok {
		t.Fatalf("touch after 1.9 s: %v %v, want refused", ok, err)
	}
	t2 := t0.Add(2 * time.Second)
	if ok, err := x.TouchClientLoginPoll(ctx, r1.DeviceCodeHash, t2, t2.Add(-2*time.Second)); err != nil || !ok {
		t.Fatalf("touch after 2 s: %v %v", ok, err)
	}
	if got, _ := x.GetClientLoginByDeviceHash(ctx, r1.DeviceCodeHash); got.LastPollAt == nil || !got.LastPollAt.Equal(t2) {
		t.Fatalf("last_poll_at = %v, want %v", got.LastPollAt, t2)
	}

	// Decisions: pending only, once, never after expiry.
	if ok, err := x.DecideClientLogin(ctx, r1.UserCode, ClientLoginApproved, userID, "", t0.Add(10*time.Minute)); err != nil || ok {
		t.Fatalf("approve at expiry: %v %v", ok, err)
	}
	if ok, err := x.DecideClientLogin(ctx, r1.UserCode, "collected", userID, "", t0); err == nil || ok {
		t.Fatal("an unknown status was accepted")
	}
	if ok, err := x.DecideClientLogin(ctx, r1.UserCode, ClientLoginApproved, userID, "laptop", t0); err != nil || !ok {
		t.Fatalf("approve: %v %v", ok, err)
	}
	if ok, err := x.DecideClientLogin(ctx, r1.UserCode, ClientLoginDenied, "", "", t0); err != nil || ok {
		t.Fatalf("deny after approve: %v %v", ok, err)
	}
	if ok, err := x.DecideClientLogin(ctx, r1.UserCode, ClientLoginApproved, userID, "other", t0); err != nil || ok {
		t.Fatalf("second approve: %v %v", ok, err)
	}
	got, _ = x.GetClientLoginByUserCode(ctx, r1.UserCode, t0)
	if got.Status != ClientLoginApproved || got.ApprovedBy != userID || got.TokenName != "laptop" {
		t.Fatalf("after approve: %+v", got)
	}
	r3 := insert(3)
	if ok, err := x.DecideClientLogin(ctx, r3.UserCode, ClientLoginDenied, "", "", t0); err != nil || !ok {
		t.Fatalf("deny: %v %v", ok, err)
	}
	got, _ = x.GetClientLoginByUserCode(ctx, r3.UserCode, t0)
	if got.Status != ClientLoginDenied || got.ApprovedBy != "" || got.TokenName != "suggested" {
		t.Fatalf("after deny: %+v", got)
	}

	// Collecting: not a pending or denied request, not for another user, not
	// after expiry; and of many concurrent collectors exactly one wins.
	tok := func(i int, uid string) ClientToken {
		return ClientToken{ID: fmt.Sprintf("tok-%s-%d", userID, i), UserID: uid, Name: "laptop", TokenHash: fmt.Sprintf("th-%s-%d", userID, i)}
	}
	r4 := insert(4)
	for name, c := range map[string]struct {
		hash string
		now  time.Time
		ct   ClientToken
	}{
		"pending":      {r4.DeviceCodeHash, t0, tok(100, userID)},
		"denied":       {r3.DeviceCodeHash, t0, tok(101, userID)},
		"other user":   {r1.DeviceCodeHash, t0, tok(102, "someone-else")},
		"expired":      {r1.DeviceCodeHash, t0.Add(10 * time.Minute), tok(103, userID)},
		"unknown hash": {"nope", t0, tok(104, userID)},
	} {
		if ok, err := x.CollectClientLogin(ctx, c.hash, c.now, c.ct); err != nil || ok {
			t.Fatalf("collect (%s): ok=%v err=%v, want refused", name, ok, err)
		}
	}
	const n = 20
	var wg sync.WaitGroup
	wins := make([]bool, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			wins[i], errs[i] = x.CollectClientLogin(ctx, r1.DeviceCodeHash, t0, tok(i, userID))
		}(i)
	}
	close(start)
	wg.Wait()
	won := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("collector %d: %v", i, errs[i])
		}
		if wins[i] {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d collectors won, want exactly 1", won)
	}
	ts, err := x.ListClientTokensByUser(ctx, userID)
	if err != nil || len(ts) != 1 {
		t.Fatalf("%d tokens (%v), want exactly 1", len(ts), err)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, r1.DeviceCodeHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the collected row is still there: %v", err)
	}

	// A token that cannot be inserted leaves the request in place.
	r5 := insert(5)
	if ok, err := x.DecideClientLogin(ctx, r5.UserCode, ClientLoginApproved, userID, "laptop", t0); err != nil || !ok {
		t.Fatal(err)
	}
	clash := tok(200, userID)
	clash.TokenHash = ts[0].TokenHash
	if ok, err := x.CollectClientLogin(ctx, r5.DeviceCodeHash, t0, clash); err == nil || ok {
		t.Fatalf("collect with a clashing token: ok=%v err=%v", ok, err)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, r5.DeviceCodeHash); err != nil {
		t.Fatalf("a failed collection removed the request: %v", err)
	}

	// The cap counts live pending requests only (r4 is the one pending now).
	for i := 10; i < 29; i++ {
		insert(i)
	}
	if ok, err := x.InsertClientLogin(ctx, mk(50), 20); err != nil || ok {
		t.Fatalf("insert over the cap: ok=%v err=%v", ok, err)
	}
	late := mk(51)
	late.CreatedAt, late.ExpiresAt = t0.Add(10*time.Minute), t0.Add(20*time.Minute)
	if ok, err := x.InsertClientLogin(ctx, late, 20); err != nil || !ok {
		t.Fatalf("insert once the others have expired: ok=%v err=%v", ok, err)
	}

	// Deleting the approver removes the approval too.
	other := userID + "-b"
	mustUser(t, x, other)
	r6 := mk(60)
	r6.CreatedAt, r6.ExpiresAt = late.CreatedAt, late.ExpiresAt
	if ok, err := x.InsertClientLogin(ctx, r6, 20); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := x.DecideClientLogin(ctx, r6.UserCode, ClientLoginApproved, other, "x", late.CreatedAt); err != nil || !ok {
		t.Fatal(err)
	}
	if err := x.DeleteUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, r6.DeviceCodeHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the request outlived its approver: %v", err)
	}

	// The sweep removes what has expired and nothing else.
	removed, err := x.DeleteExpiredClientLogins(ctx, t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 22 { // r3, r4, r5 and the nineteen of the cap test
		t.Fatalf("sweep removed %d, want 22", removed)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, late.DeviceCodeHash); err != nil {
		t.Fatalf("the live request was swept: %v", err)
	}
	if err := x.DeleteClientLogin(ctx, late.DeviceCodeHash); err != nil {
		t.Fatal(err)
	}
	if _, err := x.GetClientLoginByDeviceHash(ctx, late.DeviceCodeHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete left the row: %v", err)
	}
}
