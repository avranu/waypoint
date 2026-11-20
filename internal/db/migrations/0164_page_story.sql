-- Plan stories (WP-190, WP-ADR122): a long plan an agent
-- wrote carries a short user story, and approving the plan keeps only the
-- story as the person's intent.
--   story        the story's Markdown: one "As …, I want …, so that …" line
--                and a few points.
--   story_agent  who wrote it when no person did: waypoint/story (the story
--                model) or an agent; '' once a person wrote or kept it.
--   story_model  the model that wrote a waypoint/story story.
--   story_by     the person who wrote or kept it.
--   story_at     when it was written or kept.
--   story_base   a hash of the body it was written or kept against, so a
--                later edit to the plan shows.
--   session_id, prompt_event  the agent session that created the page, and
--                the prompt its person typed last before it (session_events).
--   story_tried  when the story model was last asked for one (ms), so the
--                hourly sweep moves past plans whose story keeps failing.
--   reviewed_on_story  the last review approved the plan on its story, not
--                line by line: it had a story when it was reviewed.
ALTER TABLE pages ADD COLUMN story TEXT NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN story_agent TEXT NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN story_model TEXT NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN story_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE pages ADD COLUMN story_at INTEGER;
ALTER TABLE pages ADD COLUMN story_base TEXT NOT NULL DEFAULT '';
ALTER TABLE pages ADD COLUMN session_id INTEGER REFERENCES agent_sessions(id) ON DELETE SET NULL;
ALTER TABLE pages ADD COLUMN prompt_event INTEGER REFERENCES session_events(id) ON DELETE SET NULL;
ALTER TABLE pages ADD COLUMN story_tried INTEGER NOT NULL DEFAULT 0;
ALTER TABLE pages ADD COLUMN reviewed_on_story INTEGER NOT NULL DEFAULT 0;
