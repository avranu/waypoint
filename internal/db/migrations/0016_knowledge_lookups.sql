-- Wikilinks look items up by name (ADR 0010).
CREATE INDEX glossary_term_name ON glossary(term COLLATE NOCASE);
CREATE INDEX design_notes_title ON design_notes(title COLLATE NOCASE);
CREATE INDEX pages_title ON pages(title COLLATE NOCASE);

-- Which agent linked a page and an issue ('' = a person), so agents can
-- only remove links agents made.
ALTER TABLE page_issues ADD COLUMN created_agent TEXT NOT NULL DEFAULT '';
