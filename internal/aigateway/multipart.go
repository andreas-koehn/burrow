package aigateway

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"

	"github.com/ankoehn/burrow/internal/aigw"
)

var (
	// errNoModelField: the form has no "model" field, or its value is empty.
	errNoModelField = errors.New("aigateway: multipart form has no model field")
	// errNotMultipart: the Content-Type is not multipart/form-data with a boundary.
	errNotMultipart = errors.New("aigateway: body is not multipart/form-data")
)

// isMultipartPath reports whether the endpoint at path takes a
// multipart/form-data body (an upload) instead of JSON.
func isMultipartPath(path string) bool {
	switch strings.TrimRight(path, "/") {
	case "/v1/audio/transcriptions", "/v1/audio/translations":
		return true
	}
	return false
}

// isMediaPath reports whether path is one of the five endpoints that are never
// cached, get no system text and are not translated.
func isMediaPath(path string) bool { return aigw.IsMediaPath(path) }

// readMultipartModel reads at most limit bytes of a multipart/form-data body,
// returns the value of its "model" field and a rewriter that returns the body
// with that field's value replaced. Every other byte of the body, the boundary
// and the other parts included, is returned as it was read. Errors:
// errNotMultipart, errBodyTooLarge, errNoModelField (also for an empty value)
// and errDuplicateModel (two "model" fields).
func readMultipartModel(r *http.Request, limit int64) (model string, rewrite func(newModel string) ([]byte, error), err error) {
	b, err := readMultipartBody(r, limit)
	if err != nil {
		return "", nil, err
	}
	return b.model, func(newModel string) ([]byte, error) {
		out := b.WithModel(newModel)
		// What goes upstream must read back as the one model it was given.
		if err := verifyFormModel(out, b.boundary, newModel); err != nil {
			return nil, err
		}
		return out, nil
	}, nil
}

// readMultipartBody is readMultipartModel as a requestBody, so that the
// gateway and the failover handler treat both body kinds alike.
func readMultipartBody(r *http.Request, limit int64) (*requestBody, error) {
	mt, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if perr != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return nil, errNotMultipart
	}
	// The body limit bounds what is held in memory: one byte more is enough
	// to know the body is over it.
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyTooLarge
	}
	start, end, err := locateFormModel(raw, params["boundary"])
	if err != nil {
		return nil, err
	}
	// The model checked here is the model a lenient reader finds.
	if err := verifyFormModel(raw, params["boundary"], string(raw[start:end])); err != nil {
		return nil, err
	}
	return &requestBody{raw: raw, model: string(raw[start:end]), keys: 1, start: start, end: end, multipart: true, boundary: params["boundary"]}, nil
}

// errMalformedForm: the form is not strict RFC 2046 (CRLF line endings, each
// delimiter alone on its line). A reader that is more lenient could read
// another "model" than the one checked here, so such a form is refused.
var errMalformedForm = errors.New("aigateway: multipart form is malformed")

// afterDelimiter says what follows a delimiter at raw[at:]: closing for "--",
// a line for optional linear whitespace and CRLF (next is the index after the
// CRLF), and neither for anything else, an LF-only line end included.
func afterDelimiter(raw []byte, at int) (closing bool, next int, ok bool) {
	if bytes.HasPrefix(raw[at:], []byte("--")) {
		return true, at + 2, true
	}
	for at < len(raw) && (raw[at] == ' ' || raw[at] == '\t') {
		at++
	}
	if bytes.HasPrefix(raw[at:], []byte("\r\n")) {
		return false, at + 2, true
	}
	return false, 0, false
}

// locateFormModel finds the bytes of the value of the form field "model". It
// walks the delimiters "--boundary" of RFC 2046 strictly: the first delimiter
// opens the body or follows a CRLF, each delimiter is followed by optional
// whitespace and CRLF (or by "--" at the end), a part's content ends at the
// next "\r\n--boundary" that is a delimiter in that sense. A binary content
// that holds "\r\n--" followed by anything else is passed over; a line end
// that is a bare LF is errMalformedForm.
func locateFormModel(raw []byte, boundary string) (start, end int, err error) {
	delim := []byte("--" + boundary)
	crlfDelim := append([]byte("\r\n"), delim...)
	// The opening delimiter: at the start, or after a preamble line.
	pos := -1
	if bytes.HasPrefix(raw, delim) {
		pos = 0
	} else if i := bytes.Index(raw, crlfDelim); i >= 0 {
		pos = i + 2
	}
	// Anything but a proper delimiter line after it is malformed.
	found := false
	for pos >= 0 {
		closing, next, ok := afterDelimiter(raw, pos+len(delim))
		if !ok {
			return 0, 0, errMalformedForm
		}
		if closing {
			break
		}
		hdrStart := next
		var contentStart int
		var hdr textproto.MIMEHeader
		if bytes.HasPrefix(raw[hdrStart:], []byte("\r\n")) { // a part without headers
			contentStart = hdrStart + 2
		} else {
			hdrEnd := bytes.Index(raw[hdrStart:], []byte("\r\n\r\n"))
			if hdrEnd < 0 {
				return 0, 0, errMalformedForm
			}
			tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw[hdrStart : hdrStart+hdrEnd+4])))
			hdr, _ = tp.ReadMIMEHeader()
			contentStart = hdrStart + hdrEnd + 4
		}
		// The end of the content: the next real delimiter.
		contentEnd, nextPos := -1, -1
		for from := contentStart; ; {
			i := bytes.Index(raw[from:], crlfDelim)
			if i < 0 {
				return 0, 0, errMalformedForm // no closing delimiter: the form is cut
			}
			at := from + i
			rest := at + len(crlfDelim)
			if bytes.HasPrefix(raw[rest:], []byte("--")) {
				contentEnd, nextPos = at, at+2
				break
			}
			if _, _, ok := afterDelimiter(raw, rest); ok {
				contentEnd, nextPos = at, at+2
				break
			}
			// Whitespace then a bare LF is a delimiter to a lenient reader.
			j := rest
			for j < len(raw) && (raw[j] == ' ' || raw[j] == '\t') {
				j++
			}
			if j < len(raw) && raw[j] == '\n' {
				return 0, 0, errMalformedForm
			}
			from = at + 2 // "--boundaryXYZ": content, not a delimiter
		}
		if _, p, perr := mime.ParseMediaType(hdr.Get("Content-Disposition")); perr == nil && p["name"] == "model" && p["filename"] == "" {
			if found {
				return 0, 0, errDuplicateModel
			}
			found, start, end = true, contentStart, contentEnd
		}
		pos = nextPos
	}
	if !found || start == end {
		return 0, 0, errNoModelField
	}
	return start, end, nil
}

// verifyFormModel reads body back with Go's mime/multipart, the reader most
// upstreams use, and refuses unless the form holds exactly one non-file
// "model" field and it equals want. It is the check that the gateway and the
// upstream see the same model.
func verifyFormModel(body []byte, boundary, want string) error {
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	seen := 0
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errMalformedForm
		}
		if part.FormName() == "model" && part.FileName() == "" {
			v, err := io.ReadAll(part)
			if err != nil {
				return errMalformedForm
			}
			seen++
			if string(v) != want {
				return errMalformedForm
			}
		}
	}
	if seen != 1 {
		return errMalformedForm
	}
	return nil
}
