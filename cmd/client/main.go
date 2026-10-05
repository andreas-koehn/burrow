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
	// viewTerminal reports whether stdout is a terminal that can show the
	// status view.
	viewTerminal func() bool
	// discover asks the relay at its dashboard address (https://host[:port])
	// where its control endpoint is and which version it runs, with the TLS
	// settings of g. It sends no token.
	discover func(ctx context.Context, relay string, g globalFlags) (client.Discovery, error)
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
		viewTerminal:   func() bool { return stdoutShowsView() },
		discover:       discoverRelay,
	}
}

// errInterrupted reports Ctrl-C at the hidden prompt.
var errInterrupted = errors.New("interrupted")

// errControlKeys reports that the line typed at the hidden prompt held
// something that cannot be part of a token: a key with a function, such as an
// arrow key or Esc, or a character outside printable ASCII. The line is refused
// as a whole; nothing of it is used.
var errControlKeys = errors.New("the input contained something other than a token")

// maxHiddenLine is the longest line the hidden prompt accepts.
const maxHiddenLine = 4096

// pasteMarker reports whether c is byte i (1 to 5) of one of the two markers a
// terminal puts around pasted text, ESC [ 2 0 0 ~ and ESC [ 2 0 1 ~.
func pasteMarker(i int, c byte) bool {
	switch i {
	case 1:
		return c == '['
	case 2:
		return c == '2'
	case 3:
		return c == '0'
	case 4:
		return c == '0' || c == '1'
	case 5:
		return c == '~'
	}
	return false
}

// readHiddenLine reads one line of keys as a terminal in raw mode delivers
// them, one byte at a time and never beyond the byte that ends it.
//
// A token is printable ASCII, so the line is bytes: 0x21 to 0x7e are kept,
// Backspace and Ctrl-H delete one, Ctrl-U and Ctrl-W clear the line, CR or LF
// ends it. The markers of a bracketed paste are dropped and what is between
// them is treated like typed keys. Anything else (Esc and the sequences that
// start with it, other control characters, space, bytes from 0x80 on, Ctrl-D
// in the middle, more than maxHiddenLine characters) refuses the line: reading
// goes on, nothing waits for a sequence to end, and the line end then gives
// errControlKeys. Ctrl-C gives errInterrupted at once, in every state. Ctrl-D
// on an untouched empty line gives io.EOF. When the input ends or fails, that
// error is returned and never a part of a line.
func readHiddenLine(r io.Reader) (string, error) {
	var line []byte
	defer func() { clear(line[:cap(line)]) }()
	refused := false
	marker := 0 // bytes of a paste marker seen so far, the ESC included
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 0 {
			if err == nil {
				err = io.EOF // a reader that gives nothing and no reason has ended
			}
			return "", err
		}
		c := buf[0]
		if marker > 0 {
			if pasteMarker(marker, c) {
				marker = (marker + 1) % 6 // the sixth byte completes the marker
				continue
			}
			// Some other sequence: the line is refused, and c is a key like any other.
			marker, refused = 0, true
		}
		switch {
		case c == 0x1b:
			marker = 1
		case c == '\r' || c == '\n':
			if refused {
				return "", errControlKeys
			}
			return string(line), nil
		case c == 0x03:
			return "", errInterrupted
		case c == 0x04:
			if len(line) == 0 && !refused {
				return "", io.EOF
			}
			refused = true
		case c == 0x7f || c == 0x08:
			if len(line) > 0 {
				line = line[:len(line)-1]
			}
		case c == 0x15 || c == 0x17:
			line = line[:0]
		case c >= 0x21 && c <= 0x7e && len(line) < maxHiddenLine:
			line = append(line, c)
		default:
			refused = true
		}
		if err != nil {
			return "", err
		}
	}
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
	quit := func(sig syscall.Signal) {
		restore()
		fmt.Fprintln(os.Stderr)
		os.Exit(128 + int(sig))
	}

	sig := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer func() {
		// The terminal first: a signal that still arrives before the
		// notification stops finds it restored and ends the process as above.
		restore()
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
		newDoctorCmd(d),
		newUpdateCmd(d),
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
	removeReplacedAtStart(runtime.GOOS)
	os.Exit(report(os.Stderr, newRoot(defaultDeps()).Execute()))
}
