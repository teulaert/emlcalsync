// Package output renders command results as JSON, a terminal table, or
// tab-separated plain lines, and defines the process exit codes from
// DESIGN.md §9.1.
//
// The contract callers rely on: JSON is an object for a single item and an
// array for a list, never pretty-printed unless asked; table output is for
// humans and may change; plain output is grep- and cut-friendly.
package output

import (
	"fmt"
	"strings"
)

// Format selects a renderer.
type Format int

const (
	// Auto resolves to Table on a TTY and JSON when piped.
	Auto Format = iota
	// JSON is the machine-readable form agents get by default.
	JSON
	// Table is the human-readable aligned form.
	Table
	// Plain is one record per line, tab separated, no header.
	Plain
)

func (f Format) String() string {
	switch f {
	case Auto:
		return "auto"
	case JSON:
		return "json"
	case Table:
		return "table"
	case Plain:
		return "plain"
	}
	return fmt.Sprintf("Format(%d)", int(f))
}

// ParseFormat parses the --format/-o flag value.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return Auto, nil
	case "json":
		return JSON, nil
	case "table":
		return Table, nil
	case "plain", "text", "tsv":
		return Plain, nil
	}
	return Auto, fmt.Errorf("unknown format %q: want auto, json, table or plain", s)
}

// Resolve turns Auto into a concrete format. Anything else passes through, so
// an explicit --format survives being piped.
func Resolve(f Format, stdoutIsTTY bool) Format {
	if f != Auto {
		return f
	}
	if stdoutIsTTY {
		return Table
	}
	return JSON
}
