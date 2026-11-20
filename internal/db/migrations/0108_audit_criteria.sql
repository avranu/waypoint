-- Audits say what to read and what to look for more exactly (WP-548): what
-- landed on a branch over a period, the parts of the repository to read or
-- leave out, the areas to look hardest at, the lowest severity worth
-- reporting and how many findings at most. The runner records the commits
-- it read.
ALTER TABLE code_audits ADD COLUMN since_spec   TEXT    NOT NULL DEFAULT '';   -- as asked: 7d, 2w, 2026-10-28 ('' : not a period)
ALTER TABLE code_audits ADD COLUMN since_at     INTEGER NOT NULL DEFAULT 0;    -- the period's start, ms (0: not a period)
ALTER TABLE code_audits ADD COLUMN focus        TEXT    NOT NULL DEFAULT '[]'; -- focus areas' keys
ALTER TABLE code_audits ADD COLUMN paths        TEXT    NOT NULL DEFAULT '[]'; -- read only these (pathspecs; [] : everything)
ALTER TABLE code_audits ADD COLUMN exclude      TEXT    NOT NULL DEFAULT '[]'; -- leave these out
ALTER TABLE code_audits ADD COLUMN min_severity TEXT    NOT NULL DEFAULT '';   -- report nothing below it ('' : low)
ALTER TABLE code_audits ADD COLUMN max_findings INTEGER NOT NULL DEFAULT 0;    -- 0: the default
ALTER TABLE code_audits ADD COLUMN from_sha     TEXT    NOT NULL DEFAULT '';   -- the commit the change was read from
ALTER TABLE code_audits ADD COLUMN to_sha       TEXT    NOT NULL DEFAULT '';   -- the commit checked out
ALTER TABLE code_audits ADD COLUMN commit_count INTEGER NOT NULL DEFAULT -1;   -- commits between them (-1: unknown)
