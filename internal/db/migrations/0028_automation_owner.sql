-- Whose runners an automation uses (WP-D18, phase 2). An automation runs jobs
-- only on the runners of the person who last saved it: anyone else editing it
-- could otherwise run commands on that person's machine. Existing rows keep
-- their creator as the saver.
ALTER TABLE automations ADD COLUMN saved_by INTEGER REFERENCES users(id) ON DELETE SET NULL;
UPDATE automations SET saved_by = created_by;

-- A runner job made by an automation step is found again by the step that made
-- it (run, step, iteration, attempt, model), so a step run twice after a wait
-- or a restart queues the job once.
ALTER TABLE runner_jobs ADD COLUMN step_key TEXT NOT NULL DEFAULT '';
CREATE INDEX runner_jobs_step ON runner_jobs(source, source_id, step_key) WHERE step_key != '';

-- A request an automation step made for a person is found again the same way
-- (ask_person, WP-D18): its key is in tool_use_id and starts with "auto:".
CREATE INDEX requests_auto_step ON requests(tool_use_id) WHERE tool_use_id LIKE 'auto:%';
