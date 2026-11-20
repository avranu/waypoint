-- Waypoint initial schema.
-- Timestamps are INTEGER unix milliseconds (UTC). Calendar dates are TEXT 'YYYY-MM-DD'.

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    email         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT    NOT NULL DEFAULT '',
    password_hash TEXT    NOT NULL DEFAULT '',
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    avatar        BLOB,
    avatar_type   TEXT    NOT NULL DEFAULT '',
    avatar_version INTEGER NOT NULL DEFAULT 0,
    timezone      TEXT    NOT NULL DEFAULT 'UTC',
    -- per-user preferences: notification channels, theme, gotify target, etc.
    prefs         TEXT    NOT NULL DEFAULT '{}',
    calendar_token TEXT   NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
    token_hash   TEXT    PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    user_agent   TEXT    NOT NULL DEFAULT '',
    ip           TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL UNIQUE,
    prefix       TEXT    NOT NULL,
    read_only    INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    expires_at   INTEGER
);
CREATE INDEX api_tokens_user ON api_tokens(user_id);

-- One-time links: invites (no user yet) and password resets (existing user).
CREATE TABLE user_links (
    token_hash TEXT    PRIMARY KEY,
    kind       TEXT    NOT NULL CHECK (kind IN ('invite','reset')),
    user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE,
    email      TEXT    NOT NULL DEFAULT '',
    is_admin   INTEGER NOT NULL DEFAULT 0,
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);

CREATE TABLE projects (
    id          INTEGER PRIMARY KEY,
    key         TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    color       TEXT    NOT NULL DEFAULT '#6366f1',
    icon        TEXT    NOT NULL DEFAULT '',
    background  TEXT    NOT NULL DEFAULT '',
    archived    INTEGER NOT NULL DEFAULT 0,
    next_number INTEGER NOT NULL DEFAULT 1,
    -- settings: sprint length, velocity window, default points, ...
    settings    TEXT    NOT NULL DEFAULT '{}',
    created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE project_members (
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK (role IN ('owner','editor','viewer')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX project_members_user ON project_members(user_id);

CREATE TABLE statuses (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL COLLATE NOCASE,
    category   TEXT    NOT NULL CHECK (category IN ('backlog','todo','in_progress','done','canceled')),
    color      TEXT    NOT NULL DEFAULT '',
    position   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (project_id, name)
);

CREATE TABLE labels (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL COLLATE NOCASE,
    color      TEXT    NOT NULL DEFAULT '#64748b',
    UNIQUE (project_id, name)
);

CREATE TABLE sprints (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    goal         TEXT    NOT NULL DEFAULT '',
    state        TEXT    NOT NULL DEFAULT 'planned' CHECK (state IN ('planned','active','closed')),
    start_date   TEXT,
    end_date     TEXT,
    started_at   INTEGER,
    closed_at    INTEGER,
    -- points committed when the sprint started, and completed when it closed
    committed_points REAL,
    completed_points REAL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX sprints_project ON sprints(project_id, state);

CREATE TABLE issues (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    number       INTEGER NOT NULL,
    type         TEXT    NOT NULL DEFAULT 'task' CHECK (type IN ('epic','story','task','bug','subtask')),
    title        TEXT    NOT NULL,
    description  TEXT    NOT NULL DEFAULT '',
    status_id    INTEGER NOT NULL REFERENCES statuses(id),
    priority     INTEGER NOT NULL DEFAULT 0, -- 0 none, 1 low, 2 medium, 3 high, 4 urgent
    parent_id    INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    sprint_id    INTEGER REFERENCES sprints(id) ON DELETE SET NULL,
    reporter_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    points       REAL,
    start_date   TEXT,
    due_date     TEXT,
    rank         REAL    NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    -- set when the issue enters a done/canceled status, cleared when reopened
    resolved_at  INTEGER,
    UNIQUE (project_id, number)
);
CREATE INDEX issues_project_status ON issues(project_id, status_id);
CREATE INDEX issues_parent ON issues(parent_id);
CREATE INDEX issues_sprint ON issues(sprint_id);
CREATE INDEX issues_updated ON issues(updated_at);
CREATE INDEX issues_resolved ON issues(project_id, resolved_at);

-- Old keys of issues that moved to another project, so links keep working.
CREATE TABLE issue_aliases (
    project_key TEXT    NOT NULL COLLATE NOCASE,
    number      INTEGER NOT NULL,
    issue_id    INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    PRIMARY KEY (project_key, number)
);

CREATE TABLE issue_assignees (
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, user_id)
);
CREATE INDEX issue_assignees_user ON issue_assignees(user_id);

CREATE TABLE issue_watchers (
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, user_id)
);
CREATE INDEX issue_watchers_user ON issue_watchers(user_id);

CREATE TABLE issue_labels (
    issue_id INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, label_id)
);
CREATE INDEX issue_labels_label ON issue_labels(label_id);

-- Sharing a single issue (and everything beneath it) with someone who is not
-- a member of its project.
CREATE TABLE issue_shares (
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT    NOT NULL CHECK (role IN ('editor','viewer')),
    created_by INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (issue_id, user_id)
);
CREATE INDEX issue_shares_user ON issue_shares(user_id);

CREATE TABLE issue_relations (
    id         INTEGER PRIMARY KEY,
    source_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id  INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL CHECK (kind IN ('blocks','relates','duplicates')),
    created_at INTEGER NOT NULL,
    UNIQUE (source_id, target_id, kind)
);
CREATE INDEX issue_relations_target ON issue_relations(target_id);

-- Links to things outside Waypoint: GitHub/Forgejo issues and PRs, or any URL.
CREATE TABLE external_links (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    provider   TEXT    NOT NULL DEFAULT 'url', -- github, forgejo, url
    kind       TEXT    NOT NULL DEFAULT 'url', -- issue, pull, commit, url
    url        TEXT    NOT NULL,
    repo       TEXT    NOT NULL DEFAULT '',
    number     INTEGER,
    title      TEXT    NOT NULL DEFAULT '',
    state      TEXT    NOT NULL DEFAULT '', -- open, closed, merged, draft
    synced_at  INTEGER,
    created_at INTEGER NOT NULL,
    UNIQUE (issue_id, url)
);
CREATE INDEX external_links_url ON external_links(url);

CREATE TABLE comments (
    id         INTEGER PRIMARY KEY,
    issue_id   INTEGER NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author_id  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    body       TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX comments_issue ON comments(issue_id, created_at);

-- Append-only history of everything that happened to an issue. Feeds the
-- activity feed, reports, automations and notifications.
CREATE TABLE events (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    issue_id   INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    actor_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    kind       TEXT    NOT NULL,
    field      TEXT    NOT NULL DEFAULT '',
    old_value  TEXT    NOT NULL DEFAULT '',
    new_value  TEXT    NOT NULL DEFAULT '',
    -- structured detail, e.g. status ids and categories for flow reports
    data       TEXT    NOT NULL DEFAULT '{}',
    -- automation id when an automation made the change
    via_automation INTEGER,
    created_at INTEGER NOT NULL
);
CREATE INDEX events_issue ON events(issue_id, id);
CREATE INDEX events_project ON events(project_id, created_at);

CREATE TABLE time_entries (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issue_id   INTEGER REFERENCES issues(id) ON DELETE SET NULL,
    project_id INTEGER REFERENCES projects(id) ON DELETE SET NULL,
    note       TEXT    NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    ended_at   INTEGER, -- NULL while the timer is running
    created_at INTEGER NOT NULL
);
CREATE INDEX time_entries_user ON time_entries(user_id, started_at);
CREATE INDEX time_entries_issue ON time_entries(issue_id);
CREATE UNIQUE INDEX time_entries_one_running ON time_entries(user_id) WHERE ended_at IS NULL;

CREATE TABLE notifications (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    issue_id   INTEGER REFERENCES issues(id) ON DELETE CASCADE,
    actor_id   INTEGER REFERENCES users(id) ON DELETE SET NULL,
    kind       TEXT    NOT NULL,
    title      TEXT    NOT NULL,
    body       TEXT    NOT NULL DEFAULT '',
    read_at    INTEGER,
    created_at INTEGER NOT NULL
);
CREATE INDEX notifications_user ON notifications(user_id, id);

CREATE TABLE automations (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT    NOT NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    -- rule: {"trigger": {...}, "conditions": [...], "actions": [...]}
    rule        TEXT    NOT NULL,
    position    INTEGER NOT NULL DEFAULT 0,
    run_count   INTEGER NOT NULL DEFAULT 0,
    last_run_at INTEGER,
    last_error  TEXT    NOT NULL DEFAULT '',
    created_by  INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE INDEX automations_project ON automations(project_id);

-- Outbound webhooks: generic JSON, Kestra flow triggers, etc.
CREATE TABLE webhooks (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE, -- NULL = all projects
    name       TEXT    NOT NULL,
    url        TEXT    NOT NULL,
    secret     TEXT    NOT NULL DEFAULT '',
    events     TEXT    NOT NULL DEFAULT '*', -- comma separated event kinds or *
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_status INTEGER,
    last_error TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

-- Connections to Git forges: used to sync linked issues/PRs and to accept
-- inbound webhooks.
CREATE TABLE forges (
    id             INTEGER PRIMARY KEY,
    kind           TEXT    NOT NULL CHECK (kind IN ('github','forgejo')),
    name           TEXT    NOT NULL,
    base_url       TEXT    NOT NULL, -- https://github.com or https://git.example.com
    token          TEXT    NOT NULL DEFAULT '',
    webhook_secret TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL
);

-- A repository attached to a project: issues there can be imported/synced.
CREATE TABLE project_repos (
    id         INTEGER PRIMARY KEY,
    project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    forge_id   INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    repo       TEXT    NOT NULL, -- owner/name
    sync_issues INTEGER NOT NULL DEFAULT 0,
    last_synced_at INTEGER,
    created_at INTEGER NOT NULL,
    UNIQUE (project_id, forge_id, repo)
);

CREATE TABLE saved_views (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT    NOT NULL,
    query      TEXT    NOT NULL,
    layout     TEXT    NOT NULL DEFAULT 'list' CHECK (layout IN ('list','board')),
    pinned     INTEGER NOT NULL DEFAULT 0,
    position   INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL
);
CREATE INDEX saved_views_user ON saved_views(user_id);

-- Full text search over issue text and comments. rowid = issues.id.
CREATE VIRTUAL TABLE issue_search USING fts5(
    key, title, body,
    tokenize = 'porter unicode61 remove_diacritics 2'
);
