import { useDeferredValue, useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { RefreshCw, ScrollText, Search } from 'lucide-react'
import { gateAuditQuery, type Assistant } from '@/lib/assistant'
import { shortDate } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Empty, Skeleton } from '@/components/ui/misc'
import { auditTime, describe } from './kinds'
import { ErrorBox, EventRow, clockTime } from './shared'

/** Kinds the gatekeeper writes, for the picker before any are seen. */
const KNOWN = [
  'tool',
  'conn_tool',
  'conn',
  'conn_refused',
  'conn_failed',
  'waypoint',
  'waypoint_failed',
  'approval_requested',
  'approval_granted',
  'approval_denied',
  'approval_expired',
  'approved_action_done',
  'approved_action_failed',
  'freeze',
  'unfreeze',
  'connection_added',
  'connection_changed',
  'connection_updated',
  'connection_removed',
  'signal_control',
  'signal_send',
  'signal_dropped_stranger',
  'signal_rpc_refused',
  'notify_skipped',
  'notify_failed',
  'control',
  'control_refused',
  'hermes_api',
]

const PAGE = 100

export function AuditView({ assistant }: { assistant: Assistant }) {
  const [kind, setKind] = useState('')
  const [limit, setLimit] = useState(500)
  const [search, setSearch] = useState('')
  const [shown, setShown] = useState(PAGE)
  const q = useQuery({ ...gateAuditQuery(assistant.id, limit, kind), refetchInterval: 20_000 })
  const term = useDeferredValue(search.trim().toLowerCase())

  const rows = useMemo(() => {
    const out = (q.data ?? []).map((e, i) => ({ e, i, at: auditTime(e.ts) }))
    out.sort((a, b) => b.at - a.at || b.i - a.i)
    return term ? out.filter((r) => JSON.stringify(r.e).toLowerCase().includes(term)) : out
  }, [q.data, term])

  const kinds = useMemo(() => {
    const s = new Set(KNOWN)
    for (const e of q.data ?? []) s.add(e.kind)
    return [...s]
  }, [q.data])

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-0 flex-1 basis-56">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <Input
            type="search"
            aria-label="Search the audit"
            placeholder="Search"
            value={search}
            onChange={(e) => {
              setSearch(e.target.value)
              setShown(PAGE)
            }}
            className="pl-8"
          />
        </div>
        <Select
          aria-label="Kind"
          value={kind}
          onChange={(e) => {
            setKind(e.target.value)
            setShown(PAGE)
          }}
          className="!w-auto min-w-36 max-w-56"
        >
          <option value="">Every kind</option>
          {kinds.map((k) => (
            <option key={k} value={k}>
              {k}
            </option>
          ))}
        </Select>
        <Select
          aria-label="How many"
          value={String(limit)}
          onChange={(e) => setLimit(Number(e.target.value))}
          className="!w-auto"
        >
          <option value="200">Last 200</option>
          <option value="500">Last 500</option>
          <option value="2000">Last 2,000</option>
          <option value="5000">Last 5,000</option>
        </Select>
        <IconButton
          label="Refresh"
          icon={<RefreshCw className={clsx('size-4', q.isFetching && 'animate-spin')} />}
          onClick={() => q.refetch()}
        />
      </div>
      {q.isPending ? (
        <Skeleton className="h-64" />
      ) : q.isError ? (
        <ErrorBox what="the gate’s audit" error={q.error} />
      ) : rows.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<ScrollText />} title={term || kind ? 'No matches' : 'Nothing recorded'}>
            {term || kind
              ? 'Nothing in these entries matches. Try another kind, or look further back.'
              : undefined}
          </Empty>
        </div>
      ) : (
        <>
          <div className="text-xs text-subtle tabular-nums">
            {rows.length.toLocaleString()} {rows.length === 1 ? 'entry' : 'entries'}
          </div>
          <ul className="overflow-hidden rounded-xl border border-line bg-panel">
            {rows.slice(0, shown).map(({ e, i, at }) => {
              const d = describe(e)
              return (
                <EventRow
                  key={`${i}:${e.ts}`}
                  assistantId={assistant.id}
                  icon={d.icon}
                  tone={d.tone}
                  text={d.text}
                  sub={d.sub}
                  at={at}
                  timeLabel={at ? `${shortDate(new Date(at).toISOString())} ${clockTime(at)}` : ''}
                  details={e}
                  aside={
                    <span className="mt-1 hidden shrink-0 rounded bg-sunken px-1.5 font-mono text-[11px] text-subtle sm:inline">
                      {e.kind}
                    </span>
                  }
                />
              )
            })}
          </ul>
          {rows.length > shown && (
            <div className="flex justify-center">
              <Button size="sm" onClick={() => setShown(shown + PAGE)}>
                Show more ({rows.length - shown})
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  )
}
