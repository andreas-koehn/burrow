package mcpserv

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
	"github.com/ankoehn/burrow/internal/store"
)

// loggableSentinels are the package-level errors of internal/store and
// internal/db that a tool's store calls can return. Their text is fixed and
// cannot carry an argument value, so a log line may show it.
var loggableSentinels = []error{
	db.ErrNotFound,
	db.ErrServiceNameReserved,
	store.ErrUnauthorized,
	store.ErrForbidden,
	store.ErrNameRequired,
	store.ErrTokenNameRequired,
	store.ErrPermissionNotInRole,
	store.ErrInvalidAccessMode,
	store.ErrServiceNotHTTP,
	store.ErrDirectService,
	store.ErrMTLSCARequired,
	store.ErrInvalidMTLSCAPEM,
	store.ErrLoginTokenName,
	store.ErrUnknownRole,
	errStoreUnavailable,
}

// errAttrs describes a tool failure for the log without quoting its text,
// which may hold argument values (a duplicate-key detail):
//   - err_chain: the types along errors.Unwrap, outermost first;
//   - err_text: the text of a sentinel of loggableSentinels the error is;
//   - err_code: the code of a database driver error in the chain. Matched by
//     method, so no driver package is imported: a Code() int (modernc sqlite's
//     *sqlite.Error) or a SQLState() string (pgx's *pgconn.PgError).
func errAttrs(err error) []any {
	var chain []string
	var attrs []any
	for e := err; e != nil; e = errors.Unwrap(e) {
		chain = append(chain, fmt.Sprintf("%T", e))
		switch c := e.(type) {
		case interface{ SQLState() string }:
			attrs = append(attrs, "err_code", "sqlstate "+c.SQLState())
		case interface{ Code() int }:
			attrs = append(attrs, "err_code", fmt.Sprintf("sqlite %d", c.Code()))
		}
	}
	attrs = append([]any{"err_chain", strings.Join(chain, " > ")}, attrs...)
	for _, sent := range loggableSentinels {
		if errors.Is(err, sent) {
			attrs = append(attrs, "err_text", sent.Error())
			break
		}
	}
	return attrs
}
