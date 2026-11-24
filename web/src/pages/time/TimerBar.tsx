import { useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Play, Square } from 'lucide-react'
import { api } from '@/lib/api'
import { useTimer } from '@/lib/queries'
import { useTick } from '@/lib/hooks'
import { clock } from '@/lib/format'
import type { IssueRef, TimeEntry } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { toastError } from '@/components/ui/toast'
import { TypeIcon } from '@/components/issue/icons'
import { IssuePick } from './IssuePick'
import {
  addDays,
  durationInput,
  hhmm,
  parseDuration,
  rfc3339,
  timeOfDay,
  useInvalidateTime,
} from './util'

/** Edit the running timer in place: its note, and when it started. */
function useEditTimer(timer: TimeEntry) {
  const qc = useQueryClient()
  const invalidate = useInvalidateTime()
  return useMutation({
    mutationFn: (patch: { note?: string; started_at?: string }) =>
      api.patch<TimeEntry>(`/time/${timer.id}`, patch),
    onMutate: (patch) =>
      qc.setQueryData<TimeEntry | null>(['timer'], (t) => (t ? { ...t, ...patch } : t)),
    onError: toastError,
    onSettled: invalidate,
  })
}

/** Text that turns into a field on click; Enter or leaving saves, Esc cancels. */
function InlineField({
  value,
  placeholder,
  label,
  onSave,
  className,
  inputClassName,
  inputType = 'text',
  display,
}: {
  value: string
  placeholder?: string
  label: string
  onSave: (v: string) => void | Promise<unknown>
  className?: string
  inputClassName?: string
  inputType?: 'text' | 'time'
  display?: React.ReactNode
}) {
  const [draft, setDraft] = useState<string | null>(null)
  const saving = useRef(false)
  if (draft === null) {
    return (
      <button
        type="button"
        onClick={() => setDraft(value)}
        title={label}
        aria-label={label}
        className={className}
      >
        {display ?? (value || <span className="text-muted">{placeholder}</span>)}
      </button>
    )
  }
  const commit = async () => {
    if (saving.current) return
    if (draft.trim() === value) return setDraft(null)
    saving.current = true
    try {
      await onSave(draft.trim())
      setDraft(null)
    } catch {
      /* the save reports its own error; the text stays to retry */
    } finally {
      saving.current = false
    }
  }
  return (
    <input
      autoFocus
      type={inputType}
      value={draft}
      aria-label={label}
      placeholder={placeholder}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') e.currentTarget.blur()
        if (e.key === 'Escape') {
          e.stopPropagation()
          setDraft(null)
        }
      }}
      className={inputClassName}
    />
  )
}

const fieldClass = 'rounded-md border border-accent bg-panel px-2 outline-none'

function Running({ timer }: { timer: TimeEntry }) {
  const { stop } = useTimer()
  const edit = useEditTimer(timer)
  useTick(1000)
  const setStart = (v: string) => {
    const [h, m] = v.split(':').map(Number)
    if (Number.isNaN(h) || Number.isNaN(m)) return
    let d = new Date()
    d.setHours(h, m, 0, 0)
    if (d.getTime() > Date.now()) d = addDays(d, -1)
    edit.mutate({ started_at: rfc3339(d) })
  }
  const setElapsed = (v: string) => {
    const secs = parseDuration(v)
    if (secs === null || secs <= 0) return toastError(new Error('Try 1h30m, 45m or 1:30.'))
    edit.mutate({ started_at: rfc3339(new Date(Date.now() - secs * 1000)) })
  }
  return (
    <div className="flex flex-col gap-3 rounded-xl border border-accent/40 bg-accent-soft/40 p-4 shadow-card sm:flex-row sm:items-center">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2 text-xs text-muted">
          <span className="relative flex size-2">
            <span className="absolute inline-flex size-full animate-ping rounded-full bg-accent opacity-60" />
            <span className="relative inline-flex size-2 rounded-full bg-accent" />
          </span>
          Running since
          <InlineField
            value={hhmm(new Date(timer.started_at))}
            inputType="time"
            label="Change when this timer started"
            display={timeOfDay(timer.started_at)}
            onSave={setStart}
            className="-mx-1 rounded px-1 underline decoration-dotted underline-offset-2 hover:bg-hover hover:text-fg"
            inputClassName={fieldClass + ' h-6 text-xs text-fg'}
          />
        </div>
        <div className="mt-1 min-w-0 text-[15px] font-medium">
          {timer.issue ? (
            <Link
              to="/issue/$key"
              params={{ key: timer.issue.key }}
              className="inline-flex max-w-full items-center gap-1.5 hover:underline"
            >
              <TypeIcon type={timer.issue.type} />
              <span className="shrink-0 text-muted">{timer.issue.key}</span>
              <span className="truncate">{timer.issue.title}</span>
            </Link>
          ) : (
            <InlineField
              value={timer.note ?? ''}
              placeholder="Untitled work"
              label="Name this work"
              onSave={(note) => edit.mutateAsync({ note })}
              className="block max-w-full truncate rounded text-left hover:bg-hover"
              inputClassName={fieldClass + ' h-8 w-full text-[15px]'}
            />
          )}
        </div>
        {timer.issue && (
          <InlineField
            value={timer.note ?? ''}
            placeholder="Add a note"
            label="Edit the note"
            onSave={(note) => edit.mutateAsync({ note })}
            className="mt-0.5 block max-w-full truncate rounded text-left text-[13px] text-muted hover:bg-hover hover:text-fg"
            inputClassName={fieldClass + ' mt-0.5 h-7 w-full text-[13px]'}
          />
        )}
      </div>
      <div className="flex items-center justify-between gap-4 sm:justify-end">
        <InlineField
          value={durationInput(elapsed(timer))}
          label="Change how long this has been running"
          display={clock(elapsed(timer))}
          onSave={setElapsed}
          className="rounded-md px-1 font-mono text-3xl font-semibold tabular-nums tracking-tight hover:bg-hover"
          inputClassName={fieldClass + ' h-10 w-32 font-mono text-xl'}
        />
        <Button
          variant="danger"
          size="md"
          icon={<Square className="size-3.5 fill-current" />}
          loading={stop.isPending}
          onClick={() => stop.mutate(undefined, { onError: toastError })}
        >
          Stop
        </Button>
      </div>
    </div>
  )
}

export function elapsed(t: TimeEntry): number {
  return t.running ? Math.max(0, (Date.now() - new Date(t.started_at).getTime()) / 1000) : t.seconds
}

export function TimerBar({ timer }: { timer: TimeEntry | null | undefined }) {
  const { start } = useTimer()
  const [note, setNote] = useState('')
  const [issue, setIssue] = useState<IssueRef | null>(null)

  if (timer) return <Running timer={timer} />

  const go = () =>
    start.mutate(
      { ...(issue ? { issue: issue.key } : {}), ...(note.trim() ? { note: note.trim() } : {}) },
      {
        onSuccess: () => {
          setNote('')
          setIssue(null)
        },
        onError: toastError,
      },
    )
  return (
    <form
      className="flex flex-col gap-2 rounded-xl border border-line bg-panel p-3 shadow-card sm:flex-row sm:items-center"
      onSubmit={(e) => {
        e.preventDefault()
        go()
      }}
    >
      <input
        value={note}
        onChange={(e) => setNote(e.target.value)}
        placeholder="What are you working on?"
        aria-label="What are you working on?"
        className="h-10 min-w-0 flex-1 rounded-lg bg-transparent px-2 text-[15px] outline-none placeholder:text-subtle"
      />
      <div className="flex items-center gap-2">
        <IssuePick
          value={issue}
          onChange={setIssue}
          className="min-w-0 flex-1 sm:max-w-64 sm:flex-none"
        />
        <Button
          type="submit"
          variant="primary"
          size="md"
          icon={<Play className="size-3.5 fill-current" />}
          loading={start.isPending}
          className="shrink-0"
        >
          Start
        </Button>
      </div>
    </form>
  )
}
