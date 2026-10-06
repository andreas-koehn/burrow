package main

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/proto"
)

// Exit codes, as the spec's table "Messages and exit codes" assigns them.
const (
	exitGeneral       = 1 // anything not listed below
	exitUsage         = 2 // wrong usage or a target that is not accepted
	exitNotSignedIn   = 3
	exitTokenRejected = 4
	exitUnreachable   = 5 // relay unreachable or its certificate not trusted
	exitClientTooOld  = 6
)

// The spec's messages, verbatim. The ones with a verb take the control
// endpoint and the minimum version.
const (
	msgNotSignedIn      = "Not signed in. Run: burrow login <your relay address>"
	msgTokenRejected    = "The relay rejected this machine's token. It may have been revoked. Run: burrow login <relay>"
	msgRelayUnreachable = "Cannot reach %s. Check the address and that port 7000 is open. Details: burrow doctor"
	msgClientTooOld     = "This relay needs burrow %s or newer. Run: burrow update"
	// When the relay's text names no minimum.
	msgClientTooOldNoMin = "This relay needs a newer burrow. Run: burrow update"
	msgHTTPNotEnabled    = "HTTP services are not enabled on this relay. Its administrator enables them with the relay setting auth_domain."
)

// maxRelayText is how much of a text from the relay is printed.
const maxRelayText = 300

// plainText makes a text that came from the relay fit for a terminal: no
// control or formatting characters, one line, at most maxRelayText characters.
func plainText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == maxRelayText {
			b.WriteString("…")
			break
		}
		switch {
		case r == utf8.RuneError || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp):
			continue
		case unicode.IsControl(r):
			r = ' '
		}
		b.WriteRune(r)
		n++
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

var versionRe = regexp.MustCompile(`\d{1,9}\.\d{1,9}\.\d{1,9}`)

// refusal returns the message and the exit code for a refusal of the relay
// the spec has a line for. ok is false for any other refusal: it is printed
// the way every other error is.
func refusal(re *client.RefusedError) (msg string, code int, ok bool) {
	switch re.Code {
	case proto.CodeInvalidToken:
		return msgTokenRejected, exitTokenRejected, true
	case proto.CodeClientTooOld:
		if min := versionRe.FindString(re.Message); min != "" {
			return fmt.Sprintf(msgClientTooOld, min), exitClientTooOld, true
		}
		return msgClientTooOldNoMin, exitClientTooOld, true
	case proto.CodeSlugTaken, proto.CodeSlugInvalid:
		return "The relay refused the slug: " + plainText(re.Message), exitGeneral, true
	case proto.CodeAccessInvalid:
		return "The relay refused the access mode: " + plainText(re.Message), exitGeneral, true
	case proto.CodeForbidden:
		return "The relay refused: " + plainText(re.Message), exitGeneral, true
	case proto.CodeHTTPNotEnabled:
		return msgHTTPNotEnabled, exitGeneral, true
	}
	return "", 0, false
}

// exitError is an error with its own exit code. Its message is complete: main
// prints it as it is, without the "error:" prefix.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// usageErrorf is an error for wrong usage: exit code 2.
func usageErrorf(format string, args ...any) error {
	return &exitError{code: exitUsage, msg: fmt.Sprintf(format, args...)}
}

// usageWithLine adds the command's usage line to a usage error.
func usageWithLine(cmd *cobra.Command, format string, args ...any) error {
	return usageErrorf("%s\nUsage: %s\nRun '%s --help' for the flags.",
		fmt.Sprintf(format, args...), cmd.UseLine(), cmd.CommandPath())
}

// flagUsageError is cobra's flag error function for every command but
// `connect`: an unknown or malformed flag is wrong usage.
func flagUsageError(cmd *cobra.Command, err error) error {
	return usageWithLine(cmd, "%s", err.Error())
}

// noArgs rejects positional arguments as wrong usage.
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageWithLine(cmd, "%s takes no arguments", cmd.CommandPath())
	}
	return nil
}

// classified reports the exit code of an error the client has a message for.
func classified(err error) (int, bool) {
	var ee *exitError
	var te *client.TargetError
	var re *client.RefusedError
	switch {
	case errors.As(err, &ee):
		return ee.code, true
	case errors.Is(err, client.ErrNotSignedIn):
		return exitNotSignedIn, true
	case errors.As(err, &te):
		return exitUsage, true
	case errors.As(err, &re):
		if _, code, ok := refusal(re); ok {
			return code, true
		}
	}
	return 0, false
}

// exitCode maps an error to the process exit code.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if code, ok := classified(err); ok {
		return code
	}
	return exitGeneral
}

// report prints err the way main does and returns the exit code. The client's
// own messages are printed as they are; anything else keeps the "error:" prefix
// it has always had.
func report(w io.Writer, err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	var te *client.TargetError
	var me *client.RelayMismatchError
	var re *client.RefusedError
	if errors.As(err, &re) && !errors.As(err, &ee) {
		if msg, _, ok := refusal(re); ok {
			fmt.Fprintln(w, msg)
			return exitCode(err)
		}
	}
	switch {
	case errors.As(err, &ee):
		fmt.Fprintln(w, ee.msg)
	case errors.As(err, &me): // not signed in to this relay: it has its own two lines
		fmt.Fprintln(w, me.Error())
	case errors.Is(err, client.ErrNotSignedIn):
		fmt.Fprintln(w, msgNotSignedIn)
	case errors.As(err, &te):
		fmt.Fprintln(w, te.Error())
	default:
		fmt.Fprintln(w, "error:", err)
	}
	return exitCode(err)
}
