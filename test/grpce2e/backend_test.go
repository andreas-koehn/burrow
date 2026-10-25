package grpce2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// rawCodec carries []byte messages as they are, so the test service needs
// no generated code. Calls name it with grpc.CallContentSubtype("raw"), and
// the content type is application/grpc+raw, which the relay classifies as
// native gRPC.
type rawCodec struct{}

func (rawCodec) Name() string { return "raw" }

func (rawCodec) Marshal(v any) ([]byte, error) {
	switch b := v.(type) {
	case []byte:
		return b, nil
	case *[]byte:
		return *b, nil
	}
	return nil, fmt.Errorf("raw codec: %T is not []byte", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	p, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("raw codec: %T is not *[]byte", v)
	}
	*p = append([]byte(nil), data...)
	return nil
}

func init() { encoding.RegisterCodec(rawCodec{}) }

const callRaw = "raw"

// echoBackend is the local gRPC server behind a burrow client: the standard
// health service plus e2e.Echo with
//   - Unary: answers "<name>:<payload>";
//   - Sleep: sleeps the payload's milliseconds, then answers; it records the
//     deadline its context had (what grpc-timeout the relay forwarded);
//   - Bidi: answers every message at once with "<name>:<message>".
type echoBackend struct {
	name string
	srv  *grpc.Server
	addr string

	mu        sync.Mutex
	deadlines map[string]time.Duration // Sleep payload -> deadline left at arrival, -1 none
}

func startEchoBackend(name string, tlsCfg *tls.Config) (*echoBackend, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var opts []grpc.ServerOption
	if tlsCfg != nil {
		opts = append(opts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	b := &echoBackend{name: name, srv: grpc.NewServer(opts...), addr: ln.Addr().String(), deadlines: map[string]time.Duration{}}
	hs := health.NewServer()
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	hs.SetServingStatus("e2e.Echo", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(b.srv, hs)
	b.srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: "e2e.Echo",
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Unary", Handler: b.unary},
			{MethodName: "Sleep", Handler: b.sleep},
		},
		Streams: []grpc.StreamDesc{{StreamName: "Bidi", Handler: b.bidi, ServerStreams: true, ClientStreams: true}},
	}, b)
	go func() { _ = b.srv.Serve(ln) }()
	return b, nil
}

func (b *echoBackend) unary(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	var in []byte
	if err := dec(&in); err != nil {
		return nil, err
	}
	return []byte(b.name + ":" + string(in)), nil
}

func (b *echoBackend) sleep(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	var in []byte
	if err := dec(&in); err != nil {
		return nil, err
	}
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	b.mu.Lock()
	b.deadlines[string(in)] = left
	b.mu.Unlock()
	ms, _ := strconv.Atoi(string(in))
	select {
	case <-time.After(time.Duration(ms) * time.Millisecond):
		return []byte(b.name + ":slept"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *echoBackend) bidi(_ any, ss grpc.ServerStream) error {
	for {
		var in []byte
		if err := ss.RecvMsg(&in); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		out := []byte(b.name + ":" + string(in))
		if err := ss.SendMsg(&out); err != nil {
			return err
		}
	}
}

func (b *echoBackend) deadline(id string) (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.deadlines[id]
	return d, ok
}

// startHTTPSBackend is a local HTTP/1.1-over-TLS service (upstream protocol
// https). It answers with what it saw: the protocol and whether TLS ran.
func startHTTPSBackend(cert tls.Certificate) (addr string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "proto=%s tls=%t sni=%s", r.Proto, r.TLS != nil, serverName(r))
		}),
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	return ln.Addr().String(), func() { _ = srv.Close() }, nil
}

func serverName(r *http.Request) string {
	if r.TLS == nil {
		return ""
	}
	return r.TLS.ServerName
}

// tlsEcho is a tcp service that speaks TLS itself (passthrough target): it
// echoes each line back with its name. accepted counts the TLS handshakes
// it completed, so a test can prove a connection never reached it.
type tlsEcho struct {
	name     string
	addr     string
	accepted atomic.Int64
	ln       net.Listener
}

func startTLSEcho(name string, cert tls.Certificate) (*tlsEcho, error) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	e := &tlsEcho{name: name, addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(30 * time.Second))
				if err := c.(*tls.Conn).Handshake(); err != nil {
					return
				}
				e.accepted.Add(1)
				buf := make([]byte, 4096)
				for {
					n, err := c.Read(buf)
					if n > 0 {
						if _, werr := c.Write(append([]byte(e.name+": "), buf[:n]...)); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return e, nil
}
