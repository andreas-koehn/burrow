package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ankoehn/burrow/internal/db"
)

// AliasStore looks up model aliases by name, best priority first.
type AliasStore interface {
	GetAliasesByPriority(ctx context.Context, alias string) ([]db.ModelAlias, error)
}

// maxAliasBody bounds how much of a request body is buffered to look for an
// alias. Larger bodies are forwarded as they are.
const maxAliasBody = 4 << 20

// rewriteModelAlias replaces the "model" field of a JSON request body when it
// names an alias defined for serviceID. Anything it cannot parse is forwarded
// unchanged: an alias is a convenience, never a reason to fail a request.
func rewriteModelAlias(r *http.Request, serviceID string, aliases AliasStore) error {
	if aliases == nil || r.Method != http.MethodPost || r.Body == nil ||
		!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, maxAliasBody+1))
	if err != nil {
		return err
	}
	if len(head) > maxAliasBody {
		// Too large to inspect: stitch the read part back in front of the rest.
		r.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), r.Body), r.Body}
		return nil
	}
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}

	var fields map[string]json.RawMessage
	var model string
	if json.Unmarshal(head, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
		restore(head)
		return nil
	}
	rows, err := aliases.GetAliasesByPriority(r.Context(), model)
	if err != nil {
		restore(head)
		return err
	}
	for _, a := range rows {
		if a.ServiceID != serviceID || a.ConcreteModel == "" {
			continue
		}
		concrete, _ := json.Marshal(a.ConcreteModel)
		fields["model"] = concrete
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false) // keep prompt text byte-for-byte readable
		if err := enc.Encode(fields); err != nil {
			break
		}
		restore(bytes.TrimRight(out.Bytes(), "\n"))
		return nil
	}
	restore(head)
	return nil
}
