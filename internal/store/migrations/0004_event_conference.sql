-- The video-call link a provider attaches to an event (a Google Meet room).
-- Read from the provider object on sync; `cal create --meet` asks Google
-- Calendar to mint one and stores what comes back. NULL for an event with no
-- conference — which is most of them, so no index.

ALTER TABLE events ADD COLUMN conference_url TEXT;
