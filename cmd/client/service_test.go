package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/ankoehn/burrow/cmd/client/svc"
)

// fakeManager stands for the system's service manager: nothing is installed
// on the machine the tests run on.
type fakeManager struct {
	calls      []string
	cfg        *service.Config // the config of the last manager that was built
	configs    []*service.Config
	status     service.Status
	statusErr  error
	installErr error
	startErr   error
	stopErr    error
	removeErr  error
	onInstall  func()
}

func (m *fakeManager) Install() error {
	m.calls = append(m.calls, "install")
	if m.onInstall != nil {
		m.onInstall()
	}
	return m.installErr
}
func (m *fakeManager) Uninstall() error { m.calls = append(m.calls, "uninstall"); return m.removeErr }
func (m *fakeManager) Start() error     { m.calls = append(m.calls, "start"); return m.startErr }
func (m *fakeManager) Stop() error      { m.calls = append(m.calls, "stop"); return m.stopErr }
func (m *fakeManager) Status() (service.Status, error) {
	m.calls = append(m.calls, "status")
	return m.status, m.statusErr
}

// did reports whether the manager was asked for the action.
func (m *fakeManager) did(action string) bool {
	for _, c := range m.calls {
		if c == action {
			return true
		}
	}
	return false
}

// serviceHarness is a harness whose machine has a service manager of the
// given system and nothing installed.
type serviceHarness struct {
	*harness
	m        *fakeManager
	env      *serviceEnv
	home     string // the installing user's home
	yaml     string // a burrow.yaml that exists
	exe      string
	unit     string   // where the system keeps the service definition
	dirs     []string // private directories that were made
	commands [][]string
}

func newServiceHarness(t *testing.T, goos string) *serviceHarness {
	t.Helper()
	h := newHarness(t)
	root := t.TempDir()
	s := &serviceHarness{
		harness: h,
		m:       &fakeManager{statusErr: service.ErrNotInstalled},
		home:    filepath.Join(root, "home", "kohn"),
		exe:     filepath.Join(root, "bin", "burrow"),
		unit:    filepath.Join(root, "units", "burrow.service"),
	}
	h.cfgPath = filepath.Join(s.home, ".config", "burrow", "config.yaml")
	s.yaml = filepath.Join(s.home, ".config", "burrow", "burrow.yaml")
	writeAt(t, s.yaml, servicesOnly)
	if err := os.MkdirAll(filepath.Dir(s.unit), 0o700); err != nil {
		t.Fatal(err)
	}
	s.env = &serviceEnv{
		goos:       goos,
		elevated:   func() bool { return goos != "darwin" },
		executable: func() (string, error) { return s.exe, nil },
		trusted:    func(string) bool { return true },
		// On the test's own system this is a directory name with a backslash
		// in it; the copies are its siblings. Good enough to see what is
		// written where and with which mode.
		programData: func() string { return filepath.Join(root, "pd") },
		lookupUser: func(user string) (string, string, error) {
			switch user {
			case "kohn":
				return s.home, "1000", nil
			case "toor": // root under another name
				return filepath.Join(root, "roothome"), "0", nil
			}
			return "", "", errors.New("no such user")
		},
		homeDir: func() (string, error) { return s.home, nil },
		system: func() string {
			switch goos {
			case "linux":
				return "linux-systemd"
			case "darwin":
				return "darwin-launchd"
			}
			return "windows-service"
		},
		manager: func(cfg *service.Config) (serviceManager, error) {
			s.m.cfg = cfg
			s.m.configs = append(s.m.configs, cfg)
			return s.m, nil
		},
		unitFile: func() string { return s.unit },
		makePrivateDir: func(path string) error {
			s.dirs = append(s.dirs, path)
			return os.Mkdir(path, 0o700)
		},
		run: func(out io.Writer, name string, args ...string) error {
			s.commands = append(s.commands, append([]string{name}, args...))
			fmt.Fprintln(out, "a line from the journal")
			return nil
		},
	}
	h.service = s.env
	if goos == "linux" {
		h.env["SUDO_USER"] = "kohn"
	}
	return s
}

// noTokenAnywhere fails when the token is in what was printed or in anything
// the service manager was given.
func (s *serviceHarness) noTokenAnywhere() {
	s.t.Helper()
	s.noToken()
	for _, cfg := range s.m.configs {
		if text := fmt.Sprintf("%+v %#v", *cfg, *cfg); strings.Contains(text, testToken) || strings.Contains(text, "--token") {
			s.t.Fatal("the token is in what the service manager was given")
		}
		if len(cfg.EnvVars) != 0 {
			s.t.Fatalf("the service definition carries environment variables: %d", len(cfg.EnvVars))
		}
	}
}

func TestServiceInstall_Linux(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.signIn()
	if code := s.exec("service", "install", s.yaml, "--config", s.cfgPath); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if !reflect.DeepEqual(s.m.calls, []string{"status", "install", "start"}) {
		t.Fatalf("calls %q", s.m.calls)
	}
	cfg := s.m.cfg
	wantArgs := []string{"up", "--file", s.yaml, "--config", s.cfgPath, "--log", "json"}
	if cfg.Name != "burrow" || cfg.UserName != "kohn" || cfg.Executable != s.exe ||
		!reflect.DeepEqual(cfg.Arguments, wantArgs) || cfg.WorkingDirectory != filepath.Dir(s.yaml) {
		t.Fatalf("config %+v", *cfg)
	}
	if cfg.Option["Restart"] != "always" || cfg.Option["UserService"] != nil {
		t.Fatalf("options %v", cfg.Option)
	}
	if !reflect.DeepEqual(cfg.Dependencies, []string{"Wants=network-online.target", "After=network-online.target"}) {
		t.Fatalf("dependencies %q", cfg.Dependencies)
	}
	out := s.stdout.String()
	if !strings.HasSuffix(out, "Installed and started. Logs: burrow service logs\n") {
		t.Fatalf("stdout %q", out)
	}
	for _, w := range []string{"Runs as:  kohn", s.exe + " up --file " + s.yaml} {
		if !strings.Contains(out, w) {
			t.Fatalf("stdout does not contain %q:\n%s", w, out)
		}
	}
	// Nothing was copied: the service runs as the user and reads the user's
	// own files.
	if len(s.dirs) != 0 {
		t.Fatalf("directories made: %q", s.dirs)
	}
	s.noTokenAnywhere()
}

// Through sudo the home is root's: the paths are those of the user who ran
// sudo.
func TestServiceInstall_LinuxDefaultPathsUnderSudo(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.signIn()
	s.cfgPath = filepath.Join(t.TempDir(), "root", ".config", "burrow", "config.yaml") // what $HOME gives under sudo
	stored := filepath.Join(s.home, ".config", "burrow", "config.yaml")
	if code := s.exec("service", "install"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	want := []string{"up", "--file", s.yaml, "--config", stored, "--log", "json"}
	if !reflect.DeepEqual(s.m.cfg.Arguments, want) {
		t.Fatalf("arguments %q", s.m.cfg.Arguments)
	}
}

// SUDO_USER ends up after User= in the unit: it is a user of this system, by
// name, or nothing is installed. Naming the files does not change that.
func TestServiceInstall_SudoUser(t *testing.T) {
	for _, name := range []string{"nobody-here", "0", "1000"} {
		s := newServiceHarness(t, "linux")
		s.signIn()
		s.harness.env["SUDO_USER"] = name
		code := s.exec("service", "install", s.yaml, "--config", s.cfgPath)
		if code != 1 || len(s.m.calls) != 0 || !strings.Contains(s.stderr.String(), "SUDO_USER") || !strings.Contains(s.stderr.String(), "Nothing was installed") {
			t.Fatalf("%q: exit %d, calls %q, stderr %q", name, code, s.m.calls, s.stderr.String())
		}
	}

	// A name that is root's: the service is root's, with all that asks.
	s := newServiceHarness(t, "linux")
	s.signIn()
	s.harness.env["SUDO_USER"] = "toor"
	if code := s.exec("service", "install", s.yaml, "--config", s.cfgPath); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if s.m.cfg.UserName != "" || !strings.Contains(s.stdout.String(), "Runs as:  root") {
		t.Fatalf("user %q, stdout %q", s.m.cfg.UserName, s.stdout.String())
	}
	s = newServiceHarness(t, "linux")
	s.signIn()
	s.harness.env["SUDO_USER"] = "toor"
	s.env.trusted = func(p string) bool { return p != s.yaml }
	if code := s.exec("service", "install", s.yaml, "--config", s.cfgPath); code != 1 || s.m.did("install") {
		t.Fatalf("exit %d, calls %q", code, s.m.calls)
	}
}

// As root the service reads these files with root's rights on every start:
// each has to be root's alone.
func TestServiceInstall_RootReadsOnlyRootsFiles(t *testing.T) {
	setup := func(t *testing.T) (s *serviceHarness, ca, tok string) {
		s = newServiceHarness(t, "linux")
		delete(s.harness.env, "SUDO_USER")
		s.signIn()
		ca = filepath.Join(s.home, "ca.pem")
		tok = filepath.Join(s.home, "tok")
		writeAt(t, ca, "not read at install\n")
		writeAt(t, tok, testToken+"\n")
		writeAt(t, s.yaml, "token_file: "+tok+"\n"+servicesOnly)
		return s, ca, tok
	}
	s, ca, tok := setup(t)
	var asked []string
	s.env.trusted = func(p string) bool { asked = append(asked, p); return true }
	if code := s.exec("service", "install", "--cacert", ca); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if !reflect.DeepEqual(asked, []string{s.exe, s.yaml, s.cfgPath, ca, tok}) {
		t.Fatalf("asked about %q", asked)
	}
	s.noTokenAnywhere()

	for _, which := range []string{"yaml", "config", "cacert", "token_file"} {
		s, ca, tok := setup(t)
		bad := map[string]string{"yaml": s.yaml, "config": s.cfgPath, "cacert": ca, "token_file": tok}[which]
		s.env.trusted = func(p string) bool { return p != bad }
		code := s.exec("service", "install", "--cacert", ca)
		if code != 1 || len(s.dirs) != 0 || s.m.did("install") || !strings.Contains(s.stderr.String(), "read "+bad+" on every start") {
			t.Fatalf("%s: exit %d, calls %q, stderr %q", which, code, s.m.calls, s.stderr.String())
		}
		s.noTokenAnywhere()
	}

	// A service that runs as the installing user reads that user's files as
	// that user: nothing is asked about them.
	s, ca, _ = setup(t)
	s.harness.env["SUDO_USER"] = "kohn"
	s.env.trusted = func(string) bool { return false }
	if code := s.exec("service", "install", "--cacert", ca); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
}

// As root the unit names the files themselves: a link in a directory of
// somebody else could be pointed elsewhere after the install.
func TestServiceInstall_RootResolvesLinks(t *testing.T) {
	s := newServiceHarness(t, "linux")
	delete(s.harness.env, "SUDO_USER")
	s.signIn()
	link := filepath.Join(t.TempDir(), "link.yaml")
	if err := os.Symlink(s.yaml, link); err != nil {
		t.Fatal(err)
	}
	if code := s.exec("service", "install", link); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if got := s.m.cfg.Arguments[2]; got != s.yaml {
		t.Fatalf("--file %q, want the file the link points to", got)
	}
}

// Run as root itself, the service is root's: root's sign-in, no User=.
func TestServiceInstall_LinuxAsRoot(t *testing.T) {
	s := newServiceHarness(t, "linux")
	delete(s.harness.env, "SUDO_USER")
	s.signIn()
	if code := s.exec("service", "install"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if s.m.cfg.UserName != "" || !strings.Contains(s.stdout.String(), "Runs as:  root") {
		t.Fatalf("user %q, stdout %q", s.m.cfg.UserName, s.stdout.String())
	}

	// ... and then never from a binary somebody else can replace.
	s = newServiceHarness(t, "linux")
	delete(s.harness.env, "SUDO_USER")
	s.signIn()
	s.env.trusted = func(string) bool { return false }
	code := s.exec("service", "install")
	if code != 1 || s.m.did("install") || !strings.Contains(s.stderr.String(), "would run as root") {
		t.Fatalf("exit %d, calls %q, stderr %q", code, s.m.calls, s.stderr.String())
	}
}

func TestServiceInstall_NotElevated(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.signIn()
	delete(s.harness.env, "SUDO_USER")
	s.env.elevated = func() bool { return false }
	code := s.exec("service", "install", "--insecure")
	want := "Installing the burrow service needs root. Nothing was changed. Run:\n" +
		"  sudo " + s.exe + " service install " + s.yaml + " --config " + s.cfgPath + " --insecure\n"
	if code != 1 || s.stderr.String() != want {
		t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
	}
	if len(s.m.calls) != 0 || len(s.dirs) != 0 {
		t.Fatalf("something was attempted: %q %q", s.m.calls, s.dirs)
	}
	if _, err := os.Lstat(s.unit); err == nil {
		t.Fatal("a unit file exists")
	}
}

func TestServiceInstall_Refusals(t *testing.T) {
	t.Run("no burrow.yaml", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		if err := os.Remove(s.yaml); err != nil {
			t.Fatal(err)
		}
		code := s.exec("service", "install")
		if code != 2 || s.stderr.String() != "burrow service needs a burrow.yaml. Create one (see: burrow up) or pass its path.\n" {
			t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
		}
	})
	t.Run("a path that is not there", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		code := s.exec("service", "install", filepath.Join(s.home, "missing.yaml"))
		if code != 2 || !strings.Contains(s.stderr.String(), "needs a burrow.yaml") {
			t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
		}
	})
	t.Run("not signed in", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		code := s.exec("service", "install")
		if code != 3 || s.stderr.String() != msgNotSignedIn+"\n" {
			t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
		}
	})
	t.Run("already installed", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		s.m.status, s.m.statusErr = service.StatusStopped, nil
		code := s.exec("service", "install")
		if code != 1 || s.stderr.String() != "The burrow service is already installed. Run: burrow service uninstall\n" || s.m.did("install") {
			t.Fatalf("exit %d, stderr %q, calls %q", code, s.stderr.String(), s.m.calls)
		}
	})
	t.Run("linux without systemd", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		s.env.system = func() string { return "linux-openrc" }
		code := s.exec("service", "install")
		if code != 1 || !strings.Contains(s.stderr.String(), "systemd") || len(s.m.calls) != 0 {
			t.Fatalf("exit %d, stderr %q, calls %q", code, s.stderr.String(), s.m.calls)
		}
	})
	t.Run("a burrow.yaml for another relay than the sign-in", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		writeAt(t, s.yaml, "server: other.example.com:7000\n"+servicesOnly)
		code := s.exec("service", "install")
		if code != 3 || s.m.did("install") {
			t.Fatalf("exit %d, calls %q, stderr %q", code, s.m.calls, s.stderr.String())
		}
	})
}

// The environment of the person who installs is not the service's: a token
// in it does not count as a sign-in, and is not carried anywhere.
func TestServiceInstall_IgnoresTheEnvironment(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.harness.env["BURROW_SERVER"], s.harness.env["BURROW_TOKEN"] = "env.example.com:7000", testToken
	code := s.exec("service", "install")
	if code != 3 || s.m.did("install") {
		t.Fatalf("exit %d, calls %q", code, s.m.calls)
	}
	s.noTokenAnywhere()
}

func TestServiceInstall_Darwin(t *testing.T) {
	s := newServiceHarness(t, "darwin")
	s.signIn()
	if code := s.exec("service", "install"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	cfg := s.m.cfg
	logDir := s.home + "/Library/Logs/burrow"
	if cfg.UserName != "" || cfg.Option["UserService"] != true || cfg.Option["RunAtLoad"] != true ||
		cfg.Option["KeepAlive"] != true || cfg.Option["LogDirectory"] != logDir {
		t.Fatalf("config %+v", *cfg)
	}
	fi, err := os.Stat(logDir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("log directory: %v %v", fi, err)
	}
	s.noTokenAnywhere()
}

func TestServiceInstall_Windows(t *testing.T) {
	s := newServiceHarness(t, "windows")
	s.signIn()
	ca := filepath.Join(s.home, "ca.pem")
	writeAt(t, ca, "-----BEGIN CERTIFICATE-----\n")
	if code := s.exec("service", "install", "--config", s.cfgPath, "--cacert", ca); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	dir := s.env.programData() + `\burrow`
	if !reflect.DeepEqual(s.dirs, []string{dir}) {
		t.Fatalf("private directories %q", s.dirs)
	}
	for from, to := range map[string]string{s.cfgPath: dir + `\config.yaml`, s.yaml: dir + `\burrow.yaml`, ca: dir + `\ca.pem`} {
		want, _ := os.ReadFile(from)
		got, err := os.ReadFile(to)
		if err != nil || string(got) != string(want) {
			t.Fatalf("copy of %s: %v", filepath.Base(from), err)
		}
		fi, _ := os.Lstat(to)
		if fi.Mode() != 0o600 {
			t.Fatalf("mode of the copy of %s: %v", filepath.Base(from), fi.Mode())
		}
		if !strings.Contains(s.stdout.String(), "Copied "+from+" to "+to) {
			t.Fatalf("stdout does not say what was copied: %s", s.stdout.String())
		}
	}
	cfg := s.m.cfg
	wantArgs := []string{"up", "--file", dir + `\burrow.yaml`, "--config", dir + `\config.yaml`, "--cacert", dir + `\ca.pem`, "--log", "json"}
	if cfg.UserName != "" || !reflect.DeepEqual(cfg.Arguments, wantArgs) || cfg.Option["OnFailure"] != "restart" {
		t.Fatalf("config %+v", *cfg)
	}
	if !strings.Contains(s.stdout.String(), "Runs as:  LocalSystem") ||
		!strings.Contains(s.stdout.String(), "only SYSTEM and Administrators") {
		t.Fatalf("stdout %q", s.stdout.String())
	}
	s.noTokenAnywhere()

	// uninstall takes the copies away again, and the directory with them.
	s.m.calls, s.m.status, s.m.statusErr = nil, service.StatusRunning, nil
	writeAt(t, dir+`\burrow.log`, "a log line\n")
	writeAt(t, dir+`\burrow.log.1`, "an older log line\n")
	if code := s.exec("service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	for _, p := range []string{dir, dir + `\config.yaml`, dir + `\burrow.yaml`, dir + `\ca.pem`, dir + `\burrow.log`, dir + `\burrow.log.1`} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s is still there (%v)", p, err)
		}
	}
	// The user's own files are not the install's to remove.
	for _, p := range []string{s.cfgPath, s.yaml, ca} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
}

// uninstall removes what an install of this client made, and only that: with
// no service installed, a directory of that name is somebody else's.
func TestServiceUninstall_WindowsLeavesWhatIsNotOurs(t *testing.T) {
	s := newServiceHarness(t, "windows")
	dir := s.env.programData() + `\burrow`
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{dir + `\config.yaml`, dir + `\burrow.yaml`, dir + `\burrow.log`}
	for _, f := range files {
		writeAt(t, f, "not this client's\n")
	}
	if code := s.exec("service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	for _, p := range append(files, dir) {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed although no service was installed", p)
		}
	}
	if !strings.Contains(s.stdout.String(), "The burrow service is not installed.") || !strings.Contains(s.stdout.String(), dir) {
		t.Fatalf("stdout %q", s.stdout.String())
	}

	// With the service installed: a link where a copy was is not followed
	// and not removed, and what is behind it stays.
	s = newServiceHarness(t, "windows")
	s.m.status, s.m.statusErr = service.StatusStopped, nil
	dir = s.env.programData() + `\burrow`
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	writeAt(t, victim, "keep\n")
	if err := os.Symlink(victim, dir+`\burrow.log`); err != nil {
		t.Fatal(err)
	}
	writeAt(t, dir+`\config.yaml`, "a copy\n")
	if code := s.exec("service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if _, err := os.Lstat(dir + `\burrow.log`); err != nil {
		t.Fatal("a link was removed")
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
		t.Fatal("the file behind the link was touched")
	}
	if _, err := os.Lstat(dir + `\config.yaml`); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the copy is still there")
	}

	// The directory itself is a link (on Windows: a junction): nothing
	// behind it is touched.
	s = newServiceHarness(t, "windows")
	s.m.status, s.m.statusErr = service.StatusStopped, nil
	dir = s.env.programData() + `\burrow`
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, dir); err != nil {
		t.Fatal(err)
	}
	writeAt(t, dir+`\config.yaml`, "somebody's\n")
	if code := s.exec("service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	for _, p := range []string{dir, dir + `\config.yaml`} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s was removed through a link", p)
		}
	}
	if !strings.Contains(s.stdout.String(), "left alone") {
		t.Fatalf("stdout %q", s.stdout.String())
	}
}

func TestServiceInstall_WindowsRefusesTokenFile(t *testing.T) {
	s := newServiceHarness(t, "windows")
	s.signIn()
	tok := filepath.Join(s.home, "tok")
	writeAt(t, tok, testToken+"\n")
	writeAt(t, s.yaml, "token_file: "+tok+"\n"+servicesOnly)
	code := s.exec("service", "install")
	if code != 1 || len(s.dirs) != 0 || s.m.did("install") || !strings.Contains(s.stderr.String(), "token_file") {
		t.Fatalf("exit %d, dirs %q, calls %q, stderr %q", code, s.dirs, s.m.calls, s.stderr.String())
	}
	s.noTokenAnywhere()
}

func TestServiceInstall_WindowsRefusals(t *testing.T) {
	t.Run("a binary a user can replace", func(t *testing.T) {
		s := newServiceHarness(t, "windows")
		s.signIn()
		s.env.trusted = func(string) bool { return false }
		code := s.exec("service", "install")
		if code != 1 || len(s.dirs) != 0 || s.m.did("install") || !strings.Contains(s.stderr.String(), "LocalSystem") {
			t.Fatalf("exit %d, dirs %q, calls %q, stderr %q", code, s.dirs, s.m.calls, s.stderr.String())
		}
	})
	t.Run("the directory is already there", func(t *testing.T) {
		s := newServiceHarness(t, "windows")
		s.signIn()
		dir := s.env.programData() + `\burrow`
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Fatal(err)
		}
		code := s.exec("service", "install")
		if code != 1 || s.m.did("install") || !strings.Contains(s.stderr.String(), dir) {
			t.Fatalf("exit %d, calls %q, stderr %q", code, s.m.calls, s.stderr.String())
		}
		// Somebody else's directory: nothing is put into it and it stays.
		if _, err := os.Lstat(dir + `\config.yaml`); err == nil {
			t.Fatal("the sign-in was copied next to a directory this install did not make")
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Fatal("the directory was removed")
		}
	})
	t.Run("not elevated", func(t *testing.T) {
		s := newServiceHarness(t, "windows")
		s.signIn()
		s.env.elevated = func() bool { return false }
		code := s.exec("service", "install")
		if code != 1 || len(s.dirs) != 0 || len(s.m.calls) != 0 || !strings.Contains(s.stderr.String(), "elevated terminal") ||
			strings.Contains(s.stderr.String(), "sudo") {
			t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
		}
	})
}

// No half-installed state: what the failed install wrote is taken away.
func TestServiceInstall_FailureLeavesNothing(t *testing.T) {
	t.Run("linux", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		s.m.installErr = errors.New("systemctl enable failed")
		s.m.removeErr = errors.New("systemctl disable failed") // as it does for a unit that never loaded
		s.m.onInstall = func() { writeAt(t, s.unit, "[Unit]\n") }
		code := s.exec("service", "install")
		if code != 1 || !strings.Contains(s.stderr.String(), "systemctl enable failed") || !strings.Contains(s.stderr.String(), "Nothing is installed") {
			t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
		}
		if _, err := os.Lstat(s.unit); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the unit file of the failed install is still there")
		}
		if s.m.did("start") {
			t.Fatal("a service that was not installed was started")
		}
	})
	t.Run("a unit that was there before stays", func(t *testing.T) {
		s := newServiceHarness(t, "linux")
		s.signIn()
		writeAt(t, s.unit, "somebody else's\n")
		s.m.installErr = errors.New("Init already exists")
		if code := s.exec("service", "install"); code != 1 {
			t.Fatalf("exit %d", code)
		}
		if b, _ := os.ReadFile(s.unit); string(b) != "somebody else's\n" {
			t.Fatal("a unit file this install did not write was touched")
		}
	})
	t.Run("windows", func(t *testing.T) {
		s := newServiceHarness(t, "windows")
		s.signIn()
		s.m.installErr = errors.New("access denied")
		dir := s.env.programData() + `\burrow`
		if code := s.exec("service", "install"); code != 1 {
			t.Fatalf("exit %d", code)
		}
		for _, p := range []string{dir, dir + `\config.yaml`, dir + `\burrow.yaml`} {
			if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s is still there", p)
			}
		}
		s.noTokenAnywhere()
	})
	t.Run("a copy fails", func(t *testing.T) {
		s := newServiceHarness(t, "windows")
		s.signIn()
		dir := s.env.programData() + `\burrow`
		s.env.makePrivateDir = func(path string) error {
			// Somebody put a link where the sign-in is to go.
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.Symlink(victim, dir+`\config.yaml`); err != nil {
				t.Fatal(err)
			}
			return os.Mkdir(path, 0o700)
		}
		code := s.exec("service", "install")
		if code != 1 || s.m.did("install") {
			t.Fatalf("exit %d, calls %q", code, s.m.calls)
		}
		// The link is not this install's: it is neither followed nor removed.
		if fi, err := os.Lstat(dir + `\config.yaml`); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link: %v", err)
		}
		if _, err := os.Lstat(dir + `\burrow.yaml`); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the copy made before the failure is still there")
		}
	})
}

// Installed, but it does not start: that is said, and it stays installed.
func TestServiceInstall_StartFails(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.signIn()
	s.m.startErr = errors.New("start request failed")
	code := s.exec("service", "install")
	if code != 1 || s.m.did("uninstall") || !strings.Contains(s.stderr.String(), "did not start") ||
		!strings.Contains(s.stderr.String(), "burrow service logs") {
		t.Fatalf("exit %d, calls %q, stderr %q", code, s.m.calls, s.stderr.String())
	}
}

func TestCopyPrivate(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("token: "+testToken+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	if err := copyPrivate(src, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dst)
	if err != nil || fi.Mode() != 0o600 {
		t.Fatalf("mode %v, err %v", fi.Mode(), err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "token: "+testToken+"\n" {
		t.Fatal("content differs")
	}

	// A file that is there is not written to.
	if err := os.WriteFile(src, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyPrivate(src, dst); err == nil {
		t.Fatal("an existing file was overwritten")
	}

	// A link is not followed, wherever it points.
	victim := filepath.Join(dir, "victim")
	for _, target := range []string{victim, filepath.Join(dir, "nowhere")} {
		if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := copyPrivate(src, link)
		if err == nil || strings.Contains(err.Error(), testToken) {
			t.Fatalf("copy onto a link: %v", err != nil)
		}
		if b, _ := os.ReadFile(victim); string(b) != "keep\n" {
			t.Fatal("the copy went through the link")
		}
		if _, err := os.Lstat(filepath.Join(dir, "nowhere")); err == nil {
			t.Fatal("the copy created the link's target")
		}
	}
}

func TestServiceUninstall(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.m.status, s.m.statusErr = service.StatusStopped, nil
	s.m.stopErr = errors.New("not running")
	if code := s.exec("service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	if !reflect.DeepEqual(s.m.calls, []string{"status", "stop", "uninstall"}) || s.stdout.String() != "Uninstalled the burrow service.\n" {
		t.Fatalf("calls %q, stdout %q", s.m.calls, s.stdout.String())
	}

	s = newServiceHarness(t, "linux")
	if code := s.exec("service", "uninstall"); code != 0 || s.stdout.String() != "The burrow service is not installed.\n" || s.m.did("uninstall") {
		t.Fatalf("exit %d, stdout %q, calls %q", code, s.stdout.String(), s.m.calls)
	}

	s = newServiceHarness(t, "linux")
	s.m.status, s.m.statusErr = service.StatusRunning, nil
	s.m.removeErr = errors.New("disable failed")
	if code := s.exec("service", "uninstall"); code != 1 || !strings.Contains(s.stderr.String(), "disable failed") {
		t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
	}

	s = newServiceHarness(t, "linux")
	s.env.elevated = func() bool { return false }
	code := s.exec("service", "uninstall")
	if code != 1 || len(s.m.calls) != 0 || s.stderr.String() != "Removing the burrow service needs root. Nothing was changed. Run:\n  sudo "+s.exe+" service uninstall\n" {
		t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
	}
}

// A service that is in a failed state is installed: uninstall removes it.
func TestServiceUninstall_FailedState(t *testing.T) {
	s := newServiceHarness(t, "linux")
	s.m.status, s.m.statusErr = service.StatusUnknown, errors.New("service in failed state")
	if code := s.exec("service", "uninstall"); code != 0 || !s.m.did("uninstall") {
		t.Fatalf("exit %d, calls %q", code, s.m.calls)
	}
}

func TestServiceStartStop(t *testing.T) {
	for _, tc := range []struct{ action, said string }{{"start", "Started the burrow service.\n"}, {"stop", "Stopped the burrow service.\n"}} {
		s := newServiceHarness(t, "linux")
		s.m.status, s.m.statusErr = service.StatusStopped, nil
		if code := s.exec("service", tc.action); code != 0 || !s.m.did(tc.action) || s.stdout.String() != tc.said {
			t.Fatalf("%s: exit %d, calls %q, stdout %q", tc.action, code, s.m.calls, s.stdout.String())
		}

		s = newServiceHarness(t, "linux")
		code := s.exec("service", tc.action)
		if code != 1 || s.m.did(tc.action) || s.stderr.String() != "The burrow service is not installed. Run: burrow service install\n" {
			t.Fatalf("%s: exit %d, stderr %q", tc.action, code, s.stderr.String())
		}

		s = newServiceHarness(t, "linux")
		s.env.elevated = func() bool { return false }
		if code := s.exec("service", tc.action); code != 1 || len(s.m.calls) != 0 || !strings.Contains(s.stderr.String(), "sudo "+s.exe+" service "+tc.action) {
			t.Fatalf("%s: exit %d, stderr %q", tc.action, code, s.stderr.String())
		}

		s = newServiceHarness(t, "linux")
		s.m.status, s.m.statusErr = service.StatusStopped, nil
		s.m.startErr, s.m.stopErr = errors.New("refused by the manager"), errors.New("refused by the manager")
		if code := s.exec("service", tc.action); code != 1 || !strings.Contains(s.stderr.String(), "refused by the manager") {
			t.Fatalf("%s: exit %d, stderr %q", tc.action, code, s.stderr.String())
		}

		// The agent on macOS is the user's own: no elevation.
		s = newServiceHarness(t, "darwin")
		s.m.status, s.m.statusErr = service.StatusStopped, nil
		if code := s.exec("service", tc.action); code != 0 {
			t.Fatalf("%s on darwin: exit %d", tc.action, code)
		}
		if s.m.cfg.Option["UserService"] != true {
			t.Fatal("the agent was looked for among the system's daemons")
		}
	}
}

func TestServiceStatus(t *testing.T) {
	cases := []struct {
		name   string
		status service.Status
		err    error
		code   int
		stdout string
		stderr string
	}{
		{"running", service.StatusRunning, nil, 0, "Service:  installed, running\n", ""},
		{"stopped", service.StatusStopped, nil, 0, "Service:  installed, stopped\n", ""},
		{"not installed", service.StatusUnknown, service.ErrNotInstalled, 1, "", "The burrow service is not installed.\n"},
		{"failed", service.StatusUnknown, errors.New("service in failed state"), 0, "Service:  unknown (service in failed state)\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newServiceHarness(t, "linux")
			s.env.elevated = func() bool { return false } // looking needs no rights
			s.m.status, s.m.statusErr = tc.status, tc.err
			code := s.exec("service", "status")
			if code != tc.code || s.stdout.String() != tc.stdout || s.stderr.String() != tc.stderr {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, s.stdout.String(), s.stderr.String())
			}
		})
	}
}

func TestServiceLogs(t *testing.T) {
	s := newServiceHarness(t, "linux")
	if code := s.exec("service", "logs"); code != 0 {
		t.Fatalf("exit %d: %s", code, s.stderr.String())
	}
	want := []string{"journalctl", "-u", "burrow", "-n", "100", "--no-pager"}
	if len(s.commands) != 1 || !reflect.DeepEqual(s.commands[0], want) {
		t.Fatalf("commands %q", s.commands)
	}
	if s.stdout.String() != "a line from the journal\n" || !strings.Contains(s.stderr.String(), "journalctl -u burrow -n 100 --no-pager") {
		t.Fatalf("stdout %q, stderr %q", s.stdout.String(), s.stderr.String())
	}

	s = newServiceHarness(t, "darwin")
	if code := s.exec("service", "logs"); code != 0 || len(s.commands) != 0 ||
		!strings.Contains(s.stdout.String(), s.home+"/Library/Logs/burrow/burrow.err.log") {
		t.Fatalf("exit %d, commands %q, stdout %q", code, s.commands, s.stdout.String())
	}

	s = newServiceHarness(t, "windows")
	if code := s.exec("service", "logs"); code != 0 || len(s.commands) != 0 ||
		!strings.Contains(s.stdout.String(), s.env.programData()+`\burrow\burrow.log`) || !strings.Contains(s.stdout.String(), "elevated") {
		t.Fatalf("exit %d, commands %q, stdout %q", code, s.commands, s.stdout.String())
	}
}

func TestService_Usage(t *testing.T) {
	s := newServiceHarness(t, "linux")
	if code := s.exec("service", "restart"); code != 2 || !strings.Contains(s.stderr.String(), `unknown command "restart" for "burrow service"`) {
		t.Fatalf("exit %d, stderr %q", code, s.stderr.String())
	}
	if code := s.exec("service", "install", "a.yaml", "b.yaml"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := s.exec("service", "stop", "now"); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := s.exec("service"); code != 0 || !strings.Contains(s.stdout.String(), "install") {
		t.Fatalf("exit %d, stdout %q", code, s.stdout.String())
	}
	if len(s.m.calls) != 0 {
		t.Fatalf("calls %q", s.m.calls)
	}
}

// `burrow status` says whether the service is installed and runs; that does
// not change its exit code.
func TestStatus_ReportsTheService(t *testing.T) {
	cases := []struct {
		status service.Status
		err    error
		want   string
	}{
		{service.StatusRunning, nil, "Service:  installed, running\n"},
		{service.StatusStopped, nil, "Service:  installed, stopped\n"},
		{service.StatusUnknown, service.ErrNotInstalled, "Service:  not installed\n"},
		{service.StatusUnknown, errors.New("no answer"), "Service:  unknown (no answer)\n"},
	}
	for _, tc := range cases {
		s := newServiceHarness(t, "linux")
		s.env.elevated = func() bool { return false }
		s.signIn()
		s.m.status, s.m.statusErr = tc.status, tc.err
		if code := s.exec("status"); code != 0 || !strings.HasSuffix(s.stdout.String(), tc.want) {
			t.Fatalf("exit %d, stdout %q", code, s.stdout.String())
		}
		if !reflect.DeepEqual(s.m.calls, []string{"status"}) {
			t.Fatalf("calls %q", s.m.calls)
		}
		s.noToken()
	}

	// No service manager this client installs into: nothing is asked.
	s := newServiceHarness(t, "linux")
	s.signIn()
	s.env.system = func() string { return "linux-openrc" }
	if code := s.exec("status"); code != 0 || !strings.HasSuffix(s.stdout.String(), "Service:  not installed\n") || len(s.m.calls) != 0 {
		t.Fatalf("exit %d, stdout %q, calls %q", code, s.stdout.String(), s.m.calls)
	}

	// Not signed in: exit 3 as before, and no service line.
	s = newServiceHarness(t, "linux")
	if code := s.exec("status"); code != 3 || s.stdout.Len() != 0 {
		t.Fatalf("exit %d, stdout %q", code, s.stdout.String())
	}
}

// Under the Windows service manager `burrow up` is started and stopped by it.
func TestServiceProgram(t *testing.T) {
	started := make(chan context.Context, 1)
	failed := make(chan error, 1)
	p := &serviceProgram{
		run: func(ctx context.Context) error {
			started <- ctx
			<-ctx.Done() // the client runs until it is told to stop
			return ctx.Err()
		},
		failed: func(err error) { failed <- err },
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	ctx := <-started
	if ctx.Err() != nil {
		t.Fatal("stopped before it was told to")
	}
	if err := p.Stop(nil); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("Stop did not end the run")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("Stop returned before the run had ended")
	}
	select {
	case err := <-failed:
		t.Fatalf("a run that was stopped counts as failed: %v", err)
	default:
	}

	// A run that ends by itself is a failure the manager has to see.
	p = &serviceProgram{
		run:    func(context.Context) error { return errors.New("not signed in") },
		failed: func(err error) { failed <- err },
	}
	if err := p.Start(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if err == nil || err.Error() != "not signed in" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the run was not reported")
	}
}

// `burrow up` hands itself to the service manager when it was started by it,
// and only then.
func TestUp_UnderTheServiceManager(t *testing.T) {
	h := newHarness(t)
	h.signIn()
	yaml := filepath.Join(filepath.Dir(h.cfgPath), "burrow.yaml")
	writeAt(t, yaml, servicesOnly)

	var logPath string
	handed := 0
	oldUnder, oldRun := underServiceManager, runUnderServiceManager
	t.Cleanup(func() { underServiceManager, runUnderServiceManager = oldUnder, oldRun })
	runUnderServiceManager = func(log string, run func(context.Context) error) error {
		handed++
		logPath = log
		return run(context.Background())
	}

	underServiceManager = func() bool { return false }
	if code := h.exec("up", "--file", yaml); code != 0 || handed != 0 {
		t.Fatalf("exit %d, handed over %d times", code, handed)
	}
	h.runs = nil
	underServiceManager = func() bool { return true }
	if code := h.exec("up", "--file", yaml, "--config", h.cfgPath, "--log", "json"); code != 0 || handed != 1 {
		t.Fatalf("exit %d, handed over %d times: %s", code, handed, h.stderr.String())
	}
	if h.oneRun().creds.Token != testToken || logPath != filepath.Join(filepath.Dir(h.cfgPath), "burrow.log") {
		t.Fatalf("log path %q", logPath)
	}
}

// The service's log on Windows does not grow without end while the service
// runs: past the limit the file becomes burrow.log.1, in place of the one
// before, and a new one begins.
func TestServiceLog_Rotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "burrow.log")
	l, err := openServiceLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.max = 20
	write := func(s string) {
		t.Helper()
		if n, err := l.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("write: %d, %v", n, err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	write("0123456789\n")
	write("abcdefgh\n") // 20 bytes: still within the limit
	if read(path) != "0123456789\nabcdefgh\n" || read(path+".1") != "" {
		t.Fatalf("log %q, older %q", read(path), read(path+".1"))
	}
	write("next\n")
	if read(path) != "next\n" || read(path+".1") != "0123456789\nabcdefgh\n" {
		t.Fatalf("log %q, older %q", read(path), read(path+".1"))
	}
	write("0123456789abcdefghij\n") // one line longer than the limit is still written whole
	if read(path) != "0123456789abcdefghij\n" || read(path+".1") != "next\n" {
		t.Fatalf("log %q, older %q", read(path), read(path+".1"))
	}
	for _, p := range []string{path, path + ".1"} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v", p, err)
		}
	}
	l.Close()

	// A start finds the log of the run before and goes on in it.
	l2, err := openServiceLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if _, err := l2.Write([]byte("again\n")); err != nil {
		t.Fatal(err)
	}
	if read(path) != "0123456789abcdefghij\nagain\n" {
		t.Fatalf("log %q", read(path))
	}
}

func TestServiceConfig_NoPlanFieldIsLost(t *testing.T) {
	p := svc.Plan{
		Name: "burrow", DisplayName: "Burrow client", Description: "d", Executable: "/usr/local/bin/burrow",
		Arguments: []string{"up"}, WorkingDir: "/w", UserName: "kohn",
	}
	cfg := serviceConfig(p, "linux")
	if cfg.Name != p.Name || cfg.DisplayName != p.DisplayName || cfg.Description != p.Description ||
		cfg.Executable != p.Executable || cfg.WorkingDirectory != p.WorkingDir || cfg.UserName != p.UserName || len(cfg.EnvVars) != 0 {
		t.Fatalf("config %+v", *cfg)
	}
}
