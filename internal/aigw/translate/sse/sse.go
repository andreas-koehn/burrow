// Package sse reads and writes server-sent events for the format
// translators.
//
// The parser is fed bytes as they arrive and holds at most one frame: the
// bytes of the line being read plus the event name and data of the frame
// being built are counted against a limit the caller chooses, and going over
// it is an error (ErrTooLarge), never a truncated frame and never a buffer
// that keeps growing. Nothing here starts a goroutine or keeps a queue: a
// caller gets the frames of the bytes it just fed, hands them on, and feeds
// again, so a slow writer slows the reader down.
package sse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
)

// DefaultMaxFrame is the limit NewParser uses when it is given none: 1 MiB
// for one frame.
const DefaultMaxFrame = 1 << 20

// readChunk is the size of the buffer Read reads the stream with.
const readChunk = 32 << 10

var (
	// ErrTooLarge reports a frame (or a single line) over the parser's limit.
	// The parser is unusable afterwards: every later Feed returns it again.
	ErrTooLarge = errors.New("sse: frame too large")
	// ErrBadFrame reports a frame Write cannot put on the wire: a line break
	// in the event name, or a carriage return in the data (a reader would
	// take it for a line break and the data would not come back the same).
	ErrBadFrame = errors.New("sse: frame cannot be written")
)

var bom = []byte{0xEF, 0xBB, 0xBF}

// Frame is one server-sent event.
type Frame struct {
	Event string // "" when the frame has no event line
	Data  []byte // data lines joined by "\n"; never nil, owned by the receiver
}

// Parser splits a byte stream into frames. It is fed as bytes arrive, so it
// can sit inside an http.ResponseWriter; it bounds the size of one frame.
//
// Lines end with "\n", "\r\n" or "\r". A frame ends at a blank line and is
// delivered when it had at least one data line; comment lines (": ping"),
// the fields "id" and "retry", and lines that are no field at all (a provider
// that answers with bare JSON) are skipped. A Parser is not safe for use by
// several goroutines.
type Parser struct {
	max     int
	line    []byte // the bytes of the line being read
	data    []byte // the data of the frame being built
	event   string
	hasData bool
	skipLF  bool // the last byte was "\r": a "\n" that follows belongs to it
	started bool // the first line was seen (a byte-order mark is cut from it)
	err     error
}

// NewParser returns a parser whose frames may hold up to maxFrame bytes
// (line being read + event name + data). maxFrame <= 0 means DefaultMaxFrame.
func NewParser(maxFrame int) *Parser {
	if maxFrame <= 0 {
		maxFrame = DefaultMaxFrame
	}
	return &Parser{max: maxFrame}
}

// Feed takes the next bytes of the stream and returns the frames they
// complete, in order. It does not keep b. When a frame goes over the limit
// it returns the frames completed before it together with ErrTooLarge.
func (p *Parser) Feed(b []byte) ([]Frame, error) {
	if p.err != nil {
		return nil, p.err
	}
	var out []Frame
	for len(b) > 0 {
		if p.skipLF {
			p.skipLF = false
			if b[0] == '\n' {
				b = b[1:]
				continue
			}
		}
		i := bytes.IndexAny(b, "\r\n")
		if i < 0 {
			if !p.fits(len(b)) {
				return out, p.fail()
			}
			p.line = append(p.line, b...)
			break
		}
		if !p.fits(i) {
			return out, p.fail()
		}
		line := b[:i]
		if len(p.line) > 0 {
			p.line = append(p.line, line...)
			line = p.line
		}
		p.skipLF = b[i] == '\r'
		b = b[i+1:]
		if f, ok := p.endLine(line); ok {
			out = append(out, f)
		}
		p.line = p.line[:0]
	}
	return out, nil
}

// Flush returns the final frame when the stream ended without the blank line
// that closes it, and nothing otherwise. The parser is empty afterwards.
func (p *Parser) Flush() []Frame {
	if p.err != nil {
		return nil
	}
	var out []Frame
	if len(p.line) > 0 {
		if f, ok := p.endLine(p.line); ok {
			out = append(out, f)
		}
	}
	if f, ok := p.endLine(nil); ok {
		out = append(out, f)
	}
	p.line, p.data, p.event, p.hasData, p.skipLF = nil, nil, "", false, false
	return out
}

// fits reports whether n more bytes of the current line stay within the limit.
func (p *Parser) fits(n int) bool {
	return len(p.line)+len(p.data)+len(p.event)+n <= p.max
}

func (p *Parser) fail() error {
	p.err = ErrTooLarge
	p.line, p.data, p.event, p.hasData = nil, nil, "", false
	return p.err
}

// endLine handles one complete line (without its line ending). The caller
// has checked that the line fits; a field's value is never longer than its
// line, so what is kept here fits as well.
func (p *Parser) endLine(line []byte) (Frame, bool) {
	if !p.started {
		p.started = true
		line = bytes.TrimPrefix(line, bom)
	}
	if len(line) == 0 {
		f := Frame{Event: p.event, Data: p.data}
		ok := p.hasData
		p.data, p.event, p.hasData = nil, "", false
		if !ok {
			return Frame{}, false
		}
		if f.Data == nil {
			f.Data = []byte{}
		}
		return f, true
	}
	if line[0] == ':' {
		return Frame{}, false
	}
	name, value, _ := bytes.Cut(line, []byte{':'})
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	switch string(name) {
	case "data":
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	case "event":
		p.event = string(value)
	}
	return Frame{}, false
}

// Write writes one frame with a single call to w.Write, so the caller can
// flush after it; event may be "". Data is split into one data line per
// "\n".
func Write(w io.Writer, event string, data []byte) error {
	if strings.ContainsAny(event, "\r\n") || bytes.IndexByte(data, '\r') >= 0 {
		return ErrBadFrame
	}
	buf := make([]byte, 0, len(event)+len(data)+32)
	if event != "" {
		buf = append(buf, "event: "...)
		buf = append(buf, event...)
		buf = append(buf, '\n')
	}
	for {
		line, rest, more := bytes.Cut(data, []byte{'\n'})
		buf = append(buf, "data: "...)
		buf = append(buf, line...)
		buf = append(buf, '\n')
		if !more {
			break
		}
		data = rest
	}
	buf = append(buf, '\n')
	_, err := w.Write(buf)
	return err
}

// Read drives a parser over r and calls fn for each frame, in order, on the
// caller's goroutine: the next bytes are read only after fn returned, so fn
// (which usually writes to the client) sets the pace. It returns nil at the
// end of the stream, after delivering a final frame that lacks its blank
// line; fn's error when fn fails; ctx's error when ctx ended; ErrTooLarge;
// or the read error.
//
// ctx is looked at between reads and between frames. A read that blocks is
// ended by closing r, which is what an HTTP transport does when the request's
// context ends. Read starts no goroutine and holds nothing after it returns.
func Read(ctx context.Context, r io.Reader, maxFrame int, fn func(Frame) error) error {
	p := NewParser(maxFrame)
	buf := make([]byte, readChunk)
	deliver := func(frames []Frame) error {
		for _, f := range frames {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := fn(f); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := r.Read(buf)
		frames, perr := p.Feed(buf[:n])
		if err := deliver(frames); err != nil {
			return err
		}
		if perr != nil {
			return perr
		}
		if rerr == io.EOF {
			return deliver(p.Flush())
		}
		if rerr != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return rerr
		}
	}
}
