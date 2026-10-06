package store

import (
	"context"

	"github.com/ankoehn/burrow/internal/db"
)

// RecordAttempts stores the attempt log of one gateway request.
func (s *Store) RecordAttempts(ctx context.Context, attempts []db.UsageAttempt) error {
	return s.q.InsertUsageAttempts(ctx, attempts)
}

// AttemptsForRequest returns the attempt log of one gateway request, by
// position (never nil).
func (s *Store) AttemptsForRequest(ctx context.Context, requestID string) ([]db.UsageAttempt, error) {
	return s.q.ListUsageAttempts(ctx, requestID)
}
