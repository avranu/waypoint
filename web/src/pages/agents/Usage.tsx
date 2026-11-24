import clsx from 'clsx'
import { useQuery } from '@tanstack/react-query'
import { Gauge, TriangleAlert, X } from 'lucide-react'
import { relativeTime } from '@/lib/format'
import { usageOverviewQuery, type UsageMachine, type UsageOverview } from '@/lib/usage-analytics'
import { Select } from '@/components/ui/input'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import type { HubTabProps } from './search'
import { QuotaSection } from './usage/Quota'
import { SuggestionsSection } from './usage/Suggestions'
import { TokensSection } from './usage/Tokens'
import { LedgerSection } from './usage/Ledger'
import { BreakdownSection } from './usage/Breakdowns'
import { BashSection, ToolsSection } from './usage/Tools'
import { RefusalsSection } from './usage/Refusals'
import { CopyButton } from './usage/shared'

export function UsageTab({ filters, setFilters }: HubTabProps) {
  const uq = useQuery(usageOverviewQuery(filters))
  const o = uq.data
  if (uq.error)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<Gauge />} title="Couldn’t load usage">
          {uq.error instanceof Error ? uq.error.message : String(uq.error)}
        </Empty>
      </div>
    )
  if (!o) return <UsageSkeleton />
  const narrowed = !!(filters.agent || filters.project || filters.machine || filters.session)
  if (!o.has_data && !narrowed) return <HowToStart />
  const quotaShown = !!o.quota && o.quota.windows.length > 0 && !filters.session
  const suggestions = o.suggestions.filter((a) => !(quotaShown && a.kind === 'quota'))
  const sessionTitle = o.top_sessions.find((s) => s.key === filters.session)?.label
  return (
    <div className={clsx('space-y-4 transition-opacity', uq.isPlaceholderData && 'opacity-60')}>
      <ExtraFilters o={o} filters={filters} setFilters={setFilters} sessionTitle={sessionTitle} />
      {o.quota && o.quota.windows.length > 0 && !filters.session && (
        <QuotaSection
          windows={o.quota.windows}
          statusAge={latest(o.reporting.map((m) => m.last_status_at))}
        />
      )}
      {suggestions.length > 0 && <SuggestionsSection advice={suggestions} />}
      {o.totals.messages === 0 && o.tools.length === 0 && o.refusals.total === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<Gauge />} title={`No usage in the last ${filters.days} days`}>
            {narrowed
              ? 'Nothing matches these filters.'
              : 'Sessions that ran in this period will show up after the next sync.'}
          </Empty>
        </div>
      ) : (
        <>
          <TokensSection o={o} />
          <LedgerSection filters={filters} />
          <BreakdownSection
            o={o}
            filters={filters}
            pickSession={(session) => setFilters({ session })}
          />
          <ToolsSection o={o} />
          <BashSection o={o} />
          <RefusalsSection key={JSON.stringify(filters)} o={o} filters={filters} />
        </>
      )}
      <MachinesSection machines={o.reporting} />
    </div>
  )
}

function latest(list: (string | null)[]): string | null {
  return (
    list
      .filter((x): x is string => !!x)
      .sort()
      .pop() ?? null
  )
}

/** Machine, session and (for admins) person, beside the page's own filters. */
function ExtraFilters({
  o,
  filters,
  setFilters,
  sessionTitle,
}: HubTabProps & { o: UsageOverview; sessionTitle?: string }) {
  const machines = o.reporting.map((m) => m.name)
  if (filters.machine && !machines.includes(filters.machine)) machines.push(filters.machine)
  const people = o.people ?? []
  if (
    machines.length < 2 &&
    !filters.machine &&
    !filters.session &&
    people.length < 2 &&
    !filters.who
  )
    return null
  return (
    <div className="flex flex-wrap items-center gap-2">
      {(machines.length > 1 || filters.machine) && (
        <Select
          aria-label="Machine"
          value={filters.machine ?? ''}
          onChange={(e) => setFilters({ machine: e.target.value || undefined })}
          className="!w-auto min-w-32 max-w-56"
        >
          <option value="">All machines</option>
          {machines.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </Select>
      )}
      {(people.length > 1 || filters.who) && (
        <Select
          aria-label="Person"
          value={filters.who ?? ''}
          onChange={(e) => setFilters({ who: e.target.value || undefined, session: undefined })}
          className="!w-auto min-w-32 max-w-56"
        >
          <option value="">Me</option>
          <option value="all">Everyone</option>
          {people.map((p) => (
            <option key={p.username} value={p.username}>
              {p.display_name || p.username}
            </option>
          ))}
        </Select>
      )}
      {filters.session && (
        <span className="inline-flex h-8 max-w-full items-center gap-1.5 rounded-lg border border-accent/50 bg-accent-soft px-2.5 text-[13px]">
          <span className="truncate">Session: {sessionTitle ?? filters.session.slice(0, 8)}</span>
          <button
            aria-label="Show every session"
            onClick={() => setFilters({ session: undefined })}
            className="-mr-1 rounded p-0.5 text-subtle hover:bg-hover hover:text-fg"
          >
            <X className="size-3" />
          </button>
        </span>
      )}
    </div>
  )
}

function HowToStart() {
  return (
    <div className="rounded-xl border border-dashed border-line">
      <Empty icon={<Gauge />} title="No usage reported yet">
        <p>
          Usage is read from Claude Code’s own session transcripts and status line by{' '}
          <code>wp</code>, on each machine you use it on. Nothing is asked of the model and no
          prompts or replies are uploaded.
        </p>
        <ol className="mt-3 space-y-2 text-left">
          <Step n={1} cmd="wp hook claude">
            keeps it current: uploads after each turn and reports your quota from the status line
            (your own status line keeps working)
          </Step>
          <Step n={2} cmd="wp usage sync --all">
            back-fills the sessions you already have (<code>--dry-run</code> first shows what would
            be sent)
          </Step>
          <Step n={3} cmd="wp usage otel --install">
            optional, for machines whose transcripts <code>wp</code> can’t read: sends Claude Code’s
            OpenTelemetry events instead (a synced transcript always wins, so nothing counts twice)
          </Step>
        </ol>
      </Empty>
    </div>
  )
}

function Step({ n, cmd, children }: { n: number; cmd: string; children: React.ReactNode }) {
  return (
    <li className="flex gap-2">
      <span className="mt-0.5 flex size-5 shrink-0 items-center justify-center rounded-full bg-sunken text-xs font-medium">
        {n}
      </span>
      <div className="min-w-0">
        <div className="flex items-center gap-1">
          <code className="rounded bg-sunken px-1.5 py-0.5 font-mono text-xs">{cmd}</code>
          <CopyButton text={cmd} />
        </div>
        <div className="text-xs text-muted">{children}</div>
      </div>
    </li>
  )
}

/** When each machine last reported, and what its parser couldn't read. */
function MachinesSection({ machines }: { machines: UsageMachine[] }) {
  if (machines.length === 0) return null
  return (
    <Section
      title="Machines"
      helpId="usage:machines"
      help={
        <p>
          Usage is only as complete as the machines that report it. A machine that hasn’t synced
          lately is missing recent sessions. <b>Unread lines</b> are transcript lines this version
          of <code>wp</code> doesn’t understand; Claude Code’s format changes, so a growing number
          means <code>wp</code> needs updating.
        </p>
      }
    >
      <ul className="divide-y divide-line">
        {machines.map((m) => {
          const stale =
            !m.last_sync_at || Date.now() - new Date(m.last_sync_at).getTime() > 36 * 3600_000
          const kinds = Object.entries(m.unknown)
            .sort((a, b) => b[1] - a[1])
            .slice(0, 4)
          return (
            <li
              key={m.name}
              className="flex flex-wrap items-baseline gap-x-4 gap-y-0.5 px-4 py-2 text-[13px]"
            >
              <span className="font-medium">{m.name}</span>
              <span className={clsx('flex items-center gap-1', stale ? 'text-warn' : 'text-muted')}>
                {stale && <TriangleAlert className="size-3.5" />}
                {m.last_sync_at ? `synced ${relativeTime(m.last_sync_at)}` : 'never synced'}
              </span>
              {m.last_status_at && (
                <span className="text-muted">status line {relativeTime(m.last_status_at)}</span>
              )}
              {m.wp_version && <span className="text-xs text-subtle">wp {m.wp_version}</span>}
              {m.skipped > 0 && (
                <span
                  className="text-xs text-subtle"
                  title={kinds.map(([k, n]) => `${k} ×${n}`).join(', ')}
                >
                  {m.skipped.toLocaleString()} unread lines
                  {kinds.length ? ` (${kinds.map(([k]) => k).join(', ')})` : ''}
                </span>
              )}
            </li>
          )
        })}
      </ul>
    </Section>
  )
}

function UsageSkeleton() {
  return (
    <div className="space-y-4" aria-busy>
      <Skeleton className="h-40" />
      <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        {Array.from({ length: 4 }, (_, i) => (
          <Skeleton key={i} className="h-[74px]" />
        ))}
      </div>
      <Skeleton className="h-64" />
      <Skeleton className="h-48" />
    </div>
  )
}
