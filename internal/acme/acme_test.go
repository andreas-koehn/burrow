package acme

import (
	"context"
	"testing"
)

func TestNew_RequiresDomain(t *testing.T) {
	if _, err := New(context.Background(), Config{Email: "a@b.c", CA: "x", Storage: t.TempDir()}); err == nil {
		t.Fatal("expected error with no domains")
	}
}

func TestACMETLSProto_NonEmpty(t *testing.T) {
	if ACMETLSProto == "" {
		t.Fatal("ACMETLSProto must be the acme-tls/1 protocol string")
	}
}
