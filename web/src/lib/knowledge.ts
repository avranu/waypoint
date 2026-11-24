// A project's glossary and design notes are its own plus its ancestors' plus the global ones.

import { queryOptions, type QueryClient } from '@tanstack/react-query'
import { api, enc, qs } from './api'
import type {
  AllResult,
  DesignKind,
  DesignNote,
  GlossaryEntry,
  GlossaryLookup,
  GoalSuggestResult,
  GoalWorkList,
  GoalsOverview,
  Grounding,
  ItemTrace,
  KItem,
  KLink,
  LinkEnd,
  LinkRel,
  Project,
  ReviewItem,
  Spec,
  TaskGoalSuggestion,
  TextRef,
} from './types'

const MIN = 60_000

export interface GlossaryParams {
  /** a key: its full glossary (own + ancestors + global); "global" for the global one */
  project?: string
  own?: boolean
  q?: string
}

export const glossaryQuery = (p: GlossaryParams) =>
  queryOptions({
    queryKey: ['glossary', 'list', p],
    queryFn: () => api.get<GlossaryEntry[]>(`/glossary${qs({ ...p, limit: 1000 })}`),
    staleTime: 30_000,
  })

export const glossaryEntryQuery = (id: number) =>
  queryOptions({
    queryKey: ['glossary', 'entry', id],
    queryFn: () => api.get<GlossaryEntry>(`/glossary/${id}`),
  })

export const glossaryLookupQuery = (term: string, project?: string) =>
  queryOptions({
    queryKey: ['glossary', 'lookup', term, project ?? ''],
    queryFn: () => api.get<GlossaryLookup>(`/glossary/lookup${qs({ term, project })}`),
    staleTime: 30_000,
  })

export interface DesignParams {
  project?: string
  own?: boolean
  kind?: string
  /** '' = active; a status; "archived"; "all" */
  status?: string
  q?: string
  pending?: boolean
}

export const designQuery = (p: DesignParams) =>
  queryOptions({
    queryKey: ['design', 'list', p],
    queryFn: () => api.get<DesignNote[]>(`/design${qs({ ...p, limit: 1000 })}`),
    staleTime: 30_000,
  })

export const designNoteQuery = (id: number) =>
  queryOptions({
    queryKey: ['design', 'note', id],
    queryFn: () => api.get<DesignNote>(`/design/${id}`),
  })

/** What waits for a person; cheap enough for the nav badge. */
export const designReviewQuery = (project = '') =>
  queryOptions({
    queryKey: ['design', 'review', project],
    queryFn: () => api.get<ReviewItem[]>(`/design/review${qs({ project })}`),
    staleTime: MIN,
  })

export const specQuery = (topic: string, project?: string) =>
  queryOptions({
    queryKey: ['spec', topic, project ?? ''],
    queryFn: () => api.get<Spec>(`/spec${qs({ topic, project })}`),
  })

/** The spec as text, exactly what `wp spec` shows an agent. */
export const specTextQuery = (topic: string, project?: string) =>
  queryOptions({
    queryKey: ['spec', topic, project ?? '', 'text'],
    queryFn: () => api.get<string>(`/spec${qs({ topic, project, format: 'text' })}`),
  })

/** An item's links both ways. Issues go by key. */
export const linksQuery = (at: LinkEnd) =>
  queryOptions({
    queryKey: ['links', at.type, at.key ?? at.id],
    queryFn: () =>
      api.get<KLink[]>(
        `/links${qs(at.type === 'issue' ? { type: 'issue', key: at.key } : { type: at.type, id: at.id })}`,
      ),
    staleTime: 15_000,
  })

/** A project's goals, what serves them, and what traces to none (WP-382). */
export const goalsQuery = (project: string) =>
  queryOptions({
    queryKey: ['goals', project],
    queryFn: () => api.get<GoalsOverview>(`/goals${qs({ project })}`),
    staleTime: 30_000,
  })

/** How an item reaches a goal, like git blame for intent. Issues go by key. */
export const traceQuery = (ref: string, project?: string) =>
  queryOptions({
    queryKey: ['trace', ref, project ?? ''],
    queryFn: () => api.get<ItemTrace>(`/trace${qs({ ref, project })}`),
    staleTime: 30_000,
  })

/**
 * Words like `assignee:me` or `is:open`: the issue query language. A
 * project's request code (API:R-28) looks like one but is not.
 */
export const hasFilters = (s: string) =>
  /(^|\s)-?[a-z_]+:\S/i.test(s.replace(/(^|\s)[a-z0-9_.-]+:r-?\d+(?=\s|$)/gi, ' '))

export interface SearchAllParams {
  q: string
  kinds?: string
  project?: string
  /** the project being looked at: of several things a code names, its own comes first */
  near?: string
  archived?: boolean
  limit?: number
}

export const searchAllQuery = (p: SearchAllParams) =>
  queryOptions({
    queryKey: ['searchAll', p],
    queryFn: () => api.get<AllResult>(`/search/all${qs({ ...p })}`),
    staleTime: 15_000,
  })

/** What a [[wikilink]] points at (null when nothing does). */
export const resolveQuery = (ref: string, project?: string) =>
  queryOptions({
    queryKey: ['resolve', ref, project ?? ''],
    queryFn: async () =>
      (await api.get<KItem | undefined>(`/resolve${qs({ ref, project, soft: 1 })}`)) ?? null,
    retry: false,
  })

/** What the codes in a text name (issues, requests, decisions, pages, pull requests, terms). */
export const refsQuery = (text: string, project?: string) =>
  queryOptions({
    queryKey: ['refs', project ?? '', text],
    queryFn: async () =>
      (await api.post<{ refs: TextRef[] }>('/refs', { text, project: project ?? '' })).refs,
    staleTime: MIN,
    retry: false,
  })

/** Refetch everything knowledge-shaped (after a change, or a live event). */
export function invalidateKnowledge(qc: QueryClient) {
  for (const k of [
    'glossary',
    'design',
    'links',
    'spec',
    'searchAll',
    'resolve',
    'refs',
    'usage',
    'goals',
    'trace',
  ])
    qc.invalidateQueries({ queryKey: [k] })
}

export const glossaryApi = {
  create: (body: {
    term: string
    definition?: string
    aliases?: string[]
    project?: string
    locked?: boolean
    links?: string[]
  }) => api.post<GlossaryEntry>('/glossary', body),
  update: (id: number, body: Record<string, unknown>) =>
    api.patch<GlossaryEntry>(`/glossary/${id}`, body),
  remove: (id: number) => api.del(`/glossary/${id}`),
}

export const designApi = {
  create: (body: {
    title: string
    kind?: string
    body?: string
    project?: string
    parent?: number
    status?: string
  }) => api.post<DesignNote>('/design', body),
  update: (id: number, body: Record<string, unknown>) =>
    api.patch<DesignNote>(`/design/${id}`, body),
  remove: (id: number) => api.del(`/design/${id}`),
  /** version: the updated_at the person saw; a 409 says the note changed since. */
  approve: (id: number, version?: string) =>
    api.post<DesignNote>(`/design/${id}/approve`, version ? { version } : undefined),
  review: (rid: number, action: 'approve' | 'reject' | 'merge', note = '') =>
    api.post<DesignNote>(`/design/revisions/${rid}/${action}`, note ? { note } : undefined),
}

export const linkApi = {
  add: (from: LinkEnd, to: LinkEnd, rel: LinkRel = 'relates') =>
    api.post<KLink>('/links', { from, to, rel }),
  /** Page↔issue links live with pages; the rest go by id. */
  remove: (at: LinkEnd, l: KLink) => {
    if (l.via === 'page_issues') {
      const page = at.type === 'page' ? at.id : l.item.id
      const key = at.type === 'issue' ? at.key : l.item.key
      return api.del(`/pages/${page}/issues/${enc(key ?? '')}`)
    }
    return api.del(`/links/${l.id}`)
  },
}

/** Goals (WP-382): confirm an agent's link or a guess as yours, or drop it. */
export const goalApi = {
  confirm: (linkID: number) => api.post<KLink>(`/goals/links/${linkID}/confirm`),
  dismiss: (linkID: number) => api.post<void>(`/goals/links/${linkID}/dismiss`),
  answerDrift: (flagID: number, answer: 'intent' | 'stop') =>
    api.post<void>(`/goals/drift/${flagID}/answer`, { answer }),
  /** stop listing these links as waiting; they keep their basis (not a confirm) */
  acknowledge: (ids: number[]) =>
    api.post<{ acknowledged: number }>('/goals/links/acknowledge', { ids }),
  /** this item doesn't serve that goal: stop offering it */
  dismissMatch: (item: LinkEnd, goal: number) =>
    api.post<void>('/goals/dismiss-match', { item, goal }),
  /** the item serves the goal, as a person's link */
  link: (item: LinkEnd, goal: number) => linkApi.add(item, { type: 'design', id: goal }, 'serves'),
  /** a model reads people's untraced tasks and proposes goals and clearer words; each waits for a person */
  suggest: (project: string) => api.post<GoalSuggestResult>(`/goals/suggest${qs({ project })}`),
  /** a model names the goal one task serves, or a new one; nothing is saved */
  suggestForTask: (key: string) => api.post<TaskGoalSuggestion>(`/issues/${enc(key)}/goal-suggest`),
}

/** Open tasks likeliest to serve a goal, or (with q) any open task matching q. */
export const goalWorkQuery = (goal: number, project: string, q: string) =>
  queryOptions({
    queryKey: ['goals', 'work', goal, project, q],
    queryFn: () => api.get<GoalWorkList>(`/goals/${goal}/work${qs({ project, q })}`),
    staleTime: 15_000,
  })

export const DESIGN_KIND_LABEL: Record<DesignKind, string> = {
  goal: 'Goal',
  principle: 'Principle',
  feature: 'Feature',
  screen: 'Screen',
  decision: 'Decision',
  requirement: 'Requirement',
  note: 'Note',
}

/** Plural headings for the design list. */
export const DESIGN_KIND_PLURAL: Record<DesignKind, string> = {
  goal: 'Goals',
  principle: 'Principles',
  feature: 'Features',
  screen: 'Screens',
  decision: 'Decisions',
  requirement: 'Requirements',
  note: 'Notes',
}

/** "relates" seen from either end: out from this item, or in from the other. */
export const REL_LABEL: Record<LinkRel, { out: string; in: string }> = {
  relates: { out: 'Relates to', in: 'Related to' },
  implements: { out: 'Implements', in: 'Implemented by' },
  serves: { out: 'Serves', in: 'Served by' },
  depends_on: { out: 'Depends on', in: 'Needed by' },
  supersedes: { out: 'Supersedes', in: 'Superseded by' },
  tension: { out: 'In tension with', in: 'In tension with' },
  mentions: { out: 'Mentions', in: 'Mentioned in' },
}

/** Groundings in a person's words, short enough for a chip. */
export const GROUNDING_LABEL: Record<Grounding, string> = {
  person_goal: 'Approved goal',
  // The three agent-made groundings share the label 'Agent' on purpose; the Goals page groups them too.
  agent_goal: 'Agent',
  inferred_goal: 'Agent',
  proposed_goal: 'Agent',
  person_request: 'Filed by a person',
  none: 'No goal',
}

/** What each agent-made kind is, for a hover that counts them apart. */
export const GROUNDING_DETAIL: Record<Grounding, string> = {
  ...GROUNDING_LABEL,
  agent_goal: "agent's link",
  inferred_goal: 'guessed goal',
  proposed_goal: 'suggested goal',
}

/**
 * The groundings as the Goals page shows them: four groups in the order of
 * the legend and of every bar. The three agent-made kinds (an agent's link,
 * Waypoint's guess, a goal nobody approved yet) are one group there; a
 * person rarely needs the difference at that level.
 */
export type GroundGroupId = 'approved' | 'asked' | 'agent' | 'none'

export interface GroundGroup {
  id: GroundGroupId
  /** short: the Goals page already says these are about goals */
  label: string
  /** what it means, for a tooltip */
  hint: string
  members: Grounding[]
}

export const GROUND_GROUPS: GroundGroup[] = [
  {
    id: 'approved',
    label: 'Approved',
    hint: 'Traces to a goal a person approved, through links people made',
    members: ['person_goal'],
  },
  {
    id: 'asked',
    label: 'Filed by a person',
    hint: 'A person filed it or asked for it, but it is not linked to a goal',
    members: ['person_request'],
  },
  {
    id: 'agent',
    label: 'Agent',
    hint: 'Traces to a goal only through an agent’s link, Waypoint’s guess, or a goal no person approved yet',
    members: ['agent_goal', 'inferred_goal', 'proposed_goal'],
  },
  {
    id: 'none',
    label: 'No goal',
    hint: 'Traces to no goal, and no person wrote or approved anything it hangs from',
    members: ['none'],
  },
]

export const groundGroupOf = (g: Grounding): GroundGroup =>
  GROUND_GROUPS.find((x) => x.members.includes(g)) ?? GROUND_GROUPS[3]

/** Counts by grounding, summed into the groups (only groups with work). */
export function groupCounts(
  counts: Partial<Record<Grounding, number>>,
): { group: GroundGroup; n: number }[] {
  return GROUND_GROUPS.map((group) => ({
    group,
    n: group.members.reduce((t, m) => t + (counts[m] ?? 0), 0),
  })).filter((x) => x.n > 0)
}

/** Tone for a grounding: people's work is calm, untraced work stands out. */
export const GROUNDING_TONE: Record<Grounding, 'ok' | 'soft' | 'warn' | 'bad'> = {
  person_goal: 'ok',
  person_request: 'ok',
  agent_goal: 'soft',
  inferred_goal: 'soft',
  proposed_goal: 'warn',
  none: 'bad',
}

/** Where an item lives, for chips: the project key, or "global". */
export const scopeLabel = (s: { project_key?: string; project_id?: number | null }) =>
  s.project_key || (s.project_id ? '?' : 'global')

/** A project followed by its ancestors (nearest first), from the projects list. */
export function projectChain(projects: Project[], key: string): Project[] {
  const byId = new Map(projects.map((p) => [p.id, p]))
  const out: Project[] = []
  let p = projects.find((x) => x.key === key)
  while (p && out.length < 20) {
    out.push(p)
    p = p.parent_id ? byId.get(p.parent_id) : undefined
  }
  return out
}

export const canWrite = (p: Pick<Project, 'role'>) => p.role === 'owner' || p.role === 'editor'

/** Where an item opens in the app. */
export function itemHref(it: Pick<KItem, 'type' | 'id' | 'key' | 'ref'>): string | null {
  switch (it.type) {
    case 'issue':
      return it.key ? `/issue/${it.key}` : null
    case 'page':
      return `/pages/${it.id}`
    case 'glossary':
      return `/glossary/${it.id}`
    case 'design':
      return `/design/${it.id}`
    case 'request':
      return it.key ? `/requests/${it.key}` : null
  }
  return null
}

const ESC: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
}
const escapeHTML = (s: string) => s.replace(/[&<>"']/g, (c) => ESC[c])

/**
 * A search snippet as HTML: the server marks matches ⟦like this⟧; those
 * become <mark>, everything else is escaped, and [[wikilink]] brackets go.
 */
export function snippetHTML(snippet: string): string {
  const text = snippet.replace(/\[\[([^\]|]*)(?:\|([^\]]*))?\]\]/g, (_m, t, l) => l || t)
  return text
    .split(/(⟦[^⟧]*⟧)/)
    .map((part) =>
      part.startsWith('⟦') && part.endsWith('⟧')
        ? `<mark>${escapeHTML(part.slice(1, -1))}</mark>`
        : escapeHTML(part),
    )
    .join('')
}

export interface DiffLine {
  op: ' ' | '+' | '-'
  text: string
}

/**
 * A line diff (longest common subsequence) from a to b. Long texts fall
 * back to "all removed, all added" rather than a slow table.
 */
export function lineDiff(a: string, b: string): DiffLine[] {
  const x = a.split('\n')
  const y = b.split('\n')
  let head = 0
  while (head < x.length && head < y.length && x[head] === y[head]) head++
  let tail = 0
  while (
    tail < x.length - head &&
    tail < y.length - head &&
    x[x.length - 1 - tail] === y[y.length - 1 - tail]
  )
    tail++
  const xs = x.slice(head, x.length - tail)
  const ys = y.slice(head, y.length - tail)
  const mid: DiffLine[] = []
  if (xs.length * ys.length > 1_000_000) {
    mid.push(...xs.map((t) => ({ op: '-' as const, text: t })))
    mid.push(...ys.map((t) => ({ op: '+' as const, text: t })))
  } else {
    const n = xs.length
    const m = ys.length
    const lcs = new Int32Array((n + 1) * (m + 1))
    for (let i = n - 1; i >= 0; i--)
      for (let j = m - 1; j >= 0; j--)
        lcs[i * (m + 1) + j] =
          xs[i] === ys[j]
            ? lcs[(i + 1) * (m + 1) + j + 1] + 1
            : Math.max(lcs[(i + 1) * (m + 1) + j], lcs[i * (m + 1) + j + 1])
    let i = 0
    let j = 0
    while (i < n && j < m) {
      if (xs[i] === ys[j]) {
        mid.push({ op: ' ', text: xs[i] })
        i++
        j++
      } else if (lcs[(i + 1) * (m + 1) + j] >= lcs[i * (m + 1) + j + 1]) {
        mid.push({ op: '-', text: xs[i++] })
      } else {
        mid.push({ op: '+', text: ys[j++] })
      }
    }
    while (i < n) mid.push({ op: '-', text: xs[i++] })
    while (j < m) mid.push({ op: '+', text: ys[j++] })
  }
  return [
    ...x.slice(0, head).map((t) => ({ op: ' ' as const, text: t })),
    ...mid,
    ...x.slice(x.length - tail).map((t) => ({ op: ' ' as const, text: t })),
  ]
}
