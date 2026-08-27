-- Message-ID graph, for backends that supply no thread id of their own.
--
-- Gmail and JMAP hand us a server-computed thread id and never reach this
-- table. IMAP has none: RFC 5256 THREAD is per-folder and recomputed on every
-- call, so it cannot be a persisted identity. Every real IMAP client therefore
-- threads client-side over Message-ID / In-Reply-To / References, and so do we.
--
-- One row per Message-ID a message names, including its own. Indexed by ref so
-- the lookup goes both ways: a reply is frequently indexed before the message
-- it answers (different folders, different backfill order), so matching only
-- "my parent is already here" would strand it in its own thread forever.
--
-- Derived data. `emlcal reindex --rethread` rebuilds it from the blob archive.
CREATE TABLE message_refs (
  account_id TEXT    NOT NULL,
  message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  ref        TEXT    NOT NULL,
  PRIMARY KEY (message_id, ref)
) WITHOUT ROWID;

CREATE INDEX message_refs_ref ON message_refs(account_id, ref);
