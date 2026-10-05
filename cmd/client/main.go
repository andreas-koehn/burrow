// Command burrow is the Burrow local client CLI.
//
// `login` stores a sign-in once; `http`, `tcp` and `up` then expose local
// services with one command each. `connect` is the explicit form with every
// value on the command line.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/term"

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
	// readSecret reads one line from the terminal without showing it.
	readSecret func() (string, error)
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
		isTerminal:     func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		readSecret:     readSecretFromTerminal,
	}
}

// errInterrupted reports Ctrl-C at the hidden prompt.
var errInterrupted = errors.New("interrupted")

// readHidden reads one line of keys as a terminal in raw mode delivers them:
// Enter ends the line, backspace removes a character, Ctrl-D on an empty line
// is the end of input (io.EOF) and Ctrl-C is errInterrupted. Nothing beyond
// the end of the line is read.
func readHidden(r io.Reader) (string, error) {
	var line []byte
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			switch c := buf[0]; {
			case c == '\r' || c == '\n':
				return string(line), nil
			case c == 0x03:
				return "", errInterrupted
			case c == 0x04:
				if len(line) == 0 {
					return "", io.EOF
				}
				return string(line), nil
			case c == 0x7f || c == 0x08:
				if len(line) > 0 {
					line = line[:len(line)-1]
				}
			case c >= 0x20:
				line = append(line, c)
			}
		}
		if err != nil || n == 0 {
			if len(line) > 0 {
				return string(line), nil
			}
			return "", io.EOF
		}
	}
}

// readSecretFromTerminal reads one line from the terminal without showing it.
//
// The terminal is put into raw mode, which has no echo and hands Ctrl-C and
// Ctrl-D over as keys, and is put back before this returns. A termination
// signal in between restores it too: the user's shell is never left without
// echo. Ctrl-C ends the process the way an interrupt does.
func readSecretFromTerminal() (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	interrupted := func() {
		_ = term.Restore(fd, state)
		fmt.Fprintln(os.Stderr)
		os.Exit(130)
	}
	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sig:
			interrupted()
		case <-done:
		}
	}()
	line, err := readHidden(os.Stdin)
	signal.Stop(sig)
	close(done)
	if errors.Is(err, errInterrupted) {
		interrupted()
	}
	_ = term.Restore(fd, state)
	return line, err
}

// newRoot builds the command tree.
func newRoot(d deps) *cobra.Command {
	root := &cobra.Command{
		Use:           "burrow",
		Short:         "Burrow local client",
		Version:       versionLine(),
		SilenceUsage:  true,
		SilenceErrors: true,
		// SuggestionsFor does not apply cobra's default distance by itself.
		SuggestionsMinimumDistance: 2,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			// cobra's own wording, suggestions included, as a usage error.
			msg := fmt.Sprintf("unknown command %q for %q", args[0], cmd.CommandPath())
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				msg += "\n\nDid you mean this?\n"
				for _, name := range s {
					msg += "\t" + name + "\n"
				}
			}
			return usageErrorf("%s\nRun '%s --help' for usage.", strings.TrimRight(msg, "\n")+"\n", cmd.CommandPath())
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
		newConnectCmd(),
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
