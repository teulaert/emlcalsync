-- A write the provider permanently rejected is not pending any more: it will
-- never succeed on a retry, so it must not be re-executed (re-sending a
-- rejected message ten times is worse than not sending it at all). done_at
-- stays NULL — the write did not go through — and failed_at records when we
-- gave up. ListOutbox(pending=true) skips rows with either column set.

ALTER TABLE outbox ADD COLUMN failed_at INTEGER;

DROP INDEX IF EXISTS outbox_pending;
CREATE INDEX outbox_pending ON outbox(done_at, failed_at, id);
