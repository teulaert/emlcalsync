// Package ollama speaks to an Ollama server's /api/chat endpoint.
//
// It is a plain HTTP client rather than the official Go library: the whole
// exchange is one POST and a stream of JSON lines back, and a dependency that
// pulls its own HTTP and type surface in for that is more than it saves.
package ollama

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
	"strings"
	"sync"
	"time"

	"github.com/teulaert/emlcalsync/internal/ai"
)

// Options configures a Client.
type Options struct {
	// URL is the server, e.g. http://localhost:11434.
	URL string
	// Model is Ollama's name for the model, e.g. "qwen3:32b".
	Model string
	// Timeout bounds one whole generation; 0 means no bound beyond ctx.
	Timeout time.Duration
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
}

// Client is one model on one server.
//
// It never sets num_ctx. Ollama loads a model at whatever window it is
// configured for -- current versions, the model's full native length -- and
// asking for a different one makes it reload the model, which on a large one
// is seconds of dead time before the first token. The window is only read,
// to size the prompt, and only when asked.
type Client struct {
	url     string
	model   string
	timeout time.Duration
	http    *http.Client

	mu     sync.Mutex
	window int // 0 until discovered
}

// New builds a Client. It does not touch the network: a server that is down
// is reported by the first Chat, with the address in the message.
func New(o Options) *Client {
	c := &Client{
		url:     strings.TrimRight(o.URL, "/"),
		model:   o.Model,
		timeout: o.Timeout,
		http:    o.HTTPClient,
	}
	if c.http == nil {
		// No client-level timeout: a generation is long-lived by nature and
		// is bounded by the context instead.
		c.http = &http.Client{}
	}
	return c
}

func (c *Client) Describe() string { return c.model + " · ollama" }

// ContextWindow asks the server what the model runs at, once, and remembers
// the answer. A model that is loaded reports its running window through
// /api/ps, which is the truth; one that is not yet loaded has only its native
// length in /api/show, which is what the server loads it at unless it was
// told otherwise. A server that cannot be asked yields 0, unknown, and is
// asked again next time -- it is presumably the same server Chat is about to
// fail against.
func (c *Client) ContextWindow() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.window == 0 {
		c.window = c.discoverWindow()
	}
	return c.window
}

func (c *Client) discoverWindow() int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var ps struct {
		Models []struct {
			Name          string `json:"name"`
			Model         string `json:"model"`
			ContextLength int    `json:"context_length"`
		} `json:"models"`
	}
	if err := c.getJSON(ctx, http.MethodGet, "/api/ps", nil, &ps); err == nil {
		for _, m := range ps.Models {
			if (m.Name == c.model || m.Model == c.model) && m.ContextLength > 0 {
				return m.ContextLength
			}
		}
	}

	var show struct {
		ModelInfo map[string]any `json:"model_info"`
	}
	body, _ := json.Marshal(map[string]string{"model": c.model})
	if err := c.getJSON(ctx, http.MethodPost, "/api/show", body, &show); err != nil {
		return 0
	}
	// The key is "<architecture>.context_length"; the architecture is not
	// worth a second lookup.
	for k, v := range show.ModelInfo {
		if strings.HasSuffix(k, ".context_length") {
			if n, ok := v.(float64); ok && n > 0 {
				return int(n)
			}
		}
	}
	return 0
}

func (c *Client) getJSON(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return c.httpError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatChunk is one line of the streamed response. Thinking models put their
// reasoning in a separate field, which is left alone: only content is the
// answer.
type chatChunk struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done       bool   `json:"done"`
	DoneReason string `json:"done_reason"`
	Error      string `json:"error"`
}

// Chat implements ai.Client.
func (c *Client) Chat(ctx context.Context, req ai.Request, emit func(string)) error {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	body := chatRequest{Model: c.model, Stream: true}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, chatMessage{Role: string(m.Role), Content: m.Content})
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return c.classify(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return c.httpError(resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ch chatChunk
		if err := json.Unmarshal(line, &ch); err != nil {
			return fmt.Errorf("ollama: bad line in stream: %w", err)
		}
		if ch.Error != "" {
			return fmt.Errorf("ollama: %s", ch.Error)
		}
		if ch.Message.Content != "" {
			emit(ch.Message.Content)
		}
		if ch.Done {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return c.classify(err)
	}
	return errors.New("ollama: the stream ended without a final message")
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
		return fmt.Errorf("%w: cannot reach Ollama at %s (%v) — is it running?", ai.ErrUnavailable, c.url, err)
	}
	return fmt.Errorf("ollama: %w", err)
}

func (c *Client) httpError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	if msg == "" {
		msg = resp.Status
	}
	if resp.StatusCode == http.StatusNotFound && strings.Contains(msg, "not found") {
		return fmt.Errorf("ollama: %s — pull it first: ollama pull %s", msg, c.model)
	}
	return fmt.Errorf("ollama: %s", msg)
}

var _ ai.Client = (*Client)(nil)
