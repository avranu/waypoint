import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookOpenCheck,
  ChevronRight,
  CircleCheckBig,
  Globe,
  PencilRuler,
  Webhook,
} from 'lucide-react'
import {
  isOldRequest,
  kindLabel,
  type HookProblem,
  type Waiting,
  type WRequest,
} from '@/lib/requests'
import { dateTime, plural, relativeTime } from '@/lib/format'
import { requestIcon } from '@/components/request-icons'
import { assistantsQuery } from '@/lib/assistant'

/**
 * How long a request has waited; past a day it stands out, as the agent has
 * likely moved on (WP-66).
 */
export function RequestAge({ r }: { r: Pick<WRequest, 'created_at'> }) {
  const old = isOldRequest(r)
  return (
    <span
      data-old={old || undefined}
      title={`Asked ${dateTime(r.created_at)}${old ? '. Over a day old: the agent may have moved on.' : ''}`}
      className={clsx(
        'shrink-0 whitespace-nowrap text-xs',
        old ? 'rounded-md bg-warn/15 px-1.5 py-0.5 font-medium text-warn' : 'text-subtle',
      )}
    >
      {relativeTime(r.created_at)}
    </span>
  )
}

export function WaitingOnYou({
  w,
  onOpen,
  className = 'px-4 pb-5 sm:px-6',
}: {
  w: Waiting
  onOpen: (ref: string) => void
  className?: string
}) {
  const row =
    'flex w-full items-center gap-3 border-b border-line px-4 py-3 text-left last:border-b-0 hover:bg-hover'
  return (
    <section aria-label="Waiting on you" className={clsx(className)}>
      <h2 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
        Waiting on you
      </h2>
      <ul className="overflow-hidden rounded-xl border border-line bg-panel">
        {w.requests.map((r) => (
          <li key={r.id}>
            <button onClick={() => onOpen(r.ref)} className={row}>
              <span
                className={clsx(
                  'flex size-8 shrink-0 items-center justify-center rounded-lg [&>svg]:size-4',
                  isOldRequest(r) ? 'bg-sunken text-muted' : 'bg-accent-soft text-fg',
                )}
              >
                {requestIcon[r.kind]}
              </span>
              <span className="min-w-0 flex-1">
                <span
                  className={clsx(
                    'block truncate text-[14px] font-medium',
                    isOldRequest(r) && 'text-muted',
                  )}
                >
                  {r.title}
                </span>
                <span className="block truncate text-[13px] text-muted">
                  {kindLabel[r.kind]}
                  {r.agent ? ` · ${r.agent.split('/').pop()}` : ''}
                  {r.session ? ` · ${r.session.machine}` : ''}
                  {r.project ? ` · ${r.project.key}` : ''}
                  {r.auto && (
                    <>
                      {' · '}
                      <span title="Filed automatically from the end of the agent's turn">auto</span>
                    </>
                  )}
                </span>
              </span>
              <RequestAge r={r} />
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </button>
          </li>
        ))}
        {w.hooks?.length > 0 && <HooksRow hooks={w.hooks} className={row} />}
        {w.design > 0 && (
          <li>
            <Link to="/design/review" className={row}>
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-sunken text-muted [&>svg]:size-4">
                <PencilRuler />
              </span>
              <span className="min-w-0 flex-1 text-[14px]">
                {plural(w.design, 'design note or edit', 'design notes and edits')} to approve
              </span>
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </Link>
          </li>
        )}
        {w.review > 0 && (
          <li>
            <Link to="/search" search={{ q: 'is:unreviewed', mode: 'issues' }} className={row}>
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-sunken text-muted [&>svg]:size-4">
                <BookOpenCheck />
              </span>
              <span className="min-w-0 flex-1 text-[14px]">
                {plural(w.review, 'piece of agent-written text', 'pieces of agent-written text')} to
                review
              </span>
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </Link>
          </li>
        )}
        {(w.closes ?? 0) > 0 && (
          <li>
            <Link to="/catch-up" className={row}>
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-sunken text-muted [&>svg]:size-4">
                <CircleCheckBig />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block text-[14px]">
                  {plural(w.closes ?? 0, 'task')} agents finished, not looked at yet
                </span>
                <span className="block text-[13px] text-muted">
                  Catch up on them one at a time: what changed, screenshots, where to try it.
                </span>
              </span>
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </Link>
          </li>
        )}
        {(w.outside ?? 0) > 0 && (
          <li>
            <Link to="/search" search={{ q: 'is:outside', mode: 'issues' }} className={row}>
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-warn/15 text-warn [&>svg]:size-4">
                <Globe />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block text-[14px]">
                  {plural(w.outside ?? 0, 'issue', 'issues')} with text from outside to approve
                </span>
                <span className="block text-[13px] text-muted">
                  Written on a forge by people who aren't Waypoint people; agents can't see it yet.
                </span>
              </span>
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </Link>
          </li>
        )}
        {(w.outside_pulls ?? 0) > 0 && (
          <li>
            <Link to="/agents" search={{ tab: 'pulls' }} className={row}>
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-warn/15 text-warn [&>svg]:size-4">
                <Globe />
              </span>
              <span className="min-w-0 flex-1">
                <span className="block text-[14px]">
                  {plural(w.outside_pulls ?? 0, 'pull request', 'pull requests')} with text from
                  outside to approve
                </span>
                <span className="block text-[13px] text-muted">
                  Marked From outside: no agent or bot touches them until you approve.
                </span>
              </span>
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            </Link>
          </li>
        )}
      </ul>
    </section>
  )
}

/** One row for every broken webhook: one fix (usually the forge's token) tends to mend them all. */
function HooksRow({ hooks, className }: { hooks: HookProblem[]; className: string }) {
  const first = hooks[0]
  const notes = new Set(hooks.map((h) => h.note ?? ''))
  const why =
    notes.size === 1 && first.note
      ? first.note
      : first.state === 'failed'
        ? 'Waypoint couldn’t add the webhook.'
        : 'Nothing has arrived from the forge’s webhook.'
  const names =
    hooks.length > 3
      ? `${hooks
          .slice(0, 3)
          .map((h) => h.repo)
          .join(', ')} and ${hooks.length - 3} more`
      : hooks.map((h) => h.repo).join(', ')
  return (
    <li>
      <Link
        to="/p/$key/settings"
        params={{ key: first.project }}
        search={{ tab: 'integrations' }}
        className={className}
      >
        <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-warn/15 text-warn [&>svg]:size-4">
          <Webhook />
        </span>
        <span className="min-w-0 flex-1">
          <span className="block text-[14px] font-medium">
            {hooks.length === 1
              ? `Webhook not working on ${first.repo}`
              : `Webhooks not working on ${plural(hooks.length, 'repo')}`}
          </span>
          <span className="block text-[13px] text-muted [overflow-wrap:anywhere]">
            {hooks.length > 1 && <>{names}. </>}
            {why} Reviews and CI follow the 10-minute sync instead, so they run late.
          </span>
        </span>
        <ChevronRight className="size-4 shrink-0 text-subtle" />
      </Link>
    </li>
  )
}

/**
 * Actions your assistants' gates hold for you (WP-ADR1). They are decided
 * on Signal, so this only says what is waiting and links to it.
 */
export function AssistantsWaiting({ className = 'px-4 pb-5 sm:px-6' }: { className?: string }) {
  const waiting = (useQuery(assistantsQuery()).data ?? []).filter(
    (a) => a.mine && a.state.pending.length,
  )
  if (!waiting.length) return null
  return (
    <section aria-label="Waiting on Signal" className={className}>
      <h2 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
        Waiting on Signal
      </h2>
      <ul className="overflow-hidden rounded-xl border border-line bg-panel">
        {waiting.flatMap((a) =>
          a.state.pending.map((p) => (
            <li key={a.id + p.code}>
              <Link
                to="/assistant/$id"
                params={{ id: String(a.id) }}
                search={{ tab: 'activity', view: 'approvals' }}
                className="flex w-full items-center gap-3 border-b border-line px-4 py-3 text-left last:border-b-0 hover:bg-hover"
              >
                <span className="shrink-0 rounded bg-warn/15 px-1.5 font-mono text-xs font-semibold text-warn">
                  {p.code}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-sm">{p.summary}</span>
                  <span className="block text-xs text-subtle">
                    {a.name} · {relativeTime(new Date(p.created * 1000).toISOString())}
                  </span>
                </span>
                <ChevronRight className="size-4 shrink-0 text-subtle" />
              </Link>
            </li>
          )),
        )}
      </ul>
    </section>
  )
}
