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
}

// LoadFileConfig reads and validates a burrow.yaml file at path.
// It returns a FileConfig with Tunnels populated, and Server and Token when the
// file has them.
func LoadFileConfig(path string) (FileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("loadfileconfig: read %s: %w", path, err)
	}

	var raw rawFileConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return FileConfig{}, fmt.Errorf("loadfileconfig: parse %s: %w", path, err)
	}

	// server and token are optional: what is absent comes from the sign-in.
	// A blank value counts as absent.
	server := strings.TrimSpace(raw.Server)
	token := strings.TrimSpace(raw.Token)

	// At most one of token / token_file.
	if token != "" && raw.TokenFile != "" {
		return FileConfig{}, fmt.Errorf("loadfileconfig: only one of token or token_file may be set")
	}

	// Resolve token_file.
	if raw.TokenFile != "" {
		b, err := os.ReadFile(raw.TokenFile)
		if err != nil {
			return FileConfig{}, fmt.Errorf("loadfileconfig: token_file %q: %w", raw.TokenFile, err)
		}
		token = strings.TrimSpace(string(b))
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
		tunnels = append(tunnels, TunnelSpec{
			Name:       svc.Name,
			Type:       typ,
			LocalAddr:  svc.Local,
			RemotePort: remotePort,
		})
	}

	return FileConfig{
		Server:  server,
		Token:   token,
		Tunnels: tunnels,
	}, nil
}
