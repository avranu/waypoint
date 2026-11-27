import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, api, enc, qs } from './api'
import type { UserRef } from './types'

export interface GateApproval {
  code: string
  tool: string
  summary: string
  status: 'pending' | 'approved' | 'denied' | 'expired' | 'done' | 'failed' | string
  created: number
  decided: number | null
  result: string | null
}

export interface AssistantState {
  reachable: boolean
  frozen: boolean
  signal: boolean
  pending: GateApproval[]
  error?: string
  checked_at: string
  down_since?: string
}

export interface Assistant {
  id: number
  name: string
  owner: UserRef
  url: string
  has_ca: boolean
  username: string
  /** As set on this assistant; '' uses the site's, in `site`. */
  key_header: string
  agent_url: string
  site: AssistantSite
  token_id: number | null
  token?: string
  agent?: string
  state: AssistantState
  mine: boolean
  created_at: string
  updated_at: string
}

/** The site's settings for every assistant's gate without its own. */
export interface AssistantSite {
  /** The header the gate's door reads its key from. */
  key_header: string
  /** The gate as its agent reaches it; '' when the site sets none. */
  agent_url: string
}

export interface AssistantInput {
  name?: string
  owner_id?: number
  url?: string
  ca_pem?: string
  key?: string
  key_header?: string
  agent_url?: string
  username?: string
  password?: string
  token_id?: number
}

export interface GateConnection {
  name: string
  kind: 'mcp' | 'http'
  url: string
  description: string
  policy: Record<string, string[]>
  header_names: string[]
  updated?: number
}

export interface AssistantLink {
  id: number
  assistant_id: number
  assistant: string
  session_id: string
  title: string
  issue?: { id: number; key: string; title: string; type: string }
  created_at: string
}

/** How an assistant is doing, in one word, for dots and pills. */
export type Health = 'ok' | 'waiting' | 'frozen' | 'down' | 'unknown'

export function health(a: Pick<Assistant, 'state'>): Health {
  const s = a.state
  if (!s.checked_at || s.checked_at.startsWith('0001')) return 'unknown'
  if (!s.reachable) return 'down'
  if (s.frozen) return 'frozen'
  if (s.pending.length) return 'waiting'
  return 'ok'
}

export const HEALTH_COLOR: Record<Health, string> = {
  ok: 'var(--wp-ok)',
  waiting: 'var(--wp-warn)',
  frozen: 'var(--wp-danger)',
  down: 'var(--wp-danger)',
  unknown: 'var(--wp-subtle)',
}

export const HEALTH_LABEL: Record<Health, string> = {
  ok: 'Running',
  waiting: 'Waiting on Signal',
  frozen: 'Frozen',
  down: 'Not answering',
  unknown: 'Checking…',
}

export const assistantsQuery = () =>
  queryOptions({
    queryKey: ['assistants'],
    queryFn: () => api.get<Assistant[]>('/assistants'),
    staleTime: 30_000,
  })

export const assistantQuery = (id: number) =>
  queryOptions({
    queryKey: ['assistants', id],
    queryFn: () => api.get<Assistant>(`/assistants/${id}`),
    staleTime: 15_000,
  })

export const approvalsQuery = (id: number, limit = 100) =>
  queryOptions({
    queryKey: ['assistant', id, 'gate', 'approvals', limit],
    queryFn: () =>
      api.get<{ pending: GateApproval[]; recent: GateApproval[] }>(
        `/assistants/${id}/gate/approvals${qs({ limit })}`,
      ),
    staleTime: 10_000,
  })

export type AuditEntry = { ts: string; kind: string } & Record<string, unknown>

export const gateAuditQuery = (id: number, limit = 300, kind = '') =>
  queryOptions({
    queryKey: ['assistant', id, 'gate', 'audit', limit, kind],
    queryFn: () => api.get<AuditEntry[]>(`/assistants/${id}/gate/audit${qs({ limit, kind })}`),
    staleTime: 10_000,
  })

export const GATE_LOGS = ['gatekeeper', 'bridge', 'squid', 'caddy', 'scraper'] as const

export const gateLogsQuery = (id: number, name: string, lines = 300) =>
  queryOptions({
    queryKey: ['assistant', id, 'gate', 'logs', name, lines],
    queryFn: () =>
      api.get<{ name: string; lines: string[] }>(
        `/assistants/${id}/gate/logs/${enc(name)}${qs({ lines })}`,
      ),
    staleTime: 5_000,
  })

export const connectionsQuery = (id: number) =>
  queryOptions({
    queryKey: ['assistant', id, 'gate', 'connections'],
    queryFn: () => api.get<GateConnection[]>(`/assistants/${id}/gate/connections`),
    staleTime: 15_000,
  })

export const assistantLinksQuery = (id: number, session = '') =>
  queryOptions({
    queryKey: ['assistant', id, 'links', session],
    queryFn: () => api.get<AssistantLink[]>(`/assistants/${id}/links${qs({ session })}`),
  })

export const issueAssistantLinksQuery = (key: string) =>
  queryOptions({
    queryKey: ['issueAssistantLinks', key],
    queryFn: () => api.get<AssistantLink[]>(`/issues/${enc(key)}/assistant-links`),
  })

export function useAssistantMutations(id?: number) {
  const qc = useQueryClient()
  const refresh = () => qc.invalidateQueries({ queryKey: ['assistants'] })
  return {
    create: useMutation({
      mutationFn: (b: AssistantInput) => api.post<Assistant>('/assistants', b),
      onSuccess: refresh,
    }),
    update: useMutation({
      mutationFn: (b: AssistantInput) => api.patch<Assistant>(`/assistants/${id}`, b),
      onSuccess: refresh,
    }),
    remove: useMutation({
      mutationFn: () => api.del(`/assistants/${id}`),
      onSuccess: refresh,
    }),
    freeze: useMutation({
      mutationFn: () => api.post<Assistant>(`/assistants/${id}/freeze`),
      onSuccess: refresh,
    }),
    check: useMutation({
      mutationFn: () => api.post<Assistant>(`/assistants/${id}/check`),
      onSuccess: refresh,
    }),
    test: useMutation({
      mutationFn: () =>
        api.post<{ name: string; ok: boolean; error?: string }[]>(`/assistants/${id}/test`),
    }),
  }
}

/**
 * A request to Hermes's API (`path` is under /api/, without it). Hermes
 * reports errors as {detail}; Waypoint as {error}.
 */
export async function hermes<T = unknown>(
  id: number,
  method: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE',
  path: string,
  body?: unknown,
  params?: Record<string, string | number | boolean | undefined | null>,
): Promise<T> {
  const headers: Record<string, string> = { 'X-Waypoint-Client': 'web' }
  let payload: BodyInit | undefined
  if (body instanceof FormData || body instanceof Blob) payload = body
  else if (body !== undefined) {
    headers['Content-Type'] = 'application/json'
    payload = JSON.stringify(body)
  }
  const res = await fetch(`/api/v1/assistants/${id}/api/${path}${qs(params ?? {})}`, {
    method,
    headers,
    body: payload,
    credentials: 'same-origin',
  })
  const text = await res.text()
  let data: unknown = undefined
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!res.ok) throw new ApiError(res.status, errorText(data) || res.statusText)
  return data as T
}

function errorText(data: unknown): string {
  if (!data || typeof data !== 'object') return typeof data === 'string' ? data.slice(0, 300) : ''
  const d = data as { error?: unknown; detail?: unknown; message?: unknown }
  for (const v of [d.error, d.detail, d.message]) {
    if (typeof v === 'string' && v) return v
    if (v && typeof v === 'object') {
      const m = (v as { message?: unknown }).message
      if (typeof m === 'string') return m
      if (Array.isArray(v))
        return v.map((x) => (x as { msg?: string }).msg ?? JSON.stringify(x)).join('; ')
      return JSON.stringify(v)
    }
  }
  return ''
}

export const hget = <T>(
  id: number,
  path: string,
  params?: Record<string, string | number | boolean | undefined | null>,
) => hermes<T>(id, 'GET', path, undefined, params)

/** A query of Hermes's API; its key starts ['assistant', id, path]. */
export const hermesQuery = <T>(
  id: number,
  path: string,
  params?: Record<string, string | number | boolean | undefined | null>,
  staleTime = 15_000,
) =>
  queryOptions({
    queryKey: ['assistant', id, path, params ?? {}],
    queryFn: () => hget<T>(id, path, params),
    staleTime,
  })

export interface HermesSession {
  id: string
  source: string
  model: string | null
  title: string | null
  preview: string
  started_at: number
  ended_at: number | null
  end_reason: string | null
  last_active: number
  message_count: number
  tool_call_count: number
  input_tokens: number
  output_tokens: number
  estimated_cost_usd?: number | null
  archived: boolean
  pinned: boolean
  unread: boolean
  is_active: boolean
  parent_session_id: string | null
  _branched_from?: string
}

export interface SessionPage {
  sessions: HermesSession[]
  total: number
  limit: number
  offset: number
}

export interface SearchHit extends Partial<HermesSession> {
  session_id: string
  snippet: string
  role: string | null
}

export type ContentPart = { type: string; text?: string; image_url?: { url: string } }

export interface StoredMessage {
  id: number
  session_id: string
  role: 'system' | 'user' | 'assistant' | 'tool'
  content: string | ContentPart[] | null
  display_content?: string
  tool_calls?: { id: string; type: string; function: { name: string; arguments: string } }[] | null
  tool_call_id?: string | null
  tool_name?: string | null
  timestamp: number
  token_count?: number | null
  finish_reason?: string | null
  reasoning?: string | null
  reasoning_content?: string | null
  active?: number
  display_kind?: string | null
}

export const sessionsQuery = (
  id: number,
  f: { archived?: 'exclude' | 'only' | 'include'; source?: string; limit?: number } = {},
) =>
  hermesQuery<SessionPage>(
    id,
    'sessions',
    {
      limit: f.limit ?? 100,
      order: 'recent',
      archived: f.archived ?? 'exclude',
      source: f.source,
      min_messages: 1,
    },
    10_000,
  )

export const sessionSearchQuery = (id: number, q: string) =>
  hermesQuery<{ results: SearchHit[] }>(id, 'sessions/search', { q, limit: 30 }, 10_000)

export const messagesQuery = (id: number, session: string) =>
  queryOptions({
    queryKey: ['assistant', id, 'messages', session],
    queryFn: () =>
      hget<{ session_id: string; messages: StoredMessage[] }>(
        id,
        `sessions/${enc(session)}/messages`,
        {
          inline_images: false,
        },
      ),
    staleTime: 5_000,
  })

/** Where Hermes's `source` came from, in words. */
export function sourceLabel(source: string): string {
  const s = source.toLowerCase()
  if (s === 'waypoint') return 'Waypoint'
  if (s === 'cron') return 'Schedule'
  if (s === 'cli' || s === 'tui') return 'Terminal'
  if (s === 'desktop') return 'Desktop'
  if (s === 'subagent') return 'Sub-agent'
  return s.charAt(0).toUpperCase() + s.slice(1).replace(/_/g, ' ')
}

/** Hermes's epoch seconds as a Date ISO string, for the format helpers. */
export const iso = (sec: number | null | undefined) =>
  sec ? new Date(sec * 1000).toISOString() : undefined

export function sessionTitle(s: Pick<HermesSession, 'title' | 'preview' | 'id'>): string {
  const t = (s.title?.trim() || s.preview?.trim() || '').replace(/^#+\s*/, '')
  return t || 'Untitled conversation'
}
