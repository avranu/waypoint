-- WP-1886: a tool a REPL script ran (the transcript record's model is
-- <synthetic>) is a tool call but not one the model typed. by_script = 1 marks
-- it; rows stored before this column read 0 until the file is re-derived
-- (ParserVersion bump).
ALTER TABLE fact_tool_calls ADD COLUMN by_script INTEGER NOT NULL DEFAULT 0;
