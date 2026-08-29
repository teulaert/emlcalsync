package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/teulaert/emlcalsync/internal/ai"
)

func TestAISummarize(t *testing.T) {
	env := mailSeed(t)
	ids := env.MustRun("mail", "list", "--thread", "-o", "json")
	var threads []map[string]any
	if err := json.Unmarshal([]byte(ids), &threads); err != nil || len(threads) == 0 {
		t.Fatalf("mail list --thread: %v\n%s", err, ids)
	}
	id, _ := threads[0]["id"].(string)

	var seen []ai.Request
	app, out, _ := env.App()
	app.AIClient = ai.Func{Name: "fake · test", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
		seen = append(seen, req)
		if len(seen) == 1 {
			return ai.Message{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{
				{Name: "mail_list", Arguments: json.RawMessage(`{"limit":1}`)},
			}}, nil
		}
		return ai.Message{Role: ai.RoleAssistant, Content: "About: hello.\nAsked of you: nothing.\nFacts: numbers attached.\nOpen: nothing"}, nil
	}}
	if code := Execute([]string{"ai", "summarize", id, "-o", "json"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var got aiSummaryOut
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.ID != id || got.Model != "fake · test" || !strings.HasPrefix(got.Summary, "About: hello.") {
		t.Errorf("out = %+v", got)
	}
	if len(got.Lookups) != 1 || got.Lookups[0].Tool != "mail_list" {
		t.Errorf("lookups = %+v", got.Lookups)
	}
	if len(seen) != 2 || len(seen[0].Tools) == 0 {
		t.Fatalf("model called %d times, tools on first: %d", len(seen), len(seen[0].Tools))
	}
	if last := seen[1].Messages[len(seen[1].Messages)-1]; last.Role != ai.RoleTool || !strings.Contains(last.Content, `"id"`) {
		t.Errorf("the lookup's JSON was not handed back: %+v", last)
	}
	if !strings.Contains(seen[0].Messages[1].Content, "No question was asked") {
		t.Error("the summary was not asked for")
	}

	// A question, plain output, no lookups offered.
	app, out, _ = env.App()
	seen = nil
	app.AIClient = ai.Func{Name: "fake", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
		seen = append(seen, req)
		return ai.Message{Role: ai.RoleAssistant, Content: "Yes, in the first message."}, nil
	}}
	if code := Execute([]string{"ai", "summarize", id, "--ask", "were numbers attached?", "--no-lookups", "-o", "plain"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if out.String() != "Yes, in the first message.\n" {
		t.Errorf("plain output = %q", out.String())
	}
	if len(seen) != 1 || seen[0].Tools != nil || !strings.Contains(seen[0].Messages[1].Content, "were numbers attached?") {
		t.Errorf("request = %+v", seen)
	}

	// No model configured is a usage error, not a crash.
	_, errOut, code := env.Run("ai", "summarize", id)
	if code != 2 || !strings.Contains(errOut, "no AI model configured") {
		t.Errorf("without a model: exit %d, %s", code, errOut)
	}
	// And the model can never call itself.
	for _, tool := range app.AITools().Tools() {
		if strings.HasPrefix(tool.Name, "ai_") {
			t.Errorf("ai commands must not be tools: %s", tool.Name)
		}
	}
}

func TestAIDraft(t *testing.T) {
	env := mailSeed(t)
	// m-unread: from alice, thread t-1, which also holds m-work from bob.
	var seen []ai.Request
	fakeModel := func(body string) ai.Client {
		return ai.Func{Name: "fake · test", Run: func(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
			seen = append(seen, req)
			return ai.Message{Role: ai.RoleAssistant, Content: body}, nil
		}}
	}

	// Printed, not stored: the reply as the composer would open it.
	app, out, _ := env.App()
	app.AIClient = fakeModel("Hi Alice,\n\nGot the numbers, thanks.\n\nMe")
	if code := Execute([]string{"ai", "draft", "work:m-unread", "--intent", "thank her", "-o", "json"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var got aiDraftOut
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if got.ID != "work:m-unread" || got.Subject != "Re: Hello there" || got.From != "me@example.com" ||
		len(got.To) != 1 || got.To[0] != "alice@example.com" || got.Intent != "thank her" ||
		!strings.HasPrefix(got.Body, "Hi Alice,") || strings.Contains(got.Body, ">") {
		t.Errorf("out = %+v", got)
	}
	if len(seen) != 1 || len(seen[0].Tools) == 0 {
		t.Fatalf("model called %d times, tools on first: %d", len(seen), len(seen[0].Tools))
	}
	user := seen[0].Messages[1].Content
	// The whole thread, the message answered marked, the intent.
	for _, want := range []string{"hello, the numbers are attached", "The message being answered ---\nId: work:m-unread", "thank her"} {
		if !strings.Contains(user, want) {
			t.Errorf("prompt lacks %q:\n%s", want, user)
		}
	}
	if n := len(env.Mail["work"].Drafts()); n != 0 {
		t.Errorf("without --save %d drafts were stored", n)
	}

	// Plain: the body alone, which pipes into `mail reply --body-file -`.
	app, out, _ = env.App()
	app.AIClient = fakeModel("Hi Alice, thanks.")
	if code := Execute([]string{"ai", "draft", "work:m-unread", "-o", "plain", "--no-lookups"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if out.String() != "Hi Alice, thanks.\n" || seen[len(seen)-1].Tools != nil {
		t.Errorf("plain = %q, tools = %v", out.String(), seen[len(seen)-1].Tools)
	}

	// A thread id answers its newest message; --all keeps the others; the
	// intent is optional.
	app, out, _ = env.App()
	app.AIClient = fakeModel("Hoi allemaal.")
	if code := Execute([]string{"ai", "draft", "work:t:t-1", "--all", "-o", "json"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	got = aiDraftOut{}
	_ = json.Unmarshal(out.Bytes(), &got)
	if got.ID != "work:m-unread" {
		t.Errorf("thread id resolved to %q, want the newest message m-unread", got.ID)
	}
	if !strings.Contains(seen[len(seen)-1].Messages[1].Content, "gave no instructions") {
		t.Error("no intent should say so")
	}

	// --dry-run prints the message --save would store.
	app, out, _ = env.App()
	app.AIClient = fakeModel("Hi Alice, thanks.")
	if code := Execute([]string{"ai", "draft", "work:m-unread", "--dry-run"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, want := range []string{"Subject: Re: Hello there", "In-Reply-To:", "Hi Alice, thanks.", "> hello, the numbers"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dry run lacks %q:\n%s", want, out.String())
		}
	}
	if n := len(env.Mail["work"].Drafts()); n != 0 {
		t.Errorf("--dry-run stored %d drafts", n)
	}

	// --save stores it on the server, in the thread, and sends nothing.
	app, out, _ = env.App()
	app.AIClient = fakeModel("Hi Alice, thanks.")
	if code := Execute([]string{"ai", "draft", "work:m-unread", "--save", "-o", "json"}, app); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	drafts := env.Mail["work"].Drafts()
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if d := string(drafts[0]); !strings.Contains(d, "Subject: Re: Hello there") || !strings.Contains(d, "Hi Alice, thanks.") || !strings.Contains(d, "In-Reply-To:") {
		t.Errorf("draft:\n%s", d)
	}
	if n := len(env.Mail["work"].Sent()); n != 0 {
		t.Errorf("--save sent %d messages", n)
	}
	if !strings.Contains(out.String(), `"subject":"Re: Hello there"`) {
		t.Errorf("save row = %s", out.String())
	}

	// Not a message or thread id, and a missing one.
	app, _, errOut := env.App()
	app.AIClient = fakeModel("x")
	if code := Execute([]string{"ai", "draft", "work:c:cal:ev"}, app); code != 2 || !strings.Contains(errOut.String(), "not a message or thread id") {
		t.Errorf("event id: exit %d, %s", code, errOut.String())
	}
	app, _, errOut = env.App()
	app.AIClient = fakeModel("x")
	if code := Execute([]string{"ai", "draft", "work:t:nope"}, app); code != 3 {
		t.Errorf("missing thread: exit %d, %s", code, errOut.String())
	}
}
