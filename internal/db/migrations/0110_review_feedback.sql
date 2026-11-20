-- The OpenLLMs requests whose answers raised a review's findings, and what
-- Waypoint last told OpenLLMs of how those findings fared (WP-514): the
-- gateway ranks review models by these reports (openllms ADR 0035).
CREATE TABLE review_requests (
    request_id      TEXT    PRIMARY KEY,      -- the X-Request-ID Waypoint sent
    run_id          INTEGER NOT NULL REFERENCES bot_runs(id) ON DELETE CASCADE,
    created_at      INTEGER NOT NULL,
    verified        INTEGER NOT NULL DEFAULT 0, -- as last reported
    false_positives INTEGER NOT NULL DEFAULT 0,
    reported_at     INTEGER,                    -- NULL: never reported
    tries           INTEGER NOT NULL DEFAULT 0, -- failed reports since the last good one
    next_try_at     INTEGER NOT NULL DEFAULT 0, -- not before (ms) after a failure
    error           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX review_requests_created ON review_requests(created_at);

CREATE TABLE review_finding_requests (
    finding_id INTEGER NOT NULL REFERENCES bot_findings(id) ON DELETE CASCADE,
    request_id TEXT    NOT NULL REFERENCES review_requests(request_id) ON DELETE CASCADE,
    PRIMARY KEY (finding_id, request_id)
);
CREATE INDEX review_finding_requests_request ON review_finding_requests(request_id);
