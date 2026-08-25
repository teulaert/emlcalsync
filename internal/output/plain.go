package output

import (
	"reflect"
	"strings"
)

// renderPlain writes one record per line, tab separated, no header — the form
// `cut -f2` and `awk -F'\t'` expect. Only `table:"..."` fields are included,
// in declaration order.
func renderPlain(rows reflect.Value, _ int, _ bool) string {
	elem := rows.Type().Elem()
	cols := columnsOf(elem)

	var b strings.Builder
	for i := 0; i < rows.Len(); i++ {
		row := indirect(rows.Index(i))
		b.WriteString(plainLine(row, cols))
		b.WriteByte('\n')
	}
	return b.String()
}

// renderPlainOne writes a single record as one tab-separated line.
func renderPlainOne(v reflect.Value, _ int, _ bool) string {
	cols := columnsOf(v.Type())
	return plainLine(v, cols) + "\n"
}

func plainLine(v reflect.Value, cols []column) string {
	if len(cols) == 0 || !v.IsValid() {
		return oneLine(cell(v))
	}
	fields := make([]string, len(cols))
	for i, c := range cols {
		// Tabs and newlines inside a value would break the record shape, so
		// they are collapsed to spaces rather than escaped.
		fields[i] = oneLine(cell(fieldByIndexSafe(v, c.index)))
	}
	return strings.Join(fields, "\t")
}
