-- Whether a conversation carries files, denormalised onto the summary row.
--
-- "Did the invoice actually go out" is a question about the list row, not one
-- worth opening a thread to answer, and the list is one indexed query over
-- threads however many the archive holds -- so the flag has to live here
-- rather than be joined per visible row. Same shape as unread_count:
-- refreshThread ORs it over the thread's live messages.

ALTER TABLE threads ADD COLUMN has_attachments INTEGER NOT NULL DEFAULT 0;

-- Backfill. Existing summaries were written before the column existed, and
-- refreshThread only runs again when something in the thread changes -- so
-- without this an untouched archive would read "no attachments" everywhere.
UPDATE threads SET has_attachments = 1
 WHERE EXISTS (SELECT 1 FROM messages m
                WHERE m.account_id = threads.account_id
                  AND m.thread_id  = threads.thread_id
                  AND m.deleted_at IS NULL
                  AND m.has_attachments = 1);
