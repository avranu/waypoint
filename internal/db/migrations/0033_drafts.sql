-- Drafts (WP-42): issues and pages a person is still thinking through.
-- Agents can't see them anywhere until a person publishes them; see
-- Actor.HidesDrafts.
ALTER TABLE issues ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pages ADD COLUMN draft INTEGER NOT NULL DEFAULT 0;
