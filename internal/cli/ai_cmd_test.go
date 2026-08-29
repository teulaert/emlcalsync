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
