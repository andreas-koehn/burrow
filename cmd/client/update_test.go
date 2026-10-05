package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

// No test of this package may ever replace the test binary: a test that does
// not say which file stands in for the running binary has none.
func init() {
	executablePath = func() (string, error) {
		return "", errors.New("the test did not set the executable to replace")
	}
}

// fakeClient is a stand-in for the client binary: a shell script that
// answers `version` the way burrow does.
func fakeClient(v string) string {
	return "#!/bin/sh\n[ \"$#\" = 1 ] && [ \"$1\" = version ] || exit 64\necho 'burrow " + v + " (commit abc1234, built 2026-10-05, linux/amd64)'\n"
}

// clientArchive packs body as the client binary the way a release does.
func clientArchive(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range [][2]string{{"LICENSE", "text"}, {"burrow", body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(f[1]))}); err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(tw, f[1])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ownCertServer is an HTTPS server on this machine with a certificate of its
// own: nothing trusts it but the pool that is returned.
func ownCertServer(t *testing.T, h http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "release host"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return srv, pool
}

// updateFixture is a relay that runs relayVersion and hands out a client, the
// file that stands in for the running binary, and the directory temporary
// files go to.
type updateFixture struct {
	h       *harness
	rs      *relayServer
	exe     string // the file `burrow update` replaces
	exeDir  string
	tmp     string
	archive []byte
	// checksums is what the relay serves as checksums.txt.
	checksums string
	// override answers every request instead of the relay when set.
	override http.HandlerFunc
	// newClient is the binary in the archive.
	newClient string
	ca        string // the relay's certificate, for --cacert
	// releaseBase is the origin of another host the relay's redirects point
	// at; releaseHits counts what that host was asked.
	releaseBase string
	releaseHits atomic.Int32
}

const oldClient = "the old client"

func newUpdateFixture(t *testing.T, clientVersion, relayVersion string) *updateFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in client is a shell script")
	}
	f := &updateFixture{h: newHarness(t)}
	f.h.realDiscover = true
	asVersion(t, clientVersion)

	f.tmp = t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, f.tmp)
	}
	f.exeDir = t.TempDir()
	f.exe = filepath.Join(f.exeDir, "burrow")
	if err := os.WriteFile(f.exe, []byte(oldClient), 0o750); err != nil {
		t.Fatal(err)
	}
	prev := executablePath
	executablePath = func() (string, error) { return f.exe, nil }
	t.Cleanup(func() { executablePath = prev })

	f.newClient = fakeClient(relayVersion)
	f.archive = clientArchive(t, f.newClient)
	sum := sha256.Sum256(f.archive)
	f.checksums = hex.EncodeToString(sum[:]) + "  " + client.ArchiveName(relayVersion, runtime.GOOS, runtime.GOARCH) + "\n"

	f.rs = newRelayServer(t, func(w http.ResponseWriter, r *http.Request) {
		if f.override != nil {
			f.override(w, r)
			return
		}
		switch r.URL.Path {
		case client.DiscoveryPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"control":"127.0.0.1:7000","version":"`+relayVersion+`","min_client_version":"","protocol_version":1}`)
		case "/download/burrow/checksums.txt":
			http.Redirect(w, r, f.releaseBase+"/files/checksums.txt", http.StatusFound)
		case "/download/burrow/" + runtime.GOOS + "/" + runtime.GOARCH:
			http.Redirect(w, r, f.releaseBase+"/files/asset-0b5c", http.StatusFound)
		case "/files/checksums.txt":
			_, _ = io.WriteString(w, f.checksums)
		case "/files/asset-0b5c":
			_, _ = w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	})
	f.ca = f.rs.caFile(t)
	return f
}

// update runs `burrow update` with the relay's certificate trusted.
func (f *updateFixture) update(args ...string) int {
	f.h.t.Helper()
	return f.h.exec(append([]string{"update", "--cacert", f.ca}, args...)...)
}

// otherReleaseHost moves the files to a second server with a certificate of
// its own and makes pool what stands in for the system's roots. trusted says
// whether that pool knows the server.
func (f *updateFixture) otherReleaseHost(trusted bool) {
	t := f.h.t
	t.Helper()
	release, pool := ownCertServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.releaseHits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			_, _ = io.WriteString(w, f.checksums)
			return
		}
		_, _ = w.Write(f.archive)
	}))
	f.releaseBase = release.URL
	if !trusted {
		pool = x509.NewCertPool()
	}
	prev := releaseRootCAs
	releaseRootCAs = pool
	t.Cleanup(func() { releaseRootCAs = prev })
}

// downloads counts the requests for anything but discovery.
func (f *updateFixture) downloads() int {
	n := 0
	for _, r := range f.rs.requests() {
		if !strings.Contains(r, client.DiscoveryPath) {
			n++
		}
	}
	return n
}

// unchanged fails the test unless the binary is the old one, alone in its
// directory, and no temporary file is left.
func (f *updateFixture) unchanged() {
	f.h.t.Helper()
	got, err := os.ReadFile(f.exe)
	if err != nil {
		f.h.t.Fatal(err)
	}
	if string(got) != oldClient {
		f.h.t.Fatalf("the binary changed: %q", got)
	}
	f.clean()
}

// clean fails the test when something was left next to the binary or in the
// temporary directory.
func (f *updateFixture) clean() {
	f.h.t.Helper()
	for dir, want := range map[string]int{f.exeDir: 1, f.tmp: 0} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			f.h.t.Fatal(err)
		}
		if len(entries) != want {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			f.h.t.Fatalf("%s holds %v", dir, names)
		}
	}
}

// noCredentialsSent fails the test when a request carried the token or any
// other credential.
func (f *updateFixture) noCredentialsSent() {
	f.h.t.Helper()
	for _, r := range f.rs.requests() {
		low := strings.ToLower(r)
		if strings.Contains(r, testToken) || strings.Contains(low, "authorization") || strings.Contains(low, "cookie") {
			f.h.t.Fatal("a request carried credentials")
		}
	}
	f.h.noToken()
}

func TestUpdate_Check(t *testing.T) {
	t.Run("another version", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "0.6.0")
		if code := f.update("--check", f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		if got := f.h.stdout.String(); got != "burrow 0.5.0 → 0.6.0 is available. Run: burrow update\n" {
			t.Fatalf("stdout = %q", got)
		}
		if f.downloads() != 0 {
			t.Fatal("--check downloaded something")
		}
		f.unchanged()
	})
	t.Run("same version", func(t *testing.T) {
		f := newUpdateFixture(t, "v0.6.0", "0.6.0")
		if code := f.update("--check", f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		if got := f.h.stdout.String(); got != "burrow 0.6.0 is up to date\n" {
			t.Fatalf("stdout = %q", got)
		}
		f.unchanged()
	})
	t.Run("untagged relay", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "develop")
		if code := f.update("--check", f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		got := f.h.stdout.String()
		if !strings.Contains(got, "untagged build") || !strings.Contains(got, "cannot be compared") || !strings.Contains(got, "burrow update --force") {
			t.Fatalf("stdout = %q", got)
		}
		f.unchanged()
	})
	t.Run("relay unreachable", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "0.6.0")
		url, ca := f.rs.URL, f.rs.caFile(t)
		f.rs.Close()
		code := f.h.exec("update", "--check", "--cacert", ca, url)
		if code != 5 || !strings.Contains(f.h.stderr.String(), "Cannot reach "+url) || f.h.stdout.Len() != 0 {
			t.Fatalf("exit %d, stderr %q, stdout %q", code, f.h.stderr.String(), f.h.stdout.String())
		}
		f.unchanged()
	})
	t.Run("certificate not trusted", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "0.6.0")
		code := f.h.exec("update", "--check", f.rs.URL)
		if code != 5 || !strings.Contains(f.h.stderr.String(), "--cacert") {
			t.Fatalf("exit %d, stderr %q", code, f.h.stderr.String())
		}
		f.unchanged()
	})
}

func TestUpdate_ReplacesTheBinary(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	if code := f.update(f.rs.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, f.h.stderr.String())
	}
	if !strings.HasSuffix(f.h.stdout.String(), "Updated burrow 0.5.0 → 0.6.0\n") {
		t.Fatalf("stdout = %q", f.h.stdout.String())
	}
	got, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != f.newClient {
		t.Fatalf("the binary holds %q", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(f.exe)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o750 {
			t.Fatalf("mode %v, want 0750", st.Mode().Perm())
		}
	}
	f.clean()
	f.noCredentialsSent()
}

func TestUpdate_UpToDateDownloadsNothing(t *testing.T) {
	f := newUpdateFixture(t, "0.6.0", "v0.6.0")
	if code := f.update(f.rs.URL); code != 0 {
		t.Fatalf("exit %d: %s", code, f.h.stderr.String())
	}
	if got := f.h.stdout.String(); got != "burrow 0.6.0 is up to date\n" {
		t.Fatalf("stdout = %q", got)
	}
	if f.downloads() != 0 {
		t.Fatal("something was downloaded")
	}
	f.unchanged()
}

// The stored sign-in says where the relay is and nothing more: the token
// stays where it is (ruling 3).
func TestUpdate_UsesOnlyTheRelayOfTheSignIn(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	c := client.UserConfig{Relay: f.rs.URL, Control: "127.0.0.1:7000", Token: testToken, TokenName: "kohns-laptop"}
	if err := client.SaveUserConfig(f.h.cfgPath, c); err != nil {
		t.Fatal(err)
	}
	// Not the relay of the sign-in, and no way to it: must play no part.
	f.h.env["BURROW_SERVER"], f.h.env["BURROW_TOKEN"] = "other.example.com:7000", testToken
	if code := f.update(); code != 0 {
		t.Fatalf("exit %d: %s", code, f.h.stderr.String())
	}
	if got, _ := os.ReadFile(f.exe); string(got) != f.newClient {
		t.Fatalf("the binary holds %q", got)
	}
	f.clean()
	f.noCredentialsSent()
	if len(f.h.runs) != 0 {
		t.Fatal("update opened a control connection")
	}
}

func TestUpdate_NotSignedInAndNoRelay(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	code := f.update()
	errOut := f.h.stderr.String()
	if code != 3 || !strings.HasPrefix(errOut, "Not signed in. Run: burrow login <your relay address>\n") ||
		!strings.Contains(errOut, "burrow update <relay>") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if len(f.rs.requests()) != 0 {
		t.Fatal("a relay was asked")
	}
	f.unchanged()
}

func TestUpdate_Usage(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	for _, args := range [][]string{
		{"a.example.com", "b.example.com"},
		{"http://burrow.example.com"},
		{"https://user:pw@burrow.example.com"},
		{"--nope"},
	} {
		if code := f.update(args...); code != 2 {
			t.Errorf("update %v: exit %d, stderr %q", args, code, f.h.stderr.String())
		}
		if strings.Contains(f.h.stderr.String(), "pw@") {
			t.Errorf("update %v repeats credentials", args)
		}
	}
	f.unchanged()
}

func TestUpdate_DirectoryNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a user that a directory mode can stop")
	}
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	if err := os.Chmod(f.exeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.exeDir, 0o755) })

	code := f.update(f.rs.URL)
	errOut := f.h.stderr.String()
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{f.exeDir, "not writable", "  sudo " + f.exe + " update " + f.rs.URL + " --cacert " + f.ca + "\n"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr does not contain %q:\n%s", want, errOut)
		}
	}
	if f.downloads() != 0 {
		t.Fatal("the client was downloaded although it cannot be installed")
	}
	f.unchanged()
}

func TestNotWritableMessage(t *testing.T) {
	const relay = "https://burrow.example.com"
	sys := notWritableMessage("linux", "/usr/local/bin/burrow", relay, nil)
	for _, want := range []string{
		"/usr/local/bin is not writable by this user",
		"  sudo /usr/local/bin/burrow update https://burrow.example.com\n",
		"  curl -fsSL https://burrow.example.com/install.sh | sudo sh -s -- --system",
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("message does not contain %q:\n%s", want, sys)
		}
	}
	// Somewhere else: the installer would not put it there, so it is not named.
	opt := notWritableMessage("darwin", "/opt/my tools/burrow", relay, []string{"--cacert", "/etc/my ca.pem", "--insecure"})
	if !strings.Contains(opt, "  sudo '/opt/my tools/burrow' update https://burrow.example.com --cacert '/etc/my ca.pem' --insecure") || strings.Contains(opt, "install.sh") {
		t.Errorf("message:\n%s", opt)
	}
	win := notWritableMessage("windows", `C:\Program Files\burrow\burrow.exe`, relay, nil)
	if strings.Contains(win, "sudo") || !strings.Contains(win, "as administrator") ||
		!strings.Contains(win, "irm https://burrow.example.com/install.ps1 | iex") {
		t.Errorf("message:\n%s", win)
	}
}

// The command to run instead repeats what the user gave on the command line.
func TestUpdate_HintRepeatsTheFlags(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("needs a user that a directory mode can stop")
	}
	f := newUpdateFixture(t, "0.5.0", "develop")
	if err := os.Chmod(f.exeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.exeDir, 0o755) })
	code := f.update(f.rs.URL, "--force", "--server-name", "example.com", "--insecure")
	want := "  sudo " + f.exe + " update " + f.rs.URL + " --cacert " + f.ca + " --server-name example.com --insecure --force\n"
	if code != 1 || !strings.Contains(f.h.stderr.String(), want) {
		t.Fatalf("exit %d, stderr:\n%s\nwant the line:\n%s", code, f.h.stderr.String(), want)
	}
	f.unchanged()
	f.h.noToken()
}

func TestUpdate_Downgrade(t *testing.T) {
	t.Run("check", func(t *testing.T) {
		f := newUpdateFixture(t, "0.10.0", "0.9.2")
		if code := f.update("--check", f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		got := f.h.stdout.String()
		if !strings.Contains(got, "burrow 0.10.0 → 0.9.2") || !strings.Contains(got, "downgrade") || !strings.Contains(got, "Run: burrow update") {
			t.Fatalf("stdout = %q", got)
		}
		f.unchanged()
	})
	t.Run("update", func(t *testing.T) {
		f := newUpdateFixture(t, "0.10.0", "0.9.2")
		if code := f.update(f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		got := f.h.stdout.String()
		if !strings.Contains(got, "0.10.0 → 0.9.2") || !strings.Contains(strings.ToLower(got), "downgrade") {
			t.Fatalf("stdout = %q", got)
		}
		if b, _ := os.ReadFile(f.exe); string(b) != f.newClient {
			t.Fatalf("the binary holds %q", b)
		}
		f.clean()
	})
	t.Run("an upgrade is not called one", func(t *testing.T) {
		f := newUpdateFixture(t, "0.9.2", "0.10.0")
		if code := f.update(f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		if strings.Contains(strings.ToLower(f.h.stdout.String()), "downgrade") {
			t.Fatalf("stdout = %q", f.h.stdout.String())
		}
	})
}

// --cacert, --server-name and --insecure are about the relay. The release
// host behind its redirects is checked against the system's roots regardless.
func TestUpdate_ReleaseHostKeepsDefaultTrust(t *testing.T) {
	t.Run("relay with its own CA, release host trusted by the system", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "0.6.0")
		f.otherReleaseHost(true)
		if code := f.update(f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		if got, _ := os.ReadFile(f.exe); string(got) != f.newClient {
			t.Fatalf("the binary holds %q", got)
		}
		if f.releaseHits.Load() != 2 {
			t.Fatalf("the release host was asked %d times", f.releaseHits.Load())
		}
		f.clean()
	})
	for _, flags := range [][]string{{"--insecure"}, nil} {
		t.Run("untrusted release host is refused "+strings.Join(flags, " "), func(t *testing.T) {
			f := newUpdateFixture(t, "0.5.0", "0.6.0")
			f.otherReleaseHost(false)
			code := f.update(append([]string{f.rs.URL}, flags...)...)
			errOut := f.h.stderr.String()
			if code != 5 || !strings.Contains(errOut, "burrow was not updated") || !strings.Contains(errOut, "certificate is not trusted") {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			// --cacert is for the relay; it is no fix for the release host.
			if strings.Contains(errOut, "--cacert <ca.pem>") {
				t.Fatalf("stderr points at --cacert: %q", errOut)
			}
			if f.releaseHits.Load() != 0 {
				t.Fatal("the release host was asked over an unchecked connection")
			}
			f.unchanged()
		})
	}
}

// The downloaded binary is run once with `version` before it replaces the
// old one.
func TestUpdate_RunCheck(t *testing.T) {
	cases := map[string]string{
		"reports another version": fakeClient("0.5.9"),
		"does not start":          "the new client",
		"fails":                   "#!/bin/sh\nexit 1\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newUpdateFixture(t, "0.5.0", "0.6.0")
			f.archive = clientArchive(t, body)
			sum := sha256.Sum256(f.archive)
			f.checksums = hex.EncodeToString(sum[:]) + "  " + client.ArchiveName("0.6.0", runtime.GOOS, runtime.GOARCH) + "\n"
			code := f.update(f.rs.URL)
			errOut := f.h.stderr.String()
			if code != 1 || !strings.Contains(errOut, "burrow was not updated") || !strings.Contains(errOut, "run check") ||
				strings.Contains(errOut, "not writable") {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			f.unchanged()
		})
	}
}

func TestUpdate_HelpListsTheExitCodes(t *testing.T) {
	h := newHarness(t)
	if code := h.exec("update", "--help"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"0 when", "1 when", "3 when", "5 when"} {
		if !strings.Contains(h.stdout.String(), want) {
			t.Errorf("help does not say %q", want)
		}
	}
}

func TestUpdate_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		set  func(f *updateFixture)
		want string
	}{
		{"checksum mismatch", func(f *updateFixture) {
			f.checksums = strings.Repeat("0", 64) + "  " + client.ArchiveName("0.6.0", runtime.GOOS, runtime.GOARCH) + "\n"
		}, "does not match its checksum"},
		{"no checksum line", func(f *updateFixture) {
			f.checksums = strings.Repeat("0", 64) + "  burrow_other.tar.gz\n"
		}, "no line for"},
		{"an HTML page instead of the archive", func(f *updateFixture) {
			f.archive = []byte("<!doctype html><html><body>Sign in</body></html>")
		}, "does not match its checksum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUpdateFixture(t, "0.5.0", "0.6.0")
			tc.set(f)
			code := f.update(f.rs.URL)
			errOut := f.h.stderr.String()
			if code != 1 || !strings.Contains(errOut, "burrow was not updated") || !strings.Contains(errOut, tc.want) {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			if strings.Contains(f.h.stdout.String(), "Updated") {
				t.Fatalf("stdout = %q", f.h.stdout.String())
			}
			f.unchanged()
		})
	}
}

func TestUpdate_NoBuildForThisPlatform(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	f.override = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case client.DiscoveryPath:
			_, _ = io.WriteString(w, `{"control":"127.0.0.1:7000","version":"0.6.0","protocol_version":1}`)
		case "/download/burrow/checksums.txt":
			_, _ = io.WriteString(w, f.checksums)
		default:
			http.NotFound(w, r)
		}
	}
	code := f.update(f.rs.URL)
	if code != 1 || !strings.Contains(f.h.stderr.String(), runtime.GOOS+"/"+runtime.GOARCH) {
		t.Fatalf("exit %d, stderr %q", code, f.h.stderr.String())
	}
	f.unchanged()
}

func TestUpdate_UntaggedRelayNeedsForce(t *testing.T) {
	t.Run("without --force", func(t *testing.T) {
		f := newUpdateFixture(t, "0.5.0", "develop")
		code := f.update(f.rs.URL)
		errOut := f.h.stderr.String()
		if code != 1 || !strings.Contains(errOut, "untagged build") || !strings.Contains(errOut, "cannot be compared") ||
			!strings.Contains(errOut, "burrow update --force") {
			t.Fatalf("exit %d, stderr %q", code, errOut)
		}
		if f.downloads() != 0 {
			t.Fatal("something was downloaded")
		}
		f.unchanged()
	})
	t.Run("with --force", func(t *testing.T) {
		f := newUpdateFixture(t, "develop", "develop")
		if code := f.update("--force", f.rs.URL); code != 0 {
			t.Fatalf("exit %d: %s", code, f.h.stderr.String())
		}
		if !strings.HasSuffix(f.h.stdout.String(), "Updated burrow develop → develop\n") {
			t.Fatalf("stdout = %q", f.h.stdout.String())
		}
		if got, _ := os.ReadFile(f.exe); string(got) != f.newClient {
			t.Fatalf("the binary holds %q", got)
		}
		f.clean()
	})
}

// An older relay has neither discovery nor downloads.
func TestUpdate_RelayWithoutDiscovery(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	f.override = http.NotFound
	code := f.update(f.rs.URL)
	if code != 1 || !strings.Contains(f.h.stderr.String(), "does not hand out the client") {
		t.Fatalf("exit %d, stderr %q", code, f.h.stderr.String())
	}
	f.unchanged()
}

// There is no automatic update: a command that learns of a newer relay says
// so at most, and downloads and replaces nothing.
func TestUpdate_OnlyWhenAsked(t *testing.T) {
	f := newUpdateFixture(t, "0.5.0", "0.6.0")
	c := client.UserConfig{Relay: f.rs.URL, Control: "127.0.0.1:7000", Token: testToken, TokenName: "kohns-laptop"}
	if err := client.SaveUserConfig(f.h.cfgPath, c); err != nil {
		t.Fatal(err)
	}
	ca := f.rs.caFile(t)
	for _, args := range [][]string{
		{"status", "--cacert", ca},
		{"version"},
		{"http", "3000", "--cacert", ca},
		{"login", f.rs.URL, "--token", testToken, "--force", "--cacert", ca},
	} {
		f.h.exec(args...)
		if f.downloads() != 0 {
			t.Fatalf("burrow %s downloaded something", args[0])
		}
	}
	f.unchanged()
}

func TestRemoveReplacedAtStart(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "burrow.exe")
	for _, p := range []string{exe, exe + ".old"} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	prev := executablePath
	executablePath = func() (string, error) { return exe, nil }
	t.Cleanup(func() { executablePath = prev })

	removeReplacedAtStart("linux")
	if _, err := os.Stat(exe + ".old"); err != nil {
		t.Fatal("a file was removed on a system that leaves none behind")
	}
	removeReplacedAtStart("windows")
	if _, err := os.Stat(exe + ".old"); !os.IsNotExist(err) {
		t.Fatalf("the leftover is still there: %v", err)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatal(err)
	}
}
