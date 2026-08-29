package ollama

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

// serve stands in for Ollama: it records the chat request and streams the
// given lines back. It reports no loaded models and a 32k native window.
func serve(t *testing.T, status int, lines ...string) (*httptest.Server, *chatRequest) {
	t.Helper()
	var got chatRequest
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			fmt.Fprint(w, `{"models":[]}`)
			return
		case "/api/show":
			fmt.Fprint(w, `{"model_info":{"qwen3.context_length":32768,"qwen3.embedding_length":4096}}`)
			return
		}
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		for _, l := range lines {
			fmt.Fprintln(w, l)
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

func TestChatStreamsContent(t *testing.T) {
	s, got := serve(t, http.StatusOK,
		`{"model":"m","message":{"role":"assistant","content":"Hoi "},"done":false}`,
		`{"model":"m","message":{"role":"assistant","thinking":"hmm","content":""},"done":false}`,
		`{"model":"m","message":{"role":"assistant","content":"Anna"},"done":false}`,
		`{"model":"m","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop"}`,
	)
	c := New(Options{URL: s.URL + "/", Model: "qwen3:8b"})

	var out strings.Builder
	if err := c.Chat(context.Background(), req(), func(s string) { out.WriteString(s) }); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if out.String() != "Hoi Anna" {
		t.Errorf("streamed %q, want %q", out.String(), "Hoi Anna")
	}
	if got.Model != "qwen3:8b" || !got.Stream {
		t.Errorf("request = %+v", *got)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[1].Content != "write" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if c.Describe() != "qwen3:8b · ollama" {
		t.Errorf("Describe = %q", c.Describe())
	}
}

// The window is never set -- that would make the server reload the model --
// only read, and read from the running model when there is one.
func TestContextWindowIsReadNotSet(t *testing.T) {
	var (
		chats, ps, shows int
		loaded           bool
	)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			chats++
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["options"]; ok {
				t.Errorf("chat sent options: %v", body["options"])
			}
			fmt.Fprintln(w, `{"message":{"content":"x"},"done":true}`)
		case "/api/ps":
			ps++
			if loaded {
				fmt.Fprint(w, `{"models":[{"name":"other:latest","context_length":4096},{"name":"qwen3.8:latest","model":"qwen3.8:latest","context_length":262144}]}`)
			} else {
				fmt.Fprint(w, `{"models":[]}`)
			}
		case "/api/show":
			shows++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "qwen3.8:latest" {
				t.Errorf("show asked for %q", body["model"])
			}
			fmt.Fprint(w, `{"model_info":{"general.architecture":"qwen35","qwen35.context_length":131072}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)

	// Not loaded: the native length is what the server would load it at.
	c := New(Options{URL: s.URL, Model: "qwen3.8:latest"})
	if got := c.ContextWindow(); got != 131072 {
		t.Errorf("ContextWindow (not loaded) = %d, want the native 131072", got)
	}
	if ps != 1 || shows != 1 {
		t.Errorf("ps=%d shows=%d, want one of each", ps, shows)
	}
	// Remembered: no second round trip.
	c.ContextWindow()
	if ps != 1 || shows != 1 {
		t.Errorf("a second ContextWindow went to the server: ps=%d shows=%d", ps, shows)
	}

	// Loaded: the running window wins, and the right model's.
	loaded = true
	c = New(Options{URL: s.URL, Model: "qwen3.8:latest"})
	if got := c.ContextWindow(); got != 262144 {
		t.Errorf("ContextWindow (loaded) = %d, want the running 262144", got)
	}
	if shows != 1 {
		t.Error("show was asked although ps had the answer")
	}
	if err := c.Chat(context.Background(), req(), func(string) {}); err != nil {
		t.Fatal(err)
	}
	if chats != 1 {
		t.Errorf("chats = %d", chats)
	}
}

func TestContextWindowUnknownWhenTheServerCannotSay(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(s.Close)
	c := New(Options{URL: s.URL, Model: "m"})
	if got := c.ContextWindow(); got != 0 {
		t.Errorf("ContextWindow = %d, want 0 for unknown", got)
	}
}

func TestChatReportsAMissingModel(t *testing.T) {
	s, _ := serve(t, http.StatusNotFound, `{"error":"model 'qwen3:900b' not found"}`)
	c := New(Options{URL: s.URL, Model: "qwen3:900b"})
	err := c.Chat(context.Background(), req(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "ollama pull qwen3:900b") {
		t.Errorf("err = %v, want the pull hint", err)
	}
}

func TestChatReportsAnErrorMidStream(t *testing.T) {
	s, _ := serve(t, http.StatusOK,
		`{"message":{"content":"Hoi"},"done":false}`,
		`{"error":"out of memory"}`,
	)
	c := New(Options{URL: s.URL, Model: "m"})
	var out strings.Builder
	err := c.Chat(context.Background(), req(), func(s string) { out.WriteString(s) })
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Errorf("err = %v", err)
	}
	if out.String() != "Hoi" {
		t.Errorf("text before the error should have been emitted, got %q", out.String())
	}
}

func TestChatSaysWhenTheServerIsNotThere(t *testing.T) {
	s, _ := serve(t, http.StatusOK)
	url := s.URL
	s.Close()
	c := New(Options{URL: url, Model: "m"})
	err := c.Chat(context.Background(), req(), func(string) {})
	if !errors.Is(err, ai.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), url) || !strings.Contains(err.Error(), "is it running") {
		t.Errorf("err = %v, want the address and a nudge", err)
	}
}

func TestChatStopsWhenCancelled(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"content":"Hoi"},"done":false}`)
		w.(http.Flusher).Flush()
		<-release // hang until the test lets go
	}))
	t.Cleanup(func() { close(release); s.Close() })

	c := New(Options{URL: s.URL, Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- c.Chat(ctx, req(), func(s string) {
			if s == "Hoi" {
				cancel()
			}
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chat did not return after cancel")
	}
}

func TestChatTimesOut(t *testing.T) {
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); s.Close() })

	c := New(Options{URL: s.URL, Model: "m", Timeout: 50 * time.Millisecond})
	err := c.Chat(context.Background(), req(), func(string) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// TestLive runs against a real server when EMLCAL_TEST_OLLAMA names one, e.g.
//
//	EMLCAL_TEST_OLLAMA=http://10.42.0.3:11434 EMLCAL_TEST_OLLAMA_MODEL=qwen3.8:latest go test ./internal/ai/ollama -run TestLive -v
//
// It is skipped otherwise: the fakes above are what CI sees. What it checks
// is only that the wire format still matches -- a stream arrives, ends with
// done, and a missing model is reported the way the hint expects.
func TestLive(t *testing.T) {
	url := os.Getenv("EMLCAL_TEST_OLLAMA")
	if url == "" {
		t.Skip("EMLCAL_TEST_OLLAMA not set")
	}
	model := os.Getenv("EMLCAL_TEST_OLLAMA_MODEL")
	if model == "" {
		t.Skip("EMLCAL_TEST_OLLAMA_MODEL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	c := New(Options{URL: url, Model: model})
	if w := c.ContextWindow(); w <= 0 {
		t.Errorf("ContextWindow = %d, want the server to say", w)
	} else {
		t.Logf("window %d", w)
	}
	var out strings.Builder
	err := c.Chat(ctx, ai.Request{Messages: []ai.Message{
		{Role: ai.RoleSystem, Content: "Answer with exactly one word."},
		{Role: ai.RoleUser, Content: "What colour is the sky on a clear day?"},
	}}, func(s string) { out.WriteString(s) })
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	t.Logf("%s said %q", c.Describe(), out.String())
	if strings.TrimSpace(out.String()) == "" {
		t.Error("no content came back")
	}

	err = New(Options{URL: url, Model: "emlcal-no-such-model:latest"}).Chat(ctx, ai.Request{
		Messages: []ai.Message{{Role: ai.RoleUser, Content: "hi"}},
	}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "ollama pull emlcal-no-such-model:latest") {
		t.Errorf("missing model: err = %v, want the pull hint", err)
	}
}
