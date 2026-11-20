-- A reply gives no verdict. Reply events carried the comment's verdict, so
-- readers took them for one (WP-763). old_verdict keeps what it was.
UPDATE review_comment_events SET verdict = '' WHERE replied = 1 AND old_verdict = verdict AND by_kind != 'forge';
