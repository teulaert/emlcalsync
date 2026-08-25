# Provider review findings (2026-08-25)
HIGH
1 jmap/mail.go Enumerate uses position cursor -> deletions during backfill skip messages. Fix: anchor/anchorOffset.
2 jmap/mail.go collectChanges truncates when hasMoreChanges && newState unchanged. Fix: error/ErrStateExpired.
3 jmap/client.go doRetry + gmail/errors.go do(): non-idempotent writes (send/import/submission/drafts.create) retried on 5xx/429 -> duplicates. Fix: retrySafe flag.
4 gcal/write.go no sendUpdates on Respond/insert/patch/delete -> organiser never notified. Fix: SendUpdates("all") when attendees.
5 jmap/jscalendar.go recurrenceOverrides dropped -> exceptions/cancellations missing for Fastmail. Fix: map excluded->EXDATE, others->exception events.
6 jmap/jscalendar.go participantKey uses email as Id (invalid alphabet). Fix: base64url(sha256(email))[:22].
7 gmail/write.go CreateDraft returns message id; draft id lost. Decision: keep message id; `send --draft` sends raw then trashes draft.
MEDIUM
8 gcal/gcal.go NewState empty when nextSyncToken missing -> guard/error.
9 gcal/map.go cancelled instances without start -> zero times. Skip time mapping.
10 gmail/changes.go Updated id with no local row never fetched -> sync engine: treat as Added.
11 oauth persistingSource swallows Save errors -> log Warn.
12 gmail batch endpoint host www.googleapis.com vs gmail.googleapis.com -> verify.
13 jmap Identity/get uses mail account; should use submission primary account.
14 jmap 403 treated as AuthError -> only 401.
LOW
15 oauth login redirect host must be 127.0.0.1. 16 oauth 400 w/o error code -> reauth spurious. 17 jmap push parseStateChange check @type. 18 oauth MemoryTokenStore mutex.
UNVERIFIED: Fastmail calendar capability URN (highest risk); quota units; JMAP calendars draft names.
