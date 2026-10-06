package view

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ankoehn/burrow/internal/client"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

func at(h, m, s int) time.Time { return time.Date(2026, 10, 5, h, m, s, 0, time.UTC) }

func httpService() Service {
	return Service{
		Name: "my-app", Type: "http", Public: "https://burrow.example.com/svc/p7baeh/", Local: "127.0.0.1:3000",
		Open: 3, Total: 41,
		Recent: []Line{
			{At: at(14, 2, 11), Method: "GET", Path: "/api/users", Status: 200},
			{At: at(14, 2, 12), Method: "POST", Path: "/api/login", Status: 401},
		},
	}
}

func tcpService() Service {
	return Service{
		Name: "pg", Type: "tcp", Public: "burrow.example.com:9000", Local: "127.0.0.1:5432",
		Open: 1, Total: 2,
		Recent: []Line{
			{At: at(14, 2, 11), SourceIP: "203.0.113.7"},
			{At: at(14, 3, 0), SourceIP: "2001:db8::1"},
		},
	}
}

func connected(services ...Service) Model {
	return Model{
		Relay: "burrow.example.com", Version: "v0.6.0", State: client.StateConnected,
		RTT: 12 * time.Millisecond, Services: services,
	}
}

// golden compares lines with testdata/<name>, line by line.
func golden(t *testing.T, name string, lines []string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	got := strings.Join(lines, "\n") + "\n"
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for i := 0; i < len(want) || i < len(lines); i++ {
		var w, g string
		if i < len(want) {
			w = want[i]
		}
		if i < len(lines) {
			g = lines[i]
		}
		if w != g {
			t.Errorf("%s line %d:\n got %q\nwant %q", name, i+1, g, w)
		}
	}
}

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func TestRender_Golden(t *testing.T) {
	withAccess := httpService()
	withAccess.Access = "open"
	withAccess.LocalDown = true
	notice := connected(httpService())
	notice.Notice = "The relay runs v0.7.0. Run: burrow update"

	cases := []struct {
		file  string
		model Model
		width int
	}{
		{"connected_http.txt", connected(httpService()), 100},
		{"mockup.txt", connected(withAccess), 100},
		{"two_services.txt", connected(httpService(), tcpService()), 100},
		{"notice.txt", notice, 100},
		{"width60.txt", connected(withAccess, tcpService()), 60},
		{"width40.txt", connected(withAccess, tcpService()), 40},
		{"width20.txt", connected(withAccess, tcpService()), 20},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) { golden(t, c.file, Render(c.model, c.width, false)) })
	}
}

func TestRender_Header(t *testing.T) {
	m := Model{Relay: "burrow.example.com", Version: "v0.6.0", State: client.StateConnecting}
	if got := Render(m, 100, false)[0]; got != "burrow  ○  connecting to burrow.example.com     v0.6.0" {
		t.Fatalf("connecting: %q", got)
	}
	m.State, m.Detail, m.RetryIn = client.StateReconnecting, "connection refused", 4*time.Second
	if got := Render(m, 100, false)[0]; got != "burrow  ○  reconnecting in 4 s — connection refused     v0.6.0" {
		t.Fatalf("reconnecting: %q", got)
	}
	m.RetryIn = 3200 * time.Millisecond // a started second counts
	if got := Render(m, 100, false)[0]; !strings.Contains(got, "reconnecting in 4 s") {
		t.Fatalf("reconnecting in 3.2 s: %q", got)
	}
	m.RetryIn, m.Detail = 0, ""
	if got := Render(m, 100, false)[0]; got != "burrow  ○  reconnecting     v0.6.0" {
		t.Fatalf("reconnecting without delay: %q", got)
	}
	// A version without the leading v gets one; a name such as "develop" does not.
	m = Model{Relay: "r", Version: "0.6.0", State: client.StateConnected, RTT: 300 * time.Microsecond}
	if got := Render(m, 100, false)[0]; got != "burrow  ●  connected to r     v0.6.0   <1 ms" {
		t.Fatalf("header: %q", got)
	}
	m.Version, m.RTT = "develop", 0
	if got := Render(m, 100, false)[0]; got != "burrow  ●  connected to r     develop" {
		t.Fatalf("header: %q", got)
	}
}

func TestRender_Lines(t *testing.T) {
	lines := Render(connected(tcpService()), 100, false)
	for _, want := range []string{
		"  pg   burrow.example.com:9000  →  127.0.0.1:5432",
		"  14:02:11  connection from 203.0.113.7",
		"  14:03:00  connection from 2001:db8::1",
	} {
		if !hasLine(lines, want) {
			t.Errorf("line %q missing in:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	down := httpService()
	down.LocalDown = true
	if lines := Render(connected(down), 100, false); !hasLine(lines, "  ! nothing is listening on 127.0.0.1:3000") {
		t.Errorf("warning missing in:\n%s", strings.Join(lines, "\n"))
	}
	// A service the relay has not confirmed yet has no public address.
	waiting := Service{Name: "my-app", Type: "http", Local: "127.0.0.1:3000"}
	if lines := Render(connected(waiting), 100, false); !hasLine(lines, "  my-app   …  →  127.0.0.1:3000") {
		t.Errorf("unregistered service:\n%s", strings.Join(lines, "\n"))
	}
	// Other access modes are named without the remark about the URL.
	login := httpService()
	login.Access = "login"
	if lines := Render(connected(login), 100, false); !hasLine(lines, "           access: login       3 open, 41 total") {
		t.Errorf("access line:\n%s", strings.Join(lines, "\n"))
	}
}

func TestRender_NarrowNeverExceedsTheWidth(t *testing.T) {
	long := httpService()
	long.Access = "open"
	long.LocalDown = true
	long.Recent = append(long.Recent, Line{At: at(14, 2, 13), Method: "DELETE", Path: "/api/a/very/long/path/that/does/not/fit/into/any/narrow/terminal/at/all", Status: 204})
	m := connected(long, tcpService())
	m.Notice = "The relay runs v0.7.0. Run: burrow update"
	recon := m
	recon.State, recon.RetryIn = client.StateReconnecting, 4*time.Second
	recon.Detail = "dial: dial tcp 203.0.113.9:7000: connect: connection refused"

	for _, model := range []Model{m, recon} {
		for _, width := range []int{100, 80, 60, 40, 30, 20, 12, 5, 1} {
			lines := Render(model, width, false)
			text := strings.Join(lines, "\n")
			for _, l := range lines {
				if n := utf8.RuneCountInString(l); n > width {
					t.Errorf("width %d: line of %d characters: %q", width, n, l)
				}
				if strings.ContainsAny(l, "\n\r") {
					t.Errorf("width %d: line break inside %q", width, l)
				}
			}
			if width >= 20 {
				for _, want := range []string{"my-app", "127.0.0.1:3000", "pg", "127.0.0.1:5432"} {
					if !strings.Contains(text, want) {
						t.Errorf("width %d: %q is not visible in:\n%s", width, want, text)
					}
				}
			}
			if width == 60 || width == 40 {
				if !strings.Contains(text, "…") {
					t.Errorf("width %d: nothing was cut with … in:\n%s", width, text)
				}
			}
		}
	}
}

func TestRender_UnknownWidthIs80(t *testing.T) {
	m := connected(httpService(), tcpService())
	want := Render(m, 80, false)
	for _, w := range []int{0, -1, -80} {
		if got := Render(m, w, false); !reflect.DeepEqual(got, want) {
			t.Fatalf("width %d:\n%s\nwant:\n%s", w, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

func TestRender_Colour(t *testing.T) {
	s := httpService()
	s.LocalDown = true
	m := connected(s)

	plain := strings.Join(Render(m, 100, false), "\n")
	if strings.ContainsRune(plain, 0x1b) {
		t.Fatalf("ESC in output without colour: %q", plain)
	}
	lines := Render(m, 100, true)
	if !strings.Contains(lines[0], "\x1b[32m●\x1b[0m") {
		t.Fatalf("the dot of a connected client is not green: %q", lines[0])
	}
	warn := lines[len(lines)-1]
	if !strings.HasPrefix(warn, "\x1b[33m") || !strings.HasSuffix(warn, "\x1b[0m") || !strings.Contains(warn, "! nothing is listening") {
		t.Fatalf("warning line: %q", warn)
	}
	m.State = client.StateConnecting
	if l := Render(m, 100, true)[0]; !strings.Contains(l, "\x1b[33m○\x1b[0m") {
		t.Fatalf("the dot of a connecting client is not yellow: %q", l)
	}
	// Without the colour sequences the lines are those of the plain rendering.
	if got := stripSGR(strings.Join(lines, "\n")); got != plain {
		t.Fatalf("coloured lines differ in more than colour:\n%q\n%q", got, plain)
	}
}

func TestRender_IsPureAndSurvivesAnEmptyModel(t *testing.T) {
	m := connected(httpService(), tcpService())
	a, b := Render(m, 70, true), Render(m, 70, true)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two renderings of one model differ")
	}
	for _, w := range []int{-1, 0, 1, 2, 3, 20, 80} {
		if lines := Render(Model{}, w, true); len(lines) == 0 {
			t.Fatalf("width %d: no lines for an empty model", w)
		}
		_ = Render(Model{Services: []Service{{}}}, w, false)
		_ = Render(Model{Services: []Service{{Recent: []Line{{}}}}}, w, false)
	}
}

// What the relay sends (names, addresses, error texts) must not be able to
// move the cursor or change the screen.
func TestRender_ControlCharactersFromOutsideAreDropped(t *testing.T) {
	s := httpService()
	s.Name = "my\x1b[2J-app"
	s.Public = "https://x/\r\n\x1b[1A"
	s.Recent = []Line{{At: at(1, 2, 3), Method: "GET", Path: "/a\x07\x1b]0;title\x07", Status: 200}, {At: at(1, 2, 4), SourceIP: "1.2.3.4\x1b[H\u009b2J\u202e"}}
	m := connected(s)
	m.State, m.Detail, m.RetryIn = client.StateReconnecting, "boom\x1b[31m\nsecond line", time.Second
	m.Notice = "note\x1b[5m"
	for _, w := range []int{100, 30} {
		for _, l := range Render(m, w, false) {
			for _, r := range l {
				if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || r == 0x202e {
					t.Fatalf("width %d: control character %U in %q", w, r, l)
				}
			}
		}
	}
}

func TestFit(t *testing.T) {
	s := httpService()
	s.Recent = nil
	for i := 0; i < 10; i++ {
		s.Recent = append(s.Recent, Line{At: at(14, 0, i), SourceIP: "203.0.113.7"})
	}
	m := connected(s, s, s)
	full := Render(m, 100, false)

	if got := Fit(m, 100, len(full)+1, false); !reflect.DeepEqual(got, full) {
		t.Fatal("a view that fits was shortened")
	}
	for _, rows := range []int{30, 24, 12, 6, 3, 2, 1} {
		got := Fit(m, 100, rows, false)
		limit := rows - 1 // the cursor needs the line below the view
		if limit < 1 {
			limit = 1
		}
		if len(got) > limit {
			t.Fatalf("%d rows: %d lines", rows, len(got))
		}
		if !strings.HasPrefix(got[0], "burrow  ●  connected") {
			t.Fatalf("%d rows: the header is gone: %q", rows, got[0])
		}
	}
	// Recent lines go first: at 24 rows all three services are still there.
	if text := strings.Join(Fit(m, 100, 24, false), "\n"); strings.Count(text, "my-app") != 3 {
		t.Fatalf("24 rows:\n%s", text)
	}
	// The model itself is left alone.
	if len(m.Services[0].Recent) != 10 {
		t.Fatal("Fit changed its argument")
	}
}

// The access modes are shown under the names a person knows; only "open" gets
// the remark about the URL.
func TestRender_AccessModes(t *testing.T) {
	cases := map[string]string{
		"open":         "           access: open (anyone with the URL)       3 open, 41 total",
		"burrow_login": "           access: Burrow login       3 open, 41 total",
		"api_key":      "           access: API key       3 open, 41 total",
		"mtls":         "           access: mtls       3 open, 41 total",
		"":             "           3 open, 41 total",
	}
	for mode, want := range cases {
		s := httpService()
		s.Access = mode
		if lines := Render(connected(s), 100, false); !hasLine(lines, want) {
			t.Errorf("access %q: %q is not in:\n%s", mode, want, strings.Join(lines, "\n"))
		}
	}
}

func TestRender_Notes(t *testing.T) {
	m := connected(httpService())
	m.Notes = []string{
		"This app shares the dashboard's origin; expose only apps you trust.",
		"This service already exists with slug p7baeh and access login. --slug and --access apply only when a service is created. " +
			"Change them in the dashboard: https://burrow.example.com/services/0b1f7c1e-6f0e-4c58-9a51-3b0f8f2f6a11",
	}
	m.Notice = "The relay runs v0.7.0. Run: burrow update"
	golden(t, "notes.txt", Render(m, 100, false))

	for _, width := range []int{100, 80, 60, 40, 30, 20, 12, 5, 1} {
		lines := Render(m, width, false)
		for _, l := range lines {
			if n := utf8.RuneCountInString(l); n > width {
				t.Errorf("width %d: line of %d characters: %q", width, n, l)
			}
		}
		if width < 12 {
			continue
		}
		// A note is wrapped, not cut: every word of it is on the screen, and
		// the address can be put together again from its pieces.
		joined := strings.Join(lines, "")
		squeezed := strings.NewReplacer(" ", "", "!", "").Replace(joined)
		for _, want := range []string{"sharesthedashboard'sorigin", "https://burrow.example.com/services/0b1f7c1e-6f0e-4c58-9a51-3b0f8f2f6a11"} {
			if !strings.Contains(squeezed, want) {
				t.Errorf("width %d: %q was cut:\n%s", width, want, strings.Join(lines, "\n"))
			}
		}
	}

	// Notes are warnings: yellow with colour, and nothing but colour differs.
	plain := Render(m, 100, false)
	coloured := Render(m, 100, true)
	if got := stripSGR(strings.Join(coloured, "\n")); got != strings.Join(plain, "\n") {
		t.Fatalf("coloured lines differ in more than colour")
	}
	found := false
	for _, l := range coloured {
		if strings.Contains(l, "! This app shares") {
			found = strings.HasPrefix(l, "\x1b[33m") && strings.HasSuffix(l, "\x1b[0m")
		}
	}
	if !found {
		t.Fatalf("the note is not a yellow line:\n%q", coloured)
	}

	// A note is text that partly comes from the relay.
	m.Notes = []string{"evil \x1b[2J\r\nnote ‮"}
	for _, l := range Render(m, 100, false) {
		if strings.ContainsAny(l, "\x1b\r\n‮") {
			t.Fatalf("control characters in %q", l)
		}
	}
}

func TestRender_RequestLines(t *testing.T) {
	lines := Render(connected(httpService()), 100, false)
	for _, want := range []string{
		"  14:02:11  GET   /api/users      200",
		"  14:02:12  POST  /api/login      401",
	} {
		if !hasLine(lines, want) {
			t.Errorf("line %q missing in:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// A status that says something went wrong is coloured, and only the status;
// without colour nothing marks it.
func TestRender_StatusColour(t *testing.T) {
	s := httpService()
	s.Recent = append(s.Recent,
		Line{At: at(14, 2, 13), Method: "GET", Path: "/boom", Status: 502},
		Line{At: at(14, 2, 14), Method: "GET", Path: "/moved", Status: 301},
		Line{At: at(14, 2, 15), Method: "GET", Path: "/500/401", Status: 404})
	m := connected(s)
	plain := Render(m, 100, false)
	if text := strings.Join(plain, "\n"); strings.ContainsRune(text, 0x1b) {
		t.Fatalf("ESC without colour: %q", text)
	}
	col := Render(m, 100, true)
	for _, want := range []string{
		"  14:02:11  GET   /api/users      200",
		"  14:02:12  POST  /api/login      \x1b[33m401\x1b[0m",
		"  14:02:13  GET   /boom           \x1b[31m502\x1b[0m",
		"  14:02:14  GET   /moved          301",
		"  14:02:15  GET   /500/401        \x1b[33m404\x1b[0m",
	} {
		if !hasLine(col, want) {
			t.Errorf("line %q missing in:\n%q", want, col)
		}
	}
	if got := stripSGR(strings.Join(col, "\n")); got != strings.Join(plain, "\n") {
		t.Fatalf("coloured lines differ in more than colour:\n%q\n%q", got, plain)
	}
	// Narrow: the status is still the last thing on the line, and coloured.
	for _, l := range Render(m, 24, true) {
		if strings.Contains(l, "/api/lo") && !strings.HasSuffix(l, " \x1b[33m401\x1b[0m") {
			t.Errorf("narrow line %q", l)
		}
	}
}

// What slipped through every check before is replaced where it is drawn.
func TestRender_RequestTextIsMadePrintable(t *testing.T) {
	s := httpService()
	s.Recent = []Line{{At: at(14, 2, 11), Method: "G\x1bT", Path: "/a\x1b[2Jb\r\nc\u202ed\u200be", Status: 200}}
	lines := Render(connected(s), 100, false)
	if want := "  14:02:11  G?T   /a?[2Jb??c?d?e  200"; !hasLine(lines, want) {
		t.Fatalf("line %q missing in:\n%q", want, lines)
	}
}

// One long path does not push the others out of their columns, and no line is
// longer than the terminal is wide.
func TestRender_LongPathIsCut(t *testing.T) {
	s := httpService()
	s.Recent = append(s.Recent, Line{At: at(14, 2, 13), Method: "DELETE", Path: "/" + strings.Repeat("x", 255), Status: 500})
	for _, width := range []int{200, 100, 80, 60, 41, 40, 30, 20, 8, 1} {
		lines := Render(connected(s), width, true)
		for _, l := range lines {
			if n := length(stripSGR(l)); n > width-1 && width > 1 {
				t.Fatalf("width %d: line of %d characters: %q", width, n, l)
			}
		}
		if width < 60 {
			continue
		}
		var req []string
		for _, l := range lines {
			if p := stripSGR(l); strings.HasPrefix(p, "  14:02:1") {
				req = append(req, p)
			}
		}
		if len(req) != 3 {
			t.Fatalf("width %d: request lines %q", width, req)
		}
		// All three end in their status, in the same column.
		for _, l := range req {
			if length(l) != length(req[0]) {
				t.Fatalf("width %d: the lines are not aligned:\n%s", width, strings.Join(req, "\n"))
			}
		}
		if !strings.HasPrefix(req[0], "  14:02:11  GET     /api/users ") || !strings.HasSuffix(req[0], "  200") {
			t.Fatalf("width %d: %q", width, req[0])
		}
		if !strings.HasPrefix(req[2], "  14:02:13  DELETE  /xxxx") || !strings.HasSuffix(req[2], "x…  500") {
			t.Fatalf("width %d: %q", width, req[2])
		}
	}
}

// A height that is not known counts as 24 rows: a taller view would scroll
// the terminal with every redraw.
func TestFit_UnknownHeightIs24Rows(t *testing.T) {
	s := httpService()
	s.Recent = nil
	for i := 0; i < 10; i++ {
		s.Recent = append(s.Recent, Line{At: at(14, 0, i), Method: "GET", Path: "/", Status: 200})
	}
	m := connected(s, s, s, s)
	if n := len(Render(m, 100, false)); n <= 24 {
		t.Fatalf("the model is only %d lines tall", n)
	}
	got := Fit(m, 100, 0, false)
	if len(got) > 23 || !reflect.DeepEqual(got, Fit(m, 100, 24, false)) {
		t.Fatalf("%d lines for an unknown height", len(got))
	}
	// A view that fits 24 rows is left alone.
	small := connected(httpService())
	if !reflect.DeepEqual(Fit(small, 100, 0, false), Render(small, 100, false)) {
		t.Fatal("a small view was shortened")
	}
}

// A gateway-only service has no URL, so "anyone with the URL" would be wrong.
func TestRender_GatewayOnlyHasNoURLRemark(t *testing.T) {
	s := httpService()
	s.Access = "open"
	s.Public = GatewayOnlyPublic
	text := strings.Join(Render(connected(s), 100, false), "\n")
	if !strings.Contains(text, "AI gateway only") || !strings.Contains(text, "access: open") {
		t.Fatalf("missing parts:\n%s", text)
	}
	if strings.Contains(text, "anyone with the URL") {
		t.Fatalf("remark about a URL that does not exist:\n%s", text)
	}
}
