// Sandbox policy (WP-418, WP-ADR28): how strongly a project's agent work is
// walled off, each machine's default and kill switch, and the log of
// changes. Only a person in the browser changes it; loosening asks for the
// password again (useReauth) and a reason.

import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, enc } from './api'
import type { Runner } from './runners'
import type { UserRef } from './types'

export type SandboxLevel = '' | 'vm' | 'contained' | 'tool' | 'off'

export interface SandboxPolicy {
  project: string
  /** runner jobs; '' = the machine's default */
  level: SandboxLevel
  /** interactive sessions; '' = the same as `level` */
  interactive_level: SandboxLevel
  updated_by?: UserRef
  updated_at?: string
  can_edit: boolean
}

export interface SandboxMachine {
  runner: Runner
  default_level: SandboxLevel
  /** the kill switch: sandboxing paused on this machine */
  paused: boolean
  updated_by?: UserRef
  updated_at?: string
  can_edit: boolean
  /** what the machine's sandbox helper last reported; none = never */
  helper?: SandboxHelper
  /** what Waypoint knows of each machine user, by name */
  users?: Record<string, SandboxUserInfo>
  /** the latest helper actions people asked for, newest first */
  ops: SandboxOp[] | null
  /** the machine's latest escape drill (WP-473); none = never */
  drill?: SandboxDrill
  /** why a drill can't run on the machine now; none = it can */
  drill_blocked?: string
}

/** One try of the escape drill: refused is what the sandbox must do, and
 *  allowed for a control, something a session must be able to do. */
export interface SandboxDrillCase {
  name: string
  group: 'files' | 'network' | 'egress' | 'kernel' | 'resources' | string
  want: 'refused' | 'allowed'
  got: 'refused' | 'allowed' | 'inconclusive'
  detail?: string
}

/** A run of the escape drill on a machine: Contained sessions that try to
 *  get out (WP-473, PL7 §13). */
export interface SandboxDrill {
  id: number
  job_id: number
  /** first, weekly, helper (its version changed), retry, person */
  reason: string
  started_by?: UserRef
  helper_version: string
  /** the machine user the sessions ran as */
  repo_user?: string
  /** failed: something got through or a control was refused; error: it
   *  couldn't run or judge enough */
  state: 'queued' | 'running' | 'passed' | 'failed' | 'error'
  passed: number
  escaped: number
  failed_controls: number
  inconclusive: number
  cases: SandboxDrillCase[]
  error?: string
  created_at: string
  finished_at?: string
  /** the escape items the drill doesn't try yet, and why */
  not_covered: string[] | null
}

export interface SandboxUserInfo {
  /** the last runner job Waypoint signed a spec naming this user for */
  last_job?: { id: number; title: string; at: string }
  /** a project here still works on its repository or organization */
  linked: boolean
}

/** A helper action a person asked for (WP-448): remove a machine user, or
 *  finish work the helper left half done. */
export interface SandboxOp {
  id: number
  op: 'remove_user' | 'reconcile'
  user?: string
  requested_by?: UserRef
  requested_at: string
  sent_at?: string
  done_at?: string
  error?: string
  /** waiting: for the runner to poll; sent: the runner has it */
  state: 'waiting' | 'sent' | 'done' | 'failed'
}

/** A machine user the helper took away; its directories wait in archive. */
export interface SandboxRemoved extends SandboxUser {
  at: number
  job?: number
  archive?: string
}

/** A machine user the sandbox helper made: one per repository (repo) or
 *  forge organization (org). */
export interface SandboxUser {
  name: string
  uid: number
  kind: 'repo' | 'org'
  /** the repository or organization's name when last used */
  slug: string
  created: number
  last_used: number
  present?: boolean
}

export interface SandboxHelper {
  installed: boolean
  error?: string
  status?: {
    version: string
    key: string
    runner_uid: number
    /** the runner the helper is pinned to; 0 = any on the machine */
    runner_id?: number
    store: {
      path: string
      ok: boolean
      problem?: string
      mounted: boolean
      fs?: string
      free?: number
    }
    kvm: boolean
    users: SandboxUser[] | null
    retired: number
    removed?: SandboxRemoved[]
    pending: number
    drift: string[] | null
  }
  at: string
  /** Waypoint hands this machine's jobs signed specs; problem says why not */
  ready: boolean
  problem?: string
}

/** The key machines' helpers pin: they act only on specs it signed. */
export interface SandboxKey {
  key: string
  fingerprint: string
}

export interface SandboxChange {
  id: number
  at: string
  user?: UserRef
  scope: 'project' | 'machine'
  target_id: number
  target_name: string
  field: 'level' | 'interactive_level' | 'default_level' | 'paused'
  old: string
  new: string
  loosens: boolean
  reason?: string
  reverts?: number
  /** the field still holds `new`, so Revert puts `old` back */
  current: boolean
}

/** The levels, strongest first, with what each one leaves open. */
export const SANDBOX_LEVELS: { value: Exclude<SandboxLevel, ''>; label: string; note: string }[] = [
  {
    value: 'vm',
    label: 'VM',
    note: 'A throwaway VM: the agent sees its checkout and nothing of the host.',
  },
  {
    value: 'contained',
    label: 'Contained',
    note: 'A user of its own on the host: no other repos, no keys, no Docker socket.',
  },
  {
    value: 'tool',
    label: 'Agent CLI’s own',
    note: 'Only the agent CLI’s sandbox: it runs as you, and a setting in the CLI can widen it.',
  },
  {
    value: 'off',
    label: 'Off',
    note: 'Runs as you: everything you can reach, other repos, keys and the Docker socket included.',
  },
]

const RANK: Record<string, number> = { vm: 3, contained: 2, tool: 1, off: 0 }

export const levelLabel = (v: string, unset: string) =>
  SANDBOX_LEVELS.find((l) => l.value === v)?.label ?? unset

/**
 * Whether a level change loosens: unset might be any level, so moving away
 * from it loosens unless to VM, and moving to it loosens unless from Off.
 * The server decides; this only shows the reason field ahead of time.
 */
export function levelLoosens(from: string, to: string): boolean {
  if (from === to) return false
  const oldMax = from === '' ? 3 : RANK[from]
  const newMin = to === '' ? 0 : RANK[to]
  return newMin < oldMax
}

/** Whether a change of field from → to loosens. */
export function changeLoosens(field: SandboxChange['field'], from: string, to: string) {
  return field === 'paused' ? from !== 'yes' && to === 'yes' : levelLoosens(from, to)
}

export interface SandboxPolicyInput {
  level?: SandboxLevel
  interactive_level?: SandboxLevel
  reason?: string
}

export interface SandboxMachineInput {
  default_level?: SandboxLevel
  paused?: boolean
  reason?: string
}

export const saveProjectSandbox = (key: string, input: SandboxPolicyInput) =>
  api.patch<SandboxPolicy>(`/projects/${enc(key)}/sandbox`, input)

export const saveSandboxMachine = (id: number, input: SandboxMachineInput) =>
  api.patch<SandboxMachine>(`/sandbox/machines/${id}`, input)

export const requestSandboxOp = (id: number, input: { op: SandboxOp['op']; user?: string }) =>
  api.post<SandboxOp>(`/sandbox/machines/${id}/ops`, input)

export const startSandboxDrill = (id: number) =>
  api.post<SandboxDrill>(`/sandbox/machines/${id}/drill`, {})

export const revertSandboxChange = (id: number, reason: string) =>
  api.post<void>(`/sandbox/log/${id}/revert`, { reason })

export const reauth = (password: string) =>
  api.post<{ until: string | null }>('/me/reauth', { password })

/** After a change: what to refetch. */
export function useSandboxRefresh() {
  const qc = useQueryClient()
  return () => qc.invalidateQueries({ queryKey: ['sandbox'] })
}

export function useRevertSandboxChange() {
  const refresh = useSandboxRefresh()
  return useMutation({
    mutationFn: ({ id, reason }: { id: number; reason: string }) => revertSandboxChange(id, reason),
    onSettled: refresh,
  })
}
