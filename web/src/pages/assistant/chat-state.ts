// Attach to the gateway (give a runtime id) only when the conversation is ours to drive; a session running elsewhere (schedule, Signal) is followed by refetching so it isn't taken over.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { messagesQuery, type HermesSession, type StoredMessage } from '@/lib/assistant'
import { rpc, useAssistantLive, useGatewayEvents, type GatewayEvent } from '@/lib/assistant-live'

export interface ToolItem {
  kind: 'tool'
  id: string
  name: string
  context?: string
  args?: Record<string, unknown> | null
  result?: string
  summary?: string
  duration?: number
  diff?: string
  done: boolean
  failed?: boolean
}

export type LiveItem =
  | { kind: 'text'; text: string; sealed?: boolean }
  | { kind: 'reasoning'; text: string; sealed?: boolean }
  | { kind: 'note'; text: string; tone?: 'info' | 'error' }
  | ToolItem

export interface Todo {
  id?: string
  content: string
  status: 'pending' | 'in_progress' | 'completed' | 'cancelled' | string
}

export interface Usage {
  model?: string
  input?: number
  output?: number
  total?: number
  context_used?: number | null
  context_max?: number | null
  context_percent?: number | null
  cost_usd?: number | null
  avg_tps?: number | null
}

export interface LiveTurn {
  user?: string
  items: LiveItem[]
  status?: string
  running: boolean
  /** the last stored message before this turn: the transcript shows up to it */
  baseId: number
  startedAt: number
}

export interface SessionInfo {
  model?: string
  provider?: string
  title?: string
  running?: boolean
  usage?: Usage | null
}

/** Runtime ids of conversations this tab attached, by assistant and stored id. */
const runtimes = new Map<string, string>()
const rkey = (a: number, s: string) => `${a}:${s}`

export function useConversation(assistantId: number, stored: string | null, row?: HermesSession) {
  const qc = useQueryClient()
  const live = useAssistantLive(assistantId)
  const [turn, setTurn] = useState<LiveTurn | null>(null)
  const [info, setInfo] = useState<SessionInfo>({})
  const [todos, setTodos] = useState<Todo[]>([])
  const [runtime, setRuntime] = useState<string | null>(() =>
    stored ? (runtimes.get(rkey(assistantId, stored)) ?? null) : null,
  )
  const [fresh, setFresh] = useState(false)
  const messages = useQuery({
    ...messagesQuery(assistantId, stored ?? ''),
    enabled: !!stored && !fresh,
    refetchInterval: (q) =>
      row?.is_active && !runtime && turnOpen(q.state.data?.messages) ? 3000 : false,
  })
  const open = !!row?.is_active && turnOpen(messages.data?.messages)

  const pending = useRef<((t: LiveTurn) => LiveTurn)[]>([])
  const frame = useRef<number | undefined>(undefined)
  const update = useCallback((f: (t: LiveTurn) => LiveTurn) => {
    pending.current.push(f)
    if (frame.current !== undefined) return
    frame.current = requestAnimationFrame(() => {
      frame.current = undefined
      const fs = pending.current
      pending.current = []
      setTurn((t) => (t ? fs.reduce((acc, g) => g(acc), t) : t))
    })
  }, [])

  const created = useRef<string | null>(null)
  useEffect(() => {
    if (stored && stored === created.current) return
    setTurn(null)
    setInfo({})
    setTodos([])
    setFresh(false)
    setRuntime(stored ? (runtimes.get(rkey(assistantId, stored)) ?? null) : null)
  }, [assistantId, stored])

  // The bridge reconnected to Hermes: runtime ids from before are gone.
  useEffect(() => {
    if (live.epoch === 0) return
    for (const k of [...runtimes.keys()]) if (k.startsWith(assistantId + ':')) runtimes.delete(k)
    setRuntime(null)
  }, [live.epoch, assistantId])

  const lastId = () => {
    const ms = messages.data?.messages
    return ms?.length ? ms[ms.length - 1].id : 0
  }

  /** Attaches the conversation to the gateway, returning its runtime id. */
  const attach = useCallback(
    async (s: string): Promise<string> => {
      const known = runtimes.get(rkey(assistantId, s))
      if (known) return known
      const r = await rpc<{
        session_id: string
        info?: SessionInfo
        running?: boolean
        inflight?: { assistant?: string; user?: string; streaming?: boolean } | null
        todo_state?: { todos: Todo[] } | null
      }>(assistantId, 'session.resume', {
        session_id: s,
        omit_messages: true,
        inline_images: false,
        source: 'waypoint',
      })
      runtimes.set(rkey(assistantId, s), r.session_id)
      setRuntime(r.session_id)
      if (r.info) setInfo((i) => ({ ...i, ...r.info }))
      if (r.todo_state?.todos) setTodos(r.todo_state.todos)
      if (r.running || r.inflight?.streaming) {
        const text = r.inflight?.assistant ?? ''
        setTurn({
          user: undefined,
          items: text ? [{ kind: 'text', text }] : [],
          running: true,
          baseId: Number.MAX_SAFE_INTEGER,
          startedAt: Date.now(),
        })
      }
      return r.session_id
    },
    [assistantId],
  )

  useEffect(() => {
    if (!stored || runtime || !open || row?.source !== 'waypoint') return
    if (live.state !== 'open') return
    attach(stored).catch(() => {})
  }, [stored, runtime, open, row?.source, live.state, attach])

  const finish = useCallback(async () => {
    await Promise.all([
      qc.invalidateQueries({ queryKey: ['assistant', assistantId, 'sessions'] }),
      stored ? qc.refetchQueries({ queryKey: messagesQuery(assistantId, stored).queryKey }) : null,
    ])
    setFresh(false)
    setTurn((t) => (t && !t.running ? null : t))
  }, [qc, assistantId, stored])

  const onEvent = useCallback(
    (e: GatewayEvent) => {
      if (!runtime || e.session_id !== runtime) return
      const p = (e.payload ?? {}) as Record<string, unknown>
      const str = (k: string) => (typeof p[k] === 'string' ? (p[k] as string) : '')
      switch (e.type) {
        case 'message.start':
          setTurn((t) => t ?? { items: [], running: true, baseId: lastId(), startedAt: Date.now() })
          return
        case 'message.delta':
          return update((t) => appendText(t, 'text', str('text')))
        case 'reasoning.delta':
          return update((t) => appendText(t, 'reasoning', str('text')))
        case 'reasoning.available':
          return update((t) =>
            t.items.some((i) => i.kind === 'reasoning')
              ? t
              : { ...t, items: [{ kind: 'reasoning', text: str('text') }, ...t.items] },
          )
        case 'message.interim':
          return update((t) =>
            p.already_streamed ? sealText(t) : sealText(appendText(t, 'text', str('text'))),
          )
        case 'status.update':
          return update((t) => ({ ...t, status: str('text') }))
        case 'tool.generating':
          return update((t) => ({ ...t, status: `Preparing ${str('name')}…` }))
        case 'tool.start':
          return update((t) => ({
            ...sealText(t),
            status: undefined,
            items: [
              ...sealText(t).items,
              {
                kind: 'tool',
                id: str('tool_id'),
                name: str('name'),
                context: str('context') || str('preview') || undefined,
                args: (p.args as Record<string, unknown>) ?? null,
                done: false,
              },
            ],
          }))
        case 'tool.complete':
          return update((t) => ({
            ...t,
            items: t.items.map((i) =>
              i.kind === 'tool' && i.id === str('tool_id')
                ? {
                    ...i,
                    done: true,
                    args: i.args ?? (p.args as Record<string, unknown>) ?? null,
                    result: str('result_text') || stringify(p.result),
                    summary: str('summary') || undefined,
                    duration: typeof p.duration_s === 'number' ? p.duration_s : undefined,
                    diff: str('inline_diff') || undefined,
                  }
                : i,
            ),
          }))
        case 'todo.updated':
          setTodos((p.todos as Todo[]) ?? [])
          return
        case 'session.info':
          setInfo((i) => ({ ...i, ...(p as SessionInfo) }))
          return
        case 'session.usage':
          setInfo((i) => ({ ...i, usage: (p.usage as Usage) ?? i.usage }))
          return
        case 'session.title':
          setInfo((i) => ({ ...i, title: str('title') }))
          qc.invalidateQueries({ queryKey: ['assistant', assistantId, 'sessions'] })
          return
        case 'error':
          return update((t) => ({
            ...t,
            items: [...t.items, { kind: 'note', text: str('message'), tone: 'error' }],
          }))
        case 'message.complete': {
          const status = str('status')
          if (p.usage) setInfo((i) => ({ ...i, usage: p.usage as Usage }))
          setTurn((t) => {
            if (!t) return t
            let items = t.items
            const text = typeof p.text === 'string' ? p.text : ''
            const streamed = items.some((i) => i.kind === 'text' && i.text.trim())
            if (p.response_transformed || (!streamed && text && !p.response_previewed))
              items = [...items.filter((i) => i.kind !== 'text'), { kind: 'text', text }]
            if (status === 'error' && !streamed && !text)
              items = [
                ...items,
                { kind: 'note', text: str('error') || 'The turn failed.', tone: 'error' },
              ]
            if (status === 'interrupted') items = [...items, { kind: 'note', text: 'Stopped.' }]
            return { ...t, items, running: false, status: undefined }
          })
          void finish()
          return
        }
      }
    },
    [runtime, update, finish, qc, assistantId, messages.data],
  )
  useGatewayEvents(assistantId, onEvent)

  /** Starts a conversation (when there is none yet) and sends a turn. */
  const send = useCallback(
    async (
      text: string,
      files: File[] = [],
      opts: { create?: (stored: string) => void; title?: string } = {},
    ) => {
      let s = stored
      let rt = runtime
      if (!s) {
        const r = await rpc<{ session_id: string; stored_session_id: string; info?: SessionInfo }>(
          assistantId,
          'session.create',
          { source: 'waypoint', ...(opts.title ? { title: opts.title } : {}) },
        )
        s = r.stored_session_id
        rt = r.session_id
        created.current = s
        runtimes.set(rkey(assistantId, s), rt)
        if (r.info) setInfo(r.info)
        setFresh(true)
        opts.create?.(s)
      }
      if (!rt) rt = await attach(s)
      setRuntime(rt)
      let body = text
      for (const f of files) {
        const data = await readDataURL(f)
        if (f.type.startsWith('image/') && f.type !== 'image/svg+xml') {
          await rpc(assistantId, 'image.attach_bytes', {
            session_id: rt,
            content_base64: data.slice(data.indexOf(',') + 1),
            filename: f.name,
          })
        } else {
          const r = await rpc<{ ref_text: string }>(assistantId, 'file.attach', {
            session_id: rt,
            data_url: data,
            name: f.name,
          })
          if (r.ref_text) body = body ? `${body}\n${r.ref_text}` : r.ref_text
        }
      }
      setTurn((t) =>
        t?.running
          ? t
          : {
              user: text || files.map((f) => f.name).join(', '),
              items: [],
              running: true,
              baseId: lastId(),
              startedAt: Date.now(),
            },
      )
      const r = await rpc<{ status?: string }>(assistantId, 'prompt.submit', {
        session_id: rt,
        text: body,
      })
      if (r.status === 'queued' || r.status === 'steered')
        update((t) => ({
          ...t,
          items: [
            ...t.items,
            {
              kind: 'note',
              text: r.status === 'queued' ? `Queued: “${text}”` : `Steering with: “${text}”`,
            },
          ],
        }))
      return s
    },
    [assistantId, stored, runtime, attach, update, messages.data],
  )

  /** Sends text again in place of the user turn at rowId and what followed (edit, retry). */
  const resend = useCallback(
    async (text: string, rowId: number) => {
      if (!stored) return
      const rt = runtime ?? (await attach(stored))
      setTurn({ user: text, items: [], running: true, baseId: rowId - 1, startedAt: Date.now() })
      try {
        await rpc(assistantId, 'prompt.submit', {
          session_id: rt,
          text,
          truncate_before_row_id: rowId,
          confirm_truncate: true,
          confirm_empty_truncate: true,
        })
      } catch (e) {
        setTurn(null)
        throw e
      }
    },
    [assistantId, stored, runtime, attach],
  )

  const stop = useCallback(async () => {
    if (runtime) await rpc(assistantId, 'session.interrupt', { session_id: runtime })
  }, [assistantId, runtime])

  /** Runs a slash command in this conversation and shows what it said. */
  const slash = useCallback(
    async (command: string) => {
      if (!stored) return
      const rt = runtime ?? (await attach(stored))
      const r = await rpc<{ output?: string; message?: string; warning?: string; notice?: string }>(
        assistantId,
        'slash.exec',
        { session_id: rt, command },
      )
      const text = [r.output, r.message, r.notice, r.warning].filter(Boolean).join('\n')
      setTurn((t) => ({
        user: command,
        items: [...(t?.items ?? []), { kind: 'note', text: text || 'Done.' }],
        running: false,
        baseId: t?.baseId ?? lastId(),
        startedAt: t?.startedAt ?? Date.now(),
      }))
      void finish()
    },
    [assistantId, stored, runtime, attach, finish, messages.data],
  )

  const shown = useMemo(() => {
    const ms = messages.data?.messages ?? []
    return turn ? ms.filter((m) => m.id <= turn.baseId) : ms
  }, [messages.data, turn])

  return {
    messages: shown,
    loading: !!stored && !fresh && messages.isPending,
    error: messages.error,
    turn,
    info,
    todos,
    runtime,
    running: !!turn?.running || (open && !runtime),
    send,
    resend,
    stop,
    slash,
    attach,
  }
}

/** The transcript ends mid-turn: a question, or tool calls without their answer. */
function turnOpen(ms?: StoredMessage[]): boolean {
  const last = ms?.filter((m) => m.display_kind !== 'hidden').at(-1)
  if (!last) return false
  return (
    last.role === 'user' ||
    last.role === 'tool' ||
    (last.role === 'assistant' && !!last.tool_calls?.length)
  )
}

function appendText(t: LiveTurn, kind: 'text' | 'reasoning', text: string): LiveTurn {
  if (!text) return t
  const items = [...t.items]
  const last = items[items.length - 1]
  if (last && last.kind === kind && !last.sealed)
    items[items.length - 1] = { ...last, text: last.text + text }
  else items.push({ kind, text })
  return { ...t, items, status: kind === 'text' ? undefined : t.status }
}

/** Ends the current text segment, so later text starts a new one. */
function sealText(t: LiveTurn): LiveTurn {
  const last = t.items[t.items.length - 1]
  if (!last || (last.kind !== 'text' && last.kind !== 'reasoning')) return t
  const items = [...t.items]
  items[items.length - 1] = { ...last, sealed: true }
  return { ...t, items }
}

function stringify(v: unknown): string {
  if (v === undefined || v === null) return ''
  if (typeof v === 'string') return v
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

function readDataURL(f: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader()
    r.onload = () => resolve(String(r.result))
    r.onerror = () => reject(r.error)
    r.readAsDataURL(f)
  })
}

export type Row =
  | { kind: 'user'; id: number; text: string; images: number; at: number }
  | {
      kind: 'assistant'
      id: number
      text: string
      reasoning?: string
      tools: ToolItem[]
      at: number
    }
  | { kind: 'note'; id: number; text: string; at: number }

/** Turns stored messages into what a person reads: tool results folded into their calls. */
export function toRows(ms: StoredMessage[]): Row[] {
  const results = new Map<string, StoredMessage>()
  for (const m of ms) if (m.role === 'tool' && m.tool_call_id) results.set(m.tool_call_id, m)
  const used = new Set<string>()
  const rows: Row[] = []
  for (const m of ms) {
    if (m.display_kind === 'hidden' || m.active === 0) continue
    const text = m.display_content ?? contentText(m.content)
    if (m.role === 'user') {
      if (text.startsWith('[System:')) continue
      rows.push({ kind: 'user', id: m.id, text, images: imageCount(m.content), at: m.timestamp })
    } else if (m.role === 'assistant') {
      const tools: ToolItem[] = (m.tool_calls ?? []).map((c) => {
        const r = results.get(c.id)
        if (r) used.add(c.id)
        return {
          kind: 'tool',
          id: c.id,
          name: c.function?.name ?? 'tool',
          args: parseArgs(c.function?.arguments),
          result: r ? contentText(r.content) : undefined,
          done: !!r,
        }
      })
      const reasoning = m.reasoning || m.reasoning_content || undefined
      if (!text.trim() && !tools.length && !reasoning) continue
      rows.push({ kind: 'assistant', id: m.id, text, reasoning, tools, at: m.timestamp })
    } else if (m.role === 'tool' && m.tool_call_id && !used.has(m.tool_call_id)) {
      rows.push({
        kind: 'assistant',
        id: m.id,
        text: '',
        tools: [
          {
            kind: 'tool',
            id: m.tool_call_id,
            name: m.tool_name ?? 'tool',
            result: contentText(m.content),
            done: true,
          },
        ],
        at: m.timestamp,
      })
    } else if (m.role === 'system' && m.display_kind) {
      rows.push({ kind: 'note', id: m.id, text, at: m.timestamp })
    }
  }
  return rows
}

function contentText(c: StoredMessage['content']): string {
  if (!c) return ''
  if (typeof c === 'string') return c
  return c
    .map((p) => (p.type === 'text' ? (p.text ?? '') : ''))
    .join('')
    .trim()
}

function imageCount(c: StoredMessage['content']): number {
  return Array.isArray(c) ? c.filter((p) => p.type === 'image_url').length : 0
}

function parseArgs(a?: string): Record<string, unknown> | null {
  if (!a) return null
  try {
    const v = JSON.parse(a)
    return v && typeof v === 'object' ? v : { value: v }
  } catch {
    return { arguments: a }
  }
}
