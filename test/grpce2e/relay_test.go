package grpce2e

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	adminEmail    = "admin@grpce2e.test"
	adminPassword = "Grpce2e-admin-pass-1234"
)

// freePort returns a port nothing listens on at the moment.
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// freeRange returns the first port of n consecutive free ports.
func freeRange(n int) (int, error) {
	for base := 30000 + os.Getpid()%2000*10; base < 60000; base += n {
		ok := true
		for p := base; p < base+n && ok; p++ {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
			if err != nil {
				ok = false
				continue
			}
			ln.Close()
		}
		if ok {
			return base, nil
		}
	}
	return 0, errors.New("no free port range")
}

// proc is a child process of the test (relay or client) with its log file.
type proc struct {
	name string
	cmd  *exec.Cmd
	log  string
	done chan struct{}
}

func startProc(name, dir, bin string, env []string, args ...string) (*proc, error) {
	logPath := filepath.Join(dir, name+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	p := &proc{name: name, cmd: cmd, log: logPath, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); f.Close(); close(p.done) }()
	return p, nil
}

// stop ends the process: SIGTERM, then SIGKILL after 10 s.
func (p *proc) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

func (p *proc) tail(n int) string {
	b, _ := os.ReadFile(p.log)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// relay is one running burrowd with an admin session on its API.
type relay struct {
	name    string
	dir     string
	env     []string
	proc    *proc
	ctrl    string // control listener host:port
	http    string // dashboard listener host:port
	tls     bool
	portMin int
	token   string // client token of the admin
	api     *apiClient

	mu      sync.Mutex
	clients []*proc
}

type relayOpts struct {
	name string
	// env are extra BURROW_* settings (KEY=VALUE).
	env []string
	// dashCert, when set, makes the dashboard listener terminate TLS.
	dashCert *leaf
}

// startRelay starts burrowd on ports that were free a moment ago. burrowd
// binds them itself, so another process may take one in between: then the
// relay stops with "address already in use", and it is started again on
// new ports (at most three times).
func startRelay(e *env, o relayOpts) (*relay, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		r, err := startRelayOnce(e, o)
		if err == nil {
			return r, nil
		}
		lastErr = err
		if r != nil {
			r.stop()
		}
		if !strings.Contains(err.Error(), "address already in use") {
			return r, err
		}
		_ = os.RemoveAll(filepath.Join(e.dir, o.name))
	}
	return nil, lastErr
}

func startRelayOnce(e *env, o relayOpts) (*relay, error) {
	dir := filepath.Join(e.dir, o.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ctrlPort, err := freePort()
	if err != nil {
		return nil, err
	}
	httpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	portMin, err := freeRange(10)
	if err != nil {
		return nil, err
	}
	ctrlCert, err := e.ca.issue("control", "127.0.0.1", "localhost")
	if err != nil {
		return nil, err
	}
	cc, ck, err := ctrlCert.write(dir, "control")
	if err != nil {
		return nil, err
	}
	r := &relay{
		name: o.name, dir: dir, portMin: portMin,
		ctrl: fmt.Sprintf("127.0.0.1:%d", ctrlPort),
		http: fmt.Sprintf("127.0.0.1:%d", httpPort),
	}
	r.env = append(baseEnv(dir),
		"BURROW_DATABASE_PATH="+filepath.Join(dir, "burrow.db"),
		"BURROW_ADMIN_EMAIL="+adminEmail,
		"BURROW_ADMIN_PASSWORD="+adminPassword,
		"BURROW_LISTEN="+r.ctrl,
		"BURROW_TLS_CERT="+cc,
		"BURROW_TLS_KEY="+ck,
		"BURROW_HTTP_LISTEN="+r.http,
		fmt.Sprintf("BURROW_PORT_MIN=%d", portMin),
		fmt.Sprintf("BURROW_PORT_MAX=%d", portMin+9),
		"BURROW_PUBLIC_BIND=127.0.0.1",
		"BURROW_LOG_LEVEL=debug",
		"BURROW_CERT_VALIDATION_ROOTS_FILE="+e.caPath,
	)
	if o.dashCert != nil {
		dc, dk, err := o.dashCert.write(dir, "dashboard")
		if err != nil {
			return nil, err
		}
		r.env = append(r.env, "BURROW_HTTP_TLS_CERT="+dc, "BURROW_HTTP_TLS_KEY="+dk)
		r.tls = true
	}
	r.env = append(r.env, o.env...)
	if r.proc, err = startProc("burrowd", dir, e.burrowd, r.env, "serve"); err != nil {
		return nil, err
	}
	r.api = newAPIClient(r.baseURL(), e)
	if err := r.waitReady(); err != nil {
		return r, fmt.Errorf("relay %s not ready: %w\n%s", o.name, err, r.proc.tail(40))
	}
	if err := r.api.login(); err != nil {
		return r, fmt.Errorf("relay %s login: %w", o.name, err)
	}
	cmd := exec.Command(e.burrowd, "token", "--email", adminEmail, "--name", "grpce2e")
	cmd.Env, cmd.Dir = r.env, dir
	out, err := cmd.Output()
	if err != nil {
		return r, fmt.Errorf("mint client token: %w", err)
	}
	r.token = strings.TrimSpace(string(out))
	return r, nil
}

// baseEnv is a minimal environment for a child: no BURROW_* of the caller
// leaks in, and the client's config lives in the run's directory.
func baseEnv(dir string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg"),
		"TMPDIR=" + os.TempDir(),
	}
}

func (r *relay) baseURL() string {
	if r.tls {
		return "https://" + r.http
	}
	return "http://" + r.http
}

func (r *relay) waitReady() error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-r.proc.done:
			return errors.New("burrowd exited")
		default:
		}
		resp, err := r.api.c.Get(r.baseURL() + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("timeout")
}

// client starts a burrow client (`burrow <args>`) against the relay and waits
// until the relay lists the service name as connected. It returns the
// service's id.
func (r *relay) client(e *env, name string, args ...string) (string, *proc, error) {
	cdir := filepath.Join(r.dir, "client-"+name)
	if err := os.MkdirAll(cdir, 0o700); err != nil {
		return "", nil, err
	}
	envv := append(baseEnv(cdir),
		"BURROW_SERVER="+r.ctrl,
		"BURROW_TOKEN="+r.token,
		"BURROW_LOG_LEVEL=debug",
	)
	full := append(append([]string{}, args...), "--name", name, "--cacert", e.caPath)
	p, err := startProc("burrow", cdir, e.burrow, envv, full...)
	if err != nil {
		return "", nil, err
	}
	r.mu.Lock()
	r.clients = append(r.clients, p)
	r.mu.Unlock()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			return "", p, fmt.Errorf("client %s exited:\n%s", name, p.tail(30))
		default:
		}
		svcs, err := r.api.services()
		if err == nil {
			for _, s := range svcs {
				if s.Name == name && s.Connected {
					return s.ID, p, nil
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return "", p, fmt.Errorf("client %s never connected:\n%s", name, p.tail(30))
}

func (r *relay) stop() {
	r.mu.Lock()
	cs := r.clients
	r.mu.Unlock()
	for _, c := range cs {
		c.stop()
	}
	if r.proc != nil {
		r.proc.stop()
	}
}

// apiClient is an admin session on the relay's REST API (cookie session,
// CSRF header, a recent confirmation for the writes that need one).
type apiClient struct {
	base string
	c    *http.Client
}

func newAPIClient(base string, e *env) *apiClient {
	jar, _ := cookiejar.New(nil)
	return &apiClient{base: base, c: &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: e.ca.pool},
			ForceAttemptHTTP2: true,
		},
	}}
}

func (a *apiClient) csrf() string {
	req, _ := http.NewRequest("GET", a.base+"/", nil)
	for _, c := range a.c.Jar.Cookies(req.URL) {
		if strings.HasSuffix(c.Name, "burrow_csrf") {
			return c.Value
		}
	}
	return ""
}

// do sends a JSON request and returns the status and body.
func (a *apiClient) do(method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	url := a.base + path
	if strings.HasPrefix(path, "/api/") || path == "/metrics" {
		// as given
	} else {
		url = a.base + "/api/v1" + path
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.base)
	req.Header.Set("X-CSRF-Token", a.csrf())
	resp, err := a.c.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// must sends the request and fails unless the status is want.
func (a *apiClient) must(t *testing.T, want int, method, path string, body any) []byte {
	t.Helper()
	st, b, err := a.do(method, path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if st != want {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, st, want, b)
	}
	return b
}

func (a *apiClient) login() error {
	st, b, err := a.do("POST", "/auth/login", map[string]string{"email": adminEmail, "password": adminPassword})
	if err != nil {
		return err
	}
	if st != 200 {
		return fmt.Errorf("status %d: %s", st, b)
	}
	return a.reauth()
}

func (a *apiClient) reauth() error {
	st, b, err := a.do("POST", "/auth/reauth", map[string]string{"password": adminPassword})
	if err != nil {
		return err
	}
	if st != 200 {
		return fmt.Errorf("reauth status %d: %s", st, b)
	}
	return nil
}

type serviceView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Type       string `json:"type"`
	Connected  bool   `json:"connected"`
	AccessMode string `json:"access_mode"`
}

func (a *apiClient) services() ([]serviceView, error) {
	st, b, err := a.do("GET", "/services", nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, fmt.Errorf("status %d: %s", st, b)
	}
	var out []serviceView
	if err := json.Unmarshal(b, &out); err != nil {
		// some versions wrap the list
		var w struct {
			Items []serviceView `json:"items"`
		}
		if err2 := json.Unmarshal(b, &w); err2 != nil {
			return nil, err
		}
		out = w.Items
	}
	return out, nil
}

func (a *apiClient) service(t *testing.T, id string) serviceView {
	t.Helper()
	var s serviceView
	if err := json.Unmarshal(a.must(t, 200, "GET", "/services/"+id, nil), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

type connLogRow struct {
	Kind       string `json:"kind"`
	ServiceID  string `json:"service_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	Protocol   string `json:"protocol"`
	SNI        string `json:"sni"`
	SourceIP   string `json:"source_ip"`
	GRPCMethod string `json:"grpc_method"`
	GRPCStatus *int   `json:"grpc_status"`
}

func (a *apiClient) connLogs(t *testing.T, serviceID string) []connLogRow {
	t.Helper()
	b := a.must(t, 200, "GET", "/connection-logs?limit=1000&service_id="+serviceID, nil)
	var rows []connLogRow
	if err := json.Unmarshal(b, &rows); err != nil {
		var w struct {
			Items []connLogRow `json:"items"`
		}
		if err2 := json.Unmarshal(b, &w); err2 != nil {
			t.Fatalf("connection logs: %v: %s", err, b)
		}
		rows = w.Items
	}
	return rows
}

// eventually retries f until it returns nil or the time is up.
func eventually(d time.Duration, f func() error) error {
	deadline := time.Now().Add(d)
	var err error
	for {
		if err = f(); err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
