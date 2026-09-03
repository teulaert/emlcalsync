package tui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
)

// spelledElsewhere is the keys the ? overlay deliberately does not name: the
// arrows and paging keys nobody has to be told to try -- the overlay draws
// them as arrows where it names them at all -- ⌫, which it draws rather than
// spells, and ctrl+c, which the q line already stands for.
var spelledElsewhere = map[string]bool{
	"up": true, "down": true, "home": true, "end": true,
	"left": true, "right": true,
	"pgup": true, "pgdown": true, "backspace": true, "ctrl+c": true,
}

// TestHelpCoversEveryKey is what keeps the overlay honest. It is prose, so it
// cannot be generated out of the bindings -- but it can be checked against
// them, which is the difference between documented and documented once.
// shift+tab worked for a release without appearing anywhere; this is so the
// next one cannot.
func TestHelpCoversEveryKey(t *testing.T) {
	k := defaultKeys()
	var b strings.Builder
	for _, l := range k.helpLines() {
		b.WriteString(l[0] + " " + l[1] + "\n")
	}
	help := b.String()

	v := reflect.ValueOf(k)
	for i := range v.NumField() {
		binding, ok := v.Field(i).Interface().(key.Binding)
		if !ok {
			continue
		}
		for _, s := range binding.Keys() {
			if spelledElsewhere[s] {
				continue
			}
			if !strings.Contains(help, s) {
				t.Errorf("%s is bound to %q, and the ? overlay never says so",
					v.Type().Field(i).Name, s)
			}
		}
	}
}
