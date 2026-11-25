import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from './api'
import type { UserRef } from './types'
import { toastError } from '@/components/ui/toast'

export interface KitComponent {
  name: string
  description?: string
}

export interface KitTarget {
  repos?: string[]
  when?: string[]
  machines?: string[]
}

export interface KitPlugin {
  name: string
  description?: string
  version?: string
  source: string
  targeted: boolean
  target: KitTarget
  skills: KitComponent[]
  agents: KitComponent[]
  commands: KitComponent[]
  hooks: KitComponent[]
  mcp: KitComponent[]
  bin: KitComponent[]
}

export interface KitNote extends KitTarget {
  text: string
}

export interface KitInfo {
  configured: boolean
  repo_id?: number
  repo?: string
  project?: string
  repo_url?: string
  branch?: string
  commit?: string
  kit: {
    marketplace: string
    git_url: string
    owner?: string
    plugins: KitPlugin[]
    notes: KitNote[]
  } | null
  good_at: string | null
  read_at: string | null
  errors: string[]
  read_sha?: string
  reach: Record<string, string[]>
  can_manage: boolean
}

export interface KitDrift {
  plugin?: string
  what: string
}

export interface KitCheckout {
  user: UserRef
  machine: string
  path: string
  repo: string
  kit_commit: string
  behind: boolean
  plugins: string[]
  drift: KitDrift[]
  wp_version: string
  error: string
  reported_at: string
}

export interface KitRollout {
  runner_id: number
  runner: string
  job_id?: number
  skipped?: string
}

export interface KitSource {
  repo_id: number
  candidates: { id: number; repo: string; project: string; forge: string }[]
}

/** The parts of a plugin, in the order the page lists them. */
export const KIT_PARTS: {
  key: keyof Pick<KitPlugin, 'skills' | 'agents' | 'commands' | 'hooks' | 'mcp' | 'bin'>
  one: string
  many: string
}[] = [
  { key: 'skills', one: 'skill', many: 'skills' },
  { key: 'agents', one: 'agent', many: 'agents' },
  { key: 'commands', one: 'command', many: 'commands' },
  { key: 'hooks', one: 'hook event', many: 'hook events' },
  { key: 'mcp', one: 'MCP server', many: 'MCP servers' },
  { key: 'bin', one: 'tool', many: 'tools' },
]

export const shortSHA = (sha?: string) => (sha ? sha.slice(0, 7) : '')

const invalidateKit = (qc: ReturnType<typeof useQueryClient>) =>
  qc.invalidateQueries({ queryKey: ['kit'] })

/** Roll out: queue `wp kit sync --all` on each of your online runners. */
export function useRolloutKit() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<KitRollout[]>('/kit/rollout'),
    onError: toastError,
    onSettled: () => {
      invalidateKit(qc)
      qc.invalidateQueries({ queryKey: ['runnerJobs'] })
    },
  })
}

/** Read the kit again now (admins). */
export function useReadKit() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<KitInfo>('/kit/read'),
    onError: toastError,
    onSettled: () => invalidateKit(qc),
  })
}

/** Pick the kit's repo (admins); 0 is none. */
export function useSetKitSource() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (repoId: number) => api.put<KitSource>('/admin/kit', { repo_id: repoId }),
    onError: toastError,
    onSettled: () => invalidateKit(qc),
  })
}
