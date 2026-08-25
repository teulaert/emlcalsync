-- emlcal initial schema. See DESIGN.md §5.
--
-- All times are unix seconds (INTEGER). Structured values (address lists,
-- References headers, provider objects) are JSON TEXT.

CREATE TABLE accounts (
  id            TEXT PRIMARY KEY,
  provider      TEXT NOT NULL,             -- 'gmail' | 'fastmail'
  email         TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);

CREATE TABLE mailboxes (
  id            INTEGER PRIMARY KEY,
  account_id    TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_id     TEXT NOT NULL,             -- Gmail labelId / JMAP mailbox id
  name          TEXT NOT NULL,
  role          TEXT,                      -- inbox|archive|sent|drafts|trash|junk|
                                           -- important|all|category:*|NULL (user label)
  parent_id     INTEGER REFERENCES mailboxes(id) ON DELETE SET NULL,
  sort_order    INTEGER,
  total_count   INTEGER,
  unread_count  INTEGER,
  UNIQUE (account_id, remote_id)
);
CREATE INDEX mailboxes_role ON mailboxes(account_id, role);

CREATE TABLE messages (
  id             INTEGER PRIMARY KEY,
  account_id     TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_id      TEXT NOT NULL,            -- Gmail message id / JMAP Email id
  thread_id      TEXT NOT NULL,
  blob_sha256    TEXT,                     -- NULL only when raw_complete = 0
  raw_complete   INTEGER NOT NULL DEFAULT 1,
  message_id_hdr TEXT,
  in_reply_to    TEXT,
  references_json TEXT,                    -- JSON array of strings
  subject        TEXT,
  from_addr      TEXT, from_name TEXT,
  to_json        TEXT, cc_json TEXT, bcc_json TEXT, reply_to_json TEXT,
  date_utc       INTEGER NOT NULL,
  received_utc   INTEGER NOT NULL,
  size           INTEGER,
  snippet        TEXT,
  text_body      TEXT,
  attachment_names TEXT,                   -- space-joined filenames, for FTS
  list_id        TEXT,
  is_bulk        INTEGER NOT NULL DEFAULT 0,  -- List-Id / Auto-Submitted / Precedence
  has_attachments INTEGER NOT NULL DEFAULT 0,
  is_unread      INTEGER NOT NULL DEFAULT 0,
  is_flagged     INTEGER NOT NULL DEFAULT 0,
  is_draft       INTEGER NOT NULL DEFAULT 0,
  is_answered    INTEGER NOT NULL DEFAULT 0,
  deleted_at     INTEGER,
  indexed_at     INTEGER NOT NULL,
  UNIQUE (account_id, remote_id)
);
CREATE INDEX messages_thread   ON messages(account_id, thread_id);
CREATE INDEX messages_received ON messages(received_utc DESC);
CREATE INDEX messages_account  ON messages(account_id, received_utc DESC);
CREATE INDEX messages_msgid    ON messages(message_id_hdr);
CREATE INDEX messages_blob     ON messages(blob_sha256);

CREATE TABLE message_mailboxes (
  message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  PRIMARY KEY (message_id, mailbox_id)
);
CREATE INDEX mm_mailbox ON message_mailboxes(mailbox_id);

CREATE TABLE attachments (
  id           INTEGER PRIMARY KEY,
  message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  part_path    TEXT NOT NULL,
  filename     TEXT,
  content_type TEXT,
  size         INTEGER,
  content_id   TEXT,
  is_inline    INTEGER NOT NULL DEFAULT 0,
  remote_ref   TEXT
);
CREATE INDEX attachments_message ON attachments(message_id);

CREATE TABLE threads (
  account_id    TEXT NOT NULL,
  thread_id     TEXT NOT NULL,
  subject       TEXT,
  first_utc     INTEGER, last_utc INTEGER,
  message_count INTEGER, unread_count INTEGER,
  participants_json TEXT,
  PRIMARY KEY (account_id, thread_id)
);
CREATE INDEX threads_last ON threads(last_utc DESC);

-- Full-text search: external-content FTS5 so text is stored once.
CREATE VIRTUAL TABLE messages_fts USING fts5(
  subject, from_addr, from_name, to_json, text_body, attachment_names,
  content='messages', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2',
  prefix='2 3'
);

CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, subject, from_addr, from_name, to_json, text_body, attachment_names)
  VALUES (new.id, new.subject, new.from_addr, new.from_name, new.to_json, new.text_body, new.attachment_names);
END;

CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, from_name, to_json, text_body, attachment_names)
  VALUES ('delete', old.id, old.subject, old.from_addr, old.from_name, old.to_json, old.text_body, old.attachment_names);
END;

-- Scoped to the indexed columns so flag-only updates (the common delta case)
-- do not churn the index.
CREATE TRIGGER messages_au
AFTER UPDATE OF subject, from_addr, from_name, to_json, text_body, attachment_names ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, subject, from_addr, from_name, to_json, text_body, attachment_names)
  VALUES ('delete', old.id, old.subject, old.from_addr, old.from_name, old.to_json, old.text_body, old.attachment_names);
  INSERT INTO messages_fts(rowid, subject, from_addr, from_name, to_json, text_body, attachment_names)
  VALUES (new.id, new.subject, new.from_addr, new.from_name, new.to_json, new.text_body, new.attachment_names);
END;

CREATE TABLE sync_state (
  account_id TEXT NOT NULL,
  resource   TEXT NOT NULL,                -- 'mail' | 'mailboxes' | 'cal:<remote_id>'
  state      TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (account_id, resource)
);

CREATE TABLE backfill_progress (
  account_id     TEXT NOT NULL,
  resource       TEXT NOT NULL,
  cursor         TEXT,
  state_at_start TEXT NOT NULL,
  total_hint     INTEGER, done INTEGER,
  finished_at    INTEGER,
  PRIMARY KEY (account_id, resource)
);

CREATE TABLE outbox (
  id          INTEGER PRIMARY KEY,
  account_id  TEXT NOT NULL,
  kind        TEXT NOT NULL,               -- send|draft|flags|mailboxes|trash|event.*
  payload     TEXT NOT NULL,               -- JSON
  created_at  INTEGER NOT NULL,
  attempts    INTEGER NOT NULL DEFAULT 0,
  last_error  TEXT,
  done_at     INTEGER
);
CREATE INDEX outbox_pending ON outbox(done_at, id);

CREATE TABLE sync_log (
  id INTEGER PRIMARY KEY, account_id TEXT, kind TEXT,
  started_at INTEGER, finished_at INTEGER,
  added INTEGER, updated INTEGER, removed INTEGER, error TEXT
);
CREATE INDEX sync_log_account ON sync_log(account_id, id DESC);

-- Calendar ---------------------------------------------------------------

CREATE TABLE calendars (
  id          INTEGER PRIMARY KEY,
  account_id  TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  remote_id   TEXT NOT NULL,
  name        TEXT NOT NULL,
  color       TEXT, timezone TEXT,
  is_primary  INTEGER NOT NULL DEFAULT 0,
  access_role TEXT,
  UNIQUE (account_id, remote_id)
);

CREATE TABLE events (
  id            INTEGER PRIMARY KEY,
  calendar_id   INTEGER NOT NULL REFERENCES calendars(id) ON DELETE CASCADE,
  remote_id     TEXT NOT NULL,
  uid           TEXT,
  title         TEXT, description TEXT, location TEXT,
  start_utc     INTEGER, end_utc INTEGER,
  all_day       INTEGER NOT NULL DEFAULT 0,
  timezone      TEXT,
  rrule         TEXT,
  recurrence_id TEXT,
  status        TEXT,
  organizer     TEXT, attendees_json TEXT,
  my_response   TEXT,
  raw_json      TEXT NOT NULL,
  updated_utc   INTEGER, deleted_at INTEGER,
  UNIQUE (calendar_id, remote_id)
);
CREATE INDEX events_uid   ON events(uid);
CREATE INDEX events_start ON events(start_utc);
CREATE INDEX events_rrule ON events(calendar_id, rrule);

CREATE TABLE event_occurrences (
  event_id  INTEGER NOT NULL REFERENCES events(id) ON DELETE CASCADE,
  start_utc INTEGER NOT NULL, end_utc INTEGER NOT NULL,
  PRIMARY KEY (event_id, start_utc)
);
CREATE INDEX occ_start ON event_occurrences(start_utc);
