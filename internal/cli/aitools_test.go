package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
	"github.com/teulaert/emlcalsync/internal/ai/ollama"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/provider/fake"
)

func toolNamed(t *testing.T, ts ai.Toolset, name string) ai.Tool {
	t.Helper()
	for _, tool := range ts.Tools() {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool %q; have %v", name, toolNames(ts))
	return ai.Tool{}
}

func toolNames(ts ai.Toolset) []string {
	var out []string
	for _, tool := range ts.Tools() {
		out = append(out, tool.Name)
	}
	return out
}

func TestAIToolsAreTheReadCommands(t *testing.T) {
	env := mailSeed(t)
	app, _, _ := env.App()
	defer app.Close()
	ts := app.AITools()

	names := toolNames(ts)
	for _, want := range []string{"mail_list", "mail_search", "mail_read", "mail_thread", "mail_mailboxes", "mail_attachment_list", "cal_agenda", "cal_show", "cal_free", "cal_calendars"} {
		if !strings.Contains(" "+strings.Join(names, " ")+" ", " "+want+" ") {
			t.Errorf("missing tool %q in %v", want, names)
		}
	}
	for _, n := range names {
		for _, w := range writeCommands {
			if n == strings.ReplaceAll(w, " ", "_") {
				t.Errorf("a write command is a tool: %s", n)
			}
		}
		if n == "status" || n == "sync" || n == "tui" {
			t.Errorf("%s should not be a tool", n)
		}
	}

	// The schema is the command's flags and positionals, described by its
	// help.
	search := toolNamed(t, ts, "mail_search")
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(search.Parameters, &schema); err != nil {
		t.Fatalf("schema: %v\n%s", err, search.Parameters)
	}
	if schema.Type != "object" || len(schema.Required) != 1 || schema.Required[0] != "query" {
		t.Errorf("schema = %+v", schema)
	}
	for name, typ := range map[string]string{"query": "string", "since": "string", "from": "string", "unread": "boolean", "limit": "integer"} {
		p, ok := schema.Properties[name]
		if !ok || p["type"] != typ {
			t.Errorf("property %s = %v, want type %s", name, p, typ)
		}
	}
	if d, _ := schema.Properties["since"]["description"].(string); !strings.Contains(d, "2026-08-01") {
		t.Errorf("since description = %q, want the flag's usage", d)
	}
	if d, _ := schema.Properties["limit"]["description"].(string); !strings.Contains(d, "default 50") {
		t.Errorf("limit description = %q, want the default named", d)
	}
	if !strings.Contains(search.Description, "FTS5") {
		t.Errorf("description should be the command's help:\n%s", search.Description)
	}
	if _, ok := schema.Properties["format"]; ok {
		t.Error("the global --format flag is not a parameter")
	}
}

func TestAIToolsRunTheCommandAndReturnItsJSON(t *testing.T) {
	env := mailSeed(t)
	app, parentOut, _ := env.App()
	defer app.Close()
	ts := app.AITools()
	ctx := context.Background()

	// mail_list: a JSON array of rows with ids, capped at a screenful.
	out, err := ts.Call(ctx, ai.ToolCall{Name: "mail_list", Arguments: json.RawMessage(`{"limit": 2}`)})
	if err != nil {
		t.Fatalf("mail_list: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("mail_list did not return JSON: %v\n%s", err, out)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (limit as a number)", len(rows))
	}
	id, _ := rows[0]["id"].(string)
	subject, _ := rows[0]["subject"].(string)
	if id == "" || subject == "" {
		t.Fatalf("row = %v", rows[0])
	}
	if parentOut.Len() != 0 {
		t.Error("the tool wrote to the parent's stdout")
	}

	// mail_read on an id a listing returned: the same shape `mail read`
	// prints, body included.
	out, err = ts.Call(ctx, ai.ToolCall{Name: "mail_read", Arguments: json.RawMessage(`{"id":"` + id + `"}`)})
	if err != nil {
		t.Fatalf("mail_read: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(out), &msg); err != nil || msg["subject"] != subject {
		t.Errorf("mail_read = %s (err %v), want subject %q", out, err, subject)
	}

	// mail_search with a word from that subject finds it.
	word := strings.Fields(subject)[0]
	out, err = ts.Call(ctx, ai.ToolCall{Name: "mail_search", Arguments: json.RawMessage(`{"query":"` + word + `"}`)})
	if err != nil {
		t.Fatalf("mail_search: %v", err)
	}
	if !strings.Contains(out, id) {
		t.Errorf("mail_search %q did not find %s:\n%s", word, id, out)
	}

	// A boolean flag, a listing the default limit applies to.
	if _, err := ts.Call(ctx, ai.ToolCall{Name: "mail_list", Arguments: json.RawMessage(`{"unread": true}`)}); err != nil {
		t.Errorf("mail_list unread: %v", err)
	}

	// Mistakes come back as errors the model can read, not panics.
	cases := map[string]string{
		`{"query":"x","nonsense":1}`: "unknown parameter",
		`{}`:                         "query is required",
		`{"query":"\"unbalanced"}`:   "quote",
	}
	for args, want := range cases {
		_, err := ts.Call(ctx, ai.ToolCall{Name: "mail_search", Arguments: json.RawMessage(args)})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("mail_search %s: err = %v, want %q", args, err, want)
		}
	}
	if _, err := ts.Call(ctx, ai.ToolCall{Name: "mail_send", Arguments: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("mail_send: err = %v", err)
	}

	// The calendar side runs too, with the same App.
	if _, err := ts.Call(ctx, ai.ToolCall{Name: "cal_agenda", Arguments: json.RawMessage(`{"days": 3}`)}); err != nil {
		t.Errorf("cal_agenda: %v", err)
	}
}

func TestSuggestedPermissionsComeFromTheSameLists(t *testing.T) {
	s := suggestedPermissions()
	for _, r := range readCommands {
		if !strings.Contains(s, `"Bash(emlcal `+r+`*)"`) {
			t.Errorf("allow list lacks %q", r)
		}
	}
	for _, w := range writeCommands {
		if !strings.Contains(s, `"Bash(emlcal `+w+`*)"`) {
			t.Errorf("ask list lacks %q", w)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		t.Errorf("not valid JSON: %v\n%s", err, s)
	}
}

// TestLiveAgentDraft runs the whole loop against a real Ollama when
// EMLCAL_TEST_OLLAMA and EMLCAL_TEST_OLLAMA_MODEL are set: the tools built
// from the command tree go to the model, whatever it asks to look up is run
// through the CLI, and it writes a reply. It is skipped otherwise. It asserts
// only that the loop closes; what the model looked up is logged for a person
// to judge.
func TestLiveAgentDraft(t *testing.T) {
	url, modelName := os.Getenv("EMLCAL_TEST_OLLAMA"), os.Getenv("EMLCAL_TEST_OLLAMA_MODEL")
	if url == "" || modelName == "" {
		t.Skip("EMLCAL_TEST_OLLAMA / EMLCAL_TEST_OLLAMA_MODEL not set")
	}
	env := newTestEnv(t)
	now := env.Now
	// An earlier exchange that settles the price, and a new thread that
	// asks about it without saying what it was.
	env.Seed("work",
		fake.NewMsg("w-1", RawMail(t, "anna@example.com", "me@example.com", "Offerte Q3",
			"Hoi,\n\nVoor Q3 hanteren we 12,50 per stuk bij 400 stuks, zoals afgesproken.\n\nGroet, Anna", now.Add(-40*24*time.Hour))).
			WithReceived(now.Add(-40*24*time.Hour)).WithThread("t-old"),
		fake.NewMsg("w-2", RawMail(t, "anna@example.com", "me@example.com", "Offerte Q4",
			"Hoi,\n\nKunnen we voor Q4 dezelfde prijs per stuk aanhouden als in Q3? Dan zet ik het door.\n\nGroet, Anna", now.Add(-time.Hour))).
			WithReceived(now.Add(-time.Hour)).WithThread("t-new").WithFlags(model.Flags{Unread: true}),
	)
	app, _, _ := env.App()
	defer app.Close()
	st, err := app.Store()
	if err != nil {
		t.Fatal(err)
	}
	_, msgs, err := st.GetThread(context.Background(), "work", "t-new", false)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("thread: %v (%d messages)", err, len(msgs))
	}

	c := ollama.New(ollama.Options{URL: url, Model: modelName, Timeout: 3 * time.Minute})
	req := ai.ReplyPrompt(ai.ReplyInput{
		Self:          model.Address{Email: "me@example.com"},
		Thread:        msgs,
		Instructions:  "ja, zelfde prijs is prima -- noem het bedrag",
		ContextWindow: c.ContextWindow(),
		Lookups:       true,
		Loc:           time.UTC,
	})
	tools := app.AITools()
	start := time.Now()
	var lookups int
	out, err := ai.Run(context.Background(), c, req, tools, ai.Observer{
		Lookup: func(call ai.ToolCall) {
			lookups++
			t.Logf("lookup %d: %s %s", lookups, call.Name, call.Arguments)
		},
		Result: func(call ai.ToolCall, res string, err error) {
			if err != nil {
				t.Logf("  -> error: %v", err)
			} else {
				t.Logf("  -> %d bytes: %.200s", len(res), res)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("%d lookup(s), %s\n--- draft ---\n%s\n---", lookups, time.Since(start).Round(time.Millisecond), ai.CleanText(out))
	if strings.TrimSpace(out) == "" {
		t.Error("empty draft")
	}
	if !strings.Contains(out, "12,50") && !strings.Contains(out, "12.50") {
		t.Logf("NOTE: the draft does not name the Q3 price; the model did not find or use it")
	}
}

// A PDF attachment is readable as text, and so through the tool: the whole
// reason the command exists. pdftotext is stood in for by a script on PATH.
func TestAIToolsReadAnAttachment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "pdftotext"), []byte("#!/bin/sh\ncat >/dev/null\necho 'FACTUUR 360954   Totaal EUR 1.234,56   vervaldatum 25-09-2026'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	env := newTestEnv(t)
	raw, err := mime.Build(&mime.Draft{
		From: model.Address{Email: "info@oostwatering.example"}, To: []model.Address{{Email: "me@example.com"}},
		Subject: "FACTUUR 360954", TextBody: "Bijgaand de factuur.", Date: env.Now.Add(-time.Hour), MessageID: "inv@example.test",
		Attachments: []mime.DraftAttachment{{Filename: "360954.pdf", ContentType: "application/pdf", Data: []byte("%PDF-1.4 stand-in")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env.Seed("work", fake.NewMsg("inv", raw).WithReceived(env.Now.Add(-time.Hour)).WithThread("t-inv"))
	app, _, _ := env.App()
	defer app.Close()
	ts := app.AITools()
	toolNamed(t, ts, "mail_attachment_text")

	out, err := ts.Call(context.Background(), ai.ToolCall{Name: "mail_attachment_text", Arguments: json.RawMessage(`{"id":"work:inv","part":"360954.pdf"}`)})
	if err != nil {
		t.Fatalf("mail_attachment_text: %v", err)
	}
	var got mailAttachmentTextOut
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !strings.Contains(got.Text, "1.234,56") || got.Filename != "360954.pdf" || got.Truncated {
		t.Errorf("out = %+v", got)
	}

	// Without pdftotext the command says what to install, and does not fail
	// the process.
	t.Setenv("PATH", bin+"-nowhere")
	_, errOut, code := env.Run("mail", "attachment", "text", "work:inv", "360954.pdf")
	if code != 2 || !strings.Contains(errOut, "poppler-utils") {
		t.Errorf("without pdftotext: exit %d, %s", code, errOut)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The command itself, plain: just the text.
	if plain := env.MustRun("mail", "attachment", "text", "work:inv", "360954.pdf", "-o", "plain"); !strings.Contains(plain, "vervaldatum 25-09-2026") {
		t.Errorf("plain = %q", plain)
	}
	// A cut is said.
	cut := env.MustRun("mail", "attachment", "text", "work:inv", "360954.pdf", "--max-chars", "10", "-o", "json")
	if !strings.Contains(cut, `"truncated":true`) || !strings.Contains(cut, "[cut:") {
		t.Errorf("cut = %s", cut)
	}
}
