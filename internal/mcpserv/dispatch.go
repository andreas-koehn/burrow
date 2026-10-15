// dispatch.go — a tool call becomes a request to the relay's own router.
//
// invoke checks what can be checked without the API (the token may use the
// operation, the arguments fit its schema, path values stay one segment,
// dry_run, confirm) and then replays the call as an HTTP request with the
// caller's Authorization header. Authentication, the permission gate, the
// validation of the handler and the API's audit rows run as for a REST call.
// The answer becomes the tool result.

package mcpserv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ankoehn/burrow/internal/audit"
)

// MaxResultBytes bounds the JSON a tool result carries.
const MaxResultBytes = 65536

// maxCapturedBytes bounds what is read of the API's answer.
const maxCapturedBytes = 4 << 20

// maxErrorBodyBytes is how much of a non-JSON error answer a result quotes.
const maxErrorBodyBytes = 500

// apiBase is the scheme and host of the replayed requests. The routes do
// not look at the host; the name cannot resolve.
const apiBase = "http://burrow.invalid"

// dryRunNote is what a dry run result says about itself.
const dryRunNote = "Nothing was sent. The arguments and this token's permission were checked; conflicts the relay finds itself (a name already taken) show up only on the real call."

// invoke runs op for tok with args and returns the result and the audit
// result word (ok, denied, dry_run, error).
func (s *Server) invoke(ctx context.Context, tok *authedToken, op Operation, args map[string]any) (*mcp.CallToolResult, string) {
	if !tok.mayUse(op) {
		return errorResult(fmt.Errorf("unknown operation %s", op.ID)), "denied"
	}
	dryRun, _ := args["dry_run"].(bool)
	if err := s.checkArgs(op, args); err != nil {
		return errorResult(err), "error"
	}
	path, err := expandPath(op, args)
	if err != nil {
		return errorResult(err), "error"
	}
	query, err := queryOf(op, args)
	if err != nil {
		return errorResult(err), "error"
	}
	target := path
	if query != "" {
		target += "?" + query
	}
	var body []byte
	if b, ok := args["body"]; ok {
		if body, err = json.Marshal(b); err != nil {
			return errorResult(fmt.Errorf("body: %w", err)), "error"
		}
	}
	if dryRun {
		return jsonResult(map[string]any{
			"dry_run": true, "method": op.Method, "path": target, "body": json.RawMessage(orNull(body)),
			"note": dryRunNote,
		}), "dry_run"
	}
	if op.Destructive {
		want := op.ID
		if n := op.ConfirmValueName(); n != "" {
			want, _ = scalarString(args[n])
		}
		if got, _ := args["confirm"].(string); got != want {
			return errorResult(fmt.Errorf("confirm must be %q to %s; nothing was sent", want, strings.ToLower(op.Summary))), "error"
		}
	}
	return s.send(ctx, tok, op, target, body)
}

// checkArgs validates args against op's input schema. The schema's
// "confirm" is not required here: a dry run needs none, and a real call
// without the right one is refused afterwards with a sentence that names
// the value to confirm with (invoke).
func (s *Server) checkArgs(op Operation, args map[string]any) error {
	rs, err := s.validator(op)
	if err != nil {
		return fmt.Errorf("the schema of %s cannot be used: %w", op.ID, err)
	}
	if err := rs.Validate(args); err != nil {
		return argumentError(err)
	}
	return nil
}

// validator returns the resolved argument schema of op, built once.
func (s *Server) validator(op Operation) (*jsonschema.Resolved, error) {
	if v, ok := s.validators.Load(op.ID); ok {
		return v.(*jsonschema.Resolved), nil
	}
	schema := op.InputSchema()
	if req, ok := schema["required"].([]string); ok {
		kept := make([]string, 0, len(req))
		for _, name := range req {
			if name != "confirm" {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = kept
		}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	var js jsonschema.Schema
	if err := json.Unmarshal(raw, &js); err != nil {
		return nil, err
	}
	rs, err := js.Resolve(nil)
	if err != nil {
		return nil, err
	}
	v, _ := s.validators.LoadOrStore(op.ID, rs)
	return v.(*jsonschema.Resolved), nil
}

// argumentError turns the validator's error into one sentence that names
// the argument. The validator wraps its finding in one "validating <schema
// path>: " per level; the innermost path says which argument it is.
func argumentError(err error) error {
	msg := err.Error()
	where := ""
	for {
		rest, ok := strings.CutPrefix(msg, "validating ")
		if !ok {
			break
		}
		ptr, tail, found := strings.Cut(rest, ": ")
		if !found {
			break
		}
		where, msg = ptr, tail
	}
	arg := argumentPath(where)
	if arg == "" {
		return fmt.Errorf("invalid arguments: %s", msg)
	}
	return fmt.Errorf("invalid argument %s: %s", arg, msg)
}

// argumentPath turns a schema path ("/properties/body/properties/name") into
// the argument it describes ("body.name"); "" for the arguments themselves.
func argumentPath(ptr string) string {
	var parts []string
	segs := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	for i := 0; i < len(segs); i++ {
		switch segs[i] {
		case "properties", "additionalProperties":
			if segs[i] == "properties" && i+1 < len(segs) {
				parts = append(parts, segs[i+1])
				i++
			}
		case "items":
			if len(parts) > 0 {
				parts[len(parts)-1] += "[]"
			}
		}
	}
	return strings.Join(parts, ".")
}

// expandPath fills op's path template with the path arguments, each one
// escaped into a single segment.
func expandPath(op Operation, args map[string]any) (string, error) {
	var b strings.Builder
	rest := op.Path
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			break
		}
		end := strings.IndexByte(rest[open:], '}')
		if end < 0 {
			return "", fmt.Errorf("the path of %s is malformed", op.ID)
		}
		name := rest[open+1 : open+end]
		v, ok := scalarString(args[name])
		if !ok {
			return "", fmt.Errorf("path value of %s must be a string or a number", name)
		}
		if v == "" || v == "." || v == ".." {
			return "", fmt.Errorf("path value of %s cannot be %q", name, v)
		}
		b.WriteString(rest[:open])
		b.WriteString(url.PathEscape(v))
		rest = rest[open+end+1:]
	}
	return b.String(), nil
}

// queryOf encodes the query arguments of op that args holds.
func queryOf(op Operation, args map[string]any) (string, error) {
	q := url.Values{}
	for _, p := range op.Params {
		if p.In != "query" {
			continue
		}
		v, present := args[p.Name]
		if !present || v == nil {
			continue
		}
		if list, ok := v.([]any); ok {
			for _, item := range list {
				s, ok := scalarString(item)
				if !ok {
					return "", fmt.Errorf("query value of %s must be a string, a number, a boolean or a list of them", p.Name)
				}
				q.Add(p.Name, s)
			}
			continue
		}
		s, ok := scalarString(v)
		if !ok {
			return "", fmt.Errorf("query value of %s must be a string, a number, a boolean or a list of them", p.Name)
		}
		q.Set(p.Name, s)
	}
	return q.Encode(), nil
}

// scalarString renders a string, number or boolean argument as text; a
// number without an exponent.
func scalarString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case bool:
		return strconv.FormatBool(x), true
	default:
		return "", false
	}
}

// send replays the call through the relay's router and turns the answer
// into the result.
func (s *Server) send(ctx context.Context, tok *authedToken, op Operation, target string, body []byte) (*mcp.CallToolResult, string) {
	h := s.api.Load()
	if h == nil || *h == nil {
		return errorResult(errors.New("the relay is starting; try again")), "error"
	}
	// The request is routed afresh: the route context of the /mcp/burrow
	// request must not reach the router, which would take it for the
	// context of a mounted sub-router.
	ctx = context.WithValue(ctx, chi.RouteCtxKey, nil)
	req, err := http.NewRequestWithContext(ctx, op.Method, apiBase+target, bytes.NewReader(body))
	if err != nil {
		return errorResult(fmt.Errorf("the request cannot be built: %w", err)), "error"
	}
	req.Header.Set("Authorization", tok.bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	} else {
		req.Body = http.NoBody
		req.ContentLength = 0
	}
	if tok.userAgent != "" {
		req.Header.Set("User-Agent", tok.userAgent)
	}
	if id := audit.LogContextFrom(ctx).RequestID; id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	req.RemoteAddr = tok.remoteAddr
	// The address the MCP client used: handlers that build links from
	// r.Host must not see the placeholder of apiBase.
	if tok.host != "" {
		req.Host = tok.host
	}
	w := &capture{header: http.Header{}}
	(*h).ServeHTTP(w, req)
	if w.over {
		return errorResult(fmt.Errorf("the answer is larger than 4 MiB; narrow it with the parameters of %s", op.ID)), "error"
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	return resultOf(op, status, w.header, w.body.Bytes())
}

// capture is the ResponseWriter of a replayed request: it keeps the status,
// the headers and at most maxCapturedBytes+1 bytes of the body.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
	over   bool
}

func (c *capture) Header() http.Header { return c.header }

func (c *capture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *capture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if room := maxCapturedBytes + 1 - c.body.Len(); room > 0 {
		if len(p) > room {
			c.body.Write(p[:room])
		} else {
			c.body.Write(p)
		}
	}
	if c.body.Len() > maxCapturedBytes {
		c.over = true
	}
	return len(p), nil
}

// resultOf turns the API's answer into the tool result and the audit word.
func resultOf(op Operation, status int, header http.Header, body []byte) (*mcp.CallToolResult, string) {
	if status < 200 || status > 299 {
		word := "error"
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			word = "denied"
		}
		return errorResult(apiError(status, body)), word
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return jsonResult(map[string]any{"status": status}), "ok"
	}
	ct := header.Get("Content-Type")
	if !strings.Contains(ct, "json") || !json.Valid(trimmed) {
		return jsonResult(cutText(map[string]any{"content_type": ct}, string(body), op)), "ok"
	}
	switch trimmed[0] {
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return errorResult(fmt.Errorf("the answer cannot be read: %w", err)), "error"
		}
		return jsonResult(cutList(op, items)), "ok"
	case '{':
		var compact bytes.Buffer
		if err := json.Compact(&compact, trimmed); err != nil {
			return errorResult(fmt.Errorf("the answer cannot be read: %w", err)), "error"
		}
		if compact.Len() <= MaxResultBytes {
			return toolResult(compact.Bytes()), "ok"
		}
		return jsonResult(cutText(map[string]any{}, compact.String(), op)), "ok"
	default:
		// A JSON string, number, boolean or null: structuredContent must be
		// an object.
		return jsonResult(cutText(map[string]any{}, string(trimmed), op)), "ok"
	}
}

// apiError is the text of a refused call: the status and the API's error
// message.
func apiError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		msg = e.Error
	} else {
		msg = strings.TrimSpace(validPrefix(string(body), maxErrorBodyBytes))
	}
	if msg == "" {
		return fmt.Errorf("%d %s", status, http.StatusText(status))
	}
	return fmt.Errorf("%d %s: %s", status, http.StatusText(status), msg)
}

// cutText puts text into fields under "text", cut so that the JSON stays
// within MaxResultBytes; a cut result says so.
func cutText(fields map[string]any, text string, op Operation) map[string]any {
	fields["text"] = text
	if b, err := json.Marshal(fields); err == nil && len(b) <= MaxResultBytes {
		return fields
	}
	fields["truncated"] = true
	fields["note"] = "The answer was cut to fit. " + narrowNote(op)
	lo, hi := 0, len(text)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		fields["text"] = validPrefix(text, mid)
		if b, err := json.Marshal(fields); err == nil && len(b) <= MaxResultBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	fields["text"] = validPrefix(text, lo)
	return fields
}

// cutList wraps items as {"items": …}. A list too large for MaxResultBytes
// keeps as many leading items as fit and says how many there were and how
// to narrow the list.
func cutList(op Operation, items []json.RawMessage) map[string]any {
	if items == nil {
		items = []json.RawMessage{}
	}
	whole := map[string]any{"items": items}
	if b, err := json.Marshal(whole); err == nil && len(b) <= MaxResultBytes {
		return whole
	}
	cut := func(n int) map[string]any {
		return map[string]any{
			"items": items[:n], "truncated": true, "total": len(items),
			"note": fmt.Sprintf("Showing %d of %d. %s", n, len(items), narrowNote(op)),
		}
	}
	lo, hi := 0, len(items)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if b, err := json.Marshal(cut(mid)); err == nil && len(b) <= MaxResultBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return cut(lo)
}

// narrowNote names the query parameters of op, which filter or page what it
// returns.
func narrowNote(op Operation) string {
	var names []string
	for _, p := range op.Params {
		if p.In == "query" {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return "This operation has no filters."
	}
	return "Narrow the list with: " + strings.Join(names, ", ") + "."
}

// validPrefix is at most n bytes of s, ending between two characters.
func validPrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// orNull is b, or the JSON null when there is no body.
func orNull(b []byte) []byte {
	if b == nil {
		return []byte("null")
	}
	return b
}

// jsonResult marshals v and renders it as a tool result.
func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult(fmt.Errorf("the result cannot be rendered: %w", err))
	}
	return toolResult(b)
}
