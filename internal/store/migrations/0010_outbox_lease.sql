-- A pending row is not necessarily idle. The process that enqueued it is
-- usually pushing it to the provider at that very moment, and nothing in the
-- table said so: the daemon's outbox pass saw a pending row, found no backoff
-- for it in its own memory (the backoff timers live per process), and executed
-- it as well. On 2026-10-02 that sent one message twice, 213 ms apart.
--
-- leased_until is the claim. Whoever is about to execute a row stamps it, and
-- an outbox pass skips a row whose lease is still running. A lease outlives
-- any push by construction (the push's context is bounded by it), so an
-- expired lease means the holder is gone and the row is free again.

ALTER TABLE outbox ADD COLUMN leased_until INTEGER;
