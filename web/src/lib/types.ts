// Types mirroring the Go API's JSON.

export type Category = 'backlog' | 'todo' | 'in_progress' | 'done' | 'canceled'
export type IssueType = 'epic' | 'story' | 'task' | 'bug' | 'subtask'
export type Role = 'owner' | 'editor' | 'viewer' | 'guest' | ''

export interface UserRef {
  id: number
  username: string
  display_name: string
  avatar_url?: string
}

export interface UserPrefs {
  theme?: '' | 'light' | 'dark'
  gotify_url?: string
  gotify_token?: string
  gotify_muted?: string[]
  push_muted?: string[]
  /** points of the week's quota in an hour that push an alert; unset 4, 0 off */
  quota_jump?: number
  week_start: number
  /** explainers this person folded away */
  hidden_help?: string[]
  /** explainers this person opened; the rest start folded */
  open_help?: string[]
  /** sidebar project order; empty means custom once project_order is set */
  project_sort?: '' | 'recent' | 'custom'
  /** project ids, first to last */
  project_order?: number[]
  /** how a session's prompt cache is kept warm: '' a short ping, 'compact' compacting it instead */
  cache_warm?: '' | 'compact'
  /** goal wording warnings this person waved off, "NOTE_ID:WARNING", newest last */
  dismissed_warnings?: string[]
  /** Claude Code settings for the sessions Waypoint starts for this person (WP-1265) */
  sessions?: SessionPrefs
}

/** One set of Claude Code session settings; an unset field leaves Claude Code's own default. */
export interface SessionSettings {
  remote_control?: boolean
  code_mode?: boolean
  chrome?: boolean
  model?: string
  effort?: '' | 'low' | 'medium' | 'high' | 'xhigh' | 'max'
  permission_mode?: '' | 'acceptEdits' | 'auto' | 'dontAsk' | 'manual' | 'plan'
}

/** Settings for every session, and per repo (owner/name) overrides of them field by field. */
export interface SessionPrefs {
  all: SessionSettings
  repos?: Record<string, SessionSettings>
}

export interface User extends UserRef {
  email: string
  is_admin: boolean
  disabled: boolean
  timezone: string
  prefs: UserPrefs
  created_at: string
}

export interface Status {
  id: number
  name: string
  category: Category
  color: string
  position: number
}

export interface Label {
  id: number
  name: string
  color: string
}

export interface Member {
  user: UserRef
  role: Role
}

/** When sessions are told of stalled background jobs (WP-1045). Times are minutes. */
export interface StallPolicy {
  /** sessions aren't told (the spells are still kept) */
  off: boolean
  /** jobs younger than this aren't told about */
  min_age: number
  /** no progress this long is a stall; the evidence may lengthen it per kind of job */
  quiet: number
  /** a job under a timeout shorter than this is left alone; 0: none */
  skip_timeout: number
  /** told this early in place of a keep-warm ping; 0: never */
  early: number
  /** a session whose cache is cold is woken after this; 0: never */
  cold_wake: number
  /** the evidence doesn't change these times */
  fixed: boolean
}

export interface ProjectSettings {
  mode: 'kanban' | 'sprints'
  sprint_days: number
  velocity_weeks: number
  default_points: number
  done_visible_days: number
  default_status_id: number
  auto_estimate?: boolean
  /** a model writes a short title for new issues whose title is long text (WP-67) */
  auto_title?: boolean
  /** runners and a free model prepare the front of the agent queue (WP-212) */
  auto_prep?: boolean
  auto_relate?: boolean
  /** an agent's or editor's pull request gets another model's audit; its findings are round work (WP-355) */
  audit_prs?: boolean
  /** this project owns the CI runners: runner faults are filed here as agent tasks (WP-589) */
  ci_runners?: boolean
  /** Agent setup sends this project's repos no shared-instructions pull requests (WP-ADR44) */
  no_shared_setup?: boolean
  /** Waypoint files no task when a kind of mistake keeps coming up here (WP-710) */
  no_recurring_checks?: boolean
  /** agents take no new work in the project (WP-D15) */
  agents_paused?: boolean
  /** where dev environments live, with {name}: https://{name}.dev.example.com (WP-D19) */
  dev_env_url?: string
  /** autopilot's wording here (WP-308); empty: every project's */
  autopilot_prompt?: string
  autopilot_done?: string
  /** when sessions are told of stalled background jobs; unset: every project's (WP-1045) */
  stalled_jobs?: StallPolicy | null
}

export interface Sprint {
  id: number
  project_id: number
  name: string
  goal: string
  state: 'planned' | 'active' | 'closed'
  start_date: string | null
  end_date: string | null
  started_at: string | null
  closed_at: string | null
  committed_points: number | null
  completed_points: number | null
  issue_count: number
  open_count: number
  points: number
  done_points: number
}

export interface Project {
  id: number
  key: string
  name: string
  description: string
  color: string
  icon: string
  background: string
  /** who chose the icon, color and background: nobody yet, the AI, a person, or reset to default */
  look: 'auto' | 'generated' | 'custom' | 'plain'
  archived: boolean
  settings: ProjectSettings
  /** nested projects: the parent's id */
  parent_id: number | null
  created_at: string
  updated_at: string
  role: Role
  open_issues: number
  /** project lists: the last change to the project or any of its issues */
  active_at?: string
  statuses?: Status[]
  labels?: Label[]
  members?: Member[]
  sprints?: Sprint[]
}

export interface IssueRef {
  id: number
  key: string
  title: string
  type: IssueType
}

export interface Issue {
  id: number
  key: string
  project_id: number
  project_key: string
  number: number
  type: IssueType
  title: string
  description?: string
  status: Status
  priority: number
  parent_id: number | null
  parent?: IssueRef
  sprint_id: number | null
  sprint_name?: string
  reporter?: UserRef
  assignees: UserRef[]
  labels: Label[]
  points: number | null
  start_date: string | null
  due_date: string | null
  rank: number
  child_count: number
  child_done: number
  created_at: string
  updated_at: string
  resolved_at: string | null
  /** agent spec: '' = humans only, 'any', or a family/model (WP-D7) */
  agent?: string
  /** set when an agent created the issue */
  reporter_agent?: string
  category?: string
  workflow_id?: number | null
  step?: string
  claimed_by?: string
  claimed_until?: string | null
  /** when the current claim began */
  claimed_at?: string | null
  /** seconds the agent worked on its claim before the stretch since claim_working_since */
  claim_worked?: number
  /** when the agent's current working stretch began; absent while it's idle (paused) */
  claim_working_since?: string | null
  /** provenance (WP-D11): the agent that wrote the title and description */
  text_agent?: string
  /** agent-written text no person has reviewed yet */
  unreviewed?: boolean
  /** the agent that closed it */
  resolved_agent?: string
  /** an agent closed it and no person has reviewed the close yet (WP-272) */
  close_unreviewed?: boolean
  /** its place in the order people set for agents; absent = automatic (WP-D15) */
  agent_order?: number
  agent_ahead?: number
  /** keys this issue blocks (list results) */
  blocks?: string[]
  /** a draft: agents can't see it until a person publishes it (WP-42) */
  draft?: boolean
  /** a draft because a forge user who isn't a Waypoint person wrote it (WP-279); approving it publishes it */
  outside?: boolean
  /** their forge login ('' when not known yet) */
  outside_by?: string
  /** the forge they wrote it on */
  outside_forge?: string
  /** on hold: no one takes it until it ends (WP-269); absent when it isn't, or it ran out */
  hold?: Hold
  /** the start of the description, on list results that don't carry it whole (WP-67) */
  excerpt?: string
}

/** A hold keeps a task from agents, wp next and taking it by name (WP-269). */
export interface Hold {
  at: string
  /** when it ends by itself; absent: when someone lifts it */
  until?: string
  /** what else it waits for, in words: "WP-1 is done and R-146 is answered" */
  waits?: string
  note?: string
  /** the person who placed it (an agent's person, when agent is set) */
  by?: string
  agent?: string
}

/** Kinds of relation between two issues, as one end reads it (WP-1036). */
export type RelationKind =
  | 'relates'
  | 'blocks'
  | 'blocked_by'
  | 'duplicates'
  | 'duplicated_by'
  | 'follows'
  | 'followed_by'
  | 'supersedes'
  | 'superseded_by'
  | 'tension'

export interface Relation {
  id: number
  kind: RelationKind
  issue: IssueRef
  state: Category
  /** narrows the kind: a follow-up that fixes the earlier work, or expands it */
  subkind?: 'fix' | 'expansion'
  /** How Waypoint knew a follow-up (WP-255); absent when someone chose it. */
  found?: 'session' | 'files' | 'after'
  /** who stands behind it; absent when it was made before Waypoint recorded that */
  basis?: 'person' | 'agent' | 'automation' | 'inferred'
  created_by?: UserRef
  created_agent?: string
  /** how sure Waypoint was, where it measured it (0 to 1) */
  score?: number
  created_at: string
}

/** A link worth adding, from the issues' own words (WP-296). */
export interface RelationSuggestion {
  key: string
  title: string
  kind: RelationKind
  confidence: 'low' | 'medium' | 'high'
  reason: string
}

export interface ExternalLink {
  id: number
  provider: 'github' | 'forgejo' | 'url'
  kind: 'issue' | 'pull' | 'commit' | 'url'
  url: string
  repo: string
  number: number | null
  title: string
  state: string
  synced_at: string | null
  /** how a forge link was made: 'added' by hand or wp ship, or the place in
   * the item's text that named the issue as its work (WP-379) */
  via?: 'added' | 'title' | 'branch' | 'body' | 'subject' | 'message'
}

/** A forge item naming an issue without being its work (WP-379). */
export interface ForgeMention {
  kind: 'pull' | 'commit' | 'issue'
  url: string
  repo: string
  number?: number
  title: string
  state: string
  /** where it names the issue: title, branch, body, subject, message, comment, review */
  places: string[]
  /** the relationship it states: follows, part_of, relates, depends_on, ... */
  verb?: string
  /** the line naming it; empty for text from outside */
  excerpt?: string
  source_url: string
  linked?: boolean
  own?: boolean
  gone?: boolean
  at: string
}

export interface Share {
  user: UserRef
  role: 'editor' | 'viewer'
}

/** Who took a reading: only Waypoint's own (runner, judge, baseline) decide a round. */
export type ObjectiveSource = 'agent' | 'runner' | 'judge' | 'baseline'

/** One reading of an objective's check, or a judge's audit (WP-927). */
export interface ObjectiveCheck {
  id: number
  round: number
  at: string
  ok: boolean
  value?: number
  base_ok?: boolean
  base_value?: number
  base_sha?: string
  tree?: string
  commit?: string
  runs?: number
  low?: number
  high?: number
  p?: number
  moved?: boolean
  met: boolean
  improved: boolean
  agent?: string
  /** who took it */
  source: ObjectiveSource
  /** whether it is one of Waypoint's own, of the objective as it stands */
  decides: boolean
  exact?: boolean
  /** files the round changed that the check's own commands name */
  touched?: string[]
  /** the keep-passing commands that no longer pass */
  broken?: string[]
  /** what the command last printed when it gave no reading, or the judge's summary */
  note?: string
  job_id?: number
  audit_id?: number
  /** in words: "1.42s (base 2.1s, 32% better)" */
  line: string
}

export type ObjectiveRoundOutcome =
  'reading' | 'met' | 'progress' | 'worked' | 'none' | 'empty' | 'unmeasured' | 'loop'

/** A round of an objective. */
export interface ObjectiveRound {
  round: number
  started_at: string
  ended_at: string
  outcome: ObjectiveRoundOutcome
  check?: ObjectiveCheck
  agent?: string
  pr_url?: string
  base_sha?: string
  head_sha?: string
  note?: string
}

/** One thing a judge found. */
export interface ObjectiveFinding {
  id: string
  severity: 'critical' | 'high' | 'medium' | 'low'
  title: string
  path?: string
  line?: number
  problem?: string
}

export type ObjectiveKind = 'until' | 'each' | 'judged' | 'loop'
export type ObjectiveHealth =
  'proposed' | 'working' | 'ready' | 'waiting' | 'reading' | 'blocked' | 'stuck' | 'met' | 'ended'
/** Where one in effect stands: '' a round is worked or waited for. */
export type ObjectivePhase = '' | 'baseline' | 'verdict' | 'blocked'
export type ObjectiveBlockedWhy = 'repo' | 'person' | 'check' | 'touched' | 'reading' | 'rounds'

/**
 * A task's objective (WP-927): worked in rounds until a check is met
 * (until), by an amount every round (each), until a judge finds nothing
 * (judged), or as a loop with no check. Waypoint reads every round itself.
 */
export interface Objective {
  key: string
  title?: string
  kind: ObjectiveKind
  check?: string
  dir?: string
  setup?: string
  measure?: 'time' | 'value' | 'pass' | 'judge'
  until?: string
  op?: string
  target?: number
  factor?: number
  better?: 'lower' | 'higher'
  unit?: string
  /** the repository (owner/name) it is read in, and the branch ('': its default) */
  repo?: string
  branch?: string
  /** commands that must keep passing */
  keep_passing: string[]
  /** what done means, for a judge model */
  judge?: string
  /** the lowest severity of finding that keeps the task open */
  judge_bar?: 'critical' | 'high' | 'medium' | 'low'
  judge_models: string[]
  /** queue: handed out like any task; spare: only on quota that would go unused */
  pace: 'queue' | 'spare'
  /** least seconds from one round's end to the next's start */
  every: number
  max_rounds?: number
  round: number
  /** rounds in a row that moved nothing */
  stalled?: number
  next_at?: string
  /** proposed: an agent's, not in effect until a person confirms it */
  state: 'proposed' | 'active' | 'met' | 'ended'
  phase?: ObjectivePhase
  phase_at?: string
  phase_why?: ObjectiveBlockedWhy | ''
  phase_note?: string
  /** where it stands, in a word and a sentence */
  health: ObjectiveHealth
  health_note?: string
  stuck_since?: string
  stuck_why?: string
  base_sha?: string
  /** the objective in a sentence */
  summary: string
  /** names what a confirm confirms */
  rev: string
  /** the whole of it, as `wp show` prints it */
  text: string
  set_by?: UserRef
  set_agent?: string
  confirmed_by?: UserRef
  confirmed_at?: string
  ended_at?: string
  ended_why?: string
  updated_at: string
  /** the newest reading of any source */
  last?: ObjectiveCheck
  /** the newest that decides: Waypoint's own */
  verdict?: ObjectiveCheck
  /** the newest few, newest first, the worker's own included */
  checks: ObjectiveCheck[]
  /** the last rounds, oldest first */
  rounds: ObjectiveRound[]
  /** what the judge last found at or above the bar */
  findings?: ObjectiveFinding[]
}

/** What setting an objective sends; a field left out keeps what is there. */
export interface ObjectiveInput {
  check?: string
  dir?: string
  setup?: string
  until?: string
  measure?: string
  better?: string
  pace?: 'queue' | 'spare'
  /** a duration: "0", "90m", "24h", "3d" */
  every?: string
  max_rounds?: number
  repo?: string
  branch?: string
  /** replaces the commands that must keep passing */
  keep_passing?: string[]
  /** what done means; '' takes the judge off */
  judge?: string
  judge_bar?: string
  judge_models?: string[]
  /** the proposal the person read, when their change builds on it */
  rev?: string
}

/** What a person steers an objective in effect with. */
export type ObjectiveAction = 'retry' | 'next' | 'accept' | 'end'

export interface IssueDetail extends Issue {
  role: Role
  /** what it's for, in a sentence or two, apart from the full description (WP-1196) */
  intent?: string
  /** the agent that suggested the intent, until a person keeps or rewrites it */
  intent_agent?: string
  /** its objective, when it is worked in rounds (WP-927) */
  objective?: Objective
  ancestors: IssueRef[]
  children: Issue[]
  relations: Relation[]
  links: ExternalLink[]
  /** forge items naming it that aren't linked (WP-379) */
  forge_mentions?: ForgeMention[]
  watchers: UserRef[]
  shares: Share[]
  watching: boolean
  time_spent: number
  attachments: Attachment[]
  agent_runs?: AgentRun[]
  pages?: PageRef[]
  /** whose token wrote the text last; the person who reviewed agent text; who closed it */
  text_by?: UserRef
  reviewed_by?: UserRef
  reviewed_at?: string
  resolved_by?: UserRef
  /** the person who reviewed an agent's close (WP-272) */
  close_reviewed_by?: UserRef
  close_reviewed_at?: string
  /** the safety screen of agent text no person reviewed (WP-ADR10) */
  safety?: TaskSafety
  /** how it traces to a goal, when an approved goal covers its project (WP-382) */
  goal?: IssueGoal
  /** an agent filed it while working in another repo (WP-378) */
  filed_from?: FiledFrom
  /** how far to trust it, from who wrote and checked it (WP-554) */
  risk?: ProvenanceRisk
  /** what was live when it was written, and what is now (WP-460) */
  written?: Written
  /** the tasks an agent wrote its text while working on (WP-275) */
  lineage?: Lineage
}

/** The chain of tasks an issue's text was written while working on, nearest first (WP-275). */
export interface Lineage {
  /** 0: a person wrote or reviewed it; else the agent-written, unreviewed tasks in the chain, this one included */
  depth: number
  chain: LineageLink[]
  /** deep enough that the safety screen holds it for a person */
  held?: boolean
}

export interface LineageLink {
  /** absent for a task the viewer can't open (hidden) or one since deleted (gone) */
  key?: string
  title?: string
  /** the agent that wrote it, when no person has reviewed it */
  agent?: string
  /** an agent wrote it and a person reviewed it */
  reviewed?: boolean
  hidden?: boolean
  gone?: boolean
  depth: number
}

/** What a project's production (or staging) environment ran when an issue was written, and runs now. */
export interface Written {
  /** absent once the environment is deleted */
  environment?: string
  version?: string
  commit?: string
  live_version?: string
  live_commit?: string
  /** other versions deployed since it was written */
  releases_since: number
}

/** An object's provenance risk (WP-554): its level and the factors behind it. */
export interface ProvenanceRisk {
  level: 'low' | 'medium' | 'high'
  reasons?: RiskReason[]
}

export interface RiskReason {
  factor: string
  level: 'low' | 'medium' | 'high'
  text: string
  /** what the reason is about when it isn't the object itself: a pull request's task */
  ref?: string
}

/** Where a task an agent filed from another repo came from, and whether it was declined. */
export interface FiledFrom {
  project?: string
  repo?: string
  agent?: string
  here?: boolean
  declined_at?: string
  declined_by?: string
  declined?: string
}

/** What the safety screen says about agent-written text before agents take it. */
export interface TaskSafety {
  verdict: 'pending' | 'clear' | 'hold'
  flags?: string[]
  rules?: string[]
  /** what a person must check */
  reason?: string
  model?: string
  at: string
  current: boolean
}

/** One claim of an issue by an agent. */
export interface AgentRun {
  id: number
  agent: string
  step?: string
  started_at: string
  ended_at: string | null
  seconds: number
  outcome?: '' | 'done' | 'released' | 'expired' | 'paused' | 'timeout'
  input_tokens: number
  output_tokens: number
  cost_usd: number
  /** The model that did the work, when tooling reported it. */
  model?: string
}

export interface PageRef {
  id: number
  /** PL13: the category's prefix and the page's number in its project */
  code?: string
  title: string
  category?: string
}

export interface Page {
  id: number
  code?: string
  /** codes it had before (another category or project) that still lead here */
  other_codes?: string[]
  parent_id: number | null
  project_id: number | null
  project_key?: string
  title: string
  body?: string
  category?: string
  position: number
  owner?: UserRef
  child_count: number
  created_at: string
  updated_at: string
  role?: Role
  ancestors?: PageRef[]
  children?: PageRef[]
  issues?: IssueRef[]
  /** provenance (WP-D11) */
  created_agent?: string
  text_by?: UserRef
  text_agent?: string
  unreviewed?: boolean
  reviewed_by?: UserRef
  reviewed_at?: string
  /** a draft: agents can't see it until a person publishes it (WP-42) */
  draft?: boolean
  /**
   * The plan's user story (WP-190): read first, and the only part of an
   * agent's plan a person's approval records as their intent. story_agent
   * is set while it is the story model's suggestion; "" once a person wrote
   * or kept it.
   */
  story?: string
  story_agent?: string
  /** the model that wrote it (stays when a person keeps it unchanged) */
  story_model?: string
  story_by?: UserRef
  story_at?: string
  /** the plan changed after the story was written or kept */
  story_stale?: boolean
  /** the review approved the plan on its story, not line by line */
  reviewed_on_story?: boolean
  /** the agent session that created the page */
  session_id?: number
  /** what its person typed last before it (to them and admins only) */
  prompt?: PagePrompt
}

export interface PagePrompt {
  text: string
  at: string
  session_id: number
}

export interface WorkflowStep {
  name: string
  /** an agent spec, or "different"/"same" relative to the previous step */
  agent?: string
  instructions?: string
}

export interface Workflow {
  id: number
  project_id: number | null
  name: string
  steps: WorkflowStep[]
  created_at: string
}

/** A file on an issue, page, design note, comment, request or session (WP-D16). */
export interface Attachment {
  id: number
  target_type: 'issue' | 'page' | 'design' | 'comment' | 'request' | 'session'
  target_id: number
  /** set for files on an issue or one of its comments */
  issue_id?: number
  name: string
  content_type: string
  size: number
  caption?: string
  url: string
  /** how it shows: in place (image, video), as a text preview, or a download */
  kind: 'image' | 'video' | 'text' | 'file'
  preview?: 'markdown' | 'json' | 'csv' | 'text'
  inline: boolean
  uploader?: UserRef
  /** the agent that uploaded it, if one did */
  agent?: string
  /** a screenshot the agent took for whoever reviews its work (wp shot) */
  review?: boolean
  created_at: string
  /** Markdown that embeds it */
  markdown: string
}

export interface Comment {
  id: number
  issue_id: number
  /** id 0: someone outside Waypoint, e.g. a GitHub user */
  author: UserRef | null
  body: string
  /** where an external comment came from, e.g. "GitHub" */
  via?: string
  /** the agent that wrote it through the author's token */
  agent?: string
  unreviewed?: boolean
  reviewed_by?: UserRef
  /** files attached to the comment itself (WP-D16) */
  attachments?: Attachment[]
  created_at: string
  updated_at: string
}

export interface WEvent {
  id?: number
  kind: string
  project_id?: number
  issue_id?: number
  issue_key?: string
  sprint_id?: number
  actor?: UserRef
  field?: string
  old?: string
  new?: string
  data?: Record<string, unknown>
  at: string
  via_automation?: string
}

export interface SearchResult {
  issues: Issue[]
  total: number
  limit: number
  offset: number
}

export interface BoardColumn {
  status: Status
  issues: Issue[]
  points: number
}

export interface Board {
  mode: 'kanban' | 'sprints'
  sprint: Sprint | null
  columns: BoardColumn[]
}

export interface Backlog {
  mode: 'kanban' | 'sprints'
  sprints: { sprint: Sprint; issues: Issue[] }[]
  backlog: Issue[]
  epics: Issue[]
}

export interface TimeEntry {
  id: number
  user: UserRef
  issue: IssueRef | null
  project_id: number | null
  project_key?: string
  note: string
  /** the agent tracking this time for its person */
  agent?: string
  started_at: string
  ended_at: string | null
  seconds: number
  running: boolean
}

export interface Notification {
  id: number
  kind: string
  title: string
  body: string
  issue_key?: string
  actor?: UserRef
  read_at: string | null
  created_at: string
  url?: string
}

/** Everything about one thing in the Inbox: one row per issue, request or kind of alert (WP-1141). */
export interface InboxGroup {
  key: string
  kind: string
  kinds: Record<string, number>
  tier: 'you' | 'alert' | 'update'
  issue_key?: string
  url?: string
  title: string
  count: number
  unread: number
  latest_at: string
  oldest_at: string
  reason?: string
  actors?: UserRef[]
  items: Notification[]
}

export interface InboxSummary {
  live_unread: number
  live_groups: number
  stale_unread: number
  by_kind: Record<string, number>
  oldest_live?: string
  unread: number
  /** Newest notification id the view saw; mark-read sends it back as `upto`. */
  as_of: number
  /** More than the server scans: the oldest unread are not counted. */
  truncated: boolean
  live_more: number
  stale_more: number
}

export interface InboxView {
  summary: InboxSummary
  live: InboxGroup[]
  stale: InboxGroup[]
  earlier: InboxGroup[]
}

export interface View {
  id: number
  project_id: number | null
  project_key?: string
  name: string
  query: string
  layout: 'list' | 'board'
  pinned: boolean
  position: number
  created_at: string
}

export interface Trigger {
  event: string
  from?: string
  to?: string
  field?: string
  /** due_soon: days before the due date; stale: days without changes */
  days?: number
  every?: 'day' | 'weekday' | 'week' | 'month' | 'cron'
  at?: string
  weekday?: number
  month_day?: number
  /** pr_command: review (also matches "full review"); empty is any command */
  command?: string
  /** The settings of every other trigger, by the names the catalog gives them. */
  [param: string]: unknown
}

/** One model of a chain: tried in order until one answers. Model may be a
 * family pattern like opencode/muse-spark-*. With `cli` it is a coding agent
 * on the rule owner's runner instead (provider_id 0; model optional). */
export interface ChainModel {
  provider_id: number
  model: string
  cli?: string
  /** costs money: asked on its own, only after the entries before it failed */
  paid?: boolean
}

/** Narrows a pull request rule; the empty filter acts on every ready PR by a person. */
export interface PRFilter {
  /** drafts too */
  drafts?: boolean
  /** pull requests opened by bots too */
  bots?: boolean
  /** target branches (globs); empty is any */
  bases?: string[]
  skip_labels?: string[]
  /** head branch globs */
  skip_heads?: string[]
  /** only these forge accounts; * is any run of characters (`*[bot]`, `renovate*`); lets bots in */
  authors?: string[]
  skip_authors?: string[]
  /** only pull requests with every one of these labels */
  labels?: string[]
  /** path globs: only pull requests touching one of them */
  paths?: string[]
  /** path globs: leave out pull requests whose every file matches one (docs only) */
  skip_paths?: string[]
  /** lines added plus deleted */
  min_changed_lines?: number
  max_changed_lines?: number
  /** repository full names (globs) of the project's */
  repos?: string[]
}

export interface Action {
  type: string
  /** Names the step so later steps can use its outputs: {{.Steps.id.Text}}. */
  id?: string
  /** A template that must render true for the step to run. */
  only_if?: string
  on_error?: 'stop' | 'continue' | 'retry'
  retries?: number
  retry_delay?: string
  /** A repeat group's steps. */
  steps?: Action[]
  status?: string
  field?: string
  value?: string
  user?: string
  title?: string
  message?: string
  url?: string
  token?: string
  priority?: number
  titles?: string[]
  body?: string
  template?: string
  issue_type?: string
  labels?: string[]
  due?: string
  /** review: models to try in order; empty uses Admin → AI's review models */
  models?: ChainModel[]
  /** review: extra instructions */
  focus?: string
  /** review: even at a head already reviewed */
  force?: boolean
  max_findings?: number
  /** The settings of every other action, by the names the catalog gives them. */
  [param: string]: unknown
}

/** A value a manual run asks for: {{.Inputs.name}}. */
export interface RuleInput {
  name: string
  label?: string
  type: 'text' | 'number' | 'bool' | 'select'
  default?: string
  options?: string[]
  required?: boolean
}

export type Concurrency = 'parallel' | 'skip' | 'queue'

export interface Rule {
  trigger: Trigger
  if?: string
  /** pull request triggers only */
  pr?: PRFilter
  /** The steps. The key stays "actions" so rules saved before steps existed load unchanged. */
  actions: Action[]
  inputs?: RuleInput[]
  concurrency?: Concurrency
  /** IANA name; where schedules run. Empty is the project owner's. */
  time_zone?: string
  /** Agents may run a manual automation only when this is on. */
  allow_agents?: boolean
}

/** A pull (merge) request of an attached repo. */
export interface PullRequest {
  id: number
  forge_id: number
  repo: string
  number: number
  title: string
  author: string
  url: string
  base_ref: string
  head_ref: string
  head_sha: string
  state: 'open' | 'draft' | 'closed' | 'merged'
  labels: string[]
  bots_paused: boolean
  updated_at: string
}

export interface ModelAttempt {
  model: string
  outcome:
    | 'ok'
    | 'rate_limit'
    | 'timeout'
    | 'server'
    | 'auth'
    | 'unavailable'
    | 'bad_output'
    | 'error'
    | 'cancelled'
  error?: string
  ms: number
}

export interface BotFinding {
  id: number
  path: string
  line: number | null
  severity: 'critical' | 'high' | 'medium' | 'low'
  claim: string
  model: string
  placement: 'inline' | 'body' | 'withheld'
  /** resolved: a person resolved its thread on the forge; dismissed: a person or an agent said it is wrong. Only open ones block the review gate. */
  state: 'open' | 'fixed' | 'withdrawn' | 'dismissed' | 'resolved' | 'dropped'
  /** how many reviewers raised it (a review pipeline's; 1 for one reviewer) */
  agreed?: number
  /** the model that checked it against the code before it was posted */
  verified_by?: string
  /** the prompt version and context strategy of the run that raised it */
  prompt_version?: string
  strategy?: string
  /** the verifier's verdict: kept, corrected, dropped, unchecked or skipped */
  verify?: string
  verify_reason?: string
}

/** One job a bot did or will do on a pull request. */
export interface BotRun {
  id: number
  project_id: number
  automation_id: number | null
  automation?: string
  pr: PullRequest
  kind: string
  head_sha: string
  reason: string
  status: 'queued' | 'running' | 'done' | 'failed' | 'skipped' | 'cancelled'
  attempts: number
  summary: string
  error?: string
  log: ModelAttempt[]
  review_url?: string
  findings?: BotFinding[]
  started_at: string | null
  finished_at: string | null
  created_at: string
  /** what the review was made of (absent on runs before it was recorded) */
  prompt_version?: string
  strategy?: string
  /** the first reviewer's arm (WP-365), e.g. "diff/none" */
  arm?: string
  verified?: boolean
}

export interface Automation {
  id: number
  /** 0 for an admin automation */
  project_id: number
  admin?: boolean
  name: string
  enabled: boolean
  rule: Rule
  run_count: number
  last_run_at: string | null
  last_error: string
  created_at: string
  updated_at: string
  /** The secret address of an incoming-webhook automation; owners only. */
  hook_url?: string
  /** Its runs are carried by a background worker (long steps). */
  durable?: boolean
  /** Who last saved it: the person whose runners its agent steps use. */
  saved_by?: UserRef | null
  /** Said when a step runs on a runner. */
  runners?: { owner: UserRef | null; total: number; online: number }
  /** The agent that wrote its rule, until a person saves it (WP-560). */
  rule_agent?: string
  /** Its provenance risk, when it isn't low (WP-ADR41). */
  risk?: ProvenanceRisk
}

export interface AutomationTemplate {
  name: string
  description: string
  rule: Rule
  default: boolean
  /** Only for Admin → Automations. */
  admin?: boolean
  /** Added switched off: it needs a setting only the person can give. */
  off?: boolean
}

export interface Webhook {
  id: number
  project_id: number | null
  name: string
  url: string
  has_secret: boolean
  events: string
  enabled: boolean
  last_status: number | null
  last_error: string
  created_at: string
}

export type ForgeKind = 'github' | 'forgejo' | 'gitlab'

export interface Forge {
  id: number
  kind: ForgeKind
  name: string
  base_url: string
  auto_projects?: boolean
  has_token: boolean
  has_webhook_secret: boolean
  /** bots post with a separate account's token */
  has_bot_token?: boolean
  /** paths a mirror keeps off the forge it copies these repos to (globs); empty: nothing mirrors it */
  mirror_private?: string[] | null
  /** the login that mirror posts its copies of the other forge's reviews as; only its copies count as the copied reviewer's */
  mirror_account?: string
  webhook_url: string
  created_at: string
  /** its logins that are Waypoint people or agents' accounts; anyone else's text there is from outside (WP-279) */
  people?: ForgePerson[] | null
}

export interface ForgePerson {
  login: string
  /** null: an agent's own account, whose text is agent text (WP-283) */
  user: UserRef | null
  /** No agent holds its credentials: its text is the person's own (WP-282). */
  person_only: boolean
}

/** Text from outside waiting for a person to read and approve (WP-279). */
export interface OutsideText {
  id: number
  issue_key: string
  kind: 'comment' | 'comment_edit' | 'issue_edit'
  /** the forge login ('' when the forge didn't say) */
  author: string
  /** GitHub, Forgejo, GitLab */
  via: string
  forge: string
  url?: string
  /** issue_edit: the new title */
  title?: string
  body: string
  /** comment_edit: the approved text it would replace */
  was?: string
  created_at: string
  updated_at: string
}

/** A repo discovery would attach (GET /forges/{id}/discover). */
export interface DiscoveredRepo {
  repo: string
  /** the organisation or group whose project it nests under */
  namespace?: string
  /** that organisation's project is new too */
  new_namespace?: boolean
  /** joins this project as a mirror, making none */
  joins?: string
}

export interface ProjectRepo {
  id: number
  forge_id: number
  forge_name: string
  repo: string
  sync_issues: boolean
  /** Waypoint comments on synced issues are posted to the forge's issue */
  post_comments: boolean
  /** automations act on its pull requests */
  bots: boolean
  /** Waypoint posts a waypoint/review status that fails while findings at or above the severity are open */
  review_gate: boolean
  gate_severity: 'critical' | 'high' | 'medium' | 'low'
  /** Waypoint deletes the repo's merged branches on the forge (a daily sweep) */
  delete_merged_branches: boolean
  /** Merge automatically: every open, non-draft pull request is armed to merge when ready */
  auto_merge: boolean
  /**
   * Which base branches Merge automatically arms pull requests into: any,
   * the default branch, every branch but the default, or ones matching
   * auto_merge_bases (globs)
   */
  auto_merge_into?: 'any' | 'default' | 'others' | 'match'
  auto_merge_bases?: string[]
  /** The repo's default branch, when auto_merge_into turns on it and the forge said */
  default_branch?: string
  /** Status contexts that must have passed on the head */
  required_checks: string[]
  /** How long expected reviewers get after a push, in minutes */
  review_wait_minutes: number
  /** How long nothing new must be posted before an armed merge, in minutes */
  quiet_minutes: number
  /** Bots outside Waypoint whose review is expected: logins, or status contexts */
  external_reviewers: string[]
  last_synced_at: string | null
  /**
   * The forge webhook Waypoint adds; '' for repos attached before it did,
   * covered when an organisation webhook brings its events instead
   */
  hook_state: '' | 'pending' | 'installed' | 'failed' | 'covered'
  /** Why it failed, or a warning about an installed one */
  hook_note?: string
  hook_checked_at: string | null
  /** The last signed delivery from the forge about this repo */
  hook_seen_at: string | null
  /** The organisation whose webhook brings this repo's events */
  covered_by?: string
}

/** A webhook Waypoint added on a forge organisation (GitLab: group) */
export interface OrgHook {
  id: number
  forge_id: number
  org: string
  state: 'pending' | 'installed' | 'failed'
  note?: string
  checked_at: string | null
  /** The last signed delivery from a repo in it */
  seen_at: string | null
}

/** One entry in a webhook's delivery log */
export interface WebhookDelivery {
  id: number
  delivery: string
  event: string
  attempts: number
  /** HTTP status of the last attempt; null when nothing answered */
  status: number | null
  error: string
  duration_ms: number
  /** The start of the receiver's answer */
  response: string
  /** false when the body was too big to keep */
  replayable: boolean
  replay_of: number | null
  created_at: string
}

export interface APIToken {
  id: number
  name: string
  prefix: string
  read_only: boolean
  /** null: not restricted (full access, or read-only when read_only is set) */
  permissions: string[] | null
  /** recent requests this token was refused, per permission (permission tokens only) */
  refusals?: TokenRefusal[]
  agent?: string
  created_at: string
  last_used_at: string | null
  expires_at: string | null
}

export interface TokenRefusal {
  permission: string
  count: number
  last_at: string
  /** e.g. "POST /api/v1/agents/start/{key}" or "MCP tool run_automation" */
  what: string
}

export type PermissionRisk = 'safe' | 'careful' | 'dangerous'

export interface TokenPermission {
  key: string
  label: string
  description: string
  risk: PermissionRisk
}

export interface TokenPermissionCatalog {
  groups: { key: string; label: string; permissions: TokenPermission[] }[]
  presets: { key: string; label: string; description: string; permissions: string[] }[]
}

export interface UserLink {
  kind: 'invite' | 'reset'
  email?: string
  username?: string
  is_admin: boolean
  expires_at: string
  url?: string
}

export interface PendingInvite {
  id: string
  email: string
  is_admin: boolean
  created_at: string
  expires_at: string
}

export interface BurndownPoint {
  date: string
  remaining_points: number | null
  remaining_issues: number | null
  ideal_points: number
}

export interface Burndown {
  sprint: Sprint
  total_points: number
  days: BurndownPoint[]
}

export interface VelocityEntry {
  sprint_id: number
  name: string
  committed: number
  completed: number
  closed_at: string
}

export interface WeekStat {
  week: string
  created_issues: number
  created_points: number
  completed_issues: number
  completed_points: number
  scope_points: number
  done_points: number
  scope_issues: number
  done_issues: number
}

export interface FlowDay {
  date: string
  counts: Partial<Record<Category, number>>
}

export interface Forecast {
  basis: 'points' | 'issues'
  remaining: number
  remaining_issues: number
  unestimated: number
  assumed_points: number
  weekly_throughput: number[]
  average_per_week: number
  p50?: string
  p85?: string
  p95?: string
  message?: string
}

export interface EpicProgress {
  epic: Issue
  total: number
  done: number
  points: number
  done_points: number
  forecast: Forecast
  sequential_p85?: string
}

export interface TimeBucket {
  key: string
  label: string
  seconds: number
}

export interface TimeReport {
  total_seconds: number
  by_day: TimeBucket[]
  by_project: TimeBucket[]
  by_issue: TimeBucket[]
  by_user?: TimeBucket[]
  /** agents' time, from their runs; kept apart from people's (not in total_seconds) */
  agent_seconds: number
  by_agent: TimeBucket[]
}

export const PRIORITIES = ['none', 'low', 'medium', 'high', 'urgent'] as const
export const ISSUE_TYPES: IssueType[] = ['epic', 'story', 'task', 'bug', 'subtask']
export const CATEGORIES: Category[] = ['backlog', 'todo', 'in_progress', 'done', 'canceled']

export const isClosed = (c: Category) => c === 'done' || c === 'canceled'

/** Kinds of work (issue.category); separate from type. */
export const ISSUE_CATEGORIES = [
  'feature',
  'bugfix',
  'improvement',
  'refactor',
  'docs',
  'test',
  'research',
  'design',
  'ops',
  'security',
  'chore',
] as const

export const PAGE_CATEGORIES = [
  'plan',
  'idea',
  'experiment',
  'reference',
  'note',
  'decision',
  'problem',
  'feature',
  'bug',
] as const

/** A signed-in browser session (GET /me/sessions). */
export interface Session {
  id: string
  created_at: string
  last_seen_at: string
  user_agent: string
  ip: string
  current: boolean
}

// ---- knowledge: glossary, design notes, links (WP-D10) ----

/** Where a glossary entry or design note lives: a project, or global (project_id null). */
export interface KScope {
  project_id: number | null
  project_key?: string
  project_name?: string
}

export interface GlossaryEntry extends KScope {
  id: number
  term: string
  aliases: string[]
  /** other names for it this project doesn't use */
  avoid?: string[]
  definition: string
  locked: boolean
  locked_by?: UserRef
  locked_at?: string
  created_by?: UserRef
  created_agent?: string
  created_at: string
  updated_by?: UserRef
  updated_agent?: string
  updated_at: string
  /** an agent wrote it as it stands and no person has reviewed it */
  unreviewed?: boolean
  reviewed_by?: UserRef
  /** in a project's full glossary: from an ancestor project or global */
  inherited?: boolean
  /** redefined in a nearer scope */
  shadowed?: boolean
  can_edit: boolean
  /** search matches, ⟦marked⟧ */
  snippet?: string
  /** lookups and places naming it in the last 90 days (WP-D15) */
  uses?: number
  last_used_at?: string
  links?: KLink[]
  history?: GlossaryRevision[]
}

export interface GlossaryRevision {
  id: number
  term: string
  aliases: string[]
  avoid?: string[]
  definition: string
  locked: boolean
  user?: UserRef
  agent?: string
  at: string
}

export interface GlossaryLookup {
  term: string
  matches: GlossaryEntry[]
  suggestions: GlossaryEntry[]
  /** entries that avoid the word looked up: use their term */
  instead?: GlossaryEntry[]
}

export const DESIGN_KINDS = [
  'goal',
  'principle',
  'feature',
  'screen',
  'decision',
  'requirement',
  'note',
] as const
export type DesignKind = (typeof DESIGN_KINDS)[number]

/** superseded and retired count as archived */
export const DESIGN_STATUSES = ['active', 'superseded', 'retired'] as const
export type DesignStatus = (typeof DESIGN_STATUSES)[number]

export interface DesignRef {
  id: number
  title: string
  kind: DesignKind
  status: DesignStatus
  approved: boolean
  project_key?: string
  code?: string
}

export interface DesignNote extends KScope {
  id: number
  /** A decision's ADR code (ADR7), numbered in its project. */
  code?: string
  parent_id: number | null
  kind: DesignKind
  title: string
  body?: string
  status: DesignStatus
  approved: boolean
  approved_by?: UserRef
  /** Counts on a person's request: the task they filed (WP-189). Nobody approved it (WP-409). */
  requested?: { issue: string; by?: UserRef }
  approved_at?: string
  created_by?: UserRef
  created_agent?: string
  created_at: string
  updated_by?: UserRef
  updated_agent?: string
  updated_at: string
  /** additions and edits waiting for a person */
  pending: number
  child_count: number
  inherited?: boolean
  /** may change the title and body directly */
  can_edit: boolean
  can_propose: boolean
  /** a person with edit rights */
  can_approve: boolean
  snippet?: string
  notice?: string
  ancestors?: DesignRef[]
  children?: DesignRef[]
  additions?: DesignRevision[]
  proposals?: DesignRevision[]
  history?: DesignRevision[]
  links?: KLink[]
}

export type RevisionState = 'approved' | 'pending' | 'rejected' | 'merged' | 'applied'

export interface DesignRevision {
  id: number
  note_id: number
  kind: 'edit' | 'addition'
  title?: string
  body: string
  base_id?: number
  /** an edit proposed against an older version */
  stale?: boolean
  state: RevisionState
  author?: UserRef
  agent?: string
  created_at: string
  reviewed_by?: UserRef
  reviewed_at?: string
  review_note?: string
}

/** Something waiting for a person: a proposed note (no revision) or a pending addition or edit. */
export interface ReviewItem {
  note: DesignRef
  revision?: DesignRevision
  agent?: string
  created_at: string
  /** The note's version, sent back when approving it. */
  note_updated_at: string
}

export interface SpecRelated {
  rel: string
  direction: 'in' | 'out'
  note: DesignRef
  summary: string
}

export interface SpecTerm {
  id: number
  term: string
  definition: string
  locked: boolean
}

export interface Spec {
  topic: string
  note: DesignNote
  parts: DesignNote[]
  related: SpecRelated[]
  terms: SpecTerm[]
  issues: KItem[]
  /** some PRs and commits linked to the note or its issues (not every one) */
  changes: KItem[]
  files: string[]
  others: DesignRef[]
}

export type KType = 'issue' | 'page' | 'glossary' | 'design' | 'file' | 'url' | 'request'

/** A short description of a linked item. */
export interface KItem {
  type: KType
  id?: number
  key?: string
  ref?: string
  title: string
  /** issue type, page category, design kind, URL kind (pull, commit, issue, url) */
  kind?: string
  /** issue status category, design status, "proposed" for unapproved notes */
  state?: string
  project_key?: string
  repo?: string
  number?: number
  provider?: string
}

export const LINK_RELS = [
  'relates',
  'implements',
  'serves',
  'depends_on',
  'supersedes',
  'tension',
  'mentions',
] as const
export type LinkRel = (typeof LINK_RELS)[number]

export interface KLink {
  id: number
  rel: LinkRel
  /** out: this item → item; in: item → this item */
  direction: 'in' | 'out'
  item: KItem
  note?: string
  /** from a [[wikilink]]; edit the text to change it */
  auto?: boolean
  /** "page_issues": a page↔issue link, removed through the pages API */
  via?: string
  created_by?: UserRef
  created_agent?: string
  created_at: string
  /** who stands behind it (WP-ADR50): a person, an agent, or Waypoint's guess */
  basis: LinkBasis
  /** a guess's similarity, 0..1 */
  score?: number
  confirmed_by?: UserRef
  confirmed_at?: string
  can_remove: boolean
  /** an agent's link or a guess a person may confirm as theirs */
  can_confirm?: boolean
}

/** text: a [[wikilink]], whoever wrote the text */
export type LinkBasis = 'person' | 'agent' | 'inferred'

// ---- goals and traces (WP-382, WP-ADR50) ----

/** How an item reaches a goal, best first. */
export const GROUNDINGS = [
  'person_goal',
  'agent_goal',
  'inferred_goal',
  'proposed_goal',
  'person_request',
  'none',
] as const
export type Grounding = (typeof GROUNDINGS)[number]

export interface GoalCoverage {
  /** tasks, pages and notes that trace to it */
  items: number
  open: number
  finished_week: number
  by_grounding: Partial<Record<Grounding, number>>
  /** open tasks linked as in tension with it (WP-1036) */
  tension?: number
}

export interface GoalView {
  note: DesignNote
  /** 0: a top-level goal; 1: a goal under it */
  depth: number
  /** why its title reads as implementation, not intent */
  wording?: string[]
  coverage: GoalCoverage
  /** the newest proposed edit to its words (Waypoint's or an agent's) */
  rewording?: { revision_id: number; title: string; agent?: string }
  /** what a goal Waypoint suggested came from, while it waits */
  why?: string
}

/** What one Suggest goals run proposed (WP-ADR50). */
export interface GoalSuggestResult {
  goals: DesignNote[]
  rewordings: number
  /** people's tasks the model read */
  read: number
  model?: string
  /** why nothing was proposed */
  note?: string
}

/** The model's pick of the goal one task serves (WP-679); nothing is saved. */
export interface TaskGoalSuggestion {
  /** an approved goal it serves */
  goal?: KItem
  /** or a new goal for it */
  title?: string
  why?: string
  model?: string
  /** why there is no suggestion */
  note?: string
}

/** An open task that may serve a goal (WP-679). */
export interface GoalWorkItem {
  item: KItem
  grounding?: Grounding
  score: number
  reason?: string
}

/** The open work likeliest to serve a goal, best first. */
export interface GoalWorkList {
  goal: KItem
  items: GoalWorkItem[]
  /** how many tasks matched, of which items is the best few */
  matched: number
}

/** A link to a goal no person stands behind yet. */
export interface GoalLink {
  link_id: number
  item: KItem
  goal: KItem
  basis: LinkBasis
  agent?: string
  score?: number
  note?: string
  created_at: string
}

export interface GoalCandidate {
  goal: KItem
  score: number
  reason: string
}

export interface UntracedItem {
  item: KItem
  grounding: Grounding
  /** the agent that wrote it, while no person reviewed it */
  agent?: string
  candidate?: GoalCandidate
}

/** An issue's stored trace to a goal; pending while Waypoint hasn't traced it yet. */
export interface IssueGoal {
  grounding?: Grounding
  goal?: KItem
  pending?: boolean
}

export interface GoalDrift {
  week: Partial<Record<Grounding, number>>
  previous: Partial<Record<Grounding, number>>
  /** share of the week's finished work at an approved goal */
  grounded: number
  /** share that reaches one only through Waypoint's guess, apart from grounded */
  guessed: number
  previous_grounded: number
  summary: string
}

/** The drift check's flag: a model's read, waiting for a person (WP-559). */
export interface DriftFlag {
  id: number
  kind: 'contradicts' | 'technology' | 'untraced' | 'run'
  /** what it found, naming the goal */
  headline: string
  goal?: string
  reason: string
  model?: string
  pr?: { repo: string; number: number; title: string; url: string; merged?: boolean }
  items: KItem[]
  /** "stop" while the pull request is still open; empty until a person answers */
  answer?: 'intent' | 'stop'
  created_at: string
}

export interface GoalsOverview {
  project: string
  project_name: string
  /** an approved goal covers the project */
  scoped: boolean
  goals: GoalView[]
  links: GoalLink[]
  untraced: UntracedItem[]
  untraced_total: number
  /** the project's notes of how no person approved or asked for (the newest few) */
  unapproved: KItem[]
  /** how many such notes there are */
  unapproved_count: number
  /** open tasks by grounding */
  totals: Partial<Record<Grounding, number>>
  /** open tasks Waypoint hasn't traced yet (it runs in the background) */
  tracing: number
  drift: GoalDrift | null
  drift_flags: DriftFlag[]
  /** what to ask a person, in plain words */
  ask: string[]
  words: Record<Grounding, string>
  can_edit: boolean
  can_approve: boolean
}

export interface TraceHop {
  from: KItem
  to: KItem
  /** subtask of, serves, implements, mentions, under, written for, documents */
  kind: string
  basis: LinkBasis
  by?: UserRef
  agent?: string
  score?: number
  link_id?: number
}

export interface TraceRoute {
  goal: KItem
  grounding: Grounding
  hops: TraceHop[]
}

export interface ItemTrace {
  item: KItem
  grounding: Grounding
  words: string
  scoped: boolean
  /** a person wrote or approved the item itself */
  person: boolean
  person_hops: number | null
  person_touches: number
  routes: TraceRoute[]
  person_route?: TraceHop[]
  candidate?: GoalCandidate
  /** a pull request's or commit's tasks */
  through?: ItemTrace[]
  note?: string
}

/** One end of a link: type plus id (key for issues), or a ref. */
export interface LinkEnd {
  type?: KType
  id?: number
  key?: string
  ref?: string
}

export type SearchKind = 'issue' | 'request' | 'design' | 'glossary' | 'page' | 'comment'

export interface SearchHit {
  kind: SearchKind
  id: number
  /** the issue, for issues and comments; a page's code */
  key?: string
  title: string
  /** matched words marked ⟦like this⟧ */
  snippet?: string
  project_key?: string
  /** issue status category; design status or "proposed"; "locked" glossary entries */
  state?: string
  /** issue status, page category, design kind, request kind, comment author */
  sub?: string
  /** with a code in the search: exact (the thing it names), mention (text writing it), text */
  match?: 'exact' | 'mention' | 'text'
  updated_at: string
}

export interface SearchGroup {
  kind: SearchKind
  total: number
  hits: SearchHit[]
}

export interface AllResult {
  query: string
  kinds: string[]
  archived: boolean
  /** what the codes in the query name, best first */
  exact: SearchHit[]
  groups: SearchGroup[]
}

/** A code found in a text and what it names (POST /refs). */
export interface TextRef {
  /** the text as written: API:R-28, page #60 */
  match: string
  kind: 'issue' | 'request' | 'design' | 'page' | 'glossary' | 'pull'
  id?: number
  key?: string
  title: string
  state?: string
  project_key?: string
  /** an app path, or the forge's page for a pull request */
  url: string
  external?: boolean
}

/** A branch on the forge with the sweep's verdict (the words `wp tidy` uses). */
export interface RemoteBranch {
  branch: string
  sha: string
  verdict: 'removable' | 'keep' | 'protected' | 'stale'
  reason: string
}

/** A repo's branches on the forge: what the sweep deletes, keeps, or calls stale. */
export interface RemoteBranches {
  repo: string
  forge: string
  default_branch: string
  /** deleting merged branches is on for the repo */
  enabled: boolean
  rows: RemoteBranch[]
  counts: Record<string, number>
  notes?: string[]
}

/** One issue an adversarial review's report names (WP-285). */
export interface WorkIssue {
  claim: string
  file?: string
  outcome: 'fixed' | 'rejected' | 'open'
}

export interface WorkLogChange {
  at: string
  state: string
  by?: string
  note?: string
}

/** One line of a task's work log. */
export interface WorkLogEntry {
  id?: number
  kind:
    'review' | 'own_review' | 'note' | 'run' | 'pr' | 'finding' | 'comment' | 'tested' | 'mismatch'
  at: string
  state?: string
  tag?: 'found' | 'fixed' | 'rejected'
  title: string
  body?: string
  author?: UserRef
  agent?: string
  by?: string
  session?: number
  pr?: { repo: string; number: number; url?: string; state?: string }
  path?: string
  line?: number
  severity?: string
  prompt?: string
  reason?: string
  started_at?: string
  ended_at?: string
  found?: number
  fixed?: number
  rejected?: number
  items?: WorkIssue[]
  read_state?: '' | 'pending' | 'done' | 'failed'
  read_by?: string
  pushed?: boolean
  history?: WorkLogChange[]
}

export interface WorkLogSummary {
  reviews: number
  counted: number
  found: number
  fixed: number
  rejected: number
  pushed: number
  uncounted: number
  /** Rounds whose report a model read, and what it read (WP-1470): never added to the counted ones. */
  model_read_counted: number
  model_read_found: number
  model_read_fixed: number
  model_read_rejected: number
  pr_findings: number
  pr_fixed: number
  pr_rejected: number
  pr_open: number
  notes: number
  mismatches: number
  /** Adversarial reviews the session ran itself (WP-1089), and what they found. */
  own_reviews: number
  own_found: number
}

/** A task's work log: review rounds, notes, runs, pull requests and what their reviews found. */
export interface WorkLog {
  issue: IssueRef
  summary: WorkLogSummary
  entries: WorkLogEntry[]
}

// ---- relation graph (WP-385) ----

export type GraphNodeType = 'issue' | 'page' | 'design' | 'glossary' | 'pull' | 'file' | 'session'

/** One thing in the relation graph. */
export interface GraphNode {
  /** the graph's id: issue:12, file:3:path, pull:URL; also takes as a ref */
  node: string
  type: GraphNodeType
  /** the code to show: WP-12, PL3, ADR7, repo#4, a path */
  ref: string
  title: string
  kind?: string
  state?: string
  project?: string
  /** an app path, or the forge's page */
  url?: string
}

/** One link, read from `from` to `to`. */
export interface GraphEdge {
  from: string
  to: string
  kind: string
  /** read from `from`: "blocked by" */
  label: string
  subkind?: string
  basis: 'person' | 'agent' | 'inferred'
  score?: number
  why?: string
  /** what the step adds to the distance */
  length: number
}

export interface GraphHit extends GraphNode {
  distance: number
  hops: number
  /** from the center to it */
  path: GraphEdge[] | null
}

/** What lies around one thing. */
export interface GraphView {
  center: GraphNode
  nodes: GraphHit[] | null
  edges: GraphEdge[] | null
  guesses: boolean
  more: boolean
  /** admins only: the whole graph, counting what others can't see */
  size?: { nodes: number; edges: number; built: string }
}

/** The nearest route between two things. */
export interface GraphPath {
  from: GraphNode
  to: GraphNode
  found: boolean
  distance: number
  hops: number
  path: GraphEdge[] | null
  nodes: GraphNode[] | null
}
