package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// The read commands, offered to a model as tools.
//
// There is no second list of what a model may look up and no second
// implementation of how: a tool is a read command, its parameters are the
// command's flags and positionals, its description is the command's help,
// and running it runs the command in-process with -o json and captures what
// it printed. A flag added to `mail search` tomorrow is a parameter the model
// has tomorrow, and what the model reads is byte for byte what an agent
// running the CLI reads. The allow list is the one `skill --install` prints
// for Claude Code: what is safe to run unasked from a shell is what is safe
// to run unasked from a prompt.

var (
	// readCommands change nothing and are safe to run without asking. They
	// are the allow list in the suggested permissions and the tools a model
	// gets.
	readCommands = []string{
		"mail list", "mail search", "mail read", "mail thread",
		"mail mailboxes", "mail attachment list",
		"cal agenda", "cal show", "cal free", "cal calendars",
		"status",
	}
	// writeCommands reach the provider and are asked about first.
	writeCommands = []string{
		"mail send", "mail reply", "mail trash", "mail move",
		"cal create", "cal update", "cal delete",
	}
	// notATool is the read commands that tell a model nothing about the
	// mail: status is about the daemon.
	notATool = map[string]bool{"status": true}
)

// cliTools is the read commands as an ai.Toolset.
type cliTools struct {
	app    *App
	tools  []ai.Tool
	byName map[string]*toolCommand
	// mu serialises calls: each one builds a command tree bound to a child
	// App, and two at once would be two printers writing at once.
	mu sync.Mutex
}

// toolCommand is how a tool's arguments become a command line.
type toolCommand struct {
	path       []string
	positional []string          // names, in the order the command takes them
	flags      map[string]string // parameter name -> pflag value type
	hasLimit   bool
}

// AITools builds the read commands as tools for the configured model.
func (a *App) AITools() ai.Toolset {
	root := NewRoot(a.child(io.Discard))
	t := &cliTools{app: a, byName: map[string]*toolCommand{}}
	for _, path := range readCommands {
		if notATool[path] {
			continue
		}
		words := strings.Fields(path)
		cmd, _, err := root.Find(words)
		if err != nil || cmd == nil || cmd.CommandPath() != "emlcal "+path {
			continue
		}
		tool, tc := toolOf(cmd, words)
		t.tools = append(t.tools, tool)
		t.byName[tool.Name] = tc
	}
	return t
}

// child is an App sharing this one's opened resources but with its own
// output, so a command can be run in-process without touching what the
// parent is printing or the flags it was started with.
func (a *App) child(out io.Writer) *App {
	_, _ = a.Config() // memoise, so the child never loads it from disk again
	return &App{
		Stdout:     out,
		Stderr:     io.Discard,
		Stdin:      strings.NewReader(""),
		IsTTY:      false,
		Now:        a.Now,
		Factory:    a.Factory,
		ConfigPath: a.ConfigPath,
		cfg:        a.cfg,
		st:         a.st,
		blobs:      a.blobs,
		eng:        a.eng,
		logger:     a.logger,
		loc:        a.loc,
	}
}

// toolOf describes one command as a tool: positionals from its Use line,
// parameters from its own flags, the description from its help.
func toolOf(cmd *cobra.Command, path []string) (ai.Tool, *toolCommand) {
	tc := &toolCommand{path: path, flags: map[string]string{}}
	props := map[string]any{}
	var required []string
	for _, tok := range strings.Fields(cmd.Use)[1:] {
		if !strings.HasPrefix(tok, "<") || !strings.HasSuffix(tok, ">") {
			continue
		}
		name := strings.Trim(tok, "<>")
		if i := strings.Index(name, "|"); i >= 0 {
			name = name[:i]
		}
		tc.positional = append(tc.positional, name)
		required = append(required, name)
		props[name] = map[string]any{"type": "string", "description": positionalDescription(name)}
	}
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		typ, items := schemaType(f.Value.Type())
		p := map[string]any{"type": typ, "description": flagDescription(f)}
		if items != "" {
			p["items"] = map[string]any{"type": items}
		}
		props[f.Name] = p
		tc.flags[f.Name] = f.Value.Type()
		if f.Name == "limit" {
			tc.hasLimit = true
		}
	})
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	raw, _ := json.Marshal(schema)
	desc := strings.TrimSpace(cmd.Short)
	if long := strings.TrimSpace(cmd.Long); long != "" {
		desc += "\n\n" + long
	}
	return ai.Tool{Name: strings.Join(path, "_"), Description: desc, Parameters: raw}, tc
}

func positionalDescription(name string) string {
	switch name {
	case "query":
		return "the search query"
	case "id":
		return "the id, as a listing returned it"
	}
	return name
}

func flagDescription(f *pflag.Flag) string {
	d := f.Usage
	switch f.DefValue {
	case "", "0", "false", "[]":
	default:
		d += " (default " + f.DefValue + ")"
	}
	return d
}

// schemaType maps a pflag value type to JSON schema. Anything unfamiliar is
// a string: pflag parses it from one.
func schemaType(t string) (typ, items string) {
	switch t {
	case "bool":
		return "boolean", ""
	case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "count":
		return "integer", ""
	case "float32", "float64":
		return "number", ""
	case "stringSlice", "stringArray", "intSlice", "int64Slice", "uintSlice":
		return "array", "string"
	}
	return "string", ""
}

func (t *cliTools) Tools() []ai.Tool { return t.tools }

// Call runs one tool: the arguments become a command line, the command runs
// in-process with -o json, and what it printed is the result. A usage error
// comes back as the tool's error, which the model is told and can act on.
func (t *cliTools) Call(ctx context.Context, call ai.ToolCall) (string, error) {
	tc, ok := t.byName[call.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
	args := map[string]any{}
	if len(bytes.TrimSpace(call.Arguments)) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return "", fmt.Errorf("arguments are not a JSON object: %w", err)
		}
	}
	argv := append(append([]string{}, tc.path...), "-o", "json")
	pos := make([]string, len(tc.positional))
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := args[k]
		if i := indexOf(tc.positional, k); i >= 0 {
			pos[i] = argString(v)
			continue
		}
		typ, ok := tc.flags[k]
		if !ok {
			return "", fmt.Errorf("unknown parameter %q", k)
		}
		if v == nil {
			continue
		}
		switch {
		case typ == "bool":
			if b, _ := v.(bool); b {
				argv = append(argv, "--"+k)
			}
		case strings.HasSuffix(typ, "Slice") || strings.HasSuffix(typ, "Array"):
			items, ok := v.([]any)
			if !ok {
				items = []any{v}
			}
			for _, it := range items {
				argv = append(argv, "--"+k+"="+argString(it))
			}
		default:
			argv = append(argv, "--"+k+"="+argString(v))
		}
	}
	// A model that does not say how many it wants gets a screenful, not the
	// command's fifty: it has to read every row it is given.
	if tc.hasLimit && args["limit"] == nil {
		argv = append(argv, "--limit=20")
	}
	for i, p := range pos {
		if p == "" {
			return "", fmt.Errorf("%s is required", tc.positional[i])
		}
	}
	if len(pos) > 0 {
		// After "--", so a value that starts with a dash is a value.
		argv = append(append(argv, "--"), pos...)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	var out bytes.Buffer
	root := NewRoot(t.app.child(&out))
	root.SetArgs(argv)
	if err := root.ExecuteContext(ctx); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return -1
}

// argString renders a JSON value as a command-line argument. Numbers come
// out of encoding/json as float64, so 20 must not become "2e+01".
func argString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

var _ ai.Toolset = (*cliTools)(nil)
