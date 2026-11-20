// What an automation did: its runs and each run's steps, running one by
// hand (with the inputs it asks for), and a test that walks the steps
// without changing anything.

import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowLeft,
  Ban,
  Check,
  ChevronDown,
  ChevronRight,
  CircleDashed,
  Clock,
  FlaskConical,
  Loader,
  Minus,
  Play,
  RotateCcw,
  TriangleAlert,
  X,
} from 'lucide-react'
import { api } from '@/lib/api'
import {
  automationRunQuery,
  automationRunsQuery,
  catalogQuery,
  runActive,
  type AutomationRun,
  type RunStep,
  type TestResult,
} from '@/lib/automations'
import { relativeTime, dateTime } from '@/lib/format'
import type { Automation, Rule, RuleInput } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { DescribeRule } from './rules'

// ---- small pieces ----

const STATUS: Record<string, { label: string; color?: string }> = {
  queued: { label: 'Queued', color: 'var(--wp-accent)' },
  running: { label: 'Running', color: 'var(--wp-accent)' },
  waiting: { label: 'Waiting', color: 'var(--wp-warn)' },
  succeeded: { label: 'Succeeded', color: 'var(--wp-ok)' },
  failed: { label: 'Failed', color: 'var(--wp-danger)' },
  cancelled: { label: 'Cancelled' },
  skipped: { label: 'Skipped' },
  would_run: { label: 'Would run', color: 'var(--wp-ok)' },
}

export function RunBadge({ status }: { status: string }) {
  const s = STATUS[status] ?? { label: status }
  return <Badge color={s.color}>{s.label}</Badge>
}

function StatusIcon({ status }: { status: string }) {
  const cls = 'size-4 shrink-0'
  switch (status) {
    case 'succeeded':
    case 'would_run':
      return <Check className={clsx(cls, 'text-ok')} />
    case 'failed':
      return <X className={clsx(cls, 'text-danger')} />
    case 'running':
      return <Loader className={clsx(cls, 'animate-spin text-accent')} />
    case 'waiting':
    case 'queued':
      return <Clock className={clsx(cls, 'text-warn')} />
    case 'cancelled':
      return <Ban className={clsx(cls, 'text-subtle')} />
    case 'skipped':
      return <Minus className={clsx(cls, 'text-subtle')} />
  }
  return <CircleDashed className={clsx(cls, 'text-subtle')} />
}

export function msText(ms: number | undefined | null): string {
  if (ms === undefined || ms === null) return ''
  if (ms < 1000) return `${ms} ms`
  const s = ms / 1000
  if (s < 60) return `${s < 10 ? s.toFixed(1) : Math.round(s)} s`
  const m = Math.floor(s / 60)
  const rest = Math.round(s % 60)
  if (m < 60) return rest ? `${m}m ${rest}s` : `${m}m`
  const h = Math.floor(m / 60)
  return m % 60 ? `${h}h ${m % 60}m` : `${h}h`
}

const VIA: Record<string, string> = {
  event: 'an event',
  schedule: 'the schedule',
  manual: 'Run now',
  api: 'the API',
  cli: 'the CLI',
  webhook: 'the webhook',
  rerun: 'Run again',
  test: 'a test',
}

/** A value from a step's inputs or outputs: long text is clamped. */
function ValueView({ value }: { value: unknown }) {
  const [all, setAll] = useState(false)
  const text =
    typeof value === 'string'
      ? value
      : value === null || value === undefined
        ? ''
        : typeof value === 'object'
          ? JSON.stringify(value, null, 2)
          : String(value)
  const long = text.length > 280 || text.split('\n').length > 5
  const mono = typeof value === 'object' && value !== null
  return (
    <div className="min-w-0">
      <pre
        className={clsx(
          'whitespace-pre-wrap break-words font-sans text-xs leading-relaxed text-fg',
          mono && 'font-mono',
          long && !all && 'line-clamp-5',
        )}
      >
        {text === '' ? <span className="text-subtle">empty</span> : text}
      </pre>
      {long && (
        <button
          type="button"
          onClick={() => setAll((a) => !a)}
          className="mt-0.5 text-xs text-accent hover:underline"
        >
          {all ? 'Show less' : 'Show all'}
        </button>
      )}
    </div>
  )
}

function KV({ title, data }: { title: string; data?: Record<string, unknown> }) {
  const entries = Object.entries(data ?? {})
  if (entries.length === 0) return null
  return (
    <div>
      <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
        {title}
      </div>
      <dl className="space-y-1.5">
        {entries.map(([k, v]) => (
          <div key={k} className="grid gap-x-3 sm:grid-cols-[8rem_1fr]">
            <dt className="truncate font-mono text-xs text-muted" title={k}>
              {k}
            </dt>
            <dd className="min-w-0">
              <ValueView value={v} />
            </dd>
          </div>
        ))}
      </dl>
    </div>
  )
}

// ---- steps ----

/** The steps inside repeats, by name, so their rounds can be told apart. */
function nestedIDs(rule?: Rule): Set<string> {
  const out = new Set<string>()
  rule?.actions.forEach((a, i) => {
    if (a.type !== 'repeat') return
    const pid = a.id || `s${i + 1}`
    a.steps?.forEach((k, j) => out.add(k.id || `${pid}.${j + 1}`))
  })
  return out
}

export function StepsView({
  steps,
  rule,
  test,
}: {
  steps: RunStep[]
  rule?: Rule
  test?: boolean
}) {
  const nested = useMemo(() => nestedIDs(rule), [rule])
  let round = -1
  return (
    <ol className="space-y-2">
      {steps.map((s) => {
        const inner = nested.has(s.step_id)
        const heading = inner && s.iteration !== round
        round = inner ? s.iteration : -1
        return (
          <li key={`${s.step_id}-${s.iteration}`} className={clsx(inner && 'ml-3 sm:ml-5')}>
            {heading && (
              <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                Round {s.iteration}
              </div>
            )}
            <StepCard step={s} test={test} />
          </li>
        )
      })}
    </ol>
  )
}

/** What a parked step is waiting for, from its settings. */
function waitingFor(s: RunStep): string {
  const i = s.inputs ?? {}
  if (s.type === 'delay' && i.duration) return `Waiting for ${String(i.duration)} to pass`
  if (s.type === 'wait_for' && i.event)
    return `Waiting for ${String(i.event)}${i.key ? ` (${String(i.key)})` : ''}`
  if (s.type === 'tell_agent') return 'Waiting for the agent to take it and finish'
  if (s.type === 'start_session') return 'Waiting for the new agent to finish'
  if (s.type === 'run_automation') return 'Waiting for the run to finish'
  return 'Waiting'
}

function StepCard({ step: s, test }: { step: RunStep; test?: boolean }) {
  const cat = useQuery(catalogQuery()).data
  const def = cat?.actions.find((d) => d.name === s.type)
  const hasMore = !!(
    Object.keys(s.inputs ?? {}).length ||
    Object.keys(s.outputs ?? {}).length ||
    s.log ||
    s.error
  )
  const [open, setOpen] = useState(
    s.status === 'failed' || s.status === 'waiting' || s.status === 'running' || !!test,
  )
  const name = s.label || def?.label || s.type.replace(/_/g, ' ')
  const status =
    test && s.status === 'skipped'
      ? 'Skipped: its “only if” is false'
      : test && s.status === 'failed'
        ? 'Would fail'
        : s.status === 'waiting'
          ? waitingFor(s)
          : (STATUS[s.status]?.label ?? s.status)
  return (
    <div
      className={clsx(
        'rounded-lg border bg-panel',
        s.status === 'failed' ? 'border-danger/40' : 'border-line',
      )}
    >
      <button
        type="button"
        aria-expanded={open}
        disabled={!hasMore}
        onClick={() => setOpen((o) => !o)}
        className="flex w-full items-center gap-2 px-3 py-2 text-left disabled:cursor-default"
      >
        <StatusIcon status={s.status} />
        <span className="min-w-0 flex-1">
          <span className="flex flex-wrap items-baseline gap-x-2">
            <span className="text-[13px] font-medium">{name}</span>
            <span className="font-mono text-[11px] text-subtle">{s.step_id}</span>
          </span>
          <span className="block text-xs text-muted">
            {status}
            {s.attempts > 1 && ` · tried ${s.attempts} times`}
            {s.long && !test && s.status !== 'waiting' && ' · in the background'}
          </span>
        </span>
        {s.duration_ms !== undefined && (
          <span className="shrink-0 text-xs tabular-nums text-subtle">{msText(s.duration_ms)}</span>
        )}
        {hasMore &&
          (open ? (
            <ChevronDown className="size-4 shrink-0 text-subtle" />
          ) : (
            <ChevronRight className="size-4 shrink-0 text-subtle" />
          ))}
      </button>
      {open && hasMore && (
        <div className="space-y-3 border-t border-line px-3 py-2.5">
          {s.error && (
            <p className="rounded-md bg-danger/10 px-2.5 py-1.5 text-xs text-danger">{s.error}</p>
          )}
          {s.log && (
            <div>
              <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                {test ? 'What it would do' : s.status === 'waiting' ? 'Waiting for' : 'Log'}
              </div>
              <ValueView value={s.log} />
            </div>
          )}
          <KV title="Settings" data={s.inputs} />
          {!test && <KV title="Results" data={s.outputs} />}
        </div>
      )}
    </div>
  )
}

// ---- inputs form ----

type InputValues = Record<string, string | boolean>

function initialInputs(inputs: RuleInput[]): InputValues {
  const v: InputValues = {}
  for (const i of inputs) v[i.name] = i.type === 'bool' ? i.default === 'true' : (i.default ?? '')
  return v
}

/** What a run gets: empty text is left out so defaults apply. */
function inputPayload(inputs: RuleInput[], v: InputValues): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const i of inputs) {
    const x = v[i.name]
    if (i.type === 'bool') out[i.name] = !!x
    else if (typeof x === 'string' && x.trim() !== '')
      out[i.name] = i.type === 'number' ? Number(x) : x
  }
  return out
}

function missingInput(inputs: RuleInput[], v: InputValues): RuleInput | undefined {
  return inputs.find((i) => i.required && i.type !== 'bool' && !String(v[i.name] ?? '').trim())
}

export function InputsForm({
  inputs,
  values,
  onChange,
  tried,
}: {
  inputs: RuleInput[]
  values: InputValues
  onChange: (v: InputValues) => void
  tried?: boolean
}) {
  const set = (name: string, x: string | boolean) => onChange({ ...values, [name]: x })
  return (
    <div className="space-y-3">
      {inputs.map((i) => {
        const label = (
          <>
            {i.label || i.name}
            {i.required && <span className="text-danger"> *</span>}
          </>
        )
        const x = values[i.name]
        const empty = i.required && i.type !== 'bool' && !String(x ?? '').trim()
        const error = tried && empty ? 'This one is needed' : undefined
        if (i.type === 'bool')
          return (
            <div key={i.name} className="flex items-center justify-between gap-3">
              <span className="text-[13px] font-medium">{label}</span>
              <Switch checked={!!x} onChange={(v) => set(i.name, v)} label={i.label || i.name} />
            </div>
          )
        return (
          <Field key={i.name} label={label} error={error}>
            {i.type === 'select' ? (
              <Select value={String(x ?? '')} onChange={(e) => set(i.name, e.target.value)}>
                {!i.required && <option value="">—</option>}
                {(i.options ?? []).map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </Select>
            ) : (
              <Input
                type={i.type === 'number' ? 'number' : 'text'}
                value={String(x ?? '')}
                onChange={(e) => set(i.name, e.target.value)}
              />
            )}
          </Field>
        )
      })}
    </div>
  )
}

// ---- running by hand ----

/** Run now: with inputs it asks first; the run it starts opens in the Runs window. */
export function RunNowDialog({
  automation,
  open,
  onClose,
  onStarted,
}: {
  automation: Automation
  open: boolean
  onClose: () => void
  onStarted: (run: AutomationRun) => void
}) {
  const qc = useQueryClient()
  const inputs = automation.rule.inputs ?? []
  const [values, setValues] = useState<InputValues>({})
  const [tried, setTried] = useState(false)
  useEffect(() => {
    if (open) {
      setValues(initialInputs(inputs))
      setTried(false)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, automation.id])
  const run = useMutation({
    mutationFn: () =>
      api.post<AutomationRun>(`/automations/${automation.id}/run`, {
        inputs: inputPayload(inputs, values),
      }),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ['automations'] })
      qc.invalidateQueries({ queryKey: ['automationRuns'] })
      onClose()
      onStarted(r)
    },
    onError: toastError,
  })
  const submit = () => {
    setTried(true)
    if (!missingInput(inputs, values)) run.mutate()
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Run “${automation.name}”`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            icon={<Play className="size-3.5" />}
            loading={run.isPending}
            onClick={submit}
          >
            Run now
          </Button>
        </>
      }
    >
      <form
        className="space-y-3 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          This automation asks for a few things before it runs.
        </p>
        <InputsForm inputs={inputs} values={values} onChange={setValues} tried={tried} />
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}

// ---- a test ----

/** Walks the saved steps against a recent event (or sample data) and says what each would do. */
export function TestDialog({
  automation,
  open,
  onClose,
}: {
  automation: Automation
  open: boolean
  onClose: () => void
}) {
  const rule = automation.rule
  const inputs = rule.inputs ?? []
  const webhook = rule.trigger.event === 'webhook'
  const [values, setValues] = useState<InputValues>({})
  const [data, setData] = useState('{\n  \n}')
  const [dataError, setDataError] = useState('')
  const [result, setResult] = useState<TestResult | null>(null)
  useEffect(() => {
    if (open) {
      setValues(initialInputs(inputs))
      setResult(null)
      setDataError('')
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, automation.id])
  const test = useMutation({
    mutationFn: () => {
      let parsed: unknown = undefined
      if (webhook && data.trim()) {
        try {
          parsed = JSON.parse(data)
        } catch {
          throw new Error('The sample data isn’t valid JSON')
        }
      }
      return api.post<TestResult>(`/automations/${automation.id}/test`, {
        inputs: inputPayload(inputs, values),
        data: parsed,
      })
    },
    onSuccess: (r) => {
      setResult(r)
      setDataError('')
    },
    onError: (e) => {
      if (webhook && e instanceof Error && /JSON/.test(e.message)) setDataError(e.message)
      else toastError(e)
    },
  })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      wide
      title={`Test “${automation.name}”`}
      footer={
        <>
          <span className="mr-auto text-xs text-subtle">Nothing is changed or sent.</span>
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
          <Button
            variant="primary"
            icon={<FlaskConical className="size-3.5" />}
            loading={test.isPending}
            onClick={() => test.mutate()}
          >
            {result ? 'Test again' : 'Run the test'}
          </Button>
        </>
      }
    >
      <div className="space-y-4 p-4">
        <p className="text-[13px] text-muted">
          Walks the <b>saved</b> steps against a recent matching event and says what each would do.
          Save your edits first to test them.
        </p>
        {inputs.length > 0 && <InputsForm inputs={inputs} values={values} onChange={setValues} />}
        {webhook && (
          <Field
            label="Sample request body"
            hint="Stands in for what the webhook receives: JSON that becomes .Trigger.Data."
            error={dataError || undefined}
          >
            <Textarea
              rows={4}
              value={data}
              onChange={(e) => setData(e.target.value)}
              spellCheck={false}
              className="font-mono text-xs"
            />
          </Field>
        )}
        {result && (
          <div className="space-y-3" aria-live="polite">
            <div className="rounded-lg bg-sunken px-3 py-2 text-xs text-muted">
              {result.sample ? (
                <>No recent event matched, so sample data stood in.</>
              ) : (
                <>
                  Using a recent event: <b className="text-fg">{result.summary}</b>
                </>
              )}
              {result.note && <> {result.note}</>}
            </div>
            {result.error && (
              <p className="flex items-start gap-1.5 rounded-lg bg-danger/10 px-3 py-2 text-xs text-danger">
                <TriangleAlert className="mt-px size-3.5 shrink-0" />
                {result.error}
              </p>
            )}
            {result.steps && result.steps.length > 0 ? (
              <StepsView steps={result.steps} rule={rule} test />
            ) : (
              !result.error && <p className="text-[13px] text-muted">No step would run.</p>
            )}
          </div>
        )}
      </div>
    </Dialog>
  )
}

// ---- runs ----

function RunRow({ run, onOpen }: { run: AutomationRun; onOpen: () => void }) {
  const by = run.agent ? run.agent : run.actor?.display_name || run.actor?.username
  return (
    <button
      type="button"
      onClick={onOpen}
      className="flex w-full items-center gap-3 px-4 py-2.5 text-left hover:bg-hover"
    >
      <span className="w-[5.5rem] shrink-0">
        <RunBadge status={run.status} />
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-[13px]">{run.summary || run.trigger}</span>
        <span className="block truncate text-xs text-subtle">
          {VIA[run.via] ?? run.via}
          {by && run.via !== 'event' && run.via !== 'schedule' ? ` · ${by}` : ''}
          {' · '}
          {relativeTime(run.created_at)}
          {run.durable && ' · in the background'}
        </span>
      </span>
      <span className="hidden shrink-0 text-xs tabular-nums text-subtle sm:block">
        {msText(run.duration_ms)}
      </span>
      <ChevronRight className="size-4 shrink-0 text-subtle" />
    </button>
  )
}

/** An automation's runs; opens a run's steps. `initial` opens that run first. */
export function RunsDialog({
  automation,
  open,
  onClose,
  initial,
  canRun,
}: {
  automation: Automation
  open: boolean
  onClose: () => void
  initial?: number | null
  canRun: boolean
}) {
  const [selected, setSelected] = useState<number | null>(null)
  useEffect(() => {
    if (open) setSelected(initial ?? null)
  }, [open, initial])
  const runs = useInfiniteQuery({ ...automationRunsQuery(automation.id), enabled: open })
  const list = runs.data?.pages.flat() ?? []
  return (
    <Dialog
      open={open}
      onClose={selected ? () => setSelected(null) : onClose}
      wide
      title={
        selected ? (
          <span className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setSelected(null)}
              aria-label="Back to the runs"
              className="-ml-1 inline-flex size-7 items-center justify-center rounded-md text-muted hover:bg-hover hover:text-fg"
            >
              <ArrowLeft className="size-4" />
            </button>
            Run #{selected}
          </span>
        ) : (
          `Runs · ${automation.name}`
        )
      }
    >
      {selected ? (
        <RunDetail id={selected} canRun={canRun} onOpenRun={setSelected} />
      ) : runs.isLoading ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-12" />
          <Skeleton className="h-12" />
          <Skeleton className="h-12" />
        </div>
      ) : runs.isError ? (
        <Empty icon={<TriangleAlert />} title="Couldn’t load the runs">
          {(runs.error as Error).message}
        </Empty>
      ) : list.length === 0 ? (
        <Empty icon={<Clock />} title="No runs yet">
          Each time this automation fires, the run and its steps are kept here for 90 days.
        </Empty>
      ) : (
        <div>
          <div className="divide-y divide-line">
            {list.map((r) => (
              <RunRow key={r.id} run={r} onOpen={() => setSelected(r.id)} />
            ))}
          </div>
          {runs.hasNextPage && (
            <div className="border-t border-line p-3 text-center">
              <Button
                size="sm"
                loading={runs.isFetchingNextPage}
                onClick={() => runs.fetchNextPage()}
              >
                Load more
              </Button>
            </div>
          )}
        </div>
      )}
    </Dialog>
  )
}

function RunDetail({
  id,
  canRun,
  onOpenRun,
}: {
  id: number
  canRun: boolean
  onOpenRun: (id: number) => void
}) {
  const qc = useQueryClient()
  const q = useQuery(automationRunQuery(id, true))
  const run = q.data
  const done = () => {
    qc.invalidateQueries({ queryKey: ['automationRun'] })
    qc.invalidateQueries({ queryKey: ['automationRuns'] })
    qc.invalidateQueries({ queryKey: ['automations'] })
  }
  const cancel = useMutation({
    mutationFn: () => api.post<AutomationRun>(`/automation-runs/${id}/cancel`),
    onSuccess: () => {
      toast('Run cancelled')
      done()
    },
    onError: toastError,
  })
  const again = useMutation({
    mutationFn: () => api.post<AutomationRun>(`/automation-runs/${id}/rerun`),
    onSuccess: (r) => {
      done()
      onOpenRun(r.id)
    },
    onError: toastError,
  })
  if (q.isLoading)
    return (
      <div className="space-y-2 p-4">
        <Skeleton className="h-16" />
        <Skeleton className="h-16" />
      </div>
    )
  if (!run)
    return (
      <Empty icon={<TriangleAlert />} title="Couldn’t load this run">
        {(q.error as Error | null)?.message}
      </Empty>
    )
  const by = run.agent ? `the agent ${run.agent}` : run.actor?.display_name || run.actor?.username
  const active = runActive(run.status)
  return (
    <div className="space-y-4 p-4">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <RunBadge status={run.status} />
        <span className="min-w-0 flex-1 text-[13px]">{run.summary || run.trigger}</span>
        <span className="flex items-center gap-1.5">
          {active && canRun && (
            <Button
              size="sm"
              icon={<Ban className="size-3.5" />}
              loading={cancel.isPending}
              onClick={() => cancel.mutate()}
            >
              Cancel
            </Button>
          )}
          {!active && canRun && (
            <Button
              size="sm"
              icon={<RotateCcw className="size-3.5" />}
              loading={again.isPending}
              onClick={() => again.mutate()}
            >
              Run again
            </Button>
          )}
        </span>
      </div>
      <p className="text-xs text-muted">
        {VIA[run.via] ?? run.via}
        {by && ` · ${by}`}
        {run.started_at
          ? ` · started ${dateTime(run.started_at)}`
          : ` · queued ${dateTime(run.created_at)}`}
        {run.duration_ms !== undefined && ` · took ${msText(run.duration_ms)}`}
        {run.durable && ' · runs in the background'}
        {run.rerun_of && (
          <>
            {' · '}
            <button
              type="button"
              className="text-accent hover:underline"
              onClick={() => onOpenRun(run.rerun_of!)}
            >
              again of #{run.rerun_of}
            </button>
          </>
        )}
      </p>
      {run.error && (
        <p className="flex items-start gap-1.5 rounded-lg bg-danger/10 px-3 py-2 text-xs text-danger">
          <TriangleAlert className="mt-px size-3.5 shrink-0" />
          <span className="min-w-0 break-words">{run.error}</span>
        </p>
      )}
      {run.rule && (
        <div className="rounded-lg bg-sunken px-3 py-2">
          <div className="mb-0.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
            It ran as
          </div>
          <DescribeRule rule={run.rule} statuses={[]} />
        </div>
      )}
      <KV title="Inputs" data={run.inputs} />
      {run.event && (
        <details className="text-xs">
          <summary className="cursor-pointer text-muted hover:text-fg">
            The event ({run.event.kind})
          </summary>
          <pre className="mt-1 max-h-48 overflow-auto rounded-lg bg-sunken p-2 font-mono text-[11px]">
            {JSON.stringify(run.event, null, 2)}
          </pre>
        </details>
      )}
      <div>
        <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
          Steps
        </div>
        {run.steps && run.steps.length > 0 ? (
          <StepsView steps={run.steps} rule={run.rule} />
        ) : (
          <p className="text-[13px] text-muted">
            {run.status === 'skipped'
              ? 'Skipped: ' + (run.error || 'another run was in progress.')
              : active
                ? 'No step has started yet.'
                : 'No step ran.'}
          </p>
        )}
      </div>
    </div>
  )
}

/** The pieces a card needs: open the Runs window, run by hand. */
export function useRunNow(
  automation: Automation,
  openRun: (id: number) => void,
): {
  start: () => void
  dialog: ReactNode
  pending: boolean
} {
  const qc = useQueryClient()
  const [asking, setAsking] = useState(false)
  const run = useMutation({
    mutationFn: () => api.post<AutomationRun>(`/automations/${automation.id}/run`, {}),
    onSuccess: (r) => {
      qc.invalidateQueries({ queryKey: ['automations'] })
      qc.invalidateQueries({ queryKey: ['automationRuns'] })
      openRun(r.id)
    },
    onError: toastError,
  })
  return {
    start: () => ((automation.rule.inputs ?? []).length > 0 ? setAsking(true) : run.mutate()),
    pending: run.isPending,
    dialog: (
      <RunNowDialog
        automation={automation}
        open={asking}
        onClose={() => setAsking(false)}
        onStarted={(r) => openRun(r.id)}
      />
    ),
  }
}
