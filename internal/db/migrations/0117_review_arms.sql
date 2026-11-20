-- Review arms (WP-365): the arm (context strategy × verifier on or off) a
-- review run drew, and the models OpenLLMs answered its reviewers with, in
-- order (the first reviewer's first).
ALTER TABLE bot_runs ADD COLUMN arm TEXT NOT NULL DEFAULT '';
ALTER TABLE bot_runs ADD COLUMN reviewer_models TEXT NOT NULL DEFAULT '[]';
