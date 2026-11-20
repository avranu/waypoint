-- Each keep-warm decision and what became of the session after it (WP-1000,
-- WP-ADR95): whether its next real prompt came within the hour a ping buys,
-- later, or never (it was cleared or ended first). The decision reads these
-- rows back: a ping or a compaction goes ahead only while sessions of its
-- kind are used again often enough for it to pay.
CREATE TABLE warm_decisions (
    id             INTEGER PRIMARY KEY,
    session_id     INTEGER NOT NULL REFERENCES agent_sessions(id) ON DELETE CASCADE,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    decided_at     INTEGER NOT NULL,
    action         TEXT    NOT NULL,            -- ping, compact, cool, news
    rule           TEXT    NOT NULL DEFAULT '', -- the rule's action, when the evidence changed it
    class          TEXT    NOT NULL,            -- why: work, requests, chat, unattended, finished, clearable, …
    model          TEXT    NOT NULL DEFAULT '',
    context_tokens INTEGER NOT NULL DEFAULT 0,
    next_at        INTEGER,                     -- its next real prompt
    ended_at       INTEGER,                     -- it ended before one
    end_reason     TEXT    NOT NULL DEFAULT ''  -- clear, prompt_input_exit, other, …
);
CREATE INDEX warm_decisions_user_class ON warm_decisions(user_id, class, decided_at);
CREATE INDEX warm_decisions_open ON warm_decisions(session_id) WHERE next_at IS NULL AND ended_at IS NULL;

-- The decisions so far, read back from the session timelines' "Prompt
-- cache" lines (the timeline keeps each session's last 1,500 events).
INSERT INTO warm_decisions (session_id, user_id, decided_at, action, class, model, context_tokens, next_at, ended_at, end_reason)
SELECT d.session_id, d.user_id, d.at, d.action, d.class, d.model, d.ctx,
    CASE WHEN d.next_at IS NOT NULL AND (d.end_at IS NULL OR d.next_at <= d.end_at) THEN d.next_at END,
    CASE WHEN d.end_at IS NOT NULL AND (d.next_at IS NULL OR d.next_at > d.end_at) THEN d.end_at END,
    CASE WHEN d.end_at IS NOT NULL AND (d.next_at IS NULL OR d.next_at > d.end_at)
        THEN coalesce((SELECT x.detail FROM session_events x WHERE x.session_id = d.session_id AND x.kind = 'ended' AND x.created_at = d.end_at LIMIT 1), '')
        ELSE '' END
FROM (
    SELECT e.session_id, s.user_id, e.created_at AS at, s.model,
        CASE WHEN e.detail LIKE 'Kept the prompt cache warm: its batched news%' THEN 'news'
             WHEN e.detail LIKE 'Kept the prompt cache warm:%' THEN 'ping'
             WHEN e.detail LIKE 'Compacting it (%' THEN 'compact'
             ELSE 'cool' END AS action,
        CASE WHEN e.detail LIKE '%batched news%' THEN 'news'
             WHEN e.detail LIKE '%paused agents%' THEN 'paused'
             WHEN e.detail LIKE '%can be cleared%' THEN 'clearable'
             WHEN e.detail LIKE '%waited over%' THEN 'work_cap'
             WHEN e.detail LIKE '%waiting on %' THEN 'work'
             WHEN e.detail LIKE '%autopilot runs it%' THEN 'unattended'
             WHEN e.detail LIKE '%its person is away%' THEN 'requests_away'
             WHEN e.detail LIKE '%unanswered for over%' THEN 'requests_cap'
             WHEN e.detail LIKE '%request%' THEN 'requests'
             WHEN e.detail LIKE '%hasn''t come back%' THEN 'chat_cap'
             WHEN e.detail LIKE '%working with it%' THEN 'chat'
             WHEN e.detail LIKE '%finished, nothing waits%' THEN 'finished'
             ELSE 'other' END AS class,
        CASE WHEN e.detail LIKE 'Compacting it (%k tokens%'
             THEN CAST(substr(e.detail, 16, instr(e.detail, 'k tokens') - 16) AS INTEGER) * 1000
             ELSE 0 END AS ctx,
        (SELECT min(p.created_at) FROM session_events p WHERE p.session_id = e.session_id AND p.kind = 'prompt' AND p.created_at > e.created_at) AS next_at,
        (SELECT min(p.created_at) FROM session_events p WHERE p.session_id = e.session_id AND p.kind = 'ended' AND p.created_at > e.created_at) AS end_at
    FROM session_events e JOIN agent_sessions s ON s.id = e.session_id
    WHERE e.kind = 'cache' AND (e.detail LIKE 'Kept the prompt cache warm:%' OR e.detail LIKE 'Compacting it (%' OR e.detail LIKE 'Let the prompt cache cool:%')
) d;
