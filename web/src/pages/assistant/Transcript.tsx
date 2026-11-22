// How a conversation reads (WP-ADR1): your messages, the assistant's
// answers as markdown (its text is untrusted: no remote images, no HTML),
// its reasoning folded away, and each tool call as a quiet card that opens
// to show what went in and what came back.

import { memo, useState, type ReactNode } from 'react'
import clsx from 'clsx'
import {
  Brain,
  Check,
  ChevronRight,
  Copy,
  FileText,
  Globe,
  HelpCircle,
  Image as ImageIcon,
  ListChecks,
  Pencil,
  Plug,
  RotateCcw,
  Search,
  TerminalSquare,
  Users,
  Wrench,
  X,
} from 'lucide-react'
import { copyText } from '@/lib/clipboard'
import { dateTime } from '@/lib/format'
import { iso } from '@/lib/assistant'
import { Markdown } from '@/components/Markdown'
import { Spinner } from '@/components/ui/button'
import { toast } from '@/components/ui/toast'
import type { LiveItem, LiveTurn, Row, Todo, ToolItem } from './chat-state'

function toolIcon(name: string): ReactNode {
  const n = name.toLowerCase()
  if (n.includes('terminal') || n.includes('shell') || n.includes('execute') || n === 'process')
    return <TerminalSquare />
  if (n.includes('search')) return <Search />
  if (n.includes('web') || n.includes('browse') || n.includes('fetch') || n.includes('extract'))
    return <Globe />
  if (n.includes('file') || n.includes('patch') || n.includes('read') || n.includes('write'))
    return <FileText />
  if (n.includes('memory')) return <Brain />
  if (n.includes('clarify')) return <HelpCircle />
  if (n.includes('todo')) return <ListChecks />
  if (n.includes('delegate') || n.includes('subagent')) return <Users />
  if (n.includes('vision') || n.includes('image')) return <ImageIcon />
  if (n.startsWith('mcp_') || n.includes('__')) return <Plug />
  return <Wrench />
}

/** A tool's name as people read it: mcp_gate_send_email → gate · send email. */
export function toolLabel(name: string): string {
  const n = name.replace(/^mcp_/, '')
  const [server, ...rest] = n.includes('__') ? n.split('__') : [null, n]
  const words = (rest.join(' ') || n).replace(/_/g, ' ')
  return server ? `${server.replace(/_/g, ' ')} · ${words}` : words
}

function argsLine(t: ToolItem): string {
  if (t.context) return t.context
  const a = t.args
  if (!a) return ''
  for (const k of [
    'command',
    'query',
    'path',
    'url',
    'file_path',
    'name',
    'goal',
    'prompt',
    'text',
  ])
    if (typeof a[k] === 'string') return a[k] as string
  const first = Object.values(a).find((v) => typeof v === 'string')
  return typeof first === 'string' ? first : ''
}

export function ToolCard({ t }: { t: ToolItem }) {
  const [open, setOpen] = useState(false)
  const line = argsLine(t)
  return (
    <div className="rounded-lg border border-line bg-sunken/40 text-[13px]">
      <button
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex w-full min-w-0 items-center gap-2 px-2.5 py-1.5 text-left hover:bg-hover"
      >
        <span className="shrink-0 text-subtle [&>svg]:size-3.5">{toolIcon(t.name)}</span>
        <span className="shrink-0 font-medium text-muted">{toolLabel(t.name)}</span>
        {line && (
          <span className="min-w-0 flex-1 truncate font-mono text-xs text-subtle">{line}</span>
        )}
        {!line && <span className="flex-1" />}
        {t.duration !== undefined && (
          <span className="shrink-0 text-xs text-subtle">{fmtSeconds(t.duration)}</span>
        )}
        <span className="shrink-0">
          {!t.done ? (
            <Spinner className="size-3.5 text-accent" />
          ) : t.failed ? (
            <X className="size-3.5 text-danger" />
          ) : (
            <Check className="size-3.5 text-ok" />
          )}
        </span>
        <ChevronRight
          className={clsx(
            'size-3.5 shrink-0 text-subtle transition-transform',
            open && 'rotate-90',
          )}
        />
      </button>
      {open && (
        <div className="space-y-2 border-t border-line px-2.5 py-2">
          {t.args && Object.keys(t.args).length > 0 && (
            <Block label="Input" text={JSON.stringify(t.args, null, 2)} />
          )}
          {t.diff && <Block label="Changes" text={t.diff} />}
          {t.summary && <div className="text-xs text-muted">{t.summary}</div>}
          {t.result !== undefined && <Block label="Output" text={t.result || '(nothing)'} />}
          {!t.done && <div className="text-xs text-subtle">Running…</div>}
        </div>
      )}
    </div>
  )
}

function Block({ label, text }: { label: string; text: string }) {
  const [all, setAll] = useState(false)
  const long = text.length > 4000
  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-[11px] font-semibold uppercase tracking-wide text-subtle">
        {label}
        <button
          className="rounded p-0.5 normal-case hover:bg-hover hover:text-fg"
          aria-label={`Copy ${label.toLowerCase()}`}
          onClick={() => copyText(text).then(() => toast('Copied'))}
        >
          <Copy className="size-3" />
        </button>
      </div>
      <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md bg-sunken p-2 font-mono text-xs text-muted">
        {long && !all ? text.slice(0, 4000) + '…' : text}
      </pre>
      {long && (
        <button
          className="mt-1 text-xs text-accent hover:underline"
          onClick={() => setAll((v) => !v)}
        >
          {all ? 'Show less' : `Show all ${Math.round(text.length / 1000)}k characters`}
        </button>
      )}
    </div>
  )
}

const fmtSeconds = (s: number) =>
  s < 1 ? `${Math.round(s * 1000)} ms` : s < 60 ? `${s.toFixed(1)} s` : `${Math.round(s / 60)} min`

/** Tool calls in a row: more than three fold into “N steps”. */
export function Tools({ tools }: { tools: ToolItem[] }) {
  const [open, setOpen] = useState(false)
  const running = tools.some((t) => !t.done)
  if (tools.length <= 3 || open || running)
    return (
      <div className="space-y-1">
        {tools.map((t) => (
          <ToolCard key={t.id} t={t} />
        ))}
        {open && (
          <button className="text-xs text-subtle hover:text-fg" onClick={() => setOpen(false)}>
            Fold steps
          </button>
        )}
      </div>
    )
  const names = [...new Set(tools.map((t) => toolLabel(t.name)))].slice(0, 3).join(', ')
  return (
    <button
      onClick={() => setOpen(true)}
      className="flex w-full items-center gap-2 rounded-lg border border-line bg-sunken/40 px-2.5 py-1.5 text-left text-[13px] text-muted hover:bg-hover"
    >
      <Wrench className="size-3.5 text-subtle" />
      <span className="font-medium">{tools.length} steps</span>
      <span className="min-w-0 flex-1 truncate text-xs text-subtle">{names}</span>
      <ChevronRight className="size-3.5 text-subtle" />
    </button>
  )
}

export function Reasoning({ text, live }: { text: string; live?: boolean }) {
  const [open, setOpen] = useState(false)
  if (!text.trim()) return null
  return (
    <div className="text-[13px]">
      <button
        onClick={() => setOpen((o) => !o)}
        aria-expanded={open}
        className="flex items-center gap-1.5 text-subtle hover:text-muted"
      >
        <Brain className={clsx('size-3.5', live && 'animate-pulse text-accent')} />
        {live ? 'Thinking…' : 'Thought'}
        <ChevronRight className={clsx('size-3 transition-transform', open && 'rotate-90')} />
      </button>
      {(open || live) && (
        <div
          className={clsx(
            'mt-1 whitespace-pre-wrap border-l-2 border-line pl-3 text-xs leading-relaxed text-subtle',
            live && !open && 'line-clamp-3',
          )}
        >
          {text}
        </div>
      )}
    </div>
  )
}

function UserBubble({
  text,
  images,
  at,
  onEdit,
  onRetry,
}: {
  text: string
  images?: number
  at?: number
  onEdit?: () => void
  onRetry?: () => void
}) {
  return (
    <div className="group flex items-center justify-end gap-1">
      {onEdit && (
        <span className="flex opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
          <button
            className="rounded p-1 text-subtle hover:bg-hover hover:text-fg"
            aria-label="Edit and send again"
            title="Edit and send again"
            onClick={onEdit}
          >
            <Pencil className="size-3.5" />
          </button>
          <button
            className="rounded p-1 text-subtle hover:bg-hover hover:text-fg"
            aria-label="Ask again"
            title="Ask again"
            onClick={onRetry}
          >
            <RotateCcw className="size-3.5" />
          </button>
        </span>
      )}
      <div
        className="max-w-[85%] rounded-2xl rounded-br-md bg-accent-soft px-3.5 py-2 text-[14px] leading-relaxed"
        title={at ? dateTime(iso(at)!) : undefined}
      >
        <div className="whitespace-pre-wrap break-words">{text}</div>
        {!!images && (
          <div className="mt-1 flex items-center gap-1 text-xs text-muted">
            <ImageIcon className="size-3" /> {images === 1 ? 'an image' : `${images} images`}
          </div>
        )}
      </div>
    </div>
  )
}

function AssistantBlock({
  children,
  copy,
  at,
}: {
  children: ReactNode
  copy?: string
  at?: number
}) {
  return (
    <div className="group relative space-y-2">
      {children}
      {(copy || at) && (
        <div className="flex h-5 items-center gap-2 text-xs text-subtle opacity-0 transition-opacity group-focus-within:opacity-100 group-hover:opacity-100">
          {at && <span>{dateTime(iso(at)!)}</span>}
          {copy && (
            <button
              className="inline-flex items-center gap-1 rounded px-1 hover:bg-hover hover:text-fg"
              onClick={() => copyText(copy).then(() => toast('Copied'))}
            >
              <Copy className="size-3" /> Copy
            </button>
          )}
        </div>
      )}
    </div>
  )
}

/** The stored transcript. Consecutive assistant rows share one block. */
export const StoredRows = memo(function StoredRows({
  rows,
  onEdit,
  onRetry,
}: {
  rows: Row[]
  /** the last question can be edited or asked again */
  onEdit?: (text: string, id: number) => void
  onRetry?: (text: string, id: number) => void
}) {
  const lastUser = rows.findLast((r) => r.kind === 'user')
  const groups: Row[][] = []
  for (const r of rows) {
    const last = groups[groups.length - 1]
    if (last && r.kind === 'assistant' && last[0].kind === 'assistant') last.push(r)
    else groups.push([r])
  }
  return (
    <>
      {groups.map((g) => {
        const r = g[0]
        if (r.kind === 'user')
          return (
            <UserBubble
              key={r.id}
              text={r.text}
              images={r.images}
              at={r.at}
              onEdit={r === lastUser && onEdit ? () => onEdit(r.text, r.id) : undefined}
              onRetry={r === lastUser && onRetry ? () => onRetry(r.text, r.id) : undefined}
            />
          )
        if (r.kind === 'note')
          return (
            <div key={r.id} className="text-center text-xs text-subtle">
              {r.text}
            </div>
          )
        const texts = g.flatMap((x) => (x.kind === 'assistant' && x.text.trim() ? [x.text] : []))
        return (
          <AssistantBlock key={r.id} copy={texts.join('\n\n')} at={g[g.length - 1].at}>
            {g.map((x) =>
              x.kind === 'assistant' ? (
                <div key={x.id} className="space-y-2">
                  {x.reasoning && <Reasoning text={x.reasoning} />}
                  {x.text.trim() && <Markdown text={x.text} />}
                  {x.tools.length > 0 && <Tools tools={x.tools} />}
                </div>
              ) : null,
            )}
          </AssistantBlock>
        )
      })}
    </>
  )
})

/** The turn streaming now. */
export function LiveTurnView({ turn }: { turn: LiveTurn }) {
  const blocks: (LiveItem | ToolItem[])[] = []
  for (const it of turn.items) {
    const last = blocks[blocks.length - 1]
    if (it.kind === 'tool') {
      if (Array.isArray(last)) last.push(it)
      else blocks.push([it])
    } else blocks.push(it)
  }
  const lastText = [...turn.items].reverse().find((i) => i.kind === 'text')
  const showDots =
    turn.running && !turn.items.some((i) => i.kind === 'text' && i.text) && !turn.status
  return (
    <>
      {turn.user && <UserBubble text={turn.user} />}
      <AssistantBlock
        copy={turn.running ? undefined : lastText?.kind === 'text' ? lastText.text : undefined}
      >
        {blocks.map((b, i) =>
          Array.isArray(b) ? (
            <Tools key={i} tools={b} />
          ) : b.kind === 'reasoning' ? (
            <Reasoning key={i} text={b.text} live={turn.running && i === blocks.length - 1} />
          ) : b.kind === 'text' ? (
            <Markdown key={i} text={b.text} />
          ) : b.kind === 'note' ? (
            <div
              key={i}
              className={clsx(
                'whitespace-pre-wrap rounded-lg px-3 py-2 text-[13px]',
                b.tone === 'error' ? 'bg-danger/10 text-danger' : 'bg-sunken text-muted',
              )}
            >
              {b.text}
            </div>
          ) : null,
        )}
        {turn.running && (turn.status || showDots) && (
          <div className="flex items-center gap-2 text-[13px] text-subtle">
            <Spinner className="size-3.5 text-accent" />
            {turn.status || 'Working…'}
          </div>
        )}
      </AssistantBlock>
    </>
  )
}

export function TodoStrip({ todos }: { todos: Todo[] }) {
  const [open, setOpen] = useState(false)
  if (!todos.length) return null
  const active = todos.filter((t) => t.status === 'in_progress')
  const done = todos.filter((t) => t.status === 'completed').length
  return (
    <div className="rounded-lg border border-line bg-panel text-[13px]">
      <button
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-1.5 text-left hover:bg-hover"
      >
        <ListChecks className="size-3.5 text-accent" />
        <span className="min-w-0 flex-1 truncate">{active[0]?.content ?? 'Plan'}</span>
        <span className="shrink-0 text-xs text-subtle">
          {done}/{todos.length}
        </span>
        <ChevronRight
          className={clsx('size-3.5 text-subtle transition-transform', open && 'rotate-90')}
        />
      </button>
      {open && (
        <ul className="space-y-1 border-t border-line px-3 py-2">
          {todos.map((t, i) => (
            <li key={t.id ?? i} className="flex items-start gap-2">
              <span
                className={clsx(
                  'mt-1 size-2.5 shrink-0 rounded-full border',
                  t.status === 'completed' && 'border-ok bg-ok',
                  t.status === 'in_progress' && 'border-accent bg-accent-soft',
                  (t.status === 'pending' || !t.status) && 'border-line-strong',
                  t.status === 'cancelled' && 'border-subtle',
                )}
              />
              <span
                className={clsx(
                  t.status === 'completed' && 'text-subtle line-through',
                  t.status === 'cancelled' && 'text-subtle',
                )}
              >
                {t.content}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
