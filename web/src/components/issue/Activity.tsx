import { useMemo, useState } from 'react'
import { useDraft } from '@/lib/drafts'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Bot, ChevronRight, MoreHorizontal, Paperclip, Play, Plus, Square } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { downloadURL } from '@/lib/attachments'
import { q, useTimer } from '@/lib/queries'
import { refsQuery } from '@/lib/knowledge'
import { clock, dateTime, duration, relativeTime, safeUrl } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { Avatar } from '@/components/ui/misc'
import { Button, IconButton } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { Confirm } from '@/components/ui/dialog'
import { toastError } from '@/components/ui/toast'
import { agentLabel, whoLabel } from '@/lib/agents'
import { WrittenBy } from '@/components/Provenance'
import { MarkdownEditor } from './Description'
import { MarkdownInput } from './MarkdownInput'
import type { Comment, IssueDetail, TextRef, WEvent } from '@/lib/types'

type Tab = 'comments' | 'history' | 'time'

export function IssueActivity({ issue }: { issue: IssueDetail }) {
  const [tab, setTab] = useState<Tab>('comments')
  const comments = useQuery(q.comments(issue.key))
  return (
    <div>
      <div className="mb-3 flex items-center gap-1 border-b border-line">
        {(
          [
            ['comments', `Comments${comments.data?.length ? ` · ${comments.data.length}` : ''}`],
            ['history', 'History'],
            ['time', `Time${issue.time_spent ? ` · ${duration(issue.time_spent)}` : ''}`],
          ] as [Tab, string][]
        ).map(([t, label]) => (
          <button
            key={t}
            onClick={() => setTab(t)}
            className={clsx(
              '-mb-px border-b-2 px-2.5 pb-2 text-[13px] font-medium',
              tab === t ? 'border-accent text-fg' : 'border-transparent text-muted hover:text-fg',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === 'comments' && <Comments issue={issue} comments={comments.data ?? []} />}
      {tab === 'history' && <History issueKey={issue.key} />}
      {tab === 'time' && <TimeLog issue={issue} />}
    </div>
  )
}

function Comments({ issue, comments }: { issue: IssueDetail; comments: Comment[] }) {
  const qc = useQueryClient()
  const me = useQuery(q.me()).data
  const [body, setBody, { restored }] = useDraft(`comment:${issue.key}`, '')
  const add = useMutation({
    mutationFn: (b: string) => api.post<Comment>(`/issues/${enc(issue.key)}/comments`, { body: b }),
    onSuccess: (c) => {
      // A live refetch may already have brought it in.
      qc.setQueryData<Comment[]>(['comments', issue.key], (l) =>
        l?.some((x) => x.id === c.id) ? l : [...(l ?? []), c],
      )
      setBody('')
      qc.invalidateQueries({ queryKey: ['issue', issue.key] })
    },
    onError: toastError,
  })
  const submit = () => body.trim() && !add.isPending && add.mutate(body.trim())
  const all = useMemo(() => comments.map((c) => c.body).join('\n\n'), [comments])
  const refs = useQuery({
    ...refsQuery(all, issue.key.replace(/-\d+$/, '')),
    placeholderData: (p) => p,
    enabled: comments.length > 0,
  }).data
  const batched = useMemo(() => ({ refs }), [refs])
  return (
    <div className="space-y-4">
      {comments.map((c) => (
        <CommentItem
          key={c.id}
          c={c}
          issueKey={issue.key}
          mine={c.author?.id === me?.id}
          canModerate={issue.role === 'owner'}
          canReview={issue.role === 'owner' || issue.role === 'editor'}
          batchedRefs={batched}
        />
      ))}
      <div className="flex gap-2.5">
        <Avatar user={me} size={28} className="mt-0.5 hidden sm:inline-flex" />
        <div className="flex-1 space-y-2">
          <MarkdownInput
            autoGrow
            rows={2}
            value={body}
            onValueChange={setBody}
            issueKey={issue.key}
            placeholder="Leave a comment… (@mention someone, paste or drop files)"
            aria-label="Comment"
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault()
                submit()
              }
            }}
          />
          {body.trim() && (
            <div className="flex items-center justify-end gap-2">
              {restored && <span className="mr-auto text-xs text-muted">Draft restored</span>}
              <span className="text-xs text-subtle">⌘↵</span>
              <Button size="sm" variant="primary" loading={add.isPending} onClick={submit}>
                Comment
              </Button>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

function CommentItem({
  c,
  issueKey,
  mine,
  canModerate,
  canReview,
  batchedRefs,
}: {
  c: Comment
  issueKey: string
  mine: boolean
  canModerate: boolean
  canReview: boolean
  batchedRefs: { refs: TextRef[] | undefined }
}) {
  const qc = useQueryClient()
  const menu = usePopover()
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const refresh = () => qc.invalidateQueries({ queryKey: ['comments', issueKey] })
  // Rethrow after a failed save, so the editor reopens with the typed text.
  const save = (body: string) => {
    qc.setQueryData<Comment[]>(['comments', issueKey], (l) =>
      l?.map((x) => (x.id === c.id ? { ...x, body } : x)),
    )
    return api.patch(`/comments/${c.id}`, { body }).then(
      () => {
        refresh()
        setEditing(false)
      },
      (e) => {
        refresh()
        toastError(e)
        throw e
      },
    )
  }
  const del = () => api.del(`/comments/${c.id}`).then(refresh, toastError)
  return (
    <div className="group flex gap-2.5" id={`comment-${c.id}`}>
      {c.agent ? (
        <span className="mt-0.5 inline-flex size-7 shrink-0 items-center justify-center rounded-full bg-sunken text-muted">
          <Bot className="size-4" />
        </span>
      ) : (
        <Avatar
          user={c.author && c.author.id === 0 ? { ...c.author, avatar_url: undefined } : c.author}
          size={28}
          className="mt-0.5"
        />
      )}
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 text-[13px]">
          {c.agent ? (
            <span className="font-medium" title={c.agent}>
              {agentLabel(c.agent)}
            </span>
          ) : (
            <span className="font-medium">
              {c.author?.display_name || c.author?.username || 'Someone'}
              {c.via && <span className="font-normal text-subtle"> via {c.via}</span>}
            </span>
          )}
          <span className="text-xs text-subtle" title={dateTime(c.created_at)}>
            {relativeTime(c.created_at)}
            {c.updated_at !== c.created_at && ' · edited'}
          </span>
          {c.agent && (c.unreviewed || c.reviewed_by) && (
            <WrittenBy
              agent={c.agent}
              unreviewed={c.unreviewed}
              reviewedBy={c.reviewed_by}
              review={canReview ? { type: 'comment', ref: String(c.id) } : undefined}
              stateOnly
            />
          )}
          {(mine || canModerate) && (
            <>
              <IconButton
                ref={menu.anchor}
                label="Comment actions"
                size="xs"
                className="ml-auto opacity-0 group-hover:opacity-100 focus-visible:opacity-100 max-sm:opacity-100"
                onClick={menu.toggle}
                icon={<MoreHorizontal className="size-3.5" />}
              />
              <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
                <MenuList
                  onClose={menu.close}
                  items={[
                    ...(mine ? [{ label: 'Edit', onSelect: () => setEditing(true) }] : []),
                    { label: 'Delete', danger: true, onSelect: () => setDeleting(true) },
                  ]}
                />
              </Popover>
            </>
          )}
        </div>
        {editing ? (
          <div className="mt-1">
            <MarkdownEditor
              key="edit"
              value={c.body}
              onSave={save}
              issueKey={issueKey}
              minRows={2}
              autoEdit
              onCancel={() => setEditing(false)}
              batchedRefs={batchedRefs}
            />
          </div>
        ) : (
          <div className="mt-0.5">
            <MarkdownEditor
              key="view"
              value={c.body}
              onSave={save}
              editable={false}
              project={issueKey.replace(/-\d+$/, '')}
              batchedRefs={batchedRefs}
            />
            <CommentFiles comment={c} />
          </div>
        )}
      </div>
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        title="Delete this comment?"
        onConfirm={del}
      />
    </div>
  )
}

/** Files attached to the comment itself (by wp attach comment:ID), unless its text already shows them. */
function CommentFiles({ comment: c }: { comment: Comment }) {
  const files = (c.attachments ?? []).filter((f) => !c.body.includes(f.url))
  if (files.length === 0) return null
  return (
    <ul className="mt-1.5 flex flex-wrap gap-2" aria-label="Attached files">
      {files.map((f) => (
        <li key={f.id} className="min-w-0">
          {f.kind === 'image' ? (
            <a href={f.url} target="_blank" rel="noreferrer" title={f.caption || f.name}>
              <img
                src={f.url}
                alt={f.caption || f.name}
                loading="lazy"
                className="max-h-32 rounded-lg border border-line"
              />
            </a>
          ) : (
            <a
              href={f.kind === 'video' || f.kind === 'text' ? f.url : downloadURL(f)}
              target={f.kind === 'file' ? undefined : '_blank'}
              rel="noreferrer"
              className="inline-flex max-w-full items-center gap-1.5 rounded-lg border border-line px-2 py-1 text-xs hover:bg-hover"
            >
              <Paperclip className="size-3 shrink-0 text-subtle" />
              <span className="truncate">{f.caption || f.name}</span>
            </a>
          )}
        </li>
      ))}
    </ul>
  )
}

const fieldNames: Record<string, string> = {
  due_date: 'due date',
  start_date: 'start date',
  points: 'estimate',
}

function describe(e: WEvent): React.ReactNode {
  const v = (s?: string) => <span className="font-medium text-fg">{s || 'none'}</span>
  switch (e.kind) {
    case 'issue.created':
      return 'created the issue'
    case 'issue.status_changed':
      return (
        <>
          moved from {v(e.old)} to {v(e.new)}
        </>
      )
    case 'issue.assigned':
      return <>assigned {v('@' + e.new)}</>
    case 'issue.unassigned':
      return <>unassigned {v('@' + e.old)}</>
    case 'issue.commented':
      return 'commented'
    case 'issue.linked':
      return (
        <>
          linked {v(e.field)}{' '}
          {e.new && (
            <a
              className="text-accent hover:underline"
              href={safeUrl(e.new)}
              target="_blank"
              rel="noreferrer"
            >
              {e.new.replace(/^https?:\/\//, '')}
            </a>
          )}
        </>
      )
    case 'issue.shared':
      return <>shared with {v('@' + e.new)}</>
    case 'external.pr_opened':
      return 'pull request opened'
    case 'external.pr_merged':
      return 'pull request merged'
    case 'external.pr_closed':
      return 'pull request closed'
    case 'external.issue_closed':
      return 'linked issue closed'
    case 'sprint.started':
      return 'sprint started'
    case 'issue.updated': {
      const f = fieldNames[e.field ?? ''] ?? e.field
      if (e.field === 'agent')
        return (
          <>
            changed who can take it from {v(whoLabel(e.old))} to {v(whoLabel(e.new))}
          </>
        )
      if (e.field === 'description') return 'edited the description'
      if (e.field === 'intent') {
        if (!e.new) return 'cleared the intent'
        if (e.new === e.old) return 'kept the suggested intent'
        return (
          <>
            {e.old ? 'rewrote' : 'set'} the intent: {v(e.new)}
          </>
        )
      }
      if (e.field === 'attachment') return e.new ? <>added {v(e.new)}</> : <>removed {v(e.old)}</>
      if (e.field === 'relation') {
        // Older removals have the word "removed" in old, newer ones what was removed.
        if (e.new) return <>added relation {v(e.new)}</>
        return e.old && e.old !== 'removed' ? (
          <>removed relation {v(e.old)}</>
        ) : (
          'removed a relation'
        )
      }
      if (!e.new) return <>cleared {f}</>
      if (!e.old)
        return (
          <>
            set {f} to {v(e.new)}
          </>
        )
      return (
        <>
          changed {f} from {v(e.old)} to {v(e.new)}
        </>
      )
    }
  }
  return e.kind
}

/** What repeats may fold into one line, and how that line reads for n events (null: never fold). */
function repeated(e: WEvent): { key: string; text: (n: number) => string } | null {
  const who = `${e.actor?.id ?? ''}|${e.via_automation ?? ''}`
  const times = (n: number, one: string) => `${one} ${n} times`
  if (e.kind === 'issue.commented') return { key: `${who}|c`, text: (n) => times(n, 'commented') }
  if (e.kind !== 'issue.updated') return null
  switch (e.field) {
    case 'description':
      return { key: `${who}|d`, text: (n) => times(n, 'edited the description') }
    case 'intent':
      return { key: `${who}|i`, text: (n) => times(n, 'edited the intent') }
    case 'attachment': {
      const add = !!e.new
      return {
        key: `${who}|a${add}`,
        text: (n) => `${add ? 'added' : 'removed'} ${n} attachments`,
      }
    }
    case 'relation': {
      const add = !!e.new
      return {
        key: `${who}|r${add}`,
        text: (n) => `${add ? 'added' : 'removed'} ${n} relations`,
      }
    }
  }
  return null
}

type HistoryRow = { events: WEvent[]; text?: string }

/** Consecutive repeats by one actor become one row; the list is newest first. */
function foldHistory(events: WEvent[]): HistoryRow[] {
  const rows: (HistoryRow & { key?: string; fold?: (n: number) => string })[] = []
  for (const e of events) {
    const r = repeated(e)
    const last = rows[rows.length - 1]
    if (r && last?.key === r.key) last.events.push(e)
    else rows.push({ events: [e], key: r?.key, fold: r?.text })
  }
  return rows.map((r) => ({
    events: r.events,
    text: r.events.length > 1 ? r.fold?.(r.events.length) : undefined,
  }))
}

function EventActor({ e }: { e: WEvent }) {
  return e.via_automation ? (
    <span
      className="mt-0.5 flex size-5 items-center justify-center rounded-full bg-accent-soft text-accent"
      title={`Automation: ${e.via_automation}`}
    >
      <Bot className="size-3" />
    </span>
  ) : (
    <Avatar user={e.actor} size={20} />
  )
}

function HistoryRowView({ row }: { row: HistoryRow }) {
  const [open, setOpen] = useState(false)
  const first = row.events[0]
  if (!row.text) {
    return (
      <li className="flex items-start gap-2.5 text-[13px] text-muted">
        <EventActor e={first} />
        <div className="min-w-0 flex-1">
          <span className="font-medium text-fg">{first.actor?.display_name ?? 'Waypoint'}</span>{' '}
          {describe(first)}
          {first.via_automation && (
            <span className="text-subtle"> · via “{first.via_automation}”</span>
          )}
          <span className="ml-1.5 text-xs text-subtle" title={dateTime(first.at)}>
            {relativeTime(first.at)}
          </span>
        </div>
      </li>
    )
  }
  // Newest first: the last event is the earliest.
  const from = row.events[row.events.length - 1].at
  const to = first.at
  const a = relativeTime(from)
  const b = relativeTime(to)
  return (
    <li className="flex items-start gap-2.5 text-[13px] text-muted">
      <EventActor e={first} />
      <div className="min-w-0 flex-1">
        <button
          onClick={() => setOpen(!open)}
          aria-expanded={open}
          className="inline-flex max-w-full flex-wrap items-baseline gap-x-1 text-left hover:text-fg"
        >
          <span className="font-medium text-fg">{first.actor?.display_name ?? 'Waypoint'}</span>
          <span>{row.text}</span>
          <span className="text-xs text-subtle" title={`${dateTime(from)} – ${dateTime(to)}`}>
            · {a === b ? a : `${a} – ${b}`}
          </span>
          <ChevronRight
            className={clsx(
              'size-3 self-center text-subtle transition-transform',
              open && 'rotate-90',
            )}
          />
        </button>
        {open && (
          <ul className="mt-1.5 space-y-1 border-l border-line pl-3">
            {row.events.map((e) => (
              <li key={e.id} className="text-xs">
                {describe(e)}
                <span className="ml-1.5 text-subtle" title={dateTime(e.at)}>
                  {relativeTime(e.at)}
                </span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </li>
  )
}

function History({ issueKey }: { issueKey: string }) {
  const h = useQuery(q.history(issueKey))
  const rows = useMemo(() => foldHistory(h.data ?? []), [h.data])
  return (
    <ol className="relative space-y-3 pl-1">
      {rows.map((r) => (
        <HistoryRowView key={r.events[0].id ?? r.events[0].at} row={r} />
      ))}
      {h.data?.length === 0 && <li className="text-[13px] text-subtle">No history yet.</li>}
    </ol>
  )
}

function TimeLog({ issue }: { issue: IssueDetail }) {
  const qc = useQueryClient()
  const entries = useQuery(q.issueTime(issue.key))
  const timer = useQuery(q.timer()).data
  const { start, stop } = useTimer()
  const running = timer?.issue?.key === issue.key
  useTick(1000, running)
  const [dur, setDur] = useState('')
  const [note, setNote] = useState('')
  const log = useMutation({
    mutationFn: () => api.post('/time', { issue: issue.key, duration: dur, note }),
    onSuccess: () => {
      setDur('')
      setNote('')
      qc.invalidateQueries({ queryKey: ['issueTime', issue.key] })
      qc.invalidateQueries({ queryKey: ['issue', issue.key] })
      qc.invalidateQueries({ queryKey: ['time'] })
    },
    onError: toastError,
  })
  const liveSecs = running && timer ? (Date.now() - new Date(timer.started_at).getTime()) / 1000 : 0
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        {running ? (
          <Button
            size="sm"
            variant="secondary"
            onClick={() => stop.mutate()}
            icon={<Square className="size-3 fill-danger text-danger" />}
          >
            Stop · <span className="tabular-nums">{clock(liveSecs)}</span>
          </Button>
        ) : (
          <Button
            size="sm"
            variant="secondary"
            onClick={() => start.mutate({ issue: issue.key })}
            icon={<Play className="size-3 fill-ok text-ok" />}
          >
            Start timer
          </Button>
        )}
        <form
          className="flex flex-1 items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            if (dur.trim()) log.mutate()
          }}
        >
          <Input
            value={dur}
            onChange={(e) => setDur(e.target.value)}
            placeholder="1h30m"
            className="!h-7 w-24 text-[13px]"
          />
          <Input
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Note (optional)"
            className="!h-7 min-w-0 flex-1 text-[13px]"
          />
          <Button
            size="sm"
            type="submit"
            disabled={!dur.trim()}
            loading={log.isPending}
            icon={<Plus className="size-3.5" />}
          >
            Log
          </Button>
        </form>
      </div>
      <ul className="divide-y divide-line rounded-lg border border-line">
        {(entries.data ?? []).map((t) => (
          <li key={t.id} className="flex items-center gap-2.5 px-3 py-2 text-[13px]">
            <Avatar user={t.user} size={20} />
            <span className="w-16 font-medium tabular-nums">
              {t.running ? clock(t.seconds) : duration(t.seconds)}
            </span>
            <span className="min-w-0 flex-1 truncate text-muted">{t.note}</span>
            <span className="text-xs text-subtle">{dateTime(t.started_at)}</span>
          </li>
        ))}
        {entries.data?.length === 0 && (
          <li className="px-3 py-3 text-[13px] text-subtle">No time tracked yet.</li>
        )}
      </ul>
    </div>
  )
}
