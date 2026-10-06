package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/cmd/client/svc"
	"github.com/ankoehn/burrow/internal/client"
)

// serviceManager is what the commands need of the system's service manager.
// A service.Service is one.
type serviceManager interface {
	Install() error
	Uninstall() error
	Start() error
	Stop() error
	Status() (service.Status, error)
}

// serviceEnv is what `burrow service` takes from the machine. Tests replace
// all of it: no test installs, starts or asks about a real service.
type serviceEnv struct {
	goos string
	// elevated reports whether this process is root or an administrator.
	elevated func() bool
	// executable is the running binary, symbolic links resolved.
	executable func() (string, error)
	// trusted reports whether only root or an administrator can change the
	// binary at exe or a directory above it.
	trusted func(exe string) bool
	// programData is the machine's ProgramData directory (Windows).
	programData func() string
	// lookupHome is the home directory of a user by name.
	lookupHome func(user string) (string, error)
	// homeDir is the home directory of the current user.
	homeDir func() (string, error)
	// system names the service manager that was found: "linux-systemd",
	// "darwin-launchd", "windows-service", or something else.
	system func() string
	// manager is the service manager's view of a service definition.
	manager func(cfg *service.Config) (serviceManager, error)
	// unitFile is the file the service manager keeps the definition in, ""
	// where there is none (Windows).
	unitFile func() string
	// makePrivateDir creates a directory that only the system and its
	// administrators can read. It fails when the directory exists.
	makePrivateDir func(path string) error
	// run runs a command of the system with fixed arguments and shows its
	// output.
	run func(out io.Writer, name string, args ...string) error
}

// noProgram is the program of a service definition that is only installed,
// started or asked about from here; `burrow up` is what runs.
type noProgram struct{}

func (noProgram) Start(service.Service) error { return nil }
func (noProgram) Stop(service.Service) error  { return nil }

func realServiceEnv() *serviceEnv {
	return &serviceEnv{
		goos:        runtime.GOOS,
		elevated:    isElevated,
		executable:  func() (string, error) { return executablePath() },
		trusted:     executableTrusted,
		programData: programDataDir,
		lookupHome: func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			if u.HomeDir == "" {
				return "", errors.New("no home directory")
			}
			return u.HomeDir, nil
		},
		homeDir: os.UserHomeDir,
		system:  service.Platform,
		manager: func(cfg *service.Config) (serviceManager, error) { return service.New(noProgram{}, cfg) },
		unitFile: func() string {
			switch runtime.GOOS {
			case "linux":
				return "/etc/systemd/system/" + svc.Name + ".service"
			case "darwin":
				// Where the library puts the agent of the current user.
				if u, err := user.Current(); err == nil && u.HomeDir != "" {
					return u.HomeDir + "/Library/LaunchAgents/" + svc.Name + ".plist"
				}
			}
			return ""
		},
		makePrivateDir: makePrivateDir,
		run: func(out io.Writer, name string, args ...string) error {
			c := exec.Command(name, args...)
			c.Stdout, c.Stderr = out, out
			return c.Run()
		},
	}
}

// serviceConfig turns a plan into the library's service definition. Nothing
// else goes in: no environment variable, and so no token.
func serviceConfig(p svc.Plan, goos string) *service.Config {
	cfg := &service.Config{
		Name:             p.Name,
		DisplayName:      p.DisplayName,
		Description:      p.Description,
		Executable:       p.Executable,
		Arguments:        p.Arguments,
		WorkingDirectory: p.WorkingDir,
		UserName:         p.UserName,
		Option:           service.KeyValue{},
	}
	switch goos {
	case "linux":
		cfg.Option["Restart"] = "always"
		// Started at boot once the network is there; the client keeps trying
		// by itself when it is not.
		cfg.Dependencies = []string{"Wants=network-online.target", "After=network-online.target"}
	case "darwin":
		cfg.Option["UserService"] = true
		cfg.Option["RunAtLoad"] = true
		cfg.Option["KeepAlive"] = true
		if p.LogDir != "" {
			cfg.Option["LogDirectory"] = p.LogDir
		}
	case "windows":
		// The counterpart of Restart=always.
		cfg.Option["OnFailure"] = "restart"
		cfg.Option["OnFailureDelayDuration"] = "30s"
	}
	return cfg
}

// supported reports whether this client installs into the machine's service
// manager: systemd on Linux, and the one of macOS and of Windows.
func (e *serviceEnv) supported() bool {
	return e.goos != "linux" || e.system() == "linux-systemd"
}

func (e *serviceEnv) unsupported() error {
	if e.goos == "linux" {
		return &exitError{code: exitGeneral, msg: "burrow service needs systemd on Linux, and this system does not run it.\n" +
			"Start `burrow up` from this system's own service manager instead."}
	}
	return &exitError{code: exitGeneral, msg: "burrow service is not available on " + e.goos + "."}
}

// state asks the service manager about the installed service.
// service.ErrNotInstalled says that there is none.
func (e *serviceEnv) state() (serviceManager, service.Status, error) {
	if !e.supported() {
		return nil, service.StatusUnknown, service.ErrNotInstalled
	}
	m, err := e.manager(serviceConfig(svc.Plan{Name: svc.Name}, e.goos))
	if err != nil {
		return nil, service.StatusUnknown, err
	}
	st, err := m.Status()
	return m, st, err
}

// describeService is the line `burrow status` and `burrow service status`
// print about the service.
func describeService(st service.Status, err error) string {
	switch {
	case errors.Is(err, service.ErrNotInstalled):
		return "not installed"
	case err != nil:
		return "unknown (" + plainText(err.Error()) + ")"
	case st == service.StatusRunning:
		return "installed, running"
	case st == service.StatusStopped:
		return "installed, stopped"
	}
	return "installed, state unknown"
}

// winWord writes s so that cmd.exe and PowerShell read it as one word.
func winWord(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t&()^;,") {
		return s
	}
	return `"` + s + `"`
}

// again is the command line to run with more rights: the binary by its full
// path and the given words.
func (e *serviceEnv) again(words ...string) string {
	exe, err := e.executable()
	if err != nil {
		exe = "burrow"
	}
	quote, line := shellWord, "sudo "
	if e.goos == "windows" {
		quote, line = winWord, ""
	}
	line += quote(exe)
	for _, w := range words {
		line += " " + quote(w)
	}
	return line
}

// needRights refuses an action that changes the service when this process
// may not: on Linux and Windows that takes root or an administrator.
func (e *serviceEnv) needRights(action string, words ...string) error {
	if e.goos == "darwin" || e.elevated() {
		return nil
	}
	return &exitError{code: exitGeneral, msg: svc.ElevationError(e.goos, action, e.again(words...)).Error()}
}

// copyPrivate copies src to the new file dst, which only its owner can read
// from its first byte on. A file, a link or anything else that is already at
// dst is an error: nothing is followed and nothing is overwritten.
func copyPrivate(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %s", src, pathless(err))
	}
	// O_EXCL refuses a symbolic link at dst too, also one that points nowhere.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %s", dst, pathless(err))
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("write %s: %s", dst, pathless(err))
	}
	return nil
}

// installPaths finds the burrow.yaml and the user config of the install, and
// the user the service is to run as on Linux ("" is root).
func installPaths(cmd *cobra.Command, d deps, e *serviceEnv, args []string) (yaml, cfg, userName string, err error) {
	override, _ := cmd.Flags().GetString("config")
	// Through sudo the environment is root's, the files are not.
	home := ""
	if e.goos == "linux" && e.elevated() {
		if u := strings.TrimSpace(d.getenv("SUDO_USER")); u != "" && u != "root" {
			userName = u
			if override == "" || len(args) == 0 {
				if home, err = e.lookupHome(u); err != nil {
					return "", "", "", &exitError{code: exitGeneral, msg: fmt.Sprintf(
						"The home directory of %q, who ran sudo, cannot be found. Nothing was installed.\n"+
							"Name the files: burrow service install <burrow.yaml> --config <config.yaml>", plainText(u))}
				}
			}
		}
	}
	defaultCfg := ""
	if home != "" {
		defaultCfg = filepath.Join(home, ".config", "burrow", "config.yaml")
	} else if defaultCfg, err = d.userConfigPath(""); err != nil {
		return "", "", "", err
	}
	cfg = defaultCfg
	if override != "" {
		cfg = override
	}
	// As for `burrow up`: the directory is the one the sign-in lives in by
	// default; --config names a file and does not move it.
	yaml = filepath.Join(filepath.Dir(defaultCfg), serviceFileName)
	if len(args) == 1 {
		yaml = args[0]
	}
	if yaml, err = filepath.Abs(yaml); err != nil {
		return "", "", "", err
	}
	if cfg, err = filepath.Abs(cfg); err != nil {
		return "", "", "", err
	}
	return yaml, cfg, userName, nil
}

// signedInFor reports whether the service would find a sign-in: the stored
// one, and a burrow.yaml that goes with it. The environment of the person who
// installs is not the service's and is not looked at.
func signedInFor(yaml, cfg string, yamlExists bool) (bool, error) {
	uc, err := client.LoadUserConfig(cfg)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	case strings.TrimSpace(uc.Token) == "" || strings.TrimSpace(uc.Control) == "":
		return false, nil
	}
	if !yamlExists {
		return true, nil
	}
	fc, err := client.LoadFileConfig(yaml)
	if err != nil {
		return false, err
	}
	// The stored token goes to the relay it was stored for and to no other.
	if _, err := client.Resolve(client.Sources{FileServer: fc.Server, FileToken: fc.Token, User: &uc}); err != nil {
		return false, err
	}
	return true, nil
}

// planError turns a refusal of svc.BuildPlan into the command's error.
func planError(err error) error {
	switch {
	case errors.Is(err, svc.ErrNotSignedIn):
		return client.ErrNotSignedIn
	case errors.Is(err, svc.ErrNoYAML):
		return usageErrorf("%s", err.Error())
	}
	return &exitError{code: exitGeneral, msg: err.Error()}
}

func runServiceInstall(cmd *cobra.Command, d deps, args []string) error {
	e := d.service
	if !e.supported() {
		return e.unsupported()
	}
	g, err := readGlobals(cmd, d)
	if err != nil {
		return err
	}
	yaml, cfg, userName, err := installPaths(cmd, d, e, args)
	if err != nil {
		return err
	}
	exe, err := e.executable()
	if err != nil {
		return err
	}
	in := svc.Inputs{
		GOOS: e.goos, Executable: exe, YAMLPath: yaml, UserConfigPath: cfg, UserName: userName,
		Elevated: e.elevated(), YAMLExists: isFile(yaml), ExecutableTrusted: e.trusted(exe),
		ExtraArguments: repeatedFlags(g, false),
	}
	in.Command = e.again(append([]string{"service", "install", yaml, "--config", cfg}, in.ExtraArguments...)...)
	if in.SignedIn, err = signedInFor(yaml, cfg, in.YAMLExists); err != nil {
		return err
	}
	switch e.goos {
	case "windows":
		in.ProgramData = e.programData()
	case "darwin":
		if in.HomeDir, err = e.homeDir(); err != nil {
			return err
		}
	}
	if e.goos == "darwin" || in.Elevated {
		_, _, stErr := e.state()
		in.AlreadyInstalled = !errors.Is(stErr, service.ErrNotInstalled)
	}
	plan, err := svc.BuildPlan(in)
	if err != nil {
		return planError(err)
	}

	out := cmd.OutOrStdout()
	// What this install creates, to take away again when it does not finish.
	var created []string
	undo := func() {
		for i := len(created) - 1; i >= 0; i-- {
			_ = os.Remove(created[i])
		}
	}
	if plan.ConfigDir != "" {
		// The directory is made new and closed to everybody but the system
		// and its administrators before the sign-in goes into it: one that
		// is there already may belong to anybody.
		if err := e.makePrivateDir(plan.ConfigDir); err != nil {
			return &exitError{code: exitGeneral, msg: fmt.Sprintf(
				"%s cannot be created: %s. Nothing was installed.\n"+
					"If it exists, it was not made by this install and may be readable by others: "+
					"remove it from an elevated terminal, then run the install again.", plan.ConfigDir, plainText(pathless(err)))}
		}
		created = append(created, plan.ConfigDir)
		for _, c := range plan.Copies {
			if err := copyPrivate(c.From, c.To); err != nil {
				undo()
				return &exitError{code: exitGeneral, msg: "The service's files could not be copied: " + plainText(err.Error()) + ". Nothing was installed."}
			}
			created = append(created, c.To)
			fmt.Fprintf(out, "Copied %s to %s\n", c.From, c.To)
		}
		fmt.Fprintf(out, "The service reads these copies; only SYSTEM and Administrators can read %s.\n"+
			"A later `burrow login` or a change to burrow.yaml reaches the service after: burrow service uninstall, burrow service install.\n", plan.ConfigDir)
	}
	if plan.LogDir != "" {
		// launchd writes the log file and does not make its directory.
		if err := os.MkdirAll(plan.LogDir, 0o700); err != nil {
			return err
		}
	}
	if e.goos != "windows" {
		if wide, err := client.FileModeTooWide(cfg); err == nil && wide {
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s holds this machine's token and can be read by other users. Run: chmod 600 %s\n", cfg, shellWord(cfg))
		}
	}

	m, err := e.manager(serviceConfig(plan, e.goos))
	if err != nil {
		undo()
		return err
	}
	unit := e.unitFile()
	unitWasThere := false
	if unit != "" {
		_, statErr := os.Lstat(unit)
		unitWasThere = statErr == nil
	}
	if err := m.Install(); err != nil {
		if !unitWasThere {
			// Whatever the failed install left: the registration, and the
			// definition file when removing the registration did not get
			// that far.
			_ = m.Uninstall()
			if unit != "" {
				_ = os.Remove(unit)
			}
		}
		undo()
		return &exitError{code: exitGeneral, msg: "The burrow service could not be installed: " + plainText(err.Error()) + ".\nNothing is installed."}
	}

	fmt.Fprintf(out, "Service:  %s (%s)\n", plan.Name, e.system())
	fmt.Fprintf(out, "Runs as:  %s\n", plan.RunsAs)
	fmt.Fprintf(out, "Runs:     %s %s\n", plan.Executable, strings.Join(plan.Arguments, " "))
	if err := m.Start(); err != nil {
		return &exitError{code: exitGeneral, msg: "The burrow service is installed, but it did not start: " + plainText(err.Error()) + ".\n" +
			"Look at: burrow service logs"}
	}
	fmt.Fprintln(out, "Installed and started. Logs: burrow service logs")
	return nil
}

// pathless is the reason of a file error without the path, which the message
// around it names already.
func pathless(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// windowsConfigDir is the directory the install on Windows copies into.
func (e *serviceEnv) windowsConfigDir() string {
	pd := strings.TrimRight(e.programData(), `\/`)
	if e.goos != "windows" || pd == "" {
		return ""
	}
	return pd + `\` + svc.Name
}

// removeCopies takes away what the install on Windows put under ProgramData:
// the two copies, the service's log, and the directory when nothing else is
// in it.
func (e *serviceEnv) removeCopies(out io.Writer) {
	dir := e.windowsConfigDir()
	if dir == "" {
		return
	}
	if _, err := os.Lstat(dir); err != nil {
		return
	}
	for _, name := range []string{"burrow.yaml", "config.yaml", "burrow.log"} {
		_ = os.Remove(dir + `\` + name)
	}
	if err := os.Remove(dir); err != nil {
		fmt.Fprintf(out, "%s was not removed: it holds files this client did not put there.\n", dir)
		return
	}
	fmt.Fprintf(out, "Removed %s and the copies in it.\n", dir)
}

func runServiceUninstall(cmd *cobra.Command, d deps) error {
	e := d.service
	if !e.supported() {
		return e.unsupported()
	}
	if err := e.needRights("Removing", "service", "uninstall"); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	m, _, err := e.state()
	if errors.Is(err, service.ErrNotInstalled) {
		// Copies an interrupted install or a removal by other means left.
		e.removeCopies(out)
		fmt.Fprintln(out, "The burrow service is not installed.")
		return nil
	}
	if m == nil {
		return err
	}
	_ = m.Stop() // one that is not running cannot be stopped; that is fine
	if err := m.Uninstall(); err != nil {
		return &exitError{code: exitGeneral, msg: "The burrow service could not be removed: " + plainText(err.Error()) + "."}
	}
	e.removeCopies(out)
	fmt.Fprintln(out, "Uninstalled the burrow service.")
	return nil
}

func runServiceStartStop(cmd *cobra.Command, d deps, action string) error {
	e := d.service
	if !e.supported() {
		return e.unsupported()
	}
	doing, done := "Starting", "Started"
	if action == "stop" {
		doing, done = "Stopping", "Stopped"
	}
	if err := e.needRights(doing, "service", action); err != nil {
		return err
	}
	m, _, err := e.state()
	if errors.Is(err, service.ErrNotInstalled) {
		return &exitError{code: exitGeneral, msg: "The burrow service is not installed. Run: burrow service install"}
	}
	if m == nil {
		return err
	}
	if action == "stop" {
		err = m.Stop()
	} else {
		err = m.Start()
	}
	if err != nil {
		return &exitError{code: exitGeneral, msg: doing + " the burrow service failed: " + plainText(err.Error()) + "."}
	}
	fmt.Fprintln(cmd.OutOrStdout(), done+" the burrow service.")
	return nil
}

func runServiceStatus(cmd *cobra.Command, d deps) error {
	_, st, err := d.service.state()
	if errors.Is(err, service.ErrNotInstalled) {
		return &exitError{code: exitGeneral, msg: "The burrow service is not installed."}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Service:  %s\n", describeService(st, err))
	return nil
}

func runServiceLogs(cmd *cobra.Command, d deps) error {
	e := d.service
	out := cmd.OutOrStdout()
	switch e.goos {
	case "linux":
		args := []string{"-u", svc.Name, "-n", "100", "--no-pager"}
		fmt.Fprintln(cmd.ErrOrStderr(), "Logs: journalctl "+strings.Join(args, " ")+"  (add -f to follow; sudo when nothing is shown)")
		if err := e.run(out, "journalctl", args...); err != nil {
			return &exitError{code: exitGeneral, msg: "journalctl did not run: " + plainText(err.Error()) + "."}
		}
	case "darwin":
		home, err := e.homeDir()
		if err != nil {
			return err
		}
		log := strings.TrimRight(home, "/") + "/Library/Logs/" + svc.Name + "/burrow.err.log"
		fmt.Fprintf(out, "Logs: %s\nShow the last lines: tail -n 100 %s\n", log, shellWord(log))
	case "windows":
		log := e.windowsConfigDir() + `\burrow.log`
		fmt.Fprintf(out, "Logs: %s\nShow the last lines from an elevated PowerShell: Get-Content -Tail 100 '%s'\n", log, log)
	default:
		return e.unsupported()
	}
	return nil
}

// newServiceCmd builds `burrow service` and its subcommands.
func newServiceCmd(d deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "service",
		Short: "Run `burrow up` as a system service",
		Long: "Run `burrow up` as a system service, for a machine that should stay connected.\n\n" +
			"  Linux    systemd unit, restarted always, runs as the user who installed it (through sudo)\n" +
			"  macOS    launchd agent of the current user, runs while that user is logged in\n" +
			"  Windows  Windows service, runs as LocalSystem; burrow.yaml and the sign-in are copied to\n" +
			"           a directory under ProgramData that only SYSTEM and Administrators can read\n\n" +
			"The service definition holds paths only. The token stays in the sign-in's file.",
		SuggestionsMinimumDistance: 2,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return usageErrorf("unknown command %q for %q\nRun '%s --help' for usage.", args[0], cmd.CommandPath(), cmd.CommandPath())
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	install := &cobra.Command{
		Use:   "install [burrow.yaml]",
		Short: "Install the service and start it",
		Long: "Install the service and start it.\n\n" +
			"Needs a burrow.yaml (the path given, or burrow.yaml in the user config directory) and a\n" +
			"stored sign-in (burrow login). The service runs `burrow up` with the full paths of both;\n" +
			"--cacert, --server-name and --insecure of this command are kept for it. On Linux and\n" +
			"Windows this needs root or an administrator. A second copy is not installed.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return usageWithLine(cmd, "%s takes at most one burrow.yaml", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error { return runServiceInstall(cmd, d, args) },
	}
	root.AddCommand(
		install,
		&cobra.Command{Use: "uninstall", Short: "Stop the service and remove it", Args: noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return runServiceUninstall(cmd, d) }},
		&cobra.Command{Use: "start", Short: "Start the installed service", Args: noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return runServiceStartStop(cmd, d, "start") }},
		&cobra.Command{Use: "stop", Short: "Stop the installed service", Args: noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return runServiceStartStop(cmd, d, "stop") }},
		&cobra.Command{Use: "status", Short: "Show whether the service is installed and running", Args: noArgs,
			Long: "Show whether the service is installed and running.\n\nExit code 0 when it is installed, 1 when not.",
			RunE: func(cmd *cobra.Command, _ []string) error { return runServiceStatus(cmd, d) }},
		&cobra.Command{Use: "logs", Short: "Show the service's last log lines, or where they are", Args: noArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return runServiceLogs(cmd, d) }},
	)
	return root
}

// underServiceManager reports whether this process was started by the Windows
// service manager. A service there has to answer the manager, or it is ended
// after half a minute; systemd and launchd ask nothing of the kind.
var underServiceManager = func() bool {
	return runtime.GOOS == "windows" && !service.Interactive()
}

// serviceStopWait is how long a stop waits for the client to end.
const serviceStopWait = 10 * time.Second

// serviceLogMax is the size at which the service's log file starts again.
const serviceLogMax = 10 << 20

// serviceProgram is `burrow up` as the service manager starts and stops it.
type serviceProgram struct {
	run func(ctx context.Context) error
	// failed is told that the run ended without having been stopped.
	failed func(err error)
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *serviceProgram) Start(service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		if err := p.run(ctx); ctx.Err() == nil {
			p.failed(err)
		}
	}()
	return nil
}

func (p *serviceProgram) Stop(service.Service) error {
	p.cancel()
	select {
	case <-p.done:
	case <-time.After(serviceStopWait):
	}
	return nil
}

// openServiceLog opens the file the service's log lines go to where the
// system keeps none for it. A file that has grown past serviceLogMax starts
// again.
func openServiceLog(path string) (*os.File, error) {
	flags := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > serviceLogMax {
		flags |= os.O_TRUNC
	}
	return os.OpenFile(path, flags, 0o600)
}

// runUnderServiceManager runs the client as the program of the Windows
// service: started and stopped by the manager, its log lines in logPath
// because a service has no stderr anybody reads. A run that ends by itself
// ends the process with its exit code, and the manager starts it again.
var runUnderServiceManager = func(logPath string, run func(ctx context.Context) error) error {
	if f, err := openServiceLog(logPath); err == nil {
		defer f.Close()
		viewErr = f
	}
	p := &serviceProgram{run: run, failed: func(err error) {
		code := report(viewErr, err)
		if code == 0 {
			code = exitGeneral
		}
		os.Exit(code)
	}}
	s, err := service.New(p, &service.Config{Name: svc.Name})
	if err != nil {
		return err
	}
	return s.Run()
}
