-- PR-link surfacing (MAQ-10): the sync loop pushes each mapped task's
-- pr_url to its ticket issue exactly once per URL. pr_url_synced is the
-- last URL successfully written to the issue — the dedup key that makes
-- repeated 10 s sync ticks idempotent (NULL = never pushed).

ALTER TABLE ticket_issue_map ADD COLUMN IF NOT EXISTS pr_url_synced TEXT;

COMMENT ON COLUMN ticket_issue_map.pr_url_synced IS
  'Last pr_url successfully pushed to the ticket issue (exactly-once link sync dedup).';
