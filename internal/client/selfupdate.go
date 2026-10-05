package client

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/ankoehn/burrow/internal/version"
)

// This file replaces the running binary with one downloaded through the relay.
// It is the same road the install scripts take, and it stops at the same
// places: anything that is not exactly the expected file ends the update with
// the old binary untouched.
//
// What the SHA-256 is worth: checksums.txt comes from the same place as the
// archive. It catches a damaged, cut-off or wrong file. It says nothing about
// a relay or a release host that hands out something else on purpose.

// Limits of one update. They are variables so that tests can reach them.
var (
	// maxArchiveBytes is the largest archive that is downloaded.
	maxArchiveBytes int64 = 200 << 20
	// maxBinaryBytes is the largest binary that is unpacked. Twice that is
	// the most a tar archive may unpack to in all, the files that are only
	// skipped included.
	maxBinaryBytes int64 = 200 << 20
	// updateTimeout bounds both downloads together.
	updateTimeout = 15 * time.Minute
)

const (
	// maxChecksumsBytes is the largest checksums.txt that is read.
	maxChecksumsBytes = 1 << 20
	// maxUpdateRedirects is how many redirects one download follows.
	maxUpdateRedirects = 5
	// maxArchiveEntries is how many entries of an archive are looked at.
	maxArchiveEntries = 1000
)

// ErrChecksum says that the downloaded archive is not the file checksums.txt
// lists, or that checksums.txt lists no usable SHA-256 for it.
var ErrChecksum = errors.New("downloaded client does not match its checksum")

// ErrNoBuild says that the relay hands out no client for the platform.
var ErrNoBuild = errors.New("the relay has no burrow build for this platform")

// releaseVersionRE is a release version: MAJOR.MINOR.PATCH, optional leading
// v. It is the relay's own rule (internal/api, channelOf). \z, not $: a
// trailing newline is not part of a version.
var releaseVersionRE = regexp.MustCompile(`\Av?([0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9})\z`)

// ReleaseVersion returns v without its leading "v" when v is a version-tagged
// release. Everything else ("develop", "dev", a release candidate, a `git
// describe`) is a build of the rolling channel and gives false.
func ReleaseVersion(v string) (string, bool) {
	m := releaseVersionRE.FindStringSubmatch(v)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ArchiveName is the file name of the client archive a relay of relayVersion
// hands out for goos/goarch, as CI publishes it and checksums.txt lists it:
// burrow_<os>_<arch>_<version>.tar.gz for a release (arm spelled armv7),
// burrow_<os>_<arch>.tar.gz on the rolling channel, .zip for Windows.
func ArchiveName(relayVersion, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	v, ok := ReleaseVersion(relayVersion)
	if !ok {
		return "burrow_" + goos + "_" + goarch + ext
	}
	if goarch == "arm" {
		goarch = "armv7"
	}
	return "burrow_" + goos + "_" + goarch + "_" + v + ext
}

// UpdateSource is where a client for one platform comes from.
type UpdateSource struct {
	// HTTP supplies the transport and with it the TLS settings. Its redirect
	// policy, cookie jar and timeout are not used.
	HTTP *http.Client
	// Relay is the relay's dashboard address, https://host[:port]. Plain
	// http:// is accepted for this machine only (localhost or a loopback
	// address).
	Relay string
	// Version is the relay's version as its discovery document reports it.
	// It decides the archive's name and nothing else.
	Version  string
	OS, Arch string // in Go's spelling: runtime.GOOS, runtime.GOARCH
}

// platformPartRE is what an OS or architecture name may consist of. The names
// become part of a URL path and of a file name.
var platformPartRE = regexp.MustCompile(`\A[a-z0-9]{1,16}\z`)

// isLoopbackHost reports whether host is this machine: localhost or a
// loopback IP address.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// origin checks Relay and returns it as scheme://host[:port].
func (s UpdateSource) origin() (string, error) {
	bad := errors.New("the relay address must be https://host or https://host:port")
	u, err := url.Parse(strings.TrimSpace(s.Relay))
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", bad
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", errors.New("the client is downloaded over https only; plain http is for a relay on this machine")
		}
	default:
		return "", bad
	}
	return u.Scheme + "://" + u.Host, nil
}

// httpClient is the client for the downloads: the caller's transport, no
// cookies, and a redirect policy that stays on HTTPS.
//
// A release host answers a download with a redirect to its storage, so
// redirects are followed, to other hosts too. The requests carry no token, no
// cookie and no credentials; a redirect whose address holds credentials, and
// one that leaves HTTPS, is not followed. When the relay itself is on this
// machine over plain HTTP, a redirect may stay on this machine over plain HTTP.
func (s UpdateSource) httpClient(plainRelay bool) *http.Client {
	hc := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			switch {
			case len(via) > maxUpdateRedirects:
				return errors.New("too many redirects")
			case req.URL.User != nil:
				return errors.New("a redirect to an address with credentials is not followed")
			case req.URL.Scheme == "https":
				return nil
			case req.URL.Scheme == "http" && plainRelay && isLoopbackHost(req.URL.Hostname()):
				return nil
			}
			return errors.New("a redirect away from https is not followed")
		},
	}
	if s.HTTP != nil {
		hc.Transport = s.HTTP.Transport
	}
	return hc
}

// download gets rawURL and writes at most limit bytes of the answer to w.
// redirected says whether the answer came from another address than rawURL;
// status is the answer's status when it is not 200.
func download(ctx context.Context, hc *http.Client, rawURL string, limit int64, w io.Writer) (redirected bool, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false, 0, errors.New("the download address is not valid")
	}
	req.Header.Set("User-Agent", "burrow/"+version.Version)
	// The archive as it is: the SHA-256 is over these bytes.
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		// Without the *url.Error around it: that repeats an address, and
		// after a redirect one that is not ours.
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			err = ue.Err
		}
		return false, 0, err
	}
	defer resp.Body.Close()
	redirected = resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() != rawURL
	if resp.StatusCode != http.StatusOK {
		return redirected, resp.StatusCode, fmt.Errorf("status %d", resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return redirected, 0, fmt.Errorf("it is too large (%d bytes, at most %d are accepted)", resp.ContentLength, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return redirected, 0, ctx.Err()
		}
		return redirected, 0, fmt.Errorf("the download was cut off: %w", err)
	}
	if n > limit {
		return redirected, 0, fmt.Errorf("it is too large (more than %d bytes)", limit)
	}
	return redirected, 0, nil
}

// checksumFor returns the SHA-256 that sums lists for the file called name,
// in lower case. The name must match exactly; a leading "*" (binary mode) is
// not part of it. No line, a line without a SHA-256, or two lines that
// disagree give ErrChecksum.
func checksumFor(sums []byte, name string) (string, error) {
	found := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	sc.Buffer(make([]byte, 0, 4096), maxChecksumsBytes+1)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
			return "", fmt.Errorf("%w: the line for %s in checksums.txt does not hold a SHA-256", ErrChecksum, name)
		}
		if found != "" && found != sum {
			return "", fmt.Errorf("%w: checksums.txt lists %s twice with different values", ErrChecksum, name)
		}
		found = sum
	}
	if sc.Err() != nil || found == "" {
		return "", fmt.Errorf("%w: checksums.txt has no line for %s", ErrChecksum, name)
	}
	return found, nil
}

// Fetch downloads the archive and the checksums through the relay's download
// endpoint, verifies the archive's SHA-256, and unpacks the one binary. It
// returns the binary's path in a temporary directory of its own and a function
// that removes that directory. After an error nothing is left behind and the
// returned function does nothing.
//
// Nothing of the archive is unpacked before its SHA-256 matched the line that
// checksums.txt has for exactly the archive's name (ArchiveName). The requests
// carry no token.
func (s UpdateSource) Fetch(ctx context.Context) (binPath string, cleanup func(), err error) {
	nothing := func() {}
	origin, err := s.origin()
	if err != nil {
		return "", nothing, err
	}
	if !platformPartRE.MatchString(s.OS) || !platformPartRE.MatchString(s.Arch) {
		return "", nothing, errors.New("the platform to download for is not valid")
	}
	archiveName := ArchiveName(s.Version, s.OS, s.Arch)
	binName := "burrow"
	if s.OS == "windows" {
		binName = "burrow.exe"
	}
	hc := s.httpClient(strings.HasPrefix(origin, "http://"))

	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	dir, err := os.MkdirTemp("", "burrow-update-")
	if err != nil {
		return "", nothing, fmt.Errorf("create a temporary directory: %w", err)
	}
	remove := func() { _ = os.RemoveAll(dir) }
	defer func() {
		if err != nil {
			remove()
			binPath, cleanup = "", nothing
		}
	}()

	// 1. The checksums.
	var sums bytes.Buffer
	if _, _, err := download(ctx, hc, origin+"/download/burrow/checksums.txt", maxChecksumsBytes, &sums); err != nil {
		if ctx.Err() != nil {
			return "", nothing, ctx.Err()
		}
		return "", nothing, fmt.Errorf("download checksums.txt: %w", err)
	}
	want, err := checksumFor(sums.Bytes(), archiveName)
	if err != nil {
		return "", nothing, err
	}

	// 2. The archive, hashed while it is written.
	archivePath := filepath.Join(dir, "archive")
	f, err := os.OpenFile(archivePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", nothing, fmt.Errorf("create a temporary file: %w", err)
	}
	hash := sha256.New()
	redirected, status, derr := download(ctx, hc, origin+"/download/burrow/"+s.OS+"/"+s.Arch, maxArchiveBytes, io.MultiWriter(f, hash))
	cerr := f.Close()
	switch {
	case ctx.Err() != nil:
		return "", nothing, ctx.Err()
	case status == http.StatusNotFound && !redirected:
		return "", nothing, fmt.Errorf("%w: %s/%s", ErrNoBuild, s.OS, s.Arch)
	case status == http.StatusNotFound:
		return "", nothing, fmt.Errorf("download %s: the release the relay points at does not have this file (status 404)", archiveName)
	case derr != nil:
		return "", nothing, fmt.Errorf("download %s: %w", archiveName, derr)
	case cerr != nil:
		return "", nothing, fmt.Errorf("write the download: %w", cerr)
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want {
		return "", nothing, fmt.Errorf("%w: %s has SHA-256 %s, checksums.txt lists %s", ErrChecksum, archiveName, got, want)
	}

	// 3. The one binary.
	binPath = filepath.Join(dir, binName)
	if s.OS == "windows" {
		err = unpackZip(archivePath, binName, binPath)
	} else {
		err = unpackTarGz(archivePath, binName, binPath)
	}
	if err != nil {
		return "", nothing, fmt.Errorf("%s: %w", archiveName, err)
	}
	return binPath, remove, nil
}

// errUnsafeArchive is the refusal of an archive with an entry that has no
// place in a client archive.
func errUnsafeArchive(why string) error {
	return errors.New("the archive is unsafe and was not unpacked: " + why)
}

// entryAtRoot checks the name of an archive entry and reports whether it is
// the file called binName at the archive's root. A name that is absolute,
// leaves the archive, or is written with backslashes or a drive is an error:
// such an archive is not unpacked at all.
func entryAtRoot(name, binName string) (bool, error) {
	if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return false, errUnsafeArchive("an entry has an absolute or foreign path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return false, errUnsafeArchive("an entry leaves the archive with ..")
		}
	}
	return strings.TrimPrefix(name, "./") == binName, nil
}

// cappedReader fails once more than n bytes were read through it.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errors.New("the archive is too large when unpacked")
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// writeBinary copies at most maxBinaryBytes from r to a new file at dest.
func writeBinary(r io.Reader, dest string) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("create a temporary file: %w", err)
	}
	n, err := io.Copy(out, io.LimitReader(r, maxBinaryBytes+1))
	cerr := out.Close()
	switch {
	case err != nil:
		return fmt.Errorf("the archive cannot be unpacked: %w", err)
	case n > maxBinaryBytes:
		return errors.New("the binary in the archive is too large")
	case n == 0:
		return errors.New("the binary in the archive is empty")
	case cerr != nil:
		return fmt.Errorf("write the binary: %w", cerr)
	}
	return nil
}

// unpackTarGz writes the regular file binName at the root of the .tar.gz at
// path to dest. Other regular files and directories are passed over; any
// other kind of entry (a link, a device) refuses the whole archive.
func unpackTarGz(path, binName, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the archive cannot be unpacked: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(&cappedReader{r: gz, n: 2 * maxBinaryBytes})
	found := false
	for entries := 0; ; entries++ {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("the archive cannot be unpacked: %w", err)
		}
		if entries >= maxArchiveEntries {
			return errUnsafeArchive("it has too many entries")
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader:
			continue // carries no file
		case tar.TypeReg, tar.TypeDir:
		default:
			return errUnsafeArchive("it holds a link or a special file")
		}
		isBin, err := entryAtRoot(strings.TrimSuffix(hdr.Name, "/"), binName)
		if err != nil {
			return err
		}
		if !isBin || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if found {
			return errUnsafeArchive("it holds " + binName + " more than once")
		}
		if hdr.Size > maxBinaryBytes {
			return errors.New("the binary in the archive is too large")
		}
		if err := writeBinary(tr, dest); err != nil {
			return err
		}
		found = true
	}
	if !found {
		return errors.New("the archive holds no " + binName)
	}
	return nil
}

// unpackZip does for a .zip what unpackTarGz does for a .tar.gz.
func unpackZip(path, binName, dest string) error {
	zr, err := zip.OpenReader(path)
	if err != nil {
		if errors.Is(err, zip.ErrInsecurePath) {
			if zr != nil {
				_ = zr.Close()
			}
			return errUnsafeArchive("an entry has an absolute or foreign path")
		}
		return fmt.Errorf("the archive cannot be unpacked: %w", err)
	}
	defer zr.Close()
	if len(zr.File) > maxArchiveEntries {
		return errUnsafeArchive("it has too many entries")
	}
	var bin *zip.File
	for _, zf := range zr.File {
		mode := zf.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			return errUnsafeArchive("it holds a link or a special file")
		}
		isBin, err := entryAtRoot(strings.TrimSuffix(zf.Name, "/"), binName)
		if err != nil {
			return err
		}
		if !isBin || !mode.IsRegular() {
			continue
		}
		if bin != nil {
			return errUnsafeArchive("it holds " + binName + " more than once")
		}
		bin = zf
	}
	if bin == nil {
		return errors.New("the archive holds no " + binName)
	}
	if bin.UncompressedSize64 > uint64(maxBinaryBytes) {
		return errors.New("the binary in the archive is too large")
	}
	rc, err := bin.Open()
	if err != nil {
		return fmt.Errorf("the archive cannot be unpacked: %w", err)
	}
	defer rc.Close()
	return writeBinary(rc, dest)
}

// replacedSuffix is what the running binary is renamed to on Windows while a
// new one takes its place.
const replacedSuffix = ".old"

// CheckReplaceable reports whether a new file can be put next to current: the
// error is fs.ErrPermission when the directory is not writable by this user.
// It leaves nothing behind.
func CheckReplaceable(current string) error {
	st, err := os.Lstat(current)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", current)
	}
	tmp, err := os.CreateTemp(filepath.Dir(current), filepath.Base(current)+".*.new")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_ = tmp.Close()
	return os.Remove(name)
}

// ReplaceExecutable swaps newBin into the place of the binary at current,
// which is a regular file (the caller resolves symbolic links first).
//
// The new content is written to a new file in the same directory with the old
// file's permission bits and then renamed over the old one: the path holds the
// old binary or the new one at every moment, never a part of one. On Windows a
// running file cannot be replaced, so the old one is first renamed to
// <current>.old; RemoveReplacedExecutable removes that on a later start. When
// anything fails the old binary is where it was and the new file is removed.
// A directory this user cannot write to gives an error that is
// fs.ErrPermission. newBin itself is left alone.
func ReplaceExecutable(current, newBin string) error {
	return replaceExecutable(current, newBin, runtime.GOOS == "windows")
}

func replaceExecutable(current, newBin string, aside bool) (err error) {
	st, err := os.Lstat(current)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", current)
	}
	src, err := os.Open(newBin)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.CreateTemp(filepath.Dir(current), filepath.Base(current)+".*.new")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	n, err := io.Copy(tmp, src)
	if err == nil && n == 0 {
		err = errors.New("the new binary is empty")
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmpName, st.Mode().Perm())
	}
	if err != nil {
		return err
	}

	if !aside {
		return os.Rename(tmpName, current)
	}
	old := current + replacedSuffix
	if rerr := os.Remove(old); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
		return rerr
	}
	if err = os.Rename(current, old); err != nil {
		return err
	}
	if err = os.Rename(tmpName, current); err != nil {
		// Back to where it was. If even that fails, the old binary is at
		// <current>.old and the error says so.
		if back := os.Rename(old, current); back != nil {
			return fmt.Errorf("%w; the previous binary is now at %s", err, old)
		}
		return err
	}
	// Fails while the old binary still runs; the next start removes it.
	_ = os.Remove(old)
	return nil
}

// RemoveReplacedExecutable removes what an update on Windows left next to the
// binary at current: the previous binary, renamed to <current>.old because it
// was running. Anything that is not a regular file is left alone, and so is
// every failure: the next start tries again.
func RemoveReplacedExecutable(current string) {
	old := current + replacedSuffix
	if st, err := os.Lstat(old); err != nil || !st.Mode().IsRegular() {
		return
	}
	_ = os.Remove(old)
}
