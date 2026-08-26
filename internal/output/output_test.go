package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/model"
)

type row struct {
	ID      string `json:"id"        table:"ID"`
	Subject string `json:"subject"   table:"Subject"`
	Size    int64  `json:"size"      table:"Size"`
	Hidden  string `json:"hidden"`      // no table tag: JSON only
	Skipped string `json:"-" table:"-"` // in neither
}

func lines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"": Auto, "auto": Auto, "json": JSON, "JSON": JSON,
		"table": Table, "plain": Plain, " table ": Table,
	} {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseFormat("yaml"); err == nil {
		t.Error("ParseFormat(yaml) should fail")
	}
}

func TestResolve(t *testing.T) {
	if got := Resolve(Auto, true); got != Table {
		t.Errorf("Auto on a TTY = %v, want Table", got)
	}
	if got := Resolve(Auto, false); got != JSON {
		t.Errorf("Auto when piped = %v, want JSON", got)
	}
	// An explicit format survives being piped, and vice versa.
	if got := Resolve(Table, false); got != Table {
		t.Errorf("explicit Table when piped = %v", got)
	}
	if got := Resolve(JSON, true); got != JSON {
		t.Errorf("explicit JSON on a TTY = %v", got)
	}
}

func TestPrintJSONShapes(t *testing.T) {
	p := &Printer{Format: JSON}

	// A single item is an object.
	out, err := p.Sprint(row{ID: "work:1", Subject: "Hi", Size: 12})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "{") {
		t.Errorf("single item should be an object, got %s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["hidden"]; !ok {
		t.Error("JSON should include untagged-for-table fields")
	}
	if _, ok := obj["Skipped"]; ok {
		t.Error(`json:"-" fields must not appear`)
	}

	// A list is an array.
	out, err = p.Sprint([]row{{ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "[") {
		t.Errorf("list should be an array, got %s", out)
	}

	// An empty or nil list is [], never null.
	for name, v := range map[string]any{"nil slice": []row(nil), "empty slice": []row{}} {
		out, err := p.Sprint(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out) != "[]" {
			t.Errorf("%s printed %q, want []", name, strings.TrimSpace(out))
		}
	}

	// Compact by default, indented with --pretty.
	if strings.Contains(out, "\n  ") {
		t.Error("JSON should be compact unless Pretty")
	}
	q := &Printer{Format: JSON, Pretty: true}
	out, _ = q.Sprint([]row{{ID: "a"}})
	if !strings.Contains(out, "\n  ") {
		t.Errorf("Pretty output should be indented:\n%s", out)
	}
}

func TestTimeMarshalsRFC3339Local(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	tm := time.Date(2026, 8, 25, 14, 30, 0, 0, loc)

	b, err := json.Marshal(struct {
		At    Time  `json:"at"`
		AtUTC int64 `json:"at_utc"`
	}{At: T(tm), AtUTC: T(tm).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		At    string `json:"at"`
		AtUTC int64  `json:"at_utc"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	// Rendered in the local zone, but the same instant.
	parsed, err := time.Parse(time.RFC3339, got.At)
	if err != nil {
		t.Fatalf("not RFC 3339: %q", got.At)
	}
	if !parsed.Equal(tm) {
		t.Errorf("At = %q, a different instant from %v", got.At, tm)
	}
	if got.AtUTC != tm.Unix() {
		t.Errorf("at_utc = %d, want %d", got.AtUTC, tm.Unix())
	}
	// Only meaningful when the local zone actually has an offset; CI runners
	// sit in UTC, where "Z" is the correct rendering.
	if _, off := tm.In(time.Local).Zone(); off != 0 && strings.HasSuffix(got.At, "Z") {
		t.Errorf("At should carry a local offset, got %q", got.At)
	}

	// A zero time is null, not year 1.
	b, _ = json.Marshal(struct {
		At Time `json:"at"`
	}{})
	if string(b) != `{"at":null}` {
		t.Errorf("zero Time = %s, want null", b)
	}
}

func TestTableRendering(t *testing.T) {
	p := &Printer{Format: Table, Width: 60}
	out, err := p.Sprint([]row{
		{ID: "work:1", Subject: "Short", Size: 12},
		{ID: "personal:2", Subject: "A rather longer subject line", Size: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := lines(out)
	if len(got) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(got), out)
	}
	if !strings.HasPrefix(got[0], "ID") || !strings.Contains(got[0], "Subject") {
		t.Errorf("header = %q", got[0])
	}
	if strings.Contains(got[0], "Hidden") || strings.Contains(got[0], "Skipped") {
		t.Errorf("untagged fields leaked into the table: %q", got[0])
	}
	// Columns line up: Subject starts at the same offset on every line.
	col := strings.Index(got[0], "Subject")
	if idx := strings.Index(got[1], "Short"); idx != col {
		t.Errorf("Subject column not aligned: header at %d, row at %d\n%s", col, idx, out)
	}
	// Size is numeric, so it is right-aligned: the digits end at the same column.
	if !strings.HasSuffix(strings.TrimRight(got[1], " "), "12") ||
		!strings.HasSuffix(strings.TrimRight(got[2], " "), "4096") {
		t.Errorf("numeric column should be right-aligned:\n%s", out)
	}
	if i, j := strings.Index(got[1], "12"), strings.Index(got[2], "4096"); i != j+2 {
		t.Errorf("right alignment off: %d vs %d\n%s", i, j, out)
	}
	// No borders.
	if strings.ContainsAny(out, "|+") {
		t.Errorf("table should be borderless:\n%s", out)
	}
}

func TestTableTruncatesToWidth(t *testing.T) {
	long := strings.Repeat("x", 200)
	p := &Printer{Format: Table, Width: 40}
	out, err := p.Sprint([]row{{ID: "work:1", Subject: long, Size: 1}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range lines(out) {
		if n := len([]rune(ln)); n > 40 {
			t.Errorf("line is %d runes wide, want <= 40: %q", n, ln)
		}
	}
	if !strings.Contains(out, "…") {
		t.Errorf("truncated cells should end in an ellipsis:\n%s", out)
	}
}

func TestTableMaxTagCapsColumn(t *testing.T) {
	type capped struct {
		Name string `table:"Name,max=5"`
		Note string `table:"Note"`
	}
	p := &Printer{Format: Table, Width: 200}
	out, _ := p.Sprint([]capped{{Name: "abcdefghij", Note: "ok"}})
	if !strings.Contains(out, "abcd…") {
		t.Errorf("max=5 not honoured:\n%s", out)
	}
}

func TestTableFallbackWidth(t *testing.T) {
	// Width 0 means "unknown terminal": DefaultWidth is used, not zero.
	p := &Printer{Format: Table}
	out, _ := p.Sprint([]row{{ID: "a", Subject: strings.Repeat("y", 400), Size: 1}})
	for _, ln := range lines(out) {
		if n := len([]rune(ln)); n > DefaultWidth {
			t.Errorf("line %d runes wide, want <= %d", n, DefaultWidth)
		}
	}
}

func TestSingleStructIsKeyValue(t *testing.T) {
	p := &Printer{Format: Table, Width: 60}
	out, err := p.Sprint(row{ID: "work:1", Subject: "Hello", Size: 42})
	if err != nil {
		t.Fatal(err)
	}
	got := lines(out)
	if len(got) != 3 {
		t.Fatalf("want one line per field, got:\n%s", out)
	}
	for i, want := range []string{"ID", "Subject", "Size"} {
		if !strings.HasPrefix(got[i], want) {
			t.Errorf("line %d = %q, want it to start with %q", i, got[i], want)
		}
	}
	if !strings.Contains(got[1], "Hello") {
		t.Errorf("value missing: %q", got[1])
	}
}

func TestPlainRendering(t *testing.T) {
	p := &Printer{Format: Plain}
	out, err := p.Sprint([]row{
		{ID: "work:1", Subject: "Short", Size: 12},
		{ID: "personal:2", Subject: "Multi\nline\tsubject", Size: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := lines(out)
	if len(got) != 2 {
		t.Fatalf("plain output should have no header, got:\n%s", out)
	}
	if got[0] != "work:1\tShort\t12" {
		t.Errorf("line 0 = %q", got[0])
	}
	// Embedded newlines and tabs must not break the record shape.
	if n := strings.Count(got[1], "\t"); n != 2 {
		t.Errorf("line 1 has %d tabs, want 2: %q", n, got[1])
	}
	if strings.Contains(got[1], "\n") {
		t.Errorf("line 1 still contains a newline: %q", got[1])
	}

	// An empty list prints nothing at all.
	out, _ = p.Sprint([]row{})
	if out != "" {
		t.Errorf("empty list printed %q", out)
	}
}

func TestErrorOutput(t *testing.T) {
	var buf bytes.Buffer
	p := &Printer{Format: JSON, ErrW: &buf}
	p.Error(CodeNotFound, "read work:1", model.ErrNotFound)

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("stderr is not the JSON envelope: %q", buf.String())
	}
	if env.Error.Code != CodeNotFound {
		t.Errorf("code = %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "read work:1") || !strings.Contains(env.Error.Message, "not found") {
		t.Errorf("message = %q", env.Error.Message)
	}

	buf.Reset()
	q := &Printer{Format: Table, ErrW: &buf}
	q.Error(CodeNotFound, "read work:1", model.ErrNotFound)
	if got := strings.TrimSpace(buf.String()); got != "emlcal: read work:1: not found" {
		t.Errorf("text error = %q", got)
	}
}

func TestExitCodes(t *testing.T) {
	if ExitCodeOf(nil) != ExitOK {
		t.Error("nil error is ExitOK")
	}
	if got := ExitCodeOf(errors.New("boom")); got != ExitGeneric {
		t.Errorf("plain error = %d, want %d", got, ExitGeneric)
	}
	wrapped := fmt.Errorf("context: %w", &ExitError{Code: ExitQueued, Err: errors.New("offline")})
	if got := ExitCodeOf(wrapped); got != ExitQueued {
		t.Errorf("wrapped ExitError = %d, want %d", got, ExitQueued)
	}
	if got := Errorf(ExitUsage, "bad flag %q", "-z").Error(); got != `bad flag "-z"` {
		t.Errorf("Errorf message = %q", got)
	}

	// Fail maps the model sentinels even without an ExitError.
	var buf bytes.Buffer
	p := &Printer{Format: JSON, ErrW: &buf}
	if got := p.Fail("", fmt.Errorf("sync: %w", model.ErrOffline)); got != ExitOffline {
		t.Errorf("Fail(offline) = %d, want %d", got, ExitOffline)
	}
	if !strings.Contains(buf.String(), CodeOffline) {
		t.Errorf("envelope should carry the offline code: %s", buf.String())
	}
	if got := CodeForExit(ExitProvider); got != CodeProvider {
		t.Errorf("CodeForExit(%d) = %q", ExitProvider, got)
	}
}

func TestRelTime(t *testing.T) {
	now := time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		t    time.Time
		want string
	}{
		{"seconds ago", now.Add(-30 * time.Second), "now"},
		{"minutes ago", now.Add(-12 * time.Minute), "12m"},
		{"hours ago", now.Add(-3 * time.Hour), "3h"},
		{"just under a day", now.Add(-23 * time.Hour), "23h"},
		{"yesterday", time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC), "yesterday"},
		{"this year", time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), "Aug 20"},
		{"last year", time.Date(2025, 1, 3, 9, 0, 0, 0, time.UTC), "2025-01-03"},
		{"soon", now.Add(20 * time.Minute), "in 20m"},
		// Under a day out, hours still win over the day name.
		{"later today", time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC), "in 18h"},
		{"tomorrow", time.Date(2026, 8, 26, 20, 0, 0, 0, time.UTC), "tomorrow"},
		{"next month", time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC), "Sep 30"},
		{"zero", time.Time{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RelTime(tc.t, now); got != tc.want {
				t.Errorf("RelTime(%v) = %q, want %q", tc.t, got, tc.want)
			}
		})
	}
}

func TestRelTimeAcrossDST(t *testing.T) {
	// 25 October 2026 is the European autumn transition; the calendar-day
	// arithmetic must not be thrown off by the 25-hour day.
	loc, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, loc)
	if got := RelTime(time.Date(2026, 10, 25, 10, 0, 0, 0, loc), now); got != "yesterday" {
		t.Errorf("across the DST change = %q, want yesterday", got)
	}
}

func TestHumanSize(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0B"}, {912, "912B"}, {1024, "1KB"}, {1536, "1.5KB"},
		{4096, "4KB"}, {1024 * 1024, "1MB"}, {1258291, "1.2MB"},
		{1024 * 1024 * 1024, "1GB"},
	} {
		if got := HumanSize(tc.in); got != tc.want {
			t.Errorf("HumanSize(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hel…"},
		{"hello", 1, "…"},
		{"hello", 0, ""},
		{"日本語テキスト", 3, "日本…"},
		{"héllo wörld", 7, "héllo…"}, // trailing space is trimmed before the ellipsis
	} {
		if got := Truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}
