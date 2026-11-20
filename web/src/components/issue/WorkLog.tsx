// A task's work log on its issue page (WP-285): autopilot's adversarial
// review rounds with what each found and fixed, the findings of the pull
// request reviews and what became of them, notes, and the runs and pull
// requests around them, oldest first. Waypoint writes all of it; a note is
// the only thing a person adds.

import { useRef, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  ChevronRight,
  FlaskConical,
  GitPullRequest,
  MessageSquareText,
  ScanSearch,
  StickyNote,
  TriangleAlert,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { plural, relativeTime, safeUrl } from '@/lib/format'
import type { WorkLog as WorkLogData, WorkLogEntry, WorkLogSummary } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { Select, Textarea } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'

const MAX_NOTE = 4000
const SHOWN = 8

/** What ended well, badly or not at all, as a colour. */
const tone = (state?: string) => {
  switch (state) {
    case 'fixed':
    case 'verified':
    case 'done':
    case 'merged':
      return 'var(--color-ok)'
    case 'rejected':
    case 'dismissed':
    case 'withdrawn':
    case 'skipped':
    case 'closed':
      return 'var(--color-muted)'
    case 'interrupted':
    case 'failed':
      return 'var(--color-warn)'
    case 'open':
    case 'running':
    case 'pending':
      return 'var(--color-accent)'
  }
  return undefined
}

const icon = (e: WorkLogEntry) => {
  const cls = 'mt-0.5 size-3.5 shrink-0 text-subtle'
  switch (e.kind) {
    case 'review':
    case 'own_review':
      return (
        <ScanSearch
          className={clsx(
            cls,
            e.state === 'running' && 'animate-pulse text-accent motion-reduce:animate-none',
          )}
        />
      )
    case 'note':
      return <StickyNote className={cls} />
    case 'pr':
      return <GitPullRequest className={cls} />
    case 'finding':
    case 'comment':
      return <MessageSquareText className={cls} />
    case 'tested':
      return <FlaskConical className={cls} />
    case 'mismatch':
      return <TriangleAlert className={clsx(cls, 'text-warn')} />
    default:
      return <Bot className={cls} />
  }
}

/** The log's counts in a line; empty when there's nothing to count. */
export function summaryParts(s: WorkLogSummary): string[] {
  const parts: string[] = []
  if (s.reviews > 0) {
    let p = plural(s.reviews, 'review round')
    if (s.counted > 0 || s.found > 0)
      p += `: ${s.found} found, ${s.fixed} fixed, ${s.rejected} rejected`
    if (s.pushed > 0) p += ` · ${s.pushed} pushed fixes`
    parts.push(p)
  } else if (s.found > 0) {
    // Only notes tagged found, fixed or rejected.
    parts.push(`${s.found} found, ${s.fixed} fixed, ${s.rejected} rejected`)
  }
  if (s.own_reviews > 0) parts.push(`${plural(s.own_reviews, 'own review')}: ${s.own_found} found`)
  if (s.pr_findings > 0)
    parts.push(
      `PR review: ${plural(s.pr_findings, 'finding')} (${s.pr_fixed} fixed, ${s.pr_rejected} rejected, ${s.pr_open} open)`,
    )
  if (s.notes > 0) parts.push(plural(s.notes, 'note'))
  if (s.mismatches > 0) parts.push(`${plural(s.mismatches, 'statement')} the records contradict`)
  return parts
}

/** Whether the log holds more than the runs and pull requests the page shows elsewhere. */
export const workLogHasNews = (l: WorkLogData | undefined) =>
  !!l && l.entries.some((e) => e.kind !== 'run' && e.kind !== 'pr')

export function WorkLog({ issueKey, editable }: { issueKey: string; editable: boolean }) {
  const log = useQuery(q.workLog(issueKey))
  const [all, setAll] = useState(false)
  const [adding, setAdding] = useState(false)
  if (log.isError)
    return <p className="text-xs text-subtle">The work log couldn’t be loaded right now.</p>
  if (!log.data) return null
  const entries = log.data.entries
  const parts = summaryParts(log.data.summary)
  const shown = all ? entries : entries.slice(-SHOWN)
  return (
    <div className="overflow-hidden rounded-lg border border-line">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 border-b border-line bg-sunken/60 px-3 py-2 text-xs text-muted">
        {parts.length > 0 ? (
          parts.map((p) => <span key={p}>{p}</span>)
        ) : (
          <span>Nothing found or fixed yet</span>
        )}
        {editable && !adding && (
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="ml-auto text-subtle hover:text-fg"
          >
            + Note
          </button>
        )}
      </div>
      {adding && <NoteForm issueKey={issueKey} onDone={() => setAdding(false)} />}
      {entries.length > SHOWN && (
        <button
          onClick={() => setAll(!all)}
          className="w-full border-b border-line py-1.5 text-xs text-subtle hover:text-fg"
        >
          {all ? 'Show the latest only' : `Show all ${entries.length}`}
        </button>
      )}
      <ul className="divide-y divide-line">
        {shown.map((e, i) => (
          <Entry key={`${e.kind}-${e.id ?? i}-${e.at}`} e={e} />
        ))}
      </ul>
    </div>
  )
}

function Entry({ e }: { e: WorkLogEntry }) {
  const [open, setOpen] = useState(false)
  const who =
    e.by ||
    (e.agent ? agentLabel(e.agent) : '') ||
    e.author?.display_name ||
    e.author?.username ||
    ''
  const detail =
    !!e.body ||
    (e.items?.length ?? 0) > 0 ||
    (e.history?.length ?? 0) > 0 ||
    !!e.reason ||
    e.read_state === 'pending'
  const quiet = e.kind === 'run' || e.kind === 'pr' || e.kind === 'tested'
  const href = e.kind === 'pr' ? safeUrl(e.pr?.url) : undefined
  return (
    <li className="px-3 py-2 text-[13px]">
      <div className="flex items-start gap-2.5">
        {icon(e)}
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
            {detail ? (
              <button
                type="button"
                onClick={() => setOpen(!open)}
                aria-expanded={open}
                className="inline-flex min-w-0 items-baseline gap-1 text-left hover:text-fg"
              >
                <ChevronRight
                  className={clsx(
                    'size-3 shrink-0 self-center text-subtle transition-transform',
                    open && 'rotate-90',
                  )}
                />
                <span className="break-words">{e.title}</span>
              </button>
            ) : href ? (
              <a
                href={href}
                target="_blank"
                rel="noreferrer"
                className="break-words hover:underline"
              >
                {e.title}
              </a>
            ) : (
              <span className={clsx('break-words', quiet && 'text-muted')}>{e.title}</span>
            )}
            {e.tag && <Badge color={tone(e.tag === 'found' ? 'open' : e.tag)}>{e.tag}</Badge>}
            {(e.kind === 'finding' || e.kind === 'comment') && e.state && (
              <Badge color={tone(e.state)}>{e.state}</Badge>
            )}
            {e.kind === 'review' && e.pushed && <Badge color={tone('fixed')}>pushed fixes</Badge>}
          </div>
          <div className="mt-0.5 flex flex-wrap gap-x-2 text-xs text-subtle">
            <span title={new Date(e.at).toLocaleString()}>{relativeTime(e.at)}</span>
            {who && <span>{who}</span>}
            {e.pr && e.kind !== 'pr' && (
              <span>
                {e.pr.repo}#{e.pr.number}
              </span>
            )}
            {e.path && (
              <span className="truncate font-mono">
                {e.path}
                {e.line ? `:${e.line}` : ''}
              </span>
            )}
          </div>
          {open && <EntryDetail e={e} />}
        </div>
      </div>
    </li>
  )
}

function EntryDetail({ e }: { e: WorkLogEntry }) {
  return (
    <div className="mt-2 space-y-2 text-xs">
      {e.reason && <p className="text-muted">{e.reason}</p>}
      {e.read_state === 'pending' && <p className="text-subtle">Counting what the report found…</p>}
      {e.read_state === 'failed' && <p className="text-subtle">The report couldn’t be counted.</p>}
      {(e.items?.length ?? 0) > 0 && (
        <ul className="space-y-1">
          {e.items!.map((it, i) => (
            <li key={i} className="flex items-baseline gap-2">
              <Badge color={tone(it.outcome)} className="shrink-0">
                {it.outcome}
              </Badge>
              <span className="min-w-0 break-words text-fg">
                {it.claim}
                {it.file && <span className="font-mono text-subtle"> ({it.file})</span>}
              </span>
            </li>
          ))}
        </ul>
      )}
      {(e.history?.length ?? 0) > 0 && (
        <ul className="space-y-0.5 text-muted">
          {e.history!.map((h, i) => (
            <li key={i} className="break-words">
              <span title={new Date(h.at).toLocaleString()}>{relativeTime(h.at)}</span>:{' '}
              <b className="font-medium">{h.state}</b>
              {h.by && <> by {h.by}</>}
              {h.note && <> — {h.note}</>}
            </li>
          ))}
        </ul>
      )}
      {e.body && e.kind !== 'note' && (
        <div>
          {e.kind === 'review' && <p className="mb-1 text-subtle">The agent’s report:</p>}
          {e.kind === 'own_review' && <p className="mb-1 text-subtle">The reviewer’s report:</p>}
          <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md bg-sunken p-2 font-sans text-[12px] leading-relaxed text-fg">
            {e.body}
          </pre>
        </div>
      )}
      {e.body && e.kind === 'note' && e.body.includes('\n') && (
        <p className="whitespace-pre-wrap break-words text-fg">{e.body}</p>
      )}
      {e.kind === 'own_review' && e.prompt && (
        <details>
          <summary className="cursor-pointer text-subtle">What it was asked</summary>
          <pre className="mt-1 max-h-60 overflow-auto whitespace-pre-wrap break-words rounded-md bg-sunken p-2 font-sans text-[12px] leading-relaxed text-muted">
            {e.prompt}
          </pre>
        </details>
      )}
      {(e.kind === 'review' || e.kind === 'own_review') && e.read_by && e.read_by !== 'rules' && (
        <p className="text-subtle">Counted by {e.read_by}.</p>
      )}
    </div>
  )
}

function NoteForm({ issueKey, onDone }: { issueKey: string; onDone: () => void }) {
  const qc = useQueryClient()
  const [body, setBody] = useState('')
  const [tag, setTag] = useState('')
  const box = useRef<HTMLTextAreaElement>(null)
  const add = useMutation({
    mutationFn: () => api.post(`/issues/${enc(issueKey)}/worklog`, { body: body.trim(), tag }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['workLog', issueKey] })
      // The form stays for the next note; Esc or Cancel closes it.
      setBody('')
      box.current?.focus()
    },
    onError: toastError,
  })
  const tooLong = body.trim().length > MAX_NOTE
  return (
    <form
      className="space-y-2 border-b border-line p-3"
      onSubmit={(ev) => {
        ev.preventDefault()
        if (body.trim() && !tooLong) add.mutate()
      }}
    >
      <Textarea
        ref={box}
        autoFocus
        autoGrow
        rows={2}
        value={body}
        onChange={(ev) => setBody(ev.target.value)}
        placeholder="What was found or fixed, and why it matters"
        aria-label="Note"
        onKeyDown={(ev) => {
          if (ev.key === 'Escape') onDone()
          if (ev.key === 'Enter' && (ev.metaKey || ev.ctrlKey))
            ev.currentTarget.form?.requestSubmit()
        }}
      />
      {tooLong && (
        <p className="text-xs text-danger">A note can be at most {MAX_NOTE} characters.</p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Select
          value={tag}
          onChange={(ev) => setTag(ev.target.value)}
          aria-label="Count it as"
          className="w-auto"
        >
          <option value="">Just a note</option>
          <option value="found">An issue found</option>
          <option value="fixed">An issue fixed</option>
          <option value="rejected">An issue rejected</option>
        </Select>
        <div className="ml-auto flex gap-2">
          <Button size="sm" variant="ghost" onClick={onDone}>
            Cancel
          </Button>
          <Button
            type="submit"
            size="sm"
            variant="primary"
            loading={add.isPending}
            disabled={!body.trim() || tooLong}
          >
            Add note
          </Button>
        </div>
      </div>
    </form>
  )
}
