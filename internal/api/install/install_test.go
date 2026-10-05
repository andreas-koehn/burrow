package install

import (
	"strings"
	"testing"
)

func goodParams() Params {
	return Params{
		Relay:     "https://burrow.example.com",
		Version:   "0.6.0",
		Channel:   ChannelRelease,
		ManualURL: "https://github.com/andreas-koehn/burrow/releases/tag/v0.6.0",
		Assets: []Asset{
			{OS: "linux", Arch: "amd64", Name: "burrow_linux_amd64_0.6.0.tar.gz"},
			{OS: "linux", Arch: "arm", Name: "burrow_linux_armv7_0.6.0.tar.gz"},
			{OS: "darwin", Arch: "arm64", Name: "burrow_darwin_arm64_0.6.0.tar.gz"},
			{OS: "windows", Arch: "amd64", Name: "burrow_windows_amd64_0.6.0.zip"},
		},
	}
}

// firstCodeLine is the first line that is neither blank nor a comment.
func firstCodeLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "#") {
			return line
		}
	}
	return ""
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1]
}

func TestShell_ShapeAndValues(t *testing.T) {
	out, err := Shell(goodParams())
	if err != nil {
		t.Fatalf("Shell: %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, "#!/bin/sh\n") {
		t.Fatalf("script does not start with #!/bin/sh: %q", s[:20])
	}
	if !strings.Contains(s, "\n{\n    set -eu\n") {
		t.Fatal("script lacks `set -eu`")
	}
	// All work happens in main, called on the very last line, so a download
	// cut off anywhere before it runs nothing.
	// The whole script is one brace group, opened before the first command
	// and closed on the last line: the shell runs nothing of a group it has
	// not read to its end, so no strict prefix of the file runs anything.
	if got := lastLine(s); got != "}" {
		t.Fatalf("last line = %q, want the closing brace of the outer group", got)
	}
	if !strings.HasSuffix(s, "\n    main \"$@\"\n}\n") {
		t.Fatal("the outer group does not end with the call of main")
	}
	if first := firstCodeLine(s); first != "{" {
		t.Fatalf("first line that is not a comment = %q, want {", first)
	}
	for _, want := range []string{
		"BURROW_INSTALL_ALLOW_HTTP",
		"--proto '=https' --proto-redir '=https' --tlsv1.2",
		"compromised",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	if !strings.HasSuffix(s, "\n") {
		t.Fatal("script does not end with a newline")
	}
	for _, want := range []string{
		`RELAY="https://burrow.example.com"`,
		`VERSION="0.6.0"`,
		`CHANNEL="release"`,
	} {
		if n := strings.Count(s, want); n != 1 {
			t.Fatalf("%s appears %d times, want 1", want, n)
		}
	}
	// The assignments come before main is defined.
	if strings.Index(s, `RELAY="`) > strings.Index(s, "main() {") {
		t.Fatal("RELAY is assigned after main()")
	}
	for _, want := range []string{
		"linux/amd64 burrow_linux_amd64_0.6.0.tar.gz\n",
		"linux/arm burrow_linux_armv7_0.6.0.tar.gz\n",
		"darwin/arm64 burrow_darwin_arm64_0.6.0.tar.gz\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks the asset line %q", want)
		}
	}
	if strings.Contains(s, "burrow_windows") {
		t.Fatal("the shell installer lists a Windows archive")
	}
	for _, banned := range []string{"eval ", "sudo sh -c", "{{", "}}", "pipefail", "local "} {
		for _, line := range strings.Split(s, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if strings.Contains(line, banned) {
				t.Fatalf("script contains %q: %s", banned, line)
			}
		}
	}
}

func TestPowerShell_ShapeAndValues(t *testing.T) {
	out, err := PowerShell(goodParams())
	if err != nil {
		t.Fatalf("PowerShell: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`$ErrorActionPreference = 'Stop'`,
		"Set-StrictMode -Version Latest",
		"Get-FileHash -Algorithm SHA256",
		"Invoke-WebRequest -UseBasicParsing",
		"Expand-Archive",
		`$Relay = 'https://burrow.example.com'`,
		`$Version = '0.6.0'`,
		`'windows/amd64' = 'burrow_windows_amd64_0.6.0.zip'`,
	} {
		n := strings.Count(s, want)
		if n == 0 || (strings.Contains(want, "' = '") || strings.HasPrefix(want, "$Relay") || strings.HasPrefix(want, "$Version")) && n != 1 {
			t.Fatalf("%s appears %d times", want, n)
		}
	}
	for _, want := range []string{"BURROW_INSTALL_ALLOW_HTTP", "DoNotExpandEnvironmentNames", "ExpandString", "compromised"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "GetEnvironmentVariable('Path', 'User')") {
		t.Fatal("the user Path is read expanded; %VAR% entries would be frozen")
	}
	// One script block, invoked as a whole: a cut-off copy does not parse.
	if got := lastLine(s); got != "}" {
		t.Fatalf("last line = %q, want the closing brace of the script block", got)
	}
	if !strings.HasSuffix(s, "\n    Install-Burrow\n}\n") {
		t.Fatal("the script block does not end with the call of Install-Burrow")
	}
	if first := firstCodeLine(s); first != "& {" {
		t.Fatalf("first line that is not a comment = %q, want & {", first)
	}
	for _, want := range []string{"BURROW_INSTALL_REFRESH", ".Dispose()", "GetValueKind"} {
		if !strings.Contains(s, want) {
			t.Fatalf("script lacks %q", want)
		}
	}
	if strings.Contains(s, "Set-ItemProperty") || strings.Contains(s, "sign out") {
		t.Fatal("the Path is stored without a broadcast, or the sign-out hint is still there")
	}
	if strings.Contains(s, "burrow_linux") || strings.Contains(s, "Invoke-Expression") || strings.Contains(s, "{{") {
		t.Fatal("PowerShell installer contains a foreign archive, Invoke-Expression or template residue")
	}
}

// Every value that reaches a script is checked against a narrow pattern. A
// value outside it is an error, never escaped and passed on.
func TestRender_RefusesHostileValues(t *testing.T) {
	hostile := []string{
		`https://x"; rm -rf ~; "`,
		"https://x$(id)",
		"https://x`id`",
		"https://x\nrm -rf ~",
		"https://x'; calc; '",
		"https://x y",
		"https://x\\",
		"https://x;id",
		"javascript:alert(1)",
		"https://user:pw@x",
		"https://x/path",
		"",
	}
	for _, v := range hostile {
		p := goodParams()
		p.Relay = v
		if _, err := Shell(p); err == nil {
			t.Errorf("Shell accepted relay %q", v)
		}
		if _, err := PowerShell(p); err == nil {
			t.Errorf("PowerShell accepted relay %q", v)
		}
	}
	for _, v := range []string{`0.6.0"; id; "`, "$(id)", "`id`", "0.6.0\nid", "0.6.0'", "", "a b"} {
		p := goodParams()
		p.Version = v
		if _, err := Shell(p); err == nil {
			t.Errorf("Shell accepted version %q", v)
		}
		if _, err := PowerShell(p); err == nil {
			t.Errorf("PowerShell accepted version %q", v)
		}
	}
	for _, v := range []string{"x\"", "$(id)", "a/b", "../x", "a b", "", "x'", "x\n"} {
		p := goodParams()
		p.Assets = []Asset{{OS: "linux", Arch: "amd64", Name: v}, {OS: "windows", Arch: "amd64", Name: v}}
		if _, err := Shell(p); err == nil {
			t.Errorf("Shell accepted asset name %q", v)
		}
		if _, err := PowerShell(p); err == nil {
			t.Errorf("PowerShell accepted asset name %q", v)
		}
		p = goodParams()
		p.Assets = []Asset{{OS: v, Arch: "amd64", Name: "a.tar.gz"}, {OS: "windows", Arch: v, Name: "a.zip"}}
		if _, err := Shell(p); err == nil {
			t.Errorf("Shell accepted os %q", v)
		}
		if _, err := PowerShell(p); err == nil {
			t.Errorf("PowerShell accepted arch %q", v)
		}
	}
	for _, v := range []string{`https://x/"`, "https://x/$(id)", "https://x/`id`", "https://x/\n", "https://x/'", "ftp://x/", "https://x/?a=b"} {
		p := goodParams()
		p.ManualURL = v
		if _, err := Shell(p); err == nil {
			t.Errorf("Shell accepted manual URL %q", v)
		}
		if _, err := PowerShell(p); err == nil {
			t.Errorf("PowerShell accepted manual URL %q", v)
		}
	}
	p := goodParams()
	p.Channel = "nightly"
	if _, err := Shell(p); err == nil {
		t.Error("Shell accepted an unknown channel")
	}
	// An empty manual URL is allowed: a relay that serves the archives itself
	// has no page to point at.
	p = goodParams()
	p.ManualURL = ""
	if _, err := Shell(p); err != nil {
		t.Errorf("empty manual URL: %v", err)
	}
}

func TestOrigin(t *testing.T) {
	good := map[string]string{
		"burrow.example.com":      "https://burrow.example.com",
		"burrow.example.com:8443": "https://burrow.example.com:8443",
		"127.0.0.1:8080":          "https://127.0.0.1:8080",
		"Relay-1":                 "https://Relay-1",
	}
	for host, want := range good {
		got, ok := Origin("https", host)
		if !ok || got != want {
			t.Errorf("Origin(https, %q) = %q, %v; want %q", host, got, ok, want)
		}
	}
	bad := []string{
		"", `x"; rm -rf ~; "`, "$(id)", "x`id`", "x\ny", "x y", "x/y", "x:", "x:port", "x:1:2",
		"[::1]:80", "x'", "x\\y", "x;y", "x:123456", strings.Repeat("a", 254), "-", ".", "..",
	}
	for _, host := range bad {
		if got, ok := Origin("https", host); ok {
			t.Errorf("Origin accepted host %q → %q", host, got)
		}
	}
	if _, ok := Origin("ftp", "burrow.example.com"); ok {
		t.Error("Origin accepted scheme ftp")
	}
	if got, ok := Origin("http", "relay:8080"); !ok || got != "http://relay:8080" {
		t.Errorf("Origin(http, relay:8080) = %q, %v", got, ok)
	}
}
