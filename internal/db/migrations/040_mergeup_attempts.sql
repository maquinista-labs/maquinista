-- 040_mergeup_attempts.sql
--
-- MAQ-26: auto merge-up attempt budget for merge_queue entries.
--
-- When the gh merge gate's rebase hits a conflict, it now attempts an
-- automatic merge-up first (GitHub update-branch API, falling back to a
-- local ref merge of the base into the branch), re-checks mergeability, and
-- only parks needs-human after 2 FAILED merge-ups. Each failed attempt
-- consumes one unit of this budget; the counter persists across the
-- release-and-reclaim passes the retry loop uses.
--
-- Deliberately separate from `attempts` (the CI-red / merger-episode
-- reclaim budget, EX-06/MAQ-15): the two caps mean different things and
-- must not eat each other's room — a PR that churned red three times still
-- gets its two merge-up tries, and a twice-failed merge-up still leaves the
-- CI budget untouched for the post-resolution passes.

ALTER TABLE merge_queue ADD COLUMN IF NOT EXISTS mergeup_attempts INT NOT NULL DEFAULT 0;

COMMENT ON COLUMN merge_queue.mergeup_attempts IS
  'Failed auto merge-up attempts (MAQ-26); at 2 the rebase conflict parks needs-human.';
