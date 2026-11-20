-- What a review was made of and how its findings held up (WP-363, WP-362):
-- each run and finding records the prompt version and context strategy it
-- came from, and the verifier's verdict. A finding the verifier (or the
-- evidence check) dropped is kept as a row with state 'dropped', placement
-- 'withheld': it is not open and was not posted, but its fate can be judged.
-- bot_runs also keeps the tokens spent per stage, from the model-call log.

ALTER TABLE bot_findings ADD COLUMN prompt_version TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_findings ADD COLUMN strategy       TEXT NOT NULL DEFAULT '';  -- diff, diff+files
ALTER TABLE bot_findings ADD COLUMN verify         TEXT NOT NULL DEFAULT '';  -- kept, corrected, dropped, unchecked, skipped ('' before WP-363)
ALTER TABLE bot_findings ADD COLUMN verify_reason  TEXT NOT NULL DEFAULT '';

ALTER TABLE bot_runs ADD COLUMN prompt_version    TEXT    NOT NULL DEFAULT '';
ALTER TABLE bot_runs ADD COLUMN strategy          TEXT    NOT NULL DEFAULT '';
ALTER TABLE bot_runs ADD COLUMN verified          INTEGER NOT NULL DEFAULT 0; -- 1: the verifier judged at least one finding
ALTER TABLE bot_runs ADD COLUMN review_tokens_in  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bot_runs ADD COLUMN review_tokens_out INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bot_runs ADD COLUMN verify_tokens_in  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bot_runs ADD COLUMN verify_tokens_out INTEGER NOT NULL DEFAULT 0;
