import { useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ArrowLeft, Copy, Lock, SearchX, Square } from 'lucide-react'
import { copyText } from '@/lib/clipboard'
import { dateTime, duration, plural, relativeTime } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import {
  CLI_LABELS,
  finished,
  KIND_LABELS,
  runnerJobQuery,
  useCancelJob,
  type RunnerJob,
} from '@/lib/runners'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { JobStateBadge, jobName } from '@/components/Runners'

const SOURCES: Record<string, string> = {
  request_cell: 'a request',
  automation_step: 'an automation',
  bot_run: 'a pull request bot',
  manual: 'by hand',
}

export function RunnerJobPage() {
  const { id: raw } = useParams({ from: '/app/runner-jobs/$id' })
  const id = Number(raw)
  const job = useQuery(runnerJobQuery(id))
  const cancel = useCancelJob()
  const [confirming, setConfirming] = useState(false)
  const j = job.data
  useEffect(() => {
    document.title = (j ? jobName(j) : 'Job') + ' · Waypoint'
  }, [j])
  const back = (
    <Link
      to="/settings"
      search={{ tab: 'runners' }}
      className="inline-flex items-center gap-1 text-[13px] text-muted hover:text-fg"
    >
      <ArrowLeft className="size-3.5" /> Runners
    </Link>
  )
  if (job.isPending) {
    return (
      <Page>
        <div className="space-y-3 px-4 sm:px-6">
          <Skeleton className="h-8 w-64" />
          <Skeleton className="h-24" />
          <Skeleton className="h-64" />
        </div>
      </Page>
    )
  }
  if (!j) {
    return (
      <Page>
        <div className="px-4 pt-4 sm:px-6">{back}</div>
        <Empty icon={<SearchX />} title="Job not found">
          It may belong to someone else, or it was removed after a month.
        </Empty>
      </Page>
    )
  }
  const active = !finished(j.state)
  return (
    <Page>
      <div className="px-4 pt-4 sm:px-6">{back}</div>
      <PageHeader
        title={jobName(j)}
        subtitle={<Subtitle job={j} />}
        actions={
          active &&
          !j.redacted && (
            <Button
              size="sm"
              icon={<Square className="size-3.5" />}
              loading={cancel.isPending}
              disabled={j.cancel_requested}
              onClick={() => setConfirming(true)}
            >
              {j.cancel_requested ? 'Stopping…' : 'Cancel'}
            </Button>
          )
        }
      />
      <div className="space-y-4 px-4 sm:px-6">
        {j.redacted ? (
          <Section>
            <Empty icon={<Lock />} title="What this job did is private">
              Only the person who started it and the owner of the runner can see its command, output
              and result.
            </Empty>
          </Section>
        ) : (
          <>
            <StatusCard job={j} />
            <ResultSection job={j} />
            <InputSection job={j} />
            <LogSection job={j} />
          </>
        )}
      </div>
      <Confirm
        open={confirming}
        onClose={() => setConfirming(false)}
        title="Cancel this job?"
        body={
          j.state === 'running'
            ? 'The runner stops it, and anything it is running with it.'
            : 'It was not started yet and will not run.'
        }
        confirmLabel="Cancel job"
        onConfirm={() => cancel.mutate(j.id)}
      />
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

function Subtitle({ job: j }: { job: RunnerJob }) {
  const parts = [KIND_LABELS[j.kind]]
  if (j.runner) parts.push(`on ${j.runner}`)
  if (j.created_by) parts.push(`started by ${j.created_by.display_name}`)
  if (j.source) parts.push(`from ${SOURCES[j.source] ?? j.source}`)
  return <>{parts.join(' · ')}</>
}

function StatusCard({ job: j }: { job: RunnerJob }) {
  const running = j.state === 'running'
  useTick(1000, running)
  let took = ''
  if (j.started_at) {
    const end = j.finished_at ? new Date(j.finished_at).getTime() : Date.now()
    took = duration((end - new Date(j.started_at).getTime()) / 1000)
  }
  return (
    <Section>
      <div className="space-y-3 p-4">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[13px]">
          <JobStateBadge state={j.state} />
          {running && <span className="text-muted">for {took}</span>}
          {j.state === 'queued' && (
            <span className="text-muted">{j.waiting || 'Waiting for a runner.'}</span>
          )}
          {finished(j.state) && took && <span className="text-muted">took {took}</span>}
          {j.exit_code != null && (
            <span className="text-muted">
              exit code <span className="font-mono text-fg">{j.exit_code}</span>
            </span>
          )}
          <span className="text-subtle" title={dateTime(j.created_at)}>
            {j.finished_at
              ? `finished ${relativeTime(j.finished_at)}`
              : `queued ${relativeTime(j.created_at)}`}
          </span>
        </div>
        {j.error && finished(j.state) && (
          <div className="rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-[13px] text-fg">
            {j.error}
          </div>
        )}
        {j.cancel_requested && active(j) && (
          <p className="text-xs text-muted">Cancelled by a person; the runner is stopping it.</p>
        )}
      </div>
    </Section>
  )
}

const active = (j: RunnerJob) => !finished(j.state)

const str = (v: unknown) => (typeof v === 'string' ? v : '')

function ResultSection({ job: j }: { job: RunnerJob }) {
  const r = j.result
  if (!r || Object.keys(r).length === 0) return null
  const answer = str(r.answer)
  const commits = Array.isArray(r.commits) ? (r.commits as string[]) : []
  const files = Array.isArray(r.files) ? (r.files as string[]) : []
  const conflicts = Array.isArray(r.conflicts)
    ? (r.conflicts as { path: string; reason: string }[])
    : []
  const tokens = r.tokens as { input?: number; output?: number; cost_usd?: number } | undefined
  // A plain command's result is its exit code, which the status line has.
  const worthShowing =
    answer ||
    files.length ||
    conflicts.length ||
    commits.length ||
    str(r.commit) ||
    str(r.branch) ||
    tokens?.input ||
    tokens?.output ||
    r.timed_out === true ||
    r.truncated === true
  if (!worthShowing) return null
  return (
    <Section title="Result">
      <div className="space-y-3 p-4 text-[13px]">
        {answer && (
          <div>
            <div className="mb-1 text-xs font-medium text-subtle">
              Answer{r.cli ? ` from ${CLI_LABELS[str(r.cli)] ?? str(r.cli)}` : ''}
            </div>
            <div className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken p-3 text-[13px] leading-relaxed">
              {answer}
            </div>
          </div>
        )}
        {files.length > 0 && (
          <Facts label={plural(files.length, 'file') + ' written'}>
            {files.map((f) => (
              <Code key={f}>{f}</Code>
            ))}
          </Facts>
        )}
        {conflicts.length > 0 && (
          <Facts label="Changed since this change was made">
            {conflicts.map((c) => (
              <Code key={c.path}>
                {c.path} ({c.reason})
              </Code>
            ))}
          </Facts>
        )}
        {str(r.branch) && (
          <Facts label="Branch">
            <Code>{str(r.branch)}</Code>
            {r.pushed === true && <span className="text-muted">pushed</span>}
          </Facts>
        )}
        {(commits.length > 0 || str(r.commit)) && (
          <Facts label={commits.length > 1 ? 'Commits' : 'Commit'}>
            {(commits.length > 0 ? commits : [str(r.commit)]).map((c) => (
              <Code key={c}>{c.slice(0, 12)}</Code>
            ))}
            {r.pushed === true && !str(r.branch) && <span className="text-muted">pushed</span>}
          </Facts>
        )}
        {tokens && (tokens.input || tokens.output) ? (
          <Facts label="Used">
            <span className="text-muted">
              {(tokens.input ?? 0).toLocaleString()} tokens in,{' '}
              {(tokens.output ?? 0).toLocaleString()} out
              {tokens.cost_usd ? ` · about $${tokens.cost_usd.toFixed(2)} at list prices` : ''}
            </span>
          </Facts>
        ) : null}
        {r.timed_out === true && <p className="text-muted">It ran out of time and was stopped.</p>}
        {r.truncated === true && str(r.tail) && (
          <div>
            <div className="mb-1 text-xs font-medium text-subtle">
              The output was cut at 1 MB; this is its end
            </div>
            <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken p-3 font-mono text-xs">
              {str(r.tail)}
            </pre>
          </div>
        )}
        <details className="text-xs">
          <summary className="cursor-pointer select-none text-subtle hover:text-fg">
            Raw result
          </summary>
          <pre className="mt-2 max-h-72 overflow-auto rounded-lg bg-sunken p-3 font-mono text-xs">
            {JSON.stringify(r, null, 2)}
          </pre>
        </details>
      </div>
    </Section>
  )
}

function Facts({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <span className="w-40 shrink-0 text-xs text-subtle">{label}</span>
      <div className="flex min-w-0 flex-wrap items-center gap-1.5">{children}</div>
    </div>
  )
}

function Code({ children }: { children: ReactNode }) {
  return (
    <code className="max-w-full break-all rounded bg-sunken px-1.5 py-0.5 font-mono text-[11px]">
      {children}
    </code>
  )
}

/** What the job was asked to do. */
function InputSection({ job: j }: { job: RunnerJob }) {
  const i = j.input
  if (!i) return null
  const files = Array.isArray(i.files) ? (i.files as { path: string; delete?: boolean }[]) : []
  return (
    <Section title="What was asked">
      <div className="space-y-3 p-4 text-[13px]">
        {str(i.command) && (
          <pre className="overflow-x-auto whitespace-pre-wrap break-words rounded-lg bg-sunken p-3 font-mono text-xs">
            $ {str(i.command)}
          </pre>
        )}
        {str(i.prompt) && (
          <div className="max-h-60 overflow-auto whitespace-pre-wrap break-words rounded-lg bg-sunken p-3 text-[13px] leading-relaxed">
            {str(i.prompt)}
          </div>
        )}
        {str(i.dir) && (
          <Facts label="Folder">
            <Code>{str(i.dir)}</Code>
          </Facts>
        )}
        {files.length > 0 && (
          <Facts label={plural(files.length, 'file')}>
            {files.slice(0, 30).map((f) => (
              <Code key={f.path}>
                {f.delete ? 'delete ' : ''}
                {f.path}
              </Code>
            ))}
            {files.length > 30 && <span className="text-muted">and {files.length - 30} more</span>}
          </Facts>
        )}
        {str(i.commit) && (
          <Facts label="Commit message">
            <span className="text-muted">{str(i.commit)}</span>
          </Facts>
        )}
        {str(i.repo) && (
          <Facts label="Repository">
            <Code>{str(i.repo)}</Code>
            {str(i.base) && <span className="text-muted">from {str(i.base)}</span>}
            {str(i.branch) && <span className="text-muted">to {str(i.branch)}</span>}
          </Facts>
        )}
        {str(i.model) && (
          <Facts label="Model">
            <Code>{str(i.model)}</Code>
          </Facts>
        )}
        {i.read_only === true && (
          <p className="text-muted">Read-only: it may look but not change.</p>
        )}
        <p className="text-xs text-subtle">Stops after {duration(j.timeout)} if still running.</p>
      </div>
    </Section>
  )
}

const ANSI = /\x1b\[[0-9;?]*[A-Za-z]/g // eslint-disable-line no-control-regex

/** Terminal output as plain lines: no colour codes, and a progress bar's redraws show their last state. */
export function cleanLog(text: string): string {
  return text
    .replace(ANSI, '')
    .split('\n')
    .map((l) => l.replace(/\r+$/, ''))
    .map((l) => (l.includes('\r') ? l.slice(l.lastIndexOf('\r') + 1) : l))
    .join('\n')
}

function LogSection({ job: j }: { job: RunnerJob }) {
  const text = cleanLog(j.log ?? '')
  const ref = useRef<HTMLPreElement>(null)
  const stick = useRef(true)
  // Follow the output while it grows, unless the reader scrolled up.
  useLayoutEffect(() => {
    const el = ref.current
    if (el && stick.current) el.scrollTop = el.scrollHeight
  }, [text])
  return (
    <Section
      title="Output"
      actions={
        text && (
          <Button
            size="xs"
            variant="ghost"
            icon={<Copy className="size-3" />}
            onClick={() => copyText(text).then(() => toast('Output copied'), toastError)}
          >
            Copy
          </Button>
        )
      }
    >
      {text ? (
        <pre
          ref={ref}
          tabIndex={0}
          aria-label="Job output"
          onScroll={(e) => {
            const el = e.currentTarget
            stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
          }}
          data-testid="job-log"
          className="max-h-[60vh] min-h-24 overflow-auto whitespace-pre-wrap break-words p-4 font-mono text-xs leading-relaxed"
        >
          {text}
        </pre>
      ) : (
        <p className="p-4 text-[13px] text-muted">
          {j.state === 'queued'
            ? 'Nothing yet: the job has not started.'
            : j.state === 'running'
              ? 'No output yet.'
              : 'It printed nothing.'}
        </p>
      )}
      {j.log_size >= 1 << 20 && (
        <p className="border-t border-line px-4 py-2 text-xs text-subtle">
          Only the first 1 MB of output is kept.
        </p>
      )}
    </Section>
  )
}
