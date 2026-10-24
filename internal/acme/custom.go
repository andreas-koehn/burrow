package acme

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Issuer is what CustomDomains needs of certmagic. *Manager satisfies it;
// tests use a fake.
type Issuer interface {
	// ManageAsync starts obtaining and renewing certificates for names in
	// the background and returns before any ACME transaction. Cancelling
	// ctx stops the retries of a failed order.
	ManageAsync(ctx context.Context, names []string) error
	// Unmanage stops the renewal of the names' certificates.
	Unmanage(names []string)
	// HasCertificate reports whether a valid certificate for name is loaded.
	HasCertificate(name string) bool
}

// ErrNotOwned refuses a name the relay must not order a certificate for:
// one it does not own by the host-name rules (H12), or a wildcard, which
// HTTP-01 and TLS-ALPN-01 cannot prove.
var ErrNotOwned = errors.New("acme: the relay does not own this name")

// CustomName is a custom domain with cert_source auto.
type CustomName struct {
	ServiceID string
	Hostname  string
}

// CustomOptions configure CustomDomains.
type CustomOptions struct {
	// Allow says why hostname cannot be a custom domain of this relay
	// ("" when it can): the same refusals as the API's (H12). It is asked
	// before every order, also for the names loaded at startup, whose rows
	// may predate a change of the configuration. nil refuses every name.
	Allow func(hostname string) string
	// OnFailure is told of every failure for a managed name: a refusal, an
	// error of ManageAsync, and every cert_failed of certmagic (the caller
	// audits it). nil only logs.
	OnFailure func(serviceID, hostname, reason string)
	// Stagger is the pause between two names loaded at startup, so that a
	// relay with many names does not send a burst of orders to the CA
	// (certmagic adds its own limiter and exponential backoff on failures).
	Stagger time.Duration
	Log     *slog.Logger
}

// CustomDomains obtains certificates for the custom domains with
// cert_source auto (H10): certmagic ManageAsync when a name is bound and at
// startup, and Unmanage when it is deleted or switched to an uploaded
// certificate. Nothing here waits on the CA: an order runs in the
// background, and its failures are logged and reported to OnFailure.
type CustomDomains struct {
	base context.Context
	iss  Issuer
	opt  CustomOptions
	log  *slog.Logger

	mu      sync.Mutex
	managed map[string]customEntry // hostname → its order
	queued  map[string]string      // hostname → service id, waiting in Load
	wg      sync.WaitGroup
	gen     uint64
}

type customEntry struct {
	serviceID string
	cancel    context.CancelFunc
	gen       uint64 // tells this order from a later one for the name
}

// NewCustomDomains returns a CustomDomains whose orders live as long as
// ctx, the relay's.
func NewCustomDomains(ctx context.Context, iss Issuer, o CustomOptions) *CustomDomains {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &CustomDomains{
		base: ctx, iss: iss, opt: o, log: log,
		managed: map[string]customEntry{},
		queued:  map[string]string{},
	}
}

// Manage starts obtaining a certificate for hostname, a custom domain of
// the service. It returns ErrNotOwned (wrapped) for a name the relay must
// not order, and the error of ManageAsync, which happens before any ACME
// transaction. A name already managed for the service is left as it is.
func (c *CustomDomains) Manage(serviceID, hostname string) error {
	return c.manage(serviceID, hostname, false)
}

// manage registers the name under the lock and calls ManageAsync outside
// it, so a slow certmagic never holds up the API. fromQueue: only while
// Load still queues the name (a delete in between wins).
func (c *CustomDomains) manage(serviceID, hostname string, fromQueue bool) error {
	c.mu.Lock()
	if fromQueue {
		if sid, ok := c.queued[hostname]; !ok || sid != serviceID {
			c.mu.Unlock()
			return nil
		}
	}
	delete(c.queued, hostname)
	if why := c.refusal(hostname); why != "" {
		c.mu.Unlock()
		c.log.Warn("acme: no certificate for a custom domain the relay does not own",
			"hostname", hostname, "service_id", serviceID, "reason", why)
		c.fail(serviceID, hostname, why)
		return errors.Join(ErrNotOwned, errors.New(why))
	}
	if e, ok := c.managed[hostname]; ok {
		if e.serviceID == serviceID {
			c.mu.Unlock()
			return nil
		}
		e.cancel()
	}
	// The order outlives the request that bound the name; Unmanage or the
	// relay's end stops its retries.
	ctx, cancel := context.WithCancel(c.base)
	c.gen++
	gen := c.gen
	c.managed[hostname] = customEntry{serviceID: serviceID, cancel: cancel, gen: gen}
	c.mu.Unlock()

	if err := c.iss.ManageAsync(ctx, []string{hostname}); err != nil {
		// Nothing was started: the name is not managed, and the next bind
		// tries again.
		c.mu.Lock()
		if e, ok := c.managed[hostname]; ok && e.gen == gen {
			delete(c.managed, hostname)
		}
		c.mu.Unlock()
		cancel()
		c.log.Warn("acme: managing a custom domain failed", "hostname", hostname, "service_id", serviceID, "err", err)
		c.fail(serviceID, hostname, err.Error())
		return err
	}
	c.log.Info("acme: obtaining a certificate for a custom domain", "hostname", hostname, "service_id", serviceID)
	return nil
}

func (c *CustomDomains) refusal(hostname string) string {
	switch {
	case hostname == "":
		return "no host name"
	case strings.HasPrefix(hostname, "*."):
		return "a wildcard custom domain needs an uploaded certificate"
	case c.opt.Allow == nil:
		return "no host-name rules configured"
	}
	return c.opt.Allow(hostname)
}

// Unmanage stops obtaining and renewing the certificate of hostname, and
// takes it off the startup queue. A name that is not managed is ignored.
func (c *CustomDomains) Unmanage(hostname string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.queued, hostname)
	e, ok := c.managed[hostname]
	if !ok {
		return
	}
	e.cancel()
	delete(c.managed, hostname)
	c.iss.Unmanage([]string{hostname})
	c.log.Info("acme: stopped managing a custom domain", "hostname", hostname, "service_id", e.serviceID)
	// certmagic loads a stored certificate into its cache on a goroutine
	// of its own, which a cancelled context does not stop: a delete right
	// after a bind could find nothing to remove yet. Remove once more a
	// little later, unless the name was bound again meanwhile.
	time.AfterFunc(reunmanageAfter, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, again := c.managed[hostname]; !again {
			c.iss.Unmanage([]string{hostname})
		}
	})
}

// reunmanageAfter is how long Unmanage waits before it removes the name a
// second time; tests shorten it.
var reunmanageAfter = 30 * time.Second

// Ready reports whether hostname is managed and its certificate is loaded
// (cert_status ready; pending otherwise).
func (c *CustomDomains) Ready(hostname string) bool {
	c.mu.Lock()
	_, ok := c.managed[hostname]
	c.mu.Unlock()
	return ok && c.iss.HasCertificate(hostname)
}

// CertEvent takes an event of certmagic (Manager.OnCertEvent). A
// cert_failed of a managed name is logged and reported to OnFailure; the
// events of the relay's own names are not this type's.
func (c *CustomDomains) CertEvent(event, hostname string, err error) {
	c.mu.Lock()
	e, ok := c.managed[hostname]
	c.mu.Unlock()
	if !ok {
		return
	}
	switch event {
	case "cert_failed":
		reason := "certificate order failed"
		if err != nil {
			reason = err.Error()
		}
		c.log.Warn("acme: obtaining the certificate of a custom domain failed; certmagic retries with backoff",
			"hostname", hostname, "service_id", e.serviceID, "err", reason)
		c.fail(e.serviceID, hostname, reason)
	case "cert_obtained":
		c.log.Info("acme: certificate of a custom domain obtained", "hostname", hostname, "service_id", e.serviceID)
	}
}

func (c *CustomDomains) fail(serviceID, hostname, reason string) {
	if c.opt.OnFailure != nil {
		c.opt.OnFailure(serviceID, hostname, reason)
	}
}

// Load queues the custom domains with cert_source auto that exist at
// startup and manages them in the background, Stagger apart. It returns at
// once: startup never waits on the CA. A name unmanaged before its turn is
// skipped.
func (c *CustomDomains) Load(names []CustomName) {
	c.mu.Lock()
	var todo []CustomName
	for _, n := range names {
		if _, ok := c.managed[n.Hostname]; ok {
			continue
		}
		if _, ok := c.queued[n.Hostname]; ok {
			continue
		}
		c.queued[n.Hostname] = n.ServiceID
		todo = append(todo, n)
	}
	c.mu.Unlock()
	if len(todo) == 0 {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for i, n := range todo {
			if i > 0 && c.opt.Stagger > 0 {
				t := time.NewTimer(c.opt.Stagger)
				select {
				case <-c.base.Done():
					t.Stop()
					return
				case <-t.C:
				}
			}
			if c.base.Err() != nil {
				return
			}
			_ = c.manage(n.ServiceID, n.Hostname, true)
		}
	}()
}

// Wait waits for the startup queue to finish or stop (tests, shutdown).
func (c *CustomDomains) Wait() { c.wg.Wait() }
