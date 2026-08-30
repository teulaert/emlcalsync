---
name: emlcal
description: "Read, search and act on the local mail & calendar archive via the emlcal CLI: list/search/read mail, agenda and free slots, draft/send mail, create events."
---

# emlcal

`emlcal` is a complete local archive of the user's mail and calendar accounts
(Gmail, Fastmail, iCloud and any IMAP server), with a CLI built for you. Reads hit a local SQLite index
and never touch the network; writes go to the provider and are queued when
offline.

## When to use this skill

- The user asks what is in their mail: "did X reply", "find the invoice from Y",
  "what did we agree about Z", "anything unread from my boss".
- The user asks about their day or availability: "what's on tomorrow",
  "when am I free Thursday afternoon".
- The user wants to act: reply to a message, send mail, file or archive
  something, create or move a meeting, accept an invite.

Do not use it for mail on a server that is not one of the configured accounts —
run `emlcal account list` to see which accounts exist.

## Ids

Every list prints an `id`; every command that takes an id accepts what a list
printed. They are stable and opaque:

- message — `<account>:<remote>` (e.g. `work:18f3a2b9c1d4e5f6`)
- thread — `<account>:t:<thread>`
- event — `<account>:c:<calendar>:<event>`

The account is part of the id, so `mail reply` knows which account to send from.

## Output

Output is JSON whenever stdout is not a TTY, which is always the case for you —
so **always pipe or capture the output** (`emlcal mail list | head`) and parse
it as JSON. Add `-o json` if you want to be explicit, `--pretty` only when a
human will read it. Errors are `{"error":{"code":"...","message":"..."}}` on
stderr. Timestamps are RFC 3339 with offset.

`--limit` defaults to 50. `--since`/`--until` take `2d`, `12h` or `2026-08-01`.
`--account NAME` is repeatable and defaults to every account.

## Read commands (safe, no confirmation needed)

```
emlcal mail mailboxes [--account A]              # mailbox/label names and roles
emlcal mail list [--mailbox inbox] [--unread] [--flagged] [--from X] [--to X] \
                 [--since 2d] [--no-bulk] [--thread] [--limit N]
emlcal mail search "<fts query>" [same filters]  # FTS5: AND OR NOT "phrase" col:term
emlcal mail read <id> [--full] [--html] [--raw] [--headers]
emlcal mail thread <id>                          # whole conversation, oldest first
emlcal mail attachment list <id>
emlcal mail attachment get <id> <part|filename> [-O path]   # -O - writes to stdout
emlcal mail attachment text <id> <part|filename>          # a PDF / HTML / text attachment as text
emlcal cal calendars [--account A]
emlcal cal agenda [--days 7 | --from .. --to ..] [--calendar C]
emlcal cal show <id>
emlcal cal free --from .. --to .. [--duration 30m] [--hours 09:00-18:00]
emlcal status                                    # counts, last sync, daemon state
```

Examples:

```
emlcal mail list --mailbox inbox --unread --since 3d --limit 20
emlcal mail search 'invoice AND from:acme' --since 30d
emlcal mail read work:18f3a2b9c1d4e5f6
emlcal mail thread work:t:18f3a2b9c1d4e5f6
emlcal mail attachment list work:18f3a2b9c1d4e5f6
emlcal cal agenda --days 3
emlcal cal free --from tomorrow --to +7d --duration 45m --hours 09:00-18:00
```

## Write commands (ask the user before running)

```
emlcal mail mark <id>... --read|--unread|--flag|--unflag
emlcal mail move <id>... --to <mailbox>
emlcal mail archive <id>...
emlcal mail trash <id>...
emlcal mail draft --account A --to .. [--cc ..] --subject .. (--body .. | --body-file f) \
                  [--reply <id> [--all]] [--attach f]
emlcal mail send --draft <id> | (same flags as draft) [--dry-run]
emlcal mail reply <id> (--body .. | --body-file f) [--all] [--dry-run]
emlcal mail forward <id> --to .. [--body ..] [--no-attachments] [--dry-run]
emlcal cal create --title .. --start .. --end .. [--calendar C] [--attendees ..] \
                  [--location ..] [--description ..] [--dry-run]
emlcal cal update <id> [same flags]
emlcal cal delete <id>
emlcal cal respond <id> --accept|--decline|--tentative
```

Examples:

```
emlcal mail reply work:18f3a2b9c1d4e5f6 --body "Works for me — see you Tuesday." --dry-run
emlcal mail archive work:18f3a2b9c1d4e5f6
emlcal cal create --title "Design review" --start "2026-09-01T14:00" --end "2026-09-01T15:00" --dry-run
emlcal cal respond home:c:primary:abc123 --accept
```

`--dry-run` prints exactly what would be sent (full RFC 822 for mail) and exits
0 without queueing anything.

## JSON shapes

`mail list` / `mail search` — an array of:

```json
{"id":"work:18f3a2b9c1d4e5f6","thread_id":"work:t:18f3a2b9c1d4e5f6",
 "date":"2026-08-24T09:12:00+02:00","from":{"name":"Alice","email":"alice@example.com"},
 "subject":"Invoice 4021","snippet":"As agreed, attached is …","unread":true,
 "flagged":false,"has_attachments":true,"mailboxes":["Inbox"],"account":"work"}
```

`mail read` — one object with those fields plus:

```json
{"to":[{"name":"","email":"me@example.com"}],"cc":[],
 "body":"As agreed, attached is the invoice …",
 "attachments":[{"part":"2","filename":"invoice.pdf","content_type":"application/pdf","size":48213}]}
```

A message that carries a calendar invitation (a `text/calendar` part, listed
under `attachments` as `invite.ics`) also has `invite`:

```json
{"invite":{"kind":"invitation","method":"REQUEST","title":"Design review",
 "start":"2026-09-02T10:00:00+02:00","end":"2026-09-02T10:45:00+02:00","all_day":false,
 "location":"Teams","organizer":{"name":"Alice","email":"alice@example.com"},
 "attendees":[{"email":"me@example.com","response":"needs-action","self":true}],
 "my_response":"needs-action","needs_answer":true,
 "event_id":"work:c:primary:abc123"}}
```

`kind` is `invitation`, `cancellation`, `reply` or `event`. `event_id` is the
calendar's own copy of the event and is what `cal respond` takes; when it is
missing the calendar has not synced the event yet (run `emlcal sync`) or the
account has no calendar, and the invite cannot be answered from here.

`cal agenda` — an array of:

```json
{"id":"home:c:primary:abc123","start":"2026-08-26T10:00:00+02:00",
 "end":"2026-08-26T11:00:00+02:00","all_day":false,"title":"Standup",
 "calendar":"Work","location":"Room 2","my_response":"accepted","account":"home"}
```

## Guidance

- Bound every search with `--since` (and `--limit`) — the archive holds years of
  mail and an unbounded query wastes tokens.
- Read before you answer: `mail read <id>` for one message, `mail thread <id>`
  when the conversation matters. Never reply from a snippet alone.
- Run write commands with `--dry-run` first and show the user the result, unless
  they have already told you to just send it.
- Never `trash`, `delete` or `move` anything unless the user asked for it.
  Archiving is not deleting, but it still needs their say-so.
- Quote the message id when you report a finding, so the user can jump to it.
- An invitation is answered on the calendar, not by mail: read the message,
  then `cal respond <invite.event_id> --accept|--decline|--tentative`. Do not
  reply to the organizer's mail instead.
- If results look stale or a message the user mentions is missing, run
  `emlcal status` to see the last sync and whether the daemon runs, then
  `emlcal sync` once.
- Searching is FTS5: `AND`, `OR`, `NOT`, `"exact phrase"`, `subject:budget`.
  Quote the whole query so the shell does not eat it.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | generic failure |
| 2 | bad flags or arguments |
| 3 | the id, account or mailbox does not exist |
| 4 | offline and the operation needs the network |
| 5 | the provider rejected the request |
| 6 | write accepted locally and queued in the outbox (it will go out later) |

Exit 6 is not a failure: tell the user the write is queued and will be sent when
the connection is back.
