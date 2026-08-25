package output

import (
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultWidth is the terminal width assumed when the real one is unknown.
const DefaultWidth = 120

// minColWidth is the narrowest a column is squeezed to before the renderer
// gives up and lets the row overflow.
const minColWidth = 6

// column is one tagged struct field.
type column struct {
	header string
	index  []int // field index path, for embedded structs
	right  bool
	max    int // per-column cap from `table:"...,max=N"`; 0 = no explicit cap
}

// columnsOf reflects over a struct type and returns its `table:"..."` fields.
//
// Tag syntax: `table:"Header"`, `table:"Header,right"`, `table:"Header,left"`,
// `table:"Header,max=30"`. `table:"-"` and untagged fields are skipped, so a
// struct can carry ids and epoch fields that only JSON output shows.
func columnsOf(t reflect.Type) []column {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var cols []column
	var walk func(t reflect.Type, prefix []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			idx := append(append([]int{}, prefix...), i)
			tag, ok := f.Tag.Lookup("table")
			if !ok {
				// Descend into anonymous embedded structs so an embedded row
				// type contributes its columns.
				if f.Anonymous && f.Type.Kind() == reflect.Struct && f.IsExported() {
					walk(f.Type, idx)
				}
				continue
			}
			if tag == "-" || !f.IsExported() {
				continue
			}
			parts := strings.Split(tag, ",")
			c := column{header: parts[0], index: idx, right: numericKind(f.Type)}
			if c.header == "" {
				c.header = f.Name
			}
			for _, opt := range parts[1:] {
				switch {
				case opt == "right":
					c.right = true
				case opt == "left":
					c.right = false
				case strings.HasPrefix(opt, "max="):
					if n, err := strconv.Atoi(opt[len("max="):]); err == nil && n > 0 {
						c.max = n
					}
				}
			}
			cols = append(cols, c)
		}
	}
	walk(t, nil)
	return cols
}

// fieldByIndexSafe walks an index path, stopping at a nil pointer.
func fieldByIndexSafe(v reflect.Value, index []int) reflect.Value {
	for i, x := range index {
		if i > 0 {
			for v.Kind() == reflect.Pointer {
				if v.IsNil() {
					return reflect.Value{}
				}
				v = v.Elem()
			}
		}
		if v.Kind() != reflect.Struct {
			return reflect.Value{}
		}
		v = v.Field(x)
	}
	return v
}

// renderTable draws a headed, borderless table for a slice of structs.
func renderTable(rows reflect.Value, width int, color bool) string {
	if width <= 0 {
		width = DefaultWidth
	}
	elem := rows.Type().Elem()
	cols := columnsOf(elem)
	if len(cols) == 0 {
		// No tagged fields: fall back to one value per line so the caller
		// still sees something useful (e.g. a []string of mailbox names).
		var b strings.Builder
		for i := 0; i < rows.Len(); i++ {
			b.WriteString(cell(rows.Index(i)))
			b.WriteByte('\n')
		}
		return b.String()
	}

	cells := make([][]string, rows.Len())
	for i := 0; i < rows.Len(); i++ {
		row := rows.Index(i)
		for row.Kind() == reflect.Pointer || row.Kind() == reflect.Interface {
			if row.IsNil() {
				break
			}
			row = row.Elem()
		}
		line := make([]string, len(cols))
		for j, c := range cols {
			line[j] = oneLine(cell(fieldByIndexSafe(row, c.index)))
		}
		cells[i] = line
	}
	return layout(cols, cells, width, color)
}

// renderKeyValue draws the two-column layout used for a single struct.
func renderKeyValue(v reflect.Value, width int, color bool) string {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	cols := columnsOf(v.Type())
	if len(cols) == 0 {
		return oneLine(cell(v)) + "\n"
	}
	keyw := 0
	for _, c := range cols {
		keyw = max(keyw, utf8.RuneCountInString(c.header))
	}
	valw := max(minColWidth, width-keyw-2)

	var b strings.Builder
	for _, c := range cols {
		val := cell(fieldByIndexSafe(v, c.index))
		key := pad(c.header, keyw, false)
		if color {
			key = bold(key)
		}
		// Multi-line values (descriptions, bodies) are indented under the key.
		lines := strings.Split(strings.TrimRight(val, "\n"), "\n")
		for i, ln := range lines {
			if i == 0 {
				b.WriteString(key + "  " + Truncate(ln, valw) + "\n")
			} else {
				b.WriteString(strings.Repeat(" ", keyw+2) + Truncate(ln, valw) + "\n")
			}
		}
	}
	return b.String()
}

// layout sizes columns to their content, shrinking the widest ones until the
// row fits, then writes header + rows with no borders.
func layout(cols []column, cells [][]string, width int, color bool) string {
	const gap = 2

	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = utf8.RuneCountInString(c.header)
	}
	for _, row := range cells {
		for i, s := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(s))
		}
	}
	// Per-column caps from the struct tag come first: they are the author's
	// explicit intent, independent of terminal size.
	for i, c := range cols {
		if c.max > 0 {
			widths[i] = min(widths[i], c.max)
		}
	}

	total := func() int {
		t := gap * (len(widths) - 1)
		for _, w := range widths {
			t += w
		}
		return t
	}
	// Shrink the widest column repeatedly; ties go to the leftmost, which
	// keeps trailing columns (dates, sizes) intact.
	for {
		over := total() - width
		if over <= 0 {
			break
		}
		widest, wi := 0, -1
		for i, w := range widths {
			if w > widest && w > minColWidth {
				widest, wi = w, i
			}
		}
		if wi < 0 {
			break // every column is already at the floor
		}
		want := max(minColWidth, widths[wi]-over)
		widths[wi] = min(want, widths[wi]-1)
	}

	var b strings.Builder
	writeRow := func(vals []string, header bool) {
		var line strings.Builder
		for i, s := range vals {
			s = Truncate(s, widths[i])
			if i == len(vals)-1 && !cols[i].right {
				line.WriteString(s) // no trailing padding
			} else {
				line.WriteString(pad(s, widths[i], cols[i].right))
			}
			if i < len(vals)-1 {
				line.WriteString(strings.Repeat(" ", gap))
			}
		}
		out := strings.TrimRight(line.String(), " ")
		if header && color {
			out = bold(out)
		}
		b.WriteString(out)
		b.WriteByte('\n')
	}

	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	writeRow(headers, true)
	for _, row := range cells {
		writeRow(row, false)
	}
	return b.String()
}

func pad(s string, w int, right bool) string {
	n := w - utf8.RuneCountInString(s)
	if n <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", n) + s
	}
	return s + strings.Repeat(" ", n)
}

// oneLine collapses embedded newlines and tabs so a value never breaks a row.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	r := strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(r.Replace(s))
}

func bold(s string) string { return "\x1b[1m" + s + "\x1b[0m" }
