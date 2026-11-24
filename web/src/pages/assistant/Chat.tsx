import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Archive,
  ArrowDown,
  ArrowUp,
  Download,
  GitBranch,
  Link2,
  MessageSquarePlus,
  Mic,
  MicOff,
  MoreHorizontal,
  PanelLeft,
  Paperclip,
  Pencil,
  Pin,
  PinOff,
  Search,
  Square,
  Trash2,
  X,
} from 'lucide-react'
import {
  assistantLinksQuery,
  hermes,
  iso,
  sessionSearchQuery,
  sessionsQuery,
  sessionTitle,
  sourceLabel,
  type Assistant,
  type AssistantLink,
  type HermesSession,
} from '@/lib/assistant'
import { rpc, useAssistantLive } from '@/lib/assistant-live'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { useDebounced, useMediaQuery } from '@/lib/hooks'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Input, Textarea } from '@/components/ui/input'
import { MenuList, Picker, Popover, usePopover, type MenuItem } from '@/components/ui/popover'
import { Badge, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { useConversation, toRows } from './chat-state'
import { LiveTurnView, StoredRows, TodoStrip } from './Transcript'
import { QuestionCard } from './Questions'
import type { AssistantSearch } from './search'

export function ChatTab({ assistant }: { assistant: Assistant }) {
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const wide = useMediaQuery('(min-width: 900px)')
  const [listOpen, setListOpen] = useState(false)
  const [convKey, setConvKey] = useState(() => search.s ?? 'new')
  useEffect(() => {
    if (search.send === '1' && search.ask && !search.s) setConvKey(`ask-${Date.now()}`)
  }, [search.send, search.ask, search.s])
  const open = (s?: string) => {
    setListOpen(false)
    setConvKey(s ?? `new-${Date.now()}`)
    navigate({
      search: (p: AssistantSearch) => ({
        ...p,
        s,
        ask: undefined,
        issue: undefined,
        send: undefined,
      }),
    })
  }
  const list = (
    <SessionList
      assistant={assistant}
      current={search.s}
      onOpen={open}
      onNew={() => open(undefined)}
    />
  )
  return (
    <div className="flex min-h-0 flex-1">
      {wide ? (
        <aside className="flex w-72 shrink-0 flex-col border-r border-line">{list}</aside>
      ) : (
        listOpen && (
          <div className="fixed inset-0 z-40 flex" role="dialog" aria-label="Conversations">
            <div className="flex w-[85vw] max-w-sm flex-col border-r border-line bg-panel shadow-pop">
              {list}
            </div>
            <button
              className="flex-1 bg-black/30"
              aria-label="Close"
              onClick={() => setListOpen(false)}
            />
          </div>
        )
      )}
      <Conversation
        key={convKey}
        assistant={assistant}
        stored={search.s ?? null}
        ask={search.ask}
        issue={search.issue}
        sendNow={search.send === '1'}
        onCreated={(s) =>
          navigate({
            search: (p: AssistantSearch) => ({
              ...p,
              s,
              ask: undefined,
              issue: undefined,
              send: undefined,
            }),
            replace: true,
          })
        }
        onList={wide ? undefined : () => setListOpen(true)}
        onNew={() => open(undefined)}
      />
    </div>
  )
}

type Filter = 'all' | 'chats' | 'scheduled' | 'archived'

function SessionList({
  assistant,
  current,
  onOpen,
  onNew,
}: {
  assistant: Assistant
  current?: string
  onOpen: (s: string) => void
  onNew: () => void
}) {
  const [text, setText] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const term = useDebounced(text.trim(), 250)
  const sessions = useQuery({
    ...sessionsQuery(assistant.id, {
      archived: filter === 'archived' ? 'only' : 'exclude',
      source: filter === 'scheduled' ? 'cron' : undefined,
    }),
    enabled: assistant.state.reachable && !assistant.state.frozen,
  })
  const hits = useQuery({ ...sessionSearchQuery(assistant.id, term), enabled: term.length > 1 })
  const rows = useMemo(() => {
    const all = sessions.data?.sessions ?? []
    return filter === 'chats' ? all.filter((s) => s.source !== 'cron') : all
  }, [sessions.data, filter])
  const groups = useMemo(() => group(rows), [rows])
  return (
    <>
      <div className="space-y-2 border-b border-line p-2">
        <div className="flex items-center gap-1.5">
          <div className="relative flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
            <Input
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="Search conversations"
              className="h-8 pl-8 text-[13px]"
              aria-label="Search conversations"
            />
          </div>
          <IconButton
            label="New conversation"
            icon={<MessageSquarePlus className="size-4" />}
            onClick={onNew}
          />
        </div>
        {!term && (
          <div role="radiogroup" aria-label="Show" className="flex gap-1 text-xs">
            {(['all', 'chats', 'scheduled', 'archived'] as Filter[]).map((f) => (
              <button
                key={f}
                role="radio"
                aria-checked={filter === f}
                onClick={() => setFilter(f)}
                className={clsx(
                  'rounded-md px-2 py-1 capitalize',
                  filter === f ? 'bg-accent-soft font-medium text-fg' : 'text-muted hover:bg-hover',
                )}
              >
                {f}
              </button>
            ))}
          </div>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-1.5">
        {term ? (
          hits.isPending ? (
            <ListSkeleton />
          ) : !hits.data?.results.length ? (
            <div className="p-4 text-center text-[13px] text-subtle">Nothing matches “{term}”</div>
          ) : (
            hits.data.results.map((h) => (
              <button
                key={h.session_id}
                onClick={() => onOpen(h.session_id)}
                className={clsx(
                  'block w-full rounded-lg px-2.5 py-2 text-left hover:bg-hover',
                  current === h.session_id && 'bg-hover',
                )}
              >
                <div className="truncate text-[13px] font-medium">
                  {h.title || h.preview || 'Untitled conversation'}
                </div>
                <div className="line-clamp-2 text-xs text-muted">
                  <Snippet text={h.snippet} />
                </div>
              </button>
            ))
          )
        ) : sessions.isPending ? (
          <ListSkeleton />
        ) : sessions.isError ? (
          <div className="p-4 text-center text-[13px] text-subtle">
            {assistant.state.frozen ? 'Frozen' : String(sessions.error.message)}
          </div>
        ) : !rows.length ? (
          <div className="p-4 text-center text-[13px] text-subtle">No conversations</div>
        ) : (
          groups.map(([label, ss]) => (
            <div key={label} className="mb-2">
              <div className="px-2.5 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                {label}
              </div>
              {ss.map((s) => (
                <SessionRow
                  key={s.id}
                  assistant={assistant}
                  s={s}
                  active={current === s.id}
                  onOpen={() => onOpen(s.id)}
                />
              ))}
            </div>
          ))
        )}
      </div>
    </>
  )
}

function ListSkeleton() {
  return (
    <div className="space-y-2 p-2">
      {[0, 1, 2, 3].map((i) => (
        <Skeleton key={i} className="h-10" />
      ))}
    </div>
  )
}

/** Search snippets mark matches with >>> <<<. */
function Snippet({ text }: { text: string }) {
  const parts = text.split(/(>>>.*?<<<)/g)
  return (
    <>
      {parts.map((p, i) =>
        p.startsWith('>>>') ? (
          <mark key={i} className="rounded bg-warn/25 px-0.5 text-fg">
            {p.slice(3, -3)}
          </mark>
        ) : (
          <span key={i}>{p}</span>
        ),
      )}
    </>
  )
}

function group(rows: HermesSession[]): [string, HermesSession[]][] {
  const day = 86400_000
  const start = new Date()
  start.setHours(0, 0, 0, 0)
  const t0 = start.getTime()
  const out = new Map<string, HermesSession[]>()
  const add = (k: string, s: HermesSession) => out.set(k, [...(out.get(k) ?? []), s])
  for (const s of rows) {
    const at = (s.last_active || s.started_at) * 1000
    if (s.pinned) add('Pinned', s)
    else if (at >= t0) add('Today', s)
    else if (at >= t0 - day) add('Yesterday', s)
    else if (at >= t0 - 6 * day) add('This week', s)
    else add('Earlier', s)
  }
  return [...out.entries()]
}

function SessionRow({
  assistant,
  s,
  active,
  onOpen,
}: {
  assistant: Assistant
  s: HermesSession
  active: boolean
  onOpen: () => void
}) {
  const pop = usePopover()
  const actions = useSessionActions(assistant, s)
  return (
    <div
      className={clsx(
        'group relative flex items-center rounded-lg hover:bg-hover',
        active && 'bg-accent-soft hover:bg-accent-soft',
      )}
    >
      <button onClick={onOpen} className="min-w-0 flex-1 px-2.5 py-2 text-left">
        <div className="flex items-center gap-1.5">
          {s.is_active && (
            <span className="size-1.5 shrink-0 rounded-full bg-ok" aria-label="Active lately" />
          )}
          {s.unread && !s.is_active && !active && (
            <span className="size-1.5 shrink-0 rounded-full bg-accent" aria-label="Unread" />
          )}
          <span
            className={clsx('truncate text-[13px]', s.unread ? 'font-semibold' : 'font-medium')}
          >
            {sessionTitle(s)}
          </span>
        </div>
        <div className="mt-0.5 flex items-center gap-1.5 text-xs text-subtle">
          {s.source !== 'waypoint' && <span className="shrink-0">{sourceLabel(s.source)}</span>}
          <span className="truncate">{relativeTime(iso(s.last_active || s.started_at))}</span>
        </div>
      </button>
      <IconButton
        ref={pop.anchor as React.Ref<HTMLButtonElement>}
        label="Conversation actions"
        size="xs"
        icon={<MoreHorizontal className="size-3.5" />}
        onClick={pop.toggle}
        className="mr-1 opacity-0 focus:opacity-100 group-hover:opacity-100"
      />
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <MenuList items={actions.items} onClose={pop.close} />
      </Popover>
      {actions.dialogs}
    </div>
  )
}

/** Rename, pin, archive, export, link, delete: for the row menu and the header. */
function useSessionActions(
  assistant: Assistant,
  s: Pick<HermesSession, 'id' | 'title' | 'pinned' | 'archived' | 'preview'>,
  onDeleted?: () => void,
) {
  const qc = useQueryClient()
  const id = assistant.id
  const [renaming, setRenaming] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const [linking, setLinking] = useState(false)
  const refresh = () => qc.invalidateQueries({ queryKey: ['assistant', id, 'sessions'] })
  const patch = useMutation({
    mutationFn: (b: Record<string, unknown>) => hermes(id, 'PATCH', `sessions/${enc(s.id)}`, b),
    onSuccess: refresh,
    onError: toastError,
  })
  const del = useMutation({
    mutationFn: () => hermes(id, 'DELETE', `sessions/${enc(s.id)}`),
    onSuccess: () => {
      refresh()
      onDeleted?.()
    },
    onError: toastError,
  })
  const exportIt = async () => {
    try {
      const data = await hermes(id, 'GET', `sessions/${enc(s.id)}/export`)
      const url = URL.createObjectURL(
        new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }),
      )
      const a = document.createElement('a')
      a.href = url
      a.download = `${(s.title || s.id).replace(/[^\w.-]+/g, '-').slice(0, 60)}.json`
      a.click()
      URL.revokeObjectURL(url)
    } catch (e) {
      toastError(e)
    }
  }
  const items: MenuItem[] = [
    { label: 'Rename', icon: <Pencil />, onSelect: () => setRenaming(true) },
    {
      label: s.pinned ? 'Unpin' : 'Pin',
      icon: s.pinned ? <PinOff /> : <Pin />,
      onSelect: () => patch.mutate({ pinned: !s.pinned }),
    },
    { label: 'Link to an issue…', icon: <Link2 />, onSelect: () => setLinking(true) },
    { label: 'Export', icon: <Download />, onSelect: exportIt },
    {
      label: s.archived ? 'Unarchive' : 'Archive',
      icon: <Archive />,
      onSelect: () => patch.mutate({ archived: !s.archived }),
    },
    { separator: true, label: '' },
    { label: 'Delete…', icon: <Trash2 />, danger: true, onSelect: () => setDeleting(true) },
  ]
  const dialogs = (
    <>
      {renaming && (
        <RenameDialog
          title={s.title ?? ''}
          onClose={() => setRenaming(false)}
          onSave={(title) => patch.mutate({ title }, { onSuccess: () => setRenaming(false) })}
        />
      )}
      {linking && (
        <LinkDialog
          assistant={assistant}
          session={s.id}
          title={s.title ?? s.preview}
          onClose={() => setLinking(false)}
        />
      )}
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        danger
        title="Delete this conversation?"
        body={`“${sessionTitle({ ...s, title: s.title ?? null })}” and its transcript go for good.`}
        confirmLabel="Delete"
        onConfirm={() => del.mutate()}
      />
    </>
  )
  return { items, dialogs }
}

function RenameDialog({
  title,
  onClose,
  onSave,
}: {
  title: string
  onClose: () => void
  onSave: (t: string) => void
}) {
  const [v, setV] = useState(title)
  return (
    <Dialog
      open
      onClose={onClose}
      title="Rename"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => onSave(v.trim())}>
            Save
          </Button>
        </>
      }
    >
      <Input
        autoFocus
        value={v}
        maxLength={100}
        onChange={(e) => setV(e.target.value)}
        onKeyDown={(e) => e.key === 'Enter' && onSave(v.trim())}
      />
    </Dialog>
  )
}

function LinkDialog({
  assistant,
  session,
  title,
  onClose,
}: {
  assistant: Assistant
  session: string
  title?: string | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const issues = useQuery(q.search('', { limit: 300, order: 'updated' })).data?.issues ?? []
  const link = (key: string) =>
    api
      .post(`/assistants/${assistant.id}/links`, {
        session_id: session,
        issue: key,
        title: title ?? '',
      })
      .then(() => {
        qc.invalidateQueries({ queryKey: ['assistant', assistant.id, 'links'] })
        qc.invalidateQueries({ queryKey: ['issueAssistantLinks', key] })
        toast(`Linked to ${key}`, { kind: 'success' })
        onClose()
      }, toastError)
  return (
    <Dialog open onClose={onClose} title="Link to an issue">
      <Picker
        items={issues.map((i) => ({
          value: i.key,
          label: `${i.key} ${i.title}`,
          keywords: i.project_key,
        }))}
        selected={[]}
        onPick={(k) => link(String(k))}
        onCreate={(t) => link(t.toUpperCase())}
        createLabel={(t) => `Link ${t.toUpperCase()}`}
        placeholder="Issue key or title…"
      />
    </Dialog>
  )
}

const SUGGESTIONS = [
  'What needs my attention today?',
  'What did you do since yesterday?',
  'What is scheduled for this week?',
  'Check my email for anything urgent',
]

function Conversation({
  assistant,
  stored,
  ask,
  issue,
  sendNow,
  onCreated,
  onList,
  onNew,
}: {
  assistant: Assistant
  stored: string | null
  ask?: string
  issue?: string
  sendNow?: boolean
  onCreated: (s: string) => void
  onList?: () => void
  onNew: () => void
}) {
  const live = useAssistantLive(assistant.id)
  const qc = useQueryClient()
  const sessions = qc.getQueryData<{ sessions: HermesSession[] }>(
    sessionsQuery(assistant.id).queryKey,
  )
  const row = useMemo(() => sessions?.sessions.find((s) => s.id === stored), [sessions, stored])
  const c = useConversation(assistant.id, stored, row)
  const rows = useMemo(() => toRows(c.messages), [c.messages])
  const links =
    useQuery({ ...assistantLinksQuery(assistant.id, stored ?? ''), enabled: !!stored }).data ?? []
  const scroller = useRef<HTMLDivElement>(null)
  const [pinned, setPinned] = useState(true)
  const [editing, setEditing] = useState<{ text: string; id: number } | null>(null)
  const blocked = assistant.state.frozen || !assistant.state.reachable
  const open = live.open.filter((r) => !c.runtime || r.params.session_id === c.runtime)
  const elsewhere = live.open.length - open.length

  useEffect(() => {
    const el = scroller.current
    if (el && pinned) el.scrollTop = el.scrollHeight
  })
  const onScroll = () => {
    const el = scroller.current
    if (el) setPinned(el.scrollHeight - el.scrollTop - el.clientHeight < 80)
  }

  const title = c.info.title
    ? sessionTitle({ id: '', title: c.info.title, preview: '' })
    : row
      ? sessionTitle(row)
      : stored
        ? ''
        : 'New conversation'
  const send = useCallback(
    async (text: string, files: File[]) => {
      setPinned(true)
      const s = await c.send(text, files, {
        create: (s) => onCreated(s),
        title: issue
          ? text.includes(issue)
            ? text.slice(0, 80)
            : `${issue}: ${text.slice(0, 60)}`
          : undefined,
      })
      if (issue && s && !stored)
        api
          .post(`/assistants/${assistant.id}/links`, {
            session_id: s,
            issue,
            title: text.slice(0, 120),
          })
          .then(() => qc.invalidateQueries({ queryKey: ['assistant', assistant.id, 'links'] }))
          .catch(() => {})
    },
    [c, onCreated, issue, stored, assistant.id, qc],
  )
  const sentNow = useRef(false)
  useEffect(() => {
    if (!sendNow || !ask || stored || sentNow.current || blocked || live.state !== 'open') return
    sentNow.current = true
    send(ask, []).catch(toastError)
  }, [sendNow, ask, stored, blocked, live.state, send])
  return (
    <div className="flex min-w-0 flex-1 flex-col">
      <div className="flex h-11 shrink-0 items-center gap-2 border-b border-line px-3">
        {onList && (
          <IconButton
            label="Conversations"
            icon={<PanelLeft className="size-4" />}
            onClick={onList}
          />
        )}
        <div className="min-w-0 flex-1">
          <div className="truncate text-[13px] font-medium">{title}</div>
        </div>
        {links.map((l) => (
          <LinkChip key={l.id} l={l} />
        ))}
        {!!c.info.usage?.context_percent && (
          <ContextMeter pct={c.info.usage.context_percent} usage={c.info.usage} />
        )}
        {c.info.model && (
          <Badge className="hidden max-w-40 truncate sm:inline-flex" color="var(--wp-subtle)">
            {c.info.model}
          </Badge>
        )}
        {stored && row && (
          <HeaderMenu
            assistant={assistant}
            s={row}
            runtime={c.runtime}
            attach={c.attach}
            onNew={onNew}
          />
        )}
        {!onList && stored && (
          <IconButton
            label="New conversation"
            icon={<MessageSquarePlus className="size-4" />}
            onClick={onNew}
          />
        )}
      </div>
      <div ref={scroller} onScroll={onScroll} className="relative min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-3xl space-y-5 px-4 py-5">
          {!stored && !c.turn ? (
            <Welcome assistant={assistant} onPick={(t) => send(t, []).catch(toastError)} />
          ) : c.loading ? (
            <div className="space-y-4">
              <Skeleton className="ml-auto h-10 w-2/3" />
              <Skeleton className="h-24" />
              <Skeleton className="ml-auto h-10 w-1/2" />
            </div>
          ) : c.error ? (
            <div className="text-center text-[13px] text-subtle">{String(c.error.message)}</div>
          ) : (
            <StoredRows
              rows={rows}
              onEdit={c.running || blocked ? undefined : (text, id) => setEditing({ text, id })}
              onRetry={
                c.running || blocked
                  ? undefined
                  : (text, id) => {
                      setPinned(true)
                      c.resend(text, id).catch(toastError)
                    }
              }
            />
          )}
          {c.turn && <LiveTurnView turn={c.turn} />}
          {c.running && !c.turn && (
            <div className="flex items-center gap-2 text-[13px] text-subtle">
              <Spinner className="size-3.5 text-accent" /> Running in{' '}
              {row ? sourceLabel(row.source) : 'another place'}…
            </div>
          )}
          {open.map((r) => (
            <QuestionCard key={r.id} assistantId={assistant.id} r={r} />
          ))}
        </div>
        {!pinned && (
          <button
            onClick={() => {
              setPinned(true)
              scroller.current?.scrollTo({ top: scroller.current.scrollHeight, behavior: 'smooth' })
            }}
            className="sticky bottom-3 left-1/2 mx-auto flex -translate-x-0 items-center gap-1 rounded-full border border-line bg-raised px-3 py-1 text-xs shadow-pop"
          >
            <ArrowDown className="size-3" /> Latest
          </button>
        )}
      </div>
      <div className="mx-auto w-full max-w-3xl space-y-2 px-4 pb-4 pt-1">
        {elsewhere > 0 && (
          <div className="rounded-lg bg-accent-soft px-3 py-1.5 text-xs">
            {assistant.name} asked you something in{' '}
            {elsewhere === 1 ? 'another conversation' : `${elsewhere} other conversations`}.
          </div>
        )}
        {live.declined.slice(-1).map((d) => (
          <div key={d.at} className="rounded-lg bg-warn/10 px-3 py-1.5 text-xs text-warn">
            {assistant.name} asked for{' '}
            {d.params?.env_var ? <code>{d.params.env_var}</code> : 'a password'}. Waypoint doesn't
            pass secrets; add the service as a connection under Tools.
          </div>
        ))}
        <TodoStrip todos={c.todos} />
        <Composer
          assistant={assistant}
          disabled={blocked}
          running={c.running && !!c.runtime}
          initial={sendNow ? undefined : ask}
          onSend={
            editing
              ? async (text) => {
                  setPinned(true)
                  await c.resend(text, editing.id)
                  setEditing(null)
                }
              : send
          }
          editing={editing?.text}
          onCancelEdit={() => setEditing(null)}
          onStop={() => c.stop().catch(toastError)}
          onSlash={stored ? (cmd) => c.slash(cmd).catch(toastError) : undefined}
          runtime={c.runtime}
        />
      </div>
    </div>
  )
}

function Welcome({ assistant, onPick }: { assistant: Assistant; onPick: (t: string) => void }) {
  return (
    <div className="flex flex-col items-center gap-4 py-[12vh] text-center">
      <div className="text-xl font-semibold tracking-tight">What should {assistant.name} do?</div>
      <div className="flex max-w-xl flex-wrap justify-center gap-2">
        {SUGGESTIONS.map((s) => (
          <button
            key={s}
            onClick={() => onPick(s)}
            disabled={assistant.state.frozen || !assistant.state.reachable}
            className="rounded-full border border-line bg-panel px-3 py-1.5 text-[13px] text-muted hover:bg-hover hover:text-fg disabled:opacity-50"
          >
            {s}
          </button>
        ))}
      </div>
    </div>
  )
}

function LinkChip({ l }: { l: AssistantLink }) {
  if (!l.issue) return null
  return (
    <a
      href={`/issue/${l.issue.key}`}
      title={l.issue.title}
      className="hidden shrink-0 items-center gap-1 rounded-md bg-sunken px-1.5 py-0.5 text-xs font-medium text-muted hover:text-fg sm:inline-flex"
    >
      <Link2 className="size-3" />
      {l.issue.key}
    </a>
  )
}

function ContextMeter({
  pct,
  usage,
}: {
  pct: number
  usage: { context_used?: number | null; context_max?: number | null; cost_usd?: number | null }
}) {
  const r = 7
  const len = 2 * Math.PI * r
  const color = pct > 85 ? 'var(--wp-danger)' : pct > 65 ? 'var(--wp-warn)' : 'var(--wp-accent)'
  const tip = `Context ${pct}%${usage.context_used ? ` · ${usage.context_used.toLocaleString()} of ${usage.context_max?.toLocaleString()} tokens` : ''}${usage.cost_usd ? ` · $${usage.cost_usd.toFixed(3)}` : ''}`
  return (
    <span title={tip} aria-label={tip} className="hidden shrink-0 sm:inline-flex">
      <svg viewBox="0 0 18 18" className="size-[18px] -rotate-90">
        <circle cx="9" cy="9" r={r} fill="none" stroke="var(--wp-line)" strokeWidth="2.5" />
        <circle
          cx="9"
          cy="9"
          r={r}
          fill="none"
          stroke={color}
          strokeWidth="2.5"
          strokeDasharray={`${(len * pct) / 100} ${len}`}
          strokeLinecap="round"
        />
      </svg>
    </span>
  )
}

function HeaderMenu({
  assistant,
  s,
  runtime,
  attach,
  onNew,
}: {
  assistant: Assistant
  s: HermesSession
  runtime: string | null
  attach: (s: string) => Promise<string>
  onNew: () => void
}) {
  const pop = usePopover()
  const navigate = useNavigate({ from: '/assistant/$id' })
  const actions = useSessionActions(assistant, s, onNew)
  const fork = async () => {
    try {
      const rt = runtime ?? (await attach(s.id))
      const r = await rpc<{ stored_session_id: string }>(assistant.id, 'session.branch', {
        session_id: rt,
      })
      navigate({ search: (p: AssistantSearch) => ({ ...p, s: r.stored_session_id }) })
      toast('Forked into a new conversation', { kind: 'success' })
    } catch (e) {
      toastError(e)
    }
  }
  const items: MenuItem[] = [
    { label: 'Fork from here', icon: <GitBranch />, onSelect: fork },
    ...actions.items,
  ]
  return (
    <>
      <IconButton
        ref={pop.anchor as React.Ref<HTMLButtonElement>}
        label="Conversation actions"
        icon={<MoreHorizontal className="size-4" />}
        onClick={pop.toggle}
      />
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <MenuList items={items} onClose={pop.close} />
      </Popover>
      {actions.dialogs}
    </>
  )
}

interface Completion {
  text: string
  display?: string
  meta?: string
}

function Composer({
  assistant,
  disabled,
  running,
  initial,
  onSend,
  onStop,
  onSlash,
  runtime,
  editing,
  onCancelEdit,
}: {
  assistant: Assistant
  disabled: boolean
  running: boolean
  initial?: string
  /** the question being edited, to send again in its place */
  editing?: string
  onCancelEdit: () => void
  onSend: (text: string, files: File[]) => Promise<void>
  onStop: () => void
  onSlash?: (command: string) => void
  runtime: string | null
}) {
  const key = `wp.assistant.${assistant.id}.draft`
  const [text, setText] = useState(() => initial ?? readDraft(key))
  const [files, setFiles] = useState<File[]>([])
  const [sending, setSending] = useState(false)
  const [drag, setDrag] = useState(false)
  const [completions, setCompletions] = useState<Completion[]>([])
  const [sel, setSel] = useState(0)
  const ta = useRef<HTMLTextAreaElement>(null)
  const fileInput = useRef<HTMLInputElement>(null)
  const touch = useMediaQuery('(pointer: coarse)')
  const before = useRef('')
  const dictation = useDictation((done, interim) => {
    const sep = before.current && !/\s$/.test(before.current) ? ' ' : ''
    setText(before.current + sep + done + interim)
  })
  useEffect(() => {
    if (editing === undefined) return
    setText(editing)
    ta.current?.focus()
  }, [editing])

  useEffect(() => {
    try {
      if (text && !initial) localStorage.setItem(key, text)
      else localStorage.removeItem(key)
    } catch {}
  }, [key, text, initial])
  useEffect(() => {
    if (!touch) ta.current?.focus()
  }, [touch])

  const slashText = text.startsWith('/') && !text.includes('\n') ? text : ''
  const debounced = useDebounced(slashText, 120)
  useEffect(() => {
    if (!debounced) return setCompletions([])
    let gone = false
    rpc<{ items?: Completion[] }>(assistant.id, 'complete.slash', {
      text: debounced,
      ...(runtime ? { session_id: runtime } : {}),
    })
      .then((r) => !gone && (setCompletions((r.items ?? []).slice(0, 8)), setSel(0)))
      .catch(() => !gone && setCompletions([]))
    return () => {
      gone = true
    }
  }, [debounced, assistant.id, runtime])

  const fresh = completions.length > 0 && !!slashText && debounced === slashText
  const submit = async () => {
    const t = text.trim()
    if ((!t && !files.length) || sending || disabled) return
    if (t.startsWith('/') && onSlash && !files.length) {
      setText('')
      onSlash(t)
      return
    }
    setSending(true)
    try {
      await onSend(t, files)
      setText('')
      setFiles([])
      try {
        localStorage.removeItem(key)
      } catch {}
    } catch (e) {
      toastError(e)
    } finally {
      setSending(false)
      ta.current?.focus()
    }
  }
  const addFiles = (fs: FileList | File[]) => {
    const list = [...fs].filter((f) => f.size <= 25 * 1024 * 1024)
    if (list.length < [...fs].length) toast('Files over 25 MB were left out', { kind: 'error' })
    setFiles((cur) => [...cur, ...list].slice(0, 10))
  }
  const accept = (c: Completion) => {
    setText(c.text.endsWith(' ') ? c.text : c.text + ' ')
    setCompletions([])
    ta.current?.focus()
  }
  return (
    <div
      className={clsx(
        'relative rounded-2xl border bg-panel shadow-card transition-colors',
        drag ? 'border-accent' : 'border-line focus-within:border-line-strong',
      )}
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes('Files')) {
          e.preventDefault()
          setDrag(true)
        }
      }}
      onDragLeave={() => setDrag(false)}
      onDrop={(e) => {
        e.preventDefault()
        setDrag(false)
        addFiles(e.dataTransfer.files)
      }}
    >
      {completions.length > 0 && (
        <div
          role="listbox"
          className="absolute bottom-full left-0 right-0 mb-2 overflow-hidden rounded-xl border border-line bg-raised shadow-pop"
        >
          {completions.map((c, i) => (
            <button
              key={c.text}
              role="option"
              aria-selected={i === sel}
              onMouseDown={(e) => {
                e.preventDefault()
                accept(c)
              }}
              className={clsx(
                'flex w-full items-baseline gap-2 px-3 py-1.5 text-left text-[13px]',
                i === sel && 'bg-hover',
              )}
            >
              <span className="font-mono">{c.display || c.text}</span>
              {c.meta && <span className="truncate text-xs text-subtle">{c.meta}</span>}
            </button>
          ))}
        </div>
      )}
      {editing !== undefined && (
        <div className="flex items-center gap-2 px-3.5 pt-2.5 text-xs text-muted">
          <Pencil className="size-3" />
          Editing your last message: sending replaces it and what came after
          <button
            className="ml-auto rounded px-1 hover:bg-hover hover:text-fg"
            onClick={() => {
              setText('')
              onCancelEdit()
            }}
          >
            Cancel
          </button>
        </div>
      )}
      {files.length > 0 && (
        <div className="flex flex-wrap gap-1.5 px-3 pt-2.5">
          {files.map((f, i) => (
            <span
              key={i}
              className="inline-flex max-w-56 items-center gap-1 rounded-md bg-sunken py-0.5 pl-2 pr-1 text-xs"
            >
              <Paperclip className="size-3 shrink-0 text-subtle" />
              <span className="truncate">{f.name}</span>
              <button
                aria-label={`Remove ${f.name}`}
                onClick={() => setFiles((cur) => cur.filter((_, j) => j !== i))}
                className="rounded p-0.5 hover:bg-hover"
              >
                <X className="size-3" />
              </button>
            </span>
          ))}
        </div>
      )}
      <Textarea
        ref={ta}
        autoGrow
        rows={1}
        value={text}
        disabled={disabled}
        onChange={(e) => setText(e.target.value)}
        onPaste={(e) => {
          const fs = [...e.clipboardData.files]
          if (fs.length) {
            e.preventDefault()
            addFiles(fs)
          }
        }}
        onKeyDown={(e) => {
          if (fresh) {
            if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
              e.preventDefault()
              setSel(
                (s) =>
                  (s + (e.key === 'ArrowDown' ? 1 : -1) + completions.length) % completions.length,
              )
              return
            }
            if (
              e.key === 'Tab' ||
              (e.key === 'Enter' && !e.shiftKey && completions[sel].text.trim() !== text.trim())
            ) {
              e.preventDefault()
              accept(completions[sel])
              return
            }
            if (e.key === 'Escape') {
              setCompletions([])
              return
            }
          }
          if (e.key === 'Escape' && running && !text) {
            e.preventDefault()
            onStop()
            return
          }
          if (e.key === 'Escape' && editing !== undefined) {
            setText('')
            onCancelEdit()
            return
          }
          if (e.key === 'Enter' && !e.shiftKey && !touch && !e.nativeEvent.isComposing) {
            e.preventDefault()
            void submit()
          }
        }}
        placeholder={
          disabled
            ? assistant.state.frozen
              ? `${assistant.name} is frozen`
              : `${assistant.name} isn't answering`
            : running
              ? 'Add to what it is doing…'
              : `Message ${assistant.name}`
        }
        aria-label={`Message ${assistant.name}`}
        className="!max-h-[40vh] !resize-none !border-0 !bg-transparent !px-3.5 !pt-3 !ring-0"
      />
      <div className="flex items-center gap-1 px-2 pb-2">
        <input
          ref={fileInput}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            if (e.target.files) addFiles(e.target.files)
            e.target.value = ''
          }}
        />
        <IconButton
          label="Attach files"
          icon={<Paperclip className="size-4" />}
          disabled={disabled}
          onClick={() => fileInput.current?.click()}
        />
        {dictation.supported && (
          <IconButton
            label={dictation.on ? 'Stop dictating' : 'Dictate'}
            icon={dictation.on ? <MicOff className="size-4" /> : <Mic className="size-4" />}
            disabled={disabled}
            onClick={() => {
              before.current = text
              dictation.toggle()
            }}
            className={clsx(dictation.on && 'animate-pulse !text-danger')}
          />
        )}
        <span className="flex-1" />
        {running && (
          <Button
            size="sm"
            variant="secondary"
            icon={<Square className="size-3 fill-current" />}
            onClick={onStop}
          >
            Stop
          </Button>
        )}
        <IconButton
          label={running ? 'Send now' : 'Send'}
          variant="primary"
          disabled={disabled || sending || (!text.trim() && !files.length)}
          loading={sending}
          icon={<ArrowUp className="size-4" />}
          onClick={() => void submit()}
          className="!rounded-full"
        />
      </div>
    </div>
  )
}

// Dictation, where the browser offers speech recognition (Chrome, Safari).
type Recognition = {
  continuous: boolean
  interimResults: boolean
  lang: string
  onresult: (e: {
    resultIndex: number
    results: ArrayLike<{ isFinal: boolean; 0: { transcript: string } }>
  }) => void
  onend: () => void
  onerror: (e: { error: string }) => void
  start: () => void
  stop: () => void
}

function useDictation(onText: (finalText: string, interim: string) => void) {
  const [on, setOn] = useState(false)
  const rec = useRef<Recognition | null>(null)
  const w = window as unknown as Record<string, (new () => Recognition) | undefined>
  const Ctor = w.SpeechRecognition ?? w.webkitSpeechRecognition
  const toggle = () => {
    if (on) return rec.current?.stop()
    if (!Ctor) return
    const r = new Ctor()
    r.continuous = true
    r.interimResults = true
    r.lang = navigator.language
    let done = ''
    r.onresult = (e) => {
      let interim = ''
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const res = e.results[i]
        if (res.isFinal) done += res[0].transcript
        else interim += res[0].transcript
      }
      onText(done, interim)
    }
    r.onend = () => setOn(false)
    r.onerror = (e) => {
      setOn(false)
      if (e.error === 'not-allowed')
        toast('Microphone access is blocked for this site', { kind: 'error' })
    }
    rec.current = r
    r.start()
    setOn(true)
  }
  useEffect(() => () => rec.current?.stop(), [])
  return { supported: !!Ctor, on, toggle }
}

function readDraft(key: string): string {
  try {
    return localStorage.getItem(key) ?? ''
  } catch {
    return ''
  }
}
