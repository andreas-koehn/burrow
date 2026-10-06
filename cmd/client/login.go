package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// defaultControlPort is where the relay's control endpoint listens unless
// --control says otherwise.
const defaultControlPort = "7000"

const msgRelayForms = "Give the relay address as burrow.example.com or https://burrow.example.com"

// normalizeRelay turns what the user typed into the dashboard URL
// (https://host[:port]) and the host. It accepts host, host:port and
// https://host[:port][/]. The input is not repeated in errors: it may hold
// credentials.
func normalizeRelay(in string) (relay, host string, err error) {
	in = strings.TrimSpace(in)
	bad := func(reason string) (string, string, error) {
		return "", "", usageErrorf("Cannot use that relay address: %s.\n%s", reason, msgRelayForms)
	}
	if in == "" {
		return bad("it is empty")
	}
	if strings.Contains(in, "@") {
		return bad("it must not contain credentials")
	}
	if i := strings.Index(in, "://"); i >= 0 {
		if !strings.EqualFold(in[:i], "https") {
			return bad("the relay address must start with https://")
		}
		in = in[i+3:]
	}
	in = strings.TrimSuffix(in, "/")
	if strings.ContainsAny(in, "/?#") {
		return bad("it must not contain a path")
	}
	u, perr := url.Parse("https://" + in)
	if perr != nil || u.Hostname() == "" || u.Host != in ||
		strings.IndexFunc(in, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return bad("it is not a valid host name with an optional port")
	}
	if p := u.Port(); p != "" {
		if n, e := strconv.Atoi(p); e != nil || n < 1 || n > 65535 {
			return bad("the port must be a number between 1 and 65535")
		}
	} else if strings.HasSuffix(in, ":") {
		return bad("the port must be a number between 1 and 65535")
	}
	host = strings.ToLower(u.Hostname())
	return "https://" + strings.ToLower(u.Host), host, nil
}

// msgPasteToken is how a token gets in without passing the shell history or
// the process list. It takes the relay as typed after `burrow login`.
const msgPasteToken = "run: burrow login %s --token -\nthen paste the token and press Enter."

const (
	msgNotAToken    = "That is not a token: it contains a space or a character other than letters, digits and punctuation. Nothing was stored."
	msgControlKeys  = "The input contained something other than a token, such as an arrow key, Esc or a character that is not plain ASCII. Nothing was stored; run the command again and paste the token."
	msgTokenOneLine = "The token must be a single word on one line. Nothing was stored."
)

// readAnswer reads one line and nothing beyond it, byte by byte: on a terminal
// the input does not end after the line, and what is typed next belongs to
// whoever reads next. end reports that the input ended without a line.
func readAnswer(r io.Reader) (line string, end bool) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n == 1 {
			if buf[0] == '\n' {
				return strings.TrimSpace(b.String()), false
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			line = strings.TrimSpace(b.String())
			return line, line == ""
		}
	}
}

// readPipedToken reads a token from input that is not a terminal, to its end.
// Blank lines around the token are skipped; ok is false when more than one
// line has content.
func readPipedToken(r io.Reader) (token string, ok bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
		case token != "":
			return "", false
		default:
			token = line
		}
	}
	return token, sc.Err() == nil
}

// cleanToken checks a token as typed or read. The token is never repeated.
// relayArg is the relay as it goes after `burrow login` in the hint.
func cleanToken(token, relayArg string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", usageErrorf("The token is empty. Create one in the dashboard (Clients, tab Tokens), "+msgPasteToken, relayArg)
	}
	// A token is printable ASCII without spaces. Anything else is a slip of
	// the keyboard or the clipboard and would be stored as a token that fails.
	if !client.ValidToken(token) {
		return "", usageErrorf(msgNotAToken)
	}
	return token, nil
}

// discoverControl asks the relay for its discovery document and returns the
// control endpoint to store: the one given with --control, else the one the
// relay names, else fallback (<relay host>:7000) for a relay that answers 404
// because it is older than discovery; older reports that 404. Notes for the
// user go to errOut, one line each.
//
// Without --control the relay's answer is needed: when it cannot be asked (not
// reachable, an error status, a redirect) or what answers is not a relay, it
// fails with exit 5 and nothing is stored. With --control the user has said
// where to connect, so those are one warning line and fallback (the --control
// value) is returned.
//
// Two things fail either way: a certificate that is not trusted (exit 5), and
// a relay that did answer and needs a newer client (exit 6).
func discoverControl(ctx context.Context, d deps, g globalFlags, errOut io.Writer, relay, fallback string, controlGiven bool) (control string, older bool, err error) {
	control, err = askDiscovery(ctx, d, g, errOut, relay, fallback, controlGiven)
	if errors.Is(err, client.ErrNoDiscovery) {
		return fallback, true, nil
	}
	return control, false, err
}

// askDiscovery is discoverControl with the 404 of an older relay left as
// client.ErrNoDiscovery.
func askDiscovery(ctx context.Context, d deps, g globalFlags, errOut io.Writer, relay, fallback string, controlGiven bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	disc, err := d.discover(ctx, relay, g)
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return "", errLoginInterrupted
	}
	var se *client.DiscoveryStatusError
	// Only an answer went through a handshake, and only then was a
	// certificate left unchecked.
	answered := err == nil || errors.Is(err, client.ErrNoDiscovery) || errors.Is(err, client.ErrNotARelay) || errors.As(err, &se)
	if g.insecure && answered {
		fmt.Fprintln(errOut, "Warning: --insecure is set: the relay's certificate was not checked.")
	}
	// unasked ends a failed discovery: with --control it is a warning, without
	// it an error with the two lines given.
	unasked := func(what, hint string) (string, error) {
		if controlGiven {
			fmt.Fprintf(errOut, "Warning: %s; storing the sign-in for --control %s without checking the relay's versions.\n", what, fallback)
			return fallback, nil
		}
		return "", &exitError{code: exitUnreachable, msg: what + ".\n" + hint}
	}
	const orControl = "If the control endpoint is known, pass --control <host:port> to sign in without asking the relay's web address."
	switch {
	case err == nil:
		me := version.Version
		if c, ok := compareVersions(me, disc.MinClientVersion); ok && c < 0 {
			return "", &exitError{code: exitClientTooOld, msg: fmt.Sprintf(msgClientTooOld, disc.MinClientVersion)}
		}
		if c, ok := compareVersions(me, disc.Version); ok && c < 0 {
			fmt.Fprintf(errOut, "The relay runs %s; this client is %s. Run: burrow update\n", disc.Version, me)
		} else if ok && c > 0 {
			fmt.Fprintf(errOut, "The relay runs %s; this client is %s, which is newer.\n", disc.Version, me)
		}
		if controlGiven {
			return fallback, nil
		}
		// The token will go to the control endpoint. When the relay names one
		// on another host than its own, that is said, not just done.
		if !client.SameHost(disc.Control, relay) {
			fmt.Fprintf(errOut, "Note: this relay's control endpoint is on another host: %s. The token will be sent there, not to %s.\n",
				disc.Control, strings.TrimPrefix(relay, "https://"))
		}
		return disc.Control, nil
	case errors.Is(err, client.ErrNoDiscovery):
		return "", client.ErrNoDiscovery
	case errors.Is(err, client.ErrNotARelay):
		// Some web page answered. No control endpoint is guessed from that.
		return unasked(relay+" does not look like a Burrow relay",
			"Check the address. If it is right and the control endpoint is known, --control <host:port> overrides this.")
	case errors.As(err, &se):
		hint := "Check the relay address. " + orControl
		if se.RedirectHost != "" {
			hint = "If that is the relay, run: burrow login " + se.RedirectHost + "\n" + orControl
		}
		return unasked(relay+" cannot be asked: "+se.Error(), hint)
	}
	// A certificate that is not trusted is not a relay to be skipped past.
	if problem, ok := certProblem(err); ok {
		return "", &exitError{code: exitUnreachable, msg: "Cannot trust " + relay + ": " + problem + ".\n" + fixCacert}
	}
	reason := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "no answer within " + client.DiscoveryTimeout.String()
	}
	return unasked("Cannot reach "+relay+": "+reason, "Check the address and your network connection. "+orControl)
}

// errLoginInterrupted is Ctrl-C while the relay is asked or the approval is
// awaited.
var errLoginInterrupted = &exitError{code: exitGeneral, msg: "Interrupted. Nothing was stored."}

// interruptible returns a context that ends on Ctrl-C or SIGTERM, until stop
// is called.
func interruptible(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

// errNoBrowserLogin is the answer to a relay without browser sign-in: the way
// that works there. relayArg is the relay as it goes after `burrow login`.
func errNoBrowserLogin(relayArg string) error {
	return usageErrorf("This relay does not support browser sign-in. Create a token in the dashboard (Clients → Tokens) and run: burrow login %s --token -\n"+
		"then paste the token and press Enter.", relayArg)
}

// browserSignIn is what `login` has settled before the sign-in starts.
type browserSignIn struct {
	relay, relayArg, control, path string
	hostname                       string // as the machine reports it, "" when it does not
	name                           string // the token's name when the relay reports none
	nameGiven                      bool   // --name was given: it is suggested to the approval page
	noBrowser                      bool
}

// browserLogin signs the machine in through the browser: it opens a sign-in
// request on the relay, shows the page and the code, waits for the approval
// and stores the token.
//
// The device code of the request is a secret like the token. It stays inside
// client.DeviceLogin; only the user code and the page's address are shown.
func browserLogin(ctx context.Context, d deps, g globalFlags, out, errOut io.Writer, p browserSignIn) error {
	hc, err := d.relayHTTP(g)
	if err != nil {
		return err
	}
	// The polls share a connection; it is closed when the sign-in is over.
	defer hc.CloseIdleConnections()
	dl := client.DeviceLogin{HTTP: hc, Relay: p.relay, Sleep: d.sleep}
	dl.Meta.Hostname, dl.Meta.OS, dl.Meta.Arch, dl.Meta.ClientVersion = p.hostname, runtime.GOOS, runtime.GOARCH, version.Version
	if p.nameGiven {
		dl.Meta.TokenName = p.name
	}
	tokenHint := "If this goes on, create a token in the dashboard (Clients, tab Tokens) and run: burrow login " + p.relayArg + " --token -"

	start, err := dl.Start(ctx)
	var se *client.DeviceStatusError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return errLoginInterrupted
	case errors.Is(err, client.ErrNoBrowserLogin):
		return errNoBrowserLogin(p.relayArg)
	case errors.Is(err, client.ErrLoginBusy):
		return &exitError{code: exitGeneral, msg: "The relay has too many pending sign-ins. Try again in a minute."}
	case errors.As(err, &se), errors.Is(err, client.ErrNotASignIn):
		return &exitError{code: exitGeneral, msg: "The relay did not start a sign-in: " + err.Error() + ".\n" + tokenHint}
	default:
		if problem, ok := certProblem(err); ok {
			return &exitError{code: exitUnreachable, msg: "Cannot trust " + p.relay + ": " + problem + ".\n" + fixCacert}
		}
		return &exitError{code: exitUnreachable, msg: "Cannot reach " + p.relay + ": " + waitReason(err) + ".\nCheck the address and your network connection."}
	}

	if start.OwnURL {
		fmt.Fprintf(errOut, "Note: this relay named a sign-in page that is not on %s; the address below is the relay's own page.\n", p.relay)
	}
	fmt.Fprintf(out, "Open this page to sign this machine in:\n\n  %s\n\nCheck that the page shows the code %s.\n", start.VerificationURL, start.UserCode)
	// The browser is opened where somebody will see it. When it does not
	// open, the address is on the screen all the same.
	if !p.noBrowser && d.stdoutTerminal != nil && d.stdoutTerminal() && d.openBrowser != nil && desktopSession(runtime.GOOS, d.getenv) {
		_ = d.openBrowser(start.VerificationURL)
	}
	fmt.Fprint(out, "Waiting for approval…  ")

	tok, err := dl.Wait(ctx, start)
	if err != nil {
		fmt.Fprintln(out) // the line that waited ends before the message
		switch {
		case ctx.Err() != nil:
			return errLoginInterrupted
		case errors.Is(err, client.ErrLoginDenied):
			return &exitError{code: exitGeneral, msg: "The sign-in was denied in the dashboard."}
		case errors.Is(err, client.ErrLoginExpired):
			return &exitError{code: exitGeneral, msg: "The code expired. Run burrow login again."}
		}
		if problem, ok := certProblem(err); ok {
			return &exitError{code: exitUnreachable, msg: "Cannot trust " + p.relay + ": " + problem + ".\n" + fixCacert}
		}
		return &exitError{code: exitGeneral, msg: "The sign-in did not finish: " + waitReason(err) + ".\nRun burrow login again. " + tokenHint}
	}

	name := tok.TokenName
	if name == "" {
		name = p.name
	}
	// The config is built field by field and handed to the one function that
	// writes it; neither it nor the token is ever printed.
	if err := client.SaveUserConfig(p.path, client.UserConfig{Relay: p.relay, Control: p.control, Token: tok.Token, TokenName: name}); err != nil {
		fmt.Fprintln(out)
		// The relay gave the token once; it cannot be asked for again.
		return &exitError{code: exitGeneral, msg: "Could not store the sign-in: " + err.Error() + "\n" +
			"The token was created but could not be stored; revoke it in the dashboard (Clients, tab Tokens) and run burrow login again."}
	}
	if tok.Email != "" {
		fmt.Fprintf(out, "signed in as %s (token %q)\n", tok.Email, name)
	} else {
		fmt.Fprintf(out, "signed in (token %q)\n", name)
	}
	fmt.Fprintf(out, "Token %s for control endpoint %s, stored in %s\n", tokenLabel(name, tok.Token), p.control, p.path)
	return nil
}

// waitReason says why a sign-in request got no answer, without the Go error
// chain around a timeout.
func waitReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer from the relay in time"
	}
	return err.Error()
}

// newLoginCmd builds `burrow login <relay>`: through the browser, or with a
// token created in the dashboard.
func newLoginCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <relay>",
		Short: "Sign this machine in, once",
		Long: "Sign this machine in, once.\n\n" +
			"<relay> is the dashboard address: burrow.example.com or a full https:// URL.\n" +
			"burrow shows a page of that dashboard and a short code. Open the page, check\n" +
			"that it shows the same code and approve the sign-in; the token is then stored\n" +
			"on this machine. On a desktop the page opens by itself unless --no-browser is\n" +
			"given.\n\n" +
			"On a machine without a browser, or with a relay that has no browser sign-in,\n" +
			"a token created in the dashboard (Clients, tab Tokens) is stored with `--token -`:\n" +
			"burrow asks for it, or reads it from standard input, which keeps it out of the\n" +
			"shell history and the process list.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageWithLine(cmd, "%s needs the relay address, for example: burrow login burrow.example.com", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			g, err := readGlobals(cmd, d)
			if err != nil {
				return err
			}
			relay, host, err := normalizeRelay(args[0])
			if err != nil {
				return err
			}

			control := net.JoinHostPort(host, defaultControlPort)
			controlGiven := flags.Changed("control")
			if controlGiven {
				control, _ = flags.GetString("control")
				control = strings.TrimSpace(control)
				h, p, e := net.SplitHostPort(control)
				if n, pe := strconv.Atoi(p); e != nil || h == "" || pe != nil || n < 1 || n > 65535 {
					return usageErrorf("--control must be host:port, for example %s", net.JoinHostPort(host, defaultControlPort))
				}
			}

			name, _ := flags.GetString("name")
			name = strings.TrimSpace(name)
			if flags.Changed("name") && name == "" {
				return usageErrorf("--name must not be empty")
			}
			if name == "" {
				if h, err := d.hostname(); err == nil {
					name = strings.TrimSpace(h)
				}
				if name == "" {
					name = "burrow"
				}
			}

			relayArg := strings.TrimPrefix(relay, "https://")
			tokenGiven := flags.Changed("token")
			token, _ := flags.GetString("token")
			fromStdin := strings.TrimSpace(token) == "-"

			path, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			// The stored sign-in comes first: a machine that is signed in hears
			// that, whatever the relay would have answered.
			terminal := d.isTerminal()
			force, _ := flags.GetBool("force")
			// A stored file that cannot be read or holds no token is not a
			// sign-in: it is replaced without a question.
			if cur, lerr := client.LoadUserConfig(path); lerr == nil && strings.TrimSpace(cur.Token) != "" && !force {
				where := strings.TrimSpace(cur.Relay)
				if where == "" {
					where = strings.TrimSpace(cur.Control)
				}
				needForce := usageErrorf("This machine is already signed in to %s. Pass --force to replace the stored sign-in.", where)
				if !terminal {
					return needForce
				}
				// Asked on stderr, like the token: it shows when stdout is piped.
				fmt.Fprintf(errOut, "This machine is already signed in to %s. Replace the stored sign-in? [y/N] ", where)
				answer, end := readAnswer(d.stdin)
				if end {
					// Nobody is there to answer, as with Ctrl-D or input from /dev/null.
					fmt.Fprintln(errOut)
					return needForce
				}
				if a := strings.ToLower(answer); a != "y" && a != "yes" {
					fmt.Fprintln(out, "Kept the stored sign-in.")
					return nil
				}
			}

			// From here Ctrl-C ends the command with a line of its own: while
			// the relay is asked, and while the approval is awaited. The
			// questions before this and the token prompt after it read the
			// terminal, where Ctrl-C ends the process as it always did.
			ctx, stop := interruptible(cmd.Context())
			defer stop()

			// The relay says where its control endpoint is, before anything is
			// stored. What it says is shown before the token is read.
			control, older, err := discoverControl(ctx, d, g, errOut, relay, control, controlGiven)
			if err != nil {
				return err
			}
			if !tokenGiven {
				if older {
					// A relay from before discovery has no browser sign-in either.
					return errNoBrowserLogin(relayArg)
				}
				hostname := ""
				if hn, herr := d.hostname(); herr == nil {
					hostname = strings.TrimSpace(hn)
				}
				noBrowser, _ := flags.GetBool("no-browser")
				return browserLogin(ctx, d, g, out, errOut, browserSignIn{
					relay: relay, relayArg: relayArg, control: control, path: path,
					hostname: hostname, name: name, nameGiven: flags.Changed("name"), noBrowser: noBrowser,
				})
			}
			if older && !controlGiven {
				fmt.Fprintf(errOut, "This relay is older and does not say where its control endpoint is; using %s. Pass --control if it is elsewhere.\n", control)
			}
			interrupted := ctx.Err() != nil
			stop()
			if interrupted {
				return errLoginInterrupted
			}

			switch {
			case fromStdin && terminal:
				// Asked on stderr and read without echo: the token is on no screen.
				fmt.Fprint(errOut, "Token (input is hidden): ")
				token, err = d.readSecret()
				fmt.Fprintln(errOut)
				if errors.Is(err, errControlKeys) {
					return usageErrorf(msgControlKeys)
				}
				if err != nil && !errors.Is(err, io.EOF) {
					return errors.New("could not read the token from the terminal")
				}
				// Ctrl-D gives an empty token, which is reported below.
			case fromStdin:
				var ok bool
				if token, ok = readPipedToken(d.stdin); !ok {
					return usageErrorf(msgTokenOneLine)
				}
			}
			if token, err = cleanToken(token, relayArg); err != nil {
				return err
			}

			// The config is built field by field and handed to the one function
			// that writes it; it is never printed.
			if err := client.SaveUserConfig(path, client.UserConfig{Relay: relay, Control: control, Token: token, TokenName: name}); err != nil {
				return err
			}
			fmt.Fprintf(out, "Signed in to %s (control endpoint %s, token %q).\nStored in %s\n", relay, control, name, path)
			return nil
		},
	}
	f := cmd.Flags()
	f.String("token", "", "store a token created in the dashboard; give - and paste the token when asked, so that it stays out of the shell history")
	f.String("name", "", "name for the token (default: this machine's hostname)")
	f.Bool("no-browser", false, "do not open the browser; show the address and the code only")
	f.String("control", "", "control endpoint host:port (default: the one the relay names, else <relay host>:7000)")
	f.Bool("force", false, "replace a stored sign-in without asking")
	return cmd
}

// newLogoutCmd builds `burrow logout`.
func newLogoutCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Forget the stored sign-in",
		Long: "Forget the stored sign-in.\n\n" +
			"The token itself stays valid until it is revoked in the dashboard (Clients, tab Tokens).",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, _, err := logFormatFlag(cmd); err != nil {
				return err
			}
			path, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !isFile(path) {
				fmt.Fprintln(out, "Not signed in.")
				return nil
			}
			// The relay is read before the file goes; a file that cannot be
			// read is removed all the same.
			cur, _ := client.LoadUserConfig(path)
			if err := client.RemoveUserConfig(path); err != nil {
				return err
			}
			if relay := strings.TrimSuffix(strings.TrimSpace(cur.Relay), "/"); relay != "" {
				fmt.Fprintf(out, "Signed out. Revoke the token in the dashboard: %s/clients?tab=tokens\n", relay)
			} else {
				fmt.Fprintln(out, "Signed out. Revoke the token in the dashboard under Clients, tab Tokens.")
			}
			return nil
		},
	}
}
