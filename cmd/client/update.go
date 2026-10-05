package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/version"
)

// executablePath is the file `burrow update` replaces: the running binary,
// with symbolic links resolved. Tests point it at a file of their own.
var executablePath = func() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

// removeReplacedAtStart removes the previous binary that an update on Windows
// had to leave next to the new one, because it was still running.
func removeReplacedAtStart(goos string) {
	if goos != "windows" {
		return
	}
	if p, err := executablePath(); err == nil {
		client.RemoveReplacedExecutable(p)
	}
}

// shellWord writes s so that a POSIX shell reads it as one word.
func shellWord(s string) string {
	plain := s != ""
	for i := 0; i < len(s) && plain; i++ {
		c := s[i]
		plain = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/._-+:", c) >= 0
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// notWritableMessage says what to run instead when the directory of the binary
// at exe cannot be written to. relay is the checked relay address.
func notWritableMessage(goos, exe, relay string) string {
	if goos == "windows" {
		dir := exe
		if i := strings.LastIndexAny(exe, `\/`); i >= 0 {
			dir = exe[:i]
		}
		return dir + " is not writable by this user. burrow was not updated.\n" +
			"Run this command again from a terminal opened as administrator, or install burrow for your user:\n" +
			"  irm " + relay + "/install.ps1 | iex"
	}
	dir := filepath.Dir(exe)
	msg := dir + " is not writable by this user. burrow was not updated. Run:\n" +
		"  sudo " + shellWord(exe) + " update " + relay + "\n"
	if dir == "/usr/local/bin" {
		// Where the installer puts it with --system.
		msg += "or install it again:\n" +
			"  curl -fsSL " + relay + "/install.sh | sudo sh -s -- --system\n"
	}
	return strings.TrimRight(msg, "\n")
}

// updateRelay returns the relay to update from: the argument, or else the
// relay of the stored sign-in. Of the sign-in only the relay address is used.
func updateRelay(cmd *cobra.Command, d deps, args []string) (string, error) {
	if len(args) == 1 {
		relay, _, err := normalizeRelay(args[0])
		return relay, err
	}
	path, err := userConfigFile(cmd, d)
	if err != nil {
		return "", err
	}
	uc, err := client.LoadUserConfig(path)
	stored := uc.Relay // the one value of the sign-in this command reads
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", &exitError{code: exitNotSignedIn, msg: msgNotSignedIn + "\nOr name the relay to update from: burrow update <relay>"}
	case err != nil:
		return "", err
	case strings.TrimSpace(stored) == "":
		return "", usageErrorf("The stored sign-in does not name the relay's web address.\nName the relay to update from: burrow update <relay>")
	}
	relay, _, err := normalizeRelay(stored)
	if err != nil {
		return "", usageErrorf("The relay address of the stored sign-in cannot be used.\nName the relay to update from: burrow update <relay>")
	}
	return relay, nil
}

// updateDiscovery asks the relay which version it runs. Every failure is an
// error: without the relay's version there is nothing to update to.
func updateDiscovery(ctx context.Context, d deps, g globalFlags, relay string) (client.Discovery, error) {
	disc, err := d.discover(ctx, relay, g)
	var se *client.DiscoveryStatusError
	switch {
	case err == nil:
		return disc, nil
	case errors.Is(ctx.Err(), context.Canceled):
		return disc, &exitError{code: exitGeneral, msg: "Interrupted. burrow was not updated."}
	case errors.Is(err, client.ErrNoDiscovery):
		return disc, &exitError{code: exitGeneral, msg: relay + " runs an older Burrow that does not hand out the client.\n" +
			"Update the relay first, or check the address."}
	case errors.Is(err, client.ErrNotARelay):
		return disc, &exitError{code: exitUnreachable, msg: relay + " does not look like a Burrow relay.\nCheck the address."}
	case errors.As(err, &se):
		return disc, &exitError{code: exitUnreachable, msg: relay + " cannot be asked for its version: " + se.Error() + ".\nCheck the relay address."}
	}
	if problem, ok := certProblem(err); ok {
		return disc, &exitError{code: exitUnreachable, msg: "Cannot trust " + relay + ": " + problem + ".\n" + fixCacert}
	}
	reason := err.Error()
	if errors.Is(err, context.DeadlineExceeded) {
		reason = "no answer within " + client.DiscoveryTimeout.String()
	}
	return disc, &exitError{code: exitUnreachable, msg: "Cannot reach " + relay + ": " + reason + ".\nCheck the address and your network connection."}
}

// notUpdated turns a failed download or a failed replacement into the
// command's error. exe is the binary that stays as it was.
func notUpdated(ctx context.Context, err error, exe, relay string) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return &exitError{code: exitGeneral, msg: "Interrupted. burrow was not updated."}
	case errors.Is(err, fs.ErrPermission):
		return &exitError{code: exitGeneral, msg: notWritableMessage(runtime.GOOS, exe, relay)}
	case errors.Is(err, client.ErrNoBuild):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"The builds a relay hands out are listed at " + relay + "/download/"}
	case errors.Is(err, client.ErrChecksum):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"Nothing was installed. Run it again; when it fails the same way, the release behind this relay is damaged or incomplete."}
	}
	if problem, ok := certProblem(err); ok {
		return &exitError{code: exitUnreachable, msg: "burrow was not updated: " + problem + ".\n" + fixCacert}
	}
	return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + "."}
}

func runUpdate(cmd *cobra.Command, d deps, args []string) error {
	check, _ := cmd.Flags().GetBool("check")
	force, _ := cmd.Flags().GetBool("force")
	g, err := readGlobals(cmd, d)
	if err != nil {
		return err
	}
	relay, err := updateRelay(cmd, d, args)
	if err != nil {
		return err
	}
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	if g.insecure {
		fmt.Fprintln(errOut, "Warning: --insecure is set: the relay's certificate is not checked. The download is only as good as the network it crosses.")
	}
	disc, err := updateDiscovery(ctx, d, g, relay)
	if err != nil {
		return err
	}

	// Which version, and is there anything to do.
	cur := version.Version
	target, tagged := client.ReleaseVersion(disc.Version)
	if !tagged {
		target = disc.Version
		if target == "" {
			target = "unknown"
		}
		untagged := "The relay runs an untagged build (" + target + "), so versions cannot be compared.\n" +
			"To install the relay's current build anyway, run: burrow update --force"
		if check {
			fmt.Fprintln(out, untagged)
			return nil
		}
		if !force {
			return &exitError{code: exitGeneral, msg: untagged}
		}
	} else if mine, ok := client.ReleaseVersion(cur); ok && mine == target {
		fmt.Fprintf(out, "burrow %s is up to date\n", target)
		return nil
	}
	if check {
		fmt.Fprintf(out, "burrow %s → %s is available. Run: burrow update\n", cur, target)
		return nil
	}

	// Where to, before anything is downloaded.
	exe, err := executablePath()
	if err != nil {
		return &exitError{code: exitGeneral, msg: "burrow was not updated: cannot find the running binary: " + err.Error() + "."}
	}
	if err := client.CheckReplaceable(exe); err != nil {
		return notUpdated(ctx, err, exe, relay)
	}

	hc, err := relayHTTPClient(g)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Downloading burrow %s for %s/%s from %s\n", target, runtime.GOOS, runtime.GOARCH, relay)
	src := client.UpdateSource{HTTP: hc, Relay: relay, Version: disc.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	bin, cleanup, err := src.Fetch(ctx)
	if err != nil {
		return notUpdated(ctx, err, exe, relay)
	}
	defer cleanup()
	if ctx.Err() != nil {
		return notUpdated(ctx, ctx.Err(), exe, relay)
	}
	if err := client.ReplaceExecutable(exe, bin); err != nil {
		return notUpdated(ctx, err, exe, relay)
	}
	fmt.Fprintf(out, "Updated burrow %s → %s\n", cur, target)
	return nil
}

// newUpdateCmd builds `burrow update`.
func newUpdateCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update [relay]",
		Short: "Replace this binary with the relay's version",
		Long: "Replace this binary with the client that matches the relay's version.\n\n" +
			"The relay is the one this machine is signed in to, or the address given.\n" +
			"The client is downloaded through the relay's download address over HTTPS,\n" +
			"its SHA-256 is compared with the release's checksums, and only then does it\n" +
			"take the place of the running binary. The checksum catches a damaged or\n" +
			"incomplete download; it comes from the same place as the client itself.\n" +
			"The token is not sent.\n\n" +
			"When the binary's directory is not writable, the command stops and prints\n" +
			"what to run instead. Nothing updates by itself: only this command does.\n\n" +
			"Exit code 0 when updated or already up to date, 3 when no relay is known,\n" +
			"5 when the relay cannot be reached or its certificate is not trusted.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageWithLine(cmd, "%s takes at most one relay address", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return runUpdate(cmd, d, args) },
	}
	cmd.Flags().Bool("check", false, "only report whether an update exists")
	cmd.Flags().Bool("force", false, "install the relay's build when it is untagged and versions cannot be compared")
	return cmd
}
