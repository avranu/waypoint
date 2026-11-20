-- The goal context policy (WP-1035): how much of a task's goal chain an
-- agent is shown. Drawn once per task, the first time an agent holding a
-- fresh claim gets the task's text, and kept: goal (only the goal the task
-- traces to: today's way, the control), chain (every parent goal, titles
-- as written) or pruned (parents the coverage judgement says the task and
-- its goal already convey left out, long titles clipped, the chain cut at
-- a length; whether one text conveys another is a judgement of meaning, not
-- a count of shared words, WP-ADR133). Shares
-- come from how the tasks' pull requests fared on the drift check
-- (goal_context.go); nothing here is polled.
--
-- policy: thompson (drawn at the shares). propensity: the drawn arm's share.
-- shares: JSON, every candidate's share when drawn. candidates: the arms
-- whose text differed for this task, comma separated. chain_len: the
-- parents shown under chain; text_len: the task's title and description in
-- characters (covariates, known before the draw). covered: JSON array of the
-- parent goal ids the coverage judgement (goal_meaning_pairs) said the
-- task already conveyed when drawn, kept so what an agent is shown doesn't
-- change after the draw; NULL when there was no current judgement.
CREATE TABLE goal_context_assignments (
    issue_id    INTEGER PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    arm         TEXT    NOT NULL,
    policy      TEXT    NOT NULL,
    propensity  REAL    NOT NULL DEFAULT 1,
    shares      TEXT    NOT NULL DEFAULT '{}',
    candidates  TEXT    NOT NULL DEFAULT '',
    chain_len   INTEGER NOT NULL DEFAULT 0,
    text_len    INTEGER NOT NULL DEFAULT 0,
    via         TEXT    NOT NULL DEFAULT '', -- next, take
    covered     TEXT,
    assigned_at INTEGER NOT NULL
);
CREATE INDEX goal_context_assignments_at ON goal_context_assignments(assigned_at);


-- Judgements of meaning about goals (WP-1035, WP-ADR133, page WP-PL17).
-- Three questions about pairs of texts: overlap (two goals overlap in
-- meaning), restates (a goal says nothing beyond its parent) and cover (a
-- task and its goal already convey a parent goal). Several scorers answer
-- each pair in the background (shared words, embeddings, an NLI model, a
-- language model asked about the one pair); a decider per question, chosen
-- on held-out labels that come from what people and agents did, turns the
-- scores into a yes or no. Everything is kept by hash of the text it read,
-- so only new or reworded text is scored again.

-- meaning_texts: every text a scorer read or a label names, by hash, so a
-- label keeps its text after the goal or task is reworded.
CREATE TABLE meaning_texts (
    hash    TEXT    PRIMARY KEY,
    text    TEXT    NOT NULL,
    seen_at INTEGER NOT NULL
);

-- meaning_vectors: an embedding model's vector of a text. model: the id and
-- revision the meaning service named. vec: little-endian float32s.
CREATE TABLE meaning_vectors (
    model TEXT NOT NULL,
    hash  TEXT NOT NULL,
    vec   BLOB NOT NULL,
    PRIMARY KEY (model, hash)
);

-- meaning_scores: one scorer's score of an ordered pair of texts. scorer
-- names the scorer and its version (nli:MODEL@REV, llm:QUESTION:vN), so a
-- new version scores afresh. score NULL: the try failed (error); it is
-- tried again after a back-off. model and request_id: the language model
-- that answered and the gateway's X-Request-ID, for the report on how the
-- answer fared (meaning_feedback).
CREATE TABLE meaning_scores (
    scorer     TEXT    NOT NULL,
    a_hash     TEXT    NOT NULL,
    b_hash     TEXT    NOT NULL,
    score      REAL,
    why        TEXT    NOT NULL DEFAULT '',
    model      TEXT    NOT NULL DEFAULT '',
    request_id TEXT    NOT NULL DEFAULT '',
    error      TEXT    NOT NULL DEFAULT '',
    tries      INTEGER NOT NULL DEFAULT 0,
    scored_at  INTEGER NOT NULL,
    PRIMARY KEY (scorer, a_hash, b_hash)
);

-- goal_meaning_pairs: what the decider said of each current pair. overlap:
-- a_id < b_id, two goals, neither an ancestor of the other. restates: a_id
-- the goal, b_id its parent. cover: a_id the task (an issue), b_id a parent
-- goal of the task's goal. A row counts only while both hashes match the
-- texts as they are now; decider names the fit (or the prior) that decided.
CREATE TABLE goal_meaning_pairs (
    judgement TEXT    NOT NULL,
    a_id      INTEGER NOT NULL,
    b_id      INTEGER NOT NULL,
    a_hash    TEXT    NOT NULL,
    b_hash    TEXT    NOT NULL,
    yes       INTEGER NOT NULL,
    p         REAL    NOT NULL,
    decider   TEXT    NOT NULL,
    why       TEXT    NOT NULL DEFAULT '',
    judged_at INTEGER NOT NULL,
    PRIMARY KEY (judgement, a_id, b_id)
);
CREATE INDEX goal_meaning_pairs_b ON goal_meaning_pairs(judgement, b_id);

-- goal_meaning_shown: a suggestion (overlap, restates) a person was shown
-- on the Goals view, at the texts it was made on, and what came of it:
-- acted (within 30 days a goal of the pair was reworded, moved or
-- archived), dismissed (the person waved it off) or ignored (30 days on,
-- both goals unchanged).
CREATE TABLE goal_meaning_shown (
    judgement  TEXT    NOT NULL,
    a_id       INTEGER NOT NULL,
    b_id       INTEGER NOT NULL,
    a_hash     TEXT    NOT NULL,
    b_hash     TEXT    NOT NULL,
    a_parent   INTEGER NOT NULL DEFAULT 0, -- each goal's parent when shown, 0 none
    b_parent   INTEGER NOT NULL DEFAULT 0,
    shown_at   INTEGER NOT NULL,
    outcome    TEXT    NOT NULL DEFAULT '',
    decided_at INTEGER,
    PRIMARY KEY (judgement, a_id, b_id, a_hash, b_hash)
);
CREATE INDEX goal_meaning_shown_open ON goal_meaning_shown(outcome, shown_at);

-- meaning_labels: what is known of a pair from what happened, never from a
-- person asked to label: shown suggestions acted on, dismissed or ignored;
-- drift flags on agents' pull requests naming a parent goal the agent was
-- not shown (cover); and cases built from each goal by a language model,
-- labelled by how they were built (constructed). grp keeps related labels
-- (one goal's cases) on one side of the held-out split.
CREATE TABLE meaning_labels (
    id        INTEGER PRIMARY KEY,
    judgement TEXT    NOT NULL,
    a_hash    TEXT    NOT NULL,
    b_hash    TEXT    NOT NULL,
    y         INTEGER NOT NULL,
    source    TEXT    NOT NULL,
    grp       TEXT    NOT NULL,
    ref       TEXT    NOT NULL DEFAULT '',
    at        INTEGER NOT NULL,
    UNIQUE (judgement, a_hash, b_hash, source)
);
CREATE INDEX meaning_labels_judgement ON meaning_labels(judgement, id);

-- meaning_constructed: the goals whose cases were built, by title hash, so
-- each title is built from once. error: the try failed; tried again later.
CREATE TABLE meaning_constructed (
    goal_hash TEXT    PRIMARY KEY,
    goal_id   INTEGER NOT NULL,
    model     TEXT    NOT NULL DEFAULT '',
    error     TEXT    NOT NULL DEFAULT '',
    tries     INTEGER NOT NULL DEFAULT 0,
    made_at   INTEGER NOT NULL
);

-- meaning_fits: each evaluation of a question's scorers on held-out labels.
-- The newest row per question decides until the labels that came after it
-- show its decider erring more than it did held out, or a scorer's version
-- changes; then the next evaluation runs (reason: first, drift, version).
-- decider: a scorer or "combined" (a logistic regression over the scorers,
-- model JSON). line: the score at or above which the answer is yes.
-- err_pos, err_neg: the decider's error rates on the held-out labels whose
-- answer is yes (missed) and no (said yes); a later label is checked
-- against the rate of its own answer, since the labels that come in later
-- need not mix yes and no as the held-out ones did.
-- candidates: JSON, every candidate's held-out figures. label_through: the
-- newest label it saw. versions: JSON, the scorer versions it used.
CREATE TABLE meaning_fits (
    id             INTEGER PRIMARY KEY,
    judgement      TEXT    NOT NULL,
    decider        TEXT    NOT NULL,
    model          TEXT    NOT NULL DEFAULT '',
    line           REAL    NOT NULL,
    err_pos        REAL    NOT NULL,
    err_neg        REAL    NOT NULL,
    labels         INTEGER NOT NULL,
    held_out       INTEGER NOT NULL,
    candidates     TEXT    NOT NULL DEFAULT '{}',
    label_through  INTEGER NOT NULL,
    versions       TEXT    NOT NULL DEFAULT '{}',
    reason         TEXT    NOT NULL,
    note           TEXT    NOT NULL DEFAULT '',
    fitted_at      INTEGER NOT NULL
);
CREATE INDEX meaning_fits_judgement ON meaning_fits(judgement, id);

-- meaning_feedback: the report to the gateway on a language model's answer
-- once a label says whether it was right: verified 1 when it was, false
-- positives 1 when it was not (POST /v1/feedback). Sent again when a later
-- label overturns it.
CREATE TABLE meaning_feedback (
    request_id      TEXT    PRIMARY KEY,
    asked_at        INTEGER NOT NULL,
    verified        INTEGER NOT NULL,
    false_positives INTEGER NOT NULL,
    reported_at     INTEGER,
    tries           INTEGER NOT NULL DEFAULT 0,
    next_try_at     INTEGER NOT NULL DEFAULT 0,
    error           TEXT    NOT NULL DEFAULT ''
);
