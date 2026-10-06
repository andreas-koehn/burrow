package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFileConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("bur_abc\n"), 0o600)
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token_file: "+tok+"\n"+
		"services:\n"+
		"  - { name: ollama, local: 127.0.0.1:11434, type: http }\n"+
		"  - { name: db, local: 127.0.0.1:5432, type: tcp, remote: 9000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.Server != "relay.example.com:7000" || c.Token != "bur_abc" {
		t.Fatalf("bad: %+v", c)
	}
	if len(c.Tunnels) != 2 || c.Tunnels[0].Type != "http" || c.Tunnels[1].Type != "tcp" {
		t.Fatalf("services not parsed: %+v", c.Tunnels)
	}
}

func TestLoadFileConfig_DefaultTypeTCP(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tunnels[0].Type != "tcp" {
		t.Fatalf("expected default type tcp, got %q", c.Tunnels[0].Type)
	}
}

// server and token are optional: without them `burrow up` uses the sign-in.
func TestLoadFileConfig_ServerOptional(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("token: mytoken\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatalf("a file without server must load: %v", err)
	}
	if c.Server != "" || c.Token != "mytoken" || len(c.Tunnels) != 1 {
		t.Fatalf("server %q, tunnels %d", c.Server, len(c.Tunnels))
	}
}

func TestLoadFileConfig_BothTokensError(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("bur_abc\n"), 0o600)
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"token_file: "+tok+"\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestLoadFileConfig_TokenOptional(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatalf("a file without token must load: %v", err)
	}
	if c.Server != "relay.example.com:7000" || c.Token != "" {
		t.Fatalf("server %q, token empty: %v", c.Server, c.Token == "")
	}
}

func TestLoadFileConfig_ServicesOnly(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"+
		"  - { name: web, local: 127.0.0.1:8080, type: http }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatalf("a file with services only must load: %v", err)
	}
	if c.Server != "" || c.Token != "" || len(c.Tunnels) != 2 || c.Tunnels[0].Type != "tcp" || c.Tunnels[1].Type != "http" {
		t.Fatalf("server %q, tunnels %+v", c.Server, c.Tunnels)
	}
}

// Blank values count as absent, so a sign-in can fill them.
func TestLoadFileConfig_BlankServerAndTokenAreAbsent(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("  \n"), 0o600)
	for name, head := range map[string]string{
		"blank values":     "server: \"  \"\ntoken: \" \"\n",
		"blank token file": "server: \"\"\ntoken_file: " + tok + "\n",
	} {
		yml := filepath.Join(dir, "burrow.yaml")
		os.WriteFile(yml, []byte(head+"services:\n  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
		c, err := LoadFileConfig(yml)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Server != "" || c.Token != "" {
			t.Fatalf("%s: server %q, token empty: %v", name, c.Server, c.Token == "")
		}
	}
}

func TestLoadFileConfig_OnlyToken(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatalf("expected success with only token, got %v", err)
	}
	if c.Token != "mytoken" {
		t.Fatalf("expected token mytoken, got %q", c.Token)
	}
}

func TestLoadFileConfig_NoServicesError(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services: []\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil || !strings.Contains(err.Error(), "service") {
		t.Fatalf("expected service error, got %v", err)
	}
}

func TestLoadFileConfig_UnknownTypeError(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000, type: grpc }\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("expected error containing bad type, got %v", err)
	}
}

func TestLoadFileConfig_TrimTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	// Windows-style CRLF trailing newline
	os.WriteFile(tok, []byte("bur_xyz\r\n"), 0o600)
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token_file: "+tok+"\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "bur_xyz" {
		t.Fatalf("expected bur_xyz, got %q", c.Token)
	}
}

func TestLoadFileConfig_MissingTokenFile(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token_file: /nonexistent/path/token\n"+
		"services:\n"+
		"  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil {
		t.Fatal("expected error for missing token_file")
	}
	// Should contain some indication of file-read failure
	if !strings.Contains(err.Error(), "token_file") && !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("expected file-read error, got %v", err)
	}
}

func TestLoadFileConfig_ServiceMissingName(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { local: 127.0.0.1:3000 }\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name error, got %v", err)
	}
}

func TestLoadFileConfig_ServiceMissingLocal(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: app }\n"), 0o600)
	_, err := LoadFileConfig(yml)
	if err == nil || !strings.Contains(err.Error(), "local") {
		t.Fatalf("expected local error, got %v", err)
	}
}

func TestLoadFileConfig_RemoteIgnoredForHTTP(t *testing.T) {
	// remote is allowed in YAML for http type (no error), just not propagated
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: web, local: 127.0.0.1:3000, type: http, remote: 9000 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatalf("expected no error for remote with http, got %v", err)
	}
	// remote should not be propagated for http
	if c.Tunnels[0].RemotePort != 0 {
		t.Fatalf("expected RemotePort=0 for http tunnel, got %d", c.Tunnels[0].RemotePort)
	}
}

func TestLoadFileConfig_TCPRemotePort(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\n"+
		"token: mytoken\n"+
		"services:\n"+
		"  - { name: db, local: 127.0.0.1:5432, type: tcp, remote: 9100 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	if c.Tunnels[0].RemotePort != 9100 {
		t.Fatalf("expected RemotePort=9100, got %d", c.Tunnels[0].RemotePort)
	}
}

// LoadCompleteFileConfig is the loader of `burrow connect --config`: server and
// token are required, with the messages and the order of checks they always had.
func TestLoadCompleteFileConfig(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("bur_abc \r\n"), 0o600)
	svc := "services:\n  - { name: app, local: 127.0.0.1:3000 }\n"
	bad := []struct{ name, content, want string }{
		{"no server", "token: mytoken\n" + svc, "loadfileconfig: server is required"},
		{"no token", "server: relay.example.com:7000\n" + svc, "loadfileconfig: exactly one of token or token_file is required"},
		{"neither, and no services", "{}\n", "loadfileconfig: server is required"},
		{"no server, both tokens", "token: a\ntoken_file: " + tok + "\n" + svc, "loadfileconfig: server is required"},
		{"both tokens", "server: relay.example.com:7000\ntoken: a\ntoken_file: " + tok + "\n" + svc, "loadfileconfig: only one of token or token_file may be set"},
		{"no services", "server: relay.example.com:7000\ntoken: a\n", "loadfileconfig: at least one service is required"},
	}
	for _, tc := range bad {
		yml := filepath.Join(dir, "burrow.yaml")
		os.WriteFile(yml, []byte(tc.content), 0o600)
		if _, err := LoadCompleteFileConfig(yml); err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}

	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\ntoken_file: "+tok+"\n"+svc), 0o600)
	c, err := LoadCompleteFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	// Only the line end is cut from a token file, as before.
	if c.Server != "relay.example.com:7000" || c.Token != "bur_abc " || len(c.Tunnels) != 1 {
		t.Fatalf("server %q, tunnels %d, token as before: %v", c.Server, len(c.Tunnels), c.Token == "bur_abc ")
	}
}

// services[].slug and .access are the wishes of `burrow http --slug --access`,
// with the same names and the same rules.
func TestLoadFileConfig_SlugAndAccess(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("services:\n"+
		"  - { name: app, local: 127.0.0.1:3000, type: http, slug: my-app, access: login }\n"+
		"  - { name: api, local: 127.0.0.1:3001, type: http, access: api-key }\n"+
		"  - { name: pub, local: 127.0.0.1:3002, type: http, slug: pub, access: open }\n"+
		"  - { name: plain, local: 127.0.0.1:3003, type: http }\n"+
		"  - { name: db, local: 127.0.0.1:5432 }\n"), 0o600)
	c, err := LoadFileConfig(yml)
	if err != nil {
		t.Fatal(err)
	}
	want := []TunnelSpec{
		{Name: "app", Type: "http", LocalAddr: "127.0.0.1:3000", Slug: "my-app", Access: "burrow_login"},
		{Name: "api", Type: "http", LocalAddr: "127.0.0.1:3001", Access: "api_key"},
		{Name: "pub", Type: "http", LocalAddr: "127.0.0.1:3002", Slug: "pub", Access: "open"},
		{Name: "plain", Type: "http", LocalAddr: "127.0.0.1:3003"},
		{Name: "db", Type: "tcp", LocalAddr: "127.0.0.1:5432"},
	}
	if len(c.Tunnels) != len(want) {
		t.Fatalf("tunnels: %+v", c.Tunnels)
	}
	for i := range want {
		if c.Tunnels[i] != want[i] {
			t.Errorf("service %d:\n got %+v\nwant %+v", i, c.Tunnels[i], want[i])
		}
	}

	bad := []struct{ name, service, want string }{
		{"slug with capitals", "{ name: app, local: 127.0.0.1:3000, type: http, slug: Bad_Slug }", `service[0] "app": slug must be 3 to 40 characters`},
		{"slug too short", "{ name: app, local: 127.0.0.1:3000, type: http, slug: ab }", `service[0] "app": slug must be 3 to 40 characters`},
		{"unknown access", "{ name: app, local: 127.0.0.1:3000, type: http, access: nonsense }", `service[0] "app": access must be one of: open, login, api-key`},
		{"the relay's name for an access mode", "{ name: app, local: 127.0.0.1:3000, type: http, access: burrow_login }", `access must be one of: open, login, api-key`},
		{"mtls", "{ name: app, local: 127.0.0.1:3000, type: http, access: mtls }", `access must be one of: open, login, api-key`},
		{"slug on tcp", "{ name: db, local: 127.0.0.1:5432, type: tcp, slug: my-db }", `service[0] "db": slug applies to http services`},
		{"access on tcp", "{ name: db, local: 127.0.0.1:5432, type: tcp, access: open }", `service[0] "db": access applies to http services`},
		{"slug on the default type", "{ name: db, local: 127.0.0.1:5432, slug: my-db }", `service[0] "db": slug applies to http services`},
	}
	for _, tc := range bad {
		os.WriteFile(yml, []byte("services:\n  - "+tc.service+"\n"), 0o600)
		if _, err := LoadFileConfig(yml); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// `burrow connect --config` reads the file as it always has: the two keys are
// not part of what it knows, so it neither checks nor sends them.
func TestLoadCompleteFileConfig_IgnoresSlugAndAccess(t *testing.T) {
	yml := filepath.Join(t.TempDir(), "burrow.yaml")
	for _, svc := range []string{
		"{ name: app, local: 127.0.0.1:3000, type: http, slug: my-app, access: login }",
		"{ name: app, local: 127.0.0.1:3000, type: http, slug: Bad_Slug, access: nonsense }",
		"{ name: app, local: 127.0.0.1:3000, slug: my-db, access: open }",
	} {
		os.WriteFile(yml, []byte("server: relay.example.com:7000\ntoken: bur_test_0000\nservices:\n  - "+svc+"\n"), 0o600)
		c, err := LoadCompleteFileConfig(yml)
		if err != nil {
			t.Fatalf("%s: %v", svc, err)
		}
		if len(c.Tunnels) != 1 || c.Tunnels[0].Slug != "" || c.Tunnels[0].Access != "" {
			t.Fatalf("%s: %+v", svc, c.Tunnels)
		}
	}
}

func TestAccessModeAndName(t *testing.T) {
	for cli, mode := range map[string]string{"open": "open", "login": "burrow_login", "api-key": "api_key"} {
		if got, ok := AccessMode(cli); !ok || got != mode {
			t.Errorf("AccessMode(%q) = %q, %v", cli, got, ok)
		}
		if got := AccessName(mode); got != cli {
			t.Errorf("AccessName(%q) = %q", mode, got)
		}
	}
	for _, bad := range []string{"", "burrow_login", "api_key", "mtls", "Open", " open"} {
		if _, ok := AccessMode(bad); ok {
			t.Errorf("AccessMode(%q) is accepted", bad)
		}
	}
	// A mode the client has no name for keeps the relay's.
	if got := AccessName("mtls"); got != "mtls" {
		t.Errorf("AccessName(mtls) = %q", got)
	}
}

// What token_file yields is sent to the relay as the token. A file that
// cannot hold one (several lines, a space inside, bytes outside printable
// ASCII) is refused before anything is sent, and none of it is shown.
func TestLoadFileConfig_TokenFileMustHoldAToken(t *testing.T) {
	const svc = "services:\n  - { name: app, local: 127.0.0.1:3000 }\n"
	const secret = "s3cretOfAnotherKind"
	bad := map[string]string{
		"several lines":  "root:" + secret + ":19000:0:99999:7:::\ndaemon:*:19000:0:99999:7:::\n",
		"a space inside": "user " + secret + "\n",
		"a tab inside":   "user\t" + secret + "\n",
		"binary":         secret + "\x00\x01\x02",
		"not ASCII":      secret + "ü\n",
	}
	loaders := map[string]func(string) (FileConfig, error){"up": LoadFileConfig, "connect --config": LoadCompleteFileConfig}
	for name, content := range bad {
		for which, load := range loaders {
			dir := t.TempDir()
			tok := filepath.Join(dir, "tok")
			os.WriteFile(tok, []byte(content), 0o600)
			yml := filepath.Join(dir, "burrow.yaml")
			os.WriteFile(yml, []byte("server: relay.example.com:7000\ntoken_file: "+tok+"\n"+svc), 0o600)
			c, err := load(yml)
			if err == nil || c.Token != "" {
				t.Fatalf("%s, %s: the file's content was taken as a token", name, which)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s, %s: the message shows the file's content", name, which)
			}
			want := `loadfileconfig: token_file "` + tok + `": the file does not hold a token`
			if err.Error() != want {
				t.Fatalf("%s, %s: err = %q, want %q", name, which, err, want)
			}
		}
	}

	// A token as the relay makes them, with the white space an editor leaves
	// around it, is taken as before by both.
	for which, load := range loaders {
		for _, content := range []string{
			"bur_Zm9vYmFyLWJhel9xdXV4MDEyMzQ1Njc4OWFiY2RlZmdoaWo\n",
			"bur_Zm9vYmFyLWJhel9xdXV4MDEyMzQ1Njc4OWFiY2RlZmdoaWo\r\n",
			"  bur_Zm9vYmFyLWJhel9xdXV4MDEyMzQ1Njc4OWFiY2RlZmdoaWo \n\n",
			"anything-printable!#$%&~\n",
		} {
			dir := t.TempDir()
			tok := filepath.Join(dir, "tok")
			os.WriteFile(tok, []byte(content), 0o600)
			yml := filepath.Join(dir, "burrow.yaml")
			os.WriteFile(yml, []byte("server: relay.example.com:7000\ntoken_file: "+tok+"\n"+svc), 0o600)
			c, err := load(yml)
			if err != nil || strings.TrimSpace(c.Token) != strings.TrimSpace(content) {
				t.Fatalf("%s, %q: err = %v", which, content, err)
			}
		}
	}

	// An empty file is no token; that is as it was: `up` goes on to the
	// sign-in, `connect --config` hands on the empty value.
	dir := t.TempDir()
	tok := filepath.Join(dir, "tok")
	os.WriteFile(tok, []byte("\n"), 0o600)
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("server: relay.example.com:7000\ntoken_file: "+tok+"\n"+svc), 0o600)
	for which, load := range loaders {
		if c, err := load(yml); err != nil || c.Token != "" {
			t.Fatalf("%s: err = %v", which, err)
		}
	}
}

func TestTokenFilePath(t *testing.T) {
	dir := t.TempDir()
	yml := filepath.Join(dir, "burrow.yaml")
	os.WriteFile(yml, []byte("token_file: /etc/burrow/token\nservices:\n  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	if p, err := TokenFilePath(yml); err != nil || p != "/etc/burrow/token" {
		t.Fatalf("%q, %v", p, err)
	}
	os.WriteFile(yml, []byte("services:\n  - { name: app, local: 127.0.0.1:3000 }\n"), 0o600)
	if p, err := TokenFilePath(yml); err != nil || p != "" {
		t.Fatalf("%q, %v", p, err)
	}
	if _, err := TokenFilePath(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("no error for a missing file")
	}
}
