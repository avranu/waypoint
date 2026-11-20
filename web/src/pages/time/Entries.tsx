import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation } from '@tanstack/react-query'
import clsx from 'clsx'
import { Bot, MoreHorizontal, Pencil, Play, Timer, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import { agentLabel } from '@/lib/agents'
import { useTimer } from '@/lib/queries'
import { duration, localISO } from '@/lib/format'
import type { IssueRef, TimeEntry } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Empty } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { TypeIcon } from '@/components/issue/icons'
import { IssuePick } from './IssuePick'
import { elapsed } from './TimerBar'
import {
  dayHeading,
  durationInput,
  hhmm,
  parseDuration,
  rfc3339,
  timeOfDay,
  useInvalidateTime,
} from './util'
import { activate } from '@/lib/hooks'

export function EntryList({ entries }: { entries: TimeEntry[] }) {
  const days = useMemo(() => {
    const m = new Map<string, { date: Date; items: TimeEntry[] }>()
    for (const e of entries) {
      const d = new Date(e.started_at)
      const k = localISO(d)
      if (!m.has(k))
        m.set(k, { date: new Date(d.getFullYear(), d.getMonth(), d.getDate()), items: [] })
      m.get(k)!.items.push(e)
    }
    return [...m.entries()]
  }, [entries])
  if (entries.length === 0) {
    return (
      <Empty icon={<Timer />} title="No time this week">
        Start the timer above, or add time you've already spent.
      </Empty>
    )
  }
  return (
    <div className="space-y-5">
      {days.map(([key, { date, items }]) => (
        <section key={key} id={`day-${key}`} className="scroll-mt-4">
          <div className="mb-1.5 flex items-baseline justify-between px-1">
            <h2 className="text-xs font-semibold uppercase tracking-wide text-subtle">
              {dayHeading(date)}
            </h2>
            <span className="text-xs font-medium tabular-nums text-muted">
              {duration(items.reduce((s, e) => s + elapsed(e), 0))}
            </span>
          </div>
          <ul className="overflow-hidden rounded-xl border border-line bg-panel">
            {items.map((e) => (
              <EntryRow key={e.id} entry={e} />
            ))}
          </ul>
        </section>
      ))}
    </div>
  )
}

function EntryRow({ entry: e }: { entry: TimeEntry }) {
  const invalidate = useInvalidateTime()
  const { start } = useTimer()
  const m = usePopover()
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  const del = useMutation({
    mutationFn: () => api.del(`/time/${e.id}`),
    onSettled: invalidate,
    onError: toastError,
  })
  const cont = () =>
    start.mutate(
      { ...(e.issue ? { issue: e.issue.key } : {}), ...(e.note ? { note: e.note } : {}) },
      { onSuccess: () => toast('Timer started'), onError: toastError },
    )
  if (editing) return <EntryEditor entry={e} onDone={() => setEditing(false)} />
  return (
    <li
      className={clsx(
        'group flex items-center gap-3 border-b border-line px-3 py-2.5 last:border-b-0 sm:px-4',
        e.running && 'bg-accent-soft/40',
      )}
    >
      <div
        role="button"
        tabIndex={0}
        onClick={() => setEditing(true)}
        onKeyDown={(ev) => activate(ev) && setEditing(true)}
        className="min-w-0 flex-1 cursor-pointer text-left"
        title="Edit entry"
      >
        <div className="flex min-w-0 items-center gap-1.5 text-[13px]">
          {e.running && <span className="size-2 shrink-0 animate-pulse rounded-full bg-accent" />}
          {e.issue ? (
            <>
              <TypeIcon type={e.issue.type} />
              <Link
                to="/issue/$key"
                params={{ key: e.issue.key }}
                onClick={(ev) => ev.stopPropagation()}
                className="shrink-0 font-medium text-muted hover:text-fg hover:underline"
              >
                {e.issue.key}
              </Link>
              <span className="truncate">{e.issue.title}</span>
            </>
          ) : (
            <span className={clsx('truncate', !e.note && 'text-subtle')}>
              {e.note || 'No description'}
            </span>
          )}
        </div>
        <div className="mt-0.5 flex min-w-0 gap-2 text-xs text-subtle">
          <span className="shrink-0 tabular-nums">
            {timeOfDay(e.started_at)} – {e.ended_at ? timeOfDay(e.ended_at) : 'now'}
          </span>
          {e.issue && e.note && <span className="truncate">· {e.note}</span>}
          {e.agent && (
            <span className="inline-flex shrink-0 items-center gap-0.5" title={e.agent}>
              · <Bot className="size-3" /> {agentLabel(e.agent)}
            </span>
          )}
        </div>
      </div>
      <span
        className={clsx(
          'shrink-0 text-[13px] font-medium tabular-nums',
          e.running && 'text-accent',
        )}
      >
        {duration(elapsed(e))}
      </span>
      {!e.running && (
        <IconButton
          label="Continue"
          onClick={cont}
          className="hidden shrink-0 sm:inline-flex sm:opacity-0 sm:group-hover:opacity-100 sm:focus-visible:opacity-100"
          icon={<Play className="size-3.5" />}
        />
      )}
      <IconButton
        ref={m.anchor}
        label="Entry actions"
        onClick={m.toggle}
        className="shrink-0"
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            ...(e.running ? [] : [{ label: 'Continue', icon: <Play />, onSelect: cont }]),
            { label: 'Edit', icon: <Pencil />, onSelect: () => setEditing(true) },
            { label: 'Delete', icon: <Trash2 />, danger: true, onSelect: () => setDeleting(true) },
          ]}
        />
      </Popover>
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        title="Delete this time entry?"
        onConfirm={() => del.mutate()}
      />
    </li>
  )
}

function EntryEditor({ entry: e, onDone }: { entry: TimeEntry; onDone: () => void }) {
  const invalidate = useInvalidateTime()
  const started = new Date(e.started_at)
  const [note, setNote] = useState(e.note)
  const [issue, setIssue] = useState<IssueRef | null>(e.issue)
  const [startTime, setStartTime] = useState(hhmm(started))
  const [dur, setDur] = useState(e.running ? '' : durationInput(e.seconds))
  const durSecs = parseDuration(dur)
  const durBad = !e.running && durSecs === null

  const save = useMutation({
    mutationFn: () => {
      const body: { note?: string; issue?: string; started_at?: string; duration?: string } = {}
      if (note.trim() !== e.note) body.note = note.trim()
      if ((issue?.key ?? null) !== (e.issue?.key ?? null)) body.issue = issue?.key ?? ''
      if (startTime !== hhmm(started)) {
        const [h, mi] = startTime.split(':').map(Number)
        const d = new Date(started)
        d.setHours(h, mi, 0, 0)
        body.started_at = rfc3339(d)
      }
      if (!e.running && (body.started_at || dur !== durationInput(e.seconds)))
        body.duration = dur.trim()
      if (Object.keys(body).length === 0) return Promise.resolve(null)
      return api.patch<TimeEntry>(`/time/${e.id}`, body)
    },
    onSuccess: () => {
      invalidate()
      onDone()
    },
    onError: toastError,
  })
  return (
    <li className="border-b border-line bg-sunken/50 px-3 py-3 last:border-b-0 sm:px-4">
      <form
        className="space-y-2"
        onSubmit={(ev) => {
          ev.preventDefault()
          if (!durBad && startTime) save.mutate()
        }}
        onKeyDown={(ev) => ev.key === 'Escape' && (ev.stopPropagation(), onDone())}
      >
        <Input
          autoFocus
          value={note}
          onChange={(ev) => setNote(ev.target.value)}
          placeholder="Description"
        />
        <div className="flex flex-wrap items-center gap-2">
          <IssuePick
            value={issue}
            onChange={setIssue}
            className="min-w-0 max-w-full flex-1 sm:max-w-72"
          />
          <label className="flex items-center gap-1.5 text-xs text-subtle">
            Start
            <Input
              type="time"
              value={startTime}
              onChange={(ev) => setStartTime(ev.target.value)}
              className="w-28"
              required
            />
          </label>
          {!e.running && (
            <label className="flex items-center gap-1.5 text-xs text-subtle">
              Duration
              <Input
                value={dur}
                onChange={(ev) => setDur(ev.target.value)}
                placeholder="1h30m"
                aria-invalid={durBad}
                className={clsx('w-24 font-mono', durBad && '!border-danger')}
              />
            </label>
          )}
        </div>
        <div className="flex justify-end gap-2">
          <Button size="sm" variant="ghost" onClick={onDone}>
            Cancel
          </Button>
          <Button
            size="sm"
            type="submit"
            variant="primary"
            disabled={durBad || !startTime}
            loading={save.isPending}
          >
            Save
          </Button>
        </div>
      </form>
    </li>
  )
}
