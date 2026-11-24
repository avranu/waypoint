import { useEffect, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import clsx from 'clsx'
import { api } from '@/lib/api'
import { duration, localISO } from '@/lib/format'
import type { IssueRef, TimeEntry } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { toast, toastError } from '@/components/ui/toast'
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

export function AddTimeDialog({
  open,
  onClose,
  defaultDate,
}: {
  open: boolean
  onClose: () => void
  defaultDate?: string
}) {
  const invalidate = useInvalidateTime()
  const [issue, setIssue] = useState<IssueRef | null>(null)
  const [date, setDate] = useState(localISO(new Date()))
  const [startTime, setStartTime] = useState('09:00')
  const [mode, setMode] = useState<'duration' | 'end'>('duration')
  const [dur, setDur] = useState('1h')
  const [endTime, setEndTime] = useState('10:00')
  const [note, setNote] = useState('')

  useEffect(() => {
    if (!open) return
    const now = new Date()
    const hourAgo = new Date(now.getTime() - 3600_000)
    hourAgo.setMinutes(Math.floor(hourAgo.getMinutes() / 15) * 15)
    setIssue(null)
    setDate(defaultDate ?? localISO(now))
    setStartTime(hhmm(hourAgo))
    setEndTime(hhmm(new Date(hourAgo.getTime() + 3600_000)))
    setMode('duration')
    setDur('1h')
    setNote('')
  }, [open, defaultDate])

  const start = date && startTime ? new Date(`${date}T${startTime}`) : null
  const durSecs = mode === 'duration' ? parseDuration(dur) : null
  let end: Date | null = null
  if (start && !isNaN(start.getTime())) {
    if (mode === 'duration' && durSecs) end = new Date(start.getTime() + durSecs * 1000)
    if (mode === 'end' && endTime) {
      end = new Date(`${date}T${endTime}`)
      if (end < start) end = addDays(end, 1)
    }
  }
  const total = start && end ? (end.getTime() - start.getTime()) / 1000 : 0
  const problem =
    !start || isNaN(start.getTime())
      ? 'Pick a date and start time.'
      : mode === 'duration' && durSecs === null
        ? 'Try 1h30m, 45m or 1:30.'
        : total < 60
          ? 'At least a minute, please.'
          : total > 24 * 3600
            ? 'At most 24 hours.'
            : null

  const save = useMutation({
    mutationFn: () => {
      const body: {
        issue?: string
        started_at: string
        duration?: string
        ended_at?: string
        note?: string
      } = { started_at: rfc3339(start!) }
      if (issue) body.issue = issue.key
      if (mode === 'duration') body.duration = dur.trim()
      else body.ended_at = rfc3339(end!)
      if (note.trim()) body.note = note.trim()
      return api.post<TimeEntry>('/time', body)
    },
    onSuccess: (t) => {
      invalidate()
      toast(`Added ${duration(t.seconds)}`)
      onClose()
    },
    onError: toastError,
  })

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Add time"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!!problem}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Add {problem ? '' : duration(total)}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (!problem) save.mutate()
        }}
      >
        <Field label="What did you work on?">
          <Input
            autoFocus
            value={note}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Notes (optional)"
          />
        </Field>
        <div className="space-y-1.5">
          <div className="text-[13px] font-medium">Issue</div>
          <IssuePick
            value={issue}
            onChange={setIssue}
            placeholder="No issue (optional)"
            className="w-full"
          />
        </div>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Date">
            <Input type="date" value={date} onChange={(e) => setDate(e.target.value)} required />
          </Field>
          <Field label="Start">
            <Input
              type="time"
              value={startTime}
              onChange={(e) => setStartTime(e.target.value)}
              required
            />
          </Field>
        </div>
        <div className="space-y-1.5">
          <div className="flex items-center gap-1 rounded-lg bg-sunken p-0.5 text-xs font-medium sm:w-fit">
            {(['duration', 'end'] as const).map((m) => (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={clsx(
                  'h-7 flex-1 whitespace-nowrap rounded-md px-3 transition-colors',
                  mode === m ? 'bg-panel text-fg shadow-card' : 'text-muted hover:text-fg',
                )}
              >
                {m === 'duration' ? 'Duration' : 'End time'}
              </button>
            ))}
          </div>
          {mode === 'duration' ? (
            <div className="flex items-center gap-3">
              <div className="w-28 shrink-0">
                <Input
                  value={dur}
                  onChange={(e) => setDur(e.target.value)}
                  placeholder="1h30m"
                  className="font-mono"
                  aria-label="Duration"
                />
              </div>
              <input
                type="range"
                min={300}
                max={8 * 3600}
                step={300}
                value={Math.min(Math.max(durSecs ?? 0, 300), 8 * 3600)}
                onChange={(e) => setDur(durationInput(Number(e.target.value)))}
                aria-label="Duration slider"
                className="h-9 min-w-0 flex-1 cursor-pointer accent-[var(--wp-accent)]"
              />
            </div>
          ) : (
            <Input
              type="time"
              value={endTime}
              onChange={(e) => setEndTime(e.target.value)}
              className="sm:w-40"
              aria-label="End time"
            />
          )}
          <div className={clsx('text-xs', problem ? 'text-danger' : 'text-subtle')}>
            {problem ??
              `${timeOfDay(start!)} – ${timeOfDay(end!)}${end && start && end.getDate() !== start.getDate() ? ' (next day)' : ''} · ${duration(total)}`}
          </div>
        </div>
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}
