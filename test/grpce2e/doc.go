// Package grpce2e drives gRPC through real burrowd and burrow binaries.
//
// It is a module of its own so that google.golang.org/grpc never enters the
// main module (spec 2026-10-25, Testing, e2e). Everything lives in the tests:
//
//	cd test/grpce2e && go test -count=1 ./...
//
// builds bin/burrowd and bin/burrow from the repository (../..) into a temp
// directory, unless BURROW_E2E_BURROWD and BURROW_E2E_BURROW name binaries
// to use. Two relays run on 127.0.0.1 with SQLite in a temp directory: one
// with a plain dashboard listener (h2c, as behind a TLS-terminating proxy)
// and one that terminates TLS itself with the 443 mux on. BURROW_E2E_KEEP=1
// keeps the temp directory with the relay and client logs.
package grpce2e
