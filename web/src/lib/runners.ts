import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, qs } from './api'
import type { UserRef } from './types'
import { toastError } from '@/components/ui/toast'

export interface RunnerCLI {
  name: string
  version?: string
  /** null: the CLI can't say */
  signed_in: boolean | null
  detail?: string
}

export interface Runner {
  id: number
  owner: UserRef
  name: string
  hostname: string
  os: string
  arch: string
  version: string
  labels: string[]
  allowed_dirs: string[]
  clis: RunnerCLI[]
  max_jobs: number
  online: boolean
  last_seen_at: string | null
  /** jobs on it now */
  running: number
  /** what it last said about its machine's load (WP-366) */
  capacity?: RunnerCapacity
  created_at: string
}

/** How many jobs a runner takes now, from how busy its machine is. */
export interface RunnerCapacity {
  /** heavy jobs (agents, commands, sessions) at once now; light ones have a lane as big */
  limit: number
  /** --max-jobs, all kinds together */
  max: number
  /** '' when the runner can't measure (not Linux) */
  level: '' | 'calm' | 'busy' | 'overloaded'
  reason?: string
  cpus?: number
  /** 1-minute load per CPU */
  load?: number
  /** share of memory available */
  mem_free?: number
  /** hour starts of the next day its machine is usually busy with other work */
  busy_at?: string[]
  heavy: number
  light: number
  at: string
}

export type JobKind = 'command' | 'apply' | 'agent' | 'review'
export type JobState = 'queued' | 'running' | 'done' | 'failed' | 'cancelled' | 'expired'

export interface RunnerJob {
  id: number
  kind: JobKind
  title: string
  state: JobState
  owner_id: number
  created_by: UserRef | null
  runner_id: number | null
  runner?: string
  target_runner_id?: number
  label?: string
  /** seconds */
  timeout: number
  attempts: number
  cancel_requested?: boolean
  exit_code: number | null
  error?: string
  log_size: number
  source?: string
  source_id?: number
  created_at: string
  started_at: string | null
  finished_at: string | null
  input?: Record<string, unknown>
  result?: Record<string, unknown>
  log?: string
  log_from?: number
  /** the byte after `log`: where the next read starts */
  log_end?: number
  waiting?: string
  /** an admin sees that the job exists, not what it did */
  redacted?: boolean
}

export const JOB_STATES: Record<JobState, { label: string; color?: string }> = {
  queued: { label: 'Waiting' },
  running: { label: 'Running', color: 'var(--wp-accent)' },
  done: { label: 'Done', color: 'var(--wp-ok)' },
  failed: { label: 'Failed', color: 'var(--wp-danger)' },
  cancelled: { label: 'Cancelled' },
  expired: { label: 'Expired', color: 'var(--wp-warn)' },
}

export const finished = (s: JobState) => s !== 'queued' && s !== 'running'

export const KIND_LABELS: Record<JobKind, string> = {
  command: 'Command',
  apply: 'Apply changes',
  agent: 'Coding agent',
  review: 'Review',
}

export const CLI_LABELS: Record<string, string> = {
  claude: 'Claude Code',
  codex: 'Codex',
  opencode: 'opencode',
  cursor: 'Cursor',
}

/**
 * The CLIs a review may use on a runner: the ones the runner can confine to
 * the checkout. Codex's read-only sandbox reads the whole disk and Cursor's
 * rules can't allow one folder alone, so a pull request's text is never
 * handed to them.
 */
export const REVIEW_CLIS = ['claude', 'opencode']

/** What to run to sign in to each CLI. */
export const CLI_SIGN_IN: Record<string, string> = {
  claude: 'claude auth login',
  codex: 'codex login',
  opencode: 'opencode auth login',
  cursor: 'cursor-agent login',
}

export type CLIReadiness =
  | { state: 'ready'; runner: string; version?: string }
  | { state: 'signed_out'; runner: string }
  | { state: 'missing' }

/**
 * Which coding-agent CLIs the given runners can take work for, as the server
 * sees it (an online runner with the CLI, not known to be signed out).
 */
export function cliReadiness(runners: Runner[]): Record<string, CLIReadiness> {
  const out: Record<string, CLIReadiness> = {}
  for (const cli of Object.keys(CLI_LABELS)) {
    out[cli] = { state: 'missing' }
    for (const r of runners) {
      const c = r.clis.find((x) => x.name === cli)
      if (!c) continue
      if (r.online && c.signed_in !== false) {
        out[cli] = { state: 'ready', runner: r.name, version: c.version }
        break
      }
      if (c.signed_in === false && out[cli].state === 'missing') {
        out[cli] = { state: 'signed_out', runner: r.name }
      }
    }
  }
  return out
}

export const runnersQuery = (all = false) =>
  queryOptions({
    queryKey: ['runners', { all }],
    queryFn: () => api.get<Runner[]>(`/runners${qs({ all })}`),
    staleTime: 15_000,
  })

export interface JobFilter {
  runner?: number
  state?: string
  source?: string
  all?: boolean
  limit?: number
}

export const runnerJobsQuery = (f: JobFilter = {}) =>
  queryOptions({
    queryKey: ['runnerJobs', f],
    queryFn: () =>
      api.get<RunnerJob[]>(
        `/runner-jobs${qs({ runner: f.runner, state: f.state, source: f.source, all: f.all, limit: f.limit })}`,
      ),
    staleTime: 15_000,
  })

/**
 * One job with its output. After the first read, a refetch (the live event
 * for new output) asks only for the bytes it lacks and adds them on.
 */
export const runnerJobQuery = (id: number) =>
  queryOptions({
    queryKey: ['runnerJob', id],
    queryFn: async ({ client }) => {
      const prev = client.getQueryData<RunnerJob>(['runnerJob', id])
      if (prev?.log !== undefined && prev.log_end !== undefined) {
        const next = await api.get<RunnerJob>(`/runner-jobs/${id}?after=${prev.log_end}`)
        if (next.log === undefined) return next
        return { ...next, log: prev.log + next.log, log_from: 0 }
      }
      return api.get<RunnerJob>(`/runner-jobs/${id}?log=1`)
    },
    staleTime: 15_000,
  })

const invalidate = (qc: ReturnType<typeof useQueryClient>) => {
  qc.invalidateQueries({ queryKey: ['runners'] })
  qc.invalidateQueries({ queryKey: ['runnerJobs'] })
}

export function useRemoveRunner() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api.del(`/runners/${id}`),
    onError: toastError,
    onSettled: () => invalidate(qc),
  })
}

export function useCancelJob() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api.post<RunnerJob>(`/runner-jobs/${id}/cancel`),
    onError: toastError,
    onSettled: (_j, _e, id) => {
      qc.invalidateQueries({ queryKey: ['runnerJob', id] })
      invalidate(qc)
    },
  })
}

/** A person's click: run a command on one of their runners. */
export function useRunCommand() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { runner: number; dir: string; command: string }) =>
      api.post<RunnerJob>('/runner-jobs', {
        kind: 'command',
        runner_id: v.runner,
        input: { command: v.command, dir: v.dir },
      }),
    onError: toastError,
    onSettled: () => invalidate(qc),
  })
}

/** "linux/amd64", or whatever the runner reported. */
export const platform = (r: Pick<Runner, 'os' | 'arch'>) => [r.os, r.arch].filter(Boolean).join('/')

/** The command that sets a machine up: wp, sign-in, Claude Code hooks and the runner (wp setup). */
export const connectCommand = (origin: string) => `curl -fsSL ${origin}/setup | sh`
