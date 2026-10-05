package client

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// tarEntry is one entry of a test archive.
type tarEntry struct {
	name string
	body string
	typ  byte   // tar type flag; 0 stands for a regular file
	link string // target of a link entry
}

func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o755, Linkname: e.link}
		if typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipEntry is one entry of a test zip archive.
type zipEntry struct {
	name string
	body string
	mode fs.FileMode // 0 stands for a regular file
}

func zipOf(t *testing.T, entries ...zipEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := e.mode
		if mode == 0 {
			mode = 0o755
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sumLine(archive []byte, name string) string {
	s := sha256.Sum256(archive)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

// updateRelay stands in for a relay and for the release host behind it: the
// relay's /download/ addresses redirect to /files/, which serves the files.
type updateRelay struct {
	*httptest.Server
	mu        sync.Mutex
	seen      []string // method, URL and headers of every request
	archive   []byte
	checksums string
	// archiveHandler answers the archive's /files/ address when set.
	archiveHandler http.HandlerFunc
	// redirect answers the relay's archive address when set.
	redirect http.HandlerFunc
	// direct serves the files at the relay's own addresses, without a redirect.
	direct bool
}

func newUpdateRelay(t *testing.T, tlsServer bool) *updateRelay {
	t.Helper()
	ur := &updateRelay{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var sb strings.Builder
		sb.WriteString(r.Method + " " + r.URL.String() + "\n")
		_ = r.Header.Write(&sb)
		ur.mu.Lock()
		ur.seen = append(ur.seen, sb.String())
		ur.mu.Unlock()
		switch {
		case r.URL.Path == "/download/burrow/checksums.txt":
			if ur.direct {
				_, _ = io.WriteString(w, ur.checksums)
				return
			}
			http.Redirect(w, r, "/files/v0.6.0/checksums.txt", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/download/burrow/"):
			if ur.redirect != nil {
				ur.redirect(w, r)
				return
			}
			pair := strings.TrimPrefix(r.URL.Path, "/download/burrow/")
			if pair != "linux/amd64" && pair != "windows/amd64" && pair != "linux/arm" {
				http.Error(w, "This relay has no burrow download at this address.", http.StatusNotFound)
				return
			}
			if ur.direct {
				_, _ = w.Write(ur.archive)
				return
			}
			// Like a release host: the address the file is finally served
			// from does not end in the file's name.
			http.Redirect(w, r, "/files/v0.6.0/0b5c2a1e-asset", http.StatusFound)
		case r.URL.Path == "/files/v0.6.0/checksums.txt":
			_, _ = io.WriteString(w, ur.checksums)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			if ur.archiveHandler != nil {
				ur.archiveHandler(w, r)
				return
			}
			_, _ = w.Write(ur.archive)
		default:
			http.NotFound(w, r)
		}
	})
	if tlsServer {
		ur.Server = httptest.NewTLSServer(h)
	} else {
		ur.Server = httptest.NewServer(h)
	}
	t.Cleanup(ur.Close)
	return ur
}

func (ur *updateRelay) requests() []string {
	ur.mu.Lock()
	defer ur.mu.Unlock()
	return append([]string(nil), ur.seen...)
}

func (ur *updateRelay) source(goos, goarch string) UpdateSource {
	return UpdateSource{HTTP: ur.Client(), Relay: ur.URL, Version: "0.6.0", OS: goos, Arch: goarch}
}

// tempRoot makes the test's own directory the place for temporary files, so
// that a test can see what a failed update left behind.
func tempRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func mustBeEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("left behind in the temporary directory: %v", names)
	}
}

const (
	linuxArchive   = "burrow_linux_amd64_0.6.0.tar.gz"
	windowsArchive = "burrow_windows_amd64_0.6.0.zip"
)

func TestArchiveName(t *testing.T) {
	cases := []struct{ version, goos, goarch, want string }{
		{"0.6.0", "linux", "amd64", "burrow_linux_amd64_0.6.0.tar.gz"},
		{"v0.6.0", "linux", "arm", "burrow_linux_armv7_0.6.0.tar.gz"},
		{"1.2.3", "windows", "386", "burrow_windows_386_1.2.3.zip"},
		{"1.2.3", "darwin", "arm64", "burrow_darwin_arm64_1.2.3.tar.gz"},
		{"develop", "linux", "arm64", "burrow_linux_arm64.tar.gz"},
		{"develop", "windows", "amd64", "burrow_windows_amd64.zip"},
		{"0.6.0-12-gabc", "linux", "amd64", "burrow_linux_amd64.tar.gz"},
		{"0.6.0-rc1", "darwin", "arm64", "burrow_darwin_arm64.tar.gz"},
		{"", "linux", "amd64", "burrow_linux_amd64.tar.gz"},
	}
	for _, c := range cases {
		if got := ArchiveName(c.version, c.goos, c.goarch); got != c.want {
			t.Errorf("ArchiveName(%q, %s, %s) = %q, want %q", c.version, c.goos, c.goarch, got, c.want)
		}
	}
}

func TestReleaseVersion(t *testing.T) {
	for in, want := range map[string]string{"0.6.0": "0.6.0", "v1.20.3": "1.20.3"} {
		if got, ok := ReleaseVersion(in); !ok || got != want {
			t.Errorf("ReleaseVersion(%q) = %q, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "dev", "develop", "0.6", "0.6.0-rc1", "0.6.0-12-gabc", "0.6.0-dirty", "1.2.3.4", "v", "0.6.0\n"} {
		if _, ok := ReleaseVersion(in); ok {
			t.Errorf("ReleaseVersion(%q) is a release", in)
		}
	}
}

func TestFetch_TarGz(t *testing.T) {
	tmp := tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.archive = tarGz(t,
		tarEntry{name: "LICENSE", body: "text"},
		tarEntry{name: "burrow", body: "new client"},
		tarEntry{name: "docs/", typ: tar.TypeDir},
		tarEntry{name: "docs/burrow", body: "not the client"},
	)
	ur.checksums = "0000000000000000000000000000000000000000000000000000000000000000  other.tar.gz\n" +
		sumLine(ur.archive, linuxArchive)

	bin, cleanup, err := ur.source("linux", "amd64").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new client" {
		t.Fatalf("extracted %q", got)
	}
	if filepath.Base(bin) != "burrow" {
		t.Fatalf("extracted to %s", bin)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o100 == 0 {
			t.Fatalf("the extracted file is not executable: %v", st.Mode())
		}
	}
	cleanup()
	mustBeEmpty(t, tmp)

	// Nothing that could identify the user travels with the downloads.
	reqs := ur.requests()
	if len(reqs) != 4 {
		t.Fatalf("%d requests, want 4: %v", len(reqs), reqs)
	}
	for _, r := range reqs {
		low := strings.ToLower(r)
		if strings.Contains(low, "authorization") || strings.Contains(low, "cookie") || strings.Contains(low, "bur_") {
			t.Fatalf("a download request carries credentials:\n%s", r)
		}
	}
}

func TestFetch_Zip(t *testing.T) {
	tmp := tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.archive = zipOf(t, zipEntry{name: "README.md", body: "text"}, zipEntry{name: "burrow.exe", body: "new windows client"})
	ur.checksums = sumLine(ur.archive, windowsArchive)

	bin, cleanup, err := ur.source("windows", "amd64").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new windows client" || filepath.Base(bin) != "burrow.exe" {
		t.Fatalf("extracted %q to %s", got, bin)
	}
	cleanup()
	mustBeEmpty(t, tmp)
}

// A relay that serves the files itself (client_download_dir) answers at its
// own addresses; the name in checksums.txt is still the archive's.
func TestFetch_ServedByTheRelayItself(t *testing.T) {
	tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.direct = true
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"})
	ur.checksums = sumLine(ur.archive, "burrow_linux_armv7_0.6.0.tar.gz")
	bin, cleanup, err := ur.source("linux", "arm").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, _ := os.ReadFile(bin); string(got) != "new client" {
		t.Fatalf("extracted %q", got)
	}
}

// The checksum line with the binary marker sha256sum -b writes.
func TestFetch_ChecksumLineWithStar(t *testing.T) {
	tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"})
	s := sha256.Sum256(ur.archive)
	ur.checksums = strings.ToUpper(hex.EncodeToString(s[:])) + " *" + linuxArchive // no newline at the end
	_, cleanup, err := ur.source("linux", "amd64").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestFetch_Refused(t *testing.T) {
	good := func(t *testing.T) []byte { return tarGz(t, tarEntry{name: "burrow", body: "new client"}) }
	cases := []struct {
		name string
		// set prepares the relay; it gets a good archive and its checksum line.
		set          func(t *testing.T, ur *updateRelay)
		goos         string
		wantChecksum bool
		wantText     string
	}{
		{
			name: "checksum mismatch",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = sumLine([]byte("something else"), linuxArchive)
			},
			wantChecksum: true,
		},
		{
			name: "no checksum line for the archive",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = sumLine(ur.archive, "burrow_linux_amd64_0.6.0.tar.gz.sig") +
					sumLine(ur.archive, "xburrow_linux_amd64_0.6.0.tar.gz") +
					sumLine(ur.archive, "burrow_linux_arm64_0.6.0.tar.gz")
			},
			wantChecksum: true,
		},
		{
			name: "two different checksum lines for the archive",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = sumLine(ur.archive, linuxArchive) + sumLine([]byte("other"), linuxArchive)
			},
			wantChecksum: true,
		},
		{
			name: "checksum line that holds no SHA-256",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = "abc123  " + linuxArchive + "\n"
			},
			wantChecksum: true,
		},
		{
			name: "an HTML page instead of the checksums",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = "<!doctype html><html><body>Sign in</body></html>\n"
			},
			wantChecksum: true,
		},
		{
			name: "an HTML page instead of the archive",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<!doctype html><html><body>Sign in</body></html>")
				}
			},
			wantChecksum: true,
		},
		{
			name: "truncated body",
			set: func(t *testing.T, ur *updateRelay) {
				full := ur.archive
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Length", "100000")
					_, _ = w.Write(full[:len(full)/2])
				}
			},
		},
		{
			name: "truncated body without a length",
			set: func(t *testing.T, ur *updateRelay) {
				full := ur.archive
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.(http.Flusher).Flush()
					_, _ = w.Write(full[:len(full)/2])
				}
			},
			wantChecksum: true,
		},
		{
			name: "oversized download by its length",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Length", "5000")
					_, _ = w.Write(bytes.Repeat([]byte("x"), 5000))
				}
			},
			wantText: "too large",
		},
		{
			name: "oversized download without a length",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					w.(http.Flusher).Flush()
					_, _ = w.Write(bytes.Repeat([]byte("x"), 5000))
				}
			},
			wantText: "too large",
		},
		{
			name: "oversized checksums",
			set: func(t *testing.T, ur *updateRelay) {
				ur.checksums = strings.Repeat("x", maxChecksumsBytes+1)
			},
			wantText: "too large",
		},
		{
			name: "binary larger than allowed",
			set: func(t *testing.T, ur *updateRelay) {
				// Compresses to far less than the archive limit.
				ur.archive = tarGz(t, tarEntry{name: "burrow", body: strings.Repeat("a", 3000)})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "too large",
		},
		{
			name: "other content larger than allowed",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "padding", body: strings.Repeat("a", 9000)}, tarEntry{name: "burrow", body: "new client"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "too large",
		},
		{
			name: "entry with ..",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"}, tarEntry{name: "../evil", body: "x"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "entry with .. inside",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "a/../../burrow", body: "x"}, tarEntry{name: "burrow", body: "new client"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "absolute entry",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "/usr/local/bin/burrow", body: "x"}, tarEntry{name: "burrow", body: "new client"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "symlink entry",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "burrow", typ: tar.TypeSymlink, link: "/bin/sh"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "symlink entry next to the binary",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"}, tarEntry{name: "link", typ: tar.TypeSymlink, link: "burrow"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "hard link entry",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "burrow", typ: tar.TypeLink, link: "/bin/sh"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "the binary twice",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "burrow", body: "one"}, tarEntry{name: "./burrow", body: "two"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "more than once",
		},
		{
			name: "no binary at the archive root",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = tarGz(t, tarEntry{name: "bin/burrow", body: "x"}, tarEntry{name: "burrow.exe", body: "x"})
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "holds no",
		},
		{
			name: "not an archive, checksum right",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = []byte("this is not gzip")
				ur.checksums = sumLine(ur.archive, linuxArchive)
			},
			wantText: "cannot be unpacked",
		},
		{
			name: "zip entry with ..",
			goos: "windows",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = zipOf(t, zipEntry{name: "burrow.exe", body: "new"}, zipEntry{name: "../evil.exe", body: "x"})
				ur.checksums = sumLine(ur.archive, windowsArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "zip entry with a backslash path",
			goos: "windows",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = zipOf(t, zipEntry{name: `..\burrow.exe`, body: "x"}, zipEntry{name: "burrow.exe", body: "new"})
				ur.checksums = sumLine(ur.archive, windowsArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "zip absolute entry",
			goos: "windows",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = zipOf(t, zipEntry{name: "/burrow.exe", body: "x"})
				ur.checksums = sumLine(ur.archive, windowsArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "zip symlink entry",
			goos: "windows",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = zipOf(t, zipEntry{name: "burrow.exe", body: "C:/Windows/System32/cmd.exe", mode: fs.ModeSymlink | 0o777})
				ur.checksums = sumLine(ur.archive, windowsArchive)
			},
			wantText: "unsafe",
		},
		{
			name: "zip binary larger than allowed",
			goos: "windows",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archive = zipOf(t, zipEntry{name: "burrow.exe", body: strings.Repeat("a", 3000)})
				ur.checksums = sumLine(ur.archive, windowsArchive)
			},
			wantText: "too large",
		},
		{
			name: "redirect to plain http",
			set: func(t *testing.T, ur *updateRelay) {
				plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					t.Error("the plain-HTTP address was asked")
					_, _ = w.Write(ur.archive)
				}))
				t.Cleanup(plain.Close)
				ur.redirect = func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, plain.URL+"/"+linuxArchive, http.StatusFound)
				}
			},
			wantText: "redirect",
		},
		{
			name: "redirect that carries credentials",
			set: func(t *testing.T, ur *updateRelay) {
				target := strings.Replace(ur.URL, "https://", "https://user:secret@", 1)
				ur.redirect = func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target+"/files/v0.6.0/x", http.StatusFound)
				}
			},
			wantText: "redirect",
		},
		{
			name: "redirect loop",
			set: func(t *testing.T, ur *updateRelay) {
				ur.redirect = func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, r.URL.Path, http.StatusFound)
				}
			},
			wantText: "redirect",
		},
		{
			name: "server error",
			set: func(t *testing.T, ur *updateRelay) {
				ur.archiveHandler = func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, "boom", http.StatusBadGateway)
				}
			},
			wantText: "502",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := tempRoot(t)
			shrinkLimits(t, 4096, 2048)
			ur := newUpdateRelay(t, true)
			ur.archive = good(t)
			goos, name := "linux", linuxArchive
			if tc.goos == "windows" {
				goos, name = "windows", windowsArchive
				ur.archive = zipOf(t, zipEntry{name: "burrow.exe", body: "new"})
			}
			ur.checksums = sumLine(ur.archive, name)
			tc.set(t, ur)

			bin, cleanup, err := ur.source(goos, "amd64").Fetch(context.Background())
			if err == nil {
				cleanup()
				t.Fatalf("no error; extracted %s", bin)
			}
			if bin != "" {
				t.Fatalf("a path came back next to the error: %s", bin)
			}
			if tc.wantChecksum != errors.Is(err, ErrChecksum) {
				t.Fatalf("errors.Is(err, ErrChecksum) = %v, want %v: %v", !tc.wantChecksum, tc.wantChecksum, err)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("error %q does not contain %q", err, tc.wantText)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the error repeats credentials: %v", err)
			}
			cleanup() // harmless after a failure
			mustBeEmpty(t, tmp)
		})
	}
}

// shrinkLimits makes the size limits small enough to be reached in a test.
func shrinkLimits(t *testing.T, archive, binary int64) {
	t.Helper()
	a, b := maxArchiveBytes, maxBinaryBytes
	maxArchiveBytes, maxBinaryBytes = archive, binary
	t.Cleanup(func() { maxArchiveBytes, maxBinaryBytes = a, b })
}

func TestFetch_NoBuildForThePlatform(t *testing.T) {
	tmp := tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "x"})
	ur.checksums = sumLine(ur.archive, "burrow_plan9_amd64_0.6.0.tar.gz")
	_, _, err := ur.source("plan9", "amd64").Fetch(context.Background())
	if !errors.Is(err, ErrNoBuild) || !strings.Contains(err.Error(), "plan9/amd64") {
		t.Fatalf("err = %v", err)
	}
	mustBeEmpty(t, tmp)
}

func TestFetch_RelayAddress(t *testing.T) {
	tempRoot(t)
	for _, relay := range []string{
		"", "burrow.example.com", "ftp://burrow.example.com", "http://burrow.example.com",
		"http://127.0.0.1.example.com", "https://user:pw@burrow.example.com", "https://burrow.example.com/path",
		"https://burrow.example.com?x=1", "http://10.0.0.1:8080",
	} {
		s := UpdateSource{Relay: relay, Version: "0.6.0", OS: "linux", Arch: "amd64"}
		if _, _, err := s.Fetch(context.Background()); err == nil || strings.Contains(err.Error(), "pw") {
			t.Errorf("relay %q: err = %v", relay, err)
		}
	}
	// Nothing of a platform name ends up in a path.
	for _, pair := range [][2]string{{"../x", "amd64"}, {"linux", "amd64/../.."}, {"", "amd64"}, {"linux", ""}, {"Linux", "amd64"}} {
		s := UpdateSource{Relay: "https://burrow.example.com", Version: "0.6.0", OS: pair[0], Arch: pair[1]}
		if _, _, err := s.Fetch(context.Background()); err == nil {
			t.Errorf("platform %q/%q was accepted", pair[0], pair[1])
		}
	}
}

// Plain HTTP is for this machine only, as for the installer.
func TestFetch_PlainHTTPOnLoopback(t *testing.T) {
	tempRoot(t)
	ur := newUpdateRelay(t, false)
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"})
	ur.checksums = sumLine(ur.archive, linuxArchive)
	bin, cleanup, err := ur.source("linux", "amd64").Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if got, _ := os.ReadFile(bin); string(got) != "new client" {
		t.Fatalf("extracted %q", got)
	}
}

func TestFetch_CancelAbortsAndCleansUp(t *testing.T) {
	tmp := tempRoot(t)
	ur := newUpdateRelay(t, true)
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"})
	ur.checksums = sumLine(ur.archive, linuxArchive)
	started := make(chan struct{})
	release := make(chan struct{})
	ur.archiveHandler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		_, _ = w.Write([]byte("some"))
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	done := make(chan error, 1)
	go func() {
		_, _, err := ur.source("linux", "amd64").Fetch(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Fetch did not end after the cancel")
	}
	mustBeEmpty(t, tmp)
}

func TestFetch_TimeLimit(t *testing.T) {
	tmp := tempRoot(t)
	prev := updateTimeout
	updateTimeout = 200 * time.Millisecond
	t.Cleanup(func() { updateTimeout = prev })
	ur := newUpdateRelay(t, true)
	ur.archive = tarGz(t, tarEntry{name: "burrow", body: "new client"})
	ur.checksums = sumLine(ur.archive, linuxArchive)
	release := make(chan struct{})
	ur.archiveHandler = func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}
	defer close(release)
	_, _, err := ur.source("linux", "amd64").Fetch(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	mustBeEmpty(t, tmp)
}

// names lists what is in dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func writeFile(t *testing.T, path, body string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceExecutable(t *testing.T) {
	for _, aside := range []bool{false, true} {
		name := "rename over"
		if aside {
			name = "old one aside first (Windows)"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			current := filepath.Join(dir, "burrow.exe")
			writeFile(t, current, "old client", 0o750)
			// A leftover of an earlier update is in the way.
			writeFile(t, current+".old", "older client", 0o600)
			newBin := filepath.Join(t.TempDir(), "burrow.exe")
			writeFile(t, newBin, "new client", 0o700)

			if err := replaceExecutable(current, newBin, aside); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(current)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "new client" {
				t.Fatalf("content %q", got)
			}
			if runtime.GOOS != "windows" {
				st, err := os.Stat(current)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm() != 0o750 {
					t.Fatalf("mode %v, want 0750", st.Mode().Perm())
				}
			}
			want := []string{"burrow.exe"}
			if !aside {
				want = []string{"burrow.exe", "burrow.exe.old"} // not this function's file on other systems
			}
			if got := names(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("directory holds %v, want %v", got, want)
			}
			// The downloaded file is the caller's to remove.
			if _, err := os.Stat(newBin); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplaceExecutable_PublicUsesThisSystem(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "burrow")
	writeFile(t, current, "old client", 0o755)
	newBin := filepath.Join(t.TempDir(), "burrow")
	writeFile(t, newBin, "new client", 0o755)
	if err := ReplaceExecutable(current, newBin); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(current); string(got) != "new client" {
		t.Fatalf("content %q", got)
	}
	if got := names(t, dir); len(got) != 1 {
		t.Fatalf("directory holds %v", got)
	}
}

func TestReplaceExecutable_Failures(t *testing.T) {
	t.Run("directory not writable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a user that a directory mode can stop")
		}
		for _, aside := range []bool{false, true} {
			dir := t.TempDir()
			current := filepath.Join(dir, "burrow")
			writeFile(t, current, "old client", 0o755)
			newBin := filepath.Join(t.TempDir(), "burrow")
			writeFile(t, newBin, "new client", 0o755)
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

			if err := CheckReplaceable(current); !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("CheckReplaceable: %v, want a permission error", err)
			}
			err := replaceExecutable(current, newBin, aside)
			if !errors.Is(err, fs.ErrPermission) {
				t.Fatalf("err = %v, want a permission error", err)
			}
			if got, _ := os.ReadFile(current); string(got) != "old client" {
				t.Fatalf("the old binary changed: %q", got)
			}
			if got := names(t, dir); len(got) != 1 {
				t.Fatalf("directory holds %v", got)
			}
		}
	})
	t.Run("writable directory passes the check and gains nothing", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "burrow")
		writeFile(t, current, "old client", 0o755)
		if err := CheckReplaceable(current); err != nil {
			t.Fatal(err)
		}
		if got := names(t, dir); len(got) != 1 {
			t.Fatalf("directory holds %v", got)
		}
	})
	t.Run("new binary missing", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "burrow")
		writeFile(t, current, "old client", 0o755)
		if err := replaceExecutable(current, filepath.Join(dir, "nothing-here"), false); err == nil {
			t.Fatal("no error")
		}
		if got, _ := os.ReadFile(current); string(got) != "old client" {
			t.Fatalf("the old binary changed: %q", got)
		}
		if got := names(t, dir); len(got) != 1 {
			t.Fatalf("directory holds %v", got)
		}
	})
	t.Run("new binary empty", func(t *testing.T) {
		dir := t.TempDir()
		current := filepath.Join(dir, "burrow")
		writeFile(t, current, "old client", 0o755)
		newBin := filepath.Join(t.TempDir(), "burrow")
		writeFile(t, newBin, "", 0o755)
		if err := replaceExecutable(current, newBin, true); err == nil {
			t.Fatal("no error")
		}
		if got, _ := os.ReadFile(current); string(got) != "old client" {
			t.Fatalf("the old binary changed: %q", got)
		}
		if got := names(t, dir); len(got) != 1 {
			t.Fatalf("directory holds %v", got)
		}
	})
	t.Run("target is not a regular file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "sub")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		newBin := filepath.Join(t.TempDir(), "burrow")
		writeFile(t, newBin, "new client", 0o755)
		if err := replaceExecutable(target, newBin, false); err == nil {
			t.Fatal("a directory was replaced")
		}
		if got := names(t, dir); len(got) != 1 {
			t.Fatalf("directory holds %v", got)
		}
	})
}

func TestRemoveReplacedExecutable(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, "burrow.exe")
	writeFile(t, current, "client", 0o755)
	writeFile(t, current+".old", "old client", 0o755)
	RemoveReplacedExecutable(current)
	if got := names(t, dir); len(got) != 1 || got[0] != "burrow.exe" {
		t.Fatalf("directory holds %v", got)
	}
	RemoveReplacedExecutable(current) // nothing there: no harm
	// A directory of that name is not a leftover of an update.
	if err := os.Mkdir(current+".old", 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(current+".old", "keep"), "x", 0o600)
	RemoveReplacedExecutable(current)
	if _, err := os.Stat(filepath.Join(current+".old", "keep")); err != nil {
		t.Fatal("a directory was removed")
	}
}
