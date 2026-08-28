# emlcal

A complete, offline-first local archive of your mail and calendars — Gmail,
Fastmail, iCloud and any IMAP server, as many accounts as you have — behind one
CLI that is built for both humans and AI agents.

```
$ emlcal mail list --unread --since 2d
ID                    DATE   FROM              SUBJECT                                   FLAGS  ACCOUNT
fm:MTQ3OTAxOQ         12m    Anna de Vries     Re: offerte Q4                            U      fastmail
gm:18f3a2b9c1d4e5f6   3h     GitHub            [teulaert/emlcalsync] CI passed           U      teulaert

$ emlcal mail read fm:MTQ3OTAxOQ | head
$ emlcal cal agenda --days 7
$ emlcal mail reply fm:MTQ3OTAxOQ --body "Akkoord, ik stuur het vanmiddag." --dry-run
```

Every message of every account is stored on disk as raw RFC 822 (zstd,
content-addressed), indexed in SQLite with full-text search, and kept in sync
with cheap incremental deltas (JMAP push for Fastmail, history polling for
Gmail, IDLE and uid-set diffing for IMAP). All read commands work without a
network. The SQLite index is derived data: `emlcal reindex` rebuilds it from
the archive.

The full design is in [DESIGN.md](DESIGN.md).

## Why

Terminal-centric setups (this was built for [Omarchy](https://omarchy.org))
have no good way to give an agent access to mail and calendar. Hosted
integrations are slow, online-only and see one account at a time. emlcal keeps
the whole history locally so an agent — or a local model — can search ten
years of mail in milliseconds, and exposes it as plain commands with JSON
output, so tools like Claude Code can use it with nothing but a shell.

## Install

Requires Go 1.25+.

```bash
git clone https://github.com/teulaert/emlcalsync
cd emlcalsync
make                 # builds ./emlcal — pure Go, no CGO, cross-compiles freely
make install         # copies it to ~/.local/bin
```

`make install` honours `PREFIX` (default `~/.local`), `BINDIR` and `DESTDIR`,
so `sudo make install PREFIX=/usr/local` works too. `make install-completions`
adds bash, zsh and fish completions; `make check` runs what CI runs (gofmt,
vet, race tests).

## Set up accounts

### Fastmail

Create an API token at https://app.fastmail.com/settings/security/tokens with
the **Mail** and **Submission** scopes. Fastmail tokens have no calendar
scope, so calendars use CalDAV with an **app password** (Settings → Privacy &
Security → Integrations → New app password, access "Calendars (CalDAV)").

```bash
emlcal account add fastmail --name fm --email you@fastmail.com   # prompts for the token
emlcal account caldav-password --name fm                         # prompts for the app password (calendars)
```

### iCloud

iCloud accounts sync **mail and calendars** — mail over IMAP and SMTP,
calendars over CalDAV.

Both halves authenticate with the same **app-specific password**, not your
Apple ID password. Create one at <https://account.apple.com/account/manage> under
Sign-In and Security → App-Specific Passwords; two-factor authentication has
to be on for that option to appear. `account add icloud` stores it once and
uses it for both.

```bash
emlcal account add icloud --name ic --email you@icloud.com       # prompts for the password
```

If your Apple ID is not the same as your iCloud mail address, pass it as
`--username`: that is what authenticates, while `--email` stays the address
that identifies you on invitations.

```bash
emlcal account add icloud --name ic --email you@icloud.com --username you@example.com
```

### Any other IMAP server

Self-hosted Dovecot, Migadu, mailbox.org and the like are configured by host
rather than by vendor. If the domain publishes RFC 6186 SRV records, `--host`
can be left out and the servers are looked up from the address.

```bash
emlcal account add imap --name home --email you@example.com \
  --host mail.example.com --smtp-host mail.example.com
```

These are supported by configuration rather than by preset: emlcal has not been
run against them, so `emlcal doctor` prints what the server actually advertises,
and the capability table in DESIGN.md §6.5 says what each missing extension
costs. Where roles cannot be worked out from the folder names, set them
explicitly:

```toml
  [accounts.mail]
  archive_folder = "Archief"
  sent_folder    = "Verzonden items"
```

### Gmail / Google Calendar

Google needs an OAuth client of your own, once, shared by all your Google
accounts:

1. In [Google Cloud Console](https://console.cloud.google.com) create a
   project, enable the **Gmail API** and **Google Calendar API**.
2. Google Auth Platform → Branding: app name, support email, developer
   contact; App domain and Authorized domains can point at this repo.
   Audience: **External**, then **Publish app** (Testing mode expires refresh
   tokens after 7 days). Never submit for verification; you'll click through
   the "unverified app" screen once per account.
3. Clients → Create → **Desktop app**. Copy the id and secret.

```bash
emlcal account google-client --id '…apps.googleusercontent.com' --secret 'GOCSPX-…'
emlcal account add gmail --name gm --email you@gmail.com        # opens the consent screen
```

### First sync

```bash
emlcal sync                      # full backfill of every account; resumable
emlcal status                    # counts, backfill progress, last sync
emlcal sync                      # afterwards: incremental delta in seconds
```

Backfill runs newest-first, so recent mail is searchable within minutes, at
roughly 50 msg/s on Fastmail and ~16 msg/s on Gmail (Google's quota). It
shows rate and ETA, rides out network drops (`--wait-offline`, default 10 m)
and can be interrupted any time; the next run continues from the cursor.
Per account you can switch a resource off in the config (`mail = false` or
`calendar = false`) — e.g. a work account synced for its calendar only. Attachments are archived in full by default (`raw_max_size` in
the config caps that).

### Keep it synced

```bash
emlcal service install           # systemd user service running `emlcal sync --watch`
emlcal service install --timer   # or a 2-minute timer instead of a daemon
```

## Commands

Read commands never need the network and are safe to allowlist for an agent;
write commands go to the provider (and are queued in an outbox when offline).

```
account   add gmail|fastmail|icloud|imap · list · remove
          google-client · caldav-password · imap-password
sync      [--account] [--full] [--watch] [--mail-only|--calendar-only]
status · doctor · outbox · reindex · gc · export (--mbox | --maildir) · service · skill
tui       interactive mail + calendar, merged across accounts

mail      mailboxes · list · search · read · thread · attachment list|get      (read)
          mark · move · archive · trash · draft · send · reply                 (write)
cal       calendars · agenda · show · free                                    (read)
          create · update · delete · respond                                  (write)
```

`emlcal <command> --help` documents every flag. Useful defaults:

- Output is a table on a TTY and **JSON when piped**; `-o json|table|plain`.
- Ids are stable and opaque: `fm:MTQ3` (message), `fm:t:…` (thread),
  `gm:c:<calendar>:<event>` (event). Every list prints them; every command
  accepts them. On IMAP a message is identified by its folder and uid, so
  moving one (archive, trash) renumbers it — the write reports the new id as
  `new_id`, and the old one stops resolving.
- `--since 2d`, `--until`, `--account` (repeatable) on every list;
  `--dry-run` on every write.
- Exit codes: 0 ok · 1 error · 2 usage · 3 not found · 4 offline ·
  5 provider rejected · 6 queued in outbox.

## Using it from an agent

```bash
emlcal skill --install           # writes ~/.claude/skills/emlcal/SKILL.md
```

The skill describes the commands, id format and JSON shapes. It also prints
a `permissions` block for `~/.claude/settings.json` that allowlists the read
commands and gates the writes:

```json
"allow": ["Bash(emlcal mail list*)", "Bash(emlcal mail search*)", "Bash(emlcal mail read*)", "Bash(emlcal cal agenda*)", "..."],
"ask":   ["Bash(emlcal mail send*)", "Bash(emlcal mail trash*)", "Bash(emlcal cal create*)", "..."]
```

## Where things live

```
~/.config/emlcal/config.toml          accounts and policies (no secrets)
~/.config/emlcal/secrets/             API tokens, OAuth tokens, Google client (0600)
~/.local/share/emlcal/emlcal.db       SQLite index (rebuildable)
~/.local/share/emlcal/blobs/          the archive: raw .eml.zst, sha256-addressed
~/.local/state/emlcal/                log, pid, locks
```

Backing up or moving to another machine = copying the first four. Sync state
travels with the database, so the other machine continues with a delta.

## Development

```bash
go test ./...                    # unit tests, fake providers
go test -race ./...
go test ./e2e/                   # drives the real binary against a fake JMAP server
EMLCAL_REVIEW=1 go test -run TestReview ./internal/...   # review probes
```

Layout: `internal/store` (SQLite), `internal/blob`, `internal/mime`,
`internal/sync` (backfill/delta/outbox/watch), `internal/provider/{jmap,gmail,gcal,caldav,imap,oauth}`,
`internal/cli`. Review notes from the first build are in `docs/reviews/`.

## Status

v0.1 — mail, Google Calendar and Fastmail calendars (CalDAV) work end-to-end
against real accounts. iCloud calendars are implemented and covered by tests
against a fake server, but have not yet been run against a real account.
`emlcal tui` browses and triages mail and calendar interactively, merged
across every account; the two screens are tabs in the header, `1` for mail and
`2` for the calendar (`tab` toggles). Threads open on the message text, newest
first, with `t` for the one-row-per-message index; `M` cycles the mailbox
(inbox, all, flagged, drafts, sent, archive, trash, spam), so unsent drafts are
a view of their own and are marked `D` wherever they turn up in a thread;
`enter` on an
event opens it, where `y` / `n` / `t` answer the invitation. Not yet: composing
from the TUI, creating or editing events from the TUI, embeddings for semantic
search, contacts.
