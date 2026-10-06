package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
)

// tokenLabel describes a token by its name and its last four characters, never
// more. Either part may be missing; the result has no empty parentheses.
func tokenLabel(name, token string) string {
	name = strings.TrimSpace(name)
	tail := client.TokenTail(token)
	switch {
	case name != "" && tail != "":
		return name + " (…" + tail + ")"
	case name != "":
		return name
	case tail != "":
		return "…" + tail
	}
	return "set"
}

// statusDiscoveryTimeout is how long `status` waits for the relay's version.
var statusDiscoveryTimeout = 3 * time.Second

// relayVersion asks the relay at its dashboard address which version it runs.
// Any failure gives "unknown": status reports the sign-in, not the network.
func relayVersion(ctx context.Context, d deps, relay string, g globalFlags) string {
	if d.discover == nil {
		return "unknown"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, statusDiscoveryTimeout)
	defer cancel()
	disc, err := d.discover(ctx, relay, g)
	if err != nil || disc.Version == "" {
		return "unknown"
	}
	return disc.Version
}

// newStatusCmd builds `burrow status`.
func newStatusCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what this machine is signed in to",
		Long: "Show what this machine is signed in to, and whether the system service\n" +
			"(burrow service) is installed and running, without connecting.\n\n" +
			"The relay is asked for its version at its web address; the token is not sent.\n" +
			"When the relay does not answer, the version is shown as unknown.\n\n" +
			"Exit code 0 when signed in, 3 when not.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			relay := strings.TrimSpace(creds.Relay)
			if relay == "" {
				relay = "not known"
			} else {
				relay += " (version " + relayVersion(cmd.Context(), d, relay, g) + ")"
			}
			out := cmd.OutOrStdout()
			// Named fields only: the credentials are never printed as a whole.
			fmt.Fprintf(out, "Relay:    %s\n", relay)
			fmt.Fprintf(out, "Control:  %s\n", creds.Control)
			fmt.Fprintf(out, "Token:    %s\n", tokenLabel(creds.TokenName, creds.Token))
			fmt.Fprintf(out, "Source:   %s\n", creds.Source)
			fmt.Fprintf(out, "Client:   %s\n", versionLine())
			if d.service != nil {
				_, st, err := d.service.state()
				fmt.Fprintf(out, "Service:  %s\n", describeService(st, err))
			}
			return nil
		},
	}
}
