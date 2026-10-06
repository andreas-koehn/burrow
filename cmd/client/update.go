package main

import (
	"context"
	"crypto/x509"
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

// releaseRootCAs is what the hosts behind the relay's download redirects are
// verified against. It is nil, the system's roots, outside tests: --cacert,
// --server-name and --insecure are settings for the relay and reach no other
// host.
var releaseRootCAs *x509.CertPool

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
// at exe cannot be written to. relay is the checked relay address; flags are
// the flags of this run to repeat, each value its own element.
func notWritableMessage(goos, exe, relay string, flags []string) string {
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
	line := "  sudo " + shellWord(exe) + " update " + shellWord(relay)
	for _, f := range flags {
		line += " " + shellWord(f)
	}
	msg := dir + " is not writable by this user. burrow was not updated. Run:\n" + line + "\n"
	if dir == "/usr/local/bin" {
		// Where the installer puts it with --system.
		msg += "or install it again:\n" +
			"  curl -fsSL " + shellWord(relay+"/install.sh") + " | sudo sh -s -- --system\n"
	}
	return strings.TrimRight(msg, "\n")
}

// repeatedFlags are the flags of this run that the same command needs when it
// is run again by another user. `update` takes no token, so none can be here.
func repeatedFlags(g globalFlags, force bool) []string {
	var out []string
	if g.cacert != "" {
		ca := g.cacert
		if abs, err := filepath.Abs(ca); err == nil {
			ca = abs
		}
		out = append(out, "--cacert", ca)
	}
	if g.serverName != "" {
		out = append(out, "--server-name", g.serverName)
	}
	if g.insecure {
		out = append(out, "--insecure")
	}
	if force {
		out = append(out, "--force")
	}
	return out
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
// command's error. exe is the binary that stays as it was; flags are the
// flags to repeat in a command to run instead.
func notUpdated(ctx context.Context, err error, exe, relay string, flags []string) error {
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return &exitError{code: exitGeneral, msg: "Interrupted. burrow was not updated."}
	case errors.Is(err, client.ErrStillRunning):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"Stop every running burrow (a service too), then run: burrow update"}
	case errors.Is(err, client.ErrRunCheck):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"The download was started once with `version` before taking the old binary's place. The old one is still installed."}
	case errors.Is(err, fs.ErrPermission):
		return &exitError{code: exitGeneral, msg: notWritableMessage(runtime.GOOS, exe, relay, flags)}
	case errors.Is(err, client.ErrNoBuild):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"The builds a relay hands out are listed at " + relay + "/download/"}
	case errors.Is(err, client.ErrChecksum):
		return &exitError{code: exitGeneral, msg: "burrow was not updated: " + err.Error() + ".\n" +
			"Nothing was installed. Run it again; when it fails the same way, the release behind this relay is damaged or incomplete."}
	}
	if problem, ok := certProblem(err); ok {
		// Discovery reached the relay just before, so this is most likely the
		// host its download redirect points at.
		return &exitError{code: exitUnreachable, msg: "burrow was not updated: " + problem + ".\n" +
			"That certificate came from the relay or from the host its download redirects to. " +
			"The relay's TLS flags do not apply to that other host: it must have a certificate this system trusts."}
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
		fmt.Fprintln(errOut, "Warning: --insecure is set: the relay's certificate is not checked, so its answers and redirects can be replaced on the way.")
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
	// The client follows the relay, also back to an older version; that is
	// said, not just done.
	downgrade := false
	if tagged {
		if c, ok := compareVersions(cur, target); ok && c > 0 {
			downgrade = true
		}
	}
	if check {
		if downgrade {
			fmt.Fprintf(out, "burrow %s → %s is available. It is a downgrade: the relay runs the older version. Run: burrow update\n", cur, target)
			return nil
		}
		fmt.Fprintf(out, "burrow %s → %s is available. Run: burrow update\n", cur, target)
		return nil
	}
	flags := repeatedFlags(g, force)

	// Where to, before anything is downloaded.
	exe, err := executablePath()
	if err != nil {
		return &exitError{code: exitGeneral, msg: "burrow was not updated: cannot find the running binary: " + err.Error() + "."}
	}
	if err := client.CheckReplaceable(exe); err != nil {
		return notUpdated(ctx, err, exe, relay, flags)
	}

	hc, err := relayHTTPClient(g)
	if err != nil {
		return err
	}
	src := client.UpdateSource{
		HTTP: hc, ReleaseRootCAs: releaseRootCAs,
		Relay: relay, Version: disc.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
	}
	if err := src.Validate(); err != nil {
		return notUpdated(ctx, err, exe, relay, flags)
	}
	fmt.Fprintf(out, "Downloading burrow %s for %s/%s from %s\n", target, runtime.GOOS, runtime.GOARCH, relay)
	bin, cleanup, err := src.Fetch(ctx)
	if err != nil {
		return notUpdated(ctx, err, exe, relay, flags)
	}
	defer cleanup()
	if ctx.Err() != nil {
		return notUpdated(ctx, ctx.Err(), exe, relay, flags)
	}
	// The new binary is started once with `version` before it takes over.
	staged := client.StagedCheck{Version: disc.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
	if err := client.ReplaceExecutable(exe, bin, staged); err != nil {
		return notUpdated(ctx, err, exe, relay, flags)
	}
	if downgrade {
		fmt.Fprintf(out, "Downgraded burrow %s → %s, the relay's version\n", cur, target)
		return nil
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
			"its SHA-256 is compared with the release's checksums, it is started once\n" +
			"with `version`, and only then does it take the place of the running binary.\n" +
			"A relay that runs an older version than this client is followed too; the\n" +
			"output calls that a downgrade. The checksum catches a damaged or\n" +
			"incomplete download; it comes from the same place as the client itself.\n" +
			"The token is not sent.\n\n" +
			"When the binary's directory is not writable, the command stops and prints\n" +
			"what to run instead. Nothing updates by itself: only this command does.\n\n" +
			"--cacert, --server-name and --insecure apply to the relay only. A host the\n" +
			"relay's download address redirects to needs a certificate this system trusts.\n\n" +
			"Exit code 0 when updated or already up to date, 1 when it was not updated\n" +
			"(the old binary stays), 2 when used wrongly, 3 when no relay is known,\n" +
			"5 when the relay or the download host cannot be reached or trusted.",
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
