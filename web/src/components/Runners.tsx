import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import { relativeTime } from '@/lib/format'
import { JOB_STATES, KIND_LABELS, type RunnerJob, type JobState } from '@/lib/runners'
import { Badge } from '@/components/ui/misc'

export function JobStateBadge({ state }: { state: JobState }) {
  const s = JOB_STATES[state]
  return <Badge color={s.color}>{s.label}</Badge>
}

/** A dot in a job's state color; the one for "running" pulses. */
export function JobDot({ state, className }: { state: JobState; className?: string }) {
  const color = JOB_STATES[state].color ?? 'var(--wp-subtle)'
  return (
    <span
      aria-hidden
      className={clsx(
        'inline-block size-2 shrink-0 rounded-full',
        state === 'running' && 'animate-pulse',
        className,
      )}
      style={{ background: color }}
    />
  )
}

/** The words that stand for a job in lists: what it does, or its kind when it's not ours to see. */
export const jobName = (j: Pick<RunnerJob, 'id' | 'title' | 'kind'>) =>
  j.title || `${KIND_LABELS[j.kind] ?? 'Job'} #${j.id}`

/** A job as one line that links to its page. */
export function JobLine({ job }: { job: RunnerJob }) {
  const at = job.finished_at ?? job.started_at ?? job.created_at
  return (
    <Link
      to="/runner-jobs/$id"
      params={{ id: String(job.id) }}
      className="-mx-1.5 flex min-w-0 items-center gap-2 rounded-md px-1.5 py-1 hover:bg-hover"
    >
      <JobDot state={job.state} />
      <span className="min-w-0 flex-1 truncate">{jobName(job)}</span>
      <span className="shrink-0 text-subtle">
        {JOB_STATES[job.state].label.toLowerCase()} · {relativeTime(at)}
      </span>
    </Link>
  )
}
