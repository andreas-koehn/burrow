package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/config"
)

// healthcheckTimeout bounds one probe. It is deliberately shorter than a
// typical container healthcheck timeout so the command reports its own
// failure instead of being killed by the runtime.
const healthcheckTimeout = 3 * time.Second

// newHealthcheckCmd returns the `burrowd healthcheck` cobra subcommand.
//
// The release images are distroless (no shell, no curl), so a container
// HEALTHCHECK has nothing to probe /healthz with. This command is that
// probe: it GETs /healthz on the running server's own dashboard listener.
//
// Usage:
//
//	burrowd healthcheck [--url <url>]
//
// Exit codes:
//
//	0 — /healthz answered 200
//	1 — anything else (connection refused, timeout, non-200)
func newHealthcheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "healthcheck",
		Short: "Probe the running server's /healthz (for container healthchecks)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, _ := cmd.Flags().GetString("url")
			if url == "" {
				cfg, err := config.LoadServer(nil)
				if err != nil {
					fmt.Fprintln(cmd.ErrOrStderr(), "error:", err)
					return cobraExit1()
				}
				url = healthcheckURL(cfg)
			}
			return runHealthcheck(cmd.Context(), url, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().String("url", "", "health URL to probe (default: derived from the server config)")
	return cmd
}

// healthcheckURL derives the loopback /healthz URL from the same config
// `burrowd serve` reads. It mirrors serve's listener choice: built-in ACME
// promotes the stock :8080 dashboard to :443 over TLS, and a file
// certificate (BURROW_HTTP_TLS_CERT/KEY) serves TLS on the configured port.
func healthcheckURL(cfg *config.ServerConfig) string {
	listen := cfg.HTTPListen
	scheme := "http"
	if cfg.ACMEDomain != "" {
		scheme = "https"
		if listen == ":8080" {
			listen = ":443"
		}
	} else if cfg.HTTPTLSCert != "" {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		host, port = "", "8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/healthz"
}

// runHealthcheck is the testable seam: one GET, 200 or exit 1.
func runHealthcheck(ctx context.Context, url string, stderr io.Writer) error {
	client := &http.Client{
		Timeout: healthcheckTimeout,
		Transport: &http.Transport{
			// The probe dials loopback, but the certificate is issued for the
			// public hostname — verification can never succeed here, and the
			// probe only asks "is the listener answering".
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: loopback liveness probe, see above.
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return cobraExit1()
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, "unhealthy:", err)
		return cobraExit1()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: %s returned %d\n", url, resp.StatusCode)
		return cobraExit1()
	}
	return nil
}
