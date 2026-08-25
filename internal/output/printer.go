package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/lennert/emlcal/internal/model"
)

// Printer renders command results. Zero value is unusable: set at least W.
type Printer struct {
	W      io.Writer // where results go, normally os.Stdout
	Format Format    // resolved format; Auto is treated as JSON
	Pretty bool      // indent JSON
	Color  bool      // ANSI attributes in table output
	Width  int       // terminal width for table output; 0 means DefaultWidth

	// ErrW is where Error writes; defaults to os.Stderr.
	ErrW io.Writer
}

// New returns a Printer writing to w.
func New(w io.Writer, f Format) *Printer { return &Printer{W: w, Format: f} }

// Print renders v. A slice or array is a list, anything else a single item.
//
// For table and plain output the fields shown are those tagged `table:"..."`;
// for JSON the whole value is marshalled, so callers normally pass a small
// presentation struct with both `json` and `table` tags.
func (p *Printer) Print(v any) error {
	w := p.W
	if w == nil {
		w = os.Stdout
	}
	switch p.Format {
	case JSON, Auto:
		return p.printJSON(w, v)
	case Table:
		return p.printText(w, v, renderTable, renderKeyValue)
	case Plain:
		return p.printText(w, v, renderPlain, renderPlainOne)
	}
	return fmt.Errorf("output: unsupported format %v", p.Format)
}

func (p *Printer) printJSON(w io.Writer, v any) error {
	// A nil or nil-typed slice must still print "[]", never "null": agents
	// parse the result without checking for null.
	if rv := indirect(reflect.ValueOf(v)); rv.IsValid() && isList(rv.Type()) && rv.IsNil() {
		v = []any{}
	} else if v == nil {
		v = struct{}{}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if p.Pretty {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(v)
}

// printText dispatches between the list and single-item renderers.
func (p *Printer) printText(
	w io.Writer,
	v any,
	list func(reflect.Value, int, bool) string,
	single func(reflect.Value, int, bool) string,
) error {
	rv := indirect(reflect.ValueOf(v))
	if !rv.IsValid() {
		return nil
	}
	var s string
	if isList(rv.Type()) {
		if rv.Len() == 0 {
			return nil // an empty list prints nothing, so `| wc -l` is 0
		}
		s = list(rv, p.width(), p.Color)
	} else {
		s = single(rv, p.width(), p.Color)
	}
	_, err := io.WriteString(w, s)
	return err
}

func (p *Printer) width() int {
	if p.Width > 0 {
		return p.Width
	}
	return DefaultWidth
}

// Error reports a failure: the JSON envelope from DESIGN.md §9.1 in JSON mode,
// a plain "emlcal: <msg>: <err>" line otherwise. It always writes to stderr.
func (p *Printer) Error(code string, msg string, err error) {
	w := p.ErrW
	if w == nil {
		w = os.Stderr
	}
	if code == "" {
		code = CodeGeneric
	}

	full := msg
	if err != nil {
		if full == "" {
			full = err.Error()
		} else {
			full = msg + ": " + err.Error()
		}
	}
	if full == "" {
		full = "unknown error"
	}

	if p.Format == JSON || p.Format == Auto {
		env := errEnvelope{}
		env.Error.Code = code
		env.Error.Message = full
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		if p.Pretty {
			enc.SetIndent("", "  ")
		}
		_ = enc.Encode(env)
		return
	}
	fmt.Fprintf(w, "emlcal: %s\n", full)
}

type errEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Fail reports err and returns the exit code the process should use. It picks
// the JSON error code from any ExitError in the chain and recognises the
// model-level sentinels, so commands can just `os.Exit(p.Fail("", err))`.
func (p *Printer) Fail(msg string, err error) int {
	if err == nil {
		return ExitOK
	}
	exit := ExitCodeOf(err)
	if exit == ExitGeneric {
		if code := sentinelExit(err); code != 0 {
			exit = code
		}
	}
	p.Error(CodeForExit(exit), msg, err)
	return exit
}

// sentinelExit maps the shared model errors onto exit codes.
func sentinelExit(err error) int {
	switch {
	case errors.Is(err, model.ErrNotFound):
		return ExitNotFound
	case errors.Is(err, model.ErrOffline):
		return ExitOffline
	}
	return 0
}

func indirect(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// isList reports whether t renders as a JSON array. []byte is a scalar.
func isList(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Slice:
		return t.Elem().Kind() != reflect.Uint8
	case reflect.Array:
		return true
	}
	return false
}

// Sprint renders v to a string with the printer's settings; handy for
// --dry-run output and tests.
func (p *Printer) Sprint(v any) (string, error) {
	var b strings.Builder
	q := *p
	q.W = &b
	err := q.Print(v)
	return b.String(), err
}
