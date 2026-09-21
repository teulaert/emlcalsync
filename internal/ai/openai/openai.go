// Package openai speaks the OpenAI chat-completions API, which is what
// llama.cpp's llama-server, vLLM, LM Studio and most hosted gateways expose.
//
// Like ai/ollama it is a plain HTTP client: one POST and a stream of
// server-sent events back. It differs from Ollama's API in three ways that
// matter here. Tool-call arguments travel as a JSON string, in fragments
// spread over the stream, and have to be put back together; a tool result
// names the call it answers by ID rather than by tool; and the server may be
// serving a model emlcal never named -- a llama-server has exactly one loaded
// and ignores the name it is sent -- so the model is optional and, when left
// out, is whatever the server says it has.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// Options configures a Client.
type Options struct {
	// URL is the API root, with or without the /v1:
	// http://localhost:8080 and http://localhost:8080/v1 are the same server.
	URL string
	// Model is the name sent with each request. Empty means whatever the
	// server is serving, which is the right setting for a box where the
	// loaded model changes and the endpoint does not.
	Model string
	// Timeout bounds one whole generation; 0 means no bound beyond ctx.
	Timeout time.Duration
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
}

// serverTTL is how long what the server said about itself is believed. The
// model behind one endpoint can be swapped while a TUI is open, and with it
// the window; a minute is short enough to notice and long enough that a
// draft is not preceded by a lookup every time.
const serverTTL = time.Minute

// Client is one endpoint, and the model behind it.
type Client struct {
	url     string // ends in /v1
	model   string
	timeout time.Duration
	http    *http.Client

	mu      sync.Mutex
	asked   time.Time
	serving string // the server's own name for its model, "" when unknown
	window  int    // 0 when unknown
}

// New builds a Client. It does not touch the network: a server that is down
// is reported by the first Chat, with the address in the message.
func New(o Options) *Client {
	u := strings.TrimRight(o.URL, "/")
	if !strings.HasSuffix(u, "/v1") {
		u += "/v1"
	}
	c := &Client{url: u, model: o.Model, timeout: o.Timeout, http: o.HTTPClient}
	if c.http == nil {
		// No client-level timeout: a generation is long-lived by nature and
		// is bounded by the context instead.
		c.http = &http.Client{}
	}
	return c
}

// Describe names the configured model, or, when none was, the one the server
// was last seen serving. It never asks: a status line is drawn from the
// update loop. Before the first request that leaves only the address.
func (c *Client) Describe() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.model != "":
		return c.model + " · openai"
	case c.serving != "":
		return c.serving + " · openai"
	}
	if u, err := url.Parse(c.url); err == nil && u.Host != "" {
		return u.Host + " · openai"
	}
	return "openai"
}

// ContextWindow is what the server runs its model at, when it says.
// llama-server does, per model in /v1/models; the API proper has no field for
// it, so against anything else this is 0, unknown. A server that cannot be
// asked also yields 0 and is asked again next time.
func (c *Client) ContextWindow() int {
	_, window := c.server()
	return window
}

// server returns the served model's name and window, asking at most once per
// serverTTL.
func (c *Client) server() (string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.asked.IsZero() && time.Since(c.asked) < serverTTL {
		return c.serving, c.window
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var list struct {
		Data []struct {
			ID   string `json:"id"`
			Meta struct {
				NCtx int `json:"n_ctx"`
			} `json:"meta"`
		} `json:"data"`
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url+"/models", nil)
	if err != nil {
		return c.serving, c.window
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.serving, c.window
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 || json.NewDecoder(resp.Body).Decode(&list) != nil || len(list.Data) == 0 {
		return c.serving, c.window
	}
	// The configured model's entry when the server lists several; otherwise
	// the first, which on a llama-server is the only one.
	pick := list.Data[0]
	for _, m := range list.Data {
		if m.ID == c.model {
			pick = m
			break
		}
	}
	c.asked, c.serving, c.window = time.Now(), pick.ID, pick.Meta.NCtx
	return c.serving, c.window
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

// chatMessage is the OpenAI message shape. A tool result names the call it
// answers with tool_call_id; an assistant turn that asked for tools carries
// them in tool_calls and is echoed back.
type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// toolCall is the OpenAI call shape: the arguments are a string holding JSON,
// not the object Ollama sends.
type toolCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatChunk is one event of the streamed response. Thinking models put their
// reasoning in a separate delta field (reasoning_content), which is left
// alone: only content is the answer.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []toolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
}

func toWire(m ai.Message) chatMessage {
	out := chatMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		w := toolCall{ID: tc.ID, Type: "function"}
		w.Function.Name = tc.Name
		w.Function.Arguments = string(tc.Arguments)
		if w.Function.Arguments == "" {
			w.Function.Arguments = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, w)
	}
	return out
}

// Chat implements ai.Client.
func (c *Client) Chat(ctx context.Context, req ai.Request, emit func(string)) (ai.Message, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	body := chatRequest{Model: c.model, Stream: true}
	if body.Model == "" {
		// The field is required by the API even where the server ignores it.
		if body.Model, _ = c.server(); body.Model == "" {
			body.Model = "default"
		}
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, toWire(m))
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, chatTool{Type: "function", Function: toolFunction{
			Name: t.Name, Description: t.Description, Parameters: params,
		}})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ai.Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ai.Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ai.Message{}, c.classify(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return ai.Message{}, c.httpError(resp)
	}

	var (
		text  strings.Builder
		calls []*toolCall // by stream index: a call arrives as a head and then argument fragments
	)
	finish := func() (ai.Message, error) {
		answer := ai.Message{Role: ai.RoleAssistant, Content: text.String()}
		for i, tc := range calls {
			if tc == nil || tc.Function.Name == "" {
				continue
			}
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return ai.Message{}, fmt.Errorf("openai: the model's arguments for %s are not JSON: %.200s", tc.Function.Name, args)
			}
			id := tc.ID
			if id == "" {
				// The result has to name the call; a server that did not
				// number it gets ours back.
				id = "call_" + strconv.Itoa(i)
			}
			answer.ToolCalls = append(answer.ToolCalls, ai.ToolCall{ID: id, Name: tc.Function.Name, Arguments: json.RawMessage(args)})
		}
		return answer, nil
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue // blank separators, comments and event names
		}
		data = bytes.TrimSpace(data)
		if bytes.Equal(data, []byte("[DONE]")) {
			return finish()
		}
		var ch chatChunk
		if err := json.Unmarshal(data, &ch); err != nil {
			return ai.Message{}, fmt.Errorf("openai: bad event in stream: %w", err)
		}
		if ch.Error != nil {
			return ai.Message{}, fmt.Errorf("openai: %s", ch.Error.Message)
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				emit(choice.Delta.Content)
			}
			for _, d := range choice.Delta.ToolCalls {
				if d.Index < 0 || d.Index > 255 {
					continue
				}
				for len(calls) <= d.Index {
					calls = append(calls, nil)
				}
				if calls[d.Index] == nil {
					calls[d.Index] = &toolCall{}
				}
				tc := calls[d.Index]
				if d.ID != "" {
					tc.ID = d.ID
				}
				if d.Function.Name != "" {
					tc.Function.Name = d.Function.Name
				}
				tc.Function.Arguments += d.Function.Arguments
			}
		}
	}
	if err := sc.Err(); err != nil {
		return ai.Message{}, c.classify(err)
	}
	return ai.Message{}, errors.New("openai: the stream ended without [DONE]")
}

// classify turns a transport failure into something a status line can act
// on: a server that is not there gets its address and a nudge, a cancelled
// generation is reported as such rather than as a network fault.
func (c *Client) classify(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, context.Canceled) || errors.Is(ue.Err, context.DeadlineExceeded) {
			return ue.Err
		}
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: cannot reach the model server at %s (%v) — is a model loaded?", ai.ErrUnavailable, c.url, err)
	}
	return fmt.Errorf("openai: %w", err)
}

func (c *Client) httpError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var e struct {
		Error apiError `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		msg = e.Error.Message
	}
	if msg == "" {
		msg = resp.Status
	}
	if resp.StatusCode == http.StatusServiceUnavailable {
		// llama-server answers 503 while it is still reading the weights in.
		return fmt.Errorf("%w: %s says: %s", ai.ErrUnavailable, c.url, msg)
	}
	return fmt.Errorf("openai: %s", msg)
}

var _ ai.Client = (*Client)(nil)
