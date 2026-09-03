-- The address book, derived from the archive.
--
-- There is no contacts sync. The people worth mailing are the ones already on
-- the messages here, so the book is a fact table -- one row per address per
-- message, replaced whenever the message is indexed again -- and a summary
-- per (account, address) that the composer and `contacts` read. Replacing
-- per message is what keeps a re-index from counting anything twice: the
-- same shape as message_mailboxes and attachments.
--
-- outbound marks the messages the account wrote (a draft, one in the sent
-- mailbox, or one with its own address on From), which is what ranks a
-- person you write to above one who writes to you; the From row never is,
-- since on such a message From is the account itself. The predicate is the one
-- in store/contacts.go; the backfill below is a copy of it, run once over
-- what the archive already holds. Reply-To is left out on purpose: that is
-- where lists and ticket systems live.
--
-- The backfill walks every message inside Open's migration window. Should it
-- ever be cut short, `emlcal reindex` puts every message back through the
-- same statements.

CREATE TABLE message_addresses (
  message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  account_id TEXT    NOT NULL,
  email      TEXT    NOT NULL,             -- lower-cased, trimmed
  name       TEXT,                         -- as written on this message
  field      TEXT    NOT NULL,             -- 'from' | 'to' | 'cc' | 'bcc'
  outbound   INTEGER NOT NULL DEFAULT 0,   -- the account wrote this message
  date_utc   INTEGER NOT NULL,
  PRIMARY KEY (message_id, email, field)
);
CREATE INDEX message_addresses_email ON message_addresses(account_id, email);

CREATE TABLE contacts (
  account_id  TEXT NOT NULL,
  email       TEXT NOT NULL,
  name        TEXT,
  sent_count  INTEGER NOT NULL,            -- messages the account wrote to them
  total_count INTEGER NOT NULL,            -- messages they are on at all
  last_utc    INTEGER,
  PRIMARY KEY (account_id, email)
);

-- Backfill: the four statements of store/contacts.go over every message.

INSERT OR IGNORE INTO message_addresses (message_id, account_id, email, name, field, outbound, date_utc)
SELECT m.id, m.account_id, lower(trim(m.from_addr)), nullif(trim(m.from_name), ''), 'from', 0, m.date_utc
  FROM messages m
 WHERE coalesce(trim(m.from_addr), '') <> '';

INSERT OR IGNORE INTO message_addresses (message_id, account_id, email, name, field, outbound, date_utc)
SELECT m.id, m.account_id, lower(trim(json_extract(j.value, '$.email'))),
       nullif(trim(json_extract(j.value, '$.name')), ''), 'to',
       CASE WHEN (m.is_draft = 1
             OR lower(m.from_addr) = (SELECT lower(a.email) FROM accounts a WHERE a.id = m.account_id)
             OR EXISTS (SELECT 1 FROM message_mailboxes mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
                         WHERE mm.message_id = m.id AND lower(mb.role) = 'sent')) THEN 1 ELSE 0 END,
       m.date_utc
  FROM messages m, json_each(m.to_json) j
 WHERE coalesce(trim(json_extract(j.value, '$.email')), '') <> '';

INSERT OR IGNORE INTO message_addresses (message_id, account_id, email, name, field, outbound, date_utc)
SELECT m.id, m.account_id, lower(trim(json_extract(j.value, '$.email'))),
       nullif(trim(json_extract(j.value, '$.name')), ''), 'cc',
       CASE WHEN (m.is_draft = 1
             OR lower(m.from_addr) = (SELECT lower(a.email) FROM accounts a WHERE a.id = m.account_id)
             OR EXISTS (SELECT 1 FROM message_mailboxes mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
                         WHERE mm.message_id = m.id AND lower(mb.role) = 'sent')) THEN 1 ELSE 0 END,
       m.date_utc
  FROM messages m, json_each(m.cc_json) j
 WHERE coalesce(trim(json_extract(j.value, '$.email')), '') <> '';

INSERT OR IGNORE INTO message_addresses (message_id, account_id, email, name, field, outbound, date_utc)
SELECT m.id, m.account_id, lower(trim(json_extract(j.value, '$.email'))),
       nullif(trim(json_extract(j.value, '$.name')), ''), 'bcc',
       CASE WHEN (m.is_draft = 1
             OR lower(m.from_addr) = (SELECT lower(a.email) FROM accounts a WHERE a.id = m.account_id)
             OR EXISTS (SELECT 1 FROM message_mailboxes mm JOIN mailboxes mb ON mb.id = mm.mailbox_id
                         WHERE mm.message_id = m.id AND lower(mb.role) = 'sent')) THEN 1 ELSE 0 END,
       m.date_utc
  FROM messages m, json_each(m.bcc_json) j
 WHERE coalesce(trim(json_extract(j.value, '$.email')), '') <> '';

INSERT INTO contacts (account_id, email, name, sent_count, total_count, last_utc)
SELECT a.account_id, a.email,
       (SELECT n.name FROM message_addresses n
         WHERE n.account_id = a.account_id AND n.email = a.email AND n.name IS NOT NULL
         ORDER BY (n.field = 'from') DESC, n.date_utc DESC LIMIT 1),
       count(DISTINCT CASE WHEN a.outbound = 1 THEN a.message_id END),
       count(DISTINCT a.message_id),
       max(a.date_utc)
  FROM message_addresses a
 GROUP BY a.account_id, a.email;
