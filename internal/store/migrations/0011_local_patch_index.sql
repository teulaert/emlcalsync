-- Every mail delta asks for the rows whose local-write fence has expired
-- without the provider agreeing with it (store.StaleLocalPatches). Almost no
-- row carries a fence, so a partial index keeps that lookup off a table scan.
CREATE INDEX messages_local_patch ON messages(account_id, local_patch_at)
  WHERE local_patch_at IS NOT NULL;
