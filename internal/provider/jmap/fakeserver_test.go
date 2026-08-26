package jmap

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token"
const testAccount = "acct-1"
const testEmail = "me@example.com"

// fakeEmail is one message in the fake store.
type fakeEmail struct {
	ID         string
	BlobID     string
	ThreadID   string
	MailboxIDs map[string]bool
	Keywords   map[string]bool
	ReceivedAt time.Time
	Size       int64
}

func (e *fakeEmail) json() map[string]any {
	return map[string]any{
		"id":         e.ID,
		"blobId":     e.BlobID,
		"threadId":   e.ThreadID,
		"mailboxIds": e.MailboxIDs,
		"keywords":   e.Keywords,
		"receivedAt": e.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"size":       e.Size,
	}
}

// changeScript is a canned /changes response, keyed by sinceState.
type changeScript struct {
	Created   []string
	Updated   []string
	Destroyed []string
	NewState  string
	HasMore   bool
	ErrorType string // when set, the method returns this error type instead
}

// capturedCall records one method invocation for assertions.
type capturedCall struct {
	Name  string
	Args  map[string]any
	Using []string
}

type fakeServer struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex

	sessionState string
	sessionHits  int

	emails    map[string]*fakeEmail
	order     []string // email ids, receivedAt ascending
	blobs     map[string][]byte
	mailboxes []map[string]any

	emailState   string
	mailboxState string

	emailChanges   map[string]changeScript
	mailboxChanges map[string]changeScript
	eventChanges   map[string]changeScript

	calendars  []map[string]any
	events     map[string]map[string]any
	eventState string
	// calendarFilterPlural makes CalendarEvent/query reject {"inCalendar": id}
	// so the client's fallback to {"inCalendars": [id]} is exercised.
	calendarFilterPlural bool
	// rejectScheduling makes CalendarEvent/set reject sendSchedulingMessages.
	rejectScheduling bool

	calls []capturedCall

	// failAPI holds HTTP statuses returned by /jmap/api before it starts
	// answering normally.
	failAPI []int
	// failMethod maps a method name to the HTTP statuses /jmap/api returns for
	// the next requests that contain that method, so one call in a flow can be
	// made to fail without disturbing the others.
	failMethod map[string][]int
	// attempts counts how often each method name reached the server, failed
	// requests included. Unlike calls it is not reset by resetCalls.
	attempts map[string]int
	// methodErrors maps a method name to a JMAP method-level error type it
	// answers with ("forbidden", "accountNotSupportedByMethod"), as a server
	// refusing a capability the token does not carry would.
	methodErrors map[string]string

	// submissionPrimary overrides primaryAccounts[urn:...:submission].
	submissionPrimary string
	// calendarURN overrides the capability URN calendars are advertised under.
	// When set, urn:ietf:params:jmap:calendars is left out of the session
	// entirely, as a server with a vendor/legacy spelling would. The "-"
	// sentinel advertises no calendars capability at all.
	calendarURN string
	// calendarPrimary overrides primaryAccounts[<calendar URN>]; "-" drops the
	// entry, leaving only the account's own accountCapabilities.
	calendarPrimary string

	// sseSubs are the currently connected EventSource clients.
	sseSubs  []chan string
	sseConns int
	// failSSE holds HTTP statuses /jmap/event returns before it starts
	// streaming normally.
	failSSE []int
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{
		t:              t,
		sessionState:   "session-0",
		emails:         map[string]*fakeEmail{},
		blobs:          map[string][]byte{},
		emailState:     "email-0",
		mailboxState:   "mailbox-0",
		emailChanges:   map[string]changeScript{},
		mailboxChanges: map[string]changeScript{},
		eventChanges:   map[string]changeScript{},
		events:         map[string]map[string]any{},
		eventState:     "cal-0",
		failMethod:     map[string][]int{},
		attempts:       map[string]int{},
		methodErrors:   map[string]string{},
	}
	f.mailboxes = []map[string]any{
		{"id": "mb-inbox", "name": "Inbox", "parentId": nil, "role": "inbox", "sortOrder": 1, "totalEmails": 3, "unreadEmails": 1},
		{"id": "mb-archive", "name": "Archive", "parentId": nil, "role": "archive", "sortOrder": 2, "totalEmails": 0, "unreadEmails": 0},
		{"id": "mb-sent", "name": "Sent", "parentId": nil, "role": "sent", "sortOrder": 3, "totalEmails": 0, "unreadEmails": 0},
		{"id": "mb-drafts", "name": "Drafts", "parentId": nil, "role": "drafts", "sortOrder": 4, "totalEmails": 0, "unreadEmails": 0},
		{"id": "mb-trash", "name": "Trash", "parentId": nil, "role": "trash", "sortOrder": 5, "totalEmails": 0, "unreadEmails": 0},
		{"id": "mb-junk", "name": "Spam", "parentId": nil, "role": "junk", "sortOrder": 6, "totalEmails": 0, "unreadEmails": 0},
		{"id": "mb-proj", "name": "Project X", "parentId": "mb-inbox", "role": nil, "sortOrder": 7, "totalEmails": 2, "unreadEmails": 0},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/jmap/session", f.handleSession)
	mux.HandleFunc("/jmap/api", f.handleAPI)
	mux.HandleFunc("/jmap/download/", f.handleDownload)
	mux.HandleFunc("/jmap/upload/", f.handleUpload)
	mux.HandleFunc("/jmap/event", f.handleEvent)
	f.srv = httptest.NewServer(f.auth(mux))
	t.Cleanup(f.srv.Close)
	return f
}

// client builds a Client wired to this fake server with fast retries.
func (f *fakeServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Options{
		Token:      testToken,
		Email:      testEmail,
		SessionURL: f.srv.URL + "/jmap/session",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.retryBase = time.Millisecond
	return c
}

func (f *fakeServer) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, `{"type":"about:blank","detail":"bad token"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Test helpers

func (f *fakeServer) addEmail(e *fakeEmail, raw []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e.BlobID == "" {
		e.BlobID = "blob-" + e.ID
	}
	if raw != nil {
		f.blobs[e.BlobID] = raw
		e.Size = int64(len(raw))
	}
	f.emails[e.ID] = e
	f.order = append(f.order, e.ID)
	sort.SliceStable(f.order, func(i, j int) bool {
		return f.emails[f.order[i]].ReceivedAt.Before(f.emails[f.order[j]].ReceivedAt)
	})
}

// deleteEmail removes a message from the fake store, as a concurrent delete
// during an enumeration would.
func (f *fakeServer) deleteEmail(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.emails, id)
	f.order = slices.DeleteFunc(f.order, func(v string) bool { return v == id })
}

// refuseMethod makes one method answer with a JMAP method-level error.
func (f *fakeServer) refuseMethod(name, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methodErrors[name] = typ
}

func (f *fakeServer) email(id string) *fakeEmail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.emails[id]
}

func (f *fakeServer) captured(name string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.Name == name {
			out = append(out, c.Args)
		}
	}
	return out
}

// attemptsFor reports how many requests carrying this method name reached the
// server, including ones the fake answered with an HTTP error.
func (f *fakeServer) attemptsFor(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts[name]
}

// capturedUsing returns the "using" list sent with the first call of a method.
func (f *fakeServer) capturedUsing(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.Name == name {
			return c.Using
		}
	}
	return nil
}

func (f *fakeServer) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// pushSSE delivers a raw SSE frame to every connected EventSource client.
func (f *fakeServer) pushSSE(frame string) {
	f.mu.Lock()
	subs := append([]chan string(nil), f.sseSubs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- frame:
		case <-time.After(time.Second):
		}
	}
}

// pushStateChange sends a StateChange for the given types.
func (f *fakeServer) pushStateChange(types map[string]string) {
	payload, _ := json.Marshal(map[string]any{
		"@type":   "StateChange",
		"changed": map[string]any{testAccount: types},
	})
	f.pushSSE("event: state\ndata: " + string(payload) + "\n\n")
}

// dropSSE disconnects every EventSource client, forcing a reconnect.
func (f *fakeServer) dropSSE() {
	f.mu.Lock()
	subs := f.sseSubs
	f.sseSubs = nil
	f.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

func (f *fakeServer) sseConnections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sseConns
}

// ---------------------------------------------------------------------------
// Handlers

func (f *fakeServer) handleSession(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.sessionHits++
	state := f.sessionState
	calURN := f.calendarURN
	calAcct := f.calendarPrimary
	submissionAcct := f.submissionPrimary
	f.mu.Unlock()
	if calURN == "" {
		calURN = CapCalendars
	}
	if submissionAcct == "" {
		submissionAcct = testAccount
	}
	// The calendars account is the same one; only the URN it hangs off varies.
	if calAcct == "" {
		calAcct = testAccount
	}
	capabilities := map[string]any{
		CapCore: map[string]any{
			"maxSizeUpload":         50 << 20,
			"maxConcurrentUpload":   4,
			"maxSizeRequest":        10 << 20,
			"maxConcurrentRequests": 4,
			"maxCallsInRequest":     16,
			"maxObjectsInGet":       50,
			"maxObjectsInSet":       20,
			"collationAlgorithms":   []string{},
		},
		CapMail:       map[string]any{},
		CapSubmission: map[string]any{},
	}
	acctCapabilities := map[string]any{
		CapMail:       map[string]any{},
		CapSubmission: map[string]any{},
	}
	if calURN != "-" {
		capabilities[calURN] = map[string]any{}
		acctCapabilities[calURN] = map[string]any{}
	}

	base := f.srv.URL
	writeJSON(w, map[string]any{
		"capabilities": capabilities,
		"accounts": map[string]any{
			testAccount: map[string]any{
				"name": testEmail, "isPersonal": true, "isReadOnly": false,
				"accountCapabilities": acctCapabilities,
			},
		},
		"primaryAccounts": primaryAccounts(map[string]any{
			CapMail:       testAccount,
			CapSubmission: submissionAcct,
			calURN:        calAcct,
		}),
		"username":       testEmail,
		"apiUrl":         base + "/jmap/api",
		"downloadUrl":    base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":      base + "/jmap/upload/{accountId}",
		"eventSourceUrl": base + "/jmap/event?types={types}&closeafter={closeafter}&ping={ping}",
		"state":          state,
	})
}

// primaryAccounts drops entries whose account id is the "-" sentinel, so a
// test can model a session that does not advertise a capability at all.
func primaryAccounts(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if v == "-" {
			continue
		}
		out[k] = v
	}
	return out
}

func (f *fakeServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jmap/download/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	blobID := parts[1]
	f.mu.Lock()
	data, ok := f.blobs[blobID]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "no such blob", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	w.Write(data)
}

func (f *fakeServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	id := fmt.Sprintf("blob-up-%d", len(f.blobs)+1)
	f.blobs[id] = data
	f.mu.Unlock()
	writeJSON(w, map[string]any{
		"accountId": testAccount,
		"blobId":    id,
		"type":      r.Header.Get("Content-Type"),
		"size":      len(data),
	})
}

func (f *fakeServer) handleEvent(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if len(f.failSSE) > 0 {
		status := f.failSSE[0]
		f.failSSE = f.failSSE[1:]
		f.mu.Unlock()
		http.Error(w, "no", status)
		return
	}
	f.mu.Unlock()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 16)
	f.mu.Lock()
	f.sseSubs = append(f.sseSubs, ch)
	f.sseConns++
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-ch:
			if !open {
				return
			}
			io.WriteString(w, frame)
			flusher.Flush()
		}
	}
}

func (f *fakeServer) handleAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	if len(f.failAPI) > 0 {
		status := f.failAPI[0]
		f.failAPI = f.failAPI[1:]
		f.mu.Unlock()
		w.Header().Set("Retry-After", "0")
		http.Error(w, "temporary", status)
		return
	}
	sessionState := f.sessionState
	f.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"type":"urn:ietf:params:jmap:error:notJSON"}`, http.StatusBadRequest)
		return
	}
	var req struct {
		Using       []string          `json:"using"`
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `{"type":"urn:ietf:params:jmap:error:notJSON"}`, http.StatusBadRequest)
		return
	}

	// Count every method that reached the server and apply any scripted
	// per-method HTTP failure before doing any work.
	f.mu.Lock()
	failStatus := 0
	for _, mc := range req.MethodCalls {
		var trip []json.RawMessage
		if json.Unmarshal(mc, &trip) != nil || len(trip) == 0 {
			continue
		}
		var name string
		json.Unmarshal(trip[0], &name)
		f.attempts[name]++
		if failStatus == 0 {
			if q := f.failMethod[name]; len(q) > 0 {
				failStatus = q[0]
				f.failMethod[name] = q[1:]
			}
		}
	}
	f.mu.Unlock()
	if failStatus != 0 {
		w.Header().Set("Retry-After", "0")
		http.Error(w, "temporary", failStatus)
		return
	}

	var results []fakeResult

	for _, mc := range req.MethodCalls {
		var trip []json.RawMessage
		if err := json.Unmarshal(mc, &trip); err != nil || len(trip) != 3 {
			http.Error(w, `{"type":"urn:ietf:params:jmap:error:notRequest"}`, http.StatusBadRequest)
			return
		}
		var name, id string
		json.Unmarshal(trip[0], &name)
		json.Unmarshal(trip[2], &id)
		args := map[string]any{}
		json.Unmarshal(trip[1], &args)

		// Resolve result references ("#ids": {resultOf, name, path}).
		refErr := ""
		for k, v := range args {
			if !strings.HasPrefix(k, "#") {
				continue
			}
			ref, _ := v.(map[string]any)
			val, err := resolveResultRef(results, ref)
			if err != nil {
				refErr = err.Error()
				break
			}
			args[strings.TrimPrefix(k, "#")] = val
			delete(args, k)
		}
		if refErr != "" {
			results = append(results, fakeResult{"error", map[string]any{
				"type": "invalidResultReference", "description": refErr}, id})
			continue
		}

		f.mu.Lock()
		f.calls = append(f.calls, capturedCall{
			Name: name, Args: deepCopyArgs(args), Using: slices.Clone(req.Using),
		})
		f.mu.Unlock()

		rname, rargs := f.dispatch(name, args, req.Using)
		results = append(results, fakeResult{rname, rargs, id})
	}

	out := make([]any, 0, len(results))
	for _, r := range results {
		out = append(out, []any{r.name, r.args, r.id})
	}
	writeJSON(w, map[string]any{
		"methodResponses": out,
		"sessionState":    sessionState,
	})
}

// fakeResult is one completed method response inside a request.
type fakeResult struct {
	name string
	args map[string]any
	id   string
}

func resolveResultRef(results []fakeResult, ref map[string]any) (any, error) {
	resultOf, _ := ref["resultOf"].(string)
	wantName, _ := ref["name"].(string)
	path, _ := ref["path"].(string)
	for _, r := range results {
		if r.id != resultOf {
			continue
		}
		if r.name != wantName {
			return nil, fmt.Errorf("resultOf %q is a %q, not %q", resultOf, r.name, wantName)
		}
		key := strings.TrimPrefix(path, "/")
		v, ok := r.args[key]
		if !ok {
			return nil, fmt.Errorf("path %q not found", path)
		}
		return v, nil
	}
	return nil, fmt.Errorf("no result with id %q", resultOf)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func deepCopyArgs(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return out
}

func methodErrorArgs(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

// ---------------------------------------------------------------------------
// Method dispatch

func (f *fakeServer) dispatch(name string, args map[string]any, using []string) (string, map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if typ, ok := f.methodErrors[name]; ok {
		return "error", methodErrorArgs(typ, "scripted "+typ+" for "+name)
	}

	// RFC 8620 §3.2: a method whose capability the request did not claim in
	// "using" is an unknownMethod.
	if strings.HasPrefix(name, "Calendar") {
		urn := f.calendarURN
		if urn == "" {
			urn = CapCalendars
		}
		if !slices.Contains(using, urn) {
			return "error", methodErrorArgs("unknownMethod",
				name+" needs "+urn+" in using")
		}
	}

	switch name {
	case "Mailbox/get":
		ids, all := argIDs(args)
		list := []map[string]any{}
		var notFound []string
		for _, mb := range f.mailboxes {
			if all || containsStr(ids, mb["id"].(string)) {
				list = append(list, mb)
			}
		}
		if !all {
			for _, id := range ids {
				if !f.hasMailbox(id) {
					notFound = append(notFound, id)
				}
			}
		}
		return name, map[string]any{
			"accountId": testAccount, "state": f.mailboxState,
			"list": list, "notFound": orEmpty(notFound),
		}

	case "Email/get":
		ids, all := argIDs(args)
		list := []map[string]any{}
		var notFound []string
		if all {
			for _, id := range f.order {
				list = append(list, f.emails[id].json())
			}
		} else {
			for _, id := range ids {
				if e, ok := f.emails[id]; ok {
					list = append(list, e.json())
				} else {
					notFound = append(notFound, id)
				}
			}
		}
		return name, map[string]any{
			"accountId": testAccount, "state": f.emailState,
			"list": list, "notFound": orEmpty(notFound),
		}

	case "Email/query":
		position := argInt(args, "position", 0)
		limit := argInt(args, "limit", 50)
		// f.order is receivedAt ascending; a query sorting the other way sees
		// the mirror image, positions and anchors included.
		order := f.order
		if !argAscending(args) {
			order = slices.Clone(f.order)
			slices.Reverse(order)
		}
		if anchor, ok := args["anchor"].(string); ok && anchor != "" {
			// RFC 8620 §5.5: the anchor's index plus anchorOffset replaces
			// position, and a missing anchor is an "anchorNotFound" error.
			idx := slices.Index(order, anchor)
			if idx < 0 {
				return "error", methodErrorArgs("anchorNotFound", anchor)
			}
			position = idx + argInt(args, "anchorOffset", 0)
			if position < 0 {
				position = 0
			}
		}
		ids := []string{}
		for i := position; i < len(order) && len(ids) < limit; i++ {
			ids = append(ids, order[i])
		}
		out := map[string]any{
			"accountId": testAccount, "queryState": f.emailState,
			"position": position, "ids": ids, "limit": limit,
			"canCalculateChanges": false,
		}
		if b, _ := args["calculateTotal"].(bool); b {
			out["total"] = len(f.order)
		}
		return name, out

	case "Email/changes":
		return f.changes(name, args, f.emailChanges, f.emailState)

	case "Mailbox/changes":
		return f.changes(name, args, f.mailboxChanges, f.mailboxState)

	case "CalendarEvent/changes":
		return f.changes(name, args, f.eventChanges, f.eventState)

	case "Email/set":
		return f.emailSet(name, args)

	case "Email/import":
		return f.emailImport(name, args)

	case "EmailSubmission/set":
		return f.submissionSet(name, args)

	case "Identity/get":
		return name, map[string]any{
			"accountId": testAccount, "state": "id-0",
			"list": []map[string]any{
				{"id": "identity-other", "name": "Alias", "email": "alias@example.com"},
				{"id": "identity-1", "name": "Me", "email": testEmail},
			},
			"notFound": []string{},
		}

	case "Calendar/get":
		ids, all := argIDs(args)
		list := []map[string]any{}
		for _, c := range f.calendars {
			if all || containsStr(ids, c["id"].(string)) {
				list = append(list, c)
			}
		}
		return name, map[string]any{
			"accountId": testAccount, "state": "calendars-0",
			"list": list, "notFound": []string{},
		}

	case "CalendarEvent/query":
		filter, _ := args["filter"].(map[string]any)
		var want string
		if v, ok := filter["inCalendar"].(string); ok {
			if f.calendarFilterPlural {
				return "error", methodErrorArgs("unsupportedFilter", "use inCalendars")
			}
			want = v
		} else if vs, ok := filter["inCalendars"].([]any); ok && len(vs) > 0 {
			want, _ = vs[0].(string)
		}
		position := argInt(args, "position", 0)
		limit := argInt(args, "limit", 50)
		var all []string
		for _, id := range sortedMapKeys(f.events) {
			cals, _ := f.events[id]["calendarIds"].(map[string]any)
			if want == "" || cals[want] == true {
				all = append(all, id)
			}
		}
		ids := []string{}
		for i := position; i < len(all) && len(ids) < limit; i++ {
			ids = append(ids, all[i])
		}
		return name, map[string]any{
			"accountId": testAccount, "queryState": f.eventState,
			"position": position, "ids": ids, "limit": limit,
			"canCalculateChanges": false,
		}

	case "CalendarEvent/get":
		ids, all := argIDs(args)
		list := []map[string]any{}
		var notFound []string
		if all {
			for _, id := range sortedMapKeys(f.events) {
				list = append(list, f.events[id])
			}
		} else {
			for _, id := range ids {
				if e, ok := f.events[id]; ok {
					list = append(list, e)
				} else {
					notFound = append(notFound, id)
				}
			}
		}
		return name, map[string]any{
			"accountId": testAccount, "state": f.eventState,
			"list": list, "notFound": orEmpty(notFound),
		}

	case "CalendarEvent/set":
		return f.calendarEventSet(name, args)
	}
	return "error", methodErrorArgs("unknownMethod", name)
}

func (f *fakeServer) changes(name string, args map[string]any, scripts map[string]changeScript, current string) (string, map[string]any) {
	since, _ := args["sinceState"].(string)
	sc, ok := scripts[since]
	if !ok && strings.HasPrefix(since, "spin-") {
		// A server that always has more changes and always advances its state:
		// the client must give up rather than loop forever.
		n, _ := strconv.Atoi(strings.TrimPrefix(since, "spin-"))
		sc, ok = changeScript{NewState: fmt.Sprintf("spin-%d", n+1), HasMore: true}, true
	}
	if !ok {
		return name, map[string]any{
			"accountId": testAccount, "oldState": since, "newState": current,
			"hasMoreChanges": false,
			"created":        []string{}, "updated": []string{}, "destroyed": []string{},
		}
	}
	if sc.ErrorType != "" {
		return "error", methodErrorArgs(sc.ErrorType, "cannot compute delta")
	}
	return name, map[string]any{
		"accountId": testAccount, "oldState": since, "newState": sc.NewState,
		"hasMoreChanges": sc.HasMore,
		"created":        orEmpty(sc.Created),
		"updated":        orEmpty(sc.Updated),
		"destroyed":      orEmpty(sc.Destroyed),
	}
}

// applyEmailPatch applies one JMAP PatchObject to a stored email.
func (f *fakeServer) applyEmailPatch(e *fakeEmail, patch map[string]any) {
	for k, v := range patch {
		switch {
		case k == "keywords":
			e.Keywords = toBoolMap(v)
		case k == "mailboxIds":
			e.MailboxIDs = toBoolMap(v)
		case strings.HasPrefix(k, "keywords/"):
			kw := strings.TrimPrefix(k, "keywords/")
			if v == nil {
				delete(e.Keywords, kw)
			} else {
				if e.Keywords == nil {
					e.Keywords = map[string]bool{}
				}
				e.Keywords[kw] = true
			}
		case strings.HasPrefix(k, "mailboxIds/"):
			mb := strings.TrimPrefix(k, "mailboxIds/")
			if v == nil {
				delete(e.MailboxIDs, mb)
			} else {
				if e.MailboxIDs == nil {
					e.MailboxIDs = map[string]bool{}
				}
				e.MailboxIDs[mb] = true
			}
		}
	}
}

func (f *fakeServer) emailSet(name string, args map[string]any) (string, map[string]any) {
	update, _ := args["update"].(map[string]any)
	updated := map[string]any{}
	notUpdated := map[string]any{}
	for id, raw := range update {
		patch, _ := raw.(map[string]any)
		e, ok := f.emails[id]
		if !ok {
			notUpdated[id] = map[string]any{"type": "notFound"}
			continue
		}
		f.applyEmailPatch(e, patch)
		updated[id] = nil
	}
	f.emailState = f.emailState + "+set"
	return name, map[string]any{
		"accountId": testAccount, "oldState": nil, "newState": f.emailState,
		"updated": updated, "notUpdated": notUpdated,
	}
}

func (f *fakeServer) emailImport(name string, args map[string]any) (string, map[string]any) {
	emails, _ := args["emails"].(map[string]any)
	created := map[string]any{}
	notCreated := map[string]any{}
	for cid, raw := range emails {
		spec, _ := raw.(map[string]any)
		blobID, _ := spec["blobId"].(string)
		data, ok := f.blobs[blobID]
		if !ok {
			notCreated[cid] = map[string]any{"type": "blobNotFound"}
			continue
		}
		id := fmt.Sprintf("email-imported-%d", len(f.emails)+1)
		e := &fakeEmail{
			ID:         id,
			BlobID:     blobID,
			ThreadID:   "thread-" + id,
			MailboxIDs: toBoolMap(spec["mailboxIds"]),
			Keywords:   toBoolMap(spec["keywords"]),
			ReceivedAt: time.Now().UTC().Truncate(time.Second),
			Size:       int64(len(data)),
		}
		f.emails[id] = e
		f.order = append(f.order, id)
		created[cid] = map[string]any{
			"id": id, "blobId": blobID, "threadId": e.ThreadID, "size": e.Size,
		}
	}
	f.emailState = f.emailState + "+import"
	return name, map[string]any{
		"accountId": testAccount, "oldState": nil, "newState": f.emailState,
		"created": created, "notCreated": notCreated,
	}
}

func (f *fakeServer) submissionSet(name string, args map[string]any) (string, map[string]any) {
	create, _ := args["create"].(map[string]any)
	onSuccess, _ := args["onSuccessUpdateEmail"].(map[string]any)
	created := map[string]any{}
	notCreated := map[string]any{}
	for cid, raw := range create {
		spec, _ := raw.(map[string]any)
		emailID, _ := spec["emailId"].(string)
		identity, _ := spec["identityId"].(string)
		if identity == "" {
			notCreated[cid] = map[string]any{"type": "invalidProperties", "properties": []string{"identityId"}}
			continue
		}
		if _, ok := f.emails[emailID]; !ok {
			notCreated[cid] = map[string]any{"type": "invalidProperties", "properties": []string{"emailId"}}
			continue
		}
		created[cid] = map[string]any{
			"id": "submission-" + cid, "emailId": emailID, "undoStatus": "final",
		}
		if patch, ok := onSuccess["#"+cid].(map[string]any); ok {
			f.applyEmailPatch(f.emails[emailID], patch)
		}
	}
	return name, map[string]any{
		"accountId": testAccount, "oldState": nil, "newState": "sub-1",
		"created": created, "notCreated": notCreated,
	}
}

func (f *fakeServer) calendarEventSet(name string, args map[string]any) (string, map[string]any) {
	if _, ok := args["sendSchedulingMessages"]; ok && f.rejectScheduling {
		return "error", methodErrorArgs("invalidArguments", "sendSchedulingMessages")
	}
	created := map[string]any{}
	notCreated := map[string]any{}
	updated := map[string]any{}
	notUpdated := map[string]any{}
	var destroyed []string
	notDestroyed := map[string]any{}

	if m, ok := args["create"].(map[string]any); ok {
		for cid, raw := range m {
			obj, _ := raw.(map[string]any)
			id := fmt.Sprintf("event-new-%d", len(f.events)+1)
			obj["id"] = id
			if obj["uid"] == nil || obj["uid"] == "" {
				obj["uid"] = "uid-" + id
			}
			obj["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			f.events[id] = obj
			created[cid] = map[string]any{"id": id, "uid": obj["uid"], "updated": obj["updated"]}
		}
	}
	if m, ok := args["update"].(map[string]any); ok {
		for id, raw := range m {
			patch, _ := raw.(map[string]any)
			obj, ok := f.events[id]
			if !ok {
				notUpdated[id] = map[string]any{"type": "notFound"}
				continue
			}
			for k, v := range patch {
				applyJSONPointerPatch(obj, k, v)
			}
			obj["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			updated[id] = nil
		}
	}
	if raw, ok := args["destroy"].([]any); ok {
		for _, v := range raw {
			id, _ := v.(string)
			if _, ok := f.events[id]; !ok {
				notDestroyed[id] = map[string]any{"type": "notFound"}
				continue
			}
			delete(f.events, id)
			destroyed = append(destroyed, id)
		}
	}
	f.eventState = f.eventState + "+set"
	return name, map[string]any{
		"accountId": testAccount, "oldState": nil, "newState": f.eventState,
		"created": created, "notCreated": notCreated,
		"updated": updated, "notUpdated": notUpdated,
		"destroyed": orEmpty(destroyed), "notDestroyed": notDestroyed,
	}
}

// applyJSONPointerPatch applies one "a/b/c" patch entry to a nested map.
func applyJSONPointerPatch(obj map[string]any, path string, val any) {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = strings.ReplaceAll(strings.ReplaceAll(segs[i], "~1", "/"), "~0", "~")
	}
	cur := obj
	for _, s := range segs[:len(segs)-1] {
		next, ok := cur[s].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[s] = next
		}
		cur = next
	}
	last := segs[len(segs)-1]
	if val == nil {
		delete(cur, last)
		return
	}
	cur[last] = val
}

// ---------------------------------------------------------------------------
// Small arg helpers

// argIDs returns the requested ids and whether "ids" was null (meaning "all").
func argIDs(args map[string]any) (ids []string, all bool) {
	v, present := args["ids"]
	if !present || v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, true
	}
	for _, e := range list {
		if s, ok := e.(string); ok {
			ids = append(ids, s)
		}
	}
	return ids, false
}

func argInt(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

// argAscending reads the direction of the first "sort" comparator. RFC 8620
// §5.5 makes isAscending default to true, and so does a query with no sort.
func argAscending(args map[string]any) bool {
	sorts, ok := args["sort"].([]any)
	if !ok || len(sorts) == 0 {
		return true
	}
	first, ok := sorts[0].(map[string]any)
	if !ok {
		return true
	}
	asc, ok := first["isAscending"].(bool)
	return !ok || asc
}

func (f *fakeServer) hasMailbox(id string) bool {
	for _, mb := range f.mailboxes {
		if mb["id"] == id {
			return true
		}
	}
	return false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toBoolMap(v any) map[string]bool {
	out := map[string]bool{}
	m, ok := v.(map[string]any)
	if !ok {
		return out
	}
	for k, val := range m {
		if b, ok := val.(bool); ok && b {
			out[k] = true
		}
	}
	return out
}

func sortedMapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
