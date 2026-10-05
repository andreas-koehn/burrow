// Package install renders the client installers a relay hands out at
// /install.sh and /install.ps1.
//
// The scripts are templates with a handful of values filled in. A script is
// piped into a shell, so each value is checked against a narrow pattern here
// and refused when it does not match: nothing is escaped and passed on. The
// patterns admit no quote, no `$`, no backtick, no backslash, no whitespace
// and no control character, which is what makes the plain substitution into
// a quoted string safe in both sh and PowerShell.
package install

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"text/template"
)

// DefaultDownloadBase is where the client archives are published: the GitHub
// releases of this repository. The redirects of /download/ point at
// <base>/<tag>/<archive>. A fork sets client_download_base instead.
//
// It names the repository as it is today. An older owner name still redirects
// there, but a redirect that somebody else could claim later must not be what
// an installer trusts.
const DefaultDownloadBase = "https://github.com/andreas-koehn/burrow/releases/download"

// The two channels a relay installs from.
const (
	// ChannelRelease is a version-tagged release.
	ChannelRelease = "release"
	// ChannelDevelop is the rolling pre-release of the develop branch.
	ChannelDevelop = "develop"
)

// Asset is one client archive: the platform it is for and its file name.
type Asset struct {
	OS, Arch, Name string
}

// Params are the values filled into an installer.
type Params struct {
	// Relay is the relay's public origin, as Origin returns it.
	Relay string
	// Version is the client version the relay hands out: MAJOR.MINOR.PATCH
	// on the release channel, "develop" on the rolling one.
	Version string
	// Channel is ChannelRelease or ChannelDevelop.
	Channel string
	// Assets are the archives of that channel. The shell installer gets the
	// ones that are not for Windows, the PowerShell installer the others.
	Assets []Asset
	// ManualURL is a page with the archives for a download by hand. Optional.
	ManualURL string
}

//go:embed install.sh.tmpl
var shellSource string

//go:embed install.ps1.tmpl
var powerShellSource string

// The templates are parsed on first use, not at start: internal/config uses
// this package for the download base alone, and the client links config.
var (
	shellTmpl = sync.OnceValue(func() *template.Template {
		return template.Must(template.New("install.sh").Parse(shellSource))
	})
	powerShellTmpl = sync.OnceValue(func() *template.Template {
		return template.Must(template.New("install.ps1").Parse(powerShellSource))
	})
)

var (
	// hostRE is a host name or IPv4 address with an optional port. IPv6
	// literals are left out on purpose: a relay is installed from by name.
	hostRE    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	originRE  = regexp.MustCompile(`^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.-]{0,63}$`)
	wordRE    = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	// urlRE is an http(s) URL of host, optional port and a path of
	// unreserved characters: no user info, no query, no fragment.
	urlRE = regexp.MustCompile(`^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)
)

// maxHostLen is the longest host (with port) an installer is rendered for.
const maxHostLen = 253

// Origin builds the relay's public origin from a scheme and the Host of a
// request. ok is false when the host is anything but a plain host name or
// IPv4 address with an optional port; the caller answers 400 then.
func Origin(scheme, host string) (origin string, ok bool) {
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if len(host) > maxHostLen || !hostRE.MatchString(host) {
		return "", false
	}
	return scheme + "://" + host, true
}

// CleanDownloadBase checks a client_download_base setting and returns it
// without trailing slashes. Empty gives DefaultDownloadBase.
func CleanDownloadBase(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultDownloadBase, nil
	}
	s = strings.TrimRight(s, "/")
	if len(s) > 512 || !urlRE.MatchString(s) || strings.Contains(s, "/../") || strings.HasSuffix(s, "/..") {
		return "", errors.New("must be an http(s) URL of host and path, without user, query or fragment")
	}
	return s, nil
}

func (p Params) check() error {
	if len(p.Relay) > maxHostLen+len("https://") || !originRE.MatchString(p.Relay) {
		return errors.New("install: relay origin is not a plain http(s) origin")
	}
	if !versionRE.MatchString(p.Version) {
		return errors.New("install: version has characters outside [0-9A-Za-z.-]")
	}
	if p.Channel != ChannelRelease && p.Channel != ChannelDevelop {
		return errors.New("install: unknown channel")
	}
	if p.ManualURL != "" && (len(p.ManualURL) > 600 || !urlRE.MatchString(p.ManualURL)) {
		return errors.New("install: manual download URL is not a plain http(s) URL")
	}
	for _, a := range p.Assets {
		if !wordRE.MatchString(a.OS) || !wordRE.MatchString(a.Arch) || !nameRE.MatchString(a.Name) {
			return errors.New("install: asset with characters outside the allowed set")
		}
	}
	return nil
}

// filtered returns the checked parameters with the assets for Windows
// (windows true) or for everything else.
func (p Params) filtered(windows bool) (Params, error) {
	if err := p.check(); err != nil {
		return Params{}, err
	}
	var keep []Asset
	for _, a := range p.Assets {
		if (a.OS == "windows") == windows {
			keep = append(keep, a)
		}
	}
	p.Assets = keep
	return p, nil
}

func render(t *template.Template, p Params) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("install: render %s: %w", t.Name(), err)
	}
	return buf.Bytes(), nil
}

// Shell renders the POSIX shell installer.
func Shell(p Params) ([]byte, error) {
	p, err := p.filtered(false)
	if err != nil {
		return nil, err
	}
	return render(shellTmpl(), p)
}

// PowerShell renders the Windows installer.
func PowerShell(p Params) ([]byte, error) {
	p, err := p.filtered(true)
	if err != nil {
		return nil, err
	}
	return render(powerShellTmpl(), p)
}
