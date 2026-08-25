package output

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Time is a time.Time that always marshals as RFC 3339 in local time with an
// offset, per the output contract. Callers building presentation structs wrap
// their timestamps in it:
//
//	type row struct {
//	    Date    output.Time `json:"date"     table:"Date"`
//	    DateUTC int64       `json:"date_utc"`
//	}
//
// A zero Time marshals as null and renders as an empty cell.
type Time struct{ time.Time }

// T wraps a time.Time for output.
func T(t time.Time) Time { return Time{t} }

// TP wraps an optional time.Time; nil yields the zero Time (JSON null).
func TP(t *time.Time) Time {
	if t == nil {
		return Time{}
	}
	return Time{*t}
}

// MarshalJSON writes RFC 3339 in local time, or null when zero.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(strconv.Quote(t.In(time.Local).Format(time.RFC3339))), nil
}

// UnmarshalJSON accepts RFC 3339 or null, so round-tripping output in tests
// and fixtures works.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	s, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// String renders RFC 3339 local time, empty when zero.
func (t Time) String() string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.Local).Format(time.RFC3339)
}

// Unix returns the epoch seconds, for the `_utc` companion fields the output
// contract asks for. Zero times give 0, not a negative epoch.
func (t Time) Unix() int64 {
	if t.IsZero() {
		return 0
	}
	return t.Time.Unix()
}

// ---------------------------------------------------------------------------
// Cell formatting shared by the table and plain renderers.

// cell renders one field value as display text.
func cell(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return cell(v.Elem())
	}

	// Concrete types worth spelling out before falling back to Stringer.
	switch x := v.Interface().(type) {
	case Time:
		return x.String()
	case time.Time:
		if x.IsZero() {
			return ""
		}
		return x.In(time.Local).Format(time.RFC3339)
	case time.Duration:
		return x.String()
	case []byte:
		return string(x)
	case fmt.Stringer:
		return x.String()
	}

	switch v.Kind() {
	case reflect.String:
		return v.String()
	case reflect.Bool:
		if v.Bool() {
			return "true"
		}
		return "false"
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64)
	case reflect.Slice, reflect.Array:
		parts := make([]string, v.Len())
		for i := range parts {
			parts[i] = cell(v.Index(i))
		}
		return strings.Join(parts, ", ")
	case reflect.Map, reflect.Struct:
		b, err := json.Marshal(v.Interface())
		if err != nil {
			return fmt.Sprint(v.Interface())
		}
		return string(b)
	}
	return fmt.Sprint(v.Interface())
}

// numericKind reports whether a column of this type should be right-aligned.
func numericKind(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(time.Duration(0)) {
		return true
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}
