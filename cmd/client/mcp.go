package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/client/mcpbridge"
	"github.com/ankoehn/burrow/internal/logging"
)

// mcpReadyWait is how long a command waits for the first tool list of a
// local MCP server before it registers the tunnel anyway: the relay's first
// sync should find the tools.
var mcpReadyWait = 30 * time.Second

// newMCPCmd builds `burrow mcp`.
func newMCPCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp <name> (-- <command> [args...] | <http-url>)",
		Short: "Publish a local MCP server as a tool source",
		Long: "Publish a local MCP server as a tool source of the relay's AI gateway.\n\n" +
			"  burrow mcp files -- npx -y @modelcontextprotocol/server-filesystem ~/docs\n" +
			"      runs the command and speaks MCP with it over stdin and stdout\n" +
			"  burrow mcp notes http://localhost:3001/mcp\n" +
			"      uses a local server that already runs (Streamable HTTP)\n\n" +
			"<name> is the service the dashboard created in Tools → Add local source; the\n" +
			"tunnel binds to it by name. Only tools are published. A command is started\n" +
			"again whenever it ends, with a growing wait in between; --restart-max limits how\n" +
			"often. Its stderr goes to the log lines with the source's name.\n\n" +
			"The command runs in the foreground until interrupted and reconnects by itself.",
		Args: func(cmd *cobra.Command, args []string) error {
			dash := cmd.ArgsLenAtDash()
			switch {
			case len(args) == 0:
				return usageWithLine(cmd, "%s needs a name, then a command after -- or an http:// URL", cmd.CommandPath())
			case dash == 1 && len(args) >= 2, dash == -1 && len(args) == 2:
				return nil
			case dash == 1 || (dash == -1 && len(args) == 1):
				return usageWithLine(cmd, "%s needs a command after -- or an http:// URL, for example: %s %s -- npx -y <package>", cmd.CommandPath(), cmd.CommandPath(), args[0])
			}
			return usageWithLine(cmd, "%s takes a name and then either -- <command> or one URL", cmd.CommandPath())
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			name := strings.TrimSpace(args[0])
			if name == "" {
				return usageErrorf("the name must not be empty")
			}
			spec := client.MCPSpec{RestartMax: -1}
			if cmd.ArgsLenAtDash() == 1 {
				spec.Command = args[1:]
			} else {
				if !client.ValidMCPURL(args[1]) {
					return usageErrorf("%q is not an http:// or https:// URL. To run a command, put it after --: %s %s -- %s",
						args[1], cmd.CommandPath(), name, args[1])
				}
				spec.URL = args[1]
				for _, f := range []string{"env", "restart-max"} {
					if flags.Changed(f) {
						return usageErrorf("--%s applies to a command, not to a URL", f)
					}
				}
			}
			env, _ := flags.GetStringArray("env")
			for _, kv := range env {
				if k, _, ok := strings.Cut(kv, "="); !ok || !client.ValidEnvKey(k) {
					return usageErrorf("--env takes KEY=VALUE, not %q", kv)
				}
			}
			spec.Env = env
			if rm, _ := flags.GetInt("restart-max"); flags.Changed("restart-max") {
				if rm < -1 {
					return usageErrorf("--restart-max must be 0 or more, or -1 for no limit")
				}
				spec.RestartMax = rm
			}

			g, err := readGlobals(cmd, d)
			if err != nil {
				return err
			}
			userPath, err := userConfigFile(cmd, d)
			if err != nil {
				return err
			}
			creds, err := resolveCredentials(d, userPath, nil)
			if err != nil {
				return err
			}
			return foregroundWithBridges(cmd, d, creds, []client.TunnelSpec{{Name: name, Type: "http", MCP: &spec}}, g)
		},
	}
	f := cmd.Flags()
	f.StringArray("env", nil, "KEY=VALUE for the command's environment (repeatable)")
	f.Int("restart-max", -1, "how often the command is started again after it ended; -1 is no limit")
	return cmd
}

// bridgeLogger is where the bridges write, among them the stderr of the
// children: the log lines of the run. The status view replaces the log lines,
// and a line printed over it would break it, so with the view it is quiet.
func bridgeLogger(g globalFlags) *slog.Logger {
	if g.view {
		return slog.New(slog.DiscardHandler)
	}
	return logging.NewTo(viewErr, g.logLevel, g.logFormat)
}

// foregroundWithBridges is foreground for tunnels of which some have a local
// MCP server: a bridge is started for each of those and becomes its local
// address. The bridges, and with them the children, end when the run ends. A
// bridge that gives up on its server (--restart-max) ends the run with its
// reason.
func foregroundWithBridges(cmd *cobra.Command, d deps, creds client.Credentials, tunnels []client.TunnelSpec, g globalFlags) error {
	if !slices.ContainsFunc(tunnels, func(t client.TunnelSpec) bool { return t.MCP != nil }) {
		return foreground(cmd, d, creds, tunnels, g)
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	log := bridgeLogger(g)
	var bridges []*mcpbridge.Bridge
	defer func() {
		for _, b := range slices.Backward(bridges) {
			_ = b.Close()
		}
	}()
	out := slices.Clone(tunnels)
	for i, t := range out {
		if t.MCP == nil {
			continue
		}
		spec := *t.MCP
		status := &client.MCPStatus{}
		spec.Status = status
		b, err := mcpbridge.Start(mcpbridge.Options{
			Name: t.Name, Command: spec.Command, Env: spec.Env, URL: spec.URL,
			RestartMax: spec.RestartMax, Logger: log,
			OnStatus: func(s mcpbridge.Status) { status.Set(s.Tools, s.Running) },
		})
		if err != nil {
			return &exitError{code: exitGeneral, msg: err.Error()}
		}
		bridges = append(bridges, b)
		t.LocalAddr, t.MCP = b.Addr(), &spec
		out[i] = t
	}

	// The first tool lists, so that the relay finds the tools when the
	// tunnel comes up. Ctrl-C while waiting still closes the bridges.
	wait, stopWait := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	timer := time.NewTimer(mcpReadyWait)
	for i, b := range bridges {
		select {
		case <-b.Ready():
		case <-timer.C:
			log.Warn("mcp server: no tool list yet; the tunnel comes up anyway", "source", out[i].Name)
		case <-wait.Done():
		}
		if err := b.Err(); err != nil {
			stopWait()
			timer.Stop()
			return &exitError{code: exitGeneral, msg: err.Error()}
		}
	}
	timer.Stop()
	interrupted := wait.Err() != nil && ctx.Err() == nil
	stopWait()
	if interrupted {
		return nil
	}

	for _, b := range bridges {
		go func() {
			select {
			case <-b.Done():
				if err := b.Err(); err != nil {
					cancel(err)
				}
			case <-ctx.Done():
			}
		}()
	}
	cmd.SetContext(ctx)
	err := foreground(cmd, d, creds, out, g)
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) && parent.Err() == nil {
		return &exitError{code: exitGeneral, msg: cause.Error()}
	}
	return err
}
