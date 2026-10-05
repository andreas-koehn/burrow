package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
)

// accessModes maps the names of --access to the relay's access modes.
var accessModes = map[string]string{
	"open":    "open",
	"login":   "burrow_login",
	"api-key": "api_key",
}

// slugRe is the relay's rule for a service slug (auth.ValidSlug). It is
// repeated here so that the client does not link the relay's auth package.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

const (
	msgSlugRule   = "--slug must be 3 to 40 characters: lowercase letters, digits and hyphens, starting and ending with a letter or digit"
	msgAccessRule = "--access must be one of: open, login, api-key"
	// msgCreateOptionsLater is printed until the protocol carries the options.
	msgCreateOptionsLater = "Note: --slug and --access are applied once the relay supports it; this version does not send them yet."
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
			"The command runs in the foreground until interrupted and reconnects by itself.",
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
				for _, name := range []string{"slug", "access"} {
					if flags.Changed(name) {
						return usageErrorf("--%s applies to http services", name)
					}
				}
			} else if flags.Changed("remote") {
				return usageErrorf("--remote applies to tcp services")
			}

			local, err := client.ParseTarget(args[0])
			if err != nil {
				return err
			}

			slug, _ := flags.GetString("slug")
			access, _ := flags.GetString("access")
			if flags.Changed("access") {
				if _, ok := accessModes[access]; !ok {
					return usageErrorf(msgAccessRule)
				}
			}
			if flags.Changed("slug") && !slugRe.MatchString(slug) {
				return usageErrorf(msgSlugRule)
			}
			remote, _ := flags.GetInt("remote")
			if remote < 0 || remote > 65535 {
				return usageErrorf("--remote must be a port between 1 and 65535, or 0 for any free port")
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

			if flags.Changed("slug") || flags.Changed("access") {
				fmt.Fprintln(cmd.ErrOrStderr(), msgCreateOptionsLater)
			}
			spec := client.TunnelSpec{Name: name, Type: typ, LocalAddr: local, RemotePort: remote}
			return foreground(cmd, d, creds, []client.TunnelSpec{spec}, g)
		},
	}
	f := cmd.Flags()
	f.String("name", "", "service name (default: <hostname>-<port>)")
	f.String("slug", "", "path under /svc/; used only when the service is created")
	f.String("access", "", "access mode: open|login|api-key; used only when the service is created")
	f.Int("remote", 0, "fixed public port (default: any free port)")
	// A flag of the other kind is known, so that it gets a message that says
	// where it belongs, but it is not listed in the help.
	hidden := []string{"remote"}
	if typ == "tcp" {
		hidden = []string{"slug", "access"}
	}
	for _, name := range hidden {
		_ = f.MarkHidden(name)
	}
	return cmd
}
