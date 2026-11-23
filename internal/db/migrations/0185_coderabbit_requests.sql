-- WP-1599: CodeRabbit reviews only the GitHub twins a mirror (forge-sync)
-- opens, and only when asked. Every project with a repo whose bots are on, on
-- a forge with a mirror account, asks it once per pull request: the bot
-- account comments "@coderabbitai review" on the Forgejo pull request and
-- forge-sync passes the command to the twin. The rule matches the "Ask
-- CodeRabbit to review" template in core.AutomationTemplates.
INSERT INTO automations(project_id, name, enabled, rule, position, created_at, updated_at)
SELECT p.id, 'Ask CodeRabbit to review', 1,
       '{"trigger":{"event":"pr_updated","on":"new"},"pr":{"skip_labels":["dependencies","autorelease: pending"],"skip_heads":["release-please--*"]},"actions":[{"type":"pr_comment","message":"@coderabbitai review"}]}',
       104, CAST(strftime('%s','now') AS INTEGER) * 1000, CAST(strftime('%s','now') AS INTEGER) * 1000
FROM projects p
WHERE EXISTS (SELECT 1 FROM project_repos r JOIN forges f ON f.id = r.forge_id
              WHERE r.project_id = p.id AND r.bots = 1 AND f.mirror_account != '')
  AND NOT EXISTS (SELECT 1 FROM automations a WHERE a.project_id = p.id AND a.name = 'Ask CodeRabbit to review');
