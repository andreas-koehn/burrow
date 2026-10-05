package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/config"
)

// buildTunnelSpec constructs a client.TunnelSpec from the provided flags values,
// validating that typ is one of "tcp" or "http".
func buildTunnelSpec(name string, remotePort int, localAddr string, typ string) (client.TunnelSpec, error) {
	if typ != "tcp" && typ != "http" {
		return client.TunnelSpec{}, fmt.Errorf("unknown tunnel type %q: must be tcp or http", typ)
	}
	return client.TunnelSpec{
		Name:       name,
		Type:       typ,
		RemotePort: remotePort,
		LocalAddr:  localAddr,
	}, nil
}

// singleFlagNames lists the per-tunnel flags that conflict with --config.
var singleFlagNames = []string{"server", "token", "local", "remote", "name", "type"}

// newConnectCmd constructs the "connect" sub-command.
func newConnectCmd() *cobra.Command { return newConnectCmdWith(defaultDeps()) }

// newConnectCmdWith is newConnectCmd with replaceable surroundings.
//
// The command is the explicit form that existed before `http`, `tcp` and `up`
// and behaves as it always has: its own --config is the path to burrow.yaml
// (it hides the root's --config), --insecure, --cacert and --server-name are
// its own flags, and its errors, flag errors included, end in "error: …" and
// exit code 1.
func newConnectCmdWith(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to a Burrow server and register a tunnel",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, _ := cmd.Flags().GetString("config")
			logFormat, logGiven, err := logFormatFlag(cmd)
			if err != nil {
				return err
			}

			if cfgPath != "" {
				// --config mode: reject any combination with single-tunnel flags.
				for _, flag := range singleFlagNames {
					if cmd.Flags().Changed(flag) {
						return fmt.Errorf("--config cannot be combined with --%s", flag)
					}
				}

				fc, err := client.LoadFileConfig(cfgPath)
				if err != nil {
					return err
				}

				g := globalFlags{logLevel: "info", logFormat: "text"}
				g.insecure, _ = cmd.Flags().GetBool("insecure")
				g.cacert, _ = cmd.Flags().GetString("cacert")
				g.serverName, _ = cmd.Flags().GetString("server-name")
				if logGiven {
					g.logFormat = logFormat
				}

				// A file with server and token uses exactly those, as before.
				creds := client.Credentials{Control: fc.Server, Token: fc.Token, Source: client.SourceFile}
				if fc.Server == "" || fc.Token == "" {
					// What the file leaves out comes from the sign-in, as for `burrow up`.
					userPath, err := d.userConfigPath("")
					if err != nil {
						return err
					}
					if creds, err = resolveCredentials(d, userPath, &fc); err != nil {
						return err
					}
				}
				return runClient(cmd.Context(), creds, fc.Tunnels, g)
			}

			// Single-tunnel mode (original path).
			server, _ := cmd.Flags().GetString("server")
			token, _ := cmd.Flags().GetString("token")
			local, _ := cmd.Flags().GetString("local")
			remote, _ := cmd.Flags().GetInt("remote")
			name, _ := cmd.Flags().GetString("name")
			insecure, _ := cmd.Flags().GetBool("insecure")
			caPath, _ := cmd.Flags().GetString("cacert")
			serverName, _ := cmd.Flags().GetString("server-name")
			typ, _ := cmd.Flags().GetString("type")

			spec, err := buildTunnelSpec(name, remote, local, typ)
			if err != nil {
				return err
			}

			cfg, err := config.LoadClient(map[string]any{
				"server": server, "token": token, "insecure": insecure,
				"cacert": caPath, "server_name": serverName,
			})
			if err != nil {
				return err
			}
			g := globalFlags{
				logLevel: cfg.LogLevel, logFormat: cfg.LogFormat,
				cacert: cfg.CACert, serverName: cfg.ServerName, insecure: cfg.Insecure,
			}
			if logGiven {
				g.logFormat = logFormat
			}
			creds := client.Credentials{Control: cfg.Server, Token: cfg.Token, Source: client.SourceFlags}
			return runClient(cmd.Context(), creds, []client.TunnelSpec{spec}, g)
		},
	}
	cmd.Flags().String("config", "", "path to burrow.yaml (multi-service)")
	cmd.Flags().String("server", "", "server host:port (required without --config)")
	cmd.Flags().String("token", "", "auth token (required without --config)")
	cmd.Flags().String("local", "127.0.0.1:3000", "local address to expose")
	cmd.Flags().Int("remote", 0, "requested remote port (0 = auto)")
	cmd.Flags().String("name", "", "tunnel name")
	cmd.Flags().Bool("insecure", false, "skip TLS verification (DEV ONLY)")
	cmd.Flags().String("cacert", "", "PEM CA to trust (e.g. certs/dev-ca.pem)")
	cmd.Flags().String("server-name", "", "TLS SNI/verify name (default: host of --server)")
	cmd.Flags().String("type", "tcp", "tunnel type: tcp|http (--remote is ignored for http)")
	// --server and --token are required only in single-tunnel mode; cobra's MarkFlagRequired
	// applies to every invocation, so we enforce the requirement manually inside RunE instead.

	// Flag errors stay plain errors here; the root turns them into usage errors
	// for the other commands.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return err })
	return cmd
}
