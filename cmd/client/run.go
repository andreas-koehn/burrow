package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/logging"
)

// globalFlags are the settings every connecting command shares.
type globalFlags struct {
	logLevel   string // debug|info|warn|error
	logFormat  string // text|json
	cacert     string // PEM file of a CA to trust
	serverName string // TLS server name; default: host of the control endpoint
	insecure   bool
	// view asks for the status view instead of log lines. Only readGlobals
	// sets it; `connect` builds its flags itself and always logs.
	view bool
}

// startClient connects and keeps the tunnels up until ctx ends. Tests replace it
// to see the options a command built without opening a connection.
var startClient = func(ctx context.Context, o client.Options) error {
	return client.New(o).Run(ctx)
}

// runClient builds the client for the given credentials and tunnels and runs it
// until the process is interrupted. It is the one place that turns flags into
// client.Options, for `connect` and for every newer command.
func runClient(ctx context.Context, creds client.Credentials, tunnels []client.TunnelSpec, g globalFlags) error {
	log := logging.New(g.logLevel, g.logFormat)
	pool, err := loadRootCAs(g.cacert)
	if err != nil {
		return err
	}
	sn := g.serverName
	if sn == "" {
		if h, _, e := net.SplitHostPort(creds.Control); e == nil {
			sn = h
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	o := client.Options{
		Server: creds.Control, Token: creds.Token, Insecure: g.insecure,
		RootCAs: pool, ServerName: sn, Logger: log,
		Tunnels: tunnels,
	}
	if g.view {
		if t, ok := openTerminal(); ok {
			return runWithView(ctx, stop, o, t)
		}
	}
	return startClient(ctx, o)
}

// loadRootCAs reads the PEM file of --cacert; nil without one.
func loadRootCAs(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("cacert %s: no certificates", path)
	}
	return pool, nil
}

// relayHTTPClient is the HTTP client for a relay's dashboard address. It
// honours --cacert, --server-name and --insecure like the control connection.
func relayHTTPClient(g globalFlags) (*http.Client, error) {
	pool, err := loadRootCAs(g.cacert)
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: g.insecure, //nolint:gosec // dev-only opt-in
			RootCAs:            pool,
			ServerName:         g.serverName,
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout: client.DiscoveryTimeout,
		// One request per command: nothing is kept open behind it.
		DisableKeepAlives: true,
	}}, nil
}

// discoverRelay asks the relay for its discovery document. It is deps.discover
// outside tests.
func discoverRelay(ctx context.Context, relay string, g globalFlags) (client.Discovery, error) {
	hc, err := relayHTTPClient(g)
	if err != nil {
		return client.Discovery{}, err
	}
	return client.Discover(ctx, hc, relay)
}

// tokenFileError says that the file BURROW_TOKEN_FILE names cannot be read.
type tokenFileError struct{ err error }

func (e *tokenFileError) Error() string { return "BURROW_TOKEN_FILE: " + e.err.Error() }
func (e *tokenFileError) Unwrap() error { return e.err }

// logFormatFlag returns the value of --log when it was given.
func logFormatFlag(cmd *cobra.Command) (format string, given bool, err error) {
	f := cmd.Flags().Lookup("log")
	if f == nil || !f.Changed {
		return "", false, nil
	}
	v := strings.ToLower(strings.TrimSpace(f.Value.String()))
	if v != "text" && v != "json" {
		return "", false, usageErrorf("--log must be text or json")
	}
	return v, true, nil
}

// readGlobals collects the global flags of a command. Without --log the level
// and the format come from BURROW_LOG_LEVEL and BURROW_LOG_FORMAT, as they do
// for `connect`.
func readGlobals(cmd *cobra.Command, d deps) (globalFlags, error) {
	g := globalFlags{logLevel: "info", logFormat: "text"}
	if v := strings.TrimSpace(d.getenv("BURROW_LOG_LEVEL")); v != "" {
		g.logLevel = v
	}
	if v := strings.TrimSpace(d.getenv("BURROW_LOG_FORMAT")); v != "" {
		g.logFormat = v
	}
	format, given, err := logFormatFlag(cmd)
	if err != nil {
		return globalFlags{}, err
	}
	if given {
		g.logFormat = format
	}
	// The status view replaces the log lines when stdout is a terminal and
	// nothing asks for log lines: neither --log nor one of the log variables.
	g.view = !given && strings.TrimSpace(d.getenv("BURROW_LOG_FORMAT")) == "" &&
		strings.TrimSpace(d.getenv("BURROW_LOG_LEVEL")) == "" &&
		d.viewTerminal != nil && d.viewTerminal()
	g.cacert, _ = cmd.Flags().GetString("cacert")
	g.serverName, _ = cmd.Flags().GetString("server-name")
	g.insecure, _ = cmd.Flags().GetBool("insecure")
	return g, nil
}

// userConfigFile returns the user config path for a command of the root: the
// global --config when given, the default place otherwise.
func userConfigFile(cmd *cobra.Command, d deps) (string, error) {
	override, _ := cmd.Flags().GetString("config")
	return d.userConfigPath(override)
}

// resolveCredentials picks the control endpoint and the token for a command:
// the environment first, then burrow.yaml (file, for `up`), then the user
// config at userPath.
func resolveCredentials(d deps, userPath string, file *client.FileConfig) (client.Credentials, error) {
	s := client.Sources{
		EnvServer: d.getenv("BURROW_SERVER"),
		EnvToken:  d.getenv("BURROW_TOKEN"),
	}
	// As in the relay's configuration, the _FILE form wins over the plain one.
	if p := d.getenv("BURROW_TOKEN_FILE"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return client.Credentials{}, &tokenFileError{err: err}
		}
		s.EnvToken = string(b)
	}
	if file != nil {
		s.FileServer, s.FileToken = file.Server, file.Token
	}

	user, userErr := client.LoadUserConfig(userPath)
	if userErr == nil {
		s.User = &user
	}
	creds, err := client.Resolve(s)
	if err != nil && userErr != nil && !errors.Is(userErr, os.ErrNotExist) {
		// The stored sign-in was needed and cannot be read: say that rather
		// than "not signed in".
		return client.Credentials{}, userErr
	}
	return creds, err
}

// foreground runs the client through d.run for a command that stays in the
// foreground. Ctrl-C is how such a command is meant to end, so the cancelled
// context is not an error.
func foreground(cmd *cobra.Command, d deps, creds client.Credentials, tunnels []client.TunnelSpec, g globalFlags) error {
	err := d.run(cmd.Context(), creds, tunnels, g)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
