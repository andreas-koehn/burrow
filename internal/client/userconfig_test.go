package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testToken is a made-up value. Tests never print it: failure messages say that
// it leaked, not what it is.
const testToken = "bur_test0123456789abcdefTAIL"

func sampleUserConfig() UserConfig {
	return UserConfig{
		Relay:     "https://burrow.example.com",
		Control:   "burrow.example.com:7000",
		Token:     testToken,
		TokenName: "kohns-laptop",
	}
}

// isolateUserConfigDir points every variable os.UserConfigDir reads at a temp
// directory, so no test can touch the real user config of this machine.
func isolateUserConfigDir(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "xdg"))
	t.Setenv("AppData", filepath.Join(base, "AppData"))
	return base
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestUserConfigPath_Default(t *testing.T) {
	base := isolateUserConfigDir(t)

	got, err := UserConfigPath("")
	if err != nil {
		t.Fatalf("UserConfigPath: %v", err)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	if want := filepath.Join(dir, "burrow", "config.yaml"); got != want {
		t.Errorf("UserConfigPath(\"\") = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, base) {
		t.Errorf("UserConfigPath(\"\") = %q, want it under the temp dir %q", got, base)
	}
}

func TestUserConfigPath_Override(t *testing.T) {
	isolateUserConfigDir(t)

	got, err := UserConfigPath("/x/y.yaml")
	if err != nil {
		t.Fatalf("UserConfigPath: %v", err)
	}
	if got != "/x/y.yaml" {
		t.Errorf("UserConfigPath(override) = %q, want it unchanged", got)
	}
}

func TestUserConfigPath_NoConfigDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the variables os.UserConfigDir needs differ per platform")
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	if _, err := UserConfigPath(""); err == nil {
		t.Fatal("UserConfigPath(\"\") without a config directory: want an error")
	}
	if got, err := UserConfigPath("/x/y.yaml"); err != nil || got != "/x/y.yaml" {
		t.Errorf("an override must not need a config directory: got %q, %v", got, err)
	}
}

func TestSaveUserConfig_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	want := sampleUserConfig()

	if err := SaveUserConfig(path, want); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	got, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Relay != want.Relay || got.Control != want.Control || got.TokenName != want.TokenName {
		t.Errorf("round trip changed relay, control or token name: got %v", got)
	}
	if got.Token != want.Token {
		t.Error("round trip changed the token")
	}
}

func TestSaveUserConfig_FileFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveUserConfig(path, sampleUserConfig()); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, key := range []string{"relay:", "control:", "token:", "token_name:"} {
		if !bytes.Contains(raw, []byte(key)) {
			t.Errorf("the saved file has no %q key", key)
		}
	}
}

func TestSaveUserConfig_CreatesDirectoriesAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "burrow")
	path := filepath.Join(dir, "config.yaml")

	if err := SaveUserConfig(path, sampleUserConfig()); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the file was not created: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode = %o, want 600", got)
	}
	if got := modeOf(t, dir); got != 0o700 {
		t.Errorf("directory mode = %o, want 700", got)
	}
}

func TestSaveUserConfig_NarrowsAnExistingWideFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("relay: https://old.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := SaveUserConfig(path, sampleUserConfig()); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	if got := modeOf(t, path); got != 0o600 {
		t.Errorf("file mode after saving over a 0644 file = %o, want 600", got)
	}
	got, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Relay != "https://burrow.example.com" {
		t.Errorf("relay = %q, want the newly saved one", got.Relay)
	}
}

func TestSaveUserConfig_LeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	for i := 0; i < 2; i++ { // the second save replaces an existing file
		if err := SaveUserConfig(path, sampleUserConfig()); err != nil {
			t.Fatalf("SaveUserConfig: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "config.yaml" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only config.yaml", names)
	}
}

func TestSaveUserConfig_FailureKeepsThePreviousFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not block writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	const previous = "relay: https://old.example\ncontrol: old.example:7000\n"
	if err := os.WriteFile(path, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	err := SaveUserConfig(path, sampleUserConfig())
	if err == nil {
		t.Fatal("SaveUserConfig into a read-only directory: want an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Error("the error contains the token")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read: %v", readErr)
	}
	if string(raw) != previous {
		t.Error("a failed save changed the previous file")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("a failed save left %d entries in the directory, want 1", len(entries))
	}
}

func TestLoadUserConfig_Missing(t *testing.T) {
	_, err := LoadUserConfig(filepath.Join(t.TempDir(), "config.yaml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want one that is os.ErrNotExist", err)
	}
}

func TestLoadUserConfig_NotYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cases := map[string]string{
		"broken syntax": "relay: [unclosed\ntoken: " + testToken + "\n",
		"wrong shape":   testToken + "\n",
		"wrong type":    "token:\n  - " + testToken + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadUserConfig(path)
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Error("a broken file must not look like a missing one")
			}
			if !strings.Contains(err.Error(), path) {
				t.Error("the error does not name the path")
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "unclosed") {
				t.Error("the error prints content of the file")
			}
		})
	}
}

func TestLoadUserConfig_IgnoresUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "relay: https://r.example\ncontrol: r.example:7000\ntoken_name: laptop\nadded_later: true\nnested:\n  a: 1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadUserConfig(path)
	if err != nil {
		t.Fatalf("LoadUserConfig: %v", err)
	}
	if got.Relay != "https://r.example" || got.Control != "r.example:7000" || got.TokenName != "laptop" {
		t.Errorf("got %v", got)
	}
}

func TestRemoveUserConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveUserConfig(path, sampleUserConfig()); err != nil {
		t.Fatalf("SaveUserConfig: %v", err)
	}
	if err := RemoveUserConfig(path); err != nil {
		t.Fatalf("RemoveUserConfig: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there: %v", err)
	}
	if err := RemoveUserConfig(path); err != nil {
		t.Errorf("RemoveUserConfig of a missing file = %v, want nil", err)
	}
}

func TestFileModeTooWide(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("always false on Windows")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("relay: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		mode os.FileMode
		want bool
	}{{0o600, false}, {0o400, false}, {0o644, true}, {0o640, true}, {0o604, true}} {
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		got, err := FileModeTooWide(path)
		if err != nil {
			t.Fatalf("FileModeTooWide(%o): %v", tc.mode, err)
		}
		if got != tc.want {
			t.Errorf("FileModeTooWide(%o) = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestFileModeTooWide_Missing(t *testing.T) {
	if _, err := FileModeTooWide(filepath.Join(t.TempDir(), "config.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("err = %v, want one that is os.ErrNotExist", err)
	}
}

// assertNoToken renders v every way a careless caller might and fails when the
// token shows up. It reports the verb only.
func assertNoToken(t *testing.T, what string, v any) {
	t.Helper()
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		if strings.Contains(fmt.Sprintf(verb, v), testToken) {
			t.Errorf("%s formatted with %s contains the token", what, verb)
		}
	}
	var text, js bytes.Buffer
	slog.New(slog.NewTextHandler(&text, nil)).Info("x", "v", v)
	slog.New(slog.NewJSONHandler(&js, nil)).Info("x", "v", v)
	if strings.Contains(text.String(), testToken) || strings.Contains(js.String(), testToken) {
		t.Errorf("%s logged through slog contains the token", what)
	}
	if raw, err := json.Marshal(v); err == nil && bytes.Contains(raw, []byte(testToken)) {
		t.Errorf("%s marshalled as JSON contains the token", what)
	}
}

func TestUserConfig_NeverPrintsTheToken(t *testing.T) {
	c := sampleUserConfig()
	assertNoToken(t, "UserConfig", c)
	assertNoToken(t, "*UserConfig", &c)

	// What is safe to show stays visible, and whether a token is there at all.
	s := fmt.Sprintf("%v", c)
	for _, want := range []string{"burrow.example.com:7000", "kohns-laptop", "[redacted]"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, want it to contain %q", s, want)
		}
	}
	if s := fmt.Sprintf("%v", UserConfig{}); !strings.Contains(s, "[none]") {
		t.Errorf("String() of an empty config = %q, want it to say there is no token", s)
	}
}
