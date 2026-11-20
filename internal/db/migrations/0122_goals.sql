-- Goals and the trace from every item to them (WP-382, WP-ADR50).

-- How a link was made, fixed when it is made: '' a person made it
-- directly (created_by, in the web or their own terminal; links from
-- before this migration read it from created_by and created_agent);
-- 'agent' an agent, or a person's token used from an agent's shell or
-- session; 'automation' an automation; 'inferred' Waypoint's matcher
-- guessed it (score is its similarity); 'suggested' Waypoint's model
-- suggested it with a goal. Any other value counts as an agent's. A
-- person confirming one by one sets confirmed_by and confirmed_at, which
-- makes it a person's link; a bulk "stop asking" sets only
-- acknowledged_by and acknowledged_at: it leaves the list of links
-- waiting for a person but stays an agent's or Waypoint's.
ALTER TABLE knowledge_links ADD COLUMN basis TEXT NOT NULL DEFAULT '';
ALTER TABLE knowledge_links ADD COLUMN score REAL;
ALTER TABLE knowledge_links ADD COLUMN confirmed_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE knowledge_links ADD COLUMN confirmed_at INTEGER;
ALTER TABLE knowledge_links ADD COLUMN acknowledged_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE knowledge_links ADD COLUMN acknowledged_at INTEGER;

-- Who put an issue under its parent: parent_by is the person who did it
-- directly, NULL for an agent, an automation or anyone unknown. Writers
-- set parent_at with it; a change of parent that doesn't (a writer that
-- forgot) loses the person, so it can only ever count as an agent's.
ALTER TABLE issues ADD COLUMN parent_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN parent_at INTEGER;
-- Before this, a parent was almost always set when the issue was filed:
-- credit it to the reporter when a person filed it.
UPDATE issues SET parent_by = reporter_id, parent_at = created_at
    WHERE parent_id IS NOT NULL AND reporter_agent = '' AND outside = 0 AND reporter_id IS NOT NULL;
CREATE TRIGGER issues_parent_by AFTER UPDATE OF parent_id ON issues
    WHEN new.parent_id IS NOT old.parent_id AND new.parent_at IS old.parent_at
BEGIN
    UPDATE issues SET parent_by = NULL, parent_at = NULL WHERE id = new.id;
END;
CREATE INDEX knowledge_links_rel ON knowledge_links(rel, dst_type, dst_id);

-- A guess a person turned down: the matcher never makes it again.
CREATE TABLE goal_dismissals (
    src_type   TEXT    NOT NULL,
    src_id     INTEGER NOT NULL,
    goal_id    INTEGER NOT NULL REFERENCES design_notes(id) ON DELETE CASCADE,
    user_id    INTEGER REFERENCES users(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (src_type, src_id, goal_id)
);

-- Each item's best route to a goal, recomputed for the whole instance
-- after changes (goal_trace.go). grounding: person_goal, agent_goal,
-- inferred_goal, proposed_goal, person_request, none. hops: to the goal;
-- person_hops: to the nearest person-written or person-approved item
-- (NULL: none); person_touches: items and links on the route a person
-- wrote, approved or confirmed. scoped: the item's project, an ancestor
-- or global has a goal a person approved.
CREATE TABLE goal_traces (
    item_type      TEXT    NOT NULL, -- issue, page, design
    item_id        INTEGER NOT NULL,
    project_id     INTEGER,
    grounding      TEXT    NOT NULL,
    goal_id        INTEGER,
    hops           INTEGER NOT NULL DEFAULT 0,
    person_hops    INTEGER,
    person_touches INTEGER NOT NULL DEFAULT 0,
    scoped         INTEGER NOT NULL DEFAULT 0,
    computed_at    INTEGER NOT NULL,
    PRIMARY KEY (item_type, item_id)
);
CREATE INDEX goal_traces_goal ON goal_traces(goal_id);
CREATE INDEX goal_traces_project ON goal_traces(project_id, grounding);

CREATE TRIGGER goal_traces_issue AFTER DELETE ON issues BEGIN
    DELETE FROM goal_traces WHERE item_type = 'issue' AND item_id = old.id;
    DELETE FROM goal_dismissals WHERE src_type = 'issue' AND src_id = old.id;
END;
CREATE TRIGGER goal_traces_page AFTER DELETE ON pages BEGIN
    DELETE FROM goal_traces WHERE item_type = 'page' AND item_id = old.id;
    DELETE FROM goal_dismissals WHERE src_type = 'page' AND src_id = old.id;
END;
CREATE TRIGGER goal_traces_design AFTER DELETE ON design_notes BEGIN
    DELETE FROM goal_traces WHERE item_type = 'design' AND item_id = old.id;
    DELETE FROM goal_dismissals WHERE src_type = 'design' AND src_id = old.id;
END;

-- The weekly drift check per project (goal_drift.go): when it last ran and
-- what it found, so the next week's notice compares with it.
-- The grounding each task had when it was finished, so the weekly check
-- reports what was true then; a later link doesn't rewrite last week.
-- Refreshed when the task is finished again (resolved_at changes).
CREATE TABLE goal_finished (
    issue_id    INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    project_id  INTEGER NOT NULL,
    grounding   TEXT    NOT NULL,
    resolved_at INTEGER NOT NULL
);
CREATE INDEX goal_finished_project ON goal_finished(project_id, resolved_at);

CREATE TABLE goal_drift_checks (
    project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    checked_at INTEGER NOT NULL,
    grounded   REAL    NOT NULL DEFAULT 0,
    finished   INTEGER NOT NULL DEFAULT 0,
    untraced   INTEGER NOT NULL DEFAULT 0,
    summary    TEXT    NOT NULL DEFAULT ''
);
