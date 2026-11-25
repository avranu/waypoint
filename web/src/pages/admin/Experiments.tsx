import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { CircleCheck, FlaskConical, List } from 'lucide-react'
import {
  experimentResultsQuery,
  experimentsQuery,
  q,
  useStartExperiment,
  useStopExperiment,
} from '@/lib/queries'
import type {
  Experiment,
  ExperimentArmResult,
  ExperimentMetric,
  ExperimentResults,
  ExperimentRun,
} from '@/lib/experiments'
import { relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toastError } from '@/components/ui/toast'

const day = (iso: string) => iso.slice(0, 10)

export function ExperimentsTab() {
  const ex = useQuery(experimentsQuery())
  const d = ex.data
  return (
    <div className="space-y-4" data-testid="experiments">
      <Section
        title="Experiments"
        help={
          <>
            <p>
              An experiment gives each task an agent takes one of its arms, at random and kept, and
              compares the tasks of each arm: cost, tokens, turns, tool calls, compactions, review
              findings. The first arm is today&apos;s behaviour; every task gets it while no run is
              live. A run&apos;s split is fixed, so changing it means a new run. Runs are short:
              each ends by itself at its end date, once every arm has its target of finished tasks,
              or when an arm costs half again as much or finishes markedly fewer tasks.
            </p>
            {d && d.caveats.length > 0 && (
              <>
                <p className="mt-2 font-medium">Before acting on a result</p>
                <ul className="mt-1 list-disc space-y-1 pl-5">
                  {d.caveats.map((c) => (
                    <li key={c}>{c}</li>
                  ))}
                </ul>
              </>
            )}
          </>
        }
      >
        <p className="px-4 py-3 text-xs text-muted">
          Only agents&apos; tasks are assigned; people and older wp builds always get the control.
          Starting and stopping need a person who is an admin. <code>wp experiment</code> does the
          same from a shell.
        </p>
      </Section>
      {ex.isPending ? (
        <Skeleton className="h-48" />
      ) : !d ? (
        <Empty icon={<FlaskConical />} title="Experiments couldn't be read">
          Reload to try again.
        </Empty>
      ) : (
        d.experiments.map((x) => <ExperimentCard key={x.key} x={x} />)
      )}
    </div>
  )
}

function ExperimentCard({ x }: { x: Experiment }) {
  const [starting, setStarting] = useState(false)
  const [stopping, setStopping] = useState(false)
  const runs = [...(x.live ? [x.live] : []), ...(x.past ?? [])]
  const [shown, setShown] = useState<number | null>(null)
  const live = x.live
  return (
    <Section
      title={x.title}
      helpId={`experiment:${x.key}`}
      help={
        <dl className="space-y-1.5">
          <div>
            <dt className="font-medium">Where the arms differ</dt>
            <dd>{x.where}</dd>
          </div>
          <div>
            <dt className="font-medium">When a task counts as exposed</dt>
            <dd>{x.exposure}</dd>
          </div>
          {x.caveats.length > 0 && (
            <div>
              <dt className="font-medium">Caveats for this one</dt>
              <dd>
                <ul className="list-disc space-y-1 pl-5">
                  {x.caveats.map((c) => (
                    <li key={c}>{c}</li>
                  ))}
                </ul>
              </dd>
            </div>
          )}
        </dl>
      }
      actions={
        live ? (
          <Button size="xs" variant="secondary" onClick={() => setStopping(true)}>
            Stop
          </Button>
        ) : (
          <Button size="xs" variant="primary" onClick={() => setStarting(true)}>
            Start
          </Button>
        )
      }
    >
      <div className="space-y-2 px-4 py-3" data-experiment={x.key} data-live={live ? 'yes' : 'no'}>
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-[11px] text-subtle">{x.key}</span>
          {live ? <Badge color="var(--wp-ok)">Live</Badge> : <Badge>Off</Badge>}
        </div>
        <p className="text-[13px]">{x.question}</p>
        <div className="flex flex-wrap gap-1.5">
          {x.arms.map((a) => (
            <Badge key={a.key} color={a.control ? undefined : 'var(--wp-accent)'}>
              {a.label}
              {a.control && ' (control)'} · {(live ?? x).weights[a.key] ?? 0}
            </Badge>
          ))}
        </div>
        {live && <RunLine run={live} x={x} />}
        {runs.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 pt-1">
            <span className="text-xs text-subtle">Results:</span>
            {runs.map((r) => (
              <Button
                key={r.id}
                size="xs"
                variant={shown === r.id ? 'subtle' : 'ghost'}
                aria-pressed={shown === r.id}
                onClick={() => setShown(shown === r.id ? null : r.id)}
              >
                Run {r.id}
                {r.stopped_at ? ` · ${day(r.started_at)} to ${day(r.stopped_at)}` : ' (live)'}
              </Button>
            ))}
          </div>
        )}
      </div>
      {shown !== null && <ResultsView x={x} run={shown} />}
      <StartDialog x={x} open={starting} onClose={() => setStarting(false)} />
      <StopDialog x={x} open={stopping} onClose={() => setStopping(false)} />
    </Section>
  )
}

function RunLine({ run: r, x }: { run: ExperimentRun; x: Experiment }) {
  return (
    <div className="space-y-0.5 text-xs text-muted">
      <div>
        Run {r.id} since {relativeTime(r.started_at)}
        {r.started_by && ` by ${r.started_by}`}
        {r.ends_at && `, ends ${day(r.ends_at)}`}
        {r.target > 0 && `, stops at ${r.target} finished tasks per arm`}
        {r.projects.length > 0 && `, in ${r.projects.join(' ')}`}
      </div>
      <div>
        Tasks so far:{' '}
        {x.arms
          .map(
            (a) =>
              `${a.label} ${r.assigned[a.key] ?? 0} (${r.exposed[a.key] ?? 0} reached the difference)`,
          )
          .join(' · ')}
      </div>
      {Object.keys(r.params).length > 0 && (
        <div className="break-words">
          {Object.entries(r.params).map(([k, v]) => (
            <span key={k}>
              {k}: <span className="font-mono">{v}</span>{' '}
            </span>
          ))}
        </div>
      )}
      {r.note && <div className="italic">{r.note}</div>}
    </div>
  )
}

function StartDialog({ x, open, onClose }: { x: Experiment; open: boolean; onClose: () => void }) {
  const start = useStartExperiment()
  const projects = useQuery({ ...q.projects(), enabled: open }).data ?? []
  const [weights, setWeights] = useState<Record<string, string>>({})
  const [params, setParams] = useState<Record<string, string>>({})
  const [days, setDays] = useState('14')
  const [target, setTarget] = useState('40')
  const [scope, setScope] = useState<string[]>([])
  const [note, setNote] = useState('')
  const w = (k: string) => weights[k] ?? String(x.weights[k] ?? 0)
  const submit = () =>
    start.mutate(
      {
        key: x.key,
        input: {
          weights: Object.fromEntries(x.arms.map((a) => [a.key, Number(w(a.key))])),
          params: Object.fromEntries(x.params.map((p) => [p.key, params[p.key] ?? p.default])),
          days: Number(days),
          target: Number(target),
          projects: scope,
          note,
        },
      },
      { onSuccess: onClose, onError: toastError },
    )
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Start: ${x.title}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={start.isPending} onClick={submit}>
            Start
          </Button>
        </>
      }
    >
      <form
        className="space-y-3"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <div className="grid grid-cols-2 gap-3">
          {x.arms.map((a) => (
            <Field key={a.key} label={`${a.label}${a.control ? ' (control)' : ''}`}>
              <Input
                type="number"
                min={0}
                max={100}
                value={w(a.key)}
                onChange={(e) => setWeights({ ...weights, [a.key]: e.target.value })}
              />
            </Field>
          ))}
        </div>
        <p className="text-xs text-subtle">
          Weights split tasks between arms: 1 and 1 is half each. Unequal splits need more tasks to
          tell a difference.
        </p>
        {x.params.map((p) => (
          <Field key={p.key} label={p.label}>
            <Textarea
              rows={2}
              value={params[p.key] ?? p.default}
              onChange={(e) => setParams({ ...params, [p.key]: e.target.value })}
            />
          </Field>
        ))}
        <div className="grid grid-cols-2 gap-3">
          <Field label="Days" note="1 to 90; it ends by itself then">
            <Input
              type="number"
              min={1}
              max={90}
              value={days}
              onChange={(e) => setDays(e.target.value)}
            />
          </Field>
          <Field label="Target" note="finished tasks per arm to stop at; 0 = none">
            <Input
              type="number"
              min={0}
              value={target}
              onChange={(e) => setTarget(e.target.value)}
            />
          </Field>
        </div>
        {projects.length > 0 && (
          <fieldset>
            <legend className="text-[13px] font-medium">Projects (none ticked = all)</legend>
            <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1">
              {projects.map((p) => (
                <label key={p.key} className="flex items-center gap-1.5 text-xs">
                  <input
                    type="checkbox"
                    checked={scope.includes(p.key)}
                    onChange={(e) =>
                      setScope(
                        e.target.checked ? [...scope, p.key] : scope.filter((k) => k !== p.key),
                      )
                    }
                  />
                  {p.key}
                </label>
              ))}
            </div>
          </fieldset>
        )}
        <Field label="Note" note="why, for whoever reads the results">
          <Input value={note} onChange={(e) => setNote(e.target.value)} />
        </Field>
      </form>
    </Dialog>
  )
}

function StopDialog({ x, open, onClose }: { x: Experiment; open: boolean; onClose: () => void }) {
  const stop = useStopExperiment()
  const [reason, setReason] = useState('')
  const submit = () =>
    stop.mutate({ key: x.key, reason }, { onSuccess: onClose, onError: toastError })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Stop: ${x.title}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="danger" loading={stop.isPending} onClick={submit}>
            Stop
          </Button>
        </>
      }
    >
      <p className="mb-3 text-[13px] text-muted">
        Every task gets the control again. The run&apos;s results stay readable.
      </p>
      <Field label="Why">
        <Input value={reason} onChange={(e) => setReason(e.target.value)} />
      </Field>
    </Dialog>
  )
}

const KIND: Record<ExperimentResults['kind'], { label: string; color?: string }> = {
  few: { label: 'Too few tasks' },
  srm: { label: 'Assignment off', color: 'var(--wp-danger)' },
  difference: { label: 'A difference', color: 'var(--wp-accent)' },
  none: { label: 'No clear difference' },
}

function ResultsView({ x, run }: { x: Experiment; run: number }) {
  const [scope, setScope] = useState<'done' | 'all'>('done')
  const res = useQuery(experimentResultsQuery(x.key, run, scope))
  const r = res.data
  return (
    <div className="border-t border-line px-4 py-3" data-testid="experiment-results">
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <SegmentedControl
          label="Which tasks"
          value={scope}
          onChange={setScope}
          textFrom="always"
          options={[
            {
              value: 'done',
              label: 'Finished tasks',
              icon: <CircleCheck />,
              hint: 'Only tasks that reached Done',
            },
            {
              value: 'all',
              label: 'All measured',
              icon: <List />,
              hint: 'Every exposed task with usage, finished or not',
            },
          ]}
        />
        {r && (
          <>
            <Badge color={KIND[r.kind].color}>{KIND[r.kind].label}</Badge>
            {r.interim && <Badge color="var(--wp-warn)">Interim</Badge>}
          </>
        )}
      </div>
      {res.isPending ? (
        <Skeleton className="h-32" />
      ) : !r ? (
        <p className="text-xs text-subtle">The results couldn&apos;t be read.</p>
      ) : (
        <div className="space-y-3">
          <p className="text-[13px]">{r.verdict}</p>
          {(r.notes ?? []).map((n) => (
            <p key={n} className="text-xs text-warn">
              {n}
            </p>
          ))}
          <ArmsTable r={r} />
          <MetricsTable r={r} />
          {r.caveats && r.caveats.length > 0 && (
            <details className="text-xs text-muted">
              <summary className="cursor-pointer select-none">Caveats ({r.caveats.length})</summary>
              <ul className="mt-1 list-disc space-y-1 pl-5">
                {r.caveats.map((c) => (
                  <li key={c}>{c}</li>
                ))}
              </ul>
            </details>
          )}
        </div>
      )}
    </div>
  )
}

const pct = (f: number | null | undefined) => (f == null ? '–' : `${Math.round(100 * f)}%`)
const signed = (ratio: number) => {
  const v = Math.round(100 * (ratio - 1))
  return `${v > 0 ? '+' : ''}${v}%`
}

function value(m: ExperimentMetric, v: number | null) {
  if (v == null) return '–'
  if (m.unit === 'usd') return `$${v.toFixed(2)}`
  if (m.unit === 'tokens') {
    if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`
    if (v >= 1e3) return `${Math.round(v / 1e3)}k`
  }
  return Number.isInteger(v) ? String(v) : v.toFixed(1)
}

function ArmsTable({ r }: { r: ExperimentResults }) {
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs tabular-nums">
        <thead className="text-left text-subtle">
          <tr>
            <th className="py-1 pr-3 font-medium">Arm</th>
            <th className="py-1 pr-3 text-right font-medium">Assigned</th>
            <th className="py-1 pr-3 text-right font-medium">Exposed</th>
            <th className="py-1 pr-3 text-right font-medium">Measured</th>
            <th className="py-1 pr-3 text-right font-medium">Done</th>
            <th className="py-1 text-right font-medium">Compared</th>
          </tr>
        </thead>
        <tbody>
          {r.arms.map((a: ExperimentArmResult) => (
            <tr key={a.key} className="border-t border-line">
              <td className="py-1 pr-3">
                {a.label}
                {a.control && <span className="text-subtle"> (control)</span>}
              </td>
              <td className="py-1 pr-3 text-right">{a.assigned}</td>
              <td className="py-1 pr-3 text-right">{a.exposed}</td>
              <td className="py-1 pr-3 text-right">{a.measured}</td>
              <td className="py-1 pr-3 text-right">{pct(a.done_rate)}</td>
              <td className="py-1 text-right">{a.in_scope}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {r.srm != null && (
        <p className="mt-1 text-[11px] text-subtle">
          Assignment against the weights: p = {r.srm.toPrecision(2)}
          {r.srm < 0.01 && ' (off: look for a bug before reading anything else)'}
        </p>
      )}
    </div>
  )
}

function MetricsTable({ r }: { r: ExperimentResults }) {
  const control = r.arms[0]
  const others = r.arms.slice(1)
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-xs tabular-nums">
        <thead className="text-left text-subtle">
          <tr>
            <th className="py-1 pr-3 font-medium">Median per task</th>
            {r.arms.map((a) => (
              <th key={a.key} className="py-1 pr-3 text-right font-medium">
                {a.label}
              </th>
            ))}
            {others.map((a) => (
              <th key={a.key} className="py-1 pr-3 font-medium">
                {a.label} against {control.label}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {r.metrics.map((m, i) => (
            <tr
              key={m.key}
              className="border-t border-line"
              data-metric={m.key}
              data-primary={m.key === r.primary ? 'yes' : undefined}
            >
              <td className="py-1 pr-3">
                <span className={m.key === r.primary ? 'font-semibold' : undefined}>{m.label}</span>
                {m.key === r.primary && <span className="text-subtle"> (decides)</span>}
              </td>
              {r.arms.map((a) => (
                <td key={a.key} className="py-1 pr-3 text-right">
                  {value(m, a.stats[i]?.median ?? null)}
                </td>
              ))}
              {others.map((a) => {
                const st = a.stats[i]
                if (!st || st.ratio == null) {
                  return (
                    <td key={a.key} className="py-1 pr-3 text-subtle">
                      –
                    </td>
                  )
                }
                const sure = st.p_adj != null && st.p_adj < 0.05
                return (
                  <td key={a.key} className="whitespace-nowrap py-1 pr-3">
                    <span className={sure ? 'font-semibold' : undefined}>{signed(st.ratio)}</span>
                    {st.low != null && st.high != null && (
                      <span className="text-subtle">
                        {' '}
                        [{signed(st.low)}, {signed(st.high)}]
                      </span>
                    )}
                    {st.p_adj != null && (
                      <span className="text-subtle"> p={st.p_adj.toPrecision(2)}</span>
                    )}
                  </td>
                )
              })}
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-1 text-[11px] text-subtle">
        Change is the arm&apos;s median over the control&apos;s, with a 95% bootstrap interval; p is
        Mann–Whitney, Holm-adjusted across arms and metrics, the follow-up, self-review, adversarial
        review and review comment measures apart from the others. Bold: p below 0.05.
        {r.mde != null &&
          ` With these tasks, a change in the deciding metric under about ${Math.round(100 * r.mde)}% can't be told apart`}
        {r.per_arm
          ? `; about ${r.per_arm} per arm would tell a 20% change.`
          : r.mde != null
            ? '.'
            : ''}
      </p>
    </div>
  )
}
