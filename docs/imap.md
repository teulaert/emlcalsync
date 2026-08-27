# IMAP and SMTP

emlcal reads mail over IMAP and sends over SMTP for accounts with no vendor API
worth using. The client is vendor-neutral: what a particular server needs that
the protocol does not say lives in a preset, and a server with no preset is
configured by host.

Where a vendor API exists it stays the default — Gmail via the Gmail API,
Fastmail via JMAP — because both give server-side threading and a real change
log, which IMAP does not.

## Configuring a server

```toml
[[accounts]]
name  = "home"
email = "you@example.com"

  [accounts.mail]
  backend   = "imap"
  host      = "mail.example.com"
  security  = "starttls"          # tls (default) | starttls | none
  smtp_host = "mail.example.com"
  smtp_port = 587
```

`port` defaults to 993 for `tls` and 143 for `starttls`; `smtp_port` to 465 and
587. `username` and `smtp_username` default to the account's address. `security
= "none"` is refused unless the process is explicitly told to allow it, since it
would put the password on the wire in the clear.

Store the password with:

```bash
emlcal account imap-password --name home --stdin
```

It is used for SMTP too. A setup that insists on separate credentials can store
a second one under `<name>.smtp.password`.

## Curated vendors

| Vendor | IMAP | Submission | Credential |
|---|---|---|---|
| `icloud` | `imap.mail.me.com:993` | `smtp.mail.me.com:587` | app-specific password |
| `fastmail` | `imap.fastmail.com:993` | `smtp.fastmail.com:465` | app password |

Fastmail's preset exists mainly so there is a second standards-complete server
to verify against; JMAP remains the recommended Fastmail backend.

**iCloud's preset is written from documentation, not observation.** Its exact
capabilities, whether its submission files its own Sent copy, and its real
connection limit have not been confirmed against a live account. Run
`emlcal doctor` first — it prints what the server actually advertises.

## What each capability buys

Everything below is optional. Run `emlcal doctor` to see which of them your
server offers.

| Capability | Present | Absent |
|---|---|---|
| `CONDSTORE` | flag changes are exact and cheap | read/unread is still immediate (via `STATUS UNSEEN`); other flags wait for a periodic sweep, roughly hourly |
| `MOVE` | one command | `COPY` + `\Deleted`, then `UID EXPUNGE` if `UIDPLUS` |
| `UIDPLUS` | a move reports the message's new id, and the row is renamed | the move is discovered as a delete plus an add, so the body is fetched again. With neither `MOVE` nor `UIDPLUS` the source copy is flagged `\Deleted` but **not** expunged — a bare `EXPUNGE` would destroy every `\Deleted` message in the folder, including ones another client flagged |
| `SPECIAL-USE` | folder roles come from the server | roles fall back to names, then to explicit config |
| `LIST-STATUS` | one round trip for the whole account | one `STATUS` per folder |
| `IDLE` | new mail arrives in seconds | mail arrives on the poll interval |
| `QRESYNC` | *not used* — the Go client cannot parse `VANISHED` | the sync state tracks uid sets itself, so nothing is lost but a round trip |
| `OBJECTID` | *not used* — the Go client cannot fetch `EMAILID` | ids are per-copy; see below |

## Things worth knowing

**Message ids change when a message moves.** A message on IMAP is
`(folder, uidvalidity, uid)`, so archiving or trashing one gives it a new id.
Where the server supports `UIDPLUS` this is handled: the write reports the new
id and the index row is renamed, keeping its body, thread and flags. An
`<account>:<id>` recorded before a move still needs looking up again.

**The same mail in two folders is two rows.** That is what IMAP says it is. The
bodies are stored once (blobs are content-addressed), so it costs index rows,
not disk. `\All`-style everything-folders are excluded by default for this
reason; `include_all_mail = true` overrides it.

**Threads are stitched locally**, from `Message-ID`, `In-Reply-To` and
`References`, because IMAP has no thread id to give. It handles a reply that
arrives before its parent, and merges two threads when a later message proves
they were one. A correspondent whose client truncates `References` can still
split a conversation. `emlcal reindex --rethread` recomputes it.

**Folders can be filtered.** `folders` restricts the sync to a list;
`exclude_folders` removes from it. `include_spam_trash = false` drops Junk and
Trash.

**Roles can be named explicitly** where a server's folders are not recognised:

```toml
  [accounts.mail]
  archive_folder = "Archief"
  sent_folder    = "Verzonden items"
  trash_folder   = "Prullenbak"
  drafts_folder  = "Concepten"
```

Without a resolvable Archive folder, `emlcal mail archive` fails with a message
saying so rather than quietly doing nothing.

## Sending

Submission happens first, then a copy is appended to Sent — unless the preset
says the server files its own, in which case appending would duplicate every
sent message. If the append fails the send still succeeds: the message is
already gone, and the next sync picks the copy up anyway.

Bcc recipients reach `RCPT TO` but never the message headers. This is why the
provider is handed the envelope separately from the bytes.

A submission that fails before the message is handed over is queued in the
outbox and retried. One that fails during or after `DATA` is not: the server may
have accepted it, and retrying would deliver it twice. `emlcal outbox list`
shows anything left in that state.
