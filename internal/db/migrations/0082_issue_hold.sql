-- A hold keeps a task from agents (wp next and take) until hold_until (ms),
-- or until someone lifts it when that is NULL; hold_at NULL = not on hold
-- (WP-269). hold_person: a person placed it, so only a person may change
-- or lift it. See core/hold.go.
ALTER TABLE issues ADD COLUMN hold_at INTEGER;
ALTER TABLE issues ADD COLUMN hold_until INTEGER;
ALTER TABLE issues ADD COLUMN hold_note TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN hold_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE issues ADD COLUMN hold_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE issues ADD COLUMN hold_person INTEGER NOT NULL DEFAULT 0;
