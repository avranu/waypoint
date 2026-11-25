-- WP-1886: a tool a REPL script ran (the transcript record's model is
-- <synthetic>) is a tool call but not one the model typed. by_script = 1 marks
-- it.
-- WP-1883: shell_write says a Bash call wrote a file that is not scratch (see
-- transcript.ShellEdit): 1 yes, 0 no, NULL not known (every tool but Bash).
-- Rows stored before these columns read by_script 0 and shell_write NULL until
-- the file is re-derived (ParserVersion bump).
ALTER TABLE fact_tool_calls ADD COLUMN by_script INTEGER NOT NULL DEFAULT 0;
ALTER TABLE fact_tool_calls ADD COLUMN shell_write INTEGER;
