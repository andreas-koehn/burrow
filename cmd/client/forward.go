package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ankoehn/burrow/internal/client"
)

// The forwards of a run ride on its context, as the mark of a foreground run
// does: the commands that have none (http, tcp, mcp, connect) pass nothing.
type forwardsKey struct{}

func withForwards(ctx context.Context, fw []client.ForwardSpec) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(fw) == 0 {
		return ctx
	}
	return context.WithValue(ctx, forwardsKey{}, fw)
}

func forwardsOf(ctx context.Context) []client.ForwardSpec {
	if ctx == nil {
		return nil
	}
	fw, _ := ctx.Value(forwardsKey{}).([]client.ForwardSpec)
	return fw
}

// warnWideForwards says on stderr that a forward listens where other machines
// can reach it: whoever reaches that address reaches the private service.
func warnWideForwards(fw []client.ForwardSpec) {
	for _, f := range fw {
		if !client.LoopbackAddr(f.Addr) {
			fmt.Fprintf(viewErr, "WARNING: the forward to %s listens on %s: every machine that can reach that address reaches the service without signing in. "+
				"Keep it on 127.0.0.1 unless that is what you want.\n", f.Target, f.Addr)
		}
	}
}

// newForwardCmd builds `burrow forward <target> [local-port]`.
func newForwardCmd(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forward <target> [local-port]",
		Short: "Reach a tcp service through the relay on a local port",
		Long: "Reach a tcp service through the relay on a local port.\n\n" +
			"<target> is the service's slug (the dashboard shows it, and `burrow tcp` prints it), the name\n" +
			"of one of your own tcp services, or its id. Private services are reachable this way only;\n" +
			"their owner and the roles the owner allows may forward to them.\n\n" +
			"The local port defaults to " + strconv.Itoa(client.DefaultForwardPort) + " on " + client.DefaultForwardBind + ". For remote desktop:\n" +
			"  burrow forward rdp            then connect mstsc or Remmina to 127.0.0.1:13389\n\n" +
			"The command runs in the foreground until interrupted. The local port stays open while the\n" +
			"relay is away; connections made then are closed at once, and it reconnects by itself.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return usageWithLine(cmd, "%s needs a target and at most a local port, for example: %s rdp 13389", cmd.CommandPath(), cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			if !client.ValidForwardTarget(target) {
				return usageErrorf("the target %s", client.ForwardTargetRule)
			}
			port := 0
			if len(args) == 2 {
				p, err := strconv.Atoi(strings.TrimSpace(args[1]))
				if err != nil || p < 1 {
					return usageErrorf("the local port must be between 1 and 65535")
				}
				port = p
			}
			bind, _ := cmd.Flags().GetString("bind")
			addr, err := client.ForwardAddr(bind, port)
			if err != nil {
				return usageErrorf("%s", err.Error())
			}
			g, err := readGlobals(cmd, d)
			if err != nil {
				return err
			}
			if err := g.requireSecureForward(); err != nil {
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
			fw := []client.ForwardSpec{{Target: target, Addr: addr}}
			warnWideForwards(fw)
			cmd.SetContext(withForwards(cmd.Context(), fw))
			return foreground(cmd, d, creds, nil, g)
		},
	}
	cmd.Flags().String("bind", client.DefaultForwardBind, "local address to listen on (an IP address or localhost)")
	return cmd
}
