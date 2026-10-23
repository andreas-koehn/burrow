package client

import (
	"fmt"
	"net"
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
	// Forwards are the forwards: section (`burrow up` only). A file may hold
	// forwards alone.
	Forwards []ForwardSpec
}

// rawFileConfig is the intermediate struct used for YAML unmarshalling.
type rawFileConfig struct {
	Server    string       `yaml:"server"`
	Token     string       `yaml:"token"`
	TokenFile string       `yaml:"token_file"`
	Services  []rawService `yaml:"services"`
	Forwards  []rawForward `yaml:"forwards"`
}

// rawForward is one entry under "forwards:" in burrow.yaml: the local
// listener of `burrow forward <target> [local-port] --bind`.
type rawForward struct {
	Target    string `yaml:"target"`
	LocalPort int    `yaml:"local_port"`
	Bind      string `yaml:"bind"`
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
	// Command, Args, Env and URL describe the local MCP server of a service
	// of type mcp (`burrow up` only).
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
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

	// Validate at least one service. `connect --config` runs services alone
	// and reads past forwards, as past any key it does not know.
	var forwards []ForwardSpec
	if !complete {
		var err error
		if forwards, err = forwardsFromFile(raw.Forwards); err != nil {
			return FileConfig{}, err
		}
	}
	if len(raw.Services) == 0 && len(forwards) == 0 {
		if complete {
			return FileConfig{}, fmt.Errorf("loadfileconfig: at least one service is required")
		}
		return FileConfig{}, fmt.Errorf("loadfileconfig: at least one service or forward is required")
	}

	// Build TunnelSpec slice, applying defaults and validations.
	tunnels := make([]TunnelSpec, 0, len(raw.Services))
	for i, svc := range raw.Services {
		if svc.Name == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d]: name is required", i)
		}
		// Default type to tcp.
		typ := svc.Type
		if typ == "" {
			typ = "tcp"
		}
		if typ == "mcp" {
			if complete {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: type mcp is run by `burrow up`, not by `burrow connect`", i, svc.Name)
			}
			mcp, err := mcpFromFile(svc)
			if err != nil {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: %w", i, svc.Name, err)
			}
			if svc.Slug != "" && !ValidSlug(svc.Slug) {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: slug %s", i, svc.Name, SlugRule)
			}
			spec := TunnelSpec{Name: svc.Name, Type: "http", Slug: svc.Slug, MCP: mcp}
			if svc.Access != "" {
				mode, ok := AccessMode(svc.Access)
				if !ok {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: access %s", i, svc.Name, AccessRule)
				}
				spec.Access = mode
			}
			tunnels = append(tunnels, spec)
			continue
		}
		if svc.Local == "" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: local is required", i, svc.Name)
		}
		if typ != "tcp" && typ != "http" {
			return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: unknown type %q: must be tcp, http or mcp", i, svc.Name, typ)
		}
		// The keys of an MCP server belong to type mcp. `connect --config`
		// reads past them like any key it does not know.
		if !complete {
			for _, k := range []struct {
				key string
				set bool
			}{{"command", svc.Command != ""}, {"args", len(svc.Args) > 0}, {"env", len(svc.Env) > 0}, {"url", svc.URL != ""}} {
				if k.set {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: %s applies to type mcp", i, svc.Name, k.key)
				}
			}
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
		// slug and access are the wishes of `burrow http --slug --access`
		// and `burrow tcp --access`. `connect --config` does not know them:
		// it reads past both keys, as it does with any key it has never had.
		if !complete {
			if typ != "http" && svc.Slug != "" {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: slug applies to http services", i, svc.Name)
			}
			if svc.Slug != "" && !ValidSlug(svc.Slug) {
				return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: slug %s", i, svc.Name, SlugRule)
			}
			// A tcp service without access: asks for nothing, so that a
			// file written before access modes serves what it served; a new
			// service is created private (AccessDefault). With remote: the
			// port asked for is the choice of a public port, as before.
			if typ == "tcp" && svc.Access == "" && svc.RemotePort == 0 {
				spec.AccessDefault = "private"
			}
			if typ == "tcp" && svc.Access != "" {
				mode, ok := TCPAccessMode(svc.Access)
				if !ok {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: access %s", i, svc.Name, TCPAccessRule)
				}
				if mode == "private" && svc.RemotePort != 0 {
					return FileConfig{}, fmt.Errorf("loadfileconfig: service[%d] %q: remote needs access open or ip: a private service has no public port", i, svc.Name)
				}
				spec.Access = mode
			} else if svc.Access != "" {
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

	if err := forwardsOverlapServices(forwards, tunnels); err != nil {
		return FileConfig{}, err
	}
	return FileConfig{
		Server:   server,
		Token:    token,
		Tunnels:  tunnels,
		Forwards: forwards,
	}, nil
}

// forwardsFromFile checks the forwards: section and applies the defaults of
// `burrow forward`: local port 13389 on 127.0.0.1.
func forwardsFromFile(raw []rawForward) ([]ForwardSpec, error) {
	out := make([]ForwardSpec, 0, len(raw))
	for i, f := range raw {
		if f.Target == "" {
			return nil, fmt.Errorf("loadfileconfig: forward[%d]: target is required", i)
		}
		if !ValidForwardTarget(f.Target) {
			return nil, fmt.Errorf("loadfileconfig: forward[%d] %q: target %s", i, f.Target, ForwardTargetRule)
		}
		addr, err := ForwardAddr(f.Bind, f.LocalPort)
		if err != nil {
			return nil, fmt.Errorf("loadfileconfig: forward[%d] %q: %w", i, f.Target, err)
		}
		for j, o := range out {
			if sameListenAddr(addr, o.Addr) {
				return nil, fmt.Errorf("loadfileconfig: forward[%d] %q: %s is the address of forward[%d] already; give it another local_port", i, f.Target, addr, j)
			}
		}
		out = append(out, ForwardSpec{Target: f.Target, Addr: addr})
	}
	return out, nil
}

// forwardsOverlapServices refuses a forward that would listen on the local
// address of a service of the same file: the service could not be reached,
// or the forward would lead back into itself.
func forwardsOverlapServices(forwards []ForwardSpec, tunnels []TunnelSpec) error {
	for i, f := range forwards {
		for _, t := range tunnels {
			if t.LocalAddr != "" && sameListenAddr(f.Addr, t.LocalAddr) {
				return fmt.Errorf("loadfileconfig: forward[%d] %q: %s is the local address of service %q; give the forward another local_port", i, f.Target, f.Addr, t.Name)
			}
		}
	}
	return nil
}

// sameListenAddr reports whether two host:port addresses take the same local
// port: the same port on the same host (localhost is the loopback address),
// or on any host when one of them is every address (0.0.0.0, ::).
func sameListenAddr(a, b string) bool {
	ha, pa, err1 := net.SplitHostPort(a)
	hb, pb, err2 := net.SplitHostPort(b)
	if err1 != nil || err2 != nil || pa != pb {
		return false
	}
	norm := func(h string) string {
		if h == "localhost" {
			return "127.0.0.1"
		}
		if ip := net.ParseIP(h); ip != nil {
			if ip.IsUnspecified() {
				return "*"
			}
			return ip.String()
		}
		return h
	}
	ha, hb = norm(ha), norm(hb)
	return ha == hb || ha == "*" || hb == "*"
}
