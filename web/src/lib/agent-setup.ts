import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from './api'
import type { UserRef } from './types'
import { toast, toastError } from '@/components/ui/toast'

export type SetupStatus = 'ok' | 'warn' | 'fail' | 'unread'
export type CheckState = 'ok' | 'warn' | 'fail'
/** Where a repo's shared block stands. */
export type SharedState = 'current' | 'missing' | 'behind' | 'edited' | 'unread' | 'none'
export type ComponentKind = 'agent' | 'skill' | 'command' | 'hook' | 'mcp'

export interface SetupCheck {
  key: string
  label: string
  state: CheckState
  note?: string
}

export interface AgentComponent {
  kind: ComponentKind
  name: string
  detail?: string
  path: string
}

export interface SetupComponent extends AgentComponent {
  /** no other repo (machine) has it */
  only_here?: boolean
}

export interface CommonComponent {
  kind: ComponentKind
  name: string
  count: number
  common: boolean
}

export interface AgentFile {
  path: string
  /** instructions, agent, skill, command, settings, mcp, hook */
  kind: string
  size: number
  lines: number
  truncated?: boolean
}

export interface AgentFilePR {
  id: number
  repo: string
  kind: 'shared' | 'edit'
  version?: number
  paths: string[]
  branch?: string
  number?: number
  url?: string
  state: 'open' | 'merged' | 'closed' | 'failed'
  error?: string
  opened_by?: UserRef
  opened_at: string
}

export interface AgentRepo {
  forge_id: number
  repo: string
  url: string
  projects: string[]
  opted_out: boolean
  can_edit: boolean
  branch?: string
  head?: string
  scanned_at: string | null
  error?: string
  root_file?: string
  shared: SharedState
  shared_version?: number
  shared_pr?: AgentFilePR
  shared_error?: string
  files: AgentFile[]
  components: SetupComponent[]
  missing: AgentComponent[]
  kit_plugins: string[]
  checks: SetupCheck[]
  prs: AgentFilePR[]
  status: SetupStatus
}

export interface AgentMachine {
  user: UserRef
  machine: string
  os: string
  wp_version: string
  reported_at: string
  files: AgentFile[]
  components: SetupComponent[]
  missing: AgentComponent[]
  checks: SetupCheck[]
  status: SetupStatus
}

export interface SharedVersion {
  version: number
  text: string
  saved_by?: UserRef
  saved_at: string
}

export interface AgentSetup {
  shared: {
    current: SharedVersion | null
    versions: SharedVersion[]
    counts: Partial<Record<SharedState, number>>
  }
  repos: AgentRepo[]
  repo_common: CommonComponent[]
  machines: AgentMachine[]
  machine_common: CommonComponent[]
  can_manage: boolean
  can_edit: boolean
}

export interface AgentFileContent extends AgentFile {
  content?: string
  repo?: string
  machine?: string
  sha?: string
  url?: string
  editable: boolean
  shared?: string
}

export interface PublishResult {
  version: number
  repos: string[]
  skipped?: string[]
}

export const STATUS: Record<SetupStatus, { label: string; color: string }> = {
  fail: { label: 'Needs fixing', color: 'var(--wp-danger)' },
  warn: { label: 'Differs', color: 'var(--wp-warn)' },
  ok: { label: 'In line', color: 'var(--wp-ok)' },
  unread: { label: 'Not read yet', color: 'var(--wp-subtle)' },
}

export const SHARED: Record<SharedState, { label: string; hint: string; color?: string }> = {
  current: { label: 'current', hint: 'Has the latest shared instructions.', color: 'var(--wp-ok)' },
  behind: {
    label: 'behind',
    hint: 'Has an older version of the shared instructions.',
    color: 'var(--wp-warn)',
  },
  missing: {
    label: 'missing',
    hint: 'Its root instruction file has no shared block.',
    color: 'var(--wp-warn)',
  },
  edited: {
    label: 'edited by hand',
    hint: 'The shared block was changed in the repo; publishing replaces it.',
    color: 'var(--wp-warn)',
  },
  unread: { label: 'not read', hint: 'Its files haven’t been read yet.' },
  none: { label: 'no shared text', hint: 'No shared instructions are saved yet.' },
}

export const KIND_LABEL: Record<ComponentKind, [string, string]> = {
  agent: ['agent', 'agents'],
  skill: ['skill', 'skills'],
  command: ['command', 'commands'],
  hook: ['hook', 'hooks'],
  mcp: ['MCP server', 'MCP servers'],
}

const invalidate = (qc: ReturnType<typeof useQueryClient>) =>
  qc.invalidateQueries({ queryKey: ['agentSetup'] })

/** Admins: save a new version of the shared instructions. */
export function useSaveShared() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (text: string) => api.put<SharedVersion>('/agent-setup/shared', { text }),
    onError: toastError,
    onSuccess: (v) =>
      toast(`Saved version ${v.version}. Repos without it get a pull request within the hour.`, {
        kind: 'success',
      }),
    onSettled: () => invalidate(qc),
  })
}

/** Admins: open the pull requests now, on every repo not current. */
export function usePublishShared() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<PublishResult>('/agent-setup/publish'),
    onError: toastError,
    onSuccess: (r) =>
      toast(
        r.repos.length === 0
          ? 'Every repo already has it.'
          : `Opening ${r.repos.length} pull request${r.repos.length === 1 ? '' : 's'}${
              r.skipped?.length ? `; ${r.skipped.length} opted out` : ''
            }.`,
        { kind: 'success' },
      ),
    onSettled: () => invalidate(qc),
  })
}

/** Read every repo's files again now. */
export function useRefreshSetup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<void>('/agent-setup/refresh'),
    onError: toastError,
    onSuccess: () => toast('Reading every repo again…'),
    onSettled: () => invalidate(qc),
  })
}

export interface AgentFileEdit {
  forge_id: number
  repo: string
  path: string
  content: string
  base_sha: string
  message?: string
}

/** Open a pull request with a change to one repo's agent file. */
export function useEditAgentFile() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (e: AgentFileEdit) =>
      api.put<{ branch: string; number: number; url: string }>('/agent-setup/file', e),
    onError: toastError,
    onSuccess: (r) =>
      toast(`Opened pull request #${r.number}; it merges when its checks pass.`, {
        kind: 'success',
        action: r.url
          ? { label: 'Open', run: () => window.open(r.url, '_blank', 'noopener') }
          : undefined,
      }),
    onSettled: () => invalidate(qc),
  })
}

/** Counts of statuses, for the at-a-glance line. */
export function tally<T extends { status: SetupStatus }>(rows: T[]) {
  const out: Record<SetupStatus, number> = { ok: 0, warn: 0, fail: 0, unread: 0 }
  for (const r of rows) out[r.status]++
  return out
}
