package view

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/client"
	"github.com/ankoehn/burrow/internal/proto"
)

// defaultWidth is used when the width of the terminal is not known.
const defaultWidth = 80

// defaultRows is used when the height of the terminal is not known: a taller
// view would scroll the terminal with every redraw.
const defaultRows = 24

const (
	sgrGreen  = "\x1b[32m"
	sgrYellow = "\x1b[33m"
	sgrRed    = "\x1b[31m"
	sgrReset  = "\x1b[0m"

	dotOn  = "●"
	dotOff = "○"
)

// row is one line of the view before colour is applied.
type row struct {
	text string
	warn bool
	// mark is the end of text that is coloured with markSGR: the status of a
	// request that went wrong.
	mark, markSGR string
}

// Render returns the lines of the view for a terminal of the given width; a
// width of 0 or less counts as 80. No line is longer than the width and none
// contains a control character, whatever the model holds: its texts partly
// come from the relay. With color, the state dot and the warning lines carry
// ANSI colour sequences; without, the result contains no escape character.
func Render(m Model, width int, color bool) []string {
	if width <= 0 {
		width = defaultWidth
	}
	// The last column stays free, so that a terminal never has a reason to wrap.
	w := width - 1
	if w < 1 {
		w = 1
	}

	rows := header(m, w)
	for _, s := range m.Services {
		rows = append(rows, row{})
		rows = append(rows, service(s, w)...)
	}
	// A note starts with "! " and its further lines are indented to its
	// text; a narrow terminal gets the smaller margin.
	first, next := "  ! ", "    "
	if w < 16 {
		first, next = "! ", "  "
	}
	for _, n := range m.Notes {
		if lines := wrap(clean(n), w-length(first)); len(lines) > 0 {
			rows = append(rows, row{})
			for i, l := range lines {
				prefix := next
				if i == 0 {
					prefix = first
				}
				rows = append(rows, row{text: prefix + l, warn: true})
			}
		}
	}
	if n := clean(m.Notice); n != "" {
		rows = append(rows, row{}, row{text: "  " + n})
	}

	out := make([]string, len(rows))
	for i, r := range rows {
		text := cut(r.text, w)
		switch {
		case !color:
		case r.warn:
			text = sgrYellow + text + sgrReset
		case r.mark != "" && strings.HasSuffix(text, r.mark):
			text = strings.TrimSuffix(text, r.mark) + r.markSGR + r.mark + sgrReset
		case i == 0 && m.State == client.StateConnected:
			text = strings.Replace(text, dotOn, sgrGreen+dotOn+sgrReset, 1)
		case i == 0:
			text = strings.Replace(text, dotOff, sgrYellow+dotOff+sgrReset, 1)
		}
		out[i] = text
	}
	return out
}

// Fit is Render for a terminal that is rows high: the view has to leave the
// line below it to the cursor, or redrawing in place would scroll. Recent
// lines are shortened first, then the view is cut at the bottom. rows of 0 or
// less means the height is not known; the view is then fitted to 24 rows.
func Fit(m Model, width, rows int, color bool) []string {
	lines := Render(m, width, color)
	if rows <= 0 {
		rows = defaultRows
	}
	limit := rows - 1
	if limit < 1 {
		limit = 1
	}
	for _, keep := range []int{5, 2, 0} {
		if len(lines) <= limit {
			return lines
		}
		short := m
		short.Services = make([]Service, len(m.Services))
		for i, s := range m.Services {
			if len(s.Recent) > keep {
				s.Recent = s.Recent[len(s.Recent)-keep:]
			}
			short.Services[i] = s
		}
		lines = Render(short, width, color)
	}
	if len(lines) > limit {
		lines = lines[:limit]
		if limit > 1 {
			lines[limit-1] = "  …"
		}
	}
	return lines
}

func header(m Model, w int) []row {
	dot, state, rest, alone := dotOff, "connecting", "", clean(m.Relay)
	switch m.State {
	case client.StateConnected:
		dot, state = dotOn, "connected"
	case client.StateReconnecting:
		state, alone = "reconnecting", clean(m.Detail)
	}
	base := state // without the delay
	switch m.State {
	case client.StateReconnecting:
		if m.RetryIn > 0 {
			// a started second counts: "in 0 s" would be wrong while it waits
			state += fmt.Sprintf(" in %d s", (m.RetryIn+time.Second-1)/time.Second)
		}
		if alone != "" {
			rest = " — " + alone
		}
	}
	if m.State != client.StateReconnecting && alone != "" {
		rest = " to " + alone
	}
	var right []string
	if v := clean(m.Version); v != "" {
		if v[0] >= '0' && v[0] <= '9' {
			v = "v" + v
		}
		right = append(right, v)
	}
	switch {
	case m.RTT <= 0:
	case m.RTT < time.Millisecond:
		right = append(right, "<1 ms")
	default:
		right = append(right, fmt.Sprintf("%d ms", m.RTT.Round(time.Millisecond)/time.Millisecond))
	}
	side := strings.Join(right, "   ")

	left := "burrow  " + dot + "  " + state
	one := left + rest
	if side != "" {
		one += "     " + side
	}
	if length(one) <= w {
		return []row{{text: one}}
	}
	var rows []row
	if length(left+rest) <= w {
		rows = append(rows, row{text: left + rest})
	} else {
		// Too narrow: the state first, in the longest form that fits, then
		// the relay or the error on a line of its own.
		first := dot + " " + base
		for _, form := range []string{left, dot + "  " + state} {
			if length(form) <= w {
				first = form
				break
			}
		}
		rows = append(rows, row{text: first})
		if alone != "" {
			rows = append(rows, row{text: place(2, alone, w)})
		}
	}
	if side != "" {
		rows = append(rows, row{text: "  " + side})
	}
	return rows
}

func service(s Service, w int) []row {
	name, public, local := clean(s.Name), clean(s.Public), clean(s.Local)
	if name == "" {
		name = "service"
	}
	if public == "" {
		public = "…" // the relay has not confirmed it yet
	}
	var rows []row
	indent := 2 + length(name) + 3
	if one := "  " + name + "   " + public + "  →  " + local; length(one) <= w {
		rows = append(rows, row{text: one})
	} else {
		// Too narrow for one line: one line each, and the public address may
		// start at the edge when that keeps it whole.
		indent = 2
		rows = append(rows, row{text: "  " + name})
		rows = append(rows, row{text: place(2, public, w)})
		rows = append(rows, row{text: "  → " + local})
	}

	counts := fmt.Sprintf("%d open, %d total", s.Open, s.Total)
	if access := accessLabel(clean(s.Access)); access != "" {
		access = "access: " + access
		remark := ""
		if s.Access == "open" && s.Public != GatewayOnlyPublic {
			remark = "(anyone with the URL)"
		}
		full := strings.TrimSpace(access + " " + remark)
		switch {
		case indent+length(full+"       "+counts) <= w:
			rows = append(rows, row{text: pad("", indent) + full + "       " + counts})
		case indent+length(full) <= w || remark == "":
			rows = append(rows, row{text: place(indent, full, w)}, row{text: place(indent, counts, w)})
		default:
			rows = append(rows, row{text: place(indent, access, w)}, row{text: place(indent, remark, w)}, row{text: place(indent, counts, w)})
		}
	} else {
		rows = append(rows, row{text: place(indent, counts, w)})
	}

	if len(s.Recent) > 0 {
		rows = append(rows, row{})
		methodCol, pathCol := 4, 14
		for _, l := range s.Recent {
			if l.Method == "" && l.Path == "" {
				continue
			}
			methodCol = max(methodCol, length(requestMethod(l)))
			pathCol = max(pathCol, length(requestPath(l)))
		}
		// One long path must not push every line out of its columns: the
		// column is no wider than the terminal leaves room for, and a path
		// that is longer is cut.
		if room := w - (2 + 8 + 2 + methodCol + 2) - (2 + 3); pathCol > room {
			pathCol = max(room, 14)
		}
		for _, l := range s.Recent {
			r := row{text: recent(l, methodCol, pathCol, w)}
			if l.Method != "" || l.Path != "" {
				switch {
				case l.Status >= 500:
					r.mark, r.markSGR = fmt.Sprintf("%d", l.Status), sgrRed
				case l.Status >= 400:
					r.mark, r.markSGR = fmt.Sprintf("%d", l.Status), sgrYellow
				}
			}
			rows = append(rows, r)
		}
	}
	if s.LocalDown {
		rows = append(rows, row{})
		if one := "  ! nothing is listening on " + local; length(one) <= w {
			rows = append(rows, row{text: one, warn: true})
		} else {
			rows = append(rows, row{text: "  ! no listener on", warn: true}, row{text: "    " + local, warn: true})
		}
	}
	return rows
}

// accessLabel names a relay access mode for a person. A mode without a name
// here is shown as the relay calls it.
func accessLabel(mode string) string {
	switch mode {
	case "burrow_login":
		return "Burrow login"
	case "api_key":
		return "API key"
	}
	return mode
}

// wrap breaks text into lines of at most n characters, at spaces. A word
// longer than a line (an address) continues on the next one instead of being
// cut, so that all of it stays readable.
func wrap(text string, n int) []string {
	if n < 1 {
		n = 1
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		for length(word) > n {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			r := []rune(word)
			lines = append(lines, string(r[:n]))
			word = string(r[n:])
		}
		switch {
		case line == "":
			line = word
		case length(line)+1+length(word) <= n:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// requestMethod and requestPath are the method and the path of a request line
// as they are drawn. Both are words of a visitor that came through the relay:
// whatever was done to them on the way, what a terminal would act on is
// replaced by "?" here.
func requestMethod(l Line) string { return clean(proto.SummaryMethod(l.Method)) }
func requestPath(l Line) string   { return clean(proto.SummaryPath(l.Path)) }

// recent renders one request or connection, in the widest form that fits.
func recent(l Line, methodCol, pathCol, w int) string {
	at := l.At.Format("15:04:05")
	method, path, ip := "", "", clean(l.SourceIP)
	if l.Method != "" || l.Path != "" {
		method, path = requestMethod(l), requestPath(l)
	}
	if method == "" && path == "" {
		if ip == "" {
			return "  " + at + "  connection"
		}
		for _, form := range []string{"  " + at + "  connection from " + ip, "  " + at + "  " + ip} {
			if length(form) <= w {
				return form
			}
		}
		return "  " + ip
	}
	status := ""
	if l.Status != 0 {
		status = fmt.Sprintf("%d", l.Status)
	}
	if status == "" {
		if full := "  " + at + "  " + pad(method, methodCol) + "  " + path; length(full) <= w {
			return full
		}
	} else if full := "  " + at + "  " + pad(method, methodCol) + "  " + pad(cut(path, pathCol), pathCol) + "  " + status; length(full) <= w {
		return full
	}
	// Narrow: the path gives way, the status stays; below 8 characters of
	// path the time goes too.
	tail := ""
	if status != "" {
		tail = " " + status
	}
	head := "  " + at + "  " + method + " "
	if w-length(head)-length(tail) < 8 {
		head = "  " + method + " "
	}
	return head + cut(path, w-length(head)-length(tail)) + tail
}

// place puts text at indent, or further left when it does not fit there: at
// the margin of two, or at the very edge to show as much of it as possible.
func place(indent int, text string, w int) string {
	switch {
	case indent+length(text) <= w:
		return pad("", indent) + text
	case 2+length(text) <= w:
		return "  " + text
	}
	return text
}

func length(s string) int { return utf8.RuneCountInString(s) }

// pad fills s with spaces to n characters.
func pad(s string, n int) string {
	if d := n - length(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// cut shortens s to at most n characters and marks the cut with "…". Every
// character counts as one column.
func cut(s string, n int) string {
	if length(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// clean drops what must not reach a terminal from a text of the model: control
// characters (ESC, CR, LF, the C1 range), formatting characters such as the
// ones that turn the writing direction, line and paragraph separators, and
// bytes that are not UTF-8.
func clean(s string) string {
	ok := true
	for _, r := range s {
		if drop(r) {
			ok = false
			break
		}
	}
	if ok {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if !drop(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func drop(r rune) bool {
	return r == utf8.RuneError || unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
}

var sgrRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// stripSGR removes the escape sequences this package writes.
func stripSGR(s string) string { return sgrRe.ReplaceAllString(s, "") }
