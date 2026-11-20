-- Decision notes get ADR codes (WP-113): ADR7, APP-ADR7. A note's code is
-- its number in the project it lives in (scope 0 = global notes). design_codes
-- keeps every code a note has had, so old codes keep resolving after a move,
-- and a deleted note's code stays reserved (note_id NULL), never reused.
ALTER TABLE design_notes ADD COLUMN number INTEGER;
CREATE TABLE design_codes (
    scope   INTEGER NOT NULL, -- project id, or 0 for global notes
    number  INTEGER NOT NULL,
    note_id INTEGER REFERENCES design_notes(id) ON DELETE SET NULL,
    PRIMARY KEY (scope, number)
);
CREATE INDEX design_codes_note ON design_codes(note_id);
