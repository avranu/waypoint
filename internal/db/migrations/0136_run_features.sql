-- What an agent had when it worked on a task (WP-256): each run's versions,
-- recorded when it happened, because a release's date doesn't say whether a
-- given run had it (not deployed yet, an old wp, a session started earlier).
-- One row per run and feature, so "which runs had X" is an index lookup.
CREATE TABLE run_features (
    run_id      INTEGER NOT NULL REFERENCES agent_runs(id) ON DELETE CASCADE,
    feature     TEXT    NOT NULL, -- server, wp, session_server, session_wp, kit, kit_drift, cli_version, *_end
    value       TEXT    NOT NULL,
    recorded_at INTEGER NOT NULL,
    PRIMARY KEY (run_id, feature)
) WITHOUT ROWID;
CREATE INDEX run_features_lookup ON run_features(feature, value, run_id);

-- What a session loaded at its start: settings, hooks and instructions stay
-- as they were until it restarts, whatever is deployed later.
ALTER TABLE agent_sessions ADD COLUMN started_server TEXT NOT NULL DEFAULT '';
ALTER TABLE agent_sessions ADD COLUMN started_wp TEXT NOT NULL DEFAULT '';
