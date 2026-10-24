package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
)

const (
	msgSlugRule      = "--slug " + client.SlugRule
	msgAccessRule    = "--access " + client.AccessRule
	msgTCPAccessRule = "--access " + client.TCPAccessRule
	// defaultTCPAccess is the mode a new service of `burrow tcp` without
	// --access is created in (D2): it gets no public port until its owner
	// says so. It is sent as TunnelSpec.AccessDefault, no wish.
	defaultTCPAccess = "private"
)

// newExposeCmd builds `burrow http <target>` (kind "http"), `burrow tcp
// <target>` (kind "tcp") and `burrow grpc <target>` (kind "grpc": an http
// service whose upstream protocol is h2c, or h2 for an https:// target).
func newExposeCmd(d deps, kind string) *cobra.Command {
	typ, grpc := kind, kind == "grpc"
	if grpc {
		typ = "http"
	}
	short := "Expose a local HTTP service"
	switch {
	case typ == "tcp":
		short = "Expose a local TCP port"
	case grpc:
		short = "Expose a local gRPC service"
	}
	cmd := &cobra.Command{
		Use:   kind + " <target>",
		Short: short,
		Long: short + ".\n\n<target> is what is listening locally:\n" +
			"  3000                    127.0.0.1:3000\n" +
			"  localhost:3000          that host and port\n" +
			"  192.168.1.20:8080       that host and port\n" +
			"  http://localhost:3000   the same as localhost:3000\n" +
			upstreamLong(kind) + "\n" +
			"The command runs in the foreground until interrupted and reconnects by itself." + tcpLong(typ),
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageWithLine(cmd, "%s needs exactly one target, for example: %s 3000", cmd.CommandPath(), cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			// Each flag belongs to one kind of service.
			if typ == "tcp" {
				if flags.Changed("slug") {
					return usageErrorf("--slug applies to http services")
				}
				if flags.Changed("upstream-protocol") {
					return usageErrorf("--upstream-protocol applies to http services")
				}
			} else if flags.Changed("remote") {
				return usageErrorf("--remote applies to tcp services")
			} else if flags.Changed("proxy-protocol") {
				return usageErrorf("--proxy-protocol applies to tcp services")
			}
			proxyProtocol, _ := flags.GetString("proxy-protocol")
			if flags.Changed("proxy-protocol") && proxyProtocol != client.ProxyProtocolV2 {
				return usageErrorf("--proxy-protocol must be v2")
			}

			local, scheme, err := client.ParseTargetProtocol(args[0])
			if err != nil {
				return err
			}
			upstream := ""
			if typ == "tcp" {
				if scheme != "" {
					return &client.TargetError{Input: args[0], Reason: scheme + " targets are for http services; a tcp service is forwarded as it is"}
				}
			} else {
				flag, _ := flags.GetString("upstream-protocol")
				if upstream, err = client.ResolveUpstream(scheme, flag, grpc); err != nil {
					return usageErrorf("%s", err.Error())
				}
			}

			// The relay checks both again; this is for a quick answer.
			slug, _ := flags.GetString("slug")
			access, _ := flags.GetString("access")
			mode, accessDefault := "", ""
			switch {
			case typ == "tcp":
				// Without --access nothing is wished: a service that exists is
				// served in its stored mode, and a new one is created private.
				accessDefault = defaultTCPAccess
				if flags.Changed("access") {
					m, ok := client.TCPAccessMode(access)
					if !ok {
						return usageErrorf(msgTCPAccessRule)
					}
					mode, accessDefault = m, ""
				}
			case flags.Changed("access"):
				m, ok := client.AccessMode(access)
				if !ok {
					return usageErrorf(msgAccessRule)
				}
				mode = m
			}
			if !flags.Changed("slug") {
				slug = ""
			} else if !client.ValidSlug(slug) {
				return usageErrorf(msgSlugRule)
			}
			remote, _ := flags.GetInt("remote")
			if remote < 0 || remote > 65535 {
				return usageErrorf("--remote must be a port between 1 and 65535, or 0 for any free port")
			}
			if typ == "tcp" && remote != 0 && (mode == "private" || accessDefault == "private") {
				return usageErrorf("--remote needs --access open or ip: a private service has no public port, and private is the default")
			}

			name, _ := flags.GetString("name")
			name = strings.TrimSpace(name)
			if flags.Changed("name") && name == "" {
				return usageErrorf("--name must not be empty")
			}
			if name == "" {
				host, _ := d.hostname() // without a hostname the name starts with "burrow"
				name = client.DefaultName(host, local)
			}

			g, err := readGlobals(cmd, d)
			if err != nil {
				return err
			}
			userPath, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			creds, err := resolveCredentials(d, userPath, nil)
			if err != nil {
				return err
			}

			spec := client.TunnelSpec{Name: name, Type: typ, LocalAddr: local, RemotePort: remote, Slug: slug, Access: mode,
				AccessDefault: accessDefault, ProxyProtocol: proxyProtocol,
				UpstreamProtocol: upstream}
			return foreground(cmd, d, creds, []client.TunnelSpec{spec}, g)
		},
	}
	f := cmd.Flags()
	f.String("name", "", "service name (default: <hostname>-<port>)")
	f.String("slug", "", "path under /svc/; used only when the service is created")
	if typ == "tcp" {
		f.String("access", defaultTCPAccess, "access mode: open|ip|private; private opens no public port (reach it with burrow forward)")
	} else {
		f.String("access", "", "access mode: open|login|api-key; used only when the service is created")
	}
	if typ == "http" {
		f.String("upstream-protocol", "", "http1|https|h2c|h2: how the relay speaks to the local service (default: the service's setting, http1)")
	} else {
		f.String("upstream-protocol", "", "")
	}
	f.Int("remote", 0, "fixed public port with --access open or ip (default: any free port)")
	f.String("proxy-protocol", "", "v2: send a PROXY protocol v2 header with the visitor's address to the target first")
	// A flag of the other kind is known, so that it gets a message that says
	// where it belongs, but it is not listed in the help.
	hidden := []string{"remote", "proxy-protocol"}
	if typ == "tcp" {
		hidden = []string{"slug", "upstream-protocol"}
	}
	for _, name := range hidden {
		_ = f.MarkHidden(name)
	}
	return cmd
}

// tcpLong is what the help of `burrow tcp` adds: the access modes.
func tcpLong(typ string) string {
	if typ != "tcp" {
		return ""
	}
	return "\n\nAccess modes (--access):\n" +
		"  private   the default: no public port; reach it with burrow forward <slug>\n" +
		"  ip        a public port that takes the addresses allowed in the dashboard only\n" +
		"  open      a public port anyone can connect to\n" +
		"Without --access a service that exists is served in the mode set in the dashboard.\n" +
		"With --access, a mode set in the dashboard that is stricter is kept; a looser one is not\n" +
		"widened by the client: the relay asks to restart with that mode."
}

// upstreamLong is what the help of `burrow http` and `burrow grpc` says of
// the upstream protocol.
func upstreamLong(kind string) string {
	switch kind {
	case "tcp":
		return ""
	case "grpc":
		return "  h2c://localhost:50051   plain HTTP/2, the default of burrow grpc\n" +
			"  https://localhost:50443 HTTP/2 over TLS (h2)\n\n" +
			"burrow grpc is burrow http with the upstream protocol h2c, or h2 for an https:// target. " +
			"gRPC visitors need a host name of their own: add a custom domain to the service in the dashboard.\n"
	}
	return "  https://localhost:8443  HTTP/1.1 over TLS, or HTTP/2 over TLS with --upstream-protocol h2\n" +
		"  h2c://localhost:50051   plain HTTP/2 (prior knowledge)\n\n" +
		"--upstream-protocol http1|https|h2c|h2 sets how the relay speaks to the local service; " +
		"without it the service's setting in the dashboard applies (http1 by default).\n"
}
