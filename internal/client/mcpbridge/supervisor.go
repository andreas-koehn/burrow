package mcpbridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/backoff"
)

func newBackoff(o Options) *backoff.Backoff { return backoff.New(o.BackoffMin, o.BackoffMax) }

// supervise runs the child until the bridge closes: started once, started
// again with backoff after it ended, until RestartMax restarts are used up.
func (b *Bridge) supervise() {
	defer b.wg.Done()
	defer close(b.done)
	bo := newBackoff(b.opts)
	for {
		began := time.Now()
		err := b.runChild()
		if b.ctx.Err() != nil {
			return
		}
		b.log.Warn("mcp server stopped", "source", b.opts.Name, "error", errText(err))
		b.mu.Lock()
		restarts := b.restarts
		b.mu.Unlock()
		if b.opts.RestartMax >= 0 && restarts >= b.opts.RestartMax {
			b.mu.Lock()
			b.err = fmt.Errorf("the MCP server of %s stopped (%s) and was restarted %d times; it is not restarted again", b.opts.Name, errText(err), restarts)
			b.mu.Unlock()
			b.log.Error("mcp server: giving up", "source", b.opts.Name, "restarts", restarts)
			b.markReady()
			return
		}
		if time.Since(began) >= stableRun {
			bo.Reset()
		}
		d := bo.NextBackOff()
		b.log.Info("mcp server: restarting", "source", b.opts.Name, "in", d.Round(time.Millisecond).String())
		if b.opts.sleep(b.ctx, d) != nil {
			return
		}
		b.mu.Lock()
		b.restarts++
		b.mu.Unlock()
	}
}

func errText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

// runChild starts the child, connects to it and serves its tools until it
// ends or the bridge closes. The child is gone when it returns.
func (b *Bridge) runChild() error {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	stderr := newLineLog(func(line string) {
		b.log.Info("mcp server stderr", "source", b.opts.Name, "line", line)
	})
	defer stderr.flush()

	p, err := startProcess(b.opts.Command, b.opts.Env, stderr, b.opts.TerminateGrace)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.pid = p.cmd.Process.Pid
	b.mu.Unlock()

	var cs *mcp.ClientSession
	stop := func() {
		b.setSession(nil, false)
		p.terminate(b.opts.TerminateGrace)
		if cs != nil {
			_ = cs.Close()
		}
		p.closePipes()
	}

	ctx, cancel := context.WithTimeout(b.ctx, connectTimeout)
	cs, err = b.client.Connect(ctx, &mcp.IOTransport{Reader: p.stdout, Writer: p.stdin}, nil)
	cancel()
	if err != nil {
		cs = nil
		stop()
		if exit := p.exitErr(); exit != nil {
			return exit
		}
		return fmt.Errorf("no MCP handshake: %w", err)
	}
	b.setSession(cs, false)
	if err := b.refresh(b.ctx); err != nil && b.ctx.Err() == nil {
		b.log.Warn("mcp server: tool list not fetched", "source", b.opts.Name, "error", err.Error())
	}

	select {
	case <-p.exited:
	case <-b.ctx.Done():
	}
	stop()
	return p.exitErr()
}

// exitErr is how the child ended: nil for status 0.
func (p *process) exitErr() error {
	select {
	case <-p.exited:
	default:
		return errors.New("still running")
	}
	return p.err
}
