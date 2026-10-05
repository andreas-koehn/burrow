// Package view is the status view of the client: what it shows (Model, Store),
// how that becomes lines (Render) and how the lines are redrawn in place on a
// terminal (Screen).
package view

import (
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ankoehn/burrow/internal/client"
)

// maxRecent is how many recent lines a service keeps.
const maxRecent = 10

// Model is everything the status view shows. It is a plain value: Render is a
// pure function of it.
type Model struct {
	Relay, Version string
	State          client.ConnState
	Detail         string // last error while reconnecting
	RetryIn        time.Duration
	RTT            time.Duration // 0 = not measured yet
	Services       []Service
	Notice         string // one line, e.g. a version notice
}

// Service is one exposed service.
type Service struct {
	Name, Type, Public, Local string
	Access                    string // "" = unknown
	Open, Total               int
	Recent                    []Line // newest last, at most 10
	LocalDown                 bool
}

// Line is one recent request or connection.
type Line struct {
	At           time.Time
	Method, Path string // request summaries; empty for a plain connection
	Status       int
	SourceIP     string
}

// Store is a concurrency-safe Model that implements client.Observer. Its
// methods only change memory, so they return at once.
type Store struct {
	changed chan struct{}
	now     func() time.Time

	mu       sync.Mutex
	m        Model
	retryAt  time.Time
	byTunnel map[string]int // tunnel id of this session → index in m.Services
	claimed  map[int]bool   // services that have a tunnel in this session
}

// NewStore returns a Store for a client that is about to connect to relay (a
// host name) with the given tunnels.
func NewStore(relay, version string, specs []client.TunnelSpec) *Store {
	s := &Store{
		changed:  make(chan struct{}, 1),
		now:      time.Now,
		m:        Model{Relay: relay, Version: version, State: client.StateConnecting},
		byTunnel: map[string]int{},
		claimed:  map[int]bool{},
	}
	for _, sp := range specs {
		s.m.Services = append(s.m.Services, Service{Name: sp.Name, Type: sp.Type, Local: sp.LocalAddr})
	}
	return s
}

// Snapshot returns a copy of the current model.
func (s *Store) Snapshot() Model {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m
	if !s.retryAt.IsZero() {
		if m.RetryIn = s.retryAt.Sub(s.now()); m.RetryIn < 0 {
			m.RetryIn = 0
		}
	}
	m.Services = make([]Service, len(s.m.Services))
	for i, sv := range s.m.Services {
		sv.Recent = append([]Line(nil), sv.Recent...)
		m.Services[i] = sv
	}
	return m
}

// Changed is signalled after every change. The signal never blocks the one
// who changes the store, and several changes may arrive as one signal.
func (s *Store) Changed() <-chan struct{} { return s.changed }

// update changes the model under the lock and signals the change.
func (s *Store) update(f func()) {
	s.mu.Lock()
	f()
	s.mu.Unlock()
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// State implements client.Observer.
func (s *Store) State(st client.ConnState, detail string, retryIn time.Duration) {
	s.update(func() {
		s.m.State, s.m.Detail, s.m.RetryIn, s.retryAt = st, "", 0, time.Time{}
		if st == client.StateReconnecting {
			s.m.Detail = detail
			if retryIn > 0 {
				s.retryAt = s.now().Add(retryIn)
			}
		}
		if st == client.StateConnected {
			return
		}
		// The session is gone, and with it its tunnel ids, its connections
		// and its round-trip time.
		clear(s.byTunnel)
		clear(s.claimed)
		s.m.RTT = 0
		for i := range s.m.Services {
			s.m.Services[i].Open = 0
		}
	})
}

// Registered implements client.Observer.
func (s *Store) Registered(t client.RegisteredTunnel) {
	s.update(func() {
		i := -1
		for j, sv := range s.m.Services {
			if !s.claimed[j] && sv.Name == t.Name && sv.Type == t.Type && sv.Local == t.LocalAddr {
				i = j
				break
			}
		}
		if i < 0 {
			s.m.Services = append(s.m.Services, Service{Name: t.Name, Type: t.Type, Local: t.LocalAddr})
			i = len(s.m.Services) - 1
		}
		s.claimed[i] = true
		s.byTunnel[t.TunnelID] = i
		switch {
		case t.URL != "":
			s.m.Services[i].Public = t.URL
		case t.RemotePort > 0:
			s.m.Services[i].Public = net.JoinHostPort(s.m.Relay, strconv.Itoa(t.RemotePort))
		}
	})
}

// Connection implements client.Observer.
func (s *Store) Connection(tunnelID string, at time.Time, sourceIP string) {
	s.update(func() {
		i, ok := s.byTunnel[tunnelID]
		if !ok {
			return
		}
		sv := &s.m.Services[i]
		sv.Open++
		sv.Total++
		// The relay sends the visitor's address with its port; the view names
		// the address.
		if host, _, err := net.SplitHostPort(sourceIP); err == nil {
			sourceIP = host
		}
		sv.Recent = append(sv.Recent, Line{At: at, SourceIP: sourceIP})
		if n := len(sv.Recent) - maxRecent; n > 0 {
			sv.Recent = append([]Line(nil), sv.Recent[n:]...)
		}
	})
}

// ConnectionClosed implements client.Observer.
func (s *Store) ConnectionClosed(tunnelID string) {
	s.update(func() {
		if i, ok := s.byTunnel[tunnelID]; ok && s.m.Services[i].Open > 0 {
			s.m.Services[i].Open--
		}
	})
}

// Latency implements client.Observer.
func (s *Store) Latency(rtt time.Duration) {
	s.update(func() { s.m.RTT = rtt })
}

// LocalTarget implements client.Observer.
func (s *Store) LocalTarget(localAddr string, reachable bool) {
	s.update(func() {
		for i := range s.m.Services {
			if s.m.Services[i].Local == localAddr {
				s.m.Services[i].LocalDown = !reachable
			}
		}
	})
}
