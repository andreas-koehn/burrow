// Package svc decides what `burrow service install` would install. It touches
// nothing: the command gathers the facts, this package turns them into a Plan
// or into the reason why nothing is installed.
//
// The token of the sign-in is no input and so is in no Plan: the service
// program is `burrow up` with the paths of two files, and reads the token from
// the user config like every other run.
package svc

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// Name is the name of the service on every system.
const Name = "burrow"

// Inputs are the facts a Plan is built from.
type Inputs struct {
	GOOS string
	// Executable is the running binary, symbolic links resolved.
	Executable string
	// YAMLPath is the burrow.yaml to serve, UserConfigPath the stored sign-in.
	YAMLPath, UserConfigPath string
	// UserName is the user who installs (SUDO_USER when run through sudo).
	// Linux only: the service runs as this user. "" and "root" mean root.
	UserName string
	// ProgramData is %ProgramData%. Windows only.
	ProgramData string
	// Elevated says whether the command runs as root or as administrator.
	Elevated bool
	// YAMLExists, SignedIn and AlreadyInstalled say what was found.
	YAMLExists, SignedIn, AlreadyInstalled bool
	// ExecutableTrusted says that only root or an administrator can change
	// the binary or a directory above it. It matters where the service has
	// more rights than the installing user: as root or as LocalSystem.
	ExecutableTrusted bool
	// HomeDir is the home directory of the current user. macOS only: the
	// agent's log goes below it.
	HomeDir string
	// ExtraArguments are flags of the install to repeat on every start
	// (--cacert and its like), each value its own element. Never a token.
	ExtraArguments []string
	// Command is the exact line to run again with more rights.
	Command string
}

// Copy is one file to copy at install.
type Copy struct{ From, To string }

// Plan is what would be installed.
type Plan struct {
	Name        string // "burrow"
	DisplayName string
	Description string
	Executable  string   // absolute path
	Arguments   []string // up --file <yaml> --config <user config> [...] --log json
	WorkingDir  string
	UserName    string // Linux: the user the unit names with User=; "" is root
	UserScope   bool   // macOS: a launchd agent of the current user
	// ConfigDir, Copies and LogFile are set on Windows only: the directory
	// under %ProgramData% that only SYSTEM and Administrators can read, the
	// files copied into it, and where the service writes its log lines.
	ConfigDir string
	Copies    []Copy
	// LogFile is where the log lines go on Windows and macOS (on Linux they
	// go to the journal); LogDir is its directory on macOS, created at install.
	LogFile string
	LogDir  string
	// RunsAs names the account for the person who installs.
	RunsAs string
}

// The reasons why nothing is installed. Their texts are the messages.
var (
	ErrNoYAML           = errors.New("burrow service needs a burrow.yaml. Create one (see: burrow up) or pass its path.")
	ErrNotSignedIn      = errors.New("not signed in")
	ErrAlreadyInstalled = errors.New("The burrow service is already installed. Run: burrow service uninstall")
)

// NotElevatedError says that the action needs root or an administrator, and
// which command to run instead.
type NotElevatedError struct{ GOOS, Action, Command string }

func (e *NotElevatedError) Error() string {
	if e.GOOS == "windows" {
		return e.Action + " the burrow service needs administrator rights. Nothing was changed.\n" +
			"Run this from an elevated terminal (Run as administrator):\n  " + e.Command
	}
	return e.Action + " the burrow service needs root. Nothing was changed. Run:\n  " + e.Command
}

// ElevationError is the error of an action other than the install that needs
// more rights. action starts the sentence: "Stopping", "Removing".
func ElevationError(goos, action, command string) error {
	return &NotElevatedError{GOOS: goos, Action: action, Command: command}
}

// UntrustedExecutableError says that the service would run with more rights
// than the people who can replace its binary.
type UntrustedExecutableError struct{ GOOS, Executable string }

func (e *UntrustedExecutableError) Error() string {
	if e.GOOS == "windows" {
		return "The service runs as LocalSystem, but " + e.Executable + " is in a place a normal user can change: " +
			"whoever replaces that file would run as LocalSystem. Nothing was installed.\n" +
			`Copy burrow.exe to %ProgramFiles%\burrow from an elevated terminal and run the install with that copy.`
	}
	return "The service would run as root, but " + e.Executable + " or a directory above it can be changed by another user: " +
		"whoever replaces that file would run as root. Nothing was installed.\n" +
		"Put burrow in /usr/local/bin (the installer does that with --system) and install with that copy, " +
		"or run the install through sudo from your own account: the service then runs as you."
}

// PathError says that a path or a name cannot be written into the service
// definition of this system.
type PathError struct{ GOOS, Value string }

func (e *PathError) Error() string {
	if e.GOOS == "linux" {
		return fmt.Sprintf("%q has a character that a systemd unit cannot carry (one of %% $ \\ \" or a control character). "+
			"Nothing was installed. Use a path without it.", e.Value)
	}
	return fmt.Sprintf("%q has a control character. Nothing was installed. Use a path without it.", e.Value)
}

// carried reports whether s can stand in the service definition of goos as it
// is. systemd expands % and $ in a command line and reads \ and " in it, and
// what writes the unit escapes none of them.
func carried(goos, s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return goos != "linux" || !strings.ContainsAny(s, "%$\\\"")
}

// userName reports whether s can follow User= in a unit.
func userName(s string) bool {
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' ||
			i > 0 && (r == '-' || r == '.' || r == '@')
		if !ok {
			return false
		}
	}
	return true
}

// BuildPlan decides what would be installed; it touches nothing.
//
// What is missing comes first, then the rights, then what is in the way: nobody
// is sent to sudo for an install that cannot work.
func BuildPlan(in Inputs) (Plan, error) {
	switch in.GOOS {
	case "linux", "darwin", "windows":
	default:
		return Plan{}, fmt.Errorf("burrow service is not available on %s", in.GOOS)
	}
	if in.YAMLPath == "" || !in.YAMLExists {
		return Plan{}, ErrNoYAML
	}
	if !in.SignedIn {
		return Plan{}, ErrNotSignedIn
	}
	if in.GOOS == "darwin" && in.Elevated {
		return Plan{}, errors.New("On macOS the service is an agent of your own user. Run this without sudo. Nothing was installed.")
	}
	if in.GOOS != "darwin" && !in.Elevated {
		return Plan{}, &NotElevatedError{GOOS: in.GOOS, Action: "Installing", Command: in.Command}
	}
	if in.AlreadyInstalled {
		return Plan{}, ErrAlreadyInstalled
	}

	// Paths of another system than the one this runs on are left as they are.
	abs := func(p string) (string, error) {
		if in.GOOS != runtime.GOOS {
			return p, nil
		}
		return filepath.Abs(p)
	}
	exe, err := abs(in.Executable)
	if err != nil {
		return Plan{}, err
	}
	yaml, err := abs(in.YAMLPath)
	if err != nil {
		return Plan{}, err
	}
	cfg, err := abs(in.UserConfigPath)
	if err != nil {
		return Plan{}, err
	}

	for _, v := range append([]string{exe, yaml, cfg}, in.ExtraArguments...) {
		if !carried(in.GOOS, v) {
			return Plan{}, &PathError{GOOS: in.GOOS, Value: v}
		}
	}
	if in.GOOS == "linux" && !userName(in.UserName) {
		return Plan{}, &PathError{GOOS: in.GOOS, Value: in.UserName}
	}

	p := Plan{
		Name:        Name,
		DisplayName: "Burrow client",
		Description: "Keeps the services of burrow.yaml connected to the relay (burrow up).",
		Executable:  exe,
		WorkingDir:  dirOf(in.GOOS, yaml),
	}
	privileged := false
	switch in.GOOS {
	case "linux":
		if in.UserName == "" || in.UserName == "root" {
			privileged, p.RunsAs = true, "root"
		} else {
			p.UserName, p.RunsAs = in.UserName, in.UserName
		}
	case "darwin":
		p.UserScope, p.RunsAs = true, "the current user"
		if in.HomeDir != "" {
			p.LogDir = strings.TrimRight(in.HomeDir, "/") + "/Library/Logs/" + Name
			p.LogFile = p.LogDir + "/burrow.err.log"
		}
	case "windows":
		if strings.TrimSpace(in.ProgramData) == "" {
			return Plan{}, errors.New("the ProgramData directory of this system is not known (%ProgramData% is empty)")
		}
		privileged, p.RunsAs = true, "LocalSystem"
		p.ConfigDir = strings.TrimRight(in.ProgramData, `\/`) + `\` + Name
		p.WorkingDir = p.ConfigDir
		p.LogFile = p.ConfigDir + `\burrow.log`
		p.Copies = []Copy{
			{From: yaml, To: p.ConfigDir + `\burrow.yaml`},
			{From: cfg, To: p.ConfigDir + `\config.yaml`},
		}
		yaml, cfg = p.Copies[0].To, p.Copies[1].To
	}
	if privileged && !in.ExecutableTrusted {
		return Plan{}, &UntrustedExecutableError{GOOS: in.GOOS, Executable: exe}
	}

	p.Arguments = append([]string{"up", "--file", yaml, "--config", cfg}, in.ExtraArguments...)
	// A service has no terminal: log lines, and in a form a journal can index.
	p.Arguments = append(p.Arguments, "--log", "json")
	return p, nil
}

// dirOf is the directory of path by the rules of goos.
func dirOf(goos, path string) string {
	if goos == runtime.GOOS {
		return filepath.Dir(path)
	}
	if i := strings.LastIndexAny(path, `\/`); i > 0 {
		return path[:i]
	}
	return path
}

// UnderAny reports whether the Windows path is inside one of the directories
// roots. Case and the direction of the slashes do not matter; a path with a
// ".." element is never inside. Empty roots are skipped.
func UnderAny(path string, roots []string) bool {
	norm := func(s string) string {
		return strings.TrimRight(strings.ToLower(strings.ReplaceAll(s, "/", `\`)), `\`)
	}
	p := norm(path)
	for _, el := range strings.Split(p, `\`) {
		if el == ".." {
			return false
		}
	}
	for _, r := range roots {
		if r = norm(r); r != "" && strings.HasPrefix(p, r+`\`) {
			return true
		}
	}
	return false
}
