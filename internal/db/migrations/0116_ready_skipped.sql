-- The checks a pull request's full run skipped although the draft tier ran
-- them (WP-552): a draft-only job keeps the draft tier's success as its
-- status, since Forgejo posts none for the skip, so no status newer than
-- the ready mark ever comes for it. Read from the head's Actions runs.
ALTER TABLE pull_requests ADD COLUMN ready_skipped TEXT NOT NULL DEFAULT '[]';
