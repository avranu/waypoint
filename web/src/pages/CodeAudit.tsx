// One audit (WP-348): what was read and asked, how it went (each model
// tried, and why the next took over), the model's summary and its findings,
// each with a verdict people and agents record: confirmed, fixed, rejected.

import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowLeft,
  Check,
  CheckCheck,
  ExternalLink,
  FileCode2,
  GitPullRequest,
  MessageSquare,
  RotateCcw,
  RotateCw,
  SlidersHorizontal,
  SearchX,
  Square,
  Undo2,
  Wrench,
  X,
} from 'lucide-react'
import { dateTime, duration, plural, relativeTime, safeUrl } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { renderMarkdown } from '@/lib/markdown'
import {
  ACTIVE,
  auditDefaultsQuery,
  cancelAudit,
  codeAuditQuery,
  FINDING_STATE,
  fixAudit,
  findingToDo,
  markFindings,
  modelShort,
  outcomeText,
  MIN_SEVERITY,
  rerunAudit,
  SEVERITY,
  sinceText,
  TARGETS,
  type AuditFinding,
  type CodeAudit,
  type FindingState,
} from '@/lib/code-audits'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Badge, Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { AuditIcon, AuditStatusBadge, auditOutcome, SeverityChips } from '@/components/audits'
import { openAuditDialog, prefillFrom } from '@/components/AuditDialog'

const SEVERITY_ORDER = { critical: 0, high: 1, medium: 2, low: 3 }

export function CodeAuditPage() {
  const { id: raw } = useParams({ from: '/app/audits/$id' })
  const id = Number(raw)
  const audit = useQuery({
    ...codeAuditQuery(id),
    // Live events refresh it; this is the fallback while it runs.
    refetchInterval: (q) => (q.state.data && ACTIVE.includes(q.state.data.status) ? 15_000 : false),
  })
  const a = audit.data
  useEffect(() => {
    document.title = (a ? `Audit #${a.id}` : 'Audit') + ' · Waypoint'
  }, [a])
  const back = (
    <Link
      to="/agents"
      search={{ tab: 'audits' }}
      className="inline-flex items-center gap-1 text-[13px] text-muted hover:text-fg"
    >
      <ArrowLeft className="size-3.5" /> Audits
    </Link>
  )
  if (audit.isPending)
    return (
      <Page>
        <div className="space-y-3 px-4 pt-4 sm:px-6">
          <Skeleton className="h-8 w-2/3" />
          <Skeleton className="h-28" />
          <Skeleton className="h-64" />
        </div>
      </Page>
    )
  if (!a)
    return (
      <Page>
        <div className="px-4 pt-4 sm:px-6">{back}</div>
        <Empty icon={<SearchX />} title="Audit not found">
          It may belong to a project you can’t see.
        </Empty>
      </Page>
    )
  return (
    <Page>
      <div className="px-4 pt-4 sm:px-6">{back}</div>
      <PageHeader
        icon={<AuditIcon audit={a} className="!size-5" />}
        title={a.title}
        subtitle={<Subtitle audit={a} />}
        actions={<Actions audit={a} />}
      />
      <div className="space-y-4 px-4 sm:px-6">
        <StatusCard audit={a} />
        <Findings audit={a} />
        <Asked audit={a} />
      </div>
    </Page>
  )
}

function Page({ children }: { children: ReactNode }) {
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-4xl">{children}</div>
    </div>
  )
}

function Subtitle({ audit: a }: { audit: CodeAudit }) {
  const who = a.requested_by?.display_name || a.agent
  const parts = [`#${a.id}`, a.project_name || a.project]
  if (who) parts.push(`asked by ${who}`)
  else if (a.automation) parts.push(`asked by the automation ${a.automation}`)
  if (a.automation && who) parts.push(`for ${a.automation}`)
  parts.push(relativeTime(a.created_at))
  return <span title={dateTime(a.created_at)}>{parts.join(' · ')}</span>
}

function Actions({ audit: a }: { audit: CodeAudit }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [stopping, setStopping] = useState(false)
  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['audit', a.id] })
    qc.invalidateQueries({ queryKey: ['audits'] })
  }
  const cancel = useMutation({
    mutationFn: () => cancelAudit(a.id),
    onError: toastError,
    onSettled: refresh,
  })
  const rerun = useMutation({
    mutationFn: () => rerunAudit(a.id),
    onError: toastError,
    onSuccess: (b) => {
      refresh()
      toast(`Audit #${b.id} asked`)
      navigate({ to: '/audits/$id', params: { id: String(b.id) } })
    },
  })
  const fix = useMutation({
    mutationFn: () => fixAudit(a.id),
    onError: toastError,
    onSuccess: (b) => toast(`${b.fix_issue} is Claude’s task`),
    onSettled: refresh,
  })
  if (!a.can_edit) return null
  const active = ACTIVE.includes(a.status)
  return (
    <>
      {active ? (
        <Button
          size="sm"
          icon={<Square className="size-3.5" />}
          loading={cancel.isPending}
          onClick={() => setStopping(true)}
        >
          Cancel
        </Button>
      ) : (
        <>
          {a.status === 'done' && a.counts.open > 0 && !a.fix_issue && (
            <Button
              size="sm"
              icon={<Wrench className="size-3.5" />}
              loading={fix.isPending}
              onClick={() => fix.mutate()}
              title="A task for Claude to confirm each open finding, fix the real ones and reject the rest"
            >
              Fix with Claude
            </Button>
          )}
          <Button
            size="sm"
            icon={<RotateCw className="size-3.5" />}
            loading={rerun.isPending}
            onClick={() => rerun.mutate()}
            title={
              a.target === 'pr' ? 'Again, at the pull request’s head now' : 'Again, as a new audit'
            }
          >
            Run again
          </Button>
          <Button
            size="sm"
            icon={<SlidersHorizontal className="size-3.5" />}
            onClick={() => openAuditDialog(prefillFrom(a))}
            title="A new audit that starts from this one’s ask: change what it reads or looks for"
          >
            Edit and run again
          </Button>
        </>
      )}
      <Confirm
        open={stopping}
        onClose={() => setStopping(false)}
        title="Cancel this audit?"
        body="The model stops reading and nothing is reported."
        confirmLabel="Cancel audit"
        cancelLabel="Keep it"
        onConfirm={() => cancel.mutate()}
      />
    </>
  )
}

function StatusCard({ audit: a }: { audit: CodeAudit }) {
  const running = a.status === 'running'
  useTick(1000, running)
  let took = ''
  if (a.started_at) {
    const end = a.finished_at ? new Date(a.finished_at).getTime() : Date.now()
    took = duration((end - new Date(a.started_at).getTime()) / 1000)
  }
  const summary = useMemo(
    () => (a.summary ? renderMarkdown(a.summary, { untrusted: true }) : ''),
    [a.summary],
  )
  const links: ReactNode[] = []
  if (a.pr_url)
    links.push(
      <ExtLink key="pr" href={a.pr_url} icon={<GitPullRequest />}>
        {a.repo}#{a.pr_number}
      </ExtLink>,
    )
  if (a.comment_url)
    links.push(
      <ExtLink key="comment" href={a.comment_url} icon={<MessageSquare />}>
        Comment on the pull request
      </ExtLink>,
    )
  if (a.fix_issue)
    links.push(
      <Link
        key="fix"
        to="/issue/$key"
        params={{ key: a.fix_issue }}
        className="inline-flex items-center gap-1 text-accent hover:underline"
      >
        <Wrench className="size-3.5" /> Claude’s task {a.fix_issue}
      </Link>,
    )
  if (a.job_id)
    links.push(
      <Link
        key="job"
        to="/runner-jobs/$id"
        params={{ id: String(a.job_id) }}
        className="inline-flex items-center gap-1 text-muted hover:text-fg"
      >
        <FileCode2 className="size-3.5" /> Runner job
      </Link>,
    )
  return (
    <Section>
      <div className="space-y-3 p-4">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[13px]">
          <AuditStatusBadge status={a.status} />
          <span className="text-muted">{auditOutcome(a)}</span>
          {a.model && a.status !== 'running' && (
            <span className="text-subtle">
              by <span className="font-mono text-xs text-muted">{a.model}</span>
            </span>
          )}
          {took && (
            <span className="text-subtle">
              {running ? 'for' : 'took'} {took}
            </span>
          )}
          <SeverityChips counts={a.counts} />
        </div>
        {a.error && a.status !== 'done' && (
          <div
            className={clsx(
              'whitespace-pre-wrap rounded-lg border px-3 py-2 text-[13px]',
              a.status === 'cancelled'
                ? 'border-line bg-sunken text-muted'
                : a.status === 'limited'
                  ? 'border-warn/30 bg-warn/10'
                  : 'border-danger/30 bg-danger/10',
            )}
          >
            {a.error}
          </div>
        )}
        {summary && (
          <div
            className="prose-wp break-words text-[14px] leading-relaxed"
            // sanitised by renderMarkdown; the model's words, so untrusted
            dangerouslySetInnerHTML={{ __html: summary }}
          />
        )}
        {links.length > 0 && (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">{links}</div>
        )}
      </div>
    </Section>
  )
}

function ExtLink({ href, icon, children }: { href: string; icon: ReactNode; children: ReactNode }) {
  return (
    <a
      href={safeUrl(href)}
      target="_blank"
      rel="noreferrer"
      className="inline-flex items-center gap-1 text-accent hover:underline [&>svg]:size-3.5"
    >
      {icon}
      {children}
      <ExternalLink className="!size-3 opacity-60" />
    </a>
  )
}

type Show = 'open' | 'all'

function Findings({ audit: a }: { audit: CodeAudit }) {
  const findings = useMemo(
    () =>
      [...(a.findings ?? [])].sort(
        (x, y) => SEVERITY_ORDER[x.severity] - SEVERITY_ORDER[y.severity] || x.ordinal - y.ordinal,
      ),
    [a.findings],
  )
  const open = findings.filter(findingToDo)
  const [show, setShow] = useState<Show>('open')
  const [all, setAll] = useState(false)
  if (a.status !== 'done') return null
  const list = show === 'open' ? open : findings
  // The server says how many are listed at a time (shown); the rest are one
  // click away and counted (WP-1421). The To do tab follows the server's
  // rule (findingToDo in lib/code-audits.ts, which reads filing_void).
  const listed = all || !a.shown ? list : list.slice(0, a.shown)
  const more = list.length - listed.length
  return (
    <Section
      title={`Findings${findings.length ? ` (${findings.length})` : ''}`}
      actions={
        findings.length > 0 && (
          <SegmentedControl
            label="Show"
            value={show}
            onChange={setShow}
            textFrom="always"
            options={[
              { value: 'open', label: `To do (${open.length})`, icon: null },
              { value: 'all', label: 'All', icon: null },
            ]}
          />
        )
      }
    >
      {findings.length === 0 ? (
        <Empty icon={<CheckCheck />} title="Nothing found">
          The model read it and reported no defects. That isn’t proof there are none.
        </Empty>
      ) : list.length === 0 ? (
        <Empty icon={<CheckCheck />} title="Every finding is decided">
          Each one is fixed or rejected.{' '}
          <button className="text-accent hover:underline" onClick={() => setShow('all')}>
            Show them
          </button>
        </Empty>
      ) : (
        <ul className="divide-y divide-line">
          {listed.map((f) => (
            <FindingItem key={f.id} finding={f} audit={a} />
          ))}
        </ul>
      )}
      {more > 0 && (
        <div className="border-t border-line px-4 py-3">
          <Button size="sm" onClick={() => setAll(true)}>
            Show {more} more
          </Button>
        </div>
      )}
    </Section>
  )
}

// strengthText spells out how a rejection compared with the finder, stored as
// "verdict: reason": only the verdict (at_least, not_compared) is a name to
// spell out; the reason is shown as is (core.RejectedAs does the same).
function strengthText(s: string) {
  const at = s.indexOf(': ')
  const verdict = at < 0 ? s : s.slice(0, at)
  return verdict.replaceAll('_', ' ') + (at < 0 ? '' : s.slice(at))
}

function FindingItem({ finding: f, audit: a }: { finding: AuditFinding; audit: CodeAudit }) {
  const qc = useQueryClient()
  const [rejecting, setRejecting] = useState(false)
  const [note, setNote] = useState('')
  const mark = useMutation({
    mutationFn: ({ state, note }: { state: FindingState; note?: string }) =>
      markFindings([f.id], state, note),
    onError: toastError,
    onSuccess: () => {
      setRejecting(false)
      setNote('')
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['audit', a.id] })
      qc.invalidateQueries({ queryKey: ['audits'] })
    },
  })
  const problem = useMemo(() => renderMarkdown(f.problem, { untrusted: true }), [f.problem])
  const confirm = useMemo(
    () => (f.confirm ? renderMarkdown(f.confirm, { untrusted: true }) : ''),
    [f.confirm],
  )
  const decided = f.state === 'fixed' || f.state === 'rejected'
  const where = f.path ? `${f.path}${f.line ? `:${f.line}` : ''}` : ''
  return (
    <li className={clsx('space-y-2 px-4 py-3', decided && 'opacity-75')} id={`finding-${f.id}`}>
      <div className="flex flex-wrap items-start gap-x-2 gap-y-1">
        <Badge color={SEVERITY[f.severity].color}>{SEVERITY[f.severity].label}</Badge>
        <span className="min-w-0 flex-1 basis-60 text-[14px] font-medium [overflow-wrap:anywhere]">
          {f.title}
        </span>
        {f.state !== 'open' && (
          <Badge color={FINDING_STATE[f.state].color}>{FINDING_STATE[f.state].label}</Badge>
        )}
        <span className="font-mono text-[11px] text-subtle">{f.id}</span>
      </div>
      {where && (
        <div className="font-mono text-xs [overflow-wrap:anywhere]">
          {f.url ? (
            <a
              href={safeUrl(f.url)}
              target="_blank"
              rel="noreferrer"
              className="text-accent hover:underline"
            >
              {where}
            </a>
          ) : (
            <span className="text-muted">{where}</span>
          )}
        </div>
      )}
      <div
        className="prose-wp break-words text-[13px] leading-relaxed"
        dangerouslySetInnerHTML={{ __html: problem }}
      />
      {confirm && (
        <div className="rounded-lg bg-sunken px-3 py-2">
          <div className="mb-0.5 text-[11px] font-medium uppercase tracking-wide text-subtle">
            How to confirm it
          </div>
          <div
            className="prose-wp break-words text-[13px] leading-relaxed"
            dangerouslySetInnerHTML={{ __html: confirm }}
          />
        </div>
      )}
      {(f.note || f.by || f.issue) && f.state !== 'open' && (
        <p className="break-words text-xs text-muted">
          {FINDING_STATE[f.state].label}
          {f.by && ` by ${f.by}`}
          {f.at && ` ${relativeTime(f.at)}`}
          {f.note && <>: {f.note}</>}
          {f.evidence && <> · evidence: {f.evidence}</>}
          {f.decided_model && (
            <>
              {' '}
              · as {f.decided_model}
              {f.decided_effort && ` (${f.decided_effort})`}
              {f.decided_strength && `, ${strengthText(f.decided_strength)}`}
            </>
          )}
          {f.filing_void && f.state === 'confirmed' && (
            <> · its issue was canceled, so it is to do again</>
          )}
          {f.issue && (
            <>
              {' '}
              ·{' '}
              <Link
                to="/issue/$key"
                params={{ key: f.issue }}
                className="text-accent hover:underline"
              >
                {f.issue}
              </Link>
            </>
          )}
        </p>
      )}
      {a.can_edit &&
        (rejecting ? (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              mark.mutate({ state: 'rejected', note: note.trim() })
            }}
          >
            <Input
              autoFocus
              value={note}
              onChange={(e) => setNote(e.target.value)}
              placeholder="Why it isn’t a defect (optional)"
              maxLength={2000}
              className="!h-7 min-w-0 flex-1 basis-56 !text-[13px]"
              onKeyDown={(e) => e.key === 'Escape' && setRejecting(false)}
            />
            <Button size="sm" type="submit" variant="danger" loading={mark.isPending}>
              Reject
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setRejecting(false)}>
              Cancel
            </Button>
          </form>
        ) : (
          <div className="flex flex-wrap gap-1">
            {f.state === 'open' && (
              <Verdict
                icon={<Check />}
                label="Confirm"
                title="It’s real: still to fix"
                onClick={() => mark.mutate({ state: 'confirmed' })}
              />
            )}
            {!decided && (
              <>
                <Verdict
                  icon={<CheckCheck />}
                  label="Fixed"
                  onClick={() => mark.mutate({ state: 'fixed' })}
                />
                <Verdict
                  icon={<X />}
                  label="Reject"
                  title="It isn’t a defect"
                  onClick={() => setRejecting(true)}
                />
              </>
            )}
            {f.state !== 'open' && (
              <Verdict
                icon={f.state === 'confirmed' ? <RotateCcw /> : <Undo2 />}
                label="Reopen"
                onClick={() => mark.mutate({ state: 'open' })}
              />
            )}
          </div>
        ))}
    </li>
  )
}

function Verdict({
  icon,
  label,
  title,
  onClick,
}: {
  icon: ReactNode
  label: string
  title?: string
  onClick: () => void
}) {
  return (
    <Button
      size="xs"
      variant="ghost"
      icon={<span className="[&>svg]:size-3.5">{icon}</span>}
      title={title}
      onClick={onClick}
    >
      {label}
    </Button>
  )
}

function Asked({ audit: a }: { audit: CodeAudit }) {
  // The number is named only when it is the one in use: a check of marks and
  // a review round's audit list every finding whatever max_findings holds.
  const showsSet = !!a.max_findings && (!a.shown || a.shown === a.max_findings)
  const target = TARGETS.find((t) => t.value === a.target)
  const focusAreas = useQuery({ ...auditDefaultsQuery(), enabled: !!a.focus?.length }).data?.focus
  const what =
    a.target === 'recent'
      ? `${a.ref || 'the default branch'}, ${sinceText(a.since, a.since_at)}`
      : a.target === 'branch'
        ? `${a.ref} against ${a.base || 'the default branch'}`
        : a.target === 'range'
          ? `${a.base}..${a.ref}`
          : a.target === 'pr'
            ? `#${a.pr_number}`
            : a.ref || 'the default branch'
  return (
    <Section title="What it was asked">
      <dl className="grid gap-x-4 gap-y-3 p-4 text-[13px] sm:grid-cols-[10rem_1fr]">
        <dt className="text-subtle">Read</dt>
        <dd className="min-w-0 [overflow-wrap:anywhere]">
          {target?.label} <span className="font-mono text-xs text-muted">{what}</span> of{' '}
          <span className="font-mono text-xs">{a.repo}</span>
        </dd>
        {(a.from_sha || a.commit_count !== undefined) && (
          <>
            <dt className="text-subtle">Change read</dt>
            <dd className="min-w-0 [overflow-wrap:anywhere]">
              {a.from_sha && a.to_sha && (
                <span className="font-mono text-xs text-muted">
                  {a.from_sha === EMPTY_TREE ? 'the first commit' : short(a.from_sha)}..
                  {short(a.to_sha)}
                </span>
              )}
              {a.commit_count !== undefined && (
                <span className="text-muted">
                  {a.from_sha ? ' · ' : ''}
                  {plural(a.commit_count, 'commit')}
                </span>
              )}
              {a.since_at && (
                <span className="text-subtle" title={dateTime(a.since_at)}>
                  {' '}
                  since {dateTime(a.since_at)}
                </span>
              )}
            </dd>
          </>
        )}
        <dt className="text-subtle">To look for</dt>
        <dd className="min-w-0 whitespace-pre-wrap [overflow-wrap:anywhere]">{a.instructions}</dd>
        {!!a.focus?.length && (
          <>
            <dt className="text-subtle">Hardest at</dt>
            <dd className="flex min-w-0 flex-wrap gap-1">
              {a.focus.map((k) => {
                const f = focusAreas?.find((x) => x.key === k)
                return (
                  <span
                    key={k}
                    title={f?.text}
                    className="rounded-full border border-line px-2 py-0.5 text-xs"
                  >
                    {f?.label ?? k}
                  </span>
                )
              })}
            </dd>
          </>
        )}
        {!!a.paths?.length && (
          <>
            <dt className="text-subtle">Only</dt>
            <dd className="min-w-0">
              <PathList paths={a.paths} />
            </dd>
          </>
        )}
        {!!a.exclude?.length && (
          <>
            <dt className="text-subtle">Leaving out</dt>
            <dd className="min-w-0">
              <PathList paths={a.exclude} />
            </dd>
          </>
        )}
        {!!(a.min_severity || showsSet) && (
          <>
            <dt className="text-subtle">Report</dt>
            <dd>
              {[
                a.min_severity &&
                  MIN_SEVERITY.find((x) => x.value === a.min_severity)?.label.toLowerCase(),
                showsSet && `showing ${plural(a.max_findings ?? 0, 'finding')} at a time`,
              ]
                .filter(Boolean)
                .join(', ')
                .replace(/^./, (c) => c.toUpperCase())}
            </dd>
          </>
        )}
        <dt className="text-subtle">Models</dt>
        <dd className="min-w-0">
          <ol className="flex flex-wrap items-center gap-1">
            {a.models.map((m, i) => (
              <li key={i} className="flex items-center gap-1">
                {i > 0 && <span className="text-subtle">→</span>}
                <span className="rounded bg-sunken px-1.5 py-0.5 font-mono text-xs">{m}</span>
              </li>
            ))}
          </ol>
        </dd>
        <dt className="text-subtle">Then</dt>
        <dd>
          {[
            a.target === 'pr' && (a.post_comment ? 'post on the pull request' : 'no comment'),
            a.fix ? 'a task for Claude to check and fix' : 'no fixing',
          ]
            .filter(Boolean)
            .join(' · ')}
        </dd>
        {a.attempts.length > 0 && (
          <>
            <dt className="text-subtle">Tries</dt>
            <dd className="min-w-0">
              <ol className="space-y-1">
                {a.attempts.map((t, i) => (
                  <li key={i} className="flex flex-wrap items-baseline gap-x-1.5">
                    <span className="font-mono text-xs">{modelShort(t.model)}</span>
                    <span className={t.outcome === 'answered' ? 'text-ok' : 'text-muted'}>
                      {outcomeText(t.outcome)}
                    </span>
                    {t.round > 0 && <span className="text-subtle">(round {t.round + 1})</span>}
                    {t.job ? (
                      <Link
                        to="/runner-jobs/$id"
                        params={{ id: String(t.job) }}
                        className="text-xs text-subtle hover:text-fg"
                      >
                        job {t.job}
                      </Link>
                    ) : null}
                    {t.error && t.outcome !== 'answered' && (
                      <span className="basis-full text-xs text-subtle [overflow-wrap:anywhere]">
                        {t.error}
                      </span>
                    )}
                  </li>
                ))}
              </ol>
              {a.retries > 0 && (
                <p className="mt-1 text-xs text-subtle">
                  {plural(a.retries, 'round')} of retries after every model was busy.
                </p>
              )}
            </dd>
          </>
        )}
        {a.via && (
          <>
            <dt className="text-subtle">Asked from</dt>
            <dd>{VIA[a.via] ?? a.via}</dd>
          </>
        )}
      </dl>
    </Section>
  )
}

const short = (sha: string) => sha.slice(0, 10)
// git's empty tree: a period that starts before the branch's first commit.
const EMPTY_TREE = '4b825dc642cb6eb9a060e54bf8d69288fbee4904'

function PathList({ paths }: { paths: string[] }) {
  return (
    <ul className="flex flex-wrap gap-1">
      {paths.map((p) => (
        <li
          key={p}
          className="rounded bg-sunken px-1.5 py-0.5 font-mono text-xs [overflow-wrap:anywhere]"
        >
          {p}
        </li>
      ))}
    </ul>
  )
}

const VIA: Record<string, string> = {
  web: 'the web app',
  cli: 'wp audit',
  api: 'the API',
  automation: 'an automation',
}
