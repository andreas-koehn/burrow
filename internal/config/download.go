package config

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// DefaultClientDownloadBase is where the client archives are published: the
// GitHub releases of this repository. The relay's /download/ routes redirect
// to <base>/<tag>/<archive>. A fork sets client_download_base instead.
//
// It names the repository as it is today. An older owner name still redirects
// there, but a redirect that somebody else could claim later must not be what
// an installer trusts.
const DefaultClientDownloadBase = "https://github.com/andreas-koehn/burrow/releases/download"

// downloadBaseRE is an http(s) URL of host, optional port and a path of
// unreserved characters: no user info, no query, no fragment, and nothing a
// shell or PowerShell would read as more than text.
var downloadBaseRE = regexp.MustCompile(`^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$`)

// CleanClientDownloadBase checks a client_download_base setting and returns
// it without trailing slashes. Empty gives DefaultClientDownloadBase. The
// error does not repeat the value: a URL may carry credentials.
func CleanClientDownloadBase(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultClientDownloadBase, nil
	}
	s = strings.TrimRight(s, "/")
	if len(s) > 512 || !downloadBaseRE.MatchString(s) || strings.Contains(s, "/../") || strings.HasSuffix(s, "/..") {
		return "", errors.New("must be an http(s) URL of host and path, without user, query or fragment")
	}
	return s, nil
}

// plainHTTPToAnotherHost reports whether a cleaned download base is plain
// HTTP to a host other than this machine (localhost or a loopback address).
func plainHTTPToAnotherHost(base string) bool {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}
