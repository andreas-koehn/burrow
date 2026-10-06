package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"github.com/ankoehn/burrow/cmd/client/svc"
)

// renderUnit has the library write the systemd unit of a plan, into a home
// directory of the test and with no systemctl in reach: the file is written,
// and nothing is enabled or started.
func renderUnit(t *testing.T, plan svc.Plan) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no systemctl
	cfg := serviceConfig(plan, "linux")
	cfg.Option["UserService"] = true // the same text, below $HOME instead of /etc
	for _, sys := range service.AvailableSystems() {
		if sys.String() != "linux-systemd" {
			continue
		}
		s, err := sys.New(noProgram{}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Install(); err == nil {
			t.Fatal("a unit was enabled on the machine the tests run on")
		}
		unit, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "burrow.service"))
		if err != nil {
			t.Fatal(err)
		}
		return string(unit)
	}
	t.Fatal("the library has no systemd")
	return ""
}

// The unit the library writes from this client's plan, as a whole: it names
// two files and no token, restarts always, and runs as the installing user.
func TestSystemdUnit(t *testing.T) {
	h := newHarness(t)
	h.signIn() // the token exists on this machine, in the user config
	plan, err := svc.BuildPlan(svc.Inputs{
		GOOS: "linux", Executable: "/usr/local/bin/burrow",
		YAMLPath: "/home/kohn/my services/burrow.yaml", UserConfigPath: h.cfgPath,
		UserName: "kohn", Elevated: true, YAMLExists: true, SignedIn: true,
		ExtraArguments: []string{"--cacert", "/etc/burrow/ca.pem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unit := renderUnit(t, plan)
	want := `[Unit]
Description=Keeps the services of burrow.yaml connected to the relay (burrow up).
ConditionFileIsExecutable=/usr/local/bin/burrow
Wants=network-online.target
After=network-online.target

[Service]
StartLimitInterval=5
StartLimitBurst=10
ExecStart=/usr/local/bin/burrow "up" "--file" "/home/kohn/my services/burrow.yaml" "--config" "` + h.cfgPath + `" "--cacert" "/etc/burrow/ca.pem" "--log" "json"
WorkingDirectory=/home/kohn/my\x20services
User=kohn
Restart=always
RestartSec=120
EnvironmentFile=-/etc/sysconfig/burrow

[Install]
WantedBy=multi-user.target
`
	if unit != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", unit, want)
	}
	if strings.Contains(unit, testToken) || strings.Contains(unit, "bur_") || strings.Contains(strings.ToLower(unit), "token") ||
		strings.Contains(unit, "Environment=") {
		t.Fatal("the unit carries a token or an environment variable")
	}
}
