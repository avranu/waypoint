// Asking for an audit (WP-348, WP-548): what to read (what landed on a
// branch over a period by default), what to look for (one sentence by
// default, editable) and the criteria (areas to look hardest at, paths to
// read or leave out, the lowest severity worth reporting, how many findings
// at most), which models to try in order, and whether to post on the pull
// request and have Claude check and fix the findings. Opened from anywhere
// with openAuditDialog, or in place with <AuditDialog>.

import { useEffect, useMemo, useState, useSyncExternalStore, type ReactNode } from 'react'
import { Link, useNavigate, useRouterState } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  CalendarClock,
  ChevronRight,
  FolderGit2,
  GitBranch,
  GitCommitHorizontal,
  GitCompareArrows,
  GitPullRequest,
  Repeat,
  RotateCcw,
  ShieldAlert,
} from 'lucide-react'
import { q } from '@/lib/queries'
import {
  auditDefaultsQuery,
  joinModel,
  MIN_SEVERITY,
  modelShort,
  requestAudit,
  splitModel,
  splitPaths,
  TARGETS,
  type AuditFocus,
  type AuditInput,
  type AuditTarget,
  type CodeAudit,
  type FindingSeverity,
} from '@/lib/code-audits'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented'
import { AgentChain, AGENT_CLIS, AUDIT_CLIS, type AgentModel } from '@/components/AgentChain'

const TARGET_ICON: Record<AuditTarget, ReactNode> = {
  recent: <CalendarClock className="size-3.5" />,
  pr: <GitPullRequest className="size-3.5" />,
  branch: <GitBranch className="size-3.5" />,
  commit: <GitCommitHorizontal className="size-3.5" />,
  range: <GitCompareArrows className="size-3.5" />,
  repo: <FolderGit2 className="size-3.5" />,
}

/** A pull request the dialog was opened from: the target is fixed. */
export interface AuditPR {
  id: number
  repo: string
  number: number
  title?: string
}

/** What the dialog starts from: a project, a target, an earlier audit's ask. */
export interface AuditPrefill {
  project?: string
  repo?: string
  target?: AuditTarget
  ref?: string
  base?: string
  /** A range's start. */
  from?: string
  since?: string
  focus?: string[]
  paths?: string[]
  exclude?: string[]
  min_severity?: FindingSeverity | ''
  max_findings?: number
  instructions?: string
  models?: string[]
  comment?: boolean
  fix?: boolean
  pr?: AuditPR
}

/** An audit's ask, to edit and run again. */
export function prefillFrom(a: CodeAudit): AuditPrefill {
  const out: AuditPrefill = {
    project: a.project,
    repo: a.repo,
    target: a.target,
    ref: a.ref,
    base: a.target === 'branch' ? a.base : undefined,
    from: a.target === 'range' ? a.base : undefined,
    since: a.since,
    focus: a.focus ?? [],
    paths: a.paths ?? [],
    exclude: a.exclude ?? [],
    min_severity: a.min_severity ?? '',
    max_findings: a.max_findings,
    instructions: a.instructions,
    models: a.models,
    comment: a.post_comment,
    fix: a.fix,
  }
  if (a.target === 'pr' && a.pr_id && a.pr_number)
    out.pr = { id: a.pr_id, repo: a.repo, number: a.pr_number }
  return out
}

// The dialog opened from anywhere: the command palette, an audit's page.
let state: { open: boolean; prefill: AuditPrefill } = { open: false, prefill: {} }
const subs = new Set<() => void>()
const set = (s: typeof state) => {
  state = s
  subs.forEach((f) => f())
}

export function openAuditDialog(prefill: AuditPrefill = {}) {
  set({ open: true, prefill })
}

/** Mounted once, in the app shell. */
export function GlobalAuditDialog() {
  const s = useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    () => state,
  )
  const pathname = useRouterState({ select: (r) => r.location.pathname })
  if (!s.open) return null
  const routeProject = pathname.match(/^\/p\/([^/]+)/)?.[1]
  const prefill = { ...s.prefill, project: s.prefill.project ?? routeProject }
  return (
    <AuditForm
      onClose={() => set({ open: false, prefill: {} })}
      prefill={prefill}
      pr={prefill.pr}
    />
  )
}

export function AuditDialog({
  open,
  onClose,
  project,
  pr,
}: {
  open: boolean
  onClose: () => void
  /** A project key to start with. */
  project?: string
  pr?: AuditPR
}) {
  if (!open) return null
  return <AuditForm onClose={onClose} prefill={{ project }} pr={pr} />
}

function AuditForm({
  onClose,
  prefill: p,
  pr,
}: {
  onClose: () => void
  prefill: AuditPrefill
  pr?: AuditPR
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const defaults = useQuery(auditDefaultsQuery()).data
  const projects = useQuery(q.projects()).data
  const editable = useMemo(
    () =>
      (projects ?? []).filter((x) => !x.archived && (x.role === 'owner' || x.role === 'editor')),
    [projects],
  )
  const [project, setProject] = useState(p.project ?? '')
  useEffect(() => {
    if (pr || !projects) return
    if (!editable.some((x) => x.key === project)) setProject(editable[0]?.key ?? '')
  }, [pr, projects, editable, project])
  const repos = useQuery({ ...q.repos(project), enabled: !pr && !!project }).data
  const [repo, setRepo] = useState(p.repo ?? '')
  useEffect(() => {
    if (!repos) return
    if (!repos.some((r) => r.repo === repo)) setRepo(repos[0]?.repo ?? '')
  }, [repos, repo])

  const [target, setTarget] = useState<AuditTarget>(pr ? 'pr' : (p.target ?? 'recent'))
  const [prNumber, setPRNumber] = useState('')
  const [ref, setRef] = useState(p.ref ?? '')
  const [base, setBase] = useState(p.base ?? '')
  const [from, setFrom] = useState(p.from ?? '')
  // null: the default period (a week).
  const [since, setSince] = useState<string | null>(p.since ?? null)
  const period = since ?? defaults?.since ?? '7d'

  const [instructions, setInstructions] = useState<string | null>(p.instructions ?? null)
  const [chain, setChain] = useState<AgentModel[] | null>(
    p.models?.length ? p.models.map(splitModel) : null,
  )
  const defaultChain = useMemo(() => (defaults?.models ?? []).map(splitModel), [defaults])
  const text = instructions ?? defaults?.instructions ?? ''
  const models = chain ?? defaultChain
  const chainChanged =
    chain !== null && models.map(joinModel).join() !== defaultChain.map(joinModel).join()

  const [focus, setFocus] = useState<string[]>(p.focus ?? [])
  const [paths, setPaths] = useState((p.paths ?? []).join('\n'))
  const [exclude, setExclude] = useState((p.exclude ?? []).join('\n'))
  const [minSeverity, setMinSeverity] = useState<FindingSeverity | ''>(p.min_severity ?? '')
  const [maxFindings, setMaxFindings] = useState(p.max_findings ? String(p.max_findings) : '')

  const [comment, setComment] = useState(p.comment ?? true)
  const [fix, setFix] = useState(p.fix ?? false)

  const cap = defaults?.findings_cap ?? 30
  const pathsCap = defaults?.paths_cap ?? 20

  const input = (): AuditInput | string => {
    const only = splitPaths(paths)
    const out = splitPaths(exclude)
    if (only.length > pathsCap || out.length > pathsCap)
      return `Name at most ${pathsCap} paths in each list.`
    let max: number | undefined
    if (maxFindings.trim()) {
      max = Number(maxFindings)
      if (!Number.isInteger(max) || max < 1 || max > cap)
        return `Ask for between 1 and ${cap} findings.`
    }
    const base_: AuditInput = {
      target,
      instructions: text.trim(),
      models: models.filter((m) => m.cli).map(joinModel),
      focus,
      paths: only,
      exclude: out,
      min_severity: minSeverity,
      max_findings: max,
      fix,
    }
    if (target === 'pr') base_.comment = comment
    if (pr) return { ...base_, pr_id: pr.id }
    if (!project) return 'Choose a project.'
    if (!repo) return 'This project has no repository attached.'
    const at = { ...base_, project, repo }
    switch (target) {
      case 'recent':
        if (!period.trim()) return 'Choose a date, or a span like 10d, to start from.'
        return { ...at, ref: ref.trim() || undefined, since: period.trim() }
      case 'pr': {
        const n = Number(prNumber.replace(/^#/, ''))
        if (!Number.isInteger(n) || n <= 0) return 'Say which pull request: its number.'
        return { ...at, pr: n }
      }
      case 'branch':
        if (!ref.trim()) return 'Say which branch.'
        return { ...at, ref: ref.trim(), base: base.trim() || undefined }
      case 'commit':
        if (!ref.trim()) return 'Say which commit.'
        return { ...at, ref: ref.trim() }
      case 'range':
        if (!from.trim() || !ref.trim()) return 'Say where the range starts and ends.'
        return { ...at, ref: `${from.trim()}..${ref.trim()}` }
      case 'repo':
        return { ...at, ref: ref.trim() || undefined }
    }
  }
  const ready = input()
  const ask = useMutation({
    mutationFn: (in_: AuditInput) => requestAudit(in_),
    onSuccess: (a) => {
      qc.invalidateQueries({ queryKey: ['audits'] })
      onClose()
      navigate({ to: '/audits/$id', params: { id: String(a.id) } })
    },
  })
  const [tried, setTried] = useState(false)
  const submit = () => {
    setTried(true)
    if (typeof ready !== 'string') ask.mutate(ready)
  }

  const noProjects = !pr && projects && editable.length === 0

  return (
    <Dialog
      open
      onClose={onClose}
      title="New audit"
      wide
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={submit}
            loading={ask.isPending}
            disabled={!defaults || !!noProjects}
            icon={<ShieldAlert className="size-4" />}
          >
            Start audit
          </Button>
        </>
      }
    >
      <form
        className="space-y-5 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <p className="text-[13px] text-muted">
          A model on your runner reads the code and reports what it finds. It never changes
          anything: each finding is a claim for someone to check.
        </p>

        {pr ? (
          <div className="flex items-center gap-2 rounded-lg border border-line bg-sunken px-3 py-2 text-[13px]">
            <GitPullRequest className="size-4 shrink-0 text-subtle" />
            <span className="font-mono text-xs text-muted">
              {pr.repo}#{pr.number}
            </span>
            {pr.title && <span className="min-w-0 truncate">{pr.title}</span>}
          </div>
        ) : noProjects ? (
          <p className="rounded-lg border border-line bg-sunken px-3 py-2 text-[13px] text-muted">
            You can audit the repositories of projects you edit, and you edit none.
          </p>
        ) : (
          <>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Project">
                <Select value={project} onChange={(e) => setProject(e.target.value)}>
                  {editable.map((x) => (
                    <option key={x.key} value={x.key}>
                      {x.name} ({x.key})
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="Repository">
                {repos && repos.length === 0 ? (
                  <p className="flex h-9 items-center text-[13px] text-subtle">
                    None attached to this project
                  </p>
                ) : (
                  <Select value={repo} onChange={(e) => setRepo(e.target.value)}>
                    {(repos ?? []).map((r) => (
                      <option key={r.id} value={r.repo}>
                        {r.repo}
                      </option>
                    ))}
                  </Select>
                )}
              </Field>
            </div>
            <div className="space-y-2">
              <div className="text-[13px] font-medium">What to read</div>
              <SegmentedControl
                label="What to read"
                value={target}
                onChange={setTarget}
                textFrom="md"
                options={TARGETS.map((t) => ({
                  value: t.value,
                  label: t.label,
                  hint: t.hint,
                  icon: TARGET_ICON[t.value],
                }))}
              />
              <p className="text-xs text-subtle">
                {TARGETS.find((t) => t.value === target)?.hint}.
              </p>
            </div>
            {target === 'recent' ? (
              <RecentFields
                periods={defaults?.periods ?? []}
                since={period}
                setSince={setSince}
                branch={ref}
                setBranch={setRef}
                project={project}
              />
            ) : (
              <TargetFields
                target={target}
                prNumber={prNumber}
                setPRNumber={setPRNumber}
                refValue={ref}
                setRef={setRef}
                base={base}
                setBase={setBase}
                from={from}
                setFrom={setFrom}
              />
            )}
          </>
        )}

        <div className="space-y-1.5">
          <div className="flex items-center gap-2">
            <label htmlFor="audit-instructions" className="text-[13px] font-medium">
              What to look for
            </label>
            {instructions !== null && instructions !== defaults?.instructions && (
              <ResetLink onClick={() => setInstructions(null)} />
            )}
          </div>
          <Textarea
            id="audit-instructions"
            autoGrow
            rows={2}
            value={text}
            maxLength={4000}
            onChange={(e) => setInstructions(e.target.value)}
            placeholder={defaults?.instructions}
          />
          <p className="text-xs text-subtle">
            The model reads this before the code. Name a part of the code, a kind of defect, or what
            changed and why.
          </p>
        </div>

        <Criteria
          focusAreas={defaults?.focus ?? []}
          focus={focus}
          setFocus={setFocus}
          paths={paths}
          setPaths={setPaths}
          exclude={exclude}
          setExclude={setExclude}
          minSeverity={minSeverity}
          setMinSeverity={setMinSeverity}
          maxFindings={maxFindings}
          setMaxFindings={setMaxFindings}
          defaultFindings={defaults?.max_findings ?? 15}
          cap={cap}
        />

        <details className="group rounded-xl border border-line" open={chainChanged || undefined}>
          <summary className="flex cursor-pointer list-none items-center gap-2 px-3 py-2.5 text-[13px] [&::-webkit-details-marker]:hidden">
            <ChevronRight className="size-3.5 shrink-0 text-subtle transition-transform group-open:rotate-90" />
            <span className="font-medium">Models</span>
            <span className="min-w-0 truncate font-mono text-xs text-subtle">
              {models
                .filter((m) => m.cli)
                .map((m) => modelShort(joinModel(m)))
                .join(' → ')}
            </span>
            {chainChanged && (
              <span className="ml-auto">
                <ResetLink onClick={() => setChain(null)} />
              </span>
            )}
          </summary>
          <div className="space-y-1.5 border-t border-line p-3">
            <div className="text-[13px] font-medium">Tried in order</div>
            <AgentChain
              value={models}
              onChange={setChain}
              clis={defaults?.gateway ? AUDIT_CLIS : AGENT_CLIS}
            />
            <p className="text-xs text-subtle">
              When one is busy or out of quota the next takes over; if all are only busy, the audit
              waits and tries again.{' '}
              {defaults?.gateway
                ? 'The LLM gateway picks its own model and reads a copy of your runner’s checkout; the others run on your runner, with your sign-ins.'
                : 'Each runs on your runner, with your sign-ins.'}
            </p>
          </div>
        </details>

        <div className="divide-y divide-line rounded-xl border border-line">
          {target === 'pr' && (
            <ToggleRow
              label="Post the findings on the pull request"
              detail="One comment, linking back here for the details and verdicts."
              checked={comment}
              onChange={setComment}
            />
          )}
          <ToggleRow
            label="Have Claude check and fix the findings"
            detail="When it ends with findings, a task for Claude to confirm each one, fix those that are real and reject the rest."
            checked={fix}
            onChange={setFix}
          />
        </div>

        {tried && typeof ready === 'string' ? (
          <p className="text-[13px] text-danger">{ready}</p>
        ) : ask.isError ? (
          <p className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-[13px]">
            {(ask.error as Error).message}
          </p>
        ) : null}
        <button type="submit" hidden />
      </form>
    </Dialog>
  )
}

/** A date input's value for a period kept as a date, or '' for a span. */
const asDate = (since: string) => (/^\d{4}-\d{2}-\d{2}$/.test(since) ? since : '')

function RecentFields({
  periods,
  since,
  setSince,
  branch,
  setBranch,
  project,
}: {
  periods: { value: string; label: string }[]
  since: string
  setSince: (v: string) => void
  branch: string
  setBranch: (v: string) => void
  project: string
}) {
  const named = periods.some((x) => x.value === since)
  const [custom, setCustom] = useState(periods.length > 0 && !named)
  // The defaults load after a prefill's period: one not offered is custom.
  useEffect(() => {
    if (periods.length && !named) setCustom(true)
  }, [periods.length, named])
  const date = asDate(since)
  const today = new Date().toISOString().slice(0, 10)
  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="text-[13px] font-medium">Over</div>
        <div role="radiogroup" aria-label="Over" className="flex flex-wrap gap-1.5">
          {periods.map((x) => (
            <Chip
              key={x.value}
              role="radio"
              on={!custom && since === x.value}
              onClick={() => {
                setCustom(false)
                setSince(x.value)
              }}
            >
              {x.label}
            </Chip>
          ))}
          <Chip
            role="radio"
            on={custom}
            onClick={() => {
              if (!custom) setSince('')
              setCustom(true)
            }}
          >
            Since a date…
          </Chip>
        </div>
        {custom && (
          <div className="flex flex-wrap items-center gap-2">
            <Input
              type="date"
              aria-label="Since"
              value={date}
              max={today}
              onChange={(e) => setSince(e.target.value)}
              className="w-44"
            />
            <span className="text-xs text-subtle">or a span:</span>
            <Input
              aria-label="Span"
              value={date ? '' : since}
              onChange={(e) => setSince(e.target.value)}
              placeholder="10d, 6w, 2mo"
              spellCheck={false}
              className="w-28 font-mono !text-[13px]"
            />
            <span className="w-full text-xs text-subtle">
              A date counts from midnight UTC; up to a year back.
            </span>
          </div>
        )}
      </div>
      <Field
        label="On branch"
        note="Every commit that landed on it in that time, merges included, read as one change at its tip."
      >
        <Input
          value={branch}
          onChange={(e) => setBranch(e.target.value)}
          placeholder="the default branch"
          spellCheck={false}
          className="font-mono !text-[13px] sm:w-64"
        />
      </Field>
      {project && (
        <p className="flex items-start gap-1.5 text-xs text-subtle">
          <Repeat className="mt-0.5 size-3.5 shrink-0" />
          <span>
            To have this run by itself every week, add the{' '}
            <Link
              to="/p/$key/settings"
              params={{ key: project }}
              search={{ tab: 'automations' }}
              className="text-accent hover:underline"
            >
              Weekly Grok audit of what landed
            </Link>{' '}
            automation from the gallery.
          </span>
        </p>
      )}
    </div>
  )
}

function Criteria({
  focusAreas,
  focus,
  setFocus,
  paths,
  setPaths,
  exclude,
  setExclude,
  minSeverity,
  setMinSeverity,
  maxFindings,
  setMaxFindings,
  defaultFindings,
  cap,
}: {
  focusAreas: AuditFocus[]
  focus: string[]
  setFocus: (v: string[]) => void
  paths: string
  setPaths: (v: string) => void
  exclude: string
  setExclude: (v: string) => void
  minSeverity: FindingSeverity | ''
  setMinSeverity: (v: FindingSeverity | '') => void
  maxFindings: string
  setMaxFindings: (v: string) => void
  defaultFindings: number
  cap: number
}) {
  const mono = 'font-mono !text-[13px]'
  return (
    <fieldset className="space-y-4">
      <legend className="mb-2 text-[13px] font-medium">Criteria</legend>
      <div className="space-y-2">
        <div className="text-[13px]">Look hardest at</div>
        <div className="flex flex-wrap gap-1.5" role="group" aria-label="Look hardest at">
          {focusAreas.map((f) => {
            const on = focus.includes(f.key)
            return (
              <Chip
                key={f.key}
                on={on}
                title={f.text}
                onClick={() => setFocus(on ? focus.filter((k) => k !== f.key) : [...focus, f.key])}
              >
                {f.label}
              </Chip>
            )
          })}
        </div>
        <p className="text-xs text-subtle">
          {focus.length
            ? 'Other defects are still reported; these get the closest reading.'
            : 'None chosen: every kind of defect gets the same reading.'}
        </p>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Only these paths" note="Empty: the whole change.">
          <Textarea
            rows={2}
            autoGrow
            value={paths}
            onChange={(e) => setPaths(e.target.value)}
            placeholder={'internal/core\nweb/src/**/*.tsx'}
            spellCheck={false}
            className={mono}
          />
        </Field>
        <Field label="Leave out" note="Lockfiles and generated code are left out anyway.">
          <Textarea
            rows={2}
            autoGrow
            value={exclude}
            onChange={(e) => setExclude(e.target.value)}
            placeholder={'docs\n**/*_test.go'}
            spellCheck={false}
            className={mono}
          />
        </Field>
      </div>
      <p className="-mt-2 text-xs text-subtle">
        Paths or globs from the repository’s root, one per line. The model still reads the rest of
        the code to follow calls, but reports only in these parts.
      </p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Report">
          <Select
            value={minSeverity}
            onChange={(e) => setMinSeverity(e.target.value as FindingSeverity | '')}
          >
            {MIN_SEVERITY.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </Select>
        </Field>
        <Field label="At most" note={`Findings, worst first; up to ${cap}.`}>
          <Input
            type="number"
            inputMode="numeric"
            min={1}
            max={cap}
            value={maxFindings}
            onChange={(e) => setMaxFindings(e.target.value)}
            placeholder={String(defaultFindings)}
            className="w-24"
          />
        </Field>
      </div>
    </fieldset>
  )
}

function Chip({
  on,
  onClick,
  title,
  role,
  children,
}: {
  on: boolean
  onClick: () => void
  title?: string
  role?: 'radio'
  children: ReactNode
}) {
  return (
    <button
      type="button"
      role={role}
      aria-checked={role ? on : undefined}
      aria-pressed={role ? undefined : on}
      title={title}
      onClick={onClick}
      className={clsx(
        'inline-flex h-7 items-center rounded-full border px-2.5 text-xs font-medium transition-colors',
        on
          ? 'border-accent/40 bg-accent-soft text-accent'
          : 'border-line text-muted hover:bg-hover hover:text-fg',
      )}
    >
      {children}
    </button>
  )
}

function TargetFields({
  target,
  prNumber,
  setPRNumber,
  refValue,
  setRef,
  base,
  setBase,
  from,
  setFrom,
}: {
  target: AuditTarget
  prNumber: string
  setPRNumber: (v: string) => void
  refValue: string
  setRef: (v: string) => void
  base: string
  setBase: (v: string) => void
  from: string
  setFrom: (v: string) => void
}) {
  const mono = 'font-mono !text-[13px]'
  switch (target) {
    case 'recent':
      return null
    case 'pr':
      return (
        <Field label="Pull request number">
          <Input
            inputMode="numeric"
            value={prNumber}
            onChange={(e) => setPRNumber(e.target.value)}
            placeholder="42"
            className={`w-32 ${mono}`}
          />
        </Field>
      )
    case 'branch':
      return (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Branch">
            <Input
              value={refValue}
              onChange={(e) => setRef(e.target.value)}
              placeholder="feature-branch"
              spellCheck={false}
              className={mono}
            />
          </Field>
          <Field label="Against">
            <Input
              value={base}
              onChange={(e) => setBase(e.target.value)}
              placeholder="the default branch"
              spellCheck={false}
              className={mono}
            />
          </Field>
        </div>
      )
    case 'commit':
      return (
        <Field label="Commit">
          <Input
            value={refValue}
            onChange={(e) => setRef(e.target.value)}
            placeholder="a SHA, a tag or a branch’s tip"
            spellCheck={false}
            className={mono}
          />
        </Field>
      )
    case 'range':
      return (
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="From">
            <Input
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              placeholder="v1.2.0"
              spellCheck={false}
              className={mono}
            />
          </Field>
          <Field label="To">
            <Input
              value={refValue}
              onChange={(e) => setRef(e.target.value)}
              placeholder="main"
              spellCheck={false}
              className={mono}
            />
          </Field>
        </div>
      )
    case 'repo':
      return (
        <Field label="At">
          <Input
            value={refValue}
            onChange={(e) => setRef(e.target.value)}
            placeholder="the default branch"
            spellCheck={false}
            className={mono}
          />
        </Field>
      )
  }
}

function ToggleRow({
  label,
  detail,
  checked,
  onChange,
}: {
  label: string
  detail: string
  checked: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <div className="flex items-start gap-3 px-3 py-2.5">
      <div className="min-w-0 flex-1">
        <div className="text-[13px] font-medium">{label}</div>
        <div className="text-xs text-subtle">{detail}</div>
      </div>
      <Switch checked={checked} onChange={onChange} label={label} />
    </div>
  )
}

function ResetLink({ onClick }: { onClick: () => void }) {
  return (
    <button
      type="button"
      onClick={(e) => {
        // Inside a <summary>: don't fold the section too.
        e.preventDefault()
        onClick()
      }}
      className="inline-flex items-center gap-1 text-xs font-normal text-subtle hover:text-fg"
    >
      <RotateCcw className="size-3" /> Default
    </button>
  )
}
