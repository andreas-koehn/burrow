package mcpbridge

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// process is a running child with pipes to its stdin and stdout.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser // our end
	stdout io.ReadCloser  // our end
	exited chan struct{}  // closed once Wait returned
	err    error          // what Wait returned; read after exited
	// empty: nothing was left in the child's group when it was reaped, so
	// there is nothing to end and the group id must not be signalled (it
	// may belong to another process by now). Read after exited.
	empty bool
	group platformGroup // the process group (Unix) or job object (Windows)
}

// startProcess starts argv with this process's environment plus env. stderr
// receives the child's stderr (nil discards it). grace bounds how long Wait
// waits for stderr after the child ended, should a grandchild keep it open.
func startProcess(argv, env []string, stderr io.Writer, grace time.Duration) (*process, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the command the user asked to run
	cmd.Env = append(os.Environ(), env...)
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, stderr
	cmd.WaitDelay = grace
	prepare(cmd)
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
		return nil, err
	}
	// The child holds its ends now.
	_ = inR.Close()
	_ = outW.Close()
	p := &process{cmd: cmd, stdin: inW, stdout: outR, exited: make(chan struct{})}
	p.started()
	go func() {
		p.err = cmd.Wait()
		p.empty = p.groupEmpty()
		close(p.exited)
	}()
	return p, nil
}

// terminate ends the child and what it started: a polite stop (SIGTERM to
// its process group; on Windows there is none, so the job is ended at once),
// and a kill when it is still there after grace. It returns once the child
// has been waited for. A child that already ended gets its leftovers killed,
// unless its group was empty when it was reaped.
func (p *process) terminate(grace time.Duration) {
	select {
	case <-p.exited:
		p.killLeftovers()
		p.release()
		return
	default:
	}
	p.stop()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.exited:
		p.killLeftovers() // grandchildren that ignored the stop
	case <-t.C:
		p.kill()
		<-p.exited
	}
	p.release()
}

// killLeftovers kills what is left in the group of a child that was reaped.
func (p *process) killLeftovers() {
	if !p.empty {
		p.kill()
	}
}

func (p *process) closePipes() {
	_ = p.stdin.Close()
	_ = p.stdout.Close()
}

// maxLine is the longest stderr line logged as one; a longer one is cut.
const maxLine = 4096

// lineLog hands what a child writes to stderr to emit line by line.
type lineLog struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func newLineLog(emit func(string)) *lineLog { return &lineLog{emit: emit} }

func (w *lineLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.line(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxLine {
		w.line(w.buf)
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// flush emits what is left after the last newline.
func (w *lineLog) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.line(w.buf)
		w.buf = nil
	}
}

func (w *lineLog) line(b []byte) {
	s := strings.TrimRight(string(b), "\r")
	if len(s) > maxLine {
		s = s[:maxLine] + "…"
	}
	if strings.TrimSpace(s) != "" {
		w.emit(s)
	}
}
