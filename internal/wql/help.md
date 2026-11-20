# Waypoint query language

A query is space-separated terms. Terms are ANDed. Free words search titles,
descriptions and comments: a word of three or more letters also matches as a
prefix (`proc` finds "process"), a shorter one only as itself (`PR` finds "PR"
and "PRs", not "project"). Matches in the title rank first. An issue key like
`WAY-12` matches that issue.

    assignee:me is:open priority:>=high "login page" -label:wontfix order:-updated

| Term | Meaning |
|---|---|
| `key:value` | filter |
| `-key:value` | exclude |
| `key:a,b` | either a or b |
| `key:"two words"` | quoted value |
| `key:>v` `>=` `<` `<=` `!=` | comparisons (priority, points, dates) |
| `order:field,-field` | sort; `-` for descending (`sort:` works too) |

## Filters

| Key | Values |
|---|---|
| `project:` | project key |
| `type:` | epic, story, task, bug, subtask |
| `status:` | status name, e.g. `status:"In Review"` |
| `category:` | status categories (backlog, todo, in_progress, done, canceled) or issue categories (feature, bugfix, improvement, refactor, docs, test, research, design, ops, security, chore), or none |
| `is:` | open, closed, done, canceled, backlog, todo, active, mine, unassigned, assigned, overdue, blocked, watching, shared, agent (delegated to agents), human (humans only), anyone (anyone may take it), agent-made, claimed, unreviewed (agent-written text or comments no person has reviewed), approved (a person wrote the task, or reviewed the agent's text), planned (a published plan names it, or its prep is ready), agent-closed (an agent moved it to Done or Won't Do), close-unreviewed (an agent closed it and no person has reviewed the close), held (unreviewed, and the safety screen held it for a person), draft (drafts; agents never see them), published, outside (text from outside waiting for a person; people only) |
| `risk:` | provenance risk (WP-554): low, medium, high; `risk:>=medium`. high: text from outside waiting for a person (people only), or held by the safety screen; medium: an agent wrote, commented on or closed it and no person has reviewed that, or an agent's task traces to no approved goal. `wp risk KEY` says why |
| `goal:` | how it traces to the project's goals (WP-382): none (to no approved goal), any, people (through links people made or confirmed), agent (through an agent's link), guess (only Waypoint's guess), proposed (to a goal no person approved), request (no goal, but a person asked for it), or a goal's number. `wp trace KEY` shows the route |
| `agent:` | none (humans only), anyone, any (any agent), or a model/family: `agent:claude`, `agent:"haiku 5.5"` |
| `step:` | a workflow step name, or none; `workflow:` a workflow name, or none |
| `has:` / `no:` | points, due, start, parent, children, sprint, assignee, label, link, description |
| `assignee:` | me, none, or usernames |
| `reporter:` | me or usernames |
| `watcher:` | me or usernames |
| `tag:` / `label:` | tag names, or none |
| `priority:` | none, low, medium, high, urgent (or 0-4) |
| `points:` | number, or none |
| `sprint:` | active, next, planned, open, none (backlog), a sprint name or id |
| `parent:` | an issue key (direct children), or none |
| `under:` / `epic:` | an issue key: everything beneath it, at any depth |
| `due:` / `start:` | dates: `2026-10-20`, `today`, `tomorrow`, `+7d`, `-2w`, `friday`, or none |
| `created:` / `updated:` / `resolved:` | dates as above; `updated:>=-7d` is the last week |
| `title:` | substring of the title |
| `link:` / `repo:` | linked URL substring or owner/repo (`repo:` also matches projects the repo is attached to) |
| `key:` | issue keys |

## Ordering

`order:` accepts rank (manual order), priority, updated, created, due, start,
points, key, title, status, resolved, type. Without `order:`, text searches
sort by relevance and others by most recently updated (boards and the backlog
use rank).

## Examples

- My open work, most urgent first: `assignee:me is:open order:-priority,due`
- Overdue in a project: `project:HOME is:overdue`
- Everything left in an epic: `under:WAY-3 is:open`
- Bugs fixed this week: `type:bug resolved:>=-7d`
- Unestimated work in the next sprint: `sprint:next no:points`
- Recently finished, newest first: `is:done order:-resolved`
