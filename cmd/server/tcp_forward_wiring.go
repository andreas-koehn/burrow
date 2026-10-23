package main

// tcp_forward_wiring.go — who may forward to a tcp service (native tcp, P04):
// the relay asks forwardAuthorizer on every forward open.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/server"
)

// forwardDB is what forwardAuthorizer reads. *db.DB satisfies it.
type forwardDB interface {
	GetUserByID(ctx context.Context, id string) (db.User, error)
	GetServiceByID(ctx context.Context, id string) (db.Service, error)
	ListServicesByUser(ctx context.Context, userID string) ([]db.Service, error)
	GetServiceBySubdomain(ctx context.Context, sub string) (db.Service, error)
	GetAccessPolicy(ctx context.Context, serviceID string) ([]string, error)
}

// forwardAuthorizer implements server.ForwardAuthorizer over the store.
//
// A target is, in this order: a service id; the name of one of the caller's
// own tcp services; a slug (global). Only tcp services are found. The owner
// may always forward; another user when their role is in the service's
// access policy, which for a tcp service is the forward policy (empty: the
// owner alone). A suspended user reaches nothing. Every read is made on every
// open, so a change of role or policy applies to the next stream.
//
// A target that does not exist reads a policy too (of no service) before it
// is refused, so that an absent service and a denied one cost the same: the
// answer is not_found for both, and its timing must not tell them apart.
// The owner's own target reads none; that is no refusal.
type forwardAuthorizer struct{ db forwardDB }

func (a forwardAuthorizer) AuthorizeForward(ctx context.Context, userID, target string) (server.ForwardTarget, error) {
	u, err := a.db.GetUserByID(ctx, userID)
	if errors.Is(err, db.ErrNotFound) {
		return server.ForwardTarget{}, server.ErrForwardNotFound
	}
	if err != nil {
		return server.ForwardTarget{}, fmt.Errorf("forward: user: %w", err)
	}
	if u.Status == "suspended" {
		return server.ForwardTarget{}, server.ErrForwardNotFound
	}
	svc, found, err := a.find(ctx, userID, target)
	if err != nil {
		return server.ForwardTarget{}, err
	}
	if !found {
		if _, err := a.db.GetAccessPolicy(ctx, noService); err != nil {
			return server.ForwardTarget{}, fmt.Errorf("forward: access policy: %w", err)
		}
		return server.ForwardTarget{}, server.ErrForwardNotFound
	}
	t := server.ForwardTarget{ServiceID: svc.ID, Name: svc.Name, Owner: svc.UserID == userID}
	if t.Owner {
		return t, nil
	}
	roles, err := a.db.GetAccessPolicy(ctx, svc.ID)
	if err != nil {
		return server.ForwardTarget{}, fmt.Errorf("forward: access policy: %w", err)
	}
	for _, r := range roles {
		if r == u.Role {
			return t, nil
		}
	}
	return t, server.ErrForwardDenied
}

// noService is the id whose policy an absent target reads: no service has
// it, so the read finds no row, as for a service with an empty policy.
const noService = "00000000-0000-0000-0000-000000000000"

// find resolves target to a tcp service.
func (a forwardAuthorizer) find(ctx context.Context, userID, target string) (db.Service, bool, error) {
	tcp := func(s db.Service, err error) (db.Service, bool, error) {
		if errors.Is(err, db.ErrNotFound) {
			return db.Service{}, false, nil
		}
		if err != nil {
			return db.Service{}, false, fmt.Errorf("forward: service: %w", err)
		}
		return s, s.Type == "tcp", nil
	}
	if _, perr := uuid.Parse(target); perr == nil {
		if s, ok, err := tcp(a.db.GetServiceByID(ctx, target)); err != nil || ok {
			return s, ok, err
		}
	}
	own, err := a.db.ListServicesByUser(ctx, userID)
	if err != nil {
		return db.Service{}, false, fmt.Errorf("forward: services: %w", err)
	}
	for _, s := range own {
		if s.Name == target && s.Type == "tcp" {
			return s, true, nil
		}
	}
	return tcp(a.db.GetServiceBySubdomain(ctx, target))
}
