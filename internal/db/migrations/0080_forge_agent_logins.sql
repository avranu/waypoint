-- waypoint:no-foreign-keys
-- An agent's own forge account on a forge's People list (WP-283, WP-ADR13):
-- no Waypoint person (user_id NULL), so nobody is named as having written
-- its text. Its text isn't held as from outside; it is unreviewed agent
-- text, like a person's login that isn't person-only, and it can never be
-- person-only. SQLite can't relax NOT NULL in place, so rebuild the table.
CREATE TABLE forge_people_new (
    forge_id    INTEGER NOT NULL REFERENCES forges(id) ON DELETE CASCADE,
    login       TEXT    NOT NULL COLLATE NOCASE,
    user_id     INTEGER REFERENCES users(id) ON DELETE CASCADE, -- NULL: an agent's account
    person_only INTEGER NOT NULL DEFAULT 0 CHECK (person_only = 0 OR user_id IS NOT NULL),
    PRIMARY KEY (forge_id, login)
);
INSERT INTO forge_people_new(forge_id, login, user_id, person_only)
    SELECT forge_id, login, user_id, person_only FROM forge_people;
DROP TABLE forge_people;
ALTER TABLE forge_people_new RENAME TO forge_people;
