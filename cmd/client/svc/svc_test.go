package svc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// fakeToken stands for the token of the sign-in. BuildPlan is never given it;
// the tests make sure nothing that looks like it ends up in a Plan.
const fakeToken = "bur_test_0000_NEVER_IN_A_UNIT"

func linuxInputs() Inputs {
	return Inputs{
		GOOS: "linux", Executable: "/usr/local/bin/burrow",
		YAMLPath: "/home/kohn/.config/burrow/burrow.yaml", UserConfigPath: "/home/kohn/.config/burrow/config.yaml",
		UserName: "kohn", Elevated: true, YAMLExists: true, SignedIn: true,
		Command: "sudo /usr/local/bin/burrow service install",
	}
}

func windowsInputs() Inputs {
	return Inputs{
		GOOS: "windows", Executable: `C:\Program Files\burrow\burrow.exe`,
		YAMLPath: `C:\Users\kohn\AppData\Roaming\burrow\burrow.yaml`, UserConfigPath: `C:\Users\kohn\AppData\Roaming\burrow\config.yaml`,
		ProgramData: `C:\ProgramData`, Elevated: true, YAMLExists: true, SignedIn: true, ExecutableTrusted: true,
		Command: `"C:\Program Files\burrow\burrow.exe" service install`,
	}
}

func TestBuildPlan_Linux(t *testing.T) {
	p, err := BuildPlan(linuxInputs())
	if err != nil {
		t.Fatal(err)
	}
	want := Plan{
		Name: "burrow", DisplayName: "Burrow client", Description: "Keeps the services of burrow.yaml connected to the relay (burrow up).",
		Executable: "/usr/local/bin/burrow",
		Arguments: []string{"up", "--file", "/home/kohn/.config/burrow/burrow.yaml",
			"--config", "/home/kohn/.config/burrow/config.yaml", "--log", "json"},
		WorkingDir: "/home/kohn/.config/burrow", UserName: "kohn", RunsAs: "kohn",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("plan:\n got %#v\nwant %#v", p, want)
	}
}

// Without sudo the installing user is root: the service runs as root, and
// then only a binary nobody else can change is accepted.
func TestBuildPlan_LinuxAsRoot(t *testing.T) {
	for _, name := range []string{"", "root"} {
		in := linuxInputs()
		in.UserName = name
		_, err := BuildPlan(in)
		var ue *UntrustedExecutableError
		if !errors.As(err, &ue) {
			t.Fatalf("user %q: err = %v, want an UntrustedExecutableError", name, err)
		}
		for _, w := range []string{"root", "/usr/local/bin/burrow", "Nothing was installed"} {
			if !strings.Contains(err.Error(), w) {
				t.Fatalf("message does not contain %q: %s", w, err)
			}
		}
		in.ExecutableTrusted = true
		p, err := BuildPlan(in)
		if err != nil {
			t.Fatal(err)
		}
		if p.UserName != "" || p.RunsAs != "root" {
			t.Fatalf("UserName %q, RunsAs %q", p.UserName, p.RunsAs)
		}
	}
}

// A service that runs as the installing user may run that user's own binary.
func TestBuildPlan_LinuxUserBinaryForUserService(t *testing.T) {
	in := linuxInputs()
	in.Executable, in.ExecutableTrusted = "/home/kohn/.local/bin/burrow", false
	if _, err := BuildPlan(in); err != nil {
		t.Fatal(err)
	}
}

func TestBuildPlan_Darwin(t *testing.T) {
	in := linuxInputs()
	in.GOOS, in.Elevated, in.UserName = "darwin", false, "kohn"
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if !p.UserScope || p.UserName != "" || p.ConfigDir != "" || len(p.Copies) != 0 {
		t.Fatalf("plan %#v", p)
	}
}

func TestBuildPlan_Windows(t *testing.T) {
	p, err := BuildPlan(windowsInputs())
	if err != nil {
		t.Fatal(err)
	}
	if p.ConfigDir != `C:\ProgramData\burrow` || p.UserName != "" || p.UserScope || p.RunsAs != "LocalSystem" {
		t.Fatalf("plan %#v", p)
	}
	wantArgs := []string{"up", "--file", `C:\ProgramData\burrow\burrow.yaml`, "--config", `C:\ProgramData\burrow\config.yaml`, "--log", "json"}
	if !reflect.DeepEqual(p.Arguments, wantArgs) {
		t.Fatalf("arguments %q", p.Arguments)
	}
	wantCopies := []Copy{
		{From: `C:\Users\kohn\AppData\Roaming\burrow\burrow.yaml`, To: `C:\ProgramData\burrow\burrow.yaml`},
		{From: `C:\Users\kohn\AppData\Roaming\burrow\config.yaml`, To: `C:\ProgramData\burrow\config.yaml`},
	}
	if !reflect.DeepEqual(p.Copies, wantCopies) {
		t.Fatalf("copies %#v", p.Copies)
	}
	if p.WorkingDir != p.ConfigDir || p.LogFile != `C:\ProgramData\burrow\burrow.log` {
		t.Fatalf("working dir %q, log %q", p.WorkingDir, p.LogFile)
	}
	for _, a := range p.Arguments {
		if strings.Contains(a, "Users") {
			t.Fatalf("argument %q points into the user's profile", a)
		}
	}
}

// LocalSystem never runs a binary a normal user can replace.
func TestBuildPlan_WindowsUntrustedBinary(t *testing.T) {
	in := windowsInputs()
	in.Executable, in.ExecutableTrusted = `C:\Users\kohn\AppData\Local\burrow\burrow.exe`, false
	_, err := BuildPlan(in)
	var ue *UntrustedExecutableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v", err)
	}
	for _, w := range []string{"LocalSystem", in.Executable, `%ProgramFiles%\burrow`, "Nothing was installed"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("message does not contain %q: %s", w, err)
		}
	}
}

func TestBuildPlan_WindowsNeedsProgramData(t *testing.T) {
	in := windowsInputs()
	in.ProgramData = ""
	if _, err := BuildPlan(in); err == nil || !strings.Contains(err.Error(), "ProgramData") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildPlan_Refusals(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Inputs)
		is   error
		want string
	}{
		{"no burrow.yaml", func(in *Inputs) { in.YAMLExists = false }, ErrNoYAML,
			"burrow service needs a burrow.yaml. Create one (see: burrow up) or pass its path."},
		{"no path at all", func(in *Inputs) { in.YAMLPath = "" }, ErrNoYAML, ""},
		{"not signed in", func(in *Inputs) { in.SignedIn = false }, ErrNotSignedIn, ""},
		{"already installed", func(in *Inputs) { in.AlreadyInstalled = true }, ErrAlreadyInstalled,
			"The burrow service is already installed. Run: burrow service uninstall"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := linuxInputs()
			tc.edit(&in)
			_, err := BuildPlan(in)
			if !errors.Is(err, tc.is) {
				t.Fatalf("err = %v", err)
			}
			if tc.want != "" && err.Error() != tc.want {
				t.Fatalf("message %q", err)
			}
		})
	}
}

func TestBuildPlan_NotElevated(t *testing.T) {
	in := linuxInputs()
	in.Elevated = false
	_, err := BuildPlan(in)
	var ne *NotElevatedError
	if !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	want := "Installing the burrow service needs root. Nothing was changed. Run:\n  sudo /usr/local/bin/burrow service install"
	if err.Error() != want {
		t.Fatalf("message %q", err)
	}

	in = windowsInputs()
	in.Elevated = false
	_, err = BuildPlan(in)
	if !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	for _, w := range []string{"administrator", "elevated terminal", in.Command, "Nothing was changed"} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("message does not contain %q: %s", w, err)
		}
	}
	if strings.Contains(err.Error(), "sudo") {
		t.Fatalf("sudo on Windows: %s", err)
	}

	// macOS installs for the current user and needs no elevation.
	in = linuxInputs()
	in.GOOS, in.Elevated = "darwin", false
	if _, err := BuildPlan(in); err != nil {
		t.Fatal(err)
	}
}

// What is missing is said before elevation is asked for: nobody is sent to
// sudo for an install that cannot work.
func TestBuildPlan_Order(t *testing.T) {
	in := linuxInputs()
	in.Elevated, in.YAMLExists, in.SignedIn, in.AlreadyInstalled = false, false, false, true
	if _, err := BuildPlan(in); !errors.Is(err, ErrNoYAML) {
		t.Fatalf("err = %v", err)
	}
	in.YAMLExists = true
	if _, err := BuildPlan(in); !errors.Is(err, ErrNotSignedIn) {
		t.Fatalf("err = %v", err)
	}
	in.SignedIn = true
	var ne *NotElevatedError
	if _, err := BuildPlan(in); !errors.As(err, &ne) {
		t.Fatalf("err = %v", err)
	}
	in.Elevated = true
	if _, err := BuildPlan(in); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildPlan_RelativePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("paths of this test are POSIX")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	in := linuxInputs()
	in.GOOS = runtime.GOOS
	in.Elevated, in.ExecutableTrusted = true, true
	in.Executable, in.YAMLPath, in.UserConfigPath = "bin/burrow", "burrow.yaml", "cfg/config.yaml"
	in.ExtraArguments = []string{"--insecure"}
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Executable != filepath.Join(cwd, "bin/burrow") || p.WorkingDir != cwd {
		t.Fatalf("executable %q, working dir %q", p.Executable, p.WorkingDir)
	}
	want := []string{"up", "--file", filepath.Join(cwd, "burrow.yaml"), "--config", filepath.Join(cwd, "cfg/config.yaml"), "--insecure", "--log", "json"}
	if !reflect.DeepEqual(p.Arguments, want) {
		t.Fatalf("arguments %q", p.Arguments)
	}
}

func TestBuildPlan_UnknownSystem(t *testing.T) {
	in := linuxInputs()
	in.GOOS = "plan9"
	if _, err := BuildPlan(in); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Fatalf("err = %v", err)
	}
}

// The token is no input of BuildPlan and so cannot be in a Plan; this pins
// that nobody adds it as one, and that the arguments name files only.
func TestBuildPlan_NoTokenAnywhere(t *testing.T) {
	if _, ok := reflect.TypeOf(Inputs{}).FieldByNameFunc(func(n string) bool {
		return strings.Contains(strings.ToLower(n), "token")
	}); ok {
		t.Fatal("Inputs has a token field")
	}
	for _, in := range []Inputs{linuxInputs(), windowsInputs()} {
		p, err := BuildPlan(in)
		if err != nil {
			t.Fatal(err)
		}
		text := fmt.Sprintf("%+v %#v", p, p)
		if strings.Contains(text, fakeToken) || strings.Contains(text, "bur_") || strings.Contains(strings.ToLower(text), "--token") {
			t.Fatalf("plan mentions a token: %s", text)
		}
	}
}

func TestElevationError(t *testing.T) {
	err := ElevationError("linux", "Stopping", "sudo burrow service stop")
	if err.Error() != "Stopping the burrow service needs root. Nothing was changed. Run:\n  sudo burrow service stop" {
		t.Fatalf("message %q", err)
	}
}

func TestUnderAny(t *testing.T) {
	roots := []string{`C:\Program Files`, "", `C:\Program Files (x86)`}
	for path, want := range map[string]bool{
		`C:\Program Files\burrow\burrow.exe`:         true,
		`c:\program files\Burrow\burrow.exe`:         true,
		`C:/Program Files/burrow/burrow.exe`:         true,
		`C:\Program Files (x86)\burrow\burrow.exe`:   true,
		`C:\Program Files Evil\burrow.exe`:           false,
		`C:\Program Files\..\Users\kohn\burrow.exe`:  false,
		`C:\Program Files\burrow\..\..\x\burrow.exe`: false,
		`C:\Users\kohn\AppData\Local\burrow.exe`:     false,
		`C:\Program Files`:                           false,
		``:                                           false,
	} {
		if got := UnderAny(path, roots); got != want {
			t.Errorf("UnderAny(%q) = %v, want %v", path, got, want)
		}
	}
	if UnderAny(`C:\Program Files\burrow.exe`, nil) {
		t.Error("no roots, yet trusted")
	}
}

// A launchd agent belongs to one user: installed through sudo it would be
// root's, and never start for the person who asked.
func TestBuildPlan_DarwinRefusesRoot(t *testing.T) {
	in := linuxInputs()
	in.GOOS, in.Elevated = "darwin", true
	_, err := BuildPlan(in)
	if err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildPlan_DarwinLogs(t *testing.T) {
	in := linuxInputs()
	in.GOOS, in.Elevated, in.HomeDir = "darwin", false, "/Users/kohn"
	p, err := BuildPlan(in)
	if err != nil {
		t.Fatal(err)
	}
	if p.LogDir != "/Users/kohn/Library/Logs/burrow" || p.LogFile != "/Users/kohn/Library/Logs/burrow/burrow.err.log" {
		t.Fatalf("log dir %q, log file %q", p.LogDir, p.LogFile)
	}
}

// A systemd unit expands % and $ and reads \ and " in a command line; the
// library that writes the unit escapes none of them. Such a path is refused
// rather than written into a unit that then starts something else.
func TestBuildPlan_PathsAUnitCannotCarry(t *testing.T) {
	for _, bad := range []string{"/home/kohn/%h/burrow.yaml", "/home/kohn/$HOME.yaml", `/home/kohn/a\b.yaml`, `/home/kohn/a".yaml`, "/home/kohn/a\nUser=root\n.yaml", "/home/kohn/a\t.yaml"} {
		for _, set := range []func(*Inputs){
			func(in *Inputs) { in.YAMLPath = bad },
			func(in *Inputs) { in.UserConfigPath = bad },
			func(in *Inputs) { in.Executable = bad },
			func(in *Inputs) { in.ExtraArguments = []string{"--cacert", bad} },
		} {
			in := linuxInputs()
			set(&in)
			_, err := BuildPlan(in)
			var pe *PathError
			if !errors.As(err, &pe) {
				t.Fatalf("%q: err = %v", bad, err)
			}
			// The path is shown quoted: no line break of it reaches the terminal.
			if strings.ContainsAny(err.Error(), "\n\t") || !strings.Contains(err.Error(), "Nothing was installed") {
				t.Fatalf("message %q", err)
			}
		}
	}
	// Elsewhere only control characters are refused.
	in := windowsInputs()
	in.YAMLPath = `C:\Users\kohn\100%\burrow.yaml`
	if _, err := BuildPlan(in); err != nil {
		t.Fatal(err)
	}
	in.YAMLPath = "C:\\Users\\kohn\\a\nb.yaml"
	if _, err := BuildPlan(in); err == nil {
		t.Fatal("a path with a line break was accepted")
	}
}

func TestBuildPlan_UserNameAUnitCannotCarry(t *testing.T) {
	for _, bad := range []string{"kohn\nExecStartPre=/bin/x", "ko hn", "-kohn", "k%u", "kohn;"} {
		in := linuxInputs()
		in.UserName = bad
		if _, err := BuildPlan(in); err == nil {
			t.Fatalf("user %q was accepted", bad)
		}
	}
	for _, ok := range []string{"kohn", "a.koehn", "svc_burrow-1", "DOMAIN.user@example"} {
		in := linuxInputs()
		in.UserName = ok
		if _, err := BuildPlan(in); err != nil {
			t.Fatalf("user %q: %v", ok, err)
		}
	}
}
