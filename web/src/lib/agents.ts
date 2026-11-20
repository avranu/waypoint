// Agents in the UI (WP-D7): whether to show issues agents made for their
// own bookkeeping, and the workflows agents move work through.

import { useSyncExternalStore } from 'react'
import { queryOptions } from '@tanstack/react-query'
import { api, enc, qs } from './api'
import { readLocal } from './hooks'
import type { Workflow } from './types'

// ---- agent-made visibility, per project ----
// Issues agents create for their own tracking are hidden from people's
// views unless they ask to see them. Delegated work (set by people) stays.

const subs = new Set<() => void>()
const storeKey = (project: string) => `agent-made-${project}`

export function showsAgentMade(project: string) {
  return readLocal(storeKey(project), false)
}

export function setShowAgentMade(project: string, v: boolean) {
  try {
    localStorage.setItem('wp:' + storeKey(project), JSON.stringify(v))
  } catch {
    // private mode: the choice lasts until reload
  }
  subs.forEach((f) => f())
}

/** The filter actually sent: hides agent-made issues unless shown or asked for. */
export function agentFilter(project: string, filter = '') {
  if (showsAgentMade(project) || /agent-?made|unreviewed/i.test(filter)) return filter
  return [filter, '-is:agent-made'].filter(Boolean).join(' ')
}

/** agentFilter that re-renders when the choice changes. */
export function useAgentFilter(project: string, filter = '') {
  const shown = useShowAgentMade(project)
  return shown || /agent-?made|unreviewed/i.test(filter)
    ? filter
    : [filter, '-is:agent-made'].filter(Boolean).join(' ')
}

export function useShowAgentMade(project: string) {
  return useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    () => showsAgentMade(project),
  )
}

// ---- agent specs ----

/** Suggestions for the agent picker; any family or model name works. */
export const AGENT_SUGGESTIONS = [
  'claude',
  'opus',
  'sonnet',
  'haiku',
  'anthropic',
  'openai',
  'gpt',
  'grok',
  'gemini',
]

// ---- who can pick a task up (WP-93) ----
// issue.agent: '' (absent) = Anyone, the default; 'none' = Humans only; 'any' or a
// family/model spec = Agents (delegated).

export type Who = 'anyone' | 'humans' | 'agents'

export const HUMANS_ONLY = 'none'

/** Which of the three states an issue's agent field is. */
export function whoOf(spec?: string): Who {
  return !spec ? 'anyone' : spec === HUMANS_ONLY ? 'humans' : 'agents'
}

/** Handed to agents on purpose (any agent, or a family/model). */
export const isDelegated = (spec?: string) => whoOf(spec) === 'agents'

/** The agent field to store for a state; Agents starts as any agent. */
export function specFor(who: Who): string {
  return who === 'anyone' ? '' : who === 'humans' ? HUMANS_ONLY : 'any'
}

/** "Anyone", "Humans only", "Any agent" or the spec: who may take it, as text. */
export function whoLabel(spec?: string) {
  const who = whoOf(spec)
  return who === 'anyone'
    ? 'Anyone'
    : who === 'humans'
      ? 'Humans only'
      : spec === 'any'
        ? 'Any agent'
        : agentLabel(spec!)
}

/** Short label for an agent spec or identity: "anthropic/claude-opus-5-5" → "claude-opus-5-5". */
export function agentLabel(spec: string) {
  if (spec === 'any') return 'any agent'
  if (spec === HUMANS_ONLY) return 'humans only'
  // Text from a forge login agents may write as (WP-282).
  if (spec === 'forge') return 'a forge login'
  if (spec.startsWith('forge/')) return `forge login ${spec.slice(6)}`
  // Waypoint's own work (its goal suggestions, the babysitter).
  if (spec === 'waypoint' || spec.startsWith('waypoint/')) return 'Waypoint'
  const i = spec.lastIndexOf('/')
  return i >= 0 ? spec.slice(i + 1) : spec
}

// ---- workflows ----

export const workflowsQuery = (project = '') =>
  queryOptions({
    queryKey: ['workflows', project],
    queryFn: () => api.get<Workflow[]>(`/workflows${qs({ project: project || undefined })}`),
    staleTime: 5 * 60_000,
  })

export const workflowPath = (id: number) => `/workflows/${enc(String(id))}`

// ---- nested projects ----

/**
 * Projects in tree order with their depth; a parent you can't see makes a root.
 * Siblings go by name, by latest activity ('recent'), or in `custom` order
 * (project ids, first to last; projects it doesn't list come first).
 */
export function projectTree<
  P extends { id: number; parent_id?: number | null; active_at?: string },
>(projects: P[], order: 'name' | 'recent' | 'custom' = 'name', custom: number[] = []) {
  const ids = new Set(projects.map((p) => p.id))
  const kids = new Map<number | null, P[]>()
  for (const p of projects) {
    const parent = p.parent_id && ids.has(p.parent_id) ? p.parent_id : null
    kids.set(parent, [...(kids.get(parent) ?? []), p])
  }
  if (order !== 'name') {
    // A parent is as recent as anything under it.
    const latest = new Map<number, number>()
    const last = (p: P): number => {
      let t = latest.get(p.id)
      if (t === undefined) {
        t = Date.parse(p.active_at ?? '') || 0
        for (const c of kids.get(p.id) ?? []) t = Math.max(t, last(c))
        latest.set(p.id, t)
      }
      return t
    }
    const at = new Map(custom.map((id, n) => [id, n]))
    for (const list of kids.values())
      list.sort((a, b) => {
        const x = at.get(a.id)
        const y = at.get(b.id)
        // Custom: the unlisted (new) first, most recent among them, then the list.
        if (order === 'custom' && (x !== undefined || y !== undefined)) {
          if (x === undefined) return -1
          if (y === undefined) return 1
          return x - y
        }
        return last(b) - last(a)
      })
  }
  const out: { project: P; depth: number; hasChildren: boolean; ancestors: number[] }[] = []
  const walk = (parent: number | null, depth: number, ancestors: number[]) => {
    for (const p of kids.get(parent) ?? []) {
      out.push({ project: p, depth, hasChildren: kids.has(p.id), ancestors })
      walk(p.id, depth + 1, [...ancestors, p.id])
    }
  }
  walk(null, 0, [])
  return out
}

/**
 * The custom order after `id` takes the place of `over` among its siblings (the
 * projects under the same parent), or null when they aren't siblings. `tree` is
 * the order now shown; the result lists every project in it, so siblings keep
 * the relative order they have on screen.
 */
export function moveProject<P extends { id: number; parent_id?: number | null }>(
  tree: { project: P }[],
  id: number,
  over: number,
): number[] | null {
  const all = tree.map((n) => n.project)
  const parent = new Map(all.map((p) => [p.id, p.parent_id ?? null]))
  if (id === over || !parent.has(id) || parent.get(id) !== parent.get(over)) return null
  const group = all.filter((p) => parent.get(p.id) === parent.get(id)).map((p) => p.id)
  const from = group.indexOf(id)
  const to = group.indexOf(over)
  group.splice(to, 0, ...group.splice(from, 1))
  // Put the reordered siblings back into the slots the group held.
  const slots = new Set(group)
  let n = 0
  return all.map((p) => (slots.has(p.id) ? group[n++] : p.id))
}

// ---- known agents (WP-D15) ----

/** An agent identity seen at work (runs, claims) or one of your agent tokens. */
export interface KnownAgent {
  identity: string
  provider: string
  /** The model without provider or version: claude-opus, claude-code. */
  family: string
  /** Models its runs reported (Claude Code names the session's model). */
  models: string[]
  runs: number
  holding: number
  token: boolean
  last_seen: string | null
}

export const knownAgentsQuery = () =>
  queryOptions({
    queryKey: ['agents', 'known'],
    queryFn: () => api.get<KnownAgent[]>('/agents'),
    staleTime: 60_000,
  })

const normalizeAgent = (s: string) =>
  s
    .trim()
    .toLowerCase()
    .replace(/[-._ ]/g, '')

/**
 * Whether an agent with this identity may take work meant for spec, as the
 * server decides (core.AgentMatches): "any" matches all, "" none, otherwise
 * every word of the spec must appear in the identity.
 */
export function agentMatches(spec: string, identity: string) {
  spec = spec.trim()
  const id = normalizeAgent(identity.replaceAll('/', ''))
  if (!spec || !id) return false
  if (spec.toLowerCase() === 'any') return true
  if (spec.toLowerCase() === HUMANS_ONLY) return false
  const words = spec
    .toLowerCase()
    .split(/[ /,]+/)
    .map(normalizeAgent)
    .filter(Boolean)
  return words.length > 0 && words.every((w) => id.includes(w))
}
