// Command burrow is the Burrow local client CLI.
//
// `login` stores a sign-in once; `http`, `tcp` and `up` then expose local
// services with one command each. `connect` is the explicit form with every
// value on the command line.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
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

// errControlKeys reports a key at the hidden prompt that moves the cursor or
// has some other function, such as an arrow key. It would change a line the
// user cannot see, so the line is not used.
var errControlKeys = errors.New("the input contained control keys")

// The markers a terminal puts around pasted text in bracketed paste mode.
var pasteStartMark, pasteEndMark = []byte("\x1b[200~"), []byte("\x1b[201~")

// keyLog passes the keys through and keeps them, to tell afterwards what the
// line editor does not report: Ctrl-C, and escape sequences.
type keyLog struct {
	r    io.Reader
	seen []byte
}

func (k *keyLog) Read(p []byte) (int, error) {
	n, err := k.r.Read(p)
	k.seen = append(k.seen, p[:n]...)
	return n, err
}

// readHiddenLine reads one line of keys as a terminal in raw mode delivers
// them. The editing is that of x/term's line editor: Backspace, Ctrl-U,
// Ctrl-W, pasted text with or without paste markers, Enter as CR, LF or CRLF.
// Ctrl-D on an empty line is io.EOF, Ctrl-C is errInterrupted, and any other
// escape sequence is errControlKeys.
func readHiddenLine(r io.Reader) (string, error) {
	keys := &keyLog{r: r}
	defer func() { clear(keys.seen) }()
	// The editor's own output (line ends, cursor movement) is not wanted.
	editor := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{keys, io.Discard}, "")
	line, err := editor.ReadPassword("")
	if errors.Is(err, term.ErrPasteIndicator) {
		err = nil // the line was pasted, which is the usual way a token gets here
	}
	typed := bytes.ReplaceAll(bytes.ReplaceAll(keys.seen, pasteStartMark, nil), pasteEndMark, nil)
	switch {
	case err != nil && bytes.IndexByte(typed, 0x03) >= 0:
		return "", errInterrupted
	case bytes.IndexByte(typed, 0x1b) >= 0:
		return "", errControlKeys
	}
	return line, err
}

// readSecretFromTerminal reads one line from the terminal without showing it.
//
// The terminal is in raw mode while the line is read: no echo, and Ctrl-C and
// Ctrl-D arrive as keys. It is put back on every way out: on return, on a
// panic, and when a signal ends the process, which then exits with 128 plus
// the signal's number as a shell reports it.
func readSecretFromTerminal() (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	restore := sync.OnceFunc(func() { _ = term.Restore(fd, state) })
	defer restore()
	quit := func(sig syscall.Signal) {
		restore()
		fmt.Fprintln(os.Stderr)
		os.Exit(128 + int(sig))
	}

	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer func() {
		signal.Stop(sig)
		close(done)
	}()
	go func() {
		select {
		case s := <-sig:
			n, _ := s.(syscall.Signal)
			quit(n)
		case <-done:
		}
	}()

	line, err := readHiddenLine(os.Stdin)
	if errors.Is(err, errInterrupted) {
		quit(syscall.SIGINT)
	}
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
