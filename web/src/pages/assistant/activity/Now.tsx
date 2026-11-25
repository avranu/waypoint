import { useMemo, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  CalendarClock,
  CircleAlert,
  Compass,
  Hand,
  HelpCircle,
  History,
  MessageSquareText,
  Moon,
  ShieldQuestion,
} from 'lucide-react'
import {
  approvalsQuery,
  gateAuditQuery,
  hermesQuery,
  iso,
  sessionTitle,
  sourceLabel,
  type Assistant,
  type AuditEntry,
  type HermesSession,
  type SessionPage,
} from '@/lib/assistant'
import { useAssistantLive } from '@/lib/assistant-live'
import { plural, relativeTime } from '@/lib/format'
import { Button, ToggleButton } from '@/components/ui/button'
import { Empty, Skeleton } from '@/components/ui/misc'
import { GROUPS, auditTime, describe, type Group, type Tone } from './kinds'
import { Chips, DayHeading, ErrorBox, EventRow, SourceIcon, byDay } from './shared'

interface CronJob {
  id: string
  name: string
  no_agent?: boolean
  last_run_at: string | null
  last_status: string | null
  last_error: string | null
  schedule_display?: string
}

interface Item {
  key: string
  at: number
  group: Group
  icon: ReactNode
  tone: Tone
  text: ReactNode
  sub?: ReactNode
  details?: unknown
  chat?: string
}

const END_REASON: Record<string, string> = {
  tui_close: 'closed in the terminal',
  ws_disconnect: 'connection closed',
  idle_timeout: 'idle',
  session_reset: 'reset',
  compression: 'continued in a compressed conversation',
  branched: 'branched',
  cron_complete: 'finished',
  cron_incomplete_no_output: 'ended without output',
}

const endReason = (r: string | null) =>
  !r ? '' : (END_REASON[r] ?? (r.startsWith('ws_') ? 'connection closed' : r.replace(/_/g, ' ')))

function counts(s: HermesSession) {
  const parts = [plural(s.message_count, 'message')]
  if (s.tool_call_count) parts.push(plural(s.tool_call_count, 'tool call'))
  return parts.join(' · ')
}

function duration(sec: number) {
  if (sec < 60) return `${Math.max(1, Math.round(sec))}s`
  if (sec < 3600) return `${Math.round(sec / 60)} min`
  return `${(sec / 3600).toFixed(1)} h`
}

/** Conversations and scheduled runs as timeline items. */
function sessionItems(sessions: HermesSession[]): Item[] {
  const out: Item[] = []
  for (const s of sessions) {
    const title = sessionTitle(s)
    if (s.source === 'cron') {
      const status = s.is_active
        ? 'running'
        : s.ended_at
          ? `${endReason(s.end_reason) || 'finished'} in ${duration(s.ended_at - s.started_at)}`
          : ''
      out.push({
        key: `run:${s.id}`,
        at: s.started_at * 1000,
        group: 'schedules',
        icon: <CalendarClock />,
        tone: s.end_reason === 'cron_incomplete_no_output' ? 'warn' : 'accent',
        text: (
          <>
            Scheduled run <span className="font-medium">{title}</span>
          </>
        ),
        sub: [status, counts(s)].filter(Boolean).join(' · '),
        chat: s.id,
      })
      continue
    }
    const where = sourceLabel(s.source || 'cli')
    out.push({
      key: `start:${s.id}`,
      at: s.started_at * 1000,
      group: 'conversations',
      icon: <SourceIcon source={s.source} />,
      tone: 'accent',
      text: (
        <>
          Conversation on {where}: <span className="font-medium">{title}</span>
        </>
      ),
      sub: counts(s) + (s.model ? ` · ${s.model}` : ''),
      chat: s.id,
    })
    if (s.ended_at)
      out.push({
        key: `end:${s.id}`,
        at: s.ended_at * 1000,
        group: 'conversations',
        icon: <MessageSquareText />,
        tone: 'muted',
        text: (
          <>
            Ended <span className="font-medium">{title}</span>
          </>
        ),
        sub: [endReason(s.end_reason), counts(s)].filter(Boolean).join(' · '),
        chat: s.id,
      })
  }
  return out
}

/** Script-only schedules and failed runs, which have no conversation. */
function cronItems(jobs: CronJob[], sessions: HermesSession[]): Item[] {
  const out: Item[] = []
  for (const j of jobs) {
    if (!j.last_run_at) continue
    const failed = !!j.last_status && j.last_status !== 'ok'
    if (!j.no_agent && !failed) continue
    const at = Date.parse(j.last_run_at)
    if (
      !failed &&
      sessions.some(
        (s) => s.id.startsWith(`cron_${j.id}_`) && Math.abs(s.started_at * 1000 - at) < 300_000,
      )
    )
      continue
    out.push({
      key: `cron:${j.id}:${j.last_run_at}`,
      at,
      group: 'schedules',
      icon: failed ? <CircleAlert /> : <CalendarClock />,
      tone: failed ? 'danger' : 'accent',
      text: (
        <>
          Schedule <span className="font-medium">{j.name}</span>{' '}
          {failed ? (j.last_status === 'delivery_failed' ? 'couldn’t deliver' : 'failed') : 'ran'}
        </>
      ),
      sub: j.last_error || j.schedule_display,
      details: failed ? { status: j.last_status, error: j.last_error } : undefined,
    })
  }
  return out
}

function auditItems(entries: AuditEntry[]): Item[] {
  return entries.map((e, i) => {
    const d = describe(e)
    return {
      key: `audit:${i}:${e.ts}`,
      at: auditTime(e.ts),
      group: d.group,
      icon: d.icon,
      tone: d.tone,
      text: d.text,
      sub: d.sub,
      details: e,
    }
  })
}

const PAGE = 60

export function NowView({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const up = assistant.state.reachable && !assistant.state.frozen
  const sessions = useQuery({
    ...hermesQuery<SessionPage>(
      id,
      'sessions',
      { limit: 100, order: 'recent', archived: 'include' },
      10_000,
    ),
    refetchInterval: 60_000,
    enabled: up,
    retry: false,
  })
  const jobs = useQuery({
    ...hermesQuery<CronJob[]>(id, 'cron/jobs'),
    enabled: up,
    retry: false,
  })
  const approvals = useQuery({ ...approvalsQuery(id, 50), refetchInterval: 10_000 })
  const audit = useQuery({ ...gateAuditQuery(id, 1000), refetchInterval: 15_000 })
  const live = useAssistantLive(id)

  const [group, setGroup] = useState<Group | ''>('')
  const [showWaypoint, setShowWaypoint] = useState(false)
  const [shown, setShown] = useState(PAGE)

  const all = useMemo(() => {
    const ss = sessions.data?.sessions ?? []
    const items = [
      ...sessionItems(ss),
      ...cronItems(jobs.data ?? [], ss),
      ...auditItems(audit.data ?? []),
    ]
    return items.sort((a, b) => b.at - a.at)
  }, [sessions.data, jobs.data, audit.data])

  const visible = all.filter((it) => showWaypoint || it.group !== 'waypoint')
  const counts = new Map<string, number>()
  for (const it of visible) counts.set(it.group, (counts.get(it.group) ?? 0) + 1)
  const filtered = group ? visible.filter((it) => it.group === group) : visible
  const days = byDay(filtered.slice(0, shown), (it) => it.at)
  const olderUnloaded =
    (sessions.data ? sessions.data.total > sessions.data.sessions.length : false) ||
    (audit.data?.length ?? 0) >= 1000
  const hiddenWaypoint = showWaypoint ? 0 : all.length - visible.length

  const active = (sessions.data?.sessions ?? []).filter((s) => s.is_active)
  const pending = approvals.data?.pending ?? assistant.state.pending
  const loading = (up && sessions.isPending) || audit.isPending

  const chips = [
    { id: '' as const, label: 'All', count: visible.length },
    ...GROUPS.filter((g) => counts.get(g.id) || group === g.id).map((g) => ({
      id: g.id,
      label: g.label,
      count: counts.get(g.id) ?? 0,
    })),
    ...(showWaypoint
      ? [{ id: 'waypoint' as const, label: 'Waypoint', count: counts.get('waypoint') ?? 0 }]
      : []),
  ]

  return (
    <div className="space-y-6">
      <div className="grid gap-4 lg:grid-cols-2">
        <Panel title="Running now" count={active.length}>
          {up && sessions.isPending ? (
            <Skeleton className="m-3 h-10" />
          ) : !up ? (
            <Quiet icon={<Moon />}>
              {assistant.state.frozen ? 'Frozen: nothing can run.' : 'Not answering right now.'}
            </Quiet>
          ) : sessions.isError ? (
            <Quiet icon={<CircleAlert />}>Couldn’t list its conversations.</Quiet>
          ) : active.length === 0 ? (
            <Quiet icon={<Moon />}>Nothing running.</Quiet>
          ) : (
            <ul>
              {active.map((s) => (
                <ActiveRow key={s.id} s={s} assistantId={id} />
              ))}
            </ul>
          )}
        </Panel>
        <Panel title="Waiting on you" count={pending.length + live.open.length} warn>
          {pending.length === 0 && live.open.length === 0 ? (
            <Quiet icon={<Hand />}>Nothing waiting.</Quiet>
          ) : (
            <ul>
              {pending.map((a) => (
                <li
                  key={a.code}
                  className="flex items-start gap-3 border-b border-line px-3 py-2.5 last:border-b-0 sm:px-4"
                >
                  <span className="mt-0.5 shrink-0 rounded bg-warn/15 px-1.5 font-mono text-xs font-semibold leading-5 text-warn">
                    {a.code}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="text-[13px] leading-5 [overflow-wrap:anywhere]">
                      {a.summary}
                    </div>
                    <div className="mt-0.5 text-xs text-subtle">
                      <span className="font-mono">{a.tool}</span> · asked{' '}
                      {relativeTime(iso(a.created))}
                    </div>
                  </div>
                </li>
              ))}
              {live.open.map((r) => (
                <li key={r.id} className="border-b border-line last:border-b-0">
                  <Link
                    to="/assistant/$id"
                    params={{ id: String(id) }}
                    search={{ s: r.params.session_id }}
                    className="flex min-h-11 items-start gap-3 px-3 py-2.5 hover:bg-hover sm:px-4"
                  >
                    {r.method === 'approval' ? (
                      <ShieldQuestion className="mt-0.5 size-4 shrink-0 text-warn" />
                    ) : (
                      <HelpCircle className="mt-0.5 size-4 shrink-0 text-accent" />
                    )}
                    <div className="min-w-0 flex-1">
                      <div className="line-clamp-2 text-[13px] leading-5 [overflow-wrap:anywhere]">
                        {r.method === 'approval'
                          ? r.params.description || `Run ${r.params.tool_name ?? 'a command'}?`
                          : (r.params.questions?.[0]?.question ?? 'A question')}
                      </div>
                      <div className="mt-0.5 text-xs text-accent">Answer in the chat</div>
                    </div>
                  </Link>
                </li>
              ))}
              {pending.length > 0 && (
                <li className="bg-sunken/60 px-3 py-2 text-xs text-muted sm:px-4">
                  Approve or deny on Signal: reply “approve {pending[0].code}” or “deny{' '}
                  {pending[0].code}”.
                </li>
              )}
            </ul>
          )}
        </Panel>
      </div>

      <section className="space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="mr-1 flex items-center gap-1.5 text-[13px] font-semibold">
            <History className="size-4 text-subtle" />
            Timeline
          </h2>
          <span className="flex-1 sm:hidden" />
          <div className="order-last min-w-0 basis-full sm:order-none sm:basis-auto sm:flex-1">
            <Chips
              label="Show"
              options={chips}
              value={group}
              onChange={(g) => {
                setGroup(g)
                setShown(PAGE)
              }}
            />
          </div>
          <ToggleButton
            label="Waypoint’s own calls"
            hint="Waypoint’s requests through the gate to the agent and the gate"
            pressed={showWaypoint}
            onPressedChange={(v) => {
              setShowWaypoint(v)
              if (!v && group === 'waypoint') setGroup('')
            }}
            icon={<Compass className="size-3.5" />}
            text="Waypoint’s calls"
            textFrom="sm"
          />
        </div>
        {audit.isError && <ErrorBox what="the gate’s audit" error={audit.error} />}
        {loading ? (
          <div className="space-y-2">
            <Skeleton className="h-4 w-24" />
            <Skeleton className="h-48" />
          </div>
        ) : filtered.length === 0 ? (
          <div className="rounded-xl border border-dashed border-line">
            <Empty icon={<History />} title="Nothing yet">
              {group || hiddenWaypoint
                ? 'Nothing of this kind recently.'
                : 'Conversations, tool calls, approvals and scheduled runs appear here.'}
            </Empty>
          </div>
        ) : (
          <div className="space-y-5">
            {days.map(([day, items]) => (
              <section key={day}>
                <DayHeading>{day}</DayHeading>
                <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                  {items.map((it) => (
                    <EventRow
                      key={it.key}
                      assistantId={id}
                      icon={it.icon}
                      tone={it.tone}
                      text={it.text}
                      sub={it.sub}
                      at={it.at}
                      details={it.details}
                      chat={it.chat}
                    />
                  ))}
                </ul>
              </section>
            ))}
            {filtered.length > shown && (
              <div className="flex justify-center">
                <Button size="sm" onClick={() => setShown(shown + PAGE)}>
                  Show older ({filtered.length - shown})
                </Button>
              </div>
            )}
            {olderUnloaded && (
              <p className="text-center text-xs text-subtle">
                This is the latest {sessions.data?.sessions.length} conversations and{' '}
                {audit.data?.length} audit entries; older ones aren’t loaded, so the counts above
                cover only these.
              </p>
            )}
          </div>
        )}
      </section>
    </div>
  )
}

function Panel({
  title,
  count,
  warn,
  children,
}: {
  title: string
  count: number
  warn?: boolean
  children: ReactNode
}) {
  return (
    <section
      className={clsx(
        'overflow-hidden rounded-xl border bg-panel',
        warn && count > 0 ? 'border-warn/40' : 'border-line',
      )}
    >
      <div className="flex items-center gap-2 border-b border-line px-3 py-2 sm:px-4">
        <h2 className="text-[13px] font-semibold">{title}</h2>
        {count > 0 && (
          <span
            className={clsx(
              'rounded-full px-1.5 text-[11px] font-semibold tabular-nums',
              warn ? 'bg-warn/15 text-warn' : 'bg-accent-soft text-accent',
            )}
          >
            {count}
          </span>
        )}
      </div>
      {children}
    </section>
  )
}

function Quiet({ icon, children }: { icon: ReactNode; children: ReactNode }) {
  return (
    <div className="flex items-center gap-2 px-4 py-3.5 text-[13px] text-muted [&>svg]:size-4 [&>svg]:text-subtle">
      {icon}
      {children}
    </div>
  )
}

function ActiveRow({ s, assistantId }: { s: HermesSession; assistantId: number }) {
  const doing = (s as HermesSession & { last_activity_description?: string | null })
    .last_activity_description
  return (
    <li className="border-b border-line last:border-b-0">
      <Link
        to="/assistant/$id"
        params={{ id: String(assistantId) }}
        search={{ s: s.id }}
        className="flex min-h-11 items-start gap-3 px-3 py-2.5 hover:bg-hover sm:px-4"
      >
        <span className="relative mt-0.5 flex size-7 shrink-0 items-center justify-center rounded-full bg-accent-soft text-accent">
          <SourceIcon source={s.source} />
          <span
            aria-hidden
            className="absolute -bottom-0.5 -right-0.5 size-2.5 animate-pulse rounded-full border-2 border-panel bg-ok"
          />
        </span>
        <div className="min-w-0 flex-1">
          <div className="truncate text-[13px] font-medium leading-5">{sessionTitle(s)}</div>
          <div className="truncate text-xs text-subtle">
            {sourceLabel(s.source || 'cli')} · started {relativeTime(iso(s.started_at))} ·{' '}
            {counts(s)}
          </div>
          {doing && <div className="mt-0.5 truncate text-xs text-muted">{doing}</div>}
        </div>
      </Link>
    </li>
  )
}
