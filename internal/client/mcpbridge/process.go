package mcpbridge

import (
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// process is a running child with pipes to its stdin and stdout.
type process struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser // our end
	stdout io.ReadCloser  // our end
	exited chan struct{}  // closed once Wait returned
	err    error          // what Wait returned; read after exited
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
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// terminate ends the child: SIGTERM (on Windows, which has no SIGTERM, a
// kill), and a kill when it is still there after grace. It returns once the
// child has been waited for.
func (p *process) terminate(grace time.Duration) {
	select {
	case <-p.exited:
		return
	default:
	}
	if runtime.GOOS == "windows" {
		_ = p.cmd.Process.Kill()
	} else {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-p.exited:
	case <-t.C:
		_ = p.cmd.Process.Kill()
		<-p.exited
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
