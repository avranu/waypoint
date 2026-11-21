// Objectives (WP-927): a task worked in rounds until a check is met ("until
// this runs in under a second"), by an amount every round, until a reviewer
// finds nothing, or as a loop with no check. A round ends where the task
// would close. Waypoint reads every round itself (the commands on the
// confirming person's runner, then a judge's audit); what the worker
// measures along the way is shown, and decides nothing.

import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ExternalLink, Plus, Repeat, Target, X } from 'lucide-react'
import clsx from 'clsx'
import type {
  Objective,
  ObjectiveCheck,
  ObjectiveFinding,
  ObjectiveInput,
  ObjectiveKind,
} from '@/lib/types'
import { q, useObjective, useObjectiveGo } from '@/lib/queries'
import {
  BLOCKED_ADVICE,
  HEALTH,
  KIND,
  OUTCOME,
  SOURCE,
  everyText,
  steers,
  workerReadings,
} from '@/lib/objective'
import { agentLabel } from '@/lib/agents'
import { dateTime, relativeTime, safeUrl } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Badge } from '@/components/ui/misc'
import { Markdown } from '@/components/Markdown'
import { toast, toastError } from '@/components/ui/toast'

export { everyText }

/** The full stop a sentence still needs: the summary may end in the person's own words, which can end in one. */
export const stop = (text: string) => (/[.!?]$/.test(text.trim()) ? '' : '.')

/** The server's sentence with its `commands` set as code. */
export function Sentence({ text }: { text: string }) {
  return (
    <>
      {text.split('`').map((part, i) =>
        i % 2 ? (
          <code key={i} className="rounded bg-sunken px-1 font-mono text-[12px]">
            {part}
          </code>
        ) : (
          part
        ),
      )}
    </>
  )
}

/** Where an objective stands, in a word. The word is the label, not only the colour. */
export function HealthBadge({ objective: o }: { objective: Pick<Objective, 'health'> }) {
  const h = HEALTH[o.health] ?? { label: o.health, tone: 'var(--wp-subtle)' }
  return <Badge color={h.tone}>{h.label}</Badge>
}

const SEVERITY: Record<ObjectiveFinding['severity'], string> = {
  critical: 'var(--wp-danger)',
  high: 'var(--wp-danger)',
  medium: 'var(--wp-warn)',
  low: 'var(--wp-subtle)',
}

/** A long command or a printed output: wraps, scrolls, never widens the page. */
function Mono({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <code
      className={clsx(
        'block max-h-32 overflow-auto whitespace-pre-wrap rounded bg-sunken px-1.5 py-1 font-mono text-[12px] [overflow-wrap:anywhere]',
        className,
      )}
    >
      {children}
    </code>
  )
}

function heading(o: Objective): string {
  switch (o.state) {
    case 'proposed':
      return 'Proposed objective'
    case 'met':
      return 'Objective met'
    case 'ended':
      return 'Objective ended'
    default:
      return `Objective · round ${o.round}${o.round_kind === 'diagnose' ? ' (diagnose)' : ''}${o.max_rounds ? ` of ${o.max_rounds}` : ''}`
  }
}

/** "1 round", "2 rounds". */
function roundsText(n: number) {
  return `${n} round${n === 1 ? '' : 's'}`
}

/** A diagnosis in the round list: collapsed, since it can run long (WP-1449). */
function DiagnosisText({ text, by, latest }: { text: string; by?: string; latest?: boolean }) {
  // The latest one is shown whole under "Latest diagnosis"; repeating it here
  // would show the same text twice on the page.
  if (latest)
    return (
      <span className="block text-subtle" data-testid="round-diagnosis">
        Diagnosis{by ? ` by ${agentLabel(by)}` : ''}: the latest, shown below
      </span>
    )
  return (
    <details className="mt-0.5 text-muted" data-testid="round-diagnosis">
      <summary className="cursor-pointer">Diagnosis{by ? ` by ${agentLabel(by)}` : ''}</summary>
      <Markdown text={text} className="text-[13px]" />
    </details>
  )
}

function Reading({ c, title }: { c: ObjectiveCheck; title: string }) {
  return (
    <div className="space-y-1">
      <p className="[overflow-wrap:anywhere]">
        <span className="font-medium">{title}</span> (round {c.round},{' '}
        <span title={dateTime(c.at)}>{relativeTime(c.at)}</span>, {SOURCE[c.source] ?? c.source}):{' '}
        {c.line}; {c.met ? 'met' : 'not met'}.
      </p>
      {c.broken && c.broken.length > 0 && (
        <p className="text-danger [overflow-wrap:anywhere]">
          The round doesn’t count, because{' '}
          {c.broken.map((b, i) => (
            <span key={i}>
              {i > 0 && ', '}
              <code className="rounded bg-sunken px-1 font-mono text-[12px]">{b}</code>
            </span>
          ))}{' '}
          no longer passes.
        </p>
      )}
      {c.touched && c.touched.length > 0 && (
        <p className="text-muted [overflow-wrap:anywhere]">
          The round changed what the check runs: {c.touched.join(', ')}
        </p>
      )}
      {c.note && <Mono>{c.note}</Mono>}
    </div>
  )
}

function Findings({ findings, bar }: { findings: ObjectiveFinding[]; bar?: string }) {
  return (
    <details open={findings.length <= 5} className="text-muted">
      <summary className="cursor-pointer">
        The reviewer’s findings: {findings.length} at or above {bar || 'medium'}
      </summary>
      <ul className="mt-1 max-h-72 space-y-2 overflow-y-auto pr-1">
        {findings.map((f) => (
          <li key={f.id} className="[overflow-wrap:anywhere]">
            <Badge color={SEVERITY[f.severity]}>{f.severity}</Badge>{' '}
            <span className="text-fg">{f.title}</span>
            {f.path && (
              <span className="ml-1 font-mono text-[12px] text-subtle">
                {f.path}
                {f.line ? `:${f.line}` : ''}
              </span>
            )}
            {f.problem && <p className="mt-0.5">{f.problem}</p>}
          </li>
        ))}
      </ul>
    </details>
  )
}

/** What the objective is, spelled out: commands, where they run, the judge. */
function Definition({ o }: { o: Objective }) {
  const rows: [string, ReactNode][] = []
  if (o.check) rows.push(['Check', <Mono key="c">{o.check}</Mono>])
  if (o.until) rows.push(['Until', o.until])
  if (o.dir) rows.push(['Folder', <Mono key="d">{o.dir}</Mono>])
  if (o.setup) rows.push(['Setup', <Mono key="s">{o.setup}</Mono>])
  if (o.keep_passing.length > 0)
    rows.push([
      'Keeps passing',
      <ul key="h" className="space-y-1">
        {o.keep_passing.map((h, i) => (
          <li key={i}>
            <Mono>{h}</Mono>
          </li>
        ))}
      </ul>,
    ])
  if (o.judge) {
    rows.push(['Reviewer reads against', <span key="j">{o.judge}</span>])
    rows.push(['Finding that keeps it open', `${o.judge_bar || 'medium'} or worse`])
    if (o.judge_models.length > 0) rows.push(['Reviewer models', o.judge_models.join(', ')])
  }
  if (o.repo) rows.push(['Read in', `${o.repo}${o.branch ? ` on ${o.branch}` : ''}`])
  rows.push(['Pace', o.pace === 'spare' ? 'Spare quota only' : 'In the queue'])
  rows.push(['Between rounds', o.every ? `at least ${everyText(o.every)}` : 'no wait'])
  rows.push(['Most rounds', o.max_rounds ? String(o.max_rounds) : 'no limit'])
  if (o.kind !== 'loop')
    rows.push([
      'Diagnose after',
      o.diagnose_after
        ? `${roundsText(o.diagnose_after)} in a row that came no nearer`
        : 'never (every round is a work round)',
    ])
  return (
    <details className="text-muted">
      <summary className="cursor-pointer">What it runs and where</summary>
      <dl className="mt-1 grid grid-cols-[minmax(0,9rem)_minmax(0,1fr)] gap-x-3 gap-y-1.5">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-subtle">{k}</dt>
            <dd className="min-w-0 [overflow-wrap:anywhere]">{v}</dd>
          </div>
        ))}
      </dl>
    </details>
  )
}

type Pending = 'end' | 'clear' | null

/** Says a task's objective and where its rounds stand, with what a person may do about it. */
export function ObjectiveBanner({
  objective: o,
  editable,
  closed = false,
  onChange,
  className,
}: {
  objective: Objective
  /** the viewer may change the task */
  editable: boolean
  /** the task is done or canceled: nothing is offered */
  closed?: boolean
  onChange: () => void
  className?: string
}) {
  const act = useObjective(o.key)
  const go = useObjectiveGo(o.key)
  const [pending, setPending] = useState<Pending>(null)
  const proposed = o.state === 'proposed'
  const active = o.state === 'active'
  const h = HEALTH[o.health] ?? HEALTH.working
  const tone = h.tone
  const Icon = o.kind === 'loop' ? Repeat : Target
  const may = editable && !closed
  const controls = may ? steers(o) : []
  const worker = workerReadings(o)
  const verdict = o.verdict
  const loop = o.kind === 'loop'
  const advice = active && o.phase === 'blocked' && o.phase_why ? BLOCKED_ADVICE[o.phase_why] : ''
  const busy = go.isPending || act.isPending
  return (
    <div
      role="note"
      aria-label="Objective"
      className={clsx(
        'rounded-lg border px-3 py-2 text-[13px]',
        proposed && 'border-dashed',
        className,
      )}
      style={{
        borderColor: `color-mix(in oklab, ${tone} 45%, transparent)`,
        background: `color-mix(in oklab, ${tone} 6%, transparent)`,
      }}
    >
      <div className="flex items-start gap-x-3">
        <Icon className="mt-0.5 size-4 shrink-0" style={{ color: tone }} aria-hidden />
        <div className="min-w-0 flex-1 space-y-2">
          <p className="[overflow-wrap:anywhere]">
            <span className="font-medium">{heading(o)}:</span> <HealthBadge objective={o} />{' '}
            <Sentence text={o.summary} />
            {stop(o.summary)}
          </p>
          <p role="status" className="text-muted [overflow-wrap:anywhere]">
            {o.health_note && <Sentence text={o.health_note} />}
            {o.health === 'stuck' && o.stuck_since && <> Stuck since {dateTime(o.stuck_since)}.</>}
            {o.health === 'waiting' && o.next_at && o.stalled ? (
              <>
                {' '}
                {o.stalled} round{o.stalled === 1 ? '' : 's'} in a row moved nothing, so it backs
                off.
              </>
            ) : null}
            {active && o.round_kind === 'diagnose' && !o.phase && (
              <>
                {' '}
                This is a diagnose round: it ships no code. The worker finds out why the last{' '}
                {roundsText(o.no_better || o.diagnose_after)} came no nearer and records it.
              </>
            )}
            {active && o.pace === 'spare' && <> Runs only on quota that would go unused.</>}
          </p>
          {proposed && (
            <div className="space-y-1.5">
              <p className="text-muted">
                {o.set_agent ? agentLabel(o.set_agent) : 'An agent'} proposed it. Nothing runs and
                the task doesn’t repeat until you confirm it. Read it first: its commands run on
                your runner.
              </p>
              <details className="text-muted">
                <summary className="cursor-pointer">Read the whole proposal</summary>
                <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap rounded bg-sunken p-2 font-mono text-[12px] [overflow-wrap:anywhere]">
                  {o.text}
                </pre>
              </details>
            </div>
          )}
          {advice && (
            <p className="[overflow-wrap:anywhere]">
              <span className="font-medium">What you can do:</span> {advice}
            </p>
          )}
          {verdict ? (
            <Reading c={verdict} title="Waypoint’s reading, which decides" />
          ) : (
            active &&
            !loop && (
              <p className="text-muted">
                Waypoint hasn’t taken a reading yet. Only its own reading decides a round.
              </p>
            )
          )}
          {o.findings && o.findings.length > 0 && (
            <Findings findings={o.findings} bar={o.judge_bar} />
          )}
          {worker.length > 0 && (
            <details className="text-muted">
              <summary className="cursor-pointer">
                The worker’s own readings ({worker.length}): shown, they don’t decide
              </summary>
              <ul className="mt-1 space-y-0.5">
                {worker.map((c) => (
                  <li key={c.id} className="[overflow-wrap:anywhere]">
                    Round {c.round}: {c.line}
                    <span className="text-subtle" title={dateTime(c.at)}>
                      {' '}
                      · {relativeTime(c.at)}
                    </span>
                  </li>
                ))}
              </ul>
            </details>
          )}
          {o.rounds.length > 0 && (
            <details className="text-muted">
              <summary className="cursor-pointer">
                {o.rounds.length === 1
                  ? '1 finished round'
                  : `Last ${o.rounds.length} finished rounds`}
              </summary>
              <ul className="mt-1 space-y-1">
                {o.rounds.map((r) => (
                  <li key={r.id} className="[overflow-wrap:anywhere]">
                    Round {r.round}: {OUTCOME[r.outcome] ?? r.outcome}
                    {r.check ? ` (${r.check.line})` : ''}
                    <span className="text-subtle" title={dateTime(r.ended_at)}>
                      {' '}
                      · {relativeTime(r.ended_at)}
                    </span>
                    {safeUrl(r.pr_url) && (
                      <a
                        href={safeUrl(r.pr_url)}
                        target="_blank"
                        rel="noreferrer noopener"
                        className="ml-1.5 inline-flex items-center gap-1 text-subtle hover:text-fg"
                      >
                        <ExternalLink className="size-3" aria-hidden />
                        pull request
                      </a>
                    )}
                    {r.note && <span className="block text-subtle">{r.note}</span>}
                    {r.diagnosis && (
                      <DiagnosisText
                        text={r.diagnosis}
                        by={r.diagnosis_by}
                        latest={o.diagnosis?.round === r.round && o.diagnosis.text === r.diagnosis}
                      />
                    )}
                  </li>
                ))}
              </ul>
            </details>
          )}
          {o.diagnosis && (
            <div className="space-y-1" data-testid="objective-diagnosis">
              <p className="[overflow-wrap:anywhere]">
                <span className="font-medium">Latest diagnosis</span> (round {o.diagnosis.round},{' '}
                {agentLabel(o.diagnosis.by)},{' '}
                <span title={dateTime(o.diagnosis.at)}>{relativeTime(o.diagnosis.at)}</span>), read
                by every round until one makes progress:
              </p>
              <Markdown text={o.diagnosis.text} className="text-[13px]" />
            </div>
          )}
          <Definition o={o} />
          {may && (
            <div className="flex flex-wrap gap-2 pt-1" role="group" aria-label="Objective controls">
              {proposed && (
                <Button
                  size="sm"
                  variant="primary"
                  disabled={busy}
                  loading={act.isPending && !!act.variables && 'confirm' in act.variables}
                  title={`Confirms this exact proposal (${o.rev})`}
                  aria-describedby={undefined}
                  onClick={() =>
                    act.mutate(
                      { confirm: o.rev },
                      {
                        onSuccess: () => toast(`${o.key}’s objective is in effect`),
                        onError: toastError,
                      },
                    )
                  }
                >
                  Confirm this proposal
                </Button>
              )}
              {controls.map((c) => (
                <Button
                  key={c.action}
                  size="sm"
                  variant={c.action === 'retry' || c.action === 'accept' ? 'primary' : 'secondary'}
                  title={c.hint}
                  disabled={busy}
                  loading={go.isPending && go.variables === c.action}
                  onClick={() =>
                    go.mutate(c.action, {
                      onSuccess: () => toast(`${o.key}: ${c.label.toLowerCase()}`),
                      onError: toastError,
                    })
                  }
                >
                  {c.label}
                </Button>
              ))}
              <Button size="sm" disabled={busy} onClick={onChange}>
                {proposed ? 'Edit and put in effect…' : active ? 'Change…' : 'Start again…'}
              </Button>
              {active && (
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={busy}
                  title="Stop the rounds; the task stays as it is, an ordinary task"
                  onClick={() => setPending('end')}
                >
                  End objective
                </Button>
              )}
              <Button
                size="sm"
                variant="ghost"
                disabled={busy}
                title="Take the objective and its history off the task"
                onClick={() => setPending('clear')}
              >
                {proposed ? 'Discard' : 'Remove'}
              </Button>
            </div>
          )}
        </div>
      </div>
      <Confirm
        open={pending === 'end'}
        onClose={() => setPending(null)}
        title={`End ${o.key}’s objective?`}
        body="The rounds stop. A reading under way is dropped, and the task stays as it is: an ordinary task. You can start it again later."
        confirmLabel="End objective"
        onConfirm={() =>
          go.mutate('end', {
            onSuccess: () => toast(`${o.key}’s objective ended`),
            onError: toastError,
          })
        }
      />
      <Confirm
        open={pending === 'clear'}
        onClose={() => setPending(null)}
        title={proposed ? `Discard the proposal on ${o.key}?` : `Remove ${o.key}’s objective?`}
        body={
          proposed
            ? 'The agent’s proposal is thrown away. Nothing has run.'
            : 'The objective, its readings and its rounds are deleted, and the task becomes an ordinary task.'
        }
        confirmLabel={proposed ? 'Discard' : 'Remove'}
        onConfirm={() =>
          act.mutate(
            { clear: true },
            {
              onSuccess: () => toast(`${o.key} has no objective now: it’s an ordinary task`),
              onError: toastError,
            },
          )
        }
      />
    </div>
  )
}

const BARS = ['critical', 'high', 'medium', 'low'] as const

const splitModels = (s: string) =>
  s
    .split(/[\n,]/)
    .map((m) => m.trim())
    .filter(Boolean)

/** Gives a task an objective, or changes the one it has. */
export function ObjectiveDialog({
  open,
  onClose,
  issueKey,
  objective: o,
}: {
  open: boolean
  onClose: () => void
  issueKey: string
  objective?: Objective
}) {
  const save = useObjective(issueKey)
  const repos = useQuery({ ...q.repos(issueKey.replace(/-\d+$/, '')), enabled: open })
  const [kind, setKind] = useState<ObjectiveKind>('until')
  const [check, setCheck] = useState('')
  const [until, setUntil] = useState('')
  const [measure, setMeasure] = useState('')
  const [better, setBetter] = useState('')
  const [dir, setDir] = useState('')
  const [setup, setSetup] = useState('')
  const [keep, setKeep] = useState<string[]>([])
  const [judge, setJudge] = useState('')
  const [alsoJudge, setAlsoJudge] = useState(false)
  const [bar, setBar] = useState('medium')
  const [models, setModels] = useState('')
  const [repo, setRepo] = useState('')
  const [branch, setBranch] = useState('')
  const [pace, setPace] = useState<Objective['pace']>('queue')
  const [every, setEvery] = useState('')
  const [rounds, setRounds] = useState('')
  const [diagnoseAfter, setDiagnoseAfter] = useState('2')
  const [tried, setTried] = useState(false)
  // Filled in as the dialog opens; a refetch of the objective while it is
  // open (a reading coming in) mustn't wipe what is being typed.
  const latest = useRef(o)
  latest.current = o
  useEffect(() => {
    if (!open) return
    const o = latest.current
    setKind(o?.kind ?? 'until')
    setCheck(o?.check ?? '')
    setUntil(o?.until ?? '')
    setMeasure(o?.measure === 'time' || o?.measure === 'value' ? o.measure : '')
    setBetter(o?.better ?? '')
    setDir(o?.dir ?? '')
    setSetup(o?.setup ?? '')
    setKeep(o?.keep_passing ?? [])
    setJudge(o?.judge ?? '')
    setAlsoJudge(!!o?.judge && o.kind === 'until')
    setBar(o?.judge_bar ?? 'medium')
    setModels((o?.judge_models ?? []).join(', '))
    setRepo(o?.repo ?? '')
    setBranch(o?.branch ?? '')
    setPace(o?.pace ?? 'queue')
    setEvery(o ? everyText(o.every) : '')
    setRounds(o?.max_rounds ? String(o.max_rounds) : '')
    setDiagnoseAfter(String(o?.diagnose_after ?? 2))
    setTried(false)
  }, [open])

  const proposed = o?.state === 'proposed'
  const hasCheck = kind === 'until' || kind === 'each'
  const judging = kind === 'judged' || (kind === 'until' && alsoJudge)
  const verified = kind !== 'loop'
  const keepList = keep.map((h) => h.trim()).filter(Boolean)
  const runsCommands = hasCheck || keepList.length > 0
  const passes = until.trim().toLowerCase() === 'pass' || until.trim().toLowerCase() === 'passes'
  const repoList = repos.data ?? []
  const missing: Partial<Record<'check' | 'until' | 'judge' | 'repo', string>> = {}
  if (hasCheck && !check.trim()) missing.check = 'Say what to run.'
  if (hasCheck && !until.trim()) missing.until = 'Say what the reading must reach.'
  if (judging && !judge.trim()) missing.judge = 'Say what done means.'
  if (verified && repoList.length > 1 && !repo)
    missing.repo = 'Choose the repository it is read in.'
  const submit = () => {
    setTried(true)
    if (Object.keys(missing).length > 0) return
    const set: ObjectiveInput = {
      check: hasCheck ? check.trim() : '',
      until: hasCheck ? until.trim() : '',
      measure: hasCheck && !passes ? measure : '',
      better: hasCheck && !passes ? better : '',
      keep_passing: verified ? keepList : [],
      dir: runsCommands ? dir.trim() : '',
      setup: runsCommands ? setup.trim() : '',
      judge: judging ? judge.trim() : '',
      judge_bar: judging ? bar : '',
      judge_models: judging ? splitModels(models) : [],
      repo: verified ? repo : '',
      branch: verified ? branch.trim() : '',
      pace,
      max_rounds: Math.max(0, parseInt(rounds, 10) || 0),
    }
    // Blank sends nothing (the server keeps the current value, or takes its
    // default of 2 for a new objective), and a loop has no rounds to read.
    if (verified && diagnoseAfter.trim()) {
      const diagnoseN = Number(diagnoseAfter)
      if (!Number.isInteger(diagnoseN) || diagnoseN < 0 || diagnoseN > 20) {
        toastError(new Error('Diagnose after is a whole number from 0 to 20.'))
        return
      }
      set.diagnose_after = diagnoseN
    }
    // Blank, or left as it was, keeps the kind's own gap: a day for one
    // that never ends, none for one with a fixed target.
    if (every.trim() && !(o && every.trim() === everyText(o.every))) set.every = every.trim()
    // A change of an agent's proposal builds on the one that was read.
    if (proposed) set.rev = o!.rev
    save.mutate(
      { set },
      {
        onSuccess: () => {
          toast(
            proposed
              ? `${issueKey}’s objective is in effect`
              : o
                ? `Changed ${issueKey}’s objective`
                : `${issueKey} has an objective`,
          )
          onClose()
        },
        onError: toastError,
      },
    )
  }
  const field = (k: keyof typeof missing) => (tried ? missing[k] : undefined)
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={
        proposed
          ? `Put ${issueKey}’s objective in effect`
          : o
            ? `Change ${issueKey}’s objective`
            : `Give ${issueKey} an objective`
      }
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={submit} loading={save.isPending}>
            {proposed
              ? 'Put in effect'
              : o?.state === 'met' || o?.state === 'ended'
                ? 'Start again'
                : o
                  ? 'Save'
                  : 'Set objective'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          Agents work the task in rounds. When a round’s work ends, Waypoint reads the result
          itself: the commands run on your runner, and a reviewer audits a judged one. Only that
          reading decides; what the worker measures never does.
        </p>
        {proposed && (
          <p className="rounded-lg border border-dashed border-warn/50 bg-warn/5 px-3 py-2 text-[13px] [overflow-wrap:anywhere]">
            {o.set_agent ? agentLabel(o.set_agent) : 'An agent'} proposed:{' '}
            <Sentence text={o.summary} />
            {stop(o.summary)} What you save builds on that proposal (rev {o.rev}) and goes into
            effect at once.
          </p>
        )}
        {(o?.state === 'met' || o?.state === 'ended') && (
          <p className="text-[13px] text-muted">
            It {o.state === 'met' ? 'was met' : 'ended'}. Saving starts it again on round{' '}
            {o.round + 1}.
          </p>
        )}
        <fieldset className="space-y-2">
          <legend className="text-[13px] font-medium leading-5">What the rounds are for</legend>
          {(Object.keys(KIND) as ObjectiveKind[]).map((k) => (
            <label
              key={k}
              className={clsx(
                'flex cursor-pointer items-start gap-2.5 rounded-lg border px-3 py-2 text-[13px]',
                kind === k ? 'border-accent bg-accent-soft' : 'border-line hover:bg-hover',
              )}
            >
              <input
                type="radio"
                name="objective-kind"
                className="mt-0.5 accent-[var(--wp-accent)]"
                checked={kind === k}
                onChange={() => setKind(k)}
              />
              <span className="min-w-0">
                <span className="block font-medium">{KIND[k].label}</span>
                <span className="block text-muted">{KIND[k].hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
        {hasCheck && (
          <>
            <Field
              label="Check command"
              hint="Shell, run in a fresh checkout of the repository on your runner, before the first round and after each round’s work."
              error={field('check')}
            >
              <Input
                autoFocus
                className="font-mono"
                value={check}
                onChange={(e) => setCheck(e.target.value)}
                placeholder="go test ./internal/report -run TestSlow"
              />
            </Field>
            <Field
              label={kind === 'each' ? 'Better by' : 'Until'}
              hint={
                kind === 'each'
                  ? '‘2x’ (twice as fast), ‘-10%’ or ‘+5%’: that much better than where each round started. Never met for good.'
                  : '‘< 1s’ or ‘>= 80%’: a fixed target (a time is how long the command takes, a number is the last one it prints). ‘pass’: the command exits 0.'
              }
              error={field('until')}
            >
              <Input
                value={until}
                onChange={(e) => setUntil(e.target.value)}
                placeholder={kind === 'each' ? '2x' : '< 1s'}
              />
            </Field>
            {!passes && (
              <div className="grid gap-4 sm:grid-cols-2">
                <Field
                  label="Measure"
                  hint="What a reading is. From the target: a time unit makes it how long the command takes, anything else a number it prints."
                >
                  <Select value={measure} onChange={(e) => setMeasure(e.target.value)}>
                    <option value="">From the target</option>
                    <option value="time">How long the command takes</option>
                    <option value="value">A number it prints</option>
                  </Select>
                </Field>
                <Field
                  label="Better is"
                  hint="Which direction is an improvement. From the target: a ceiling (‘<’) means lower, a floor (‘>’) higher."
                >
                  <Select value={better} onChange={(e) => setBetter(e.target.value)}>
                    <option value="">From the target</option>
                    <option value="lower">Lower</option>
                    <option value="higher">Higher</option>
                  </Select>
                </Field>
              </div>
            )}
          </>
        )}
        {kind === 'until' && (
          <div className="flex items-center justify-between gap-3 text-[13px]">
            <span>A reviewer must also find nothing</span>
            <Switch
              checked={alsoJudge}
              onChange={setAlsoJudge}
              label="A reviewer must also find nothing"
            />
          </div>
        )}
        {judging && (
          <>
            <Field
              label="What done means"
              hint="Said in words for a model that didn’t do the work. It reads the repository against this after every round; what it finds is the next round’s brief."
              error={field('judge')}
            >
              <Textarea
                autoGrow
                rows={3}
                value={judge}
                onChange={(e) => setJudge(e.target.value)}
                placeholder="Every exported function in internal/report has a test and no known race."
              />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field
                label="Keeps it open at"
                hint="The lowest severity of finding that keeps the task open."
              >
                <Select value={bar} onChange={(e) => setBar(e.target.value)}>
                  {BARS.map((b) => (
                    <option key={b} value={b}>
                      {b === 'low' ? 'low (anything)' : `${b} or worse`}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field
                label="Reviewer models"
                hint="Models asked, in order, separated by commas. Blank: the audit default."
              >
                <Input
                  className="font-mono"
                  value={models}
                  onChange={(e) => setModels(e.target.value)}
                  placeholder="audit default"
                />
              </Field>
            </div>
          </>
        )}
        {verified && (
          <fieldset className="space-y-2">
            <legend className="text-[13px] font-medium leading-5">Must keep passing</legend>
            <p className="text-xs text-subtle">
              Commands that must keep passing (a build, the test suite). A round that breaks one
              hasn’t met the objective, whatever the check reads. At most 8.
            </p>
            {keep.map((h, i) => (
              <div key={i} className="flex items-center gap-2">
                <Input
                  className="min-w-0 flex-1 font-mono"
                  aria-label={`Command that must keep passing ${i + 1}`}
                  value={h}
                  onChange={(e) => setKeep(keep.map((x, j) => (j === i ? e.target.value : x)))}
                  placeholder="make build"
                />
                <IconButton
                  label={`Remove command ${i + 1}`}
                  onClick={() => setKeep(keep.filter((_, j) => j !== i))}
                >
                  <X />
                </IconButton>
              </div>
            ))}
            {keep.length < 8 && (
              <Button size="sm" icon={<Plus />} onClick={() => setKeep([...keep, ''])}>
                Add a command
              </Button>
            )}
          </fieldset>
        )}
        {verified && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field
              label="Repository"
              hint="The repository the commands run in and the reviewer reads."
              error={field('repo')}
            >
              <Select value={repo} onChange={(e) => setRepo(e.target.value)}>
                <option value="">
                  {repoList.length > 1
                    ? 'Choose…'
                    : repoList.length === 1
                      ? `${repoList[0]!.repo} (its only one)`
                      : repos.isPending
                        ? 'Loading…'
                        : 'None attached to the project'}
                </option>
                {repoList.map((r) => (
                  <option key={r.id} value={r.repo}>
                    {r.repo}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Branch" hint="The branch that is read. Blank: the repository’s default.">
              <Input
                className="font-mono"
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
                placeholder="default branch"
              />
            </Field>
          </div>
        )}
        {runsCommands && (
          <div className="grid gap-4 sm:grid-cols-2">
            <Field label="Folder" hint="Where in the repository the commands run. Blank: its root.">
              <Input
                className="font-mono"
                value={dir}
                onChange={(e) => setDir(e.target.value)}
                placeholder="web"
              />
            </Field>
            <Field
              label="Setup"
              hint="Shell run once before a timed check, and not timed: a build, say."
            >
              <Input
                className="font-mono"
                value={setup}
                onChange={(e) => setSetup(e.target.value)}
                placeholder="make build"
              />
            </Field>
          </div>
        )}
        <div className="grid gap-4 sm:grid-cols-2">
          <Field
            label="Pace"
            hint="In the queue: rounds are handed out like any task. Spare quota only: a round starts only when the week’s quota would otherwise go unused, so it never takes from other work."
          >
            <Select value={pace} onChange={(e) => setPace(e.target.value as Objective['pace'])}>
              <option value="queue">In the queue</option>
              <option value="spare">Spare quota only</option>
            </Select>
          </Field>
          <Field
            label="Between rounds"
            hint="The least time from one round’s end to the next’s start: 0, 90m, 24h, 3d. Blank: a day for one that never ends by itself, none otherwise. Rounds that move nothing wait longer by themselves."
          >
            <Input value={every} onChange={(e) => setEvery(e.target.value)} placeholder="24h" />
          </Field>
          <Field
            label="Most rounds"
            hint="It stops, needing you, after this many rounds. Blank: no limit (at most 1000)."
          >
            <Input
              type="number"
              min={0}
              max={1000}
              value={rounds}
              onChange={(e) => setRounds(e.target.value)}
              placeholder="No limit"
            />
          </Field>
          {verified && (
            <Field
              label="Diagnose after"
              hint="After this many rounds in a row that come no nearer, the next round ships no code: the worker finds out why and says what to try. 0: never. Blank: the current value (2, the default, for a new objective). At most 20."
            >
              <Input
                type="number"
                min={0}
                max={20}
                value={diagnoseAfter}
                onChange={(e) => setDiagnoseAfter(e.target.value)}
              />
            </Field>
          )}
        </div>
      </form>
    </Dialog>
  )
}
