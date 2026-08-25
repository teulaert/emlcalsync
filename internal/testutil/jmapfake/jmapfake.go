// Package jmapfake is a standalone, in-process fake Fastmail JMAP server.
//
// It is good enough to drive the real internal/provider/jmap client end to
// end: session discovery, Mailbox/Email/CalendarEvent get+query+changes+set,
// Email/import, EmailSubmission/set, blob upload/download and the EventSource
// push stream.
//
// It is deliberately exported (not an in-package _test.go fake) so the e2e
// suite can spawn the real emlcal binary against it via
// EMLCAL_JMAP_SESSION_URL.
//
// Anything the client asks for that the fake does not implement comes back as
// a JMAP "unknownMethod" method error, so a client mistake is loud rather than
// silently absorbed. Set EMLCAL_E2E_VERBOSE=1 to log every method call.
package jmapfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Capability URNs, duplicated here so the fake does not import the client it
// is meant to test.
const (
	CapCore       = "urn:ietf:params:jmap:core"
	CapMail       = "urn:ietf:params:jmap:mail"
	CapSubmission = "urn:ietf:params:jmap:submission"
	CapCalendars  = "urn:ietf:params:jmap:calendars"
)

// DefaultToken is the bearer token a fresh Server accepts.
const DefaultToken = "fake-fastmail-token"

// AccountID is the single account this fake serves.
const AccountID = "acct-1"

// Well-known mailbox ids created by New.
const (
	MailboxInbox   = "mb-inbox"
	MailboxArchive = "mb-archive"
	MailboxSent    = "mb-sent"
	MailboxDrafts  = "mb-drafts"
	MailboxTrash   = "mb-trash"
	MailboxJunk    = "mb-junk"
)

// CalendarDefault is the calendar created by New.
const CalendarDefault = "cal-personal"

// baseTime anchors generated receivedAt stamps so message order is stable.
var baseTime = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

// Message is one email in the fake store.
type Message struct {
	ID         string
	BlobID     string
	ThreadID   string
	MailboxIDs map[string]bool
	Keywords   map[string]bool
	ReceivedAt time.Time
	Size       int64

	createdSeq int
	updatedSeq int
}

func (m *Message) obj() map[string]any {
	return map[string]any{
		"id":         m.ID,
		"blobId":     m.BlobID,
		"threadId":   m.ThreadID,
		"mailboxIds": boolMapAny(m.MailboxIDs),
		"keywords":   boolMapAny(m.Keywords),
		"receivedAt": m.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"size":       m.Size,
	}
}

// Submission is one accepted EmailSubmission/set create.
type Submission struct {
	ID      string
	EmailID string
	Raw     []byte
}

// Server is the fake JMAP server.
type Server struct {
	tb  testing.TB
	srv *httptest.Server

	// Token is the bearer token the server accepts. Safe to change before the
	// client under test makes its first request.
	Token string
	// Email is the account's own address (session username, Identity email).
	Email string

	mu sync.Mutex

	seq int // monotonic; every mutation bumps it

	messages map[string]*Message
	msgOrder []string // ids, receivedAt ascending
	msgGone  map[string]int
	nextMsg  int

	blobs   map[string][]byte
	nextBlb int

	mailboxes []*mailbox
	mbGone    map[string]int

	events    map[string]map[string]any
	evCreated map[string]int
	evUpdated map[string]int
	evGone    map[string]int
	nextEvent int

	calendars []map[string]any

	submissions []Submission

	// messageIDIndex maps an RFC 822 Message-ID to the thread it landed in, so
	// a reply that carries In-Reply-To is threaded with its parent.
	messageIDIndex map[string]string

	calls []Call

	sseSubs  []chan string
	sseConns int
}

// Call records one dispatched method invocation.
type Call struct {
	Name string
	Args map[string]any
}

type mailbox struct {
	ID         string
	Name       string
	ParentID   string
	Role       string
	SortOrder  int
	createdSeq int
	updatedSeq int
}

// New starts a fake server pre-populated with the standard Fastmail mailbox
// roles and one calendar. It is closed automatically when the test ends.
func New(tb testing.TB) *Server {
	tb.Helper()
	s := &Server{
		tb:             tb,
		Token:          DefaultToken,
		Email:          "me@example.com",
		messages:       map[string]*Message{},
		msgGone:        map[string]int{},
		blobs:          map[string][]byte{},
		mbGone:         map[string]int{},
		events:         map[string]map[string]any{},
		evCreated:      map[string]int{},
		evUpdated:      map[string]int{},
		evGone:         map[string]int{},
		messageIDIndex: map[string]string{},
	}
	for i, def := range []struct{ id, name, role string }{
		{MailboxInbox, "Inbox", "inbox"},
		{MailboxArchive, "Archive", "archive"},
		{MailboxSent, "Sent", "sent"},
		{MailboxDrafts, "Drafts", "drafts"},
		{MailboxTrash, "Trash", "trash"},
		{MailboxJunk, "Spam", "junk"},
	} {
		s.mailboxes = append(s.mailboxes, &mailbox{
			ID: def.id, Name: def.name, Role: def.role, SortOrder: i + 1,
		})
	}
	s.calendars = []map[string]any{{
		"id":        CalendarDefault,
		"name":      "Personal",
		"color":     "#3a87ad",
		"sortOrder": 1,
		"isDefault": true,
		"isVisible": true,
		"timeZone":  "Europe/Amsterdam",
		"myRights": map[string]any{
			"mayReadFreeBusy": true, "mayReadItems": true, "mayWriteAll": true,
			"mayWriteOwn": true, "mayUpdatePrivate": true, "mayRSVP": true,
			"mayShare": true, "mayDelete": true,
		},
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("/jmap/session", s.handleSession)
	mux.HandleFunc("/jmap/api", s.handleAPI)
	mux.HandleFunc("/jmap/download/", s.handleDownload)
	mux.HandleFunc("/jmap/upload/", s.handleUpload)
	mux.HandleFunc("/jmap/event", s.handleEvent)
	s.srv = httptest.NewServer(s.auth(mux))
	tb.Cleanup(s.Close)
	return s
}

// URL is the session resource URL, for EMLCAL_JMAP_SESSION_URL.
func (s *Server) URL() string { return s.srv.URL + "/jmap/session" }

// BaseURL is the server root.
func (s *Server) BaseURL() string { return s.srv.URL }

// Close shuts the server down. It is safe to call more than once, and the
// tests use it to simulate going offline.
func (s *Server) Close() {
	s.mu.Lock()
	subs := s.sseSubs
	s.sseSubs = nil
	s.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
	s.srv.Close()
}

// ---------------------------------------------------------------------------
// Test-facing store API

func (s *Server) bump() int {
	s.seq++
	return s.seq
}

// AddMailbox creates a mailbox and returns its id. role may be "" for an
// ordinary user folder.
func (s *Server) AddMailbox(name, role string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := "mb-" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	s.mailboxes = append(s.mailboxes, &mailbox{
		ID: id, Name: name, Role: role, SortOrder: len(s.mailboxes) + 1,
		createdSeq: s.bump(),
	})
	return id
}

// AddMessage stores a raw RFC 822 message and returns its Email id. Passing a
// nil keyword map means an unread message with no keywords.
func (s *Server) AddMessage(raw []byte, mailboxIDs []string, keywords map[string]bool) string {
	return s.AddMessageAt(raw, mailboxIDs, keywords, time.Time{})
}

// AddMessageAt is AddMessage with an explicit receivedAt. A zero time gets a
// generated stamp one minute later than the previous message.
func (s *Server) AddMessageAt(raw []byte, mailboxIDs []string, keywords map[string]bool, at time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextMsg++
	id := fmt.Sprintf("msg-%03d", s.nextMsg)
	blobID := "blob-" + id
	s.blobs[blobID] = append([]byte(nil), raw...)
	if at.IsZero() {
		at = baseTime.Add(time.Duration(s.nextMsg) * time.Minute)
	}
	if len(mailboxIDs) == 0 {
		mailboxIDs = []string{MailboxInbox}
	}
	m := &Message{
		ID:         id,
		BlobID:     blobID,
		ThreadID:   s.threadFor(raw, id),
		MailboxIDs: sliceToBoolMap(mailboxIDs),
		Keywords:   copyBoolMap(keywords),
		ReceivedAt: at.UTC().Truncate(time.Second),
		Size:       int64(len(raw)),
		createdSeq: s.bump(),
	}
	m.updatedSeq = m.createdSeq
	s.messages[id] = m
	s.msgOrder = append(s.msgOrder, id)
	s.sortLocked()
	return id
}

// threadFor picks a thread id for a raw message, honouring In-Reply-To so a
// reply lands in its parent's thread. Caller holds the lock.
func (s *Server) threadFor(raw []byte, id string) string {
	msgID, inReplyTo := scanThreadHeaders(raw)
	thread := "thread-" + id
	if inReplyTo != "" {
		if t, ok := s.messageIDIndex[inReplyTo]; ok {
			thread = t
		}
	}
	if msgID != "" {
		s.messageIDIndex[msgID] = thread
	}
	return thread
}

// SetFlags replaces a message's keywords.
func (s *Server) SetFlags(id string, keywords map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.messages[id]; ok {
		m.Keywords = copyBoolMap(keywords)
		m.updatedSeq = s.bump()
	}
}

// Move replaces a message's mailbox memberships.
func (s *Server) Move(id string, mailboxIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.messages[id]; ok {
		m.MailboxIDs = sliceToBoolMap(mailboxIDs)
		m.updatedSeq = s.bump()
	}
}

// Delete destroys a message.
func (s *Server) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.destroyLocked(id)
}

func (s *Server) destroyLocked(id string) {
	if _, ok := s.messages[id]; !ok {
		return
	}
	delete(s.messages, id)
	for i, v := range s.msgOrder {
		if v == id {
			s.msgOrder = append(s.msgOrder[:i], s.msgOrder[i+1:]...)
			break
		}
	}
	s.msgGone[id] = s.bump()
}

// Message returns a copy of one stored message.
func (s *Server) Message(id string) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return Message{}, false
	}
	out := *m
	out.Keywords = copyBoolMap(m.Keywords)
	out.MailboxIDs = copyBoolMap(m.MailboxIDs)
	return out, true
}

// MessageIDs returns every live message id, receivedAt ascending.
func (s *Server) MessageIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.msgOrder...)
}

// Raw returns the stored bytes of a message.
func (s *Server) Raw(id string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.messages[id]
	if !ok {
		return nil
	}
	return append([]byte(nil), s.blobs[m.BlobID]...)
}

// Submissions returns the raw bytes of every submitted message, in order.
func (s *Server) Submissions() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, 0, len(s.submissions))
	for _, sub := range s.submissions {
		out = append(out, append([]byte(nil), sub.Raw...))
	}
	return out
}

// AddCalendar creates an extra calendar and returns its id.
func (s *Server) AddCalendar(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("cal-%d", len(s.calendars)+1)
	s.calendars = append(s.calendars, map[string]any{
		"id": id, "name": name, "sortOrder": len(s.calendars) + 1,
		"isDefault": false, "isVisible": true,
	})
	return id
}

// AddEvent stores a JSCalendar object in a calendar and returns its id. The
// map is taken as-is apart from id/calendarIds/uid, which are filled in.
func (s *Server) AddEvent(calID string, jscal map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextEvent++
	id := fmt.Sprintf("event-%03d", s.nextEvent)
	obj := map[string]any{}
	for k, v := range jscal {
		obj[k] = v
	}
	obj["@type"] = "Event"
	obj["id"] = id
	obj["calendarIds"] = map[string]any{calID: true}
	if obj["uid"] == nil || obj["uid"] == "" {
		obj["uid"] = "uid-" + id
	}
	if obj["updated"] == nil {
		obj["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}
	s.events[id] = obj
	s.evCreated[id] = s.bump()
	s.evUpdated[id] = s.evCreated[id]
	return id
}

// Events returns a snapshot of every stored event, keyed by id.
func (s *Server) Events() map[string]map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]any, len(s.events))
	for id, ev := range s.events {
		cp := map[string]any{}
		for k, v := range ev {
			cp[k] = v
		}
		out[id] = cp
	}
	return out
}

// Bump notifies every connected EventSource client that mail and calendar
// state moved, which is what makes `sync --watch` do a pass.
func (s *Server) Bump() {
	s.mu.Lock()
	s.seq++
	payload, _ := json.Marshal(map[string]any{
		"@type": "StateChange",
		"changed": map[string]any{AccountID: map[string]string{
			"Email":         s.stateLocked("email"),
			"Mailbox":       s.stateLocked("mailbox"),
			"CalendarEvent": s.stateLocked("cal"),
		}},
	})
	subs := append([]chan string(nil), s.sseSubs...)
	s.mu.Unlock()
	frame := "event: state\ndata: " + string(payload) + "\n\n"
	for _, ch := range subs {
		select {
		case ch <- frame:
		case <-time.After(2 * time.Second):
		}
	}
}

// SSEConnections reports how many EventSource clients have connected.
func (s *Server) SSEConnections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sseConns
}

// Calls returns every method invocation the server has dispatched.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// CallsFor returns the arguments of every invocation of one method.
func (s *Server) CallsFor(name string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, c := range s.calls {
		if c.Name == name {
			out = append(out, c.Args)
		}
	}
	return out
}

// ResetCalls clears the recorded invocations.
func (s *Server) ResetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

func (s *Server) stateLocked(kind string) string {
	return kind + "-" + strconv.Itoa(s.seq)
}

func (s *Server) sortLocked() {
	sort.SliceStable(s.msgOrder, func(i, j int) bool {
		a, b := s.messages[s.msgOrder[i]], s.messages[s.msgOrder[j]]
		if a.ReceivedAt.Equal(b.ReceivedAt) {
			return a.ID < b.ID
		}
		return a.ReceivedAt.Before(b.ReceivedAt)
	})
}

// ---------------------------------------------------------------------------
// HTTP handlers

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		want := "Bearer " + s.Token
		s.mu.Unlock()
		if r.Header.Get("Authorization") != want {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"type":"about:blank","detail":"bad token"}`)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	state := "session-0"
	email := s.Email
	s.mu.Unlock()
	base := s.srv.URL
	writeJSON(w, map[string]any{
		"capabilities": map[string]any{
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
			CapMail:       map[string]any{"maxMailboxesPerEmail": nil, "maxSizeAttachmentsPerEmail": 50 << 20},
			CapSubmission: map[string]any{},
			CapCalendars:  map[string]any{},
		},
		"accounts": map[string]any{
			AccountID: map[string]any{
				"name": email, "isPersonal": true, "isReadOnly": false,
				"accountCapabilities": map[string]any{
					CapMail: map[string]any{}, CapSubmission: map[string]any{},
					CapCalendars: map[string]any{},
				},
			},
		},
		"primaryAccounts": map[string]any{
			CapMail:       AccountID,
			CapSubmission: AccountID,
			CapCalendars:  AccountID,
		},
		"username":       email,
		"apiUrl":         base + "/jmap/api",
		"downloadUrl":    base + "/jmap/download/{accountId}/{blobId}/{name}?type={type}",
		"uploadUrl":      base + "/jmap/upload/{accountId}",
		"eventSourceUrl": base + "/jmap/event?types={types}&closeafter={closeafter}&ping={ping}",
		"state":          state,
	})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jmap/download/"), "/")
	if len(parts) < 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	blobID := parts[1]
	s.mu.Lock()
	data, ok := s.blobs[blobID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "no such blob", http.StatusNotFound)
		return
	}
	s.logf("download %s (%d bytes)", blobID, len(data))
	w.Header().Set("Content-Type", "message/rfc822")
	w.Write(data)
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.nextBlb++
	id := fmt.Sprintf("blob-up-%d", s.nextBlb)
	s.blobs[id] = data
	s.mu.Unlock()
	s.logf("upload -> %s (%d bytes)", id, len(data))
	writeJSON(w, map[string]any{
		"accountId": AccountID,
		"blobId":    id,
		"type":      r.Header.Get("Content-Type"),
		"size":      len(data),
	})
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 16)
	s.mu.Lock()
	s.sseSubs = append(s.sseSubs, ch)
	s.sseConns++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, c := range s.sseSubs {
			if c == ch {
				s.sseSubs = append(s.sseSubs[:i], s.sseSubs[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()

	s.logf("eventsource connected: %s", r.URL.RawQuery)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
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

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
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

	var results []result
	for _, mc := range req.MethodCalls {
		var trip []json.RawMessage
		if err := json.Unmarshal(mc, &trip); err != nil || len(trip) != 3 {
			http.Error(w, `{"type":"urn:ietf:params:jmap:error:notRequest"}`, http.StatusBadRequest)
			return
		}
		var name, callID string
		json.Unmarshal(trip[0], &name)
		json.Unmarshal(trip[2], &callID)
		args := map[string]any{}
		json.Unmarshal(trip[1], &args)

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
			results = append(results, result{"error", methodError("invalidResultReference", refErr), callID})
			continue
		}

		s.mu.Lock()
		s.calls = append(s.calls, Call{Name: name, Args: deepCopy(args)})
		s.mu.Unlock()
		s.logf("%s %s", name, compactJSON(args))

		rname, rargs := s.dispatch(name, args, req.Using)
		results = append(results, result{rname, rargs, callID})
	}

	out := make([]any, 0, len(results))
	for _, r := range results {
		out = append(out, []any{r.name, r.args, r.id})
	}
	s.mu.Lock()
	sessionState := "session-0"
	s.mu.Unlock()
	writeJSON(w, map[string]any{"methodResponses": out, "sessionState": sessionState})
}

type result struct {
	name string
	args map[string]any
	id   string
}

func resolveResultRef(results []result, ref map[string]any) (any, error) {
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

// ---------------------------------------------------------------------------
// Method dispatch

func (s *Server) dispatch(name string, args map[string]any, using []string) (string, map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.HasPrefix(name, "Calendar") && !contains(using, CapCalendars) {
		return "error", methodError("unknownMethod", name+" needs "+CapCalendars+" in using")
	}

	switch name {
	case "Mailbox/get":
		return s.mailboxGet(name, args)
	case "Mailbox/changes":
		return s.changes(name, args, "mailbox", s.mailboxChangeSets())
	case "Email/get":
		return s.emailGet(name, args)
	case "Email/query":
		return s.emailQuery(name, args)
	case "Email/changes":
		return s.changes(name, args, "email", s.emailChangeSets())
	case "Email/set":
		return s.emailSet(name, args)
	case "Email/import":
		return s.emailImport(name, args)
	case "EmailSubmission/set":
		return s.submissionSet(name, args)
	case "Identity/get":
		return name, map[string]any{
			"accountId": AccountID, "state": "identity-0",
			"list": []map[string]any{
				{"id": "identity-1", "name": "Me", "email": s.Email},
				{"id": "identity-alias", "name": "Alias", "email": "alias@example.com"},
			},
			"notFound": []string{},
		}
	case "Calendar/get":
		return s.calendarGet(name, args)
	case "CalendarEvent/get":
		return s.eventGet(name, args)
	case "CalendarEvent/query":
		return s.eventQuery(name, args)
	case "CalendarEvent/changes":
		return s.changes(name, args, "cal", s.eventChangeSets())
	case "CalendarEvent/set":
		return s.eventSet(name, args)
	}
	return "error", methodError("unknownMethod", name)
}

func (s *Server) mailboxGet(name string, args map[string]any) (string, map[string]any) {
	ids, all := argIDs(args)
	list := []map[string]any{}
	var notFound []string
	for _, mb := range s.mailboxes {
		if all || contains(ids, mb.ID) {
			list = append(list, s.mailboxObj(mb))
		}
	}
	if !all {
		for _, id := range ids {
			if s.mailbox(id) == nil {
				notFound = append(notFound, id)
			}
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "state": s.stateLocked("mailbox"),
		"list": list, "notFound": orEmpty(notFound),
	}
}

func (s *Server) mailboxObj(mb *mailbox) map[string]any {
	total, unread := 0, 0
	for _, m := range s.messages {
		if m.MailboxIDs[mb.ID] {
			total++
			if !m.Keywords["$seen"] {
				unread++
			}
		}
	}
	var parent, role any
	if mb.ParentID != "" {
		parent = mb.ParentID
	}
	if mb.Role != "" {
		role = mb.Role
	}
	return map[string]any{
		"id": mb.ID, "name": mb.Name, "parentId": parent, "role": role,
		"sortOrder": mb.SortOrder, "totalEmails": total, "unreadEmails": unread,
	}
}

func (s *Server) mailbox(id string) *mailbox {
	for _, mb := range s.mailboxes {
		if mb.ID == id {
			return mb
		}
	}
	return nil
}

func (s *Server) emailGet(name string, args map[string]any) (string, map[string]any) {
	ids, all := argIDs(args)
	list := []map[string]any{}
	var notFound []string
	if all {
		for _, id := range s.msgOrder {
			list = append(list, s.messages[id].obj())
		}
	} else {
		for _, id := range ids {
			if m, ok := s.messages[id]; ok {
				list = append(list, m.obj())
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "state": s.stateLocked("email"),
		"list": list, "notFound": orEmpty(notFound),
	}
}

func (s *Server) emailQuery(name string, args map[string]any) (string, map[string]any) {
	order := append([]string(nil), s.msgOrder...)
	// The client sorts by receivedAt; honour a descending request too.
	if sorts, ok := args["sort"].([]any); ok {
		for _, raw := range sorts {
			sc, _ := raw.(map[string]any)
			if prop, _ := sc["property"].(string); prop != "receivedAt" && prop != "" {
				return "error", methodError("unsupportedSort", prop)
			}
			if asc, ok := sc["isAscending"].(bool); ok && !asc {
				reverse(order)
			}
		}
	}

	position := argInt(args, "position", 0)
	limit := argInt(args, "limit", 50)
	if anchor, ok := args["anchor"].(string); ok && anchor != "" {
		idx := -1
		for i, id := range order {
			if id == anchor {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "error", methodError("anchorNotFound", anchor)
		}
		position = idx + argInt(args, "anchorOffset", 0)
		if position < 0 {
			position = 0
		}
	}
	ids := []string{}
	for i := position; i >= 0 && i < len(order) && len(ids) < limit; i++ {
		ids = append(ids, order[i])
	}
	out := map[string]any{
		"accountId": AccountID, "queryState": s.stateLocked("email"),
		"position": position, "ids": ids, "limit": limit,
		"canCalculateChanges": false,
	}
	if b, _ := args["calculateTotal"].(bool); b {
		out["total"] = len(order)
	}
	return name, out
}

// changeSets is the created/updated/destroyed view of one data type, expressed
// as the sequence number at which each id last changed.
type changeSets struct {
	created   map[string]int
	updated   map[string]int
	destroyed map[string]int
}

func (s *Server) emailChangeSets() changeSets {
	cs := changeSets{map[string]int{}, map[string]int{}, s.msgGone}
	for id, m := range s.messages {
		cs.created[id] = m.createdSeq
		cs.updated[id] = m.updatedSeq
	}
	return cs
}

func (s *Server) mailboxChangeSets() changeSets {
	cs := changeSets{map[string]int{}, map[string]int{}, s.mbGone}
	for _, mb := range s.mailboxes {
		cs.created[mb.ID] = mb.createdSeq
		cs.updated[mb.ID] = mb.updatedSeq
	}
	return cs
}

func (s *Server) eventChangeSets() changeSets {
	return changeSets{s.evCreated, s.evUpdated, s.evGone}
}

// changes answers a /changes call from real bookkeeping: everything whose
// sequence number is newer than the one encoded in sinceState.
func (s *Server) changes(name string, args map[string]any, kind string, cs changeSets) (string, map[string]any) {
	since, _ := args["sinceState"].(string)
	n, ok := parseState(since, kind)
	if !ok {
		return "error", methodError("cannotCalculateChanges", "unknown state "+since)
	}
	var created, updated, destroyed []string
	for id, seq := range cs.created {
		if seq > n {
			created = append(created, id)
		} else if cs.updated[id] > n {
			updated = append(updated, id)
		}
	}
	for id, seq := range cs.destroyed {
		if seq > n {
			destroyed = append(destroyed, id)
		}
	}
	sort.Strings(created)
	sort.Strings(updated)
	sort.Strings(destroyed)
	return name, map[string]any{
		"accountId": AccountID, "oldState": since, "newState": s.stateLocked(kind),
		"hasMoreChanges": false,
		"created":        orEmpty(created),
		"updated":        orEmpty(updated),
		"destroyed":      orEmpty(destroyed),
	}
}

func (s *Server) applyEmailPatch(m *Message, patch map[string]any) {
	for k, v := range patch {
		switch {
		case k == "keywords":
			m.Keywords = toBoolMap(v)
		case k == "mailboxIds":
			m.MailboxIDs = toBoolMap(v)
		case strings.HasPrefix(k, "keywords/"):
			kw := strings.TrimPrefix(k, "keywords/")
			if v == nil {
				delete(m.Keywords, kw)
			} else {
				if m.Keywords == nil {
					m.Keywords = map[string]bool{}
				}
				m.Keywords[kw] = true
			}
		case strings.HasPrefix(k, "mailboxIds/"):
			mb := strings.TrimPrefix(k, "mailboxIds/")
			if v == nil {
				delete(m.MailboxIDs, mb)
			} else {
				if m.MailboxIDs == nil {
					m.MailboxIDs = map[string]bool{}
				}
				m.MailboxIDs[mb] = true
			}
		}
	}
	m.updatedSeq = s.bump()
}

func (s *Server) emailSet(name string, args map[string]any) (string, map[string]any) {
	updated := map[string]any{}
	notUpdated := map[string]any{}
	if update, ok := args["update"].(map[string]any); ok {
		for id, raw := range update {
			patch, _ := raw.(map[string]any)
			m, ok := s.messages[id]
			if !ok {
				notUpdated[id] = map[string]any{"type": "notFound"}
				continue
			}
			s.applyEmailPatch(m, patch)
			updated[id] = nil
		}
	}
	var destroyed []string
	notDestroyed := map[string]any{}
	if raw, ok := args["destroy"].([]any); ok {
		for _, v := range raw {
			id, _ := v.(string)
			if _, ok := s.messages[id]; !ok {
				notDestroyed[id] = map[string]any{"type": "notFound"}
				continue
			}
			s.destroyLocked(id)
			destroyed = append(destroyed, id)
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "oldState": nil, "newState": s.stateLocked("email"),
		"updated": updated, "notUpdated": notUpdated,
		"destroyed": orEmpty(destroyed), "notDestroyed": notDestroyed,
		"created": map[string]any{}, "notCreated": map[string]any{},
	}
}

func (s *Server) emailImport(name string, args map[string]any) (string, map[string]any) {
	emails, _ := args["emails"].(map[string]any)
	created := map[string]any{}
	notCreated := map[string]any{}
	for cid, raw := range emails {
		spec, _ := raw.(map[string]any)
		blobID, _ := spec["blobId"].(string)
		data, ok := s.blobs[blobID]
		if !ok {
			notCreated[cid] = map[string]any{"type": "blobNotFound"}
			continue
		}
		s.nextMsg++
		id := fmt.Sprintf("msg-%03d", s.nextMsg)
		at := time.Now().UTC().Truncate(time.Second)
		if v, ok := spec["receivedAt"].(string); ok {
			if t, err := time.Parse("2006-01-02T15:04:05Z", v); err == nil {
				at = t
			}
		}
		m := &Message{
			ID:         id,
			BlobID:     blobID,
			ThreadID:   s.threadFor(data, id),
			MailboxIDs: toBoolMap(spec["mailboxIds"]),
			Keywords:   toBoolMap(spec["keywords"]),
			ReceivedAt: at,
			Size:       int64(len(data)),
			createdSeq: s.bump(),
		}
		m.updatedSeq = m.createdSeq
		s.messages[id] = m
		s.msgOrder = append(s.msgOrder, id)
		s.sortLocked()
		created[cid] = map[string]any{
			"id": id, "blobId": blobID, "threadId": m.ThreadID, "size": m.Size,
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "oldState": nil, "newState": s.stateLocked("email"),
		"created": created, "notCreated": notCreated,
	}
}

func (s *Server) submissionSet(name string, args map[string]any) (string, map[string]any) {
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
		m, ok := s.messages[emailID]
		if !ok {
			notCreated[cid] = map[string]any{"type": "invalidProperties", "properties": []string{"emailId"}}
			continue
		}
		subID := fmt.Sprintf("submission-%d", len(s.submissions)+1)
		s.submissions = append(s.submissions, Submission{
			ID: subID, EmailID: emailID,
			Raw: append([]byte(nil), s.blobs[m.BlobID]...),
		})
		created[cid] = map[string]any{
			"id": subID, "emailId": emailID, "undoStatus": "final",
			"sendAt": time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		}
		// onSuccessUpdateEmail is what moves the message out of Drafts and
		// into Sent, exactly as Fastmail does.
		if patch, ok := onSuccess["#"+cid].(map[string]any); ok {
			s.applyEmailPatch(m, patch)
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "oldState": nil, "newState": s.stateLocked("submission"),
		"created": created, "notCreated": notCreated,
	}
}

func (s *Server) calendarGet(name string, args map[string]any) (string, map[string]any) {
	ids, all := argIDs(args)
	list := []map[string]any{}
	var notFound []string
	for _, c := range s.calendars {
		if all || contains(ids, c["id"].(string)) {
			list = append(list, c)
		}
	}
	if !all {
		for _, id := range ids {
			found := false
			for _, c := range s.calendars {
				if c["id"] == id {
					found = true
				}
			}
			if !found {
				notFound = append(notFound, id)
			}
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "state": s.stateLocked("calendars"),
		"list": list, "notFound": orEmpty(notFound),
	}
}

func (s *Server) eventGet(name string, args map[string]any) (string, map[string]any) {
	ids, all := argIDs(args)
	list := []map[string]any{}
	var notFound []string
	if all {
		for _, id := range sortedKeys(s.events) {
			list = append(list, s.events[id])
		}
	} else {
		for _, id := range ids {
			if ev, ok := s.events[id]; ok {
				list = append(list, ev)
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "state": s.stateLocked("cal"),
		"list": list, "notFound": orEmpty(notFound),
	}
}

func (s *Server) eventQuery(name string, args map[string]any) (string, map[string]any) {
	filter, _ := args["filter"].(map[string]any)
	want := ""
	if v, ok := filter["inCalendar"].(string); ok {
		want = v
	} else if vs, ok := filter["inCalendars"].([]any); ok && len(vs) > 0 {
		want, _ = vs[0].(string)
	}
	var all []string
	for _, id := range sortedKeys(s.events) {
		cals, _ := s.events[id]["calendarIds"].(map[string]any)
		if want == "" || cals[want] == true {
			all = append(all, id)
		}
	}
	position := argInt(args, "position", 0)
	limit := argInt(args, "limit", 50)
	ids := []string{}
	for i := position; i >= 0 && i < len(all) && len(ids) < limit; i++ {
		ids = append(ids, all[i])
	}
	return name, map[string]any{
		"accountId": AccountID, "queryState": s.stateLocked("cal"),
		"position": position, "ids": ids, "limit": limit,
		"canCalculateChanges": false,
	}
}

func (s *Server) eventSet(name string, args map[string]any) (string, map[string]any) {
	created := map[string]any{}
	notCreated := map[string]any{}
	updated := map[string]any{}
	notUpdated := map[string]any{}
	var destroyed []string
	notDestroyed := map[string]any{}

	if m, ok := args["create"].(map[string]any); ok {
		for cid, raw := range m {
			obj, _ := raw.(map[string]any)
			s.nextEvent++
			id := fmt.Sprintf("event-%03d", s.nextEvent)
			obj["id"] = id
			if obj["uid"] == nil || obj["uid"] == "" {
				obj["uid"] = "uid-" + id
			}
			obj["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			s.events[id] = obj
			s.evCreated[id] = s.bump()
			s.evUpdated[id] = s.evCreated[id]
			created[cid] = map[string]any{"id": id, "uid": obj["uid"], "updated": obj["updated"]}
		}
	}
	if m, ok := args["update"].(map[string]any); ok {
		for id, raw := range m {
			patch, _ := raw.(map[string]any)
			obj, ok := s.events[id]
			if !ok {
				notUpdated[id] = map[string]any{"type": "notFound"}
				continue
			}
			for k, v := range patch {
				applyPointerPatch(obj, k, v)
			}
			obj["updated"] = time.Now().UTC().Format("2006-01-02T15:04:05Z")
			s.evUpdated[id] = s.bump()
			updated[id] = nil
		}
	}
	if raw, ok := args["destroy"].([]any); ok {
		for _, v := range raw {
			id, _ := v.(string)
			if _, ok := s.events[id]; !ok {
				notDestroyed[id] = map[string]any{"type": "notFound"}
				continue
			}
			delete(s.events, id)
			delete(s.evCreated, id)
			delete(s.evUpdated, id)
			s.evGone[id] = s.bump()
			destroyed = append(destroyed, id)
		}
	}
	return name, map[string]any{
		"accountId": AccountID, "oldState": nil, "newState": s.stateLocked("cal"),
		"created": created, "notCreated": notCreated,
		"updated": updated, "notUpdated": notUpdated,
		"destroyed": orEmpty(destroyed), "notDestroyed": notDestroyed,
	}
}

// ---------------------------------------------------------------------------
// Helpers

func (s *Server) logf(format string, a ...any) {
	if os.Getenv("EMLCAL_E2E_VERBOSE") != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "[jmapfake] "+format+"\n", a...)
}

// parseState splits a "<kind>-<seq>" state token.
func parseState(s, kind string) (int, bool) {
	if s == "" {
		return 0, false
	}
	rest, ok := strings.CutPrefix(s, kind+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return n, true
}

// scanThreadHeaders pulls Message-ID and In-Reply-To out of a raw message
// without a full MIME parse; the fake only needs them for threading.
func scanThreadHeaders(raw []byte) (messageID, inReplyTo string) {
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	if h, _, ok := strings.Cut(string(raw), "\n\n"); ok && len(h) < len(head) {
		head = h
	}
	for _, line := range strings.Split(head, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "message-id:"):
			messageID = strings.TrimSpace(line[len("message-id:"):])
		case strings.HasPrefix(lower, "in-reply-to:"):
			inReplyTo = strings.TrimSpace(line[len("in-reply-to:"):])
		}
	}
	return messageID, inReplyTo
}

func applyPointerPatch(obj map[string]any, path string, val any) {
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

func methodError(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	out := map[string]any{}
	json.Unmarshal(b, &out)
	return out
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	if len(b) > 400 {
		return string(b[:400]) + "…"
	}
	return string(b)
}

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

func contains(list []string, s string) bool {
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

func reverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
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

func copyBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		if v {
			out[k] = true
		}
	}
	return out
}

func sliceToBoolMap(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, v := range list {
		out[v] = true
	}
	return out
}

func boolMapAny(m map[string]bool) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if v {
			out[k] = true
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
