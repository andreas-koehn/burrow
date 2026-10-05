package client

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestResolve_Precedence(t *testing.T) {
	user := &UserConfig{Relay: "https://r.example", Control: "r.example:7000", Token: "bur_user", TokenName: "laptop"}
	cases := []struct {
		name string
		s    Sources
		want Credentials
	}{
		{"user config only", Sources{User: user},
			Credentials{Control: "r.example:7000", Token: "bur_user", TokenName: "laptop", Relay: "https://r.example", Source: "user config"}},
		{"file overrides user config", Sources{FileServer: "f.example:7000", FileToken: "bur_file", User: user},
			Credentials{Control: "f.example:7000", Token: "bur_file", Source: "burrow.yaml"}},
		{"environment overrides file", Sources{EnvServer: "e.example:7000", EnvToken: "bur_env", FileServer: "f:7000", FileToken: "bur_file", User: user},
			Credentials{Control: "e.example:7000", Token: "bur_env", Source: "environment"}},
		{"flags override everything", Sources{FlagServer: "x.example:7000", FlagToken: "bur_flag", EnvServer: "e:7000", EnvToken: "bur_env", User: user},
			Credentials{Control: "x.example:7000", Token: "bur_flag", Source: "flags"}},
		// The two values are resolved independently: a flag for the server with the stored token is a normal case.
		{"flag server, stored token", Sources{FlagServer: "x.example:7000", User: user},
			Credentials{Control: "x.example:7000", Token: "bur_user", TokenName: "laptop", Relay: "", Source: "flags"}},
		{"env token, stored server", Sources{EnvToken: "bur_env", User: user},
			Credentials{Control: "r.example:7000", Token: "bur_env", Relay: "https://r.example", Source: "environment"}},
		{"file token, stored server", Sources{FileToken: "bur_file", User: user},
			Credentials{Control: "r.example:7000", Token: "bur_file", Relay: "https://r.example", Source: "burrow.yaml"}},
		{"flag token, env server", Sources{FlagToken: "bur_flag", EnvServer: "e.example:7000", User: user},
			Credentials{Control: "e.example:7000", Token: "bur_flag", Source: "flags"}},
		{"no user config at all", Sources{EnvServer: "e.example:7000", FileToken: "bur_file"},
			Credentials{Control: "e.example:7000", Token: "bur_file", Source: "environment"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.s)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			// Field by field, so a failure never prints a token.
			if got.Control != tc.want.Control {
				t.Errorf("Control = %q, want %q", got.Control, tc.want.Control)
			}
			if got.Token != tc.want.Token {
				t.Error("Token came from the wrong source")
			}
			if got.TokenName != tc.want.TokenName {
				t.Errorf("TokenName = %q, want %q", got.TokenName, tc.want.TokenName)
			}
			if got.Relay != tc.want.Relay {
				t.Errorf("Relay = %q, want %q", got.Relay, tc.want.Relay)
			}
			if got.Source != tc.want.Source {
				t.Errorf("Source = %q, want %q", got.Source, tc.want.Source)
			}
		})
	}
}

func TestResolve_NotSignedIn(t *testing.T) {
	for i, s := range []Sources{
		{},
		{FlagServer: "x:7000"},
		{EnvToken: "t"},
		{FileServer: "x:7000"},
		{User: &UserConfig{}},
		{User: &UserConfig{Control: "x:7000"}},
		{User: &UserConfig{Token: "t"}},
	} {
		got, err := Resolve(s)
		if !errors.Is(err, ErrNotSignedIn) {
			t.Errorf("case %d: err = %v, want ErrNotSignedIn", i, err)
		}
		if got != (Credentials{}) {
			t.Errorf("case %d: want empty credentials next to the error", i)
		}
	}
	if ErrNotSignedIn.Error() != "Not signed in. Run: burrow login <your relay address>" {
		t.Fatal("the message is part of the interface")
	}
}

func TestTokenTail(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		{"bur_abcdefgh1234", "1234"},
		{"abc", ""},
		{"", ""},
		{"abcd", ""}, // the tail of a four-character token would be the whole token
		{"abcde", "bcde"},
	} {
		if got := TokenTail(tc.token); got != tc.want {
			t.Errorf("TokenTail(%d characters) = %q, want %q", len(tc.token), got, tc.want)
		}
	}
}

func TestCredentialsAndSources_NeverPrintTheToken(t *testing.T) {
	user := sampleUserConfig()
	s := Sources{FlagToken: testToken, EnvToken: testToken, FileToken: testToken, User: &user}
	assertNoToken(t, "Sources", s)
	assertNoToken(t, "*Sources", &s)

	c, err := Resolve(Sources{User: &user})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertNoToken(t, "Credentials", c)
	assertNoToken(t, "*Credentials", &c)
}

// Only UserConfig is ever written as YAML; the other types leave every token out.
func TestCredentialsAndSources_YAMLHasNoToken(t *testing.T) {
	user := sampleUserConfig()
	for name, v := range map[string]any{
		"Credentials": Credentials{Control: "c.example:7000", Token: testToken, TokenName: "laptop", Source: SourceUserConfig},
		"Sources":     Sources{FlagToken: testToken, EnvToken: testToken, FileToken: testToken, User: &user},
	} {
		out, err := yaml.Marshal(v)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), testToken) {
			t.Fatalf("%s: the YAML encoding contains the token", name)
		}
	}
	out, err := yaml.Marshal(Credentials{Control: "c.example:7000", Token: testToken})
	if err != nil || !strings.Contains(string(out), "c.example:7000") {
		t.Fatalf("the other fields are still encoded: %q, %v", out, err)
	}
}

// A value of spaces only is not a value: the next source is asked.
func TestResolve_BlankValuesCountAsNotSet(t *testing.T) {
	user := &UserConfig{Relay: "https://r.example", Control: "r.example:7000", Token: "bur_user", TokenName: "laptop"}
	c, err := Resolve(Sources{FlagServer: " ", FlagToken: "\t", EnvServer: "  ", EnvToken: " \n", FileServer: " ", FileToken: " ", User: user})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Control != "r.example:7000" || c.Token != "bur_user" || c.Source != SourceUserConfig || c.TokenName != "laptop" {
		t.Fatalf("control %q source %q name %q", c.Control, c.Source, c.TokenName)
	}

	for name, s := range map[string]Sources{
		"blank user token":   {User: &UserConfig{Control: "r.example:7000", Token: "  "}},
		"blank user control": {User: &UserConfig{Control: " ", Token: "bur_user"}},
		"blank env":          {EnvServer: " ", EnvToken: " "},
	} {
		if _, err := Resolve(s); !errors.Is(err, ErrNotSignedIn) {
			t.Fatalf("%s: err = %v, want ErrNotSignedIn", name, err)
		}
	}
}

func TestResolve_TrimsValues(t *testing.T) {
	c, err := Resolve(Sources{EnvServer: " e.example:7000\n", EnvToken: "  bur_env\r\n"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if c.Control != "e.example:7000" || c.Token != "bur_env" {
		t.Fatalf("control %q, token trimmed: %v", c.Control, c.Token == "bur_env")
	}
}
