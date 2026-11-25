-- WP-1883: a Bash call that wrote a file that is not scratch (see
-- transcript.ShellEdit). 1 = it did, 0 = it did not, NULL = not known: rows
-- stored before this column, and every tool but Bash, until the file is
-- re-derived (ParserVersion bump).
ALTER TABLE fact_tool_calls ADD COLUMN shell_write INTEGER;
