package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// serve stands in for a llama-server: it lists one model with a 128k window,
// records the chat request and streams the given events back.
func serve(t *testing.T, status int, events ...string) (*httptest.Server, *chatRequest) {
	t.Helper()
	var got chatRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"object":"list","data":[{"id":"qwen3.8-27b","object":"model","meta":{"n_ctx":131072}}]}`)
			return
		}
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		for _, e := range events {
			if status/100 == 2 {
				fmt.Fprintf(w, "data: %s\n\n", e)
			} else {
				fmt.Fprintln(w, e)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	t.Cleanup(s.Close)
	return s, &got
}

func req() ai.Request {
	return ai.Request{Messages: []ai.Message{
		{Role: ai.RoleSystem, Content: "you draft"},
		{Role: ai.RoleUser, Content: "write"},
	}}
}

func delta(content string) string {
	b, _ := json.Marshal(content)
	return `{"choices":[{"index":0,"delta":{"content":` + string(b) + `}}]}`
}

func TestChatStreamsContent(t *testing.T) {
	s, got := serve(t, http.StatusOK,
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`,
		delta("Hoi "),
		delta("Anna"),
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	)
	c := New(Options{URL: s.URL + "/", Model: "gpt-x"})

	var out strings.Builder
	msg, err := c.Chat(context.Background(), req(), func(s string) { out.WriteString(s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.String() != "Hoi Anna" || msg.Content != "Hoi Anna" || msg.Role != ai.RoleAssistant || len(msg.ToolCalls) != 0 {
		t.Errorf("streamed %q, message %+v", out.String(), msg)
	}
	if got.Model != "gpt-x" || !got.Stream {
		t.Errorf("request = %+v", *got)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Content != "write" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if c.Describe() != "gpt-x · openai" {
		t.Errorf("Describe = %q", c.Describe())
	}
}

// With no model configured the client uses whatever the server has loaded:
// that name goes in the request and, once known, in the status line. The URL
// may be given with or without /v1.
func TestNoModelMeansWhateverIsLoaded(t *testing.T) {
	s, got := serve(t, http.StatusOK, delta("ok"), `[DONE]`)
	for _, u := range []string{s.URL, s.URL + "/v1", s.URL + "/v1/"} {
		c := New(Options{URL: u})
		if d := c.Describe(); !strings.HasSuffix(d, " · openai") || strings.Contains(d, "qwen") {
			t.Errorf("%s: Describe before any request = %q, want the address", u, d)
		}
		if _, err := c.Chat(context.Background(), req(), func(string) {}); err != nil {
			t.Fatalf("%s: Chat: %v", u, err)
		}
		if got.Model != "qwen3.8-27b" {
			t.Errorf("%s: request model = %q, want the served one", u, got.Model)
		}
		if c.Describe() != "qwen3.8-27b · openai" {
			t.Errorf("%s: Describe = %q", u, c.Describe())
		}
		if w := c.ContextWindow(); w != 131072 {
			t.Errorf("%s: ContextWindow = %d", u, w)
		}
	}
}

// The model behind an endpoint can be swapped while the client lives; what
// the server said is only believed for serverTTL.
func TestServerIsAskedAgainAfterTheTTL(t *testing.T) {
	window := 131072
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":"m","meta":{"n_ctx":%d}}]}`, window)
	}))
	defer s.Close()
	c := New(Options{URL: s.URL})
	if w := c.ContextWindow(); w != 131072 {
		t.Fatalf("ContextWindow = %d", w)
	}
	window = 262144
	if w := c.ContextWindow(); w != 131072 {
		t.Errorf("within the TTL: ContextWindow = %d, want the remembered 131072", w)
	}
	c.mu.Lock()
	c.asked = time.Now().Add(-2 * serverTTL)
	c.mu.Unlock()
	if w := c.ContextWindow(); w != 262144 {
		t.Errorf("after the TTL: ContextWindow = %d, want 262144", w)
	}
}

func TestContextWindowUnknownWhenTheServerCannotSay(t *testing.T) {
	// A plain OpenAI-style listing has no window in it.
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gpt-x","object":"model"}]}`)
	}))
	defer s.Close()
	if w := New(Options{URL: s.URL, Model: "gpt-x"}).ContextWindow(); w != 0 {
		t.Errorf("ContextWindow = %d, want 0", w)
	}
	if w := New(Options{URL: "http://127.0.0.1:1"}).ContextWindow(); w != 0 {
		t.Errorf("unreachable: ContextWindow = %d, want 0", w)
	}
}

func TestChatReportsWhatTheServerSaid(t *testing.T) {
	s, _ := serve(t, http.StatusBadRequest, `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`)
	_, err := New(Options{URL: s.URL, Model: "m"}).Chat(context.Background(), req(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "exceeds the available context size") || errors.Is(err, ai.ErrUnavailable) {
		t.Errorf("err = %v", err)
	}
}

// A llama-server that is still loading its weights answers 503: that is a
// server to wait for, not a request to fix.
func TestChatTreatsLoadingAsUnavailable(t *testing.T) {
	s, _ := serve(t, http.StatusServiceUnavailable, `{"error":{"code":503,"message":"Loading model","type":"unavailable_error"}}`)
	_, err := New(Options{URL: s.URL, Model: "m"}).Chat(context.Background(), req(), func(string) {})
	if !errors.Is(err, ai.ErrUnavailable) || !strings.Contains(err.Error(), "Loading model") {
		t.Errorf("err = %v", err)
	}
}

func TestChatReportsAnErrorMidStream(t *testing.T) {
	s, _ := serve(t, http.StatusOK, delta("Hoi"), `{"error":{"message":"slot went away"}}`)
	var out strings.Builder
	_, err := New(Options{URL: s.URL, Model: "m"}).Chat(context.Background(), req(), func(s string) { out.WriteString(s) })
	if err == nil || !strings.Contains(err.Error(), "slot went away") {
		t.Errorf("err = %v", err)
	}
	if out.String() != "Hoi" {
		t.Errorf("text before the error = %q", out.String())
	}
}

func TestChatNoticesATruncatedStream(t *testing.T) {
	s, _ := serve(t, http.StatusOK, delta("Hoi"))
	if _, err := New(Options{URL: s.URL, Model: "m"}).Chat(context.Background(), req(), func(string) {}); err == nil || !strings.Contains(err.Error(), "[DONE]") {
		t.Errorf("err = %v", err)
	}
}

func TestChatSaysWhenTheServerIsNotThere(t *testing.T) {
	s := httptest.NewServer(http.NotFoundHandler())
	addr := s.URL
	s.Close()
	_, err := New(Options{URL: addr, Model: "m"}).Chat(context.Background(), req(), func(string) {})
	if !errors.Is(err, ai.ErrUnavailable) || !strings.Contains(err.Error(), addr) {
		t.Errorf("err = %v", err)
	}
}

func TestChatStopsWhenCancelled(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			fmt.Fprint(w, `{"data":[]}`)
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", delta("Hoi"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer s.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	_, err := New(Options{URL: s.URL, Model: "m"}).Chat(ctx, req(), func(string) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestChatTimesOut(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); s.Close() })
	_, err := New(Options{URL: s.URL, Model: "m", Timeout: 50 * time.Millisecond}).Chat(context.Background(), req(), func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

// A call arrives as a head (id and name) followed by fragments of its
// argument string, interleaved by index when there are several. They come
// back whole, as JSON objects; and on the way out again the arguments are a
// string and the result names its call by ID.
func TestChatCarriesToolCalls(t *testing.T) {
	s, got := serve(t, http.StatusOK,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"abc","type":"function","function":{"name":"mail_search","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"type":"function","function":{"name":"cal_agenda","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"from:anna\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	)
	c := New(Options{URL: s.URL, Model: "m"})
	r := ai.Request{
		Messages: []ai.Message{
			{Role: ai.RoleUser, Content: "what did anna say"},
			{Role: ai.RoleAssistant, ToolCalls: []ai.ToolCall{{ID: "prev", Name: "mail_search", Arguments: json.RawMessage(`{"query":"x"}`)}}},
			{Role: ai.RoleTool, Content: "[]", ToolName: "mail_search", ToolCallID: "prev"},
		},
		Tools: []ai.Tool{{Name: "mail_search", Description: "search", Parameters: json.RawMessage(`{"type":"object"}`)}, {Name: "cal_agenda"}},
	}
	msg, err := c.Chat(context.Background(), r, func(string) {})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", msg.ToolCalls)
	}
	if tc := msg.ToolCalls[0]; tc.ID != "abc" || tc.Name != "mail_search" || string(tc.Arguments) != `{"query":"from:anna"}` {
		t.Errorf("first call = %+v (%s)", tc, tc.Arguments)
	}
	if tc := msg.ToolCalls[1]; tc.ID != "call_1" || tc.Name != "cal_agenda" || string(tc.Arguments) != `{}` {
		t.Errorf("second call = %+v (%s)", tc, tc.Arguments)
	}

	if len(got.Tools) != 2 || got.Tools[0].Function.Name != "mail_search" || string(got.Tools[1].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Errorf("tools on the wire = %+v", got.Tools)
	}
	if a := got.Messages[1]; len(a.ToolCalls) != 1 || a.ToolCalls[0].ID != "prev" || a.ToolCalls[0].Type != "function" || a.ToolCalls[0].Function.Arguments != `{"query":"x"}` {
		t.Errorf("assistant turn on the wire = %+v", a)
	}
	if tr := got.Messages[2]; tr.Role != "tool" || tr.ToolCallID != "prev" || tr.Content != "[]" {
		t.Errorf("tool result on the wire = %+v", tr)
	}
}

func TestChatRefusesArgumentsThatAreNotJSON(t *testing.T) {
	s, _ := serve(t, http.StatusOK,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"mail_search","arguments":"{\"query\":"}}]}}]}`,
		`[DONE]`,
	)
	_, err := New(Options{URL: s.URL, Model: "m"}).Chat(context.Background(), req(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("err = %v", err)
	}
}

// TestLive talks to a real server: EMLCAL_OPENAI_URL=http://localhost:8080 go test -run Live ./internal/ai/openai
func TestLive(t *testing.T) {
	u := os.Getenv("EMLCAL_OPENAI_URL")
	if u == "" {
		t.Skip("EMLCAL_OPENAI_URL not set")
	}
	c := New(Options{URL: u, Model: os.Getenv("EMLCAL_OPENAI_MODEL"), Timeout: 5 * time.Minute})
	tools := []ai.Tool{{Name: "lookup_city", Description: "Look up the city a person lives in.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"person":{"type":"string"}},"required":["person"]}`)}}
	msgs := []ai.Message{{Role: ai.RoleUser, Content: "Use the tool to find where Anna lives, then answer in one short sentence."}}

	msg, err := c.Chat(context.Background(), ai.Request{Messages: msgs, Tools: tools}, func(string) {})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	t.Logf("%s, window %d", c.Describe(), c.ContextWindow())
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "lookup_city" {
		t.Fatalf("expected one lookup_city call, got %+v", msg)
	}
	msgs = append(msgs, msg, ai.Message{Role: ai.RoleTool, Content: "Utrecht", ToolName: "lookup_city", ToolCallID: msg.ToolCalls[0].ID})
	var out strings.Builder
	if _, err := c.Chat(context.Background(), ai.Request{Messages: msgs, Tools: tools}, func(s string) { out.WriteString(s) }); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	t.Logf("answer: %s", out.String())
	if !strings.Contains(out.String(), "Utrecht") {
		t.Errorf("answer does not use the tool result: %q", out.String())
	}
}
