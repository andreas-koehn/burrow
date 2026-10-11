package db

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// tokensOf returns, in the order of list, the tokens of the given users. A
// database that outlives the test run (Postgres) holds other users' tokens.
func tokensOf(list []ClientToken, userIDs ...string) []ClientToken {
	var out []ClientToken
	for _, ct := range list {
		for _, u := range userIDs {
			if ct.UserID == u {
				out = append(out, ct)
			}
		}
	}
	return out
}

// freshTokenUser creates the user anew: deleting a user deletes its tokens.
func freshTokenUser(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	_ = x.DeleteUser(ctx, userID)
	mustUser(t, x, userID)
	t.Cleanup(func() { _ = x.DeleteUser(ctx, userID) })
}

func mustClientToken(t *testing.T, x *DB, id, userID, name string) {
	t.Helper()
	if err := x.CreateClientToken(context.Background(), ClientToken{ID: id, UserID: userID, Name: name, TokenHash: "h-" + id}); err != nil {
		t.Fatalf("create client token %s: %v", id, err)
	}
}

func TestRecordClientSession(t *testing.T) { checkRecordClientSession(t, testDB(t), "u-rec") }

func checkRecordClientSession(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshTokenUser(t, x, userID)
	id := "ct-" + userID
	mustClientToken(t, x, id, userID, "laptop")

	list, err := x.ListClientTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := tokensOf(list, userID)
	if len(before) != 1 || before[0].ID != id || before[0].Name != "laptop" || before[0].CreatedAt.IsZero() {
		t.Fatalf("before: %+v", before)
	}
	if b := before[0]; b.LastSeen != nil || b.LastServed == nil || len(b.LastServed) != 0 ||
		b.LastOS != "" || b.LastArch != "" || b.LastClientVersion != "" || b.LastRemoteAddr != "" {
		t.Fatalf("a token that never connected must read as zero with an empty, non-nil LastServed: %+v", b)
	}

	rec := ClientSessionRecord{OS: "linux", Arch: "amd64", ClientVersion: "0.9.0", RemoteAddr: "10.0.0.5:4242", Served: []string{"web", "ollama"}}
	if err := x.RecordClientSession(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	list, err = x.ListClientTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after := tokensOf(list, userID)
	if len(after) != 1 {
		t.Fatalf("after: %+v", after)
	}
	got := after[0]
	if got.LastOS != "linux" || got.LastArch != "amd64" || got.LastClientVersion != "0.9.0" || got.LastRemoteAddr != "10.0.0.5:4242" {
		t.Fatalf("recorded fields: %+v", got)
	}
	if want := []string{"ollama", "web"}; !reflect.DeepEqual(got.LastServed, want) {
		t.Fatalf("LastServed = %v, want %v (sorted)", got.LastServed, want)
	}
	if got.LastSeen == nil || time.Since(*got.LastSeen) > time.Second || time.Until(*got.LastSeen) > time.Second {
		t.Fatalf("LastSeen = %v, want within a second of now", got.LastSeen)
	}
	// The lookup by hash reads the same columns.
	byHash, err := x.GetClientTokenByHash(ctx, "h-"+id)
	if err != nil || byHash.LastOS != "linux" || !reflect.DeepEqual(byHash.LastServed, []string{"ollama", "web"}) || byHash.LastSeen == nil {
		t.Fatalf("by hash: %v %+v", err, byHash)
	}

	// A later session that served nothing replaces the record.
	if err := x.RecordClientSession(ctx, id, ClientSessionRecord{OS: "darwin"}); err != nil {
		t.Fatal(err)
	}
	byHash, err = x.GetClientTokenByHash(ctx, "h-"+id)
	if err != nil || byHash.LastOS != "darwin" || byHash.LastArch != "" || byHash.LastServed == nil || len(byHash.LastServed) != 0 {
		t.Fatalf("after the second record: %v %+v", err, byHash)
	}
}

func TestRecordClientSessionUnknownToken(t *testing.T) {
	checkRecordClientSessionUnknownToken(t, testDB(t), "u-rec-unknown")
}

func checkRecordClientSessionUnknownToken(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshTokenUser(t, x, userID)
	id := "ct-" + userID
	mustClientToken(t, x, id, userID, "laptop")

	err := x.RecordClientSession(ctx, id+"-missing", ClientSessionRecord{OS: "linux", Served: []string{"web"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
	list, err := x.ListClientTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := tokensOf(list, userID)
	if len(got) != 1 || got[0].LastSeen != nil || got[0].LastOS != "" || len(got[0].LastServed) != 0 {
		t.Fatalf("a record for an unknown token must write nothing: %+v", got)
	}
}

func TestRecordClientSessionCapsFields(t *testing.T) {
	checkRecordClientSessionCapsFields(t, testDB(t), "u-rec-caps")
}

func checkRecordClientSessionCapsFields(t *testing.T, x *DB, userID string) {
	t.Helper()
	ctx := context.Background()
	freshTokenUser(t, x, userID)
	id := "ct-" + userID
	mustClientToken(t, x, id, userID, "laptop")

	served := make([]string, 200)
	for i := range served {
		served[i] = fmt.Sprintf("svc-%03d", i)
	}
	// Names that would break the stored list or the page that shows them.
	served = append(served, "", "  ", "aaa\nbbb", "aaa\nbbb", "\x00\x1b", strings.Repeat("n", 300))
	rec := ClientSessionRecord{
		OS:            strings.Repeat("o", 300),
		Arch:          "amd\n64\x00",
		ClientVersion: strings.Repeat("é", 100),
		RemoteAddr:    strings.Repeat("a", 300),
		Served:        served,
	}
	if err := x.RecordClientSession(ctx, id, rec); err != nil {
		t.Fatal(err)
	}
	got, err := x.GetClientTokenByHash(ctx, "h-"+id)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastOS != strings.Repeat("o", 64) {
		t.Fatalf("LastOS has %d characters, want 64", len([]rune(got.LastOS)))
	}
	if got.LastArch != "amd64" {
		t.Fatalf("LastArch = %q, want control characters stripped", got.LastArch)
	}
	if got.LastClientVersion != strings.Repeat("é", 64) {
		t.Fatalf("LastClientVersion = %q, want 64 characters (not bytes)", got.LastClientVersion)
	}
	if got.LastRemoteAddr != strings.Repeat("a", 128) {
		t.Fatalf("LastRemoteAddr has %d characters, want 128", len(got.LastRemoteAddr))
	}
	if len(got.LastServed) != 50 {
		t.Fatalf("LastServed has %d names, want 50", len(got.LastServed))
	}
	for i, n := range got.LastServed {
		if n == "" || strings.TrimSpace(n) != n || strings.ContainsAny(n, "\n\x00\x1b") || len([]rune(n)) > 128 {
			t.Fatalf("LastServed[%d] = %q", i, n)
		}
		if i > 0 && got.LastServed[i-1] >= n {
			t.Fatalf("LastServed is not sorted without duplicates at %d: %q, %q", i, got.LastServed[i-1], n)
		}
	}
	// "aaabbb" sorts before every "svc-…": the newline is gone, the name is
	// one entry and it is there once.
	if got.LastServed[0] != "aaabbb" || got.LastServed[1] == "aaabbb" {
		t.Fatalf("LastServed starts with %q, %q", got.LastServed[0], got.LastServed[1])
	}
}

func TestListClientTokensOrder(t *testing.T) {
	checkListClientTokensOrder(t, testDB(t), "u-order-1", "u-order-2")
}

func checkListClientTokensOrder(t *testing.T, x *DB, user1, user2 string) {
	t.Helper()
	ctx := context.Background()
	freshTokenUser(t, x, user1)
	freshTokenUser(t, x, user2)
	// Two tokens share a name: the older one comes first, whatever its id.
	mustClientToken(t, x, "ct-order-1", user1, "workstation")
	mustClientToken(t, x, "ct-order-2", user2, "laptop")
	mustClientToken(t, x, "ct-order-3", user2, "nas")
	mustClientToken(t, x, "ct-order-4", user1, "laptop")
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for id, at := range map[string]time.Time{
		"ct-order-1": base, "ct-order-2": base.Add(2 * time.Minute), "ct-order-3": base.Add(time.Minute), "ct-order-4": base.Add(time.Minute),
	} {
		if _, err := x.sqlDB.ExecContext(ctx, `UPDATE client_tokens SET created_at=? WHERE id=?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	list, err := x.ListClientTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, ct := range tokensOf(list, user1, user2) {
		ids = append(ids, ct.ID)
	}
	if want := []string{"ct-order-4", "ct-order-2", "ct-order-3", "ct-order-1"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("order = %v, want %v (name, then created_at)", ids, want)
	}
	// The list of one user is unchanged: oldest first.
	mine, err := x.ListClientTokensByUser(ctx, user1)
	if err != nil || len(mine) != 2 || mine[0].ID != "ct-order-1" || mine[1].ID != "ct-order-4" || mine[0].LastServed == nil {
		t.Fatalf("by user: %v %+v", err, mine)
	}
}

// What migration 0026 adds, checked on both engines.
var migration0026Columns = []string{"last_seen", "last_os", "last_arch", "last_client_version", "last_remote_addr", "last_served"}

func TestMigrate0026Schema(t *testing.T) {
	x := testDB(t)
	for _, c := range migration0026Columns {
		var n int
		if err := x.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('client_tokens') WHERE name=?`, c).Scan(&n); err != nil || n != 1 {
			t.Errorf("column client_tokens.%s missing (n=%d, err=%v)", c, n, err)
		}
	}
}

// The SQLite and the Postgres file of migration 0026 hold the same
// statements; only the path in the first comment line and the type of
// last_seen differ.
func TestMigration0026_PairIsIdentical(t *testing.T) {
	body := func(name string) string {
		raw, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, rest, _ := strings.Cut(string(raw), "\n")
		return rest
	}
	a := body("0026_v0.10.0_client_last_session.sql")
	b := strings.Replace(body("0026_v0.10.0_client_last_session.postgres.sql"), "TIMESTAMPTZ;", "TIMESTAMP;", 1)
	if a != b || !strings.Contains(a, "-- +goose Down") {
		t.Fatalf("the pair differs or has no Down block:\n%s\n---\n%s", a, b)
	}
}

func TestMigration0026_DownAndUp(t *testing.T) {
	checkMigration0026DownAndUp(t, testDB(t), "0026_v0.10.0_client_last_session.sql", "u-mig26")
}

// checkMigration0026DownAndUp runs the Down block and the Up block again: Up
// only succeeds when Down removed every column Up adds, and a token that was
// there before reads as never connected. Everything happens in one
// transaction that is rolled back, so a database shared with other tests
// never shows the schema without the columns.
func checkMigration0026DownAndUp(t *testing.T, x *DB, file, userID string) {
	t.Helper()
	ctx := context.Background()
	raw, err := migrationFS.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	_, down, found := strings.Cut(string(raw), "-- +goose Down")
	if !found || strings.TrimSpace(down) == "" {
		t.Fatalf("%s has no Down block", file)
	}
	freshTokenUser(t, x, userID)

	tx, err := x.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, down); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO client_tokens(id, user_id, name, token_hash) VALUES(?,?,?,?)`,
		"ct-"+userID, userID, "old", "h-ct-"+userID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, upBlock(string(raw))); err != nil {
		t.Fatalf("up after down (a column the Down block left behind fails here): %v", err)
	}
	ct, err := scanClientToken(tx.QueryRowContext(ctx, `SELECT `+clientTokenCols+` FROM client_tokens WHERE id=?`, "ct-"+userID))
	if err != nil || ct.LastSeen != nil || ct.LastOS != "" || ct.LastRemoteAddr != "" || len(ct.LastServed) != 0 {
		t.Fatalf("old token: %v %+v, want never connected", err, ct)
	}
}
