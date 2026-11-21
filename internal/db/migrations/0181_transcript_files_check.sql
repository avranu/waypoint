-- How many indexed transcript pieces had no file on disk at the last check
-- (WP-1466): a restored database without the transcript directory points at
-- files that are not there. NULL checked_at: never checked.
ALTER TABLE transcript_totals ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;
ALTER TABLE transcript_totals ADD COLUMN missing_checked_at INTEGER;
