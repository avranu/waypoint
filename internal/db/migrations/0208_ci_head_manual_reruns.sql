-- WP-1661: how many times wp pr rerun re-ran this head's failed jobs. The
-- command is limited to twice per head; a new head is a new row.
ALTER TABLE ci_heads ADD COLUMN manual_reruns INTEGER NOT NULL DEFAULT 0;
