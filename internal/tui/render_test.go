package tui

import (
	"strings"
	"testing"
)

func TestTruncCellsCountsDisplayWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"fits", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"cuts ascii", "hello world", 8, "hello w…"},
		{"one cell", "hello", 1, "…"},
		{"zero", "hello", 0, ""},
		// Each ideograph is two cells wide, so only two of them plus the
		// ellipsis fit in five. Counting runes would have let three through
		// and pushed every column to the right of it out of line.
		{"wide runes", "日本語です", 5, "日本…"},
		{"newlines become spaces", "a\nb", 5, "a b"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncCells(tc.in, tc.n); got != tc.want {
				t.Errorf("truncCells(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
		})
	}
}

func TestPadCellsAlignsWideText(t *testing.T) {
	// Two rows padded to the same budget must be the same number of cells,
	// whatever alphabet they are written in.
	a := padCells("ok", 10)
	b := padCells("日本語", 10)
	if cellWidth(a) != cellWidth(b) {
		t.Fatalf("padCells widths differ: %q=%d %q=%d", a, cellWidth(a), b, cellWidth(b))
	}
	if cellWidth(a) != 10 {
		t.Errorf("padCells width = %d, want 10", cellWidth(a))
	}
}

func TestFrameIsExactlyHighEnough(t *testing.T) {
	got := frame("head", []string{"a", "b"}, "foot", 10, 6)
	if n := countLines(got); n != 6 {
		t.Errorf("frame produced %d lines, want 6", n)
	}
}

func cellWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeCells(r)
	}
	return w
}

func countLines(s string) int {
	n := 1
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}

// stripANSI removes SGR sequences so a test can measure the text a terminal
// would actually show.
func stripANSI(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			in = true
		case in && (r == 'm' || r == 'K'):
			in = false
		case in:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestWrapCellsBreaksOnWordsAndKeepsParagraphs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		n    int
		want []string
	}{
		{"fits", "hello world", 20, []string{"hello world"}},
		{"wraps on a space", "hello world again", 11, []string{"hello world", "again"}},
		{"blank lines survive", "one\n\ntwo", 10, []string{"one", "", "two"}},
		{"indent is kept", "  * a list item that wraps", 14, []string{"  * a list", "  item that", "  wraps"}},
		// A URL has nowhere to break, so it is cut rather than allowed to
		// overflow the column and shift everything after it.
		{"long word is cut", "see https://example.com/very/long", 10, []string{"see", "https://ex", "ample.com/", "very/long"}},
		// Two cells per ideograph: five of them do not fit in eight cells.
		{"wide runes", "日本語です", 8, []string{"日本語で", "す"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapCells(tc.in, tc.n)
			if len(got) != len(tc.want) {
				t.Fatalf("wrapCells(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("wrapCells(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
				}
			}
		})
	}
}

// Nothing the wrapper emits may be wider than the column it was given, or the
// expanded thread view would shift every line to its right. (A single column
// is the exception: a two-cell rune cannot be cut in half.)
func TestWrapCellsNeverOverflows(t *testing.T) {
	in := "A paragraph with a stupidly-long-unbreakable-token in it,\n\n  and 日本語 mixed in for width.\n"
	for _, n := range []int{2, 4, 12, 40} {
		for _, l := range wrapCells(in, n) {
			if cellWidth(l) > n {
				t.Errorf("wrapCells(_, %d) produced %q at %d cells", n, l, cellWidth(l))
			}
		}
	}
}
