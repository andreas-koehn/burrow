package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
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
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= ' ' || c > '~' {
			return "", usageErrorf(msgNotAToken)
		}
	}
	return token, nil
}

// newLoginCmd builds `burrow login <relay>`. In this version it stores a token
// created in the dashboard; signing in through the browser follows.
func newLoginCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login <relay>",
		Short: "Sign this machine in, once",
		Long: "Sign this machine in, once.\n\n" +
			"<relay> is the dashboard address: burrow.example.com or a full https:// URL.\n" +
			"A token created in the dashboard (Clients, tab Tokens) is stored with `--token -`:\n" +
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
			if _, _, err := logFormatFlag(cmd); err != nil {
				return err
			}
			relay, host, err := normalizeRelay(args[0])
			if err != nil {
				return err
			}

			control := net.JoinHostPort(host, defaultControlPort)
			if flags.Changed("control") {
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
			if !flags.Changed("token") {
				return usageErrorf("Browser sign-in arrives with a newer version of burrow.\n"+
					"Create a token in the dashboard (Clients, tab Tokens), "+msgPasteToken, relayArg)
			}
			token, _ := flags.GetString("token")
			fromStdin := strings.TrimSpace(token) == "-"

			path, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
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
	f.String("name", "", "name to remember the token by (default: this machine's hostname)")
	f.String("control", "", "control endpoint host:port (default: <relay host>:7000)")
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
