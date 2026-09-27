-- When each resource was last checked, as distinct from when it last changed.
--
-- sync_log records passes that changed something or failed; a delta that
-- found nothing new leaves no row, because a poll every minute for months
-- would otherwise bury the changes and the errors that the log is there to
-- show. But that makes the newest row's time useless as a freshness check: a
-- healthy watch service that has quietly found nothing for hours looks the
-- same as one that stopped. `emlcal status` presented exactly that time as
-- "last sync", and a monitor reading it raised a false alarm.
--
-- sync_checks is one row per account and resource ('mail' or 'calendar'),
-- moved by every pass that completes, whatever it found:
--
--   checked_at    the finish of the last pass that completed without error,
--                 including a pass with nothing to apply
--   attempted_at  the finish of the last pass that completed at all
--   error         what the last pass failed with, NULL when it succeeded
--
-- A pass still running, or one interrupted by the process shutting down, has
-- not completed and moves nothing. A failed pass moves attempted_at and sets
-- error but leaves checked_at where it was, so "last known good" and "last
-- tried" stay apart.
CREATE TABLE sync_checks (
  account_id   TEXT NOT NULL,
  resource     TEXT NOT NULL,               -- 'mail' | 'calendar'
  checked_at   INTEGER,
  attempted_at INTEGER NOT NULL,
  error        TEXT,
  PRIMARY KEY (account_id, resource)
);
