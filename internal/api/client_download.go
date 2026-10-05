package api

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ankoehn/burrow/internal/api/install"
	"github.com/ankoehn/burrow/internal/config"
	"github.com/ankoehn/burrow/internal/version"
)

// This file is the one place that knows which client archives exist and what
// they are called. The install scripts get their list from here as well, so a
// script never asks for a file the redirect does not know.
//
// The two tables mirror what CI builds and are pinned to it by
// TestClientPlatforms_MatchGoreleaser and TestClientPlatforms_MatchDevelopWorkflow:
//
//   - .goreleaser.yml, build and archive `client`: a version-tagged release,
//     archives burrow_<os>_<arch>_<version>.tar.gz (.zip for Windows) with
//     arm spelled armv7, under the tag v<version>.
//   - .github/workflows/develop.yml: the rolling pre-release `develop`,
//     archives burrow_<os>_<arch>.tar.gz (.zip for Windows).
//
// Both publish their SHA-256 list as checksums.txt.

// clientPlatform is one OS/architecture pair in Go's own spelling.
type clientPlatform struct{ OS, Arch string }

// releasePlatforms are the pairs a version-tagged release builds.
var releasePlatforms = []clientPlatform{
	{"linux", "amd64"}, {"linux", "arm64"}, {"linux", "arm"}, {"linux", "386"},
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"windows", "amd64"}, {"windows", "386"},
}

// developPlatforms are the pairs the rolling develop pre-release builds.
var developPlatforms = []clientPlatform{
	{"linux", "amd64"}, {"linux", "arm64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

// checksumsName is the checksum file of both channels.
const checksumsName = "checksums.txt"

// releaseVersionRE is a release version: MAJOR.MINOR.PATCH, optional leading
// v. Everything else — "dev", "develop", a commit, a `git describe` such as
// 0.6.0-12-gabc — is an untagged build.
var releaseVersionRE = regexp.MustCompile(`^v?([0-9]{1,9}\.[0-9]{1,9}\.[0-9]{1,9})$`)

// clientChannel is what a relay of one build version hands out.
type clientChannel struct {
	// Name is install.ChannelRelease or install.ChannelDevelop.
	Name string
	// Version is the client version: "0.6.0", or "develop".
	Version string
	// Tag is the release tag the archives are published under.
	Tag       string
	Platforms []clientPlatform
}

// channelOf maps the relay's build version to its channel. The build version
// itself goes no further than this function unless it is a release version.
func channelOf(buildVersion string) clientChannel {
	if m := releaseVersionRE.FindStringSubmatch(buildVersion); m != nil {
		return clientChannel{Name: install.ChannelRelease, Version: m[1], Tag: "v" + m[1], Platforms: releasePlatforms}
	}
	return clientChannel{Name: install.ChannelDevelop, Version: install.ChannelDevelop, Tag: install.ChannelDevelop, Platforms: developPlatforms}
}

// name returns the archive of one pair. ok is false when the channel does not
// build that pair. goos and goarch are compared with the table and never
// become part of the name themselves.
func (c clientChannel) name(goos, goarch string) (name string, ok bool) {
	for _, p := range c.Platforms {
		if p.OS != goos || p.Arch != goarch {
			continue
		}
		ext := ".tar.gz"
		if p.OS == "windows" {
			ext = ".zip"
		}
		if c.Name == install.ChannelDevelop {
			return "burrow_" + p.OS + "_" + p.Arch + ext, true
		}
		arch := p.Arch
		if arch == "arm" {
			arch = "armv7"
		}
		return "burrow_" + p.OS + "_" + arch + "_" + c.Version + ext, true
	}
	return "", false
}

// assets lists every archive of the channel.
func (c clientChannel) assets() []install.Asset {
	out := make([]install.Asset, 0, len(c.Platforms))
	for _, p := range c.Platforms {
		name, _ := c.name(p.OS, p.Arch)
		out = append(out, install.Asset{OS: p.OS, Arch: p.Arch, Name: name})
	}
	return out
}

// supported is the channel's pairs as "os/arch, os/arch", sorted.
func (c clientChannel) supported() string {
	pairs := make([]string, 0, len(c.Platforms))
	for _, p := range c.Platforms {
		pairs = append(pairs, p.OS+"/"+p.Arch)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ", ")
}

// archiveName is the file name of the client archive that a relay of the
// given build version hands out for goos/goarch. ok is false when that
// channel has no such build.
func archiveName(buildVersion, goos, goarch string) (string, bool) {
	return channelOf(buildVersion).name(goos, goarch)
}

// downloadBase is the checked client_download_base. cmd/server passes a
// value config has already checked; a value that is not a plain URL (a test,
// a caller that skipped config) falls back to the default instead of
// reaching a Location header.
func (d Deps) downloadBase() string {
	base, err := config.CleanClientDownloadBase(d.ClientDownloadBase)
	if err != nil {
		return config.DefaultClientDownloadBase
	}
	return base
}

// manualDownloadURL is the page a person downloads an archive from by hand:
// the release page when the base is a GitHub-style …/releases/download, the
// tag's directory otherwise, and nothing when the relay serves the archives
// itself.
func (d Deps) manualDownloadURL(c clientChannel) string {
	if d.ClientDownloadDir != "" {
		return ""
	}
	base := d.downloadBase()
	if strings.HasSuffix(base, "/releases/download") {
		return strings.TrimSuffix(base, "/download") + "/tag/" + c.Tag
	}
	return base + "/" + c.Tag
}

// plainText writes a short text/plain answer that no browser will sniff
// into something else.
func plainText(w http.ResponseWriter, r *http.Request, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// getOrHead lets GET and HEAD through and answers 405 to everything else.
func getOrHead(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			plainText(w, r, http.StatusMethodNotAllowed, "Method not allowed.\n")
			return
		}
		next(w, r)
	})
}

// ClientDownloadNotFound answers every path under /download/ that is not a
// build of this relay's channel. The body lists the builds and repeats
// nothing of the request.
func (d Deps) ClientDownloadNotFound(w http.ResponseWriter, r *http.Request) {
	c := channelOf(version.Version)
	plainText(w, r, http.StatusNotFound,
		"This relay has no burrow download at this address.\n"+
			"Use /download/burrow/<os>/<arch> with one of: "+c.supported()+"\n")
}

// GetClientDownload hands out the client archive for {os}/{arch}: a redirect
// to the release of the relay's own version, or the file itself when
// client_download_dir is set. GET /download/burrow/{os}/{arch}, public.
func (d Deps) GetClientDownload(w http.ResponseWriter, r *http.Request) {
	c := channelOf(version.Version)
	name, ok := c.name(chi.URLParam(r, "os"), chi.URLParam(r, "arch"))
	if !ok {
		d.ClientDownloadNotFound(w, r)
		return
	}
	ctype := "application/gzip"
	if strings.HasSuffix(name, ".zip") {
		ctype = "application/zip"
	}
	d.handOutFile(w, r, c, name, ctype)
}

// GetClientChecksums hands out the checksums of the same release.
// GET /download/burrow/checksums.txt, public.
func (d Deps) GetClientChecksums(w http.ResponseWriter, r *http.Request) {
	d.handOutFile(w, r, channelOf(version.Version), checksumsName, "text/plain; charset=utf-8")
}

// handOutFile redirects to, or serves, one file of the channel. name comes
// from the tables above or is checksumsName — never from the request.
func (d Deps) handOutFile(w http.ResponseWriter, r *http.Request, c clientChannel, name, ctype string) {
	if c.Name == install.ChannelDevelop {
		w.Header().Set("Burrow-Channel", install.ChannelDevelop)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if d.ClientDownloadDir == "" {
		// The relay's version changes with an upgrade; the redirect must
		// not outlive it in a cache.
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Location", d.downloadBase()+"/"+c.Tag+"/"+name)
		w.WriteHeader(http.StatusFound)
		return
	}
	// os.Root confines the lookup to the directory: a symlink that leads out
	// of it is an error, not a file.
	root, err := os.OpenRoot(d.ClientDownloadDir)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("client download directory cannot be opened", "err", err)
		}
		d.ClientDownloadNotFound(w, r)
		return
	}
	defer root.Close()
	f, err := root.Open(name)
	if err != nil {
		d.ClientDownloadNotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		d.ClientDownloadNotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// installParams are the values of an install script for this request. ok is
// false when the request's Host is not a plain host name with an optional
// port.
//
// The host is the request's Host, as for the discovery endpoint. The scheme
// is HTTPS when the relay serves TLS itself or marks its cookies Secure (the
// setting an operator behind a TLS-terminating proxy turns on); it is not
// read from X-Forwarded-Proto or any other header.
func (d Deps) installParams(r *http.Request) (install.Params, bool) {
	scheme := "http"
	if r.TLS != nil || d.HTTPSEnabled || d.SecureCookies {
		scheme = "https"
	}
	origin, ok := install.Origin(scheme, r.Host)
	if !ok {
		return install.Params{}, false
	}
	c := channelOf(version.Version)
	return install.Params{
		Relay:     origin,
		Version:   c.Version,
		Channel:   c.Name,
		Assets:    c.assets(),
		ManualURL: d.manualDownloadURL(c),
	}, true
}

// serveInstaller renders one installer and writes it.
func (d Deps) serveInstaller(w http.ResponseWriter, r *http.Request, ctype string, render func(install.Params) ([]byte, error)) {
	p, ok := d.installParams(r)
	if !ok {
		plainText(w, r, http.StatusBadRequest,
			"This address cannot be used in an install script. Ask for it by the relay's host name.\n")
		return
	}
	script, err := render(p)
	if err != nil {
		if d.Log != nil {
			d.Log.Error("install script not rendered", "err", err)
		}
		plainText(w, r, http.StatusInternalServerError, "The install script is not available.\n")
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(script)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(script)
	}
}

// GetInstallSh serves the POSIX shell installer. GET /install.sh, public.
func (d Deps) GetInstallSh(w http.ResponseWriter, r *http.Request) {
	d.serveInstaller(w, r, "text/x-shellscript; charset=utf-8", install.Shell)
}

// GetInstallPs1 serves the PowerShell installer. GET /install.ps1, public.
func (d Deps) GetInstallPs1(w http.ResponseWriter, r *http.Request) {
	d.serveInstaller(w, r, "text/plain; charset=utf-8", install.PowerShell)
}
