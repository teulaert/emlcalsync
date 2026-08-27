# emlcal — Design Document

Status: draft for review · 2026-08-25

`emlcal` is a single Go binary that keeps a **complete local archive** of all
mail and calendar accounts (Gmail ×N, Fastmail ×N) and exposes it through an
**agent-friendly CLI**. The archive is the product; the CLI, and later TUIs and
Omarchy integrations, are views on it.

---

## 1. Goals and non-goals

### Goals

- **Full archive, offline-first.** Every message of every account is stored
  locally as raw RFC 822, forever. All read operations work with no network.
  A local AI model must be able to work on the complete history.
- **Continuously synced.** Initial backfill, then cheap incremental delta sync
  (seconds of latency for Fastmail via push, ~1 minute for Gmail via polling).
- **One CLI for everything.** Multi-account by default, stable IDs, `--json`
  everywhere, read/write commands split at the subcommand level so Claude Code
  permission rules can allowlist reads and gate writes.
- **Rebuildable.** The SQLite index is derived data; `emlcal reindex` rebuilds
  it from the blob archive.
- **Foundation for UIs.** TUIs / Omarchy plugins read the same SQLite DB
  through an internal Go package, never through a second provider client.

### Non-goals (for now)

- An open-ended *tested* server list. The IMAP and CalDAV *clients* are
  vendor-neutral — they have to be, since vendors differ only in preset — but
  the supported vendors stay a curated set (§6.4, §6.5), because what a server
  does at the edges (capabilities, discovery, redirects, connection limits) can
  only be verified against one that is actually being used. Anything else works
  by explicit configuration and is supported on that basis.
- An open-ended CalDAV server list. The CalDAV *client* is vendor-neutral —
  it has to be, since Fastmail and iCloud differ only in preset — but the
  supported vendors stay a curated set (§6.4), because what a server does at
  the edges (discovery, redirects, scheduling) can only be verified against
  one that is actually being used.
- An MCP server. Trivial to add later on top of the same internal API.
- Contacts sync (possible later via People API / JMAP Contacts).
- Being a mail *client* with a rendering engine — HTML is stored, not rendered.

---

## 2. Architecture

```
  ┌────────────────────────┐   ┌────────────────────────┐
  │  Gmail API / GCal API  │   │  Fastmail JMAP (+push) │      providers
  └───────────┬────────────┘   └───────────┬────────────┘
              │ raw RFC822 + labels        │ raw RFC822 + mailboxes/keywords
              ▼                            ▼
  ┌──────────────────────────────────────────────────────┐
  │  sync engine   backfill → delta → reconcile · outbox │
  └───────────┬──────────────────────────────┬───────────┘
              │ write                        │ parse (MIME → text)
              ▼                              ▼
  ┌──────────────────────┐        ┌──────────────────────┐
  │  blob archive        │        │  SQLite index        │
  │  raw .eml, zstd,     │───────▶│  messages, mailboxes │
  │  content-addressed   │ reindex│  FTS5, events, state │
  └──────────────────────┘        └──────────┬───────────┘
                                             │ read-only queries
                       ┌─────────────────────┼─────────────────────┐
                       ▼                     ▼                     ▼
                 emlcal CLI            future TUI           Omarchy plugins
                 (agent + human)       (bubbletea)          (walker menus, etc.)
```

Principles:

1. **Raw RFC 822 is canonical.** Both providers hand out the full raw message in
   one call. We store it verbatim (compressed) and parse it locally. Nothing
   in SQLite is unrecoverable.
2. **Sync writes, everything else reads.** The sync process is the only thing
   that writes provider state into the index. CLI write commands
   (archive, send, …) go to the provider first, then optimistically patch the
   index; the next delta confirms.
3. **Unified model = JMAP model.** A message belongs to *many* mailboxes and
   has a set of flags. Gmail labels map onto this directly.

---

## 3. On-disk layout (XDG)

```
~/.config/emlcal/
  config.toml                     accounts, policies (no secrets)
  secrets/                        0600 — OAuth tokens, Fastmail API tokens
    work.gmail.json
    personal.fastmail.token

~/.local/share/emlcal/
  emlcal.db                       SQLite, WAL mode
  emlcal.db-wal / -shm
  blobs/
    ab/ab3f…e9.eml.zst            raw RFC822, sha256 of uncompressed bytes
    …

~/.local/state/emlcal/
  emlcal.log
  sync.<account>.lock             flock per account
```

Back up = copy `~/.local/share/emlcal` (the index is optional; blobs are the
archive). Secrets live separately so the data dir can be synced elsewhere.

---

## 4. Blob archive

- Key: `sha256(raw bytes)`. Path: `blobs/<first 2 hex>/<sha256>.eml.zst`.
- Compression: zstd level 3 (`klauspost/compress`). Base64 attachment bodies
  still compress ~25–30 %; text-heavy mail 70 %+.
- Content addressing gives free deduplication: the same message received in two
  accounts (very common with forwarding / CCs to yourself) is stored once.
  `messages` rows still exist per account — they just point at the same blob.
- Writes are atomic: write to `blobs/tmp/<random>`, fsync, rename.
- A blob is never mutated. Deletion on the server marks the `messages` row
  `deleted_at`; the blob stays (it's an archive). `emlcal gc --purge-deleted`
  exists for people who want server semantics.

**Attachment policy.** Because raw includes attachments, backfilling raw =
backfilling everything. Per-account setting:

```toml
raw_max_size = "0"        # 0 = unlimited (default: full archive)
```

If a message exceeds `raw_max_size`, only the text parts are fetched (Gmail
`format=full`, JMAP `Email/get` with body values) and the row is marked
`raw_complete = 0`; `emlcal mail attachment get` fetches on demand.

---

## 5. SQLite data model

WAL mode, `busy_timeout = 5000`, `synchronous = NORMAL`, `foreign_keys = ON`.
Migrations embedded in the binary (`internal/store/migrations/*.sql`).

```sql
CREATE TABLE accounts (
  id            TEXT PRIMARY KEY,          -- "work", "personal" (from config)
  provider      TEXT NOT NULL,             -- 'gmail' | 'fastmail'
  email         TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE mailboxes (
  id            INTEGER PRIMARY KEY,
  account_id    TEXT NOT NULL REFERENCES accounts(id),
  remote_id     TEXT NOT NULL,             -- Gmail labelId / JMAP mailbox id
  name          TEXT NOT NULL,             -- display name
  role          TEXT,                      -- inbox|archive|sent|drafts|trash|junk|
                                           -- important|category:* |NULL (user label)
  parent_id     INTEGER REFERENCES mailboxes(id),
  sort_order    INTEGER,
  total_count   INTEGER,
  unread_count  INTEGER,
  UNIQUE (account_id, remote_id)
);

CREATE TABLE messages (
  id             INTEGER PRIMARY KEY,
  account_id     TEXT NOT NULL REFERENCES accounts(id),
  remote_id      TEXT NOT NULL,            -- Gmail message id / JMAP Email id
  thread_id      TEXT NOT NULL,            -- provider thread id
  blob_sha256    TEXT,                     -- NULL only when raw_complete = 0
  raw_complete   INTEGER NOT NULL DEFAULT 1,
  message_id_hdr TEXT,                     -- Message-ID header (for cross-account stitching)
  in_reply_to    TEXT,
  references_json TEXT,                    -- JSON array
  subject        TEXT,
  from_addr      TEXT, from_name TEXT,
  to_json        TEXT, cc_json TEXT, bcc_json TEXT, reply_to_json TEXT,
  date_utc       INTEGER NOT NULL,         -- Date header, unix seconds
  received_utc   INTEGER NOT NULL,         -- provider internalDate / receivedAt
  size           INTEGER,
  snippet        TEXT,                     -- first ~200 chars of text body
  text_body      TEXT,                     -- extracted plain text, full (incl. quotes)
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_unread      INTEGER NOT NULL DEFAULT 0,
  is_flagged     INTEGER NOT NULL DEFAULT 0,
  is_draft       INTEGER NOT NULL DEFAULT 0,
  is_answered    INTEGER NOT NULL DEFAULT 0,
  deleted_at     INTEGER,                  -- set when gone on server
  indexed_at     INTEGER NOT NULL,
  UNIQUE (account_id, remote_id)
);
CREATE INDEX messages_thread   ON messages(account_id, thread_id);
CREATE INDEX messages_received ON messages(received_utc DESC);
CREATE INDEX messages_msgid    ON messages(message_id_hdr);

CREATE TABLE message_mailboxes (
  message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  PRIMARY KEY (message_id, mailbox_id)
);
CREATE INDEX mm_mailbox ON message_mailboxes(mailbox_id);

CREATE TABLE attachments (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  part_path    TEXT NOT NULL,              -- MIME part path, e.g. "1.2"
  filename     TEXT,
  content_type TEXT,
  size         INTEGER,
  content_id   TEXT,
  is_inline    INTEGER NOT NULL DEFAULT 0,
  remote_ref   TEXT                        -- Gmail attachmentId / JMAP blobId (lazy fetch)
);

CREATE TABLE threads (                     -- maintained at index time, for fast listing
  account_id    TEXT NOT NULL,
  thread_id     TEXT NOT NULL,
  subject       TEXT,
  first_utc     INTEGER, last_utc INTEGER,
  message_count INTEGER, unread_count INTEGER,
  participants_json TEXT,
  PRIMARY KEY (account_id, thread_id)
);

-- Full-text search: external-content FTS5 so text is stored once.
CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, from_addr, from_name, to_json, text_body, attachment_names,
  content='messages', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2',
  prefix='2 3'
);
-- + the standard insert/update/delete triggers.

CREATE TABLE sync_state (
  account_id TEXT NOT NULL,
  resource   TEXT NOT NULL,                -- 'mail' | 'mailboxes' | 'cal:<calendar remote_id>'
  state      TEXT NOT NULL,                -- Gmail historyId / JMAP state / GCal syncToken
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, resource)
);

CREATE TABLE backfill_progress (
  account_id   TEXT PRIMARY KEY,
  resource     TEXT NOT NULL,
  cursor       TEXT,                       -- page token / JMAP position
  state_at_start TEXT NOT NULL,            -- delta state captured before backfill began
  total_hint   INTEGER, done INTEGER,
  finished_at  INTEGER
);

CREATE TABLE outbox (                      -- queued writes (offline or failed)
  id          INTEGER PRIMARY KEY,
  account_id  TEXT NOT NULL,
  kind        TEXT NOT NULL,               -- send|draft|flags|move|trash|event.create|…
  payload     TEXT NOT NULL,               -- JSON
  created_at  INTEGER NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  done_at     INTEGER
);

CREATE TABLE sync_log (
  id INTEGER PRIMARY KEY, account_id TEXT, kind TEXT,
  started_at INTEGER, finished_at INTEGER,
  added INTEGER, updated INTEGER, removed INTEGER, error TEXT
);

-- Calendar ---------------------------------------------------------------
CREATE TABLE calendars (
  id          INTEGER PRIMARY KEY,
  account_id  TEXT NOT NULL REFERENCES accounts(id),
  remote_id   TEXT NOT NULL,
  name        TEXT NOT NULL,
  color       TEXT, timezone TEXT,
  is_primary  INTEGER NOT NULL DEFAULT 0,
  access_role TEXT,                        -- owner|writer|reader
  UNIQUE (account_id, remote_id)
);

CREATE TABLE events (
  id            INTEGER PRIMARY KEY,
  calendar_id   INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  remote_id     TEXT NOT NULL,
  uid           TEXT,                      -- iCalendar UID (cross-account dedup)
  title         TEXT, description TEXT, location TEXT,
  start_utc     INTEGER, end_utc INTEGER,  -- master occurrence
  all_day       INTEGER NOT NULL DEFAULT 0,
  timezone      TEXT,
  rrule         TEXT,                      -- RFC 5545 RRULE, NULL if single
  recurrence_id TEXT,                      -- set on exception instances
  status        TEXT,                      -- confirmed|tentative|cancelled
  organizer     TEXT, attendees_json TEXT,
  my_response   TEXT,                      -- accepted|declined|tentative|needs-action
  raw_json      TEXT NOT NULL,             -- provider object, for fidelity/writes
  updated_utc   INTEGER, deleted_at INTEGER,
  UNIQUE (calendar_id, remote_id)
);

CREATE TABLE event_occurrences (           -- expanded recurrences, ±2 years, rebuilt on change
  event_id  INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  start_utc INTEGER NOT NULL, end_utc INTEGER NOT NULL,
  PRIMARY KEY (event_id, start_utc)
);
CREATE INDEX occ_start ON event_occurrences(start_utc);
```

Notes:

- Flags (`is_unread`, `is_flagged`, …) are columns, not mailboxes. Gmail's
  `UNREAD`/`STARRED` labels and JMAP's `$seen`/`$flagged` keywords are both
  normalised into them at index time.
- Gmail system labels (`INBOX`, `SENT`, `DRAFT`, `SPAM`, `TRASH`, `IMPORTANT`,
  `CATEGORY_*`) become `mailboxes` rows with a `role`. Gmail "archive" =
  no `INBOX` label, matching JMAP "archive" mailbox semantics closely enough;
  `emlcal mail archive` does the right thing per provider.
- `text_body` holds the *full* extracted text (search must find quoted text).
  `mail read` strips quotes/signatures at display time.

---

## 6. Providers

```go
type MailProvider interface {
    Mailboxes(ctx) ([]Mailbox, error)

    // Backfill enumerates every message id (+ labels/flags) from a cursor.
    Enumerate(ctx, cursor string, limit int) (page []Envelope, next string, err error)
    // FetchRaw returns raw RFC822 + current labels/flags for a batch of ids.
    FetchRaw(ctx, ids []string, fn func(RawMessage) error) error

    // Delta since an opaque state. Returns ErrStateExpired when the server
    // can no longer compute it; caller falls back to Reconcile.
    Changes(ctx, since string) (*Changes, error)

    SetFlags(ctx, ids []string, set, clear Flags) error
    SetMailboxes(ctx, ids []string, add, remove []string) error
    Trash(ctx, ids []string) error
    CreateDraft(ctx, raw []byte) (id string, err error)
    Send(ctx, raw []byte, replyToThread string) (id string, err error)
}

type CalendarProvider interface {
    Calendars(ctx) ([]Calendar, error)
    Changes(ctx, calendarID, since string) (*EventChanges, error)   // full list when since==""
    Create/Update/Delete(ctx, …) (…)
}

type Pusher interface {                      // optional
    Watch(ctx, fn func(hint ChangeHint)) error
}
```

### 6.1 Gmail (`internal/provider/gmail`)

- Library: `google.golang.org/api/gmail/v1`, `golang.org/x/oauth2/google`.
- Auth: Desktop OAuth client, loopback redirect (`http://127.0.0.1:<port>`),
  PKCE. Scopes: `gmail.modify` (covers read, labels, send, trash — not permanent
  delete) and `calendar`. Consent screen must be **In production** or refresh
  tokens expire after 7 days. Refreshed tokens are persisted back to
  `secrets/`.
- Mailboxes: `users.labels.list`.
- Enumerate: `users.messages.list` with `includeSpamTrash=true`, `maxResults=500`,
  page token as cursor. Returns ids + threadIds only.
- FetchRaw: `users.messages.get?format=raw` in **batch requests** (50 per HTTP
  call). One response gives `raw` (base64url), `labelIds`, `threadId`,
  `internalDate`, `historyId`, `sizeEstimate`. Cost 5 units; per-user quota
  15 000 units/min → ~50 msg/s sustained. Backoff on 429/403 `rateLimitExceeded`.
- Changes: `users.history.list?startHistoryId=…&historyTypes=messageAdded,
  messageDeleted,labelAdded,labelRemoved`. Events are coalesced per message id
  before applying (history is noisy and can repeat). New state = the largest
  `historyId` seen. HTTP 404 → `ErrStateExpired`.
- Reconcile (fallback): re-enumerate all ids (cheap: 200 pages for 100k
  messages). New ids → FetchRaw. Missing ids → `deleted_at`. Then refresh
  labels for everything via `format=minimal` batches (100k msgs ≈ 35 min;
  acceptable for a rare event). State = current `profile.historyId`.
- Push: not used (requires Cloud Pub/Sub). Poll `history.list` every 60 s
  (2 units per call).
- Send: `users.messages.send` with raw RFC822 built locally, `threadId` set for
  replies. Drafts via `users.drafts.create`.

### 6.2 Fastmail JMAP (`internal/provider/jmap`)

- Library: `git.sr.ht/~rockorager/go-jmap` (used by aerc) or a thin hand-rolled
  client — JMAP is plain JSON; the hand-rolled path is ~500 lines and avoids
  a dependency with a small bus factor. Decide during phase 1.
- Auth: API token from Fastmail settings (scopes: Mail, Calendars),
  `Authorization: Bearer`. Session at `https://api.fastmail.com/jmap/session`
  → `apiUrl`, `downloadUrl`, `eventSourceUrl`, account ids, limits
  (`maxObjectsInGet`, `maxCallsInRequest`, `maxConcurrentRequests`).
- Mailboxes: `Mailbox/get` (roles are native).
- Enumerate: `Email/query` sorted by `receivedAt` asc, `position` as cursor,
  `limit` = min(500, server limit). Then `Email/get` with
  `properties: [blobId, threadId, mailboxIds, keywords, receivedAt, size]`
  chained in the same request via `#ids` back-reference.
- FetchRaw: the Email object's own `blobId` **is** the raw RFC822. Download via
  `downloadUrl` template (`{accountId}`, `{blobId}`, `{type}`, `{name}`),
  8 concurrent workers.
- Changes: `Email/changes` + `Mailbox/changes` with `sinceState`, loop on
  `hasMoreChanges`. `updated` ids → `Email/get` with `mailboxIds, keywords`
  only. `cannotCalculateChanges` → `ErrStateExpired` → reconcile via full
  `Email/query` id diff (fast; Fastmail is quick).
- Push: EventSource at `eventSourceUrl?types=Email,Mailbox,CalendarEvent&
  closeafter=no&ping=300`. A `StateChange` with a new state triggers a delta
  immediately. Reconnect with backoff.
- Send: `Email/import` (drafts mailbox) → `EmailSubmission/set`. Or
  `Email/set` create + submission in one request.

### 6.3 Google Calendar (`internal/provider/gcal`)

- `calendarList.list` → calendars. Per calendar, `events.list` with
  `singleEvents=false` (masters + exception instances), `showDeleted=true`,
  `syncToken` persisted in `sync_state` as `cal:<id>`. 410 GONE → full re-list.
- Recurrence expansion happens locally (`teambition/rrule-go`) into
  `event_occurrences` for now−1y … now+2y; refreshed when the master changes
  and by a nightly job that extends the window.
- Poll every 120 s. Writes via `events.insert/patch/delete`; `raw_json` holds
  the last server object so patches are minimal.

### 6.4 CalDAV calendars — Fastmail, iCloud (`internal/provider/caldav`)

The primary calendar path for everything that is not Google. RFC 4791 for the
objects, RFC 6578 `sync-collection` for deltas, `calendar-multiget` to fetch
the changed `.ics` text, `If-Match` on PUT. Parsing is `emersion/go-ical`; the
WebDAV transport is this package's own (`dav.go`), because go-webdav's client
exposes neither the raw `.ics`, nor sync-collection, nor `If-Match` — nor the
*host* of an href, which §6.4.1 turns out to need.

What differs between servers is a preset (`presets.go`) keyed by vendor: the
DAV root, where the user creates a password, the default calendar's name, and
whether the conventional home path can be guessed at all.

| | Fastmail | iCloud |
|---|---|---|
| root | `caldav.fastmail.com/dav/` | `caldav.icloud.com/` |
| home | `/dav/calendars/user/<email>/`, guessable | `/<dsid>/calendars/`, **not** guessable |
| credential | app password | app-specific password (2FA required) |
| auth user | the address | the Apple ID, often *not* the address |

Authentication is HTTP basic with a per-application password, never a login
password: Fastmail's JMAP API tokens carry no calendars scope, and iCloud has
no other option. A CalDAV backend with no stored password reports
`provider.ErrNotSupported`, so the engine skips calendars and the rest of the
account keeps working.

JMAP Calendars (`internal/provider/jmap/calendar.go`) still exists and is
selectable as `backend = "jmap"`, for a JMAP server whose token does grant
`urn:ietf:params:jmap:calendars`. Fastmail's does not, which is what made the
backend a per-resource choice in the first place (§11).

#### 6.4.1 Hosts, hrefs and redirects

Remote ids are bare paths, never absolute URLs, because they are persisted in
`events.remote_id`. iCloud answers discovery with a calendar home on a
*different*, per-user host (`p<NN>-caldav.icloud.com`), so the client records
that host separately and resolves later paths against it — stored ids are
untouched, and they survive Apple moving an account between partitions.

This is why discovery is two PROPFINDs of our own rather than go-webdav's:
`FindCurrentUserPrincipal` and `FindCalendarHomeSet` both return only the
*path* of the href they found, discarding exactly the host that matters.

Redirects are refused, not followed. Go rewrites a redirected PROPFIND, REPORT
or PUT into a GET, so following one appears to succeed while doing nothing —
in testing it surfaced as "calendar not found", which is a misdiagnosis rather
than a failure. Only discovery adopts a redirect, and it does so by taking the
new host explicitly.

Discovery runs on entry to every public method, not as a side effect of
`Calendars()`: the outbox reaches `CreateEvent` and friends on a client that
has never listed anything (§7).

---

### 6.5 IMAP mail — iCloud, self-hosted (`internal/provider/imap`)

The vendor-neutral mail client, matching §6.4's shape: a `Preset` per known
vendor (host, ports, credential vocabulary, connection cap, role names), and an
explicit `host` for everything else. SMTP submission rides along in the same
block, because sending is the half IMAP cannot do — it is not a separate
resource, it is the write end of this one.

**Message identity is per-copy.** A message on IMAP is
`(folder, uidvalidity, uid)`, so the same mail filed twice is genuinely two
objects and a move mints a new uid. `RemoteID` is
`base64url(folder).uidvalidity.uid` — base64url because `model.ParseID` splits
public ids on `:` and folder names are whatever the user called them.

RFC 8474 `EMAILID` would give a stable id, and was the first choice. It is not
reachable: `go-imap/v2`'s `FetchOptions` has no field for it, and the FETCH
parser's default branch is an error that tears the connection down, so asking
is not a graceful degradation. Only some servers advertise `OBJECTID` anyway,
so the per-copy path would still be the one most accounts took. Instead, writes
report what they moved (`provider.Remapper`, from the server's `COPYUID`) and
the engine renames the index row — keeping its blob, thread, flags and search
entry — rather than deleting it and downloading the same bytes again.

**The sync state carries the UID set.** IMAP has no change log, and `go-imap/v2`
has no QRESYNC (`VANISHED` is absent from the untagged-response switch, whose
default is likewise fatal), so nothing on the wire says what vanished. The
state — already an opaque `TEXT` column — holds per folder its UIDVALIDITY,
UIDNEXT, counts, the live uid set and the flag sets, all as IMAP range strings.
A healthy folder is one range, so a whole account is a few hundred bytes; a
pathological one is gzipped past 32 KiB.

That is what makes the awkward cases exact rather than catastrophic. A
UIDVALIDITY reset, a deleted folder and a renamed folder are all reported as
precise `Added`/`Removed`/`Renamed`, so **`ErrStateExpired` is reserved for a
state this build cannot parse** (plus a `changesMaxAdded` guard, where the
engine's paged backfill is the better tool). Storing the flags is what keeps a
quiet account quiet: without them every poll would re-read recent flags and
report them all as updates.

**Cost.** An account where nothing happened is one `LIST`(+`STATUS`) and no
`SELECT`. A folder is skipped entirely unless UIDNEXT, MESSAGES, UNSEEN or
(with CONDSTORE) HIGHESTMODSEQ moved.

**Degradation.** Every capability is optional and its absence costs something
specific:

| Missing | Cost |
|---|---|
| CONDSTORE | flag changes other than read/unread wait for the periodic sweep (~1 h) |
| UIDPLUS / MOVE | no `COPYUID`, so a move is discovered as a delete plus an add — and with neither, the source copy is flagged `\Deleted` but *not* expunged, because a bare `EXPUNGE` would destroy every `\Deleted` message in the folder including another client's |
| SPECIAL-USE | roles fall back to the preset's names, then to the names servers converge on, then to explicit `archive_folder`/`sent_folder`/… config |
| LIST-STATUS | one `STATUS` per folder instead of one `LIST` |
| IDLE | polling only |

**Care that is not obvious.** `BODY.PEEK[]`, never `RFC822` — the latter sets
`\Seen` and would mark the whole archive read on first contact. `\Seen` inverts
against `model.Flags.Unread`. `\Flagged` is not mapped to a role: it is a
virtual starred view, and filing into it would write somewhere that cannot hold
messages. `\All` (and the name "All Mail") is excluded by default, because
under per-copy identity it files a second copy of the entire account.

**Sending** is SMTP submission, then an `APPEND` to Sent unless the preset says
the server files its own copy. Submission comes first: a copy in Sent for a
message that was never sent is a lie nothing repairs, while a sent message
missing from Sent is repaired by the next sync. A failed `APPEND` therefore
never fails the send. Errors are classified by phase — up to the last `RCPT`
nothing was handed over and the outbox may retry; from the first byte of `DATA`
the outcome is unknowable and it must not, or the message goes twice.

**Threading** is stitched by the store (§5, `message_refs`), not the provider: a
reply is often indexed before the message it answers, and only the index sees
the whole account.

**Known unknowns.** iCloud's preset is written from documentation. Its exact
capabilities, whether it advertises `OBJECTID`, whether its submission files its
own Sent copy, and its real connection limit are unverified — which is why
`emlcal doctor` prints what the server actually advertises.

---

## 7. Sync engine

Per account, run in this order; each step is idempotent and resumable.

### 7.1 Backfill (first run, or after `--full`)

```
1. capture state S0 = provider current delta state       (before listing!)
2. store backfill_progress{cursor: "", state_at_start: S0}
3. loop:
     page, next = Enumerate(cursor)
     for ids not yet in messages (or raw_complete=0): FetchRaw in worker pool
       → blob.Put(raw) → mime.Parse → upsert messages/mailboxes/attachments/fts
       (transaction per ~100 messages)
     cursor = next; persist progress
   until next == ""
4. mark backfill finished; sync_state.mail = S0
5. run a normal delta from S0 to catch what changed during the backfill
```

Because the state token is captured *before* enumeration, nothing that happens
during a multi-hour backfill is lost. Restarting mid-way resumes from the
persisted cursor and skips ids that already have a blob.

Gmail throughput: ~50 msg/s → 100k messages ≈ 35 min, 500k ≈ 3 h.
Fastmail: typically faster; bounded by blob download bandwidth.

### 7.2 Delta (every tick)

```
changes = Changes(sync_state.mail)
  ErrStateExpired → Reconcile(); return
coalesce by message id (last event wins; add+delete → delete)
added   → FetchRaw → index
updated → patch flags/mailboxes only (no re-download)
removed → deleted_at = now, remove from message_mailboxes/fts
sync_state.mail = changes.NewState      (same transaction as the last apply)
```

State is advanced only after everything up to it is applied, so a crash
replays rather than skips.

### 7.3 Reconcile

Full id enumeration diffed against the local set; see 6.1 / 6.2. Logged loudly
in `sync_log` because it's slow and should be rare.

### 7.4 Outbox

Write commands construct an outbox row *first*, then try to apply it
immediately. Success → `done_at`. Network failure → row stays, the sync loop
retries with exponential backoff, and `emlcal status` shows pending items.
This is what makes "compose offline, send when back" work, and it makes every
write crash-safe. Kinds: `send`, `draft`, `flags`, `mailboxes`, `trash`,
`event.create|update|delete`.

### 7.5 Scheduling

- `emlcal sync` — one pass over all (or `--account`) accounts, then exit.
- `emlcal sync --watch` — long-running: JMAP push streams + Gmail/GCal polling
  timers + outbox retry. This is the systemd user service.
- `emlcal service install` writes `~/.config/systemd/user/emlcal.service`
  (`Restart=always`, `After=network-online.target`) and enables it.
  A `--timer` variant installs a `.timer` calling `emlcal sync` every 2 min
  for people who don't want a daemon.

### 7.6 Concurrency and locking

- One writer per account: `flock` on `sync.<account>.lock`. A manual
  `emlcal sync` while the daemon runs prints "daemon active — nudged" and
  sends `SIGUSR1` to trigger an immediate pass.
- SQLite WAL + `busy_timeout` handles the two writers that do exist (sync
  process, CLI write commands patching optimistically). Readers never block.
- Worker pools: Gmail 4 concurrent batch requests; JMAP 8 concurrent blob
  downloads. All provider calls go through a per-account rate limiter with
  retry-after awareness.

---

## 8. MIME parsing and text extraction (`internal/mime`)

Library: `github.com/emersion/go-message` (+ `mail` sub-package), stdlib
`mime/quotedprintable`, `golang.org/x/text/encoding` for charset soup,
`github.com/k3a/html2text` for HTML.

At index time, for each raw message:

1. Walk the MIME tree; record every leaf as a part with its path (`1.2.1`).
2. Choose the body: first `text/plain` leaf not marked attachment; else the
   first `text/html` → html2text (links kept as `text (url)`, tables kept as
   rows, scripts/styles dropped).
3. Decode charset (fall back to windows-1252 on failure — real-world mail),
   normalise line endings, collapse >2 blank lines.
4. Attachments: any part with `Content-Disposition: attachment`, or a non-text
   part with a filename, or an inline image. Store metadata; content is read
   from the blob on demand (`part_path` → re-walk).
5. Headers: `Message-ID`, `In-Reply-To`, `References`, `List-Id`,
   `Auto-Submitted`, `Precedence` (the latter three are used by
   `mail list --no-bulk`).

At display time (`mail read`, default):

- Strip quoted replies: lines starting with `>`, blocks after
  `On … wrote:` / `Op … schreef:` / `-----Original Message-----` /
  Outlook `From: … Sent: …` headers, `Le … a écrit :`, `Am … schrieb …`.
- Strip signature after `-- \n` or a trailing block matching common patterns.
- `--full` disables stripping; `--html` returns the HTML part; `--raw` the
  RFC822.

Nothing here mutates stored data, so heuristics can improve without reindexing.

---

## 9. CLI surface

Binary: `emlcal`. Framework: `spf13/cobra` (good `--help`, completions for
zsh/bash/fish).

### 9.1 Output contract

- `--format json|table|plain` (`-o`). Default: `table` on a TTY, `json` when
  piped — so agents get JSON without asking; `EMLCAL_FORMAT=json` to force.
- JSON is always an object for single items, an array for lists, never
  pretty-printed unless `--pretty`. Timestamps are RFC 3339 in local time
  with offset, plus `_utc` epoch fields.
- Errors: JSON `{"error": {"code": "...", "message": "..."}}` on stderr;
  exit codes: `0` ok, `1` generic, `2` usage, `3` not found, `4` offline and
  operation needs network, `5` provider error, `6` queued in outbox.
- IDs are stable and opaque: `<account>:<remote_id>` for messages
  (`work:18f3a2b9c1d4e5f6`), `<account>:t:<thread_id>` for threads,
  `<account>:c:<calendar>:<event_id>` for events. Every list output includes
  the id; every command that takes an id accepts what a list printed.
- `--limit` defaults to 50; `--since 2d|12h|2026-08-01` and `--until` on every
  list; `--account` is repeatable and defaults to all.

### 9.2 Commands

```
emlcal account add gmail --name work            OAuth in browser, loopback
emlcal account add fastmail --name personal     prompts for API token
emlcal account list | remove <name>

emlcal sync [--account A] [--full] [--watch]
emlcal status                                   per account: last sync, counts,
                                                backfill %, outbox, daemon state
emlcal doctor                                   tokens valid? db integrity? disk?

MAIL — read (safe to allowlist)
emlcal mail mailboxes [--account A]
emlcal mail list [--mailbox inbox] [--unread] [--flagged] [--from X] [--to X]
                 [--since 2d] [--no-bulk] [--thread] [--limit N]
emlcal mail search "<fts query>" [same filters]     FTS5 syntax: AND OR NOT "phrase" col:term
emlcal mail read <id> [--full] [--html] [--raw] [--headers]
emlcal mail thread <id>                              all messages, oldest first, stripped bodies
emlcal mail attachment list <id>
emlcal mail attachment get <id> <part|filename> [-O path]   (fetches remote if raw_complete=0; -o is the format flag)

MAIL — write (gate behind confirmation)
emlcal mail mark <id>... --read|--unread|--flag|--unflag
emlcal mail move <id>... --to <mailbox>
emlcal mail archive <id>...
emlcal mail trash <id>...
emlcal mail draft  --account A --to .. [--cc ..] --subject .. (--body .. | --body-file f)
                   [--reply <id> [--all]] [--attach f]         → draft id
emlcal mail send   --draft <id>  |  (same flags as draft) [--dry-run]
emlcal mail reply  <id> (--body .. | --body-file f) [--all] [--dry-run]

CALENDAR — read
emlcal cal calendars [--account A]
emlcal cal agenda [--days 7 | --from .. --to ..] [--calendar C]
emlcal cal show <id>
emlcal cal free --from .. --to .. [--duration 30m] [--hours 09:00-18:00]

CALENDAR — write
emlcal cal create --title .. --start .. --end .. [--calendar C] [--attendees ..]
                  [--location ..] [--description ..] [--dry-run]
emlcal cal update <id> [same flags]
emlcal cal delete <id>
emlcal cal respond <id> --accept|--decline|--tentative

MAINTENANCE
emlcal outbox list | retry | drop <id>
emlcal reindex [--account A]                  rebuild index from blobs
emlcal gc [--purge-deleted]                   remove blobs no row references
emlcal export --mbox f | --maildir dir [--account A]
emlcal service install [--timer] | uninstall
emlcal skill                                  prints SKILL.md for agents
emlcal completion zsh|bash|fish
```

`--dry-run` on every write prints exactly what would be sent (full RFC822 for
mail) and exits 0 without touching the outbox.

---

## 10. Agent integration

- `emlcal skill` emits a `SKILL.md` describing the command surface, id format,
  the JSON shapes, and guidance ("use `mail read` before replying", "prefer
  `--since` to bound results", "never `send` without `--dry-run` first unless
  told"). Install into `~/.claude/skills/emlcal/` so the agent discovers it.
- Suggested `~/.claude/settings.json` rules, mapping directly onto the
  read/write split:

```json
{
  "permissions": {
    "allow": [
      "Bash(emlcal mail list*)", "Bash(emlcal mail search*)",
      "Bash(emlcal mail read*)", "Bash(emlcal mail thread*)",
      "Bash(emlcal mail mailboxes*)", "Bash(emlcal mail attachment list*)",
      "Bash(emlcal cal agenda*)", "Bash(emlcal cal show*)",
      "Bash(emlcal cal free*)", "Bash(emlcal cal calendars*)",
      "Bash(emlcal status*)", "Bash(emlcal sync)"
    ],
    "ask": [
      "Bash(emlcal mail send*)", "Bash(emlcal mail reply*)",
      "Bash(emlcal mail trash*)", "Bash(emlcal mail move*)",
      "Bash(emlcal cal create*)", "Bash(emlcal cal update*)",
      "Bash(emlcal cal delete*)"
    ]
  }
}
```

- Output is kept token-cheap by default: `mail list` returns id, date, from,
  subject, snippet, flags, mailboxes — not bodies. `mail read` returns the
  stripped text body. `--full`/`--html` are opt-in.
- For local models later: the same `internal/store` package can back an
  embeddings table (`sqlite-vec`) for semantic search. Out of scope now, but
  the schema doesn't fight it.

---

## 11. Configuration and secrets

An account is one identity — one name, one id prefix — with a backend declared
per resource. A block's presence is what switches that resource on: an account
with no `[accounts.mail]` simply syncs no mail. This is not a cosmetic split;
a Fastmail account genuinely is JMAP mail plus CalDAV calendars, with separate
credentials and separate sync state.

```toml
# ~/.config/emlcal/config.toml
[general]
timezone       = "Europe/Amsterdam"     # default: system
default_format = "auto"                 # auto | json | table
raw_max_size   = "0"                    # global default, per-account override

[[accounts]]
name     = "work"
email    = "lennert@example.com"
poll     = "60s"
include_spam_trash = true

  [accounts.mail]
  backend = "gmail"

  [accounts.calendar]
  backend = "gcal"

[[accounts]]
name     = "personal"
email    = "lennert@fastmail.example"
push     = true
calendars = ["*"]                       # or explicit list of names

  [accounts.mail]
  backend = "jmap"

  [accounts.calendar]
  backend = "caldav"
  vendor  = "fastmail"

[[accounts]]
name     = "apple"
email    = "lennert@icloud.example"

  [accounts.mail]
  backend  = "imap"
  vendor   = "icloud"
  username = "lennert@example.com"      # Apple ID, when it is not the address

  [accounts.calendar]
  backend  = "caldav"
  vendor   = "icloud"
  username = "lennert@example.com"

[[accounts]]
name     = "home"
email    = "lennert@example.com"

  # A server with no preset: configured by host instead of vendor.
  [accounts.mail]
  backend       = "imap"
  host          = "mail.example.com"
  security      = "starttls"            # tls (default) | starttls | none
  smtp_host     = "mail.example.com"
  smtp_port     = 587
  archive_folder = "Archief"            # when the name is not recognised
```

Backends: mail is `jmap`, `gmail` or `imap`; calendar is `caldav`, `gcal` or
`jmap`. `vendor` selects the preset (§6.4, §6.5) and may be replaced by an
explicit `base_url` (CalDAV) or `host` (IMAP) for a self-hosted server. `push`
requires a backend with a stream: JMAP's EventSource, or IMAP IDLE.

Secrets are never in `config.toml`:

- Default: `~/.config/emlcal/secrets/`, mode 0600, directory 0700. Keys are
  scoped by **backend**, since one account's resources authenticate separately:
  `<name>.jmap.token`, `<name>.imap.password`, `<name>.caldav.password`,
  `<name>.google.json`, plus the shared `google-client.json` and its
  per-account override. An optional `<name>.smtp.password` overrides the IMAP
  one for a setup that splits them. On iCloud one app-specific password fills
  both `.imap.password` and `.caldav.password` — the same credential, stored
  under each protocol's key so either can be rotated alone.
- Optional: `secret_backend = "libsecret"` stores/reads via the freedesktop
  Secret Service (`secret-tool` equivalent, using `zalando/go-keyring`).
  Useful on Omarchy if a keyring is unlocked at login; otherwise the file
  backend is simpler and equally protected by full-disk encryption.
- Google client id/secret for the Desktop OAuth client are embedded in the
  binary (they are not secret for installed apps per Google's own docs) but
  can be overridden via config for people using their own GCP project.

---

## 12. Offline behaviour

| Operation | Offline |
|---|---|
| `mail list/search/read/thread`, `cal agenda/show/free` | full function, from index |
| `mail attachment get` | works if `raw_complete=1` (default policy: always) |
| `mail mark/move/archive/trash`, `cal create/update/delete` | applied to index optimistically, queued in outbox, exit 6 |
| `mail send/reply` | queued in outbox, exit 6; `status` shows pending |
| `sync` | exit 4 immediately, no error spam |

The daemon detects connectivity by provider failures, not by probing the
network, and backs off (5 s → 5 min) while offline.

---

## 13. Code layout and dependencies

```
cmd/emlcal/main.go
internal/
  cli/            cobra commands, flag parsing, output selection
  output/         json / table / plain renderers
  config/         TOML loading, validation, secret backends
  model/          Account, Mailbox, Message, Event, ids
  store/          SQLite open/migrate, typed queries (sqlc-generated), FTS helpers
  blob/           content-addressed zstd store
  mime/           parse, text extraction, quote/signature stripping, RFC822 builder
  sync/           engine: backfill, delta, reconcile, outbox, scheduler, watch
  provider/       interfaces + registry
    gmail/  gcal/  jmap/ (mail + calendar)  caldav/  imap/ (mail + smtp)  oauth/
  calendar/       recurrence expansion, free/busy, timezone helpers
  skill/          embedded SKILL.md template
```

| Concern | Choice | Why |
|---|---|---|
| Go version | 1.23+ | range-over-func iterators, stable `slices`/`maps` |
| SQLite | `modernc.org/sqlite` | pure Go → static binary, FTS5 included, trivial cross-compile for Omarchy; swap to `mattn/go-sqlite3` behind a build tag if FTS perf ever matters |
| Queries | hand-written `database/sql` | typed scan helpers, no codegen tool needed |
| Gmail / GCal | `google.golang.org/api` | first-party, batch support, retry |
| OAuth | `golang.org/x/oauth2` | token refresh, loopback flow |
| JMAP | hand-rolled (or `go-jmap`) | JSON in/out; see 6.2 |
| MIME | `emersion/go-message` | robust, widely used (aerc, hydroxide) |
| IMAP | `emersion/go-imap/v2` (beta, pinned) | the only Go client with CONDSTORE plus an in-memory server to test against; same author as go-message. Beta, so the version is pinned and no `imap.*` type escapes `internal/provider/imap`. No QRESYNC and no OBJECTID FETCH — see §6.5 |
| SMTP | `emersion/go-smtp` + `go-sasl` | `net/smtp` is frozen; needed for submission since IMAP cannot send |
| HTML→text | `k3a/html2text` | small, no DOM dependency |
| Compression | `klauspost/compress/zstd` | pure Go, fast |
| Recurrence | `teambition/rrule-go` | RFC 5545 |
| CLI | `spf13/cobra` | completions, help, ubiquitous |
| TUI | `charm.land/bubbletea/v2` (+ `bubbles/v2`, `lipgloss/v2`) | same language, same store package. The v2 module path is `charm.land/*`; it is the current stable line and pulls a smaller indirect set than v1 |

Tests: provider clients tested against recorded fixtures (`httptest` +
golden JSON); MIME tested with a corpus of nasty real-world messages; sync
engine tested with a fake provider that can inject `ErrStateExpired`,
partial pages, and crashes mid-batch.

---

## 14. Roadmap

| Phase | Deliverable | Proves |
|---|---|---|
| 1 | config, store, blob, mime, `mail list/search/read/thread`, **Fastmail mail backfill + delta** | the whole pipeline end-to-end, no OAuth needed |
| 2 | Gmail mail backfill + delta + reconcile | rate limiting, history quirks, batch API |
| 3 | writes + outbox (both providers), `--dry-run`, `mail draft/send/reply` | crash-safe writes, offline queue |
| 4 | `sync --watch`, JMAP push, systemd install, `status`, `doctor` | always-on freshness |
| 5 | calendars: GCal + JMAP Calendars, recurrence expansion, `cal *` | second resource type on the same engine |
| 6 | `skill`, `reindex`, `gc`, `export`, completions, docs | agent polish + archive guarantees |
| 7 | `tui`: unified mail list, thread, reader, agenda, event; archive/trash/mark/star with undo | the archive is usable by a person, not only an agent |
| later | composing from the TUI, Omarchy menu integrations, embeddings, contacts, MCP shim | |

Phase 1 is deliberately Fastmail-first: an API token, no consent screens, and
push support make it the fastest path to a real archive you can query.

---

## 15. Decisions to verify

Each of these is a choice I made that you might want to reverse. None are hard
to change now; several are hard to change after the first backfill.

1. **Raw RFC 822 on disk as the canonical archive**, SQLite as a derived,
   rebuildable index. Alternative: everything in SQLite (one file, simpler
   backup, but a multi-GB DB with blobs). *Hard to change after backfill.*
2. **Full attachments by default** (`raw_max_size = 0`). Matches the
   offline-archive goal; costs disk (estimate: 5–30 GB for 10+ years across
   several accounts). *Hard to change after backfill (would need re-fetch).*
3. **Content-addressed blobs, never deleted on server-side delete.** Server
   deletions mark rows, blobs stay until an explicit `gc --purge-deleted`.
4. **No Maildir.** notmuch/aerc compatibility via `export --maildir` only.
   Alternative: also write a Maildir during sync (doubles disk, adds little).
5. **Vendor APIs preferred** (Gmail API, JMAP), with IMAP+SMTP and CalDAV as
   the generic backends for everything else. A vendor API is better where it
   exists — server-side threading, a real change log — so it stays the default
   for Google and Fastmail.
6. **JMAP Calendars for Fastmail**, CalDAV as documented fallback.
7. **Gmail: polling every 60 s**, no Pub/Sub push.
8. **Secrets in 0600 files by default**, libsecret optional.
9. **Auto-JSON when stdout is not a TTY.** Convenient for agents; slightly
   surprising for shell pipelines that expect text (`| grep`). `-o plain`
   exists for that.
10. **`text_body` stored in full**, quote-stripping at display time.
11. **Go + modernc SQLite + cobra; hand-written typed queries (no sqlc — one build-time tool fewer).**
12. **Phase order: Fastmail before Gmail.**
13. **Per-copy IMAP message identity.** On IMAP a message is
    (folder, uidvalidity, uid); RFC 8474 `EMAILID` would be stable across a
    move, but go-imap/v2 cannot fetch it and only some servers advertise it, so
    the fallback would be the path actually exercised. Writes report what they
    moved (`provider.Remapper`) and the row is renamed rather than re-fetched.
    *Hard to change after backfill:* it is baked into every `remote_id`.
14. **Store-side threading for backends that supply none** (`message_refs`).
    Gmail and JMAP keep their server-side thread ids, because agreeing with
    their own UIs about what belongs together is worth more than one uniform
    algorithm. Derived data; `reindex --rethread` rebuilds it.

Answers (2026-08-25):

- **Size.** Expect 5–15 GB per mailbox. With several accounts, plan for
  50–100 GB of blobs (zstd brings that down ~25 %). Worker pools as designed
  (Gmail 4 batch requests, JMAP 8 downloads) are fine; the initial backfill
  of a 15 GB Gmail box will take a few hours and is resumable.
- **Spam and Trash are archived** (`include_spam_trash = true`). Purging is a
  policy decision later: `emlcal gc --purge-deleted` and a future
  `--purge-role junk,trash --older-than 90d`.
- **Send account.** `mail reply <id>` sends from the account that received
  the message (the id carries it). `--account` / `--from <address>` override
  it. `mail send` without `--reply` requires `--account` unless
  `general.default_account` is set.
- **Quote stripping** ships with English and Dutch patterns (German and
  French included at no extra cost, off the critical path).

---

## 16. Implementation notes (as built, 2026-08-25)

Deviations and additions relative to the sections above, recorded after the
first full build and the two adversarial reviews (`docs/reviews/`).

- **Queries** are hand-written `database/sql` code; sqlc was dropped.
- **Outbox** has a `failed_at` column (migration `0002`). A write the provider
  *rejects* (non-transport error) is rolled back in the index and marked
  permanently failed; it never retries. Sends/drafts queue only on a
  demonstrably pre-request failure (dial/DNS/connection refused →
  `provider.IsPreRequestFailure`); an ambiguous failure (timeout after the
  request went out, 5xx) is permanent so a message can never be sent twice.
  `mail send` therefore exits 6 (queued) or 4 (not sent, run again).
- **`send --draft`** sends the draft's raw bytes and then trashes the draft
  (providers return the draft's *message* id from `CreateDraft`).
- **JMAP enumeration** uses `anchor`/`anchorOffset` paging (cursor is a JSON
  `{"anchor","n"}`) so deletions during a backfill cannot skip messages;
  `recurrenceOverrides` on JMAP events become exception rows; participant ids
  are base64url(sha256(email)); the calendars capability falls back to any
  `*:calendars` URN the session advertises.
- **Gmail** never retries `messages.send`/`drafts.create`; batch endpoint is
  `https://gmail.googleapis.com/batch/gmail/v1`. **GCal** writes pass
  `sendUpdates=all` when the event has other attendees.
- **`raw_max_size`**: oversized messages are stored as envelope-only stubs
  (`raw_complete = 0`, subject "(not fetched: N MB)") and completed on demand
  by `mail read --raw/--html` or `mail attachment get`.
- **Store guards**: an empty mailbox list from a provider is refused rather
  than wiping memberships; resurrected messages are re-indexed in full.
- **Secrets**: Fastmail token in `secrets/<name>.fastmail.token`; Google
  token in `secrets/<name>.gmail.json`; the Google OAuth client in
  `secrets/google-client.json` (`emlcal account google-client`) or env
  `EMLCAL_GOOGLE_CLIENT_ID`/`_SECRET`. `EMLCAL_JMAP_SESSION_URL` overrides the
  Fastmail session URL (tests, self-hosted JMAP).
- **Flags**: `mail attachment get … -O path` (`-o` is the global format flag).
- **Testing**: `internal/provider/fake` (in-memory providers) backs the sync
  and CLI unit tests; `internal/testutil/jmapfake` + `e2e/` drive the real
  binary through a fake JMAP server, including push via `sync --watch`.
  `EMLCAL_REVIEW=1 go test -run TestReview ./internal/...` runs the review
  probes that are still open.

- **Backends are per resource** (§11), replacing the single `provider` field.
  It conflated *who runs the service* with *what protocol reaches it*, which
  held only while JMAP was expected to serve Fastmail's calendars too. It does
  not, so `model.Provider` split into `Vendor` and `Backend`, and the choice
  that used to be inferred at runtime from which secrets existed is now stated
  in config. Consequences worth knowing:
  - `config.Account`'s zero value used to mean "both halves on" and now syncs
    nothing; build accounts with `config.NewAccount`.
  - The `accounts.provider` column keeps its name and holds the vendor.
    Nothing branches on it — it is informational, and an account may mix
    vendors across its resources — so there was no migration.
  - Secret keys moved to `<name>.jmap.token`, `<name>.caldav.password` and
    `<name>.google.json`. There is no compatibility shim, because nothing was
    in production. Migrating an existing install by hand:

    ```bash
    # 1. rewrite each [[accounts]] entry in config.toml to the form in §11
    # 2. rename its secrets
    cd ~/.config/emlcal/secrets
    mv <name>.fastmail.token        <name>.jmap.token
    mv <name>.fastmail.app-password <name>.caldav.password
    mv <name>.gmail.json            <name>.google.json
    # 3. check, then sync
    emlcal doctor && emlcal sync
    ```

    The index is derived, so `emlcal sync --full` rebuilds it if anything
    looks wrong.
- **iCloud calendars** (§6.4) needed no new client, only the vendor preset and
  the two things iCloud does that Fastmail does not: serve the calendar home
  from a per-user partition host, and authenticate an Apple ID that is often
  not the account's address. `emlcal account add icloud` writes a
  calendar-only account.
- **Redirects are refused** by the CalDAV client (§6.4.1). Go downgrades a
  redirected PROPFIND/REPORT/PUT to a GET, so following one is silently
  wrong; `caldavfake` grew a `RedirectTo` knob so the whole class stays
  covered.
- **`doctor` checks the CalDAV password**, which nothing did before — a
  Fastmail account without one silently synced no calendars at all. It warns
  rather than fails, since the mail half still works.
- **Backfill is newest-first** for both providers; a JMAP cursor written by
  the older ascending run keeps its order until that backfill finishes.
- **`sync --wait-offline` (default 10 m)** rides out network drops in a
  one-shot sync; `--quiet` hides the progress line; progress carries the
  total (JMAP `Total()`, Gmail `users.getProfile` `messagesTotal`), rate and ETA.
- **Per-account `mail = false` / `calendar = false`** toggles in config.
- **Per-account Google OAuth client** in `secrets/<name>.google-client.json`
  (`account add gmail --client-id/--client-secret`).

- **TUI (`internal/tui`, `emlcal tui`)** is a second surface over the same
  store and the same engine, not a second client. Decisions worth knowing:
  - It does **not** run its own `Engine.Watch`: the daemon holds the
    per-account flock, so an in-process engine would only collect `ErrLocked`.
    Freshness comes from polling `PRAGMA data_version` every 2 s on one pinned
    `*sql.Conn` — the counter SQLite bumps when another *connection* commits.
    Pinning matters: the pragma is per-connection, so asking through the pool
    compares unrelated counters and reports a change every other poll. `R`
    additionally sends `SIGUSR1` to the pid file, the same nudge `sync` uses.
  - **Undo is a second ordinary `Apply` of the inverse op.** The engine has no
    undo primitive — `rollback` runs only inside `Apply`, when a provider
    *rejects* a write. Archive inverts to `OpMailboxes{Add: inbox, Remove:
    archive}`; trash needs the message's mailbox set captured *before* the
    write, because `mailboxPatch` sets `clearOthers` and drops the rest.
  - It consumes `ApplyResult.Renames`. Gmail and JMAP always return it empty,
    which is precisely why forgetting it would go unnoticed until IMAP.
  - `output.Truncate` counts runes; the TUI needs cells, so it has its own
    `truncCells`/`padCells` over `mattn/go-runewidth` (which arrives with
    bubbletea). The CLI tables keep the rune-counting behaviour for now.
  - `mailFlagsString`, `mailShortAddr`, `calTimeCell`, `calRSVPCell` and
    `calFormatDuration` moved from `package cli` to `internal/output` as
    `MailFlags`, `ShortAddr`, `TimeCell`, `RSVP` and `Duration`, so the two
    surfaces cannot disagree about what a flag letter means. `mime.htmlToText`
    became `mime.HTMLToText` for the same reason.
  - Composing (reply/forward) is deliberately absent; `r` is reserved.

### Still to verify against live accounts

See `docs/reviews/2026-08-25-providers.md` (checklist at the end). Highest
risk: Fastmail's actual JMAP calendars capability URN and `anchor` paging
support; Gmail threading of replies; GCal RSVP delivery.
