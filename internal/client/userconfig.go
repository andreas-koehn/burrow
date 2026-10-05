package client

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// UserConfig is the sign-in stored by `burrow login`.
//
// The token is a secret: String, GoString, LogValue and the JSON encoding all
// leave it out, so a config that ends up in a log line or an error does not
// carry it. Only SaveUserConfig writes it, to a file only its owner can read.
type UserConfig struct {
	Relay     string `yaml:"relay" json:"relay"`     // https://burrow.example.com
	Control   string `yaml:"control" json:"control"` // burrow.example.com:7000
	Token     string `yaml:"token" json:"-"`
	TokenName string `yaml:"token_name" json:"token_name"`
}

// redactToken says whether a token is there without showing any of it.
func redactToken(token string) string {
	if token == "" {
		return "[none]"
	}
	return "[redacted]"
}

// String renders the config without the token.
func (c UserConfig) String() string {
	return fmt.Sprintf("UserConfig{Relay:%q Control:%q Token:%s TokenName:%q}",
		c.Relay, c.Control, redactToken(c.Token), c.TokenName)
}

// GoString keeps %#v from printing the token.
func (c UserConfig) GoString() string { return c.String() }

// LogValue keeps slog from printing the token.
func (c UserConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("relay", c.Relay),
		slog.String("control", c.Control),
		slog.String("token", redactToken(c.Token)),
		slog.String("token_name", c.TokenName),
	)
}

// UserConfigPath returns override (the --config flag) when it is set, and
// otherwise config.yaml in the burrow directory under os.UserConfigDir().
func UserConfigPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user config directory: %w", err)
	}
	return filepath.Join(dir, "burrow", "config.yaml"), nil
}

// LoadUserConfig reads the user config. A missing file gives an error that is
// os.ErrNotExist. Unknown keys are ignored: a newer client may have written them.
func LoadUserConfig(path string) (UserConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return UserConfig{}, fmt.Errorf("read user config: %w", err)
	}
	var c UserConfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		// The YAML library quotes the offending value in its message, and that
		// value may be the token, so its error is not passed on.
		return UserConfig{}, fmt.Errorf("user config %s is not valid; run `burrow login` again to rewrite it", path)
	}
	return c, nil
}

// SaveUserConfig writes the user config so that only its owner can read it.
//
// The directory is created with mode 0700. The content goes to a new file in
// the same directory, created with mode 0600, which then replaces the target in
// one rename: a reader sees the old file or the new one, never a partial one,
// and a previous file with wider permissions is replaced by the 0600 one.
func SaveUserConfig(path string, c UserConfig) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return errors.New("encode user config") // the library's error is not passed on, see LoadUserConfig
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create user config directory: %w", err)
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("write user config: %w", err)
	}
	tmp := filepath.Join(dir, "."+filepath.Base(path)+"."+hex.EncodeToString(suffix[:])+".tmp")

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("write user config: %w", err)
	}
	if err := writeSyncClose(f, data); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write user config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write user config: %w", err)
	}
	return nil
}

// writeSyncClose writes data to f, flushes it to disk and closes f. f is closed
// on every path.
func writeSyncClose(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// RemoveUserConfig deletes the user config. It returns nil when there was none.
func RemoveUserConfig(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove user config: %w", err)
	}
	return nil
}

// FileModeTooWide reports whether group or others can access the file. It is
// always false on Windows, which has no permission bits.
func FileModeTooWide(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if runtime.GOOS == "windows" {
		return false, nil
	}
	return fi.Mode().Perm()&0o077 != 0, nil
}
