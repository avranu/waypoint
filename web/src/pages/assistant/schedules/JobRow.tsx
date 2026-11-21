// One schedule in the list, and (opened) what it asks, where it sends and
// what its runs did.

import { useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  CircleSlash,
  MoreHorizontal,
  Pause,
  Pencil,
  Play,
  Trash2,
  XCircle,
} from 'lucide-react'
import { hermes, iso, messagesQuery, type ContentPart } from '@/lib/assistant'
import { clock, dateTime, duration, plural, relativeTime } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { Markdown } from '@/components/Markdown'
import { jobPath, putJob, runNow, runsQuery, type CronJob, type CronRun } from './api'
import { storedLabel, zoneNote, type Zone } from './schedule'

// ---- state ----

export type JobState = 'running' | 'scheduled' | 'paused' | 'completed' | 'error'

export function jobState(j: CronJob, running: boolean): JobState {
  if (running || ['claimed', 'running'].includes(j.latest_execution?.status ?? '')) return 'running'
  if (j.state === 'paused' || (!j.enabled && j.state !== 'completed' && j.state !== 'error'))
    return 'paused'
  if (j.state === 'completed' || j.state === 'error') return j.state
  return 'scheduled'
}

const STATE_COLOR: Record<JobState, string> = {
  running: 'var(--wp-accent)',
  scheduled: 'var(--wp-ok)',
  paused: 'var(--wp-warn)',
  completed: 'var(--wp-subtle)',
  error: 'var(--wp-danger)',
}

export const STATE_LABEL: Record<JobState, string> = {
  running: 'Running',
  scheduled: 'Scheduled',
  paused: 'Paused',
  completed: 'Done',
  error: 'Stopped by an error',
}

function StateDot({ state }: { state: JobState }) {
  return (
    <span
      title={STATE_LABEL[state]}
      className={clsx(
        'mt-1.5 size-2.5 shrink-0 rounded-full',
        state === 'running' && 'animate-pulse',
      )}
      style={{ background: STATE_COLOR[state] }}
    >
      <span className="sr-only">{STATE_LABEL[state]}</span>
    </span>
  )
}

/** The job's last run as a word and a colour; detail is why it went wrong. */
function lastResult(
  j: CronJob,
): { tone: 'ok' | 'warn' | 'danger'; word: string; detail: string } | null {
  if (!j.last_status) return null
  if (j.last_status === 'ok') return { tone: 'ok', word: 'OK', detail: '' }
  if (j.last_status === 'delivery_failed')
    return {
      tone: 'warn',
      word: 'Not delivered',
      detail: j.last_delivery_error || j.last_error || '',
    }
  if (j.last_status === 'blocked_config')
    return { tone: 'warn', word: 'Blocked', detail: j.last_error || '' }
  return { tone: 'danger', word: 'Failed', detail: j.last_error || j.last_delivery_error || '' }
}

const TONE: Record<string, string> = { ok: 'text-ok', warn: 'text-warn', danger: 'text-danger' }

/** Where its result goes, by the delivery targets' names. */
export function deliverLabel(deliver: string, names: Map<string, string>): string {
  return (deliver || 'local')
    .split(',')
    .map((d) => d.trim())
    .map((d) =>
      d === 'local' ? 'Saved only' : d === 'origin' ? 'Home channel' : (names.get(d) ?? d),
    )
    .join(', ')
}

// ---- the row ----

export function JobRow({
  assistantId,
  job: j,
  zone,
  running,
  open,
  onToggle,
  onEdit,
  onDelete,
  deliverNames,
}: {
  assistantId: number
  job: CronJob
  zone: Zone
  /** started with Run now here at this time (ms) */
  running?: number
  open: boolean
  onToggle: () => void
  onEdit: () => void
  onDelete: () => void
  deliverNames: Map<string, string>
}) {
  useTick(running ? 1000 : 30_000)
  const actions = useJobActions(assistantId, j)
  const state = jobState(j, !!running)
  const last = lastResult(j)
  const pop = usePopover()
  const overdue =
    state === 'scheduled' &&
    j.next_run_at &&
    Date.now() - new Date(j.next_run_at).getTime() > 15 * 60_000
  const next =
    state === 'running' ? (
      <span className="inline-flex items-center gap-1.5 text-accent">
        <Spinner className="size-3" />
        Running{running ? ` · ${clock((Date.now() - running) / 1000)}` : '…'}
      </span>
    ) : state === 'paused' ? (
      <span className="text-warn">Paused</span>
    ) : state === 'completed' ? (
      <span className="text-subtle">Done</span>
    ) : state === 'error' ? (
      <span className="text-danger">Stopped</span>
    ) : j.next_run_at ? (
      <span className={clsx(overdue && 'text-warn')} title={dateTime(j.next_run_at)}>
        {overdue ? `Overdue since ${relativeTime(j.next_run_at)}` : relativeTime(j.next_run_at)}
      </span>
    ) : (
      <span className="text-subtle">—</span>
    )
  const lastCell = j.last_run_at ? (
    <span title={dateTime(j.last_run_at)} className="inline-flex min-w-0 items-center gap-1">
      {last && <span className={clsx('shrink-0 font-medium', TONE[last.tone])}>{last.word}</span>}
      <span className="truncate">{relativeTime(j.last_run_at)}</span>
      {j.failure_streak > 1 && (
        <span className="shrink-0 text-danger">· {j.failure_streak}× in a row</span>
      )}
    </span>
  ) : (
    <span className="text-subtle">Never</span>
  )
  const menu = [
    { label: 'Edit', icon: <Pencil />, onSelect: onEdit },
    state === 'paused'
      ? { label: 'Resume', icon: <Play />, onSelect: actions.resume }
      : {
          label: 'Pause',
          icon: <Pause />,
          onSelect: actions.pause,
          disabled: state === 'completed' || state === 'error',
        },
    { label: 'Delete', icon: <Trash2 />, onSelect: onDelete, danger: true },
  ]
  return (
    <li className={clsx(open && 'bg-sunken/40')}>
      <div className="flex items-center gap-1 pr-2 sm:pr-3">
        <button
          onClick={onToggle}
          aria-expanded={open}
          className="flex min-w-0 flex-1 items-start gap-3 py-3 pl-4 text-left outline-none focus-visible:bg-hover"
        >
          <StateDot state={state} />
          <div className="min-w-0 flex-1">
            <div className="truncate text-[13px] font-medium">{j.name}</div>
            <div className="truncate text-xs text-muted">
              {storedLabel(j.schedule, j.schedule_display, zone)}
              <span className="text-subtle"> · {deliverLabel(j.deliver, deliverNames)}</span>
            </div>
            <div className="mt-0.5 flex min-w-0 gap-x-2 text-xs text-muted md:hidden">
              <span className="shrink-0">{next}</span>
              {j.last_run_at && <span className="text-subtle">·</span>}
              {j.last_run_at && <span className="min-w-0 truncate">Last {lastCell}</span>}
            </div>
          </div>
          <div className="hidden w-40 shrink-0 pt-px text-xs md:block">
            <div className="text-[11px] text-subtle">Next</div>
            {next}
          </div>
          <div className="hidden w-44 shrink-0 pt-px text-xs md:block">
            <div className="text-[11px] text-subtle">Last run</div>
            <div className="truncate">{lastCell}</div>
          </div>
        </button>
        <IconButton
          label={state === 'running' ? 'Running' : 'Run now'}
          size="md"
          disabled={state === 'running'}
          icon={state === 'running' ? <Spinner className="size-4" /> : <Play className="size-4" />}
          onClick={actions.run}
        />
        <IconButton
          ref={pop.anchor}
          label="Schedule actions"
          size="md"
          icon={<MoreHorizontal className="size-4" />}
          onClick={pop.toggle}
        />
        <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
          <MenuList items={menu} onClose={pop.close} />
        </Popover>
      </div>
      {open && (
        <JobDetail
          assistantId={assistantId}
          job={j}
          state={state}
          last={last}
          deliverNames={deliverNames}
          zone={zone}
          actions={actions}
          onEdit={onEdit}
          onDelete={onDelete}
        />
      )}
    </li>
  )
}

function useJobActions(assistantId: number, j: CronJob) {
  const qc = useQueryClient()
  const post = (what: 'pause' | 'resume') => () =>
    hermes<CronJob>(assistantId, 'POST', `${jobPath(j.id)}/${what}`).then((after) => {
      putJob(qc, assistantId, after)
      toast(what === 'pause' ? `Paused “${j.name}”` : `Resumed “${j.name}”`)
    }, toastError)
  return {
    pause: post('pause'),
    resume: post('resume'),
    run: () => {
      runNow(qc, assistantId, j).then((after) => {
        const ok = after.last_status === 'ok'
        toast(
          ok
            ? `“${j.name}” ran`
            : `“${j.name}” ${after.last_status === 'delivery_failed' ? 'ran, but its result was not delivered' : 'failed'}`,
          { kind: ok ? 'success' : 'error' },
        )
      }, toastError)
    },
  }
}

// ---- opened ----

function JobDetail({
  assistantId,
  job: j,
  state,
  last,
  deliverNames,
  zone,
  actions,
  onEdit,
  onDelete,
}: {
  assistantId: number
  job: CronJob
  state: JobState
  last: ReturnType<typeof lastResult>
  deliverNames: Map<string, string>
  zone: Zone
  actions: ReturnType<typeof useJobActions>
  onEdit: () => void
  onDelete: () => void
}) {
  const expr = j.schedule?.expr || (j.schedule?.kind === 'interval' ? '' : j.schedule_display)
  const label = storedLabel(j.schedule, j.schedule_display, zone)
  return (
    <div className="space-y-4 border-t border-line/60 px-4 pb-4 pt-3 sm:pl-[2.375rem]">
      {state === 'error' || (last && last.tone !== 'ok' && last.detail) ? (
        <div
          className={clsx(
            'flex gap-2 rounded-lg px-3 py-2 text-[13px]',
            last?.tone === 'warn' && state !== 'error'
              ? 'bg-warn/10 text-warn'
              : 'bg-danger/10 text-danger',
          )}
        >
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />
          <div className="min-w-0">
            <div className="font-medium">
              {last?.word ?? 'Failed'}
              {j.failure_streak > 1 && ` ${j.failure_streak} times in a row`}
            </div>
            {(last?.detail || j.last_failure?.detail) && (
              <div className="whitespace-pre-wrap break-words text-fg/80">
                {last?.detail || j.last_failure?.detail}
              </div>
            )}
          </div>
        </div>
      ) : null}
      {j.prompt && (
        <div className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken px-3 py-2 text-[13px]">
          {j.prompt}
        </div>
      )}
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[13px] sm:grid-cols-[auto_1fr_auto_1fr]">
        <Meta label="When">
          {label}
          {zoneNote(zone) && j.schedule?.kind !== 'interval' && (
            <span className="text-subtle"> · {zoneNote(zone)} time</span>
          )}
          {expr && expr !== label && (
            <span className="ml-1.5 font-mono text-xs text-subtle">{expr}</span>
          )}
        </Meta>
        <Meta label="Sends to">{deliverLabel(j.deliver, deliverNames)}</Meta>
        {j.skills.length > 0 && <Meta label="Skills">{j.skills.join(', ')}</Meta>}
        {j.enabled_toolsets && <Meta label="Tools">{j.enabled_toolsets.join(', ') || 'None'}</Meta>}
        {j.model && (
          <Meta label="Model">
            <span className="font-mono text-xs">{j.model}</span>
          </Meta>
        )}
        <Meta label="Runs">
          {j.repeat?.times
            ? `${j.repeat.completed} of ${j.repeat.times}`
            : plural(j.repeat?.completed ?? 0, 'run')}
        </Meta>
        {state === 'paused' && j.paused_at && (
          <Meta label="Paused">
            {relativeTime(j.paused_at)}
            {j.paused_reason && ` · ${j.paused_reason}`}
          </Meta>
        )}
        <Meta label="Created">{relativeTime(j.created_at)}</Meta>
      </dl>
      <div className="flex flex-wrap gap-1.5">
        <Button
          size="sm"
          icon={<Pencil className="size-3.5" />}
          onClick={onEdit}
          className="max-sm:h-9"
        >
          Edit
        </Button>
        {state === 'paused' ? (
          <Button
            size="sm"
            icon={<Play className="size-3.5" />}
            onClick={actions.resume}
            className="max-sm:h-9"
          >
            Resume
          </Button>
        ) : (
          <Button
            size="sm"
            icon={<Pause className="size-3.5" />}
            onClick={actions.pause}
            disabled={state === 'completed' || state === 'error' || state === 'running'}
            className="max-sm:h-9"
          >
            Pause
          </Button>
        )}
        <Button
          size="sm"
          variant="ghost"
          icon={<Trash2 className="size-3.5" />}
          onClick={onDelete}
          className="text-danger max-sm:h-9"
        >
          Delete
        </Button>
      </div>
      <RunHistory assistantId={assistantId} job={j} />
    </div>
  )
}

function Meta({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  )
}

// ---- runs ----

function runState(r: CronRun): 'running' | 'ok' | 'warn' | 'error' | 'stopped' {
  if (r.source === 'cron_output') {
    const t = (r.title ?? '').toUpperCase()
    return t.startsWith('COMPLETED') || t.startsWith('OK') ? 'ok' : t ? 'error' : 'ok'
  }
  if (r.ended_at == null) return r.is_active || r.scheduler_owned ? 'running' : 'stopped'
  if (r.end_reason === 'cron_complete') return 'ok'
  if (r.end_reason === 'cron_incomplete_no_output') return 'warn'
  return r.end_reason?.includes('error') || r.end_reason?.includes('fail') ? 'error' : 'ok'
}

const RUN_ICON: Record<ReturnType<typeof runState>, ReactNode> = {
  running: <Spinner className="size-4 text-accent" />,
  ok: <CheckCircle2 className="size-4 text-ok" />,
  warn: <AlertTriangle className="size-4 text-warn" />,
  error: <XCircle className="size-4 text-danger" />,
  stopped: <CircleSlash className="size-4 text-subtle" />,
}

const RUN_LABEL: Record<ReturnType<typeof runState>, string> = {
  running: 'Running',
  ok: 'Finished',
  warn: 'Finished without an answer',
  error: 'Failed',
  stopped: 'Stopped before it finished',
}

/** Hermes's own wrapper around a cron prompt says nothing about the run. */
const usefulPreview = (p: string | null) => (p && !p.startsWith('[IMPORTANT:') ? p : '')

/** A script run's output doc opens with a header (# Cron Job, Job ID, Run Time); its status line says what happened. */
function docPreview(r: CronRun): string {
  const p = r.preview || r.title?.replace(/^\w+ · /, '') || ''
  const status = /\*\*Status:\*\*\s*(.*)$/s.exec(p)?.[1]
  return (status ?? p).replace(/[*#`]/g, '').replace(/\s+/g, ' ').trim()
}

function RunHistory({ assistantId, job }: { assistantId: number; job: CronJob }) {
  const runs = useQuery({ ...runsQuery(assistantId, job.id), refetchInterval: 30_000 })
  const list = runs.data?.runs ?? []
  return (
    <div>
      <h4 className="mb-1 text-xs font-medium text-muted">Recent runs</h4>
      {runs.isPending ? (
        <div className="space-y-1.5">
          <Skeleton className="h-9" />
          <Skeleton className="h-9" />
        </div>
      ) : runs.isError ? (
        <p className="text-[13px] text-danger">Couldn't load its runs: {runs.error.message}</p>
      ) : list.length === 0 ? (
        <p className="text-[13px] text-subtle">It hasn't run yet.</p>
      ) : (
        <ul className="divide-y divide-line/60 rounded-lg border border-line bg-panel">
          {list.map((r) => (
            <RunRow key={r.id} assistantId={assistantId} run={r} />
          ))}
        </ul>
      )}
    </div>
  )
}

function RunRow({ assistantId, run: r }: { assistantId: number; run: CronRun }) {
  const [open, setOpen] = useState(false)
  const state = runState(r)
  const session = r.source !== 'cron_output'
  const took = r.ended_at
    ? r.ended_at - r.started_at
    : state === 'running'
      ? Date.now() / 1000 - r.started_at
      : null
  const tokens = (r.input_tokens ?? 0) + (r.output_tokens ?? 0)
  const facts = [
    took != null && took >= 0 ? duration(took) : null,
    tokens ? `${tokens.toLocaleString()} tokens` : null,
    r.tool_call_count ? plural(r.tool_call_count, 'tool call') : null,
    r.model,
  ].filter(Boolean)
  const preview = session ? usefulPreview(r.preview) : docPreview(r)
  const when = iso(r.started_at)!
  const body = (
    <>
      <span className="mt-0.5 shrink-0" title={RUN_LABEL[state]}>
        {RUN_ICON[state]}
        <span className="sr-only">{RUN_LABEL[state]}</span>
      </span>
      <span className="min-w-0 flex-1">
        <span className="flex items-baseline gap-2 text-[13px]">
          <span className="shrink-0">{dateTime(when)}</span>
          <span className="truncate text-xs text-subtle">{relativeTime(when)}</span>
        </span>
        {(facts.length > 0 || preview) && (
          <span className="block truncate text-xs text-muted">
            {facts.join(' · ')}
            {preview && facts.length > 0 && ' · '}
            {preview}
          </span>
        )}
      </span>
    </>
  )
  return (
    <li>
      <div className="flex items-center gap-1 pr-1.5">
        {session ? (
          <Link
            to="/assistant/$id"
            params={{ id: String(assistantId) }}
            search={{ s: r.id }}
            title="Open in Chat"
            className="flex min-w-0 flex-1 items-start gap-2.5 rounded-lg px-3 py-2 hover:bg-hover"
          >
            {body}
          </Link>
        ) : (
          <div className="flex min-w-0 flex-1 items-start gap-2.5 px-3 py-2">{body}</div>
        )}
        {session && (
          <IconButton
            label={open ? 'Hide its answer' : 'Show its answer'}
            size="md"
            aria-expanded={open}
            icon={
              <ChevronDown className={clsx('size-4 transition-transform', open && 'rotate-180')} />
            }
            onClick={() => setOpen((o) => !o)}
          />
        )}
      </div>
      {open && <RunAnswer assistantId={assistantId} session={r.id} />}
    </li>
  )
}

const textOf = (c: string | ContentPart[] | null | undefined) =>
  typeof c === 'string' ? c : (c ?? []).map((p) => p.text ?? '').join('')

/** The run's final answer, which is what got delivered. */
function RunAnswer({ assistantId, session }: { assistantId: number; session: string }) {
  const m = useQuery(messagesQuery(assistantId, session))
  if (m.isPending) return <Skeleton className="mx-3 mb-3 h-16" />
  if (m.isError)
    return <p className="px-3 pb-3 text-[13px] text-danger">Couldn't load it: {m.error.message}</p>
  const answer = [...m.data.messages]
    .reverse()
    .find((x) => x.role === 'assistant' && textOf(x.display_content ?? x.content).trim())
  return (
    <div className="mx-3 mb-3 max-h-80 overflow-auto rounded-lg bg-sunken px-3 py-2">
      {answer ? (
        <Markdown text={textOf(answer.display_content ?? answer.content)} className="text-[13px]" />
      ) : (
        <p className="text-[13px] text-subtle">No answer.</p>
      )}
    </div>
  )
}
