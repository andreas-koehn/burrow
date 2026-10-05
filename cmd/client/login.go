package main

import (
	"bufio"
	"fmt"
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

// readLine reads one line and reports whether more input follows it.
func readLine(r *bufio.Reader) (line string, more bool) {
	line, _ = r.ReadString('\n')
	_, err := r.Peek(1)
	return strings.TrimSpace(line), err == nil
}

// cleanToken checks a token as typed or read. The token is never repeated.
func cleanToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", usageErrorf("The token is empty. Create one in the dashboard (Clients, tab Tokens) and pass it with --token.")
	}
	if strings.IndexFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", usageErrorf("The token must be a single word on one line.")
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
			"--token stores a token created in the dashboard (Clients, tab Tokens);\n" +
			"`--token -` reads it from standard input, which keeps it out of the shell history.",
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

			if !flags.Changed("token") {
				return usageErrorf("Browser sign-in arrives with a newer version of burrow.\n"+
					"Create a token in the dashboard (Clients, tab Tokens) and run: burrow login %s --token <token>",
					strings.TrimPrefix(relay, "https://"))
			}
			in := bufio.NewReader(d.stdin)
			token, _ := flags.GetString("token")
			fromStdin := strings.TrimSpace(token) == "-"
			if fromStdin {
				line, more := readLine(in)
				if more {
					return usageErrorf("The token must be a single word on one line.")
				}
				token = line
			}
			if token, err = cleanToken(token); err != nil {
				return err
			}

			path, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			force, _ := flags.GetBool("force")
			// A stored file that cannot be read or holds no token is not a
			// sign-in: it is replaced without a question.
			if cur, lerr := client.LoadUserConfig(path); lerr == nil && strings.TrimSpace(cur.Token) != "" && !force {
				where := strings.TrimSpace(cur.Relay)
				if where == "" {
					where = strings.TrimSpace(cur.Control)
				}
				if fromStdin || !d.isTerminal() {
					return usageErrorf("This machine is already signed in to %s. Pass --force to replace the stored sign-in.", where)
				}
				fmt.Fprintf(out, "This machine is already signed in to %s. Replace the stored sign-in? [y/N] ", where)
				answer, _ := readLine(in)
				fmt.Fprintln(out)
				if a := strings.ToLower(answer); a != "y" && a != "yes" {
					fmt.Fprintln(out, "Kept the stored sign-in.")
					return nil
				}
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
	f.String("token", "", "store this token instead of signing in through the browser; - reads it from standard input")
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
