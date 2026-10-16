package aigateway

import (
	"bufio"
	"bytes"
	"errors"
	"github.com/ankoehn/burrow/internal/aigw"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"strings"
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
	return b.model, func(newModel string) ([]byte, error) { return b.WithModel(newModel), nil }, nil
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
	return &requestBody{raw: raw, model: string(raw[start:end]), keys: 1, start: start, end: end, multipart: true}, nil
}

// locateFormModel finds the bytes of the value of the form field "model". It
// walks the delimiters "--boundary" of RFC 2046; a part's content ends at the
// next "\r\n--boundary", so a binary content that holds "\r\n--" followed by
// anything but the boundary is passed over.
func locateFormModel(raw []byte, boundary string) (start, end int, err error) {
	delim := []byte("--" + boundary)
	found := false
	pos := bytes.Index(raw, delim)
	// The first delimiter is at the start of a line.
	for pos >= 0 && pos > 0 && !bytes.HasSuffix(raw[:pos], []byte("\r\n")) {
		next := bytes.Index(raw[pos+len(delim):], delim)
		if next < 0 {
			pos = -1
			break
		}
		pos += len(delim) + next
	}
	for pos >= 0 {
		at := pos + len(delim)
		if bytes.HasPrefix(raw[at:], []byte("--")) {
			break // the closing delimiter
		}
		lineEnd := bytes.Index(raw[at:], []byte("\r\n")) // transport padding is skipped
		if lineEnd < 0 {
			break
		}
		hdrStart := at + lineEnd + 2
		var contentStart int
		var hdr textproto.MIMEHeader
		if bytes.HasPrefix(raw[hdrStart:], []byte("\r\n")) { // a part without headers
			contentStart = hdrStart + 2
		} else {
			hdrEnd := bytes.Index(raw[hdrStart:], []byte("\r\n\r\n"))
			if hdrEnd < 0 {
				break
			}
			tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw[hdrStart : hdrStart+hdrEnd+4])))
			hdr, _ = tp.ReadMIMEHeader()
			contentStart = hdrStart + hdrEnd + 4
		}
		stop := bytes.Index(raw[contentStart:], append([]byte("\r\n"), delim...))
		if stop < 0 {
			break // no closing delimiter: the form is cut
		}
		contentEnd := contentStart + stop
		if _, p, perr := mime.ParseMediaType(hdr.Get("Content-Disposition")); perr == nil && p["name"] == "model" && p["filename"] == "" {
			if found {
				return 0, 0, errDuplicateModel
			}
			found, start, end = true, contentStart, contentEnd
		}
		pos = contentEnd + 2
	}
	if !found || start == end {
		return 0, 0, errNoModelField
	}
	return start, end, nil
}
