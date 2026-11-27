-- WP-1883: shell_files lists the files of the work a Bash call wrote, one per
-- line (transcript.ShellSession.EditFiles; "dir/*" for a write naming none),
-- so session facts count them among the files changed. NULL when it wrote
-- none, for other tools, and for rows stored before this column until the
-- file is re-derived.
ALTER TABLE fact_tool_calls ADD COLUMN shell_files TEXT;
