package main

import (
	"fmt"
	"strings"

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

// newStatusCmd builds `burrow status`.
func newStatusCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show what this machine is signed in to",
		Long: "Show what this machine is signed in to, without connecting.\n\n" +
			"Exit code 0 when signed in, 3 when not.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, _, err := logFormatFlag(cmd); err != nil {
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
			}
			out := cmd.OutOrStdout()
			// Named fields only: the credentials are never printed as a whole.
			fmt.Fprintf(out, "Relay:    %s\n", relay)
			fmt.Fprintf(out, "Control:  %s\n", creds.Control)
			fmt.Fprintf(out, "Token:    %s\n", tokenLabel(creds.TokenName, creds.Token))
			fmt.Fprintf(out, "Source:   %s\n", creds.Source)
			fmt.Fprintf(out, "Client:   %s\n", versionLine())
			return nil
		},
	}
}
