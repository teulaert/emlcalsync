package gmail

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"

	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
)

// batchResult is one part of a batch response, matched back to the id that
// produced it.
type batchResult struct {
	id     string
	status int
	msg    *gmailapi.Message // set when status == 200
	err    *googleapi.Error  // set when status != 200
}

// batchGet fetches up to batchSize messages in a single multipart/mixed
// request against https://www.googleapis.com/batch/gmail/v1.
//
// google.golang.org/api no longer ships a batch helper, so the request is
// assembled by hand: one application/http part per message holding a bare
//
//	GET /gmail/v1/users/me/messages/<id>?format=<format>
//
// request line. The response is a multipart body of application/http parts,
// each an HTTP response that is parsed with http.ReadResponse. Per-part
// statuses are returned as-is; the caller decides what to skip and what to
// retry.
func (m *Mail) batchGet(ctx context.Context, ids []string, format string) ([]batchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	units := len(ids) * unitsMessagesGet

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := m.wait(ctx, units); err != nil {
			return nil, err
		}
		m.log.Debug("gmail batch", "method", "messages.get", "format", format,
			"messages", len(ids), "units", units, "attempt", attempt)

		res, err := m.batchGetOnce(ctx, ids, format)
		if err == nil {
			return res, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		lastErr = wrapErr("messages.get (batch)", err)
		if !retryable(err) || attempt == maxAttempts {
			return nil, lastErr
		}
		delay := backoffFor(attempt, err)
		m.log.Debug("gmail batch failed, retrying", "attempt", attempt, "in", delay, "err", err)
		if err := sleepCtx(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (m *Mail) batchGetOnce(ctx context.Context, ids []string, format string) ([]batchResult, error) {
	body, contentType, err := buildBatchBody(ids, format)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.batchURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // draining for connection reuse
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// googleapi.CheckResponse consumes the body and gives us a typed
		// error, which retryable() understands.
		if err := googleapi.CheckResponse(resp); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("batch endpoint returned %s", resp.Status)
	}
	return parseBatchResponse(resp, ids)
}

// buildBatchBody assembles the multipart/mixed request.
func buildBatchBody(ids []string, format string) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i, id := range ids {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Type", "application/http")
		h.Set("Content-Transfer-Encoding", "binary")
		h.Set("Content-ID", fmt.Sprintf("<item%d>", i+1))
		pw, err := mw.CreatePart(h)
		if err != nil {
			return nil, "", fmt.Errorf("build batch part: %w", err)
		}
		line := fmt.Sprintf("GET /gmail/v1/users/%s/messages/%s?format=%s\r\n\r\n",
			me, url.PathEscape(id), url.QueryEscape(format))
		if _, err := io.WriteString(pw, line); err != nil {
			return nil, "", fmt.Errorf("write batch part: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", fmt.Errorf("close batch body: %w", err)
	}
	return buf.Bytes(), "multipart/mixed; boundary=" + mw.Boundary(), nil
}

// parseBatchResponse walks the multipart body and turns every part into a
// batchResult. Parts are matched to ids by their Content-ID
// (<response-item3> → ids[2]); when a server omits or mangles it, the
// positional order is used, which is what the API guarantees anyway.
func parseBatchResponse(resp *http.Response, ids []string) ([]batchResult, error) {
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("parse batch content type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return nil, fmt.Errorf("batch response is %s, want multipart/mixed", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		return nil, fmt.Errorf("batch response has no multipart boundary")
	}

	mr := multipart.NewReader(resp.Body, boundary)
	out := make([]batchResult, 0, len(ids))
	for pos := 0; ; pos++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read batch part %d: %w", pos, err)
		}
		raw, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, fmt.Errorf("read batch part %d: %w", pos, err)
		}
		idx := contentIDIndex(part.Header.Get("Content-ID"), pos)
		if idx < 0 || idx >= len(ids) {
			return nil, fmt.Errorf("batch part %d refers to unknown request %q",
				pos, part.Header.Get("Content-ID"))
		}
		res, err := parseBatchPart(ids[idx], raw)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// parseBatchPart decodes one embedded HTTP response.
func parseBatchPart(id string, raw []byte) (batchResult, error) {
	hr, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		return batchResult{}, fmt.Errorf("parse response for message %s: %w", id, err)
	}
	defer hr.Body.Close()
	body, err := io.ReadAll(hr.Body)
	if err != nil {
		return batchResult{}, fmt.Errorf("read response for message %s: %w", id, err)
	}
	res := batchResult{id: id, status: hr.StatusCode}
	if hr.StatusCode/100 != 2 {
		hr.Body = io.NopCloser(bytes.NewReader(body))
		gerr := &googleapi.Error{Code: hr.StatusCode, Body: string(body), Header: hr.Header}
		if apiErr := googleapi.CheckResponse(hr); apiErr != nil {
			if typed, ok := apiErr.(*googleapi.Error); ok {
				typed.Header = hr.Header
				gerr = typed
			}
		}
		res.err = gerr
		return res, nil
	}
	var msg gmailapi.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return batchResult{}, fmt.Errorf("decode message %s: %w", id, err)
	}
	if msg.Id == "" {
		msg.Id = id
	}
	res.msg = &msg
	return res, nil
}

// contentIDIndex maps "<response-item7>" to index 6, falling back to pos.
func contentIDIndex(contentID string, pos int) int {
	cid := strings.Trim(contentID, "<>")
	if i := strings.LastIndex(cid, "item"); i >= 0 {
		if n, err := strconv.Atoi(cid[i+len("item"):]); err == nil && n >= 1 {
			return n - 1
		}
	}
	return pos
}
