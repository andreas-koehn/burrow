// Command burrow is the Burrow local client CLI.
//
// `login` stores a sign-in once; `http`, `tcp` and `up` then expose local
// services with one command each. `connect` is the explicit form with every
// value on the command line.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

func versionLine() string {
	return fmt.Sprintf("burrow %s (commit %s, built %s, %s/%s)",
		version.Version, version.Commit, version.Date, runtime.GOOS, runtime.GOARCH)
}

// deps is what the commands take from their surroundings. Tests replace it so
// that no command reads the real environment, the real user config or a
// terminal, or opens a connection.
type deps struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	hostname       func() (string, error)
	userConfigPath func(override string) (string, error)
	getenv         func(string) string
	// run connects and blocks until the client stops.
	run func(ctx context.Context, creds client.Credentials, tunnels []client.TunnelSpec, g globalFlags) error
	// isTerminal reports whether stdin is a terminal a person can answer on.
	isTerminal func() bool
}

func defaultDeps() deps {
	return deps{
		stdout:         os.Stdout,
		stderr:         os.Stderr,
		stdin:          os.Stdin,
		hostname:       os.Hostname,
		userConfigPath: client.UserConfigPath,
		getenv:         os.Getenv,
		run:            runClient,
		isTerminal: func() bool {
			fi, err := os.Stdin.Stat()
			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
	}
}

// newRoot builds the command tree.
func newRoot(d deps) *cobra.Command {
	root := &cobra.Command{
		Use:           "burrow",
		Short:         "Burrow local client",
		Version:       versionLine(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageErrorf("unknown command %q for %q\nRun '%s --help' for the commands.", args[0], cmd.CommandPath(), cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetOut(d.stdout)
	root.SetErr(d.stderr)
	root.SetIn(d.stdin)
	root.SetFlagErrorFunc(flagUsageError)

	pf := root.PersistentFlags()
	pf.String("log", "", "print log lines in this format: text|json")
	pf.String("config", "", "user config file (default: config.yaml in the burrow directory of the user config directory)")
	pf.String("cacert", "", "PEM CA to trust (e.g. certs/dev-ca.pem)")
	pf.String("server-name", "", "TLS SNI/verify name (default: host of the control endpoint)")
	pf.Bool("insecure", false, "skip TLS verification (DEV ONLY)")

	root.AddCommand(
		newLoginCmd(d),
		newLogoutCmd(d),
		newExposeCmd(d, "http"),
		newExposeCmd(d, "tcp"),
		newUpCmd(d),
		newStatusCmd(d),
		newConnectCmdWith(d),
		&cobra.Command{
			Use:   "version",
			Short: "Print version information",
			Run: func(cmd *cobra.Command, _ []string) {
				fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			},
		},
	)
	return root
}

func main() {
	os.Exit(report(os.Stderr, newRoot(defaultDeps()).Execute()))
}
