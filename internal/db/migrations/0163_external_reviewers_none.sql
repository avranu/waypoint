-- A repo that names no external reviewers (external_reviewers NULL) now
-- expects none on every forge kind (WP-1185). Before, a Forgejo repo's
-- default was the CodeQL status this site's scanner posts. Forgejo repos
-- attached then keep it, written out; one attached from now on has none
-- until an owner names some. Fresh databases have no repos, so nothing.
UPDATE project_repos SET external_reviewers = '["forge-checks/codeql"]'
WHERE external_reviewers IS NULL
  AND forge_id IN (SELECT id FROM forges WHERE kind = 'forgejo');
