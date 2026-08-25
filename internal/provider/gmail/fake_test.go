package gmail

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"

	gmailapi "google.golang.org/api/gmail/v1"
)

// fakeGmail is a hand-rolled stand-in for the Gmail API: enough of
// users.{labels,messages,history,drafts,profile} plus the multipart batch
// endpoint to exercise the provider end to end.
type fakeGmail struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	labels    []*gmailapi.Label
	counts    map[string][2]int64 // label id -> {total, unread}
	messages  map[string]*gmailapi.Message
	order     []string // enumeration order
	pageSize  int
	history   []*gmailapi.History
	historyID uint64

	// injected behaviour
	historyGone bool           // history.list answers 404
	failOnce    map[string]int // message id -> status returned on its first get

	// observations
	gets        int
	batchCalls  int
	listCalls   int
	labelGets   int
	lastModify  *gmailapi.BatchModifyMessagesRequest
	trashed     []string
	sentRaw     []string
	sentThread  string
	draftedRaw  string
	attachments map[string]string // "<msg>/<att>" -> base64url data
}

func newFakeGmail(t *testing.T) *fakeGmail {
	t.Helper()
	f := &fakeGmail{
		t:           t,
		counts:      map[string][2]int64{},
		messages:    map[string]*gmailapi.Message{},
		failOnce:    map[string]int{},
		attachments: map[string]string{},
		pageSize:    3,
		historyID:   1000,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /gmail/v1/users/{user}/profile", f.handleProfile)
	mux.HandleFunc("GET /gmail/v1/users/{user}/labels", f.handleLabels)
	mux.HandleFunc("GET /gmail/v1/users/{user}/labels/{id}", f.handleLabelGet)
	mux.HandleFunc("GET /gmail/v1/users/{user}/messages", f.handleMessagesList)
	mux.HandleFunc("GET /gmail/v1/users/{user}/messages/{id}", f.handleMessageGet)
	mux.HandleFunc("GET /gmail/v1/users/{user}/messages/{mid}/attachments/{id}", f.handleAttachment)
	mux.HandleFunc("GET /gmail/v1/users/{user}/history", f.handleHistory)
	mux.HandleFunc("POST /gmail/v1/users/{user}/messages/batchModify", f.handleBatchModify)
	mux.HandleFunc("POST /gmail/v1/users/{user}/messages/send", f.handleSend)
	mux.HandleFunc("POST /gmail/v1/users/{user}/messages/{id}/trash", f.handleTrash)
	mux.HandleFunc("POST /gmail/v1/users/{user}/drafts", f.handleDraftCreate)
	mux.HandleFunc("POST /batch/gmail/v1", f.handleBatch)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.t.Errorf("fake gmail: unexpected request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// options returns Options wired to this fake.
func (f *fakeGmail) options() Options {
	return Options{
		HTTPClient: f.srv.Client(),
		Email:      "test@example.com",
		Endpoint:   f.srv.URL + "/",
		Logger:     discardLogger(),
	}
}

func (f *fakeGmail) addMessage(id, threadID string, labels []string, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages[id] = &gmailapi.Message{
		Id:           id,
		ThreadId:     threadID,
		LabelIds:     labels,
		Raw:          base64.RawURLEncoding.EncodeToString([]byte(raw)),
		InternalDate: 1700000000000,
		SizeEstimate: int64(len(raw)),
		HistoryId:    f.historyID,
	}
	f.order = append(f.order, id)
}

func (f *fakeGmail) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("fake gmail: encode response: %v", err)
	}
}

// apiError writes a Google-shaped error document.
func apiError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"errors":[{"reason":%q,"message":%q}]}}`,
		status, message, reason, message)
}

func (f *fakeGmail) handleProfile(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeJSON(w, &gmailapi.Profile{
		EmailAddress:  "test@example.com",
		HistoryId:     f.historyID,
		MessagesTotal: int64(len(f.messages)),
	})
}

func (f *fakeGmail) handleLabels(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeJSON(w, &gmailapi.ListLabelsResponse{Labels: f.labels})
}

func (f *fakeGmail) handleLabelGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labelGets++
	for _, l := range f.labels {
		if l.Id == id {
			cp := *l
			if c, ok := f.counts[id]; ok {
				cp.MessagesTotal, cp.MessagesUnread = c[0], c[1]
			}
			f.writeJSON(w, &cp)
			return
		}
	}
	apiError(w, http.StatusNotFound, "notFound", "label not found")
}

func (f *fakeGmail) handleMessagesList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++

	start := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(tok)
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalidArgument", "bad page token")
			return
		}
		start = n
	}
	size := f.pageSize
	if v := r.URL.Query().Get("maxResults"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n < size {
			size = n
		}
	}
	ids := f.order
	if r.URL.Query().Get("includeSpamTrash") != "true" {
		var visible []string
		for _, id := range ids {
			if !hasLabel(f.messages[id], "SPAM") && !hasLabel(f.messages[id], "TRASH") {
				visible = append(visible, id)
			}
		}
		ids = visible
	}
	end := min(start+size, len(ids))
	resp := &gmailapi.ListMessagesResponse{}
	for _, id := range ids[start:end] {
		resp.Messages = append(resp.Messages, &gmailapi.Message{Id: id, ThreadId: f.messages[id].ThreadId})
	}
	if end < len(ids) {
		resp.NextPageToken = strconv.Itoa(end)
	}
	f.writeJSON(w, resp)
}

func hasLabel(m *gmailapi.Message, label string) bool {
	if m == nil {
		return false
	}
	for _, l := range m.LabelIds {
		if l == label {
			return true
		}
	}
	return false
}

// message renders one messages.get response, honouring format and any
// injected one-off failure. It returns the HTTP status and the body.
func (f *fakeGmail) message(id, format string) (int, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if status, ok := f.failOnce[id]; ok {
		delete(f.failOnce, id)
		return status, []byte(fmt.Sprintf(
			`{"error":{"code":%d,"message":"rate limited","errors":[{"reason":"rateLimitExceeded"}]}}`, status))
	}
	msg, ok := f.messages[id]
	if !ok {
		return http.StatusNotFound, []byte(
			`{"error":{"code":404,"message":"Requested entity was not found.","errors":[{"reason":"notFound"}]}}`)
	}
	cp := *msg
	if format != "raw" {
		cp.Raw = ""
	}
	b, err := json.Marshal(&cp)
	if err != nil {
		f.t.Fatalf("fake gmail: marshal message: %v", err)
	}
	return http.StatusOK, b
}

func (f *fakeGmail) handleMessageGet(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "full"
	}
	status, body := f.message(r.PathValue("id"), format)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func (f *fakeGmail) handleHistory(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.historyGone {
		apiError(w, http.StatusNotFound, "notFound", "startHistoryId is too old")
		return
	}
	start, _ := strconv.ParseUint(r.URL.Query().Get("startHistoryId"), 10, 64)
	page := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		page, _ = strconv.Atoi(tok)
	}
	var records []*gmailapi.History
	for _, h := range f.history {
		if h.Id > start {
			records = append(records, h)
		}
	}
	resp := &gmailapi.ListHistoryResponse{HistoryId: f.historyID}
	// One record per page, so pagination is always exercised.
	if page < len(records) {
		resp.History = records[page : page+1]
		if page+1 < len(records) {
			resp.NextPageToken = strconv.Itoa(page + 1)
		}
	}
	f.writeJSON(w, resp)
}

func (f *fakeGmail) handleBatchModify(w http.ResponseWriter, r *http.Request) {
	var req gmailapi.BatchModifyMessagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalidArgument", err.Error())
		return
	}
	f.mu.Lock()
	f.lastModify = &req
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeGmail) handleTrash(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	msg, ok := f.messages[id]
	if !ok {
		apiError(w, http.StatusNotFound, "notFound", "message not found")
		return
	}
	f.trashed = append(f.trashed, id)
	f.writeJSON(w, msg)
}

func (f *fakeGmail) handleSend(w http.ResponseWriter, r *http.Request) {
	var msg gmailapi.Message
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		apiError(w, http.StatusBadRequest, "invalidArgument", err.Error())
		return
	}
	f.mu.Lock()
	f.sentRaw = append(f.sentRaw, msg.Raw)
	f.sentThread = msg.ThreadId
	f.mu.Unlock()
	f.writeJSON(w, &gmailapi.Message{Id: "sent-1", ThreadId: msg.ThreadId})
}

func (f *fakeGmail) handleDraftCreate(w http.ResponseWriter, r *http.Request) {
	var d gmailapi.Draft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		apiError(w, http.StatusBadRequest, "invalidArgument", err.Error())
		return
	}
	f.mu.Lock()
	if d.Message != nil {
		f.draftedRaw = d.Message.Raw
	}
	f.mu.Unlock()
	f.writeJSON(w, &gmailapi.Draft{Id: "draft-1", Message: &gmailapi.Message{Id: "draft-msg-1", ThreadId: "t-draft"}})
}

func (f *fakeGmail) handleAttachment(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("mid") + "/" + r.PathValue("id")
	f.mu.Lock()
	data, ok := f.attachments[key]
	f.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "notFound", "attachment not found")
		return
	}
	f.writeJSON(w, &gmailapi.MessagePartBody{AttachmentId: r.PathValue("id"), Data: data, Size: int64(len(data))})
}

// handleBatch implements the multipart/mixed batch endpoint.
func (f *fakeGmail) handleBatch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.batchCalls++
	f.mu.Unlock()

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || params["boundary"] == "" {
		apiError(w, http.StatusBadRequest, "invalidArgument", "not a multipart request")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])

	type reply struct {
		contentID string
		status    int
		body      []byte
	}
	var replies []reply
	for i := 0; ; i++ {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalidArgument", err.Error())
			return
		}
		raw, _ := io.ReadAll(part)
		part.Close()

		line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "GET" {
			apiError(w, http.StatusBadRequest, "invalidArgument", "bad sub-request "+line)
			return
		}
		target := fields[1]
		format := "full"
		if i := strings.Index(target, "?"); i >= 0 {
			q := target[i+1:]
			target = target[:i]
			for _, kv := range strings.Split(q, "&") {
				if strings.HasPrefix(kv, "format=") {
					format = strings.TrimPrefix(kv, "format=")
				}
			}
		}
		id := target[strings.LastIndex(target, "/")+1:]
		status, body := f.message(id, format)
		cid := part.Header.Get("Content-ID")
		replies = append(replies, reply{
			contentID: "<response-" + strings.Trim(cid, "<>") + ">",
			status:    status,
			body:      body,
		})
	}

	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	w.WriteHeader(http.StatusOK)
	for _, rep := range replies {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Type", "application/http")
		h.Set("Content-ID", rep.contentID)
		pw, err := mw.CreatePart(h)
		if err != nil {
			f.t.Errorf("fake gmail: batch part: %v", err)
			return
		}
		fmt.Fprintf(pw, "HTTP/1.1 %d %s\r\nContent-Type: application/json; charset=UTF-8\r\nContent-Length: %d\r\n\r\n",
			rep.status, http.StatusText(rep.status), len(rep.body))
		pw.Write(rep.body)
		io.WriteString(pw, "\r\n")
	}
	mw.Close()
}
