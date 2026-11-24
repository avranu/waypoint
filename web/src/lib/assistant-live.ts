// The server holds the one gateway connection; each tab subscribes to it over SSE.

import { useEffect, useSyncExternalStore } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import { ApiError } from './api'

/** One gateway event (Hermes's `event` notification). */
export interface GatewayEvent {
  type: string
  session_id: string
  seq?: number
  payload: Record<string, unknown> | null
}

/** A question the agent is waiting on: an approval or a clarify. */
export interface OpenRequest {
  id: string
  method: 'approval' | 'clarify' | string
  params: {
    session_id: string
    request_id?: string
    command?: string
    description?: string
    choices?: string[]
    tool_name?: string | null
    questions?: {
      qid: string
      question: string
      choices?: string[] | null
      multi_select?: boolean
    }[]
  }
}

export interface Declined {
  method: string
  params: { env_var?: string; prompt?: string; session_id?: string } | null
  at: number
}

export interface LiveState {
  state: 'connecting' | 'open' | 'down'
  error: string
  open: OpenRequest[]
  declined: Declined[]
  /** Bumped when the bridge reconnected to Hermes: live sessions must resume. */
  epoch: number
}

type Listener = (e: GatewayEvent) => void

const INITIAL: LiveState = { state: 'connecting', error: '', open: [], declined: [], epoch: 0 }

class Live {
  id: number
  qc: QueryClient | null = null
  es: EventSource | null = null
  refs = 0
  snap: LiveState = INITIAL
  subs = new Set<() => void>()
  listeners = new Set<Listener>()
  closeTimer: number | undefined
  invalidate = new Set<string>()
  flushTimer: number | undefined

  constructor(id: number) {
    this.id = id
  }

  set(p: Partial<LiveState>) {
    this.snap = { ...this.snap, ...p }
    for (const f of this.subs) f()
  }

  open() {
    window.clearTimeout(this.closeTimer)
    if (this.es) return
    const es = new EventSource(`/api/v1/assistants/${this.id}/live`)
    this.es = es
    es.addEventListener('hello', (m) => {
      const h = JSON.parse((m as MessageEvent).data) as {
        state: LiveState['state']
        error?: string
        open: { seq: number; msg: { id: string; method: string; params: OpenRequest['params'] } }[]
        reset: boolean
      }
      this.set({
        state: h.state,
        error: h.error ?? '',
        open: h.open.map((f) => ({
          id: String(f.msg.id),
          method: f.msg.method,
          params: f.msg.params,
        })),
      })
      if (h.reset) this.refetchAll()
    })
    es.onmessage = (m) => this.frame(JSON.parse(m.data))
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        // EventSource stops retrying once the server refuses (CLOSED), so retry by hand.
        this.es = null
        this.set({ state: 'down', error: 'Lost the connection to Waypoint' })
        window.setTimeout(() => this.refs > 0 && this.open(), 5000)
      } else this.set({ state: 'connecting' })
    }
  }

  close() {
    this.es?.close()
    this.es = null
  }

  frame(msg: { id?: string | number; method: string; params: Record<string, unknown> }) {
    switch (msg.method) {
      case 'event': {
        const e = msg.params as unknown as GatewayEvent
        this.signal(e.type)
        if (e.type === 'message.complete') attention()
        if (e.type === 'request.cancel') {
          const id = String((e.payload as { id?: string } | null)?.id ?? '')
          this.set({ open: this.snap.open.filter((r) => r.id !== id) })
        } else if (e.type === 'approval.cancelled') {
          const ids = new Set((e.payload as { request_ids?: string[] } | null)?.request_ids ?? [])
          this.set({
            open: this.snap.open.filter(
              (r) => r.method !== 'approval' || !ids.has(r.params.request_id ?? ''),
            ),
          })
        }
        for (const l of this.listeners) l(e)
        return
      }
      case 'bridge': {
        const p = msg.params as { state: LiveState['state']; error?: string; reconnected?: boolean }
        this.set({
          state: p.state,
          error: p.error ?? '',
          epoch: p.reconnected ? this.snap.epoch + 1 : this.snap.epoch,
          open: p.state === 'open' ? this.snap.open : [],
        })
        return
      }
      case 'bridge.answered': {
        const id = String((msg.params as { id: string }).id)
        this.set({ open: this.snap.open.filter((r) => r.id !== id) })
        return
      }
      case 'bridge.declined': {
        const d = msg.params as unknown as Omit<Declined, 'at'>
        this.set({ declined: [...this.snap.declined.slice(-4), { ...d, at: Date.now() }] })
        return
      }
      default:
        if (msg.id !== undefined && (msg.method === 'approval' || msg.method === 'clarify')) {
          const r: OpenRequest = {
            id: String(msg.id),
            method: msg.method,
            params: msg.params as OpenRequest['params'],
          }
          this.set({ open: [...this.snap.open.filter((x) => x.id !== r.id), r] })
          attention()
        }
    }
  }

  signal(type: string) {
    const keys: Record<string, string[]> = {
      'sessions.changed': ['sessions', 'messages'],
      'cron.changed': ['cron/jobs'],
      'platforms.changed': ['status', 'messaging/platforms'],
      'pairing.changed': ['pairing'],
    }
    const ks = keys[type]
    if (!ks || !this.qc) return
    for (const k of ks) this.invalidate.add(k)
    if (this.flushTimer === undefined)
      this.flushTimer = window.setTimeout(() => {
        this.flushTimer = undefined
        const want = this.invalidate
        this.invalidate = new Set()
        this.qc?.invalidateQueries({
          predicate: (q) =>
            q.queryKey[0] === 'assistant' &&
            q.queryKey[1] === this.id &&
            [...want].some((w) => String(q.queryKey[2]).startsWith(w)),
        })
      }, 400)
  }

  refetchAll() {
    this.qc?.invalidateQueries({ queryKey: ['assistant', this.id] })
  }
}

/** Marks the tab's title while it is hidden and something arrives. */
function attention() {
  if (typeof document === 'undefined' || !document.hidden || document.title.startsWith('● ')) return
  document.title = '● ' + document.title
  const back = () => {
    if (document.hidden) return
    document.title = document.title.replace(/^● /, '')
    document.removeEventListener('visibilitychange', back)
  }
  document.addEventListener('visibilitychange', back)
}

const lives = new Map<number, Live>()

function live(id: number): Live {
  let l = lives.get(id)
  if (!l) {
    l = new Live(id)
    lives.set(id, l)
  }
  return l
}

/** Keeps the assistant's stream open while mounted and returns its state. */
export function useAssistantLive(id: number): LiveState {
  const l = live(id)
  const qc = useQueryClient()
  l.qc = qc
  useEffect(() => {
    l.refs++
    l.open()
    return () => {
      l.refs--
      if (l.refs === 0) l.closeTimer = window.setTimeout(() => l.close(), 15_000)
    }
  }, [l])
  return useSyncExternalStore(
    (f) => {
      l.subs.add(f)
      return () => l.subs.delete(f)
    },
    () => l.snap,
  )
}

/** Calls fn for every gateway event while mounted. */
export function useGatewayEvents(id: number, fn: Listener) {
  useEffect(() => {
    const l = live(id)
    l.listeners.add(fn)
    return () => {
      l.listeners.delete(fn)
    }
  }, [id, fn])
}

/** Sends one request to the assistant's gateway and returns its result. */
export async function rpc<T = unknown>(id: number, method: string, params?: object): Promise<T> {
  const res = await fetch(`/api/v1/assistants/${id}/rpc`, {
    method: 'POST',
    headers: { 'X-Waypoint-Client': 'web', 'Content-Type': 'application/json' },
    body: JSON.stringify({ method, params: params ?? {} }),
    credentials: 'same-origin',
  })
  const text = await res.text()
  const data = text ? JSON.parse(text) : null
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText)
  return data as T
}

/** Answers one of the agent's questions. */
export async function answer(id: number, requestId: string, result: unknown) {
  const res = await fetch(`/api/v1/assistants/${id}/answer`, {
    method: 'POST',
    headers: { 'X-Waypoint-Client': 'web', 'Content-Type': 'application/json' },
    body: JSON.stringify({ id: requestId, result }),
    credentials: 'same-origin',
  })
  if (!res.ok) {
    const data = await res.json().catch(() => null)
    throw new ApiError(res.status, data?.error || res.statusText)
  }
  const l = lives.get(id)
  if (l) l.set({ open: l.snap.open.filter((r) => r.id !== requestId) })
}
