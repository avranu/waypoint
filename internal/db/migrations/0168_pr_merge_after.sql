-- Merge order (WP-912): a pull request set to merge only after another one
-- has (wp pr after). Waits that come from task links (a task of one blocked
-- by a task of the other) are read from issue_relations, not kept here.
CREATE TABLE pr_merge_after (
    pr_id         INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    after_id      INTEGER NOT NULL REFERENCES pull_requests(id) ON DELETE CASCADE,
    created_at    INTEGER NOT NULL,
    created_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_agent TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (pr_id, after_id),
    CHECK (pr_id != after_id)
);
CREATE INDEX pr_merge_after_after ON pr_merge_after(after_id);
-- The waits from task links find the other task's pull request by its URL,
-- and a pull request's links by URL or (written another way) number.
CREATE INDEX pull_requests_url ON pull_requests(url);
CREATE INDEX external_links_number ON external_links(number);
