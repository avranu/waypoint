// The CI board (WP-225): what the shared Forgejo Actions runners are
// running, what waits and why, what finished, and when it will all be done.
// Times are ISO strings; durations are seconds.

export interface CIPoolRunner {
  name: string
  labels: string[] | null
  capacity: number
}

export interface CIForgeRunner {
  id: number
  name: string
  status: 'offline' | 'idle' | 'active' | string
  labels: string[] | null
  version: string
}

export interface CIPool {
  forge_id: number
  forge_name: string
  /** configured by an admin, summed from the hosts' reports, guessed from the forge's runners, or from the busiest moment */
  source: 'configured' | 'reported' | 'runners' | 'observed'
  runners: CIPoolRunner[]
  forge_runners: CIForgeRunner[] | null
  peak_24h: number
  capacity: number
}

export interface CIHost {
  forge_id: number
  name: string
  labels: string[] | null
  capacity: number
  running: number
  cpus: number
  /** bytes */
  memory: number
  pool_cpus: number
  pool_memory: number
  job_cpus: number
  job_memory: number
  disk_total: number
  disk_free: number
  load1: number
  version: string
  reported_at: string
  health: 'ok' | 'low_disk' | 'stale'
  why?: string
}

export interface CIPRRef {
  number: number
  url: string
  title: string
}

export interface CIRef {
  forge_id: number
  repo: string
  project?: string
  /** in a project you can't see: only its timing is shown */
  hidden?: boolean
  workflow: string
  run_id: number
  run_number: number
  title: string
  ref: string
  event: string
  sha: string
  url: string
  created: string
  pr?: CIPRRef
}

export type CIJobStatus =
  'waiting' | 'blocked' | 'running' | 'success' | 'failure' | 'cancelled' | 'skipped'

export interface CIJob extends CIRef {
  name: string
  job_id?: number
  status: CIJobStatus
  runs_on?: string[]
  needs?: string[]
  ready_at?: string
  started?: string
  finished?: string
  wait?: number
  duration?: number
  typical: number
  slow: number
  est_start?: string
  est_finish?: string
  est_finish_p90?: string
  overrun?: boolean
  no_runner?: boolean
  position?: number
  /** the host it runs on, and what it has used so far (from the hosts' reports) */
  host?: string
  cpu_seconds?: number
  mem_peak?: number
}

export interface CIRun extends CIRef {
  jobs: number
  done: number
  failed: number
  running: number
  waiting: number
  status: string
  est_finish?: string
  est_finish_p90?: string
  cant_finish?: boolean
}

export interface CIJobStats {
  repo: string
  hidden?: boolean
  workflow: string
  name: string
  runs: number
  success: number
  failure: number
  cancelled: number
  p50: number
  p90: number
  max: number
  wait_p50: number
  busy: number
  last: string
  last_at?: string
  usage_runs?: number
  mem_p95?: number
  mem_max?: number
  cores_p50?: number
  cores_p95?: number
}

export interface CIRepo {
  forge_id: number
  repo: string
  project?: string
  hidden?: boolean
  error?: string
  busy: number
  jobs: number
  running: number
  queued: number
}

export interface CIAlert {
  level: 'error' | 'warn' | 'info'
  title: string
  body?: string
  url?: string
}

export interface CISummary {
  running: number
  waiting: number
  blocked: number
  capacity: number
  all_done_at?: string
  all_done_p90?: string
  stuck: number
  oldest_waiting?: string
  finished_24h: number
  failed_24h: number
  cancelled_24h: number
  wait_p50: number
  wait_p90: number
  /** waits of the jobs that started in the last 3h */
  wait_p50_3h: number
  wait_p90_3h: number
  run_p50: number
  busy_seconds_24h: number
  utilization_24h: number
  /** where the "24h" figures start: a day ago, or later when the kept jobs have a hole */
  since: string
}

export interface CIBoard {
  generated_at: string
  summary: CISummary
  alerts: CIAlert[]
  pools: CIPool[]
  hosts?: CIHost[] | null
  runs: CIRun[]
  running: CIJob[]
  queued: CIJob[]
  recent: CIJob[]
  stats: CIJobStats[]
  repos: CIRepo[]
  timeline: { at: string; running: number; waiting: number }[]
  can_edit_pool: boolean
}

/** seconds from now (negative: past) */
export function untilSec(iso: string | undefined, now: number): number {
  return iso ? (new Date(iso).getTime() - now) / 1000 : 0
}

/** 12.0 GiB, 512.0 MiB (bytes in) */
export function gib(n: number): string {
  if (n >= 1024 ** 3) return (n / 1024 ** 3).toFixed(1) + ' GiB'
  return (n / 1024 ** 2).toFixed(1) + ' MiB'
}

// CI history (WP-490): finished runs and jobs the server keeps, bucketed by
// day or week, with the last seven days against the seven before.

export interface CIHistoryStats {
  from: string
  to: string
  /** finished jobs, skipped ones aside */
  jobs: number
  failed: number
  cancelled: number
  /** runner seconds */
  busy: number
  /** seconds a job waited for a runner */
  wait_p50: number | null
  wait_p90: number | null
  /** full (non-draft) CI runs that finished */
  runs: number
  /** a green full run, created to stopped, seconds */
  run_p50: number | null
  run_p90: number | null
  merges: number
  /** seconds from a pull request's turn to its merge */
  merge_p50: number | null
}

export interface CIHistoryPoint extends CIHistoryStats {
  at: string
}

export interface CIHistoryJobDiff {
  repo: string
  workflow: string
  name: string
  runs: number
  failed: number
  p50: number | null
  busy: number
  before_runs: number
  before_failed: number
  before_p50: number | null
}

export type CIHistoryBucket = 'day' | 'week'

export interface CIHistory {
  generated_at: string
  from: string
  to: string
  bucket: CIHistoryBucket
  days: number
  repo?: string
  /** the repos you can see that have history */
  repos: string[]
  /** the oldest job kept of those */
  since?: string
  points: CIHistoryPoint[]
  /** the last seven days */
  now: CIHistoryStats
  /** the seven before them */
  before: CIHistoryStats
  /** full-tier jobs, most runner time now first */
  jobs: CIHistoryJobDiff[]
}
