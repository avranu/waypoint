import { useState } from 'react'
import { Link } from '@tanstack/react-router'
import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Check,
  CircleAlert,
  ExternalLink,
  LayoutList,
  Loader,
  RotateCcw,
  Square,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { duration, plural, relativeTime, safeUrl } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Badge, Skeleton } from '@/components/ui/misc'
import { Select } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented'
import { toastError } from '@/components/ui/toast'
import { shortModel } from '@/components/ModelChain'
import type { BotFinding, BotRun, ModelAttempt, Project } from '@/lib/types'
import { TabIntro, canEdit } from './shared'

const STATUS: Record<BotRun['status'], { label: string; color?: string }> = {
  queued: { label: 'Queued' },
  running: { label: 'Running', color: 'var(--wp-accent)' },
  done: { label: 'Done', color: 'var(--wp-ok)' },
  failed: { label: 'Failed', color: 'var(--wp-danger)' },
  skipped: { label: 'Skipped' },
  cancelled: { label: 'Cancelled' },
}

const REASON: Record<string, string> = {
  opened: 'opened',
  pushed: 'new commits',
  ready: 'ready for review',
  reopened: 'reopened',
  review: 'asked with @waypoint review',
  'full review': 'asked with @waypoint full review',
}

export const SEVERITY: Record<BotFinding['severity'], { badge: string; color: string }> = {
  critical: { badge: 'P0', color: 'var(--wp-danger)' },
  high: { badge: 'P1', color: 'var(--wp-warn)' },
  medium: { badge: 'P2', color: 'var(--wp-accent)' },
  low: { badge: 'P3', color: 'var(--wp-subtle)' },
}

/** How sure a finding is: raised by several reviewers, checked against the code (WP-D14). */
export function FindingMarks({ f }: { f: BotFinding }) {
  const marks: string[] = []
  if ((f.agreed ?? 1) > 1) marks.push(`raised by ${f.agreed} reviewers`)
  if (f.verified_by) marks.push(`checked by ${shortModel(f.verified_by)}`)
  if (f.placement === 'later') marks.push('not posted on the pull request')
  if (marks.length === 0) return null
  return (
    <span className="text-xs text-subtle" title={f.verified_by || undefined}>
      {' · '}
      {marks.join(' · ')}
    </span>
  )
}

const OUTCOME: Record<ModelAttempt['outcome'], string> = {
  ok: 'answered',
  rate_limit: 'rate limited',
  timeout: 'timed out',
  server: 'server error',
  auth: 'not allowed',
  unavailable: 'not offered',
  bad_output: 'unusable answer',
  error: 'failed',
  cancelled: 'cancelled',
}

type StatusFilter = 'all' | 'live' | 'failed' | 'done'
const PAGE = 50

export function BotsTab({ project }: { project: Project }) {
  const [status, setStatus] = useState<StatusFilter>('all')
  const [rule, setRule] = useState(0)
  const [limit, setLimit] = useState(PAGE)
  const filter = { status: status === 'all' ? undefined : status, automation: rule || undefined }
  const runs = useQuery({
    ...q.botRuns(project.key, { ...filter, limit }),
    placeholderData: keepPreviousData,
  })
  const rules = useQuery(q.automations(project.key)).data ?? []
  const list = runs.data ?? []
  const filtered = status !== 'all' || rule !== 0
  const botRules = rules.filter((a) => a.rule.actions?.some((x) => BOT_STEPS.includes(x.type)))
  return (
    <div className="space-y-4">
      <TabIntro title="Bots">
        What bots did on the pull requests of this project’s repos: each run, why it ran, the models
        it tried (in order, until one answered) and what they found. Which bots run, and with which
        models, is set in <b>Automations</b>; repos with <b>Bots</b> off in <b>Integrations</b> are
        left alone. In a pull request, <b>@waypoint review</b> asks for a review and{' '}
        <b>@waypoint pause</b> stops automatic ones.
      </TabIntro>
      {(list.length > 0 || filtered) && (
        <div className="flex flex-wrap items-center gap-2">
          <SegmentedControl<StatusFilter>
            label="Show runs"
            value={status}
            onChange={(v) => {
              setStatus(v)
              setLimit(PAGE)
            }}
            options={[
              { value: 'all', label: 'All', icon: <LayoutList className="size-3.5" /> },
              { value: 'live', label: 'Live', icon: <Loader className="size-3.5" /> },
              { value: 'failed', label: 'Failed', icon: <CircleAlert className="size-3.5" /> },
              { value: 'done', label: 'Done', icon: <Check className="size-3.5" /> },
            ]}
          />
          {botRules.length > 1 && (
            <Select
              aria-label="Rule"
              className="w-48"
              value={rule}
              onChange={(e) => {
                setRule(Number(e.target.value))
                setLimit(PAGE)
              }}
            >
              <option value={0}>All rules</option>
              {botRules.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                </option>
              ))}
            </Select>
          )}
        </div>
      )}
      {runs.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-24" />
          <Skeleton className="h-24" />
        </div>
      ) : list.length === 0 ? (
        <p className="text-[13px] text-subtle">
          {filtered
            ? 'No runs match.'
            : 'No bot runs yet: open a pull request on an attached repo, or comment @waypoint review on one.'}
        </p>
      ) : (
        <>
          <ul className="space-y-2">
            {list.map((r) => (
              <BotRunCard key={r.id} run={r} canAct={canEdit(project)} project={project} />
            ))}
          </ul>
          {list.length >= limit && limit < 200 && (
            <Button size="sm" variant="ghost" onClick={() => setLimit((n) => n + PAGE)}>
              Show more
            </Button>
          )}
        </>
      )}
    </div>
  )
}

const BOT_STEPS = ['review', 'reply', 'fix_ci', 'summarize']

/** One bot run: why it ran, the models tried, what it found; also on the Agents page. */
export function BotRunCard({
  run: r,
  canAct,
  hidePR,
  project,
}: {
  run: BotRun
  /** May cancel or rerun it (editors of its project). */
  canAct: boolean
  /** The pull request is already the page's subject. */
  hidePR?: boolean
  /** Where its rule can be opened; the Agents page leaves it out. */
  project?: Project
}) {
  const qc = useQueryClient()
  const act = useMutation({
    mutationFn: (what: 'cancel' | 'retry') => api.post<BotRun>(`/bot-runs/${r.id}/${what}`),
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['botRuns'] })
      qc.invalidateQueries({ queryKey: ['pulls'] })
    },
  })
  const st = STATUS[r.status] ?? { label: r.status }
  const live = r.status === 'queued' || r.status === 'running'
  const findings = r.findings ?? []
  const shown = findings.filter((f) => f.placement !== 'withheld')
  const dropped = findings.filter((f) => f.state === 'dropped')
  return (
    <li className="rounded-xl border border-line bg-panel p-3 sm:p-4">
      <div className="flex items-start gap-2">
        <Badge color={st.color} className={clsx(r.status === 'running' && 'animate-pulse')}>
          {st.label}
        </Badge>
        <div className="min-w-0 flex-1">
          {!hidePR && (
            <a
              href={safeUrl(r.pr.url)}
              target="_blank"
              rel="noreferrer"
              className="block truncate text-[13px] font-medium hover:underline"
              title={r.pr.title}
            >
              <span className="font-mono text-xs text-muted">
                {r.pr.repo}#{r.pr.number}
              </span>{' '}
              {r.pr.title}
            </a>
          )}
          <div className={clsx('flex flex-wrap gap-x-2 text-xs text-subtle', !hidePR && 'mt-0.5')}>
            <span>
              {r.kind === 'review' ? 'Review' : r.kind} · {REASON[r.reason] ?? r.reason}
            </span>
            <span className="font-mono">{r.head_sha.slice(0, 7)}</span>
            {r.automation &&
              (project && r.automation_id ? (
                <Link
                  to="/p/$key/settings"
                  params={{ key: project.key }}
                  search={{ tab: 'automations', automation: r.automation_id }}
                  className="hover:text-fg hover:underline"
                >
                  {r.automation}
                </Link>
              ) : (
                <span>{r.automation}</span>
              ))}
            <span>{relativeTime(r.finished_at ?? r.started_at ?? r.created_at)}</span>
            <RunTime r={r} />
            {r.strategy && (
              <span title={r.prompt_version ? `prompt ${r.prompt_version}` : undefined}>
                {r.strategy}
                {r.verified ? ' · verified' : r.arm?.endsWith('/none') ? ' · no verifier' : ''}
              </span>
            )}
            {r.attempts > 1 && <span>{plural(r.attempts, 'attempt')}</span>}
          </div>
        </div>
      </div>

      {(r.summary || r.error) && (
        <p
          className={clsx(
            'mt-2 text-[13px]',
            r.status === 'failed' || (r.error && live) ? 'text-danger' : 'text-muted',
          )}
        >
          {r.summary || r.error}
          {live && r.error && ' Trying again shortly.'}
        </p>
      )}

      {shown.length > 0 && (
        <ul className="mt-2 space-y-1">
          {shown.map((f) => (
            <li key={f.id} className="flex items-start gap-2 text-[13px]">
              <Badge color={SEVERITY[f.severity]?.color} className="mt-px shrink-0">
                {SEVERITY[f.severity]?.badge ?? f.severity}
              </Badge>
              <span className="min-w-0">
                <span className="break-all font-mono text-xs text-muted">
                  {f.path}
                  {f.line ? `:${f.line}` : ''}
                </span>{' '}
                <span className="break-words text-fg">{f.claim}</span>
                <FindingMarks f={f} />
              </span>
            </li>
          ))}
        </ul>
      )}

      {dropped.length > 0 && (
        <details className="mt-2 text-xs text-subtle">
          <summary className="cursor-pointer select-none hover:text-fg">
            {plural(dropped.length, 'finding')} left out before posting
          </summary>
          <ul className="mt-1 space-y-1 pl-4">
            {dropped.map((f) => (
              <li key={f.id} className="min-w-0">
                <span className="break-all font-mono">
                  {f.path}
                  {f.line ? `:${f.line}` : ''}
                </span>{' '}
                <span className="break-words text-muted">{f.claim}</span>
                {(f.verify_reason || f.verified_by) && (
                  <span>
                    {' · '}
                    {f.verify_reason || 'dropped'}
                    {f.verified_by ? ` (${shortModel(f.verified_by)})` : ''}
                  </span>
                )}
              </li>
            ))}
          </ul>
        </details>
      )}

      <div className="mt-2 flex flex-wrap items-center gap-2">
        {r.log.length > 0 && (
          <details className="group min-w-0 basis-full text-xs text-subtle sm:flex-1 sm:basis-0">
            <summary className="cursor-pointer select-none truncate hover:text-fg">
              {modelSummary(r.log)}
            </summary>
            <ol className="mt-1 space-y-0.5 pl-4">
              {r.log.map((a, i) => (
                <li key={i} className="flex flex-wrap gap-x-2">
                  <span className="font-mono">{shortModel(a.model)}</span>
                  <span className={a.outcome === 'ok' ? 'text-ok' : undefined}>
                    {OUTCOME[a.outcome] ?? a.outcome}
                  </span>
                  {a.ms > 0 && <span>{seconds(a.ms)}</span>}
                  {a.error && <span className="break-words">{a.error}</span>}
                </li>
              ))}
            </ol>
          </details>
        )}
        <span className="ml-auto flex items-center gap-1">
          {r.review_url && (
            <a
              href={safeUrl(r.review_url)}
              target="_blank"
              rel="noreferrer"
              className="inline-flex h-6 items-center gap-1 rounded-md px-2 text-xs text-muted hover:bg-hover hover:text-fg"
            >
              <ExternalLink className="size-3" />
              Review
            </a>
          )}
          {canAct &&
            (live ? (
              <Button
                size="xs"
                variant="ghost"
                icon={<Square className="size-3" />}
                loading={act.isPending}
                onClick={() => act.mutate('cancel')}
              >
                Cancel
              </Button>
            ) : (
              <Button
                size="xs"
                variant="ghost"
                icon={<RotateCcw className="size-3" />}
                loading={act.isPending}
                onClick={() => act.mutate('retry')}
              >
                {r.status === 'done' ? 'Run again' : 'Retry'}
              </Button>
            ))}
        </span>
      </div>
    </li>
  )
}

/** "muse-spark-1.3 rate limited → nemotron-3-ultra answered" */
function modelSummary(log: ModelAttempt[]): string {
  const ok = log.filter((a) => a.outcome === 'ok')
  const failed = log.length - ok.length
  const used = [...new Set(ok.map((a) => shortModel(a.model)))]
  const parts = []
  if (used.length) parts.push(`Answered by ${used.join(', ')}`)
  if (failed) parts.push(`${plural(failed, 'model call')} failed`)
  return parts.join(' · ') || 'Models'
}

const seconds = (ms: number) => (ms < 1000 ? `${ms} ms` : `${Math.round(ms / 1000)} s`)

/** How long the run took (started to finished), or has been going. */
function RunTime({ r }: { r: BotRun }) {
  if (!r.started_at) return null
  const end = r.finished_at ? new Date(r.finished_at).getTime() : Date.now()
  const s = Math.max(0, Math.round((end - new Date(r.started_at).getTime()) / 1000))
  if (!r.finished_at && r.status !== 'running') return null
  if (r.finished_at && s === 0) return null
  return (
    <span title="From start to finish">
      {r.finished_at ? duration(s) : `running ${duration(s)}`}
    </span>
  )
}
