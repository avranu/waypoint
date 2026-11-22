// Usage → Breakdowns: the same tokens by repository, machine, session, agent
// kind (main, Explore, each custom subagent) and model.

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { SquareArrowOutUpRight } from 'lucide-react'
import { Link } from '@tanstack/react-router'
import { usd } from '@/lib/activity'
import { relativeTime } from '@/lib/format'
import {
  fresh,
  pct,
  tok,
  usageBreakdownQuery,
  type UsageBreakdown,
  type UsageFilters,
  type UsageOverview,
} from '@/lib/usage-analytics'
import { Button } from '@/components/ui/button'
import { Section } from '@/components/ui/misc'
import { Segmented, SortTable, type Col } from './shared'

type By = 'repo' | 'machine' | 'session' | 'kind' | 'model'

const TABS: { id: By; label: string }[] = [
  { id: 'repo', label: 'Project' },
  { id: 'session', label: 'Session' },
  { id: 'kind', label: 'Agent' },
  { id: 'model', label: 'Model' },
  { id: 'machine', label: 'Machine' },
]

export function BreakdownSection({
  o,
  filters,
  pickSession,
}: {
  o: UsageOverview
  filters: UsageFilters
  /** Narrow the whole tab to one session. */
  pickSession: (session: string) => void
}) {
  const [by, setBy] = useState<By>('repo')
  const [more, setMore] = useState(false)
  const shown = {
    repo: o.repos,
    machine: o.machines,
    session: o.top_sessions,
    kind: o.kinds,
    model: o.models,
  }[by]
  const full = useQuery(
    usageBreakdownQuery(filters, by, 500, more && (by === 'session' || by === 'repo')),
  )
  const rows = more && full.data ? full.data : shown
  const canMore = (by === 'session' || by === 'repo') && !more && o.truncated
  return (
    <Section
      title="Where it went"
      actions={
        <Segmented
          label="Break down by"
          value={by}
          onChange={(v) => {
            setBy(v)
            setMore(false)
          }}
          options={TABS}
        />
      }
    >
      {rows.length === 0 ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">Nothing in this period.</p>
      ) : (
        <SortTable
          caption={`Usage by ${TABS.find((t) => t.id === by)?.label.toLowerCase()}`}
          rows={rows}
          rowKey={(r) => r.key}
          sort="fresh"
          onRow={by === 'session' ? (r) => pickSession(r.key) : undefined}
          cols={columns(by)}
        />
      )}
      {canMore && (
        <div className="border-t border-line px-4 py-2">
          <Button size="xs" variant="ghost" loading={full.isFetching} onClick={() => setMore(true)}>
            Show all
          </Button>
        </div>
      )}
    </Section>
  )
}

function columns(by: By): Col<UsageBreakdown>[] {
  const name: Col<UsageBreakdown> = {
    id: 'name',
    label: {
      repo: 'Repository',
      machine: 'Machine',
      session: 'Session',
      kind: 'Agent',
      model: 'Model',
    }[by],
    asc: true,
    value: (r) => r.label.toLowerCase(),
    cell: (r) => (
      <div className="min-w-0">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">
            {r.label}
            {by === 'kind' && r.key === 'main' && (
              <span className="ml-1 font-normal text-subtle">(the session itself)</span>
            )}
          </span>
          {by === 'session' && !!r.session_id && (
            <Link
              to="/agents/sessions/$id"
              params={{ id: String(r.session_id) }}
              onClick={(e) => e.stopPropagation()}
              className="inline-flex h-5 shrink-0 items-center gap-1 rounded-md border border-line px-1.5 text-[11px] font-normal text-muted hover:bg-hover hover:text-fg"
              title="Open this agent session"
            >
              <SquareArrowOutUpRight className="size-3" />
              Session
            </Link>
          )}
        </div>
        {(r.sub || r.project) && (
          <div className="truncate text-xs text-subtle">
            {r.project && (
              <Link
                to="/p/$key/board"
                params={{ key: r.project }}
                onClick={(e) => e.stopPropagation()}
                className="mr-1.5 text-accent hover:underline"
              >
                {r.project}
              </Link>
            )}
            {r.sub}
            {r.last_at && ` · ${relativeTime(r.last_at)}`}
            {!!r.compactions && ` · compacted ${r.compactions}×`}
          </div>
        )}
      </div>
    ),
  }
  return [
    name,
    { id: 'fresh', label: 'Fresh tokens', value: fresh, bar: true, cell: (r) => tok(fresh(r)) },
    {
      id: 'messages',
      label: 'API messages',
      value: (r) => r.messages,
      wide: true,
      cell: (r) => tok(r.messages),
    },
    {
      id: 'hit',
      label: 'Cache hits',
      value: (r) => r.hit_rate ?? -1,
      wide: true,
      cell: (r) => pct(r.hit_rate),
    },
    {
      id: 'cost',
      label: 'API cost',
      value: (r) => r.cost_usd,
      cell: (r) => (r.cost_usd > 0 || !r.unpriced_tokens ? usd(r.cost_usd) : '–'),
    },
  ]
}
