package client

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileConfig is the parsed result of a burrow.yaml file. Server and Token are
// "" when the file does not set them.
type FileConfig struct {
	Server  string
	Token   string
	Tunnels []TunnelSpec
}

// rawFileConfig is the intermediate struct used for YAML unmarshalling.
type rawFileConfig struct {
	Server    string       `yaml:"server"`
	Token     string       `yaml:"token"`
	TokenFile string       `yaml:"token_file"`
	Services  []rawService `yaml:"services"`
}

// rawService is one entry under "services:" in burrow.yaml.
type rawService struct {
	Name       string `yaml:"name"`
	Local      string `yaml:"local"`
	Type       string `yaml:"type"`
	RemotePort int    `yaml:"remote"`
	// Slug and Access are read by `burrow up` only (see loadFileConfig).
	Slug   string `yaml:"slug"`
	Access string `yaml:"access"`
}

// LoadFileConfig reads and validates a burrow.yaml file at path for
// `burrow up`. It returns a FileConfig with Tunnels populated, and Server and
// Token when the file has them: both are optional, a blank value counts as
// absent, and what is absent comes from the sign-in.
func LoadFileConfig(path string) (FileConfig, error) { return loadFileConfig(path, false) }

// LoadCompleteFileConfig is the loader of `burrow connect --config`. server and
// one of token / token_file are required, and every check, message and value is
// the one that command has always had.
func LoadCompleteFileConfig(path string) (FileConfig, error) { return loadFileConfig(path, true) }

// TokenFilePath returns what the burrow.yaml at path names as token_file,
// "" when it names none. The file itself is not read.
func TokenFilePath(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("loadfileconfig: read %s: %w", path, err)
	}
	var raw struct {
		TokenFile string `yaml:"token_file"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("loadfileconfig: parse %s: %w", path, err)
	}
	return raw.TokenFile, nil
}

func loadFileConfig(path string, complete bool) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("loadfileconfig: read %s: %w", path, err)
	}

	var raw rawFileConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return FileConfig{}, fmt.Errorf("loadfileconfig: parse %s: %w", path, err)
	}

	server, token := raw.Server, raw.Token
	if complete {
		// Validate server non-empty.
		if server == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: server is required")
		}
		// Validate exactly one of token / token_file.
		if token == "" && raw.TokenFile == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: exactly one of token or token_file is required")
		}
	} else {
		server, token = strings.TrimSpace(server), strings.TrimSpace(token)
	}
	if token != "" && raw.TokenFile != "" {
		return FileConfig{}, fmt.Errorf("loadfileconfig: only one of token or token_file may be set")
	}

	// Resolve token_file.
	if raw.TokenFile != "" {
		b, err := os.ReadFile(raw.TokenFile)
		if err != nil {
			return FileConfig{}, fmt.Errorf("loadfileconfig: token_file %q: %w", raw.TokenFile, err)
		}
		if complete {
			// Trim trailing newlines (same convention as internal/config applyFileSecrets).
			token = strings.TrimRight(string(b), "\r\n")
		} else {
			token = strings.TrimSpace(string(b))
		}
		// What the file yields is sent to the relay. A file that cannot hold
		// a token (several lines, a space inside, other than printable ASCII)
		// is some other file: it is not sent anywhere, and nothing of it is
		// shown. An empty one is no token, as it always was.
		if t := strings.TrimSpace(token); t != "" && !ValidToken(t) {
			return FileConfig{}, fmt.Errorf("loadfileconfig: token_file %q: the file does not hold a token", raw.TokenFile)
		}
	}

	// Validate at least one service.
	if len(raw.Services) == 0 {
		return FileConfig{}, fmt.Errorf("loadfileconfig: at least one service is required")
	}

	// Build TunnelSpec slice, applying defaults and validations.
	tunnels := make([]TunnelSpec, 0, len(raw.Services))
	for i, svc := range raw.Services {
		if svc.Name == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d]: name is required", i)
		}
		if svc.Local == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: local is required", i, svc.Name)
		}
		// Default type to tcp.
		typ := svc.Type
		if typ == "" {
			typ = "tcp"
		}
		if typ != "tcp" && typ != "http" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: unknown type %q: must be tcp or http", i, svc.Name, typ)
		}
		// remote is only propagated for tcp; ignored for http.
		remotePort := 0
		if typ == "tcp" {
			remotePort = svc.RemotePort
		}
		spec := TunnelSpec{
			Name:       svc.Name,
			Type:       typ,
			LocalAddr:  svc.Local,
			RemotePort: remotePort,
		}
		// slug and access are the wishes of `burrow http --slug --access`.
		// `connect --config` does not know them: it reads past both keys, as
		// it does with any key it has never had.
		if !complete {
			if typ != "http" {
				if svc.Slug != "" {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: slug applies to http services", i, svc.Name)
				}
				if svc.Access != "" {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: access applies to http services", i, svc.Name)
				}
			}
			if svc.Slug != "" && !ValidSlug(svc.Slug) {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: slug %s", i, svc.Name, SlugRule)
			}
			if svc.Access != "" {
				mode, ok := AccessMode(svc.Access)
				if !ok {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: access %s", i, svc.Name, AccessRule)
				}
				spec.Access = mode
			}
			spec.Slug = svc.Slug
		}
		tunnels = append(tunnels, spec)
	}

	return FileConfig{
		Server:  server,
		Token:   token,
		Tunnels: tunnels,
	}, nil
}
