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
	// defaultTCPAccess is the mode `burrow tcp` asks for without --access
	// (D2): a new service gets no public port until its owner says so.
	defaultTCPAccess = "private"
)

// newExposeCmd builds `burrow http <target>` (typ "http") and
// `burrow tcp <target>` (typ "tcp").
func newExposeCmd(d deps, typ string) *cobra.Command {
	short := "Expose a local HTTP service"
	if typ == "tcp" {
		short = "Expose a local TCP port"
	}
	cmd := &cobra.Command{
		Use:   typ + " <target>",
		Short: short,
		Long: short + ".\n\n<target> is what is listening locally:\n" +
			"  3000                    127.0.0.1:3000\n" +
			"  localhost:3000          that host and port\n" +
			"  192.168.1.20:8080       that host and port\n" +
			"  http://localhost:3000   the same as localhost:3000\n\n" +
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
			} else if flags.Changed("remote") {
				return usageErrorf("--remote applies to tcp services")
			}

			local, err := client.ParseTarget(args[0])
			if err != nil {
				return err
			}

			// The relay checks both again; this is for a quick answer.
			slug, _ := flags.GetString("slug")
			access, _ := flags.GetString("access")
			mode := ""
			switch {
			case typ == "tcp":
				mode = defaultTCPAccess
				if flags.Changed("access") {
					m, ok := client.TCPAccessMode(access)
					if !ok {
						return usageErrorf(msgTCPAccessRule)
					}
					mode = m
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
			if typ == "tcp" && remote != 0 && mode == "private" {
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

			spec := client.TunnelSpec{Name: name, Type: typ, LocalAddr: local, RemotePort: remote, Slug: slug, Access: mode}
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
	f.Int("remote", 0, "fixed public port with --access open or ip (default: any free port)")
	// A flag of the other kind is known, so that it gets a message that says
	// where it belongs, but it is not listed in the help.
	hidden := []string{"remote"}
	if typ == "tcp" {
		hidden = []string{"slug"}
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
		"A mode set in the dashboard that is stricter than --access is kept; a looser one is not\n" +
		"widened by the client: the relay asks to restart with that mode."
}
