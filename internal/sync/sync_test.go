package sync

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	stdsync "sync"
	"testing"
	"time"

	"github.com/teulaert/emlcalsync/internal/blob"
	"github.com/teulaert/emlcalsync/internal/config"
	"github.com/teulaert/emlcalsync/internal/mime"
	"github.com/teulaert/emlcalsync/internal/model"
	"github.com/teulaert/emlcalsync/internal/store"
)

// ---------------------------------------------------------------------------
// Harness

type harness struct {
	t     *testing.T
	dir   string
	st    *store.Store
	blobs *blob.Store
	cfg   *config.Config
	mail  *fakeMail
	cal   *fakeCalendar
	fact  *fakeFactory
	eng   *Engine

	mu     stdsync.Mutex
	events []ProgressEvent
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "emlcal.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	st.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	blobs, err := blob.Open(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}

	cfg := &config.Config{
		General: config.General{
			DataDir: dir, StateDir: dir, ConfigDir: dir,
			DefaultFormat: config.DefaultFormat,
			SecretBackend: config.BackendFile,
		},
		Accounts: []config.Account{{
			Name:        "work",
			Provider:    model.ProviderGmail,
			Email:       "user@example.com",
			Poll:        config.Duration(time.Hour),
			Mail:        true,
			Calendar:    true,
			Calendars:   []string{"*"},
			Concurrency: 2,
		}},
	}

	h := &harness{t: t, dir: dir, st: st, blobs: blobs, cfg: cfg}
	h.mail = newFakeMail()
	h.cal = newFakeCalendar()
	h.fact = &fakeFactory{mail: h.mail, cal: h.cal, pusher: h.mail, hasPush: true}

	eng, err := New(Options{
		Store:     st,
		Blobs:     blobs,
		Config:    cfg,
		Providers: h.fact,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		LockDir:   dir,
		Progress:  h.record,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.eng = eng
	return h
}

// newEngine builds a second engine over the same store/config, the way a
// second process would.
func (h *harness) newEngine() *Engine {
	h.t.Helper()
	eng, err := New(Options{
		Store: h.st, Blobs: h.blobs, Config: h.cfg, Providers: h.fact,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		LockDir: h.dir,
	})
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	return eng
}

func (h *harness) record(ev ProgressEvent) {
	h.mu.Lock()
	h.events = append(h.events, ev)
	h.mu.Unlock()
}

func (h *harness) progress() []ProgressEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ProgressEvent(nil), h.events...)
}

func (h *harness) sync(o SyncOptions) *Report {
	h.t.Helper()
	rep, err := h.eng.SyncAccount(context.Background(), "work", o)
	if err != nil {
		h.t.Fatalf("SyncAccount: %v", err)
	}
	return rep
}

func (h *harness) messages() []model.Message {
	h.t.Helper()
	msgs, err := h.st.ListMessages(context.Background(), store.MessageFilter{IncludeDeleted: true})
	if err != nil {
		h.t.Fatalf("ListMessages: %v", err)
	}
	return msgs
}

func (h *harness) message(remote string) *model.Message {
	h.t.Helper()
	m, err := h.st.GetMessage(context.Background(), "work", remote)
	if err != nil {
		h.t.Fatalf("GetMessage %s: %v", remote, err)
	}
	return m
}

func (h *harness) state() string {
	h.t.Helper()
	s, err := h.st.GetState(context.Background(), "work", resourceMail)
	if err != nil {
		h.t.Fatalf("GetState: %v", err)
	}
	return s
}

// mailRaw builds a small well-formed RFC 822 message.
func mailRaw(t *testing.T, subject, body string) []byte {
	t.Helper()
	raw, err := mime.Build(&mime.Draft{
		From:     model.Address{Name: "Sender", Email: "sender@example.com"},
		To:       []model.Address{{Email: "user@example.com"}},
		Subject:  subject,
		TextBody: body,
		Date:     time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("mime.Build: %v", err)
	}
	return raw
}

func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---------------------------------------------------------------------------
// Backfill

func TestBackfillFromEmpty(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "Hello one", "body about zeppelins")})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "Hello two", "body about submarines")})

	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindBackfill {
		t.Fatalf("kind = %q, want %q", rep.Mail.Kind, KindBackfill)
	}
	if rep.Mail.Added != 2 {
		t.Fatalf("added = %d, want 2", rep.Mail.Added)
	}

	msgs := h.messages()
	if len(msgs) != 2 {
		t.Fatalf("stored %d messages, want 2", len(msgs))
	}
	for _, m := range msgs {
		if !m.RawComplete || m.BlobSHA256 == "" {
			t.Fatalf("%s: raw not archived (complete=%v sha=%q)", m.RemoteID, m.RawComplete, m.BlobSHA256)
		}
		if !h.blobs.Exists(m.BlobSHA256) {
			t.Fatalf("%s: blob %s missing on disk", m.RemoteID, m.BlobSHA256)
		}
		if m.Subject == "" || m.TextBody == "" && m.Snippet == "" {
			t.Fatalf("%s: message not parsed: %+v", m.RemoteID, m)
		}
	}

	hits, err := h.st.Search(context.Background(), "zeppelins", store.MessageFilter{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Message.RemoteID != "m1" {
		t.Fatalf("search hits = %+v, want just m1", hits)
	}

	// Mailboxes came along, and membership was applied.
	if got := h.message("m1").MailboxRemotes; len(got) != 1 || got[0] != "INBOX" {
		t.Fatalf("m1 mailboxes = %v, want [INBOX]", got)
	}

	bf, err := h.st.GetBackfill(context.Background(), "work", resourceMail)
	if err != nil {
		t.Fatalf("GetBackfill: %v", err)
	}
	if !bf.Finished() {
		t.Fatal("backfill not marked finished")
	}
	if h.state() == "" {
		t.Fatal("mail state not persisted")
	}
	if len(h.progress()) == 0 {
		t.Fatal("no progress events emitted")
	}
}

func TestBackfillCapturesStateBeforeEnumeration(t *testing.T) {
	h := newHarness(t)
	h.mail.SetPageSize(1)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})

	// A message that arrives while we enumerate, on a page the cursor has
	// already passed: only the post-backfill delta can see it.
	h.mail.OnEnumerate(func(call int) {
		if call == 1 {
			h.mail.Add(&fakeMsg{id: "m3", hidden: true, raw: mailRaw(t, "three", "arrived mid-backfill")})
		}
	})

	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Added != 3 {
		t.Fatalf("added = %d, want 3 (2 enumerated + 1 from the replay delta)", rep.Mail.Added)
	}
	if got := len(h.messages()); got != 3 {
		t.Fatalf("stored %d messages, want 3", got)
	}
	if m := h.message("m3"); m.Subject != "three" {
		t.Fatalf("m3 not indexed by the replay delta: %+v", m)
	}
}

func TestBackfillResumesAfterCrash(t *testing.T) {
	h := newHarness(t)
	h.mail.SetPageSize(1)
	for _, id := range []string{"m1", "m2", "m3"} {
		h.mail.Add(&fakeMsg{id: id, raw: mailRaw(t, id, "body "+id)})
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.mail.OnEnumerate(func(call int) {
		if call == 2 {
			cancel() // crash right after the first page was committed
		}
	})
	if _, err := h.eng.SyncAccount(ctx, "work", SyncOptions{Mail: true}); err == nil {
		t.Fatal("expected the interrupted backfill to fail")
	}
	if got := len(h.messages()); got != 1 {
		t.Fatalf("after the crash %d messages are indexed, want 1", got)
	}
	bf, err := h.st.GetBackfill(context.Background(), "work", resourceMail)
	if err != nil {
		t.Fatalf("GetBackfill: %v", err)
	}
	if bf.Finished() || bf.Cursor == "" {
		t.Fatalf("backfill progress not resumable: %+v", bf)
	}

	h.mail.OnEnumerate(nil)
	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindResume {
		t.Fatalf("kind = %q, want %q", rep.Mail.Kind, KindResume)
	}
	if rep.Mail.Added != 2 {
		t.Fatalf("added = %d on resume, want 2 (the first page is skipped)", rep.Mail.Added)
	}
	msgs := h.messages()
	if len(msgs) != 3 {
		t.Fatalf("stored %d messages, want 3 (no duplicates)", len(msgs))
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		if seen[m.RemoteID] {
			t.Fatalf("duplicate row for %s", m.RemoteID)
		}
		seen[m.RemoteID] = true
	}
}

// ---------------------------------------------------------------------------
// Delta

func TestDeltaAddUpdateRemove(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
	h.sync(SyncOptions{Mail: true})

	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.mail.Update("m1", model.Flags{Flagged: true}, []string{"WORK"})

	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindDelta {
		t.Fatalf("kind = %q, want %q", rep.Mail.Kind, KindDelta)
	}
	if rep.Mail.Added != 1 || rep.Mail.Updated != 1 {
		t.Fatalf("added/updated = %d/%d, want 1/1", rep.Mail.Added, rep.Mail.Updated)
	}
	m1 := h.message("m1")
	if m1.Flags.Unread || !m1.Flags.Flagged {
		t.Fatalf("m1 flags = %+v, want flagged and read", m1.Flags)
	}
	if len(m1.MailboxRemotes) != 1 || m1.MailboxRemotes[0] != "WORK" {
		t.Fatalf("m1 mailboxes = %v, want [WORK]", m1.MailboxRemotes)
	}

	h.mail.Remove("m2")
	rep = h.sync(SyncOptions{Mail: true})
	if rep.Mail.Removed != 1 {
		t.Fatalf("removed = %d, want 1", rep.Mail.Removed)
	}
	m2 := h.message("m2")
	if m2.DeletedAt == nil {
		t.Fatal("m2 not marked deleted")
	}
	if len(m2.MailboxRemotes) != 0 {
		t.Fatalf("deleted message still in mailboxes %v", m2.MailboxRemotes)
	}
	// Nothing left to do: the state must still have advanced.
	before := h.state()
	h.sync(SyncOptions{Mail: true})
	if h.state() != before {
		t.Fatalf("state moved without changes: %q -> %q", before, h.state())
	}
}

func TestDeltaMarksMessagesTheProviderCannotProduce(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.mail.MarkGone("m2") // announced, then vanished before we fetched it
	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Added != 0 {
		t.Fatalf("added = %d, want 0", rep.Mail.Added)
	}
	if _, err := h.st.GetMessage(context.Background(), "work", "m2"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("m2 should never have been indexed, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Reconcile

func TestStateExpiredTriggersReconcile(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one"), flags: model.Flags{Unread: true}})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	h.sync(SyncOptions{Mail: true})

	h.mail.Remove("m2")
	h.mail.Add(&fakeMsg{id: "m3", raw: mailRaw(t, "three", "three")})
	h.mail.Update("m1", model.Flags{}, []string{"ARCHIVE"})
	h.mail.InjectStateExpired()

	rep := h.sync(SyncOptions{Mail: true})
	if rep.Mail.Kind != KindReconcile {
		t.Fatalf("kind = %q, want %q", rep.Mail.Kind, KindReconcile)
	}
	if h.message("m2").DeletedAt == nil {
		t.Fatal("m2 not marked deleted by the reconcile")
	}
	if m := h.message("m3"); m.Subject != "three" {
		t.Fatalf("m3 not picked up: %+v", m)
	}
	m1 := h.message("m1")
	if m1.Flags.Unread {
		t.Fatal("m1 flags not refreshed by the reconcile")
	}
	if len(m1.MailboxRemotes) != 1 || m1.MailboxRemotes[0] != "ARCHIVE" {
		t.Fatalf("m1 mailboxes = %v, want [ARCHIVE]", m1.MailboxRemotes)
	}

	entries, err := h.st.RecentSyncLog(context.Background(), "work", 10)
	if err != nil {
		t.Fatalf("RecentSyncLog: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Kind == KindReconcile {
			found = true
		}
	}
	if !found {
		t.Fatalf("no reconcile entry in the sync log: %+v", entries)
	}
}

func TestFullSyncReconcilesWithoutEnvelopeRefresh(t *testing.T) {
	h := newHarness(t)
	h.fact.mail = bareMail{f: h.mail} // no FetchEnvelopes
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two")})
	rep := h.sync(SyncOptions{Mail: true, Full: true})
	if rep.Mail.Kind != KindReconcile {
		t.Fatalf("kind = %q, want %q", rep.Mail.Kind, KindReconcile)
	}
	if rep.Mail.Added != 1 {
		t.Fatalf("added = %d, want 1", rep.Mail.Added)
	}
	if rep.Mail.Updated != 0 {
		t.Fatalf("updated = %d, want 0 (the provider cannot refresh envelopes)", rep.Mail.Updated)
	}
}

// ---------------------------------------------------------------------------
// raw_max_size

func TestRawMaxSizeStubAndEnsureRaw(t *testing.T) {
	h := newHarness(t)
	h.cfg.General.RawMaxSize = config.Size(1000)

	big := mailRaw(t, "huge", "pretend this has a 5 MB attachment")
	h.mail.Add(&fakeMsg{id: "m1", raw: big, size: 5_000_000})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "small", "small")})

	h.sync(SyncOptions{Mail: true})

	stub := h.message("m1")
	if stub.RawComplete {
		t.Fatal("m1 should be an envelope-only stub")
	}
	if stub.BlobSHA256 != "" {
		t.Fatalf("stub has a blob: %q", stub.BlobSHA256)
	}
	if stub.Subject != "(not fetched: 5 MB)" {
		t.Fatalf("stub subject = %q", stub.Subject)
	}
	if stub.Size != 5_000_000 {
		t.Fatalf("stub size = %d", stub.Size)
	}
	if !h.message("m2").RawComplete {
		t.Fatal("m2 should have been fetched in full")
	}

	raw, err := h.eng.EnsureRaw(context.Background(), "work", "m1")
	if err != nil {
		t.Fatalf("EnsureRaw: %v", err)
	}
	if len(raw) != len(big) {
		t.Fatalf("EnsureRaw returned %d bytes, want %d", len(raw), len(big))
	}
	filled := h.message("m1")
	if !filled.RawComplete || filled.BlobSHA256 == "" {
		t.Fatalf("m1 not completed: %+v", filled)
	}
	if filled.Subject != "huge" {
		t.Fatalf("subject = %q, want the parsed one", filled.Subject)
	}
	if !h.blobs.Exists(filled.BlobSHA256) {
		t.Fatal("blob not stored by EnsureRaw")
	}

	// A second call is served from the archive.
	again, err := h.eng.EnsureRaw(context.Background(), "work", "m1")
	if err != nil || len(again) != len(big) {
		t.Fatalf("EnsureRaw (cached) = %d bytes, %v", len(again), err)
	}
}

// ---------------------------------------------------------------------------
// Locking

func TestSyncAccountIsLockedWhileAnotherEngineHoldsIt(t *testing.T) {
	h := newHarness(t)
	release, err := h.eng.lockAccount("work")
	if err != nil {
		t.Fatalf("lockAccount: %v", err)
	}

	// Same process, a different engine: the flock has to catch it.
	other := h.newEngine()
	if _, err := other.SyncAccount(context.Background(), "work", SyncOptions{Mail: true}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second engine got %v, want ErrLocked", err)
	}
	// Same engine: the in-process guard catches it.
	if _, err := h.eng.SyncAccount(context.Background(), "work", SyncOptions{Mail: true}); !errors.Is(err, ErrLocked) {
		t.Fatalf("same engine got %v, want ErrLocked", err)
	}

	release()
	if _, err := other.SyncAccount(context.Background(), "work", SyncOptions{Mail: true}); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestSyncAllReportsPerAccount(t *testing.T) {
	h := newHarness(t)
	h.cfg.Accounts = append(h.cfg.Accounts, config.Account{
		Name: "personal", Provider: model.ProviderFastmail, Email: "me@example.com",
		Poll: config.Duration(time.Hour), Mail: true, Calendar: true, Calendars: []string{"*"},
	})
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})

	reports, err := h.eng.SyncAll(context.Background(), SyncOptions{Mail: true})
	if err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2", len(reports))
	}
	for _, r := range reports {
		if r.Err != nil {
			t.Fatalf("%s: %v", r.Account, r.Err)
		}
		if r.Mail == nil || r.Mail.Added != 1 {
			t.Fatalf("%s: mail report %+v", r.Account, r.Mail)
		}
	}
}

func TestUnknownMailboxTriggersARefresh(t *testing.T) {
	h := newHarness(t)
	h.mail.Add(&fakeMsg{id: "m1", raw: mailRaw(t, "one", "one")})
	h.sync(SyncOptions{Mail: true})

	// A label created on the server, arriving on a message before any hint
	// that the mailbox list changed.
	h.mail.AddMailbox(model.Mailbox{RemoteID: "NEW", Name: "Newsletters"})
	h.mail.Add(&fakeMsg{id: "m2", raw: mailRaw(t, "two", "two"), mailboxes: []string{"NEW"}})

	h.sync(SyncOptions{Mail: true})
	got := h.message("m2").MailboxRemotes
	if len(got) != 1 || got[0] != "NEW" {
		t.Fatalf("m2 mailboxes = %v, want [NEW] after the refresh", got)
	}
	boxes, err := h.st.ListMailboxes(context.Background(), "work")
	if err != nil {
		t.Fatalf("ListMailboxes: %v", err)
	}
	found := false
	for _, b := range boxes {
		if b.RemoteID == "NEW" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mailbox list not refreshed: %+v", boxes)
	}
}
