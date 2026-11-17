-- A draft is not conversation activity (Gmail's rule, and Lennert's): a
-- thread is dated by its newest message that is not a draft, and only a
-- thread made of drafts alone by its newest draft. refreshThread computes that
-- from now on; this re-dates the summaries written before, which counted
-- drafts. Only threads holding a live draft can differ, so only those are
-- touched. Data only: an older binary reads the same schema.
UPDATE threads
   SET last_utc = coalesce(
         (SELECT max(m.received_utc) FROM messages m INDEXED BY messages_thread
           WHERE m.account_id = threads.account_id AND m.thread_id = threads.thread_id
             AND m.deleted_at IS NULL AND m.is_draft = 0),
         last_utc)
 WHERE EXISTS (SELECT 1 FROM messages d INDEXED BY messages_thread
                WHERE d.account_id = threads.account_id AND d.thread_id = threads.thread_id
                  AND d.deleted_at IS NULL AND d.is_draft = 1);
