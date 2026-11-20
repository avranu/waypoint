// A pull request's babysitter (WP-134): whether Waypoint is seeing it
// through to the merge, what it waits on, and every reviewer's comments with
// what was done about each; start and stop it.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { ExternalLink } from 'lucide-react'
import { dateTime, safeUrl } from '@/lib/format'
import {
  babysitQuery,
  dismissComment,
  startBabysit,
  stopBabysit,
  type Babysit,
  type BabysitRound,
  type BabysitState,
  type CommentVerdict,
  type PRDetail,
  type ReviewComment,
  type RoundClass,
} from '@/lib/pulls'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'

const STATE: Record<BabysitState, { label: string; color?: string }> = {
  watching: { label: 'Watching' },
  working: { label: 'Working' },
  waiting_review: { label: 'Waiting for reviews' },
  ready: { label: 'Ready', color: 'var(--wp-ok)' },
  needs_person: { label: 'Needs you', color: 'var(--wp-warn)' },
  resting: { label: 'Resting', color: 'var(--wp-subtle)' },
  merged: { label: 'Merged', color: 'var(--wp-ok)' },
  stopped: { label: 'Stopped', color: 'var(--wp-subtle)' },
  gave_up: { label: 'Gave up', color: 'var(--wp-danger)' },
}

const VERDICT: Record<CommentVerdict, { label: string; color?: string }> = {
  new: { label: 'New' },
  seen: { label: '👀 Seen' },
  accepted: { label: '👍 Accepted' },
  rejected: { label: '👎 Rejected', color: 'var(--wp-subtle)' },
  partial: { label: '😕 Partly' },
  needs_person: { label: '😕 Needs a person', color: 'var(--wp-warn)' },
  fixing: { label: '🚀 Being fixed' },
  fixed: { label: '🎉 Fixed', color: 'var(--wp-ok)' },
  verified: { label: '❤️ Verified', color: 'var(--wp-ok)' },
  dismissed: { label: 'Dismissed', color: 'var(--wp-subtle)' },
  deferred: { label: '👀 Deferred to a task', color: 'var(--wp-subtle)' },
}

// Why a round came (WP-438): its weightiest item's class.
const CLASS: Record<RoundClass, { label: string; title: string; color?: string }> = {
  new: { label: 'New', title: 'a finding no earlier round was told about' },
  repeat: {
    label: 'Repeat',
    title: 'the same point again after a round fixed it, or a check failing again after its fix',
    color: 'var(--wp-warn)',
  },
  caused: {
    label: 'Caused by last round',
    title: 'on code the previous round changed',
    color: 'var(--wp-danger)',
  },
  ci_or_conflict: { label: 'CI or conflict', title: 'a failed check or a conflict with the base' },
  base: {
    label: 'Base',
    title: 'a check that fails on the base branch too',
    color: 'var(--wp-subtle)',
  },
  nothing_pushed: {
    label: 'Nothing pushed',
    title: 'its agent ended without pushing',
    color: 'var(--wp-subtle)',
  },
}

/** "round 2 of at most 15 · sonnet", or why it ended. */
function summary(b: Babysit) {
  if (!b.active) return b.end_reason || ''
  // The wait often names the round already ("round 2: an agent is on it").
  const round =
    b.round > 0 && !b.wait?.startsWith(`round ${b.round}`)
      ? `round ${b.round} of at most ${b.max_rounds}`
      : ''
  const parts = [b.wait, round, b.model]
  return parts.filter(Boolean).join(' · ')
}

/** The comment's first line of prose, skipping bots' leading HTML comments. React escapes it. */
function firstLine(s: string) {
  let line = ''
  let inComment = false
  for (const raw of s.split('\n')) {
    const l = raw.trim()
    if (inComment) {
      inComment = !l.includes('-->')
    } else if (l.startsWith('<!--')) {
      inComment = !l.includes('-->')
    } else if (l !== '') {
      line = l
      break
    }
  }
  return line.length > 160 ? line.slice(0, 159) + '…' : line
}

export function Babysitter({ pr }: { pr: PRDetail }) {
  const live = pr.state === 'open' || pr.state === 'draft'
  const { data } = useQuery(babysitQuery(pr.id))
  const qc = useQueryClient()
  const refresh = () => qc.invalidateQueries({ queryKey: ['pulls'] })
  const start = useMutation({
    mutationFn: () => startBabysit(pr.id),
    onError: toastError,
    onSettled: refresh,
  })
  const stop = useMutation({
    mutationFn: () => stopBabysit(pr.id),
    onError: toastError,
    onSettled: refresh,
  })
  const dismiss = useMutation({
    mutationFn: (id: string) => dismissComment(id),
    onError: toastError,
    onSettled: refresh,
  })
  if (!data) return null
  const b = data.babysit
  const comments = data.comments.filter((c) => !c.gone)
  const canStart = data.can_edit && live && (!b || !b.active)
  const canStop = data.can_edit && !!b && (b.active || b.state === 'gave_up')
  if (!b && comments.length === 0 && !canStart) return null
  const open = comments.filter((c) => !c.dealt).length
  const state = b ? STATE[b.state] : undefined
  return (
    <section aria-label="Babysitter" className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-[13px] font-semibold">Babysitter</h3>
        {b && state && <Badge color={state.color}>{state.label}</Badge>}
        {canStart && (
          <Button
            size="xs"
            className="ml-auto"
            loading={start.isPending}
            onClick={() => start.mutate()}
            title="Waypoint watches it until it merges, and hands each round of failed checks and review comments to a fresh agent session on your runner"
          >
            Start
          </Button>
        )}
        {canStop && (
          <Button
            size="xs"
            variant="ghost"
            className="ml-auto"
            loading={stop.isPending}
            onClick={() => stop.mutate()}
            title={
              b?.active ? 'Stop babysitting it' : 'Clear it: its comments stop holding the merge'
            }
          >
            {b?.active ? 'Stop' : 'Clear'}
          </Button>
        )}
      </div>
      {b && summary(b) && <p className="text-xs text-muted">{summary(b)}</p>}
      {b && b.rounds.some((r) => r.class || r.parked) && <Rounds rounds={b.rounds} />}
      {comments.length > 0 && (
        <div>
          <h4 className="mb-1.5 text-[13px] font-medium">
            Review comments{' '}
            <span className="font-normal text-muted">
              {open > 0
                ? `${open} to deal with of ${comments.length}`
                : `all ${comments.length} dealt with`}
            </span>
          </h4>
          <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
            {comments.map((c) => (
              <CommentRow
                key={c.id}
                c={c}
                canDismiss={data.can_edit && live && !c.dealt}
                dismissing={dismiss.isPending && dismiss.variables === c.id}
                onDismiss={() => dismiss.mutate(c.id)}
              />
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

/** Each round with its class and what it was given (WP-438). */
function Rounds({ rounds }: { rounds: BabysitRound[] }) {
  return (
    <div>
      <h4 className="mb-1.5 text-[13px] font-medium">Rounds</h4>
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
        {rounds.map((r) => (
          <RoundRow key={r.n} r={r} />
        ))}
      </ul>
    </div>
  )
}

function RoundRow({ r }: { r: BabysitRound }) {
  const c = r.class ? CLASS[r.class] : undefined
  const given = r.work_class && r.work_class !== r.class ? CLASS[r.work_class] : undefined
  return (
    <li className="px-3 py-2 text-[13px]" data-class={r.class}>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="font-medium">Round {r.n}</span>
        <span className="font-mono text-xs text-subtle">{r.sha.slice(0, 7)}</span>
        {c && (
          <span title={c.title}>
            <Badge color={c.color}>{c.label}</Badge>
          </span>
        )}
        {given && (
          <span className="text-xs text-muted">given {given.label.toLowerCase()} work</span>
        )}
        {r.assessor ? (
          <span title="The stronger model at high effort, starting fresh with the task, every round and the ledger">
            <Badge color="var(--wp-accent)">Assessor</Badge>
          </span>
        ) : (
          r.model && <span className="text-xs text-muted">{r.model}</span>
        )}
        {r.outcome && <span className="text-xs text-muted">{r.outcome}</span>}
      </div>
      {r.parked && (
        <p className="mt-1 break-words text-xs" data-parked={r.rest_until ? 'rest' : 'person'}>
          <span className="font-medium">
            {r.rest_until
              ? r.reply
                ? 'Out of the merge line, rested (nobody asked):'
                : r.rerun_at
                  ? 'Out of the merge line, its checks running again (nobody asked):'
                  : `Out of the merge line, resting until ${dateTime(r.rest_until)} (nobody asked):`
              : 'Out of the merge line, waiting for a person:'}
          </span>{' '}
          <span className="text-muted">{r.parked}</span>
        </p>
      )}
      {r.reply && !r.rest_until && (
        <p className="mt-1 break-words text-xs text-muted">
          <span className="font-medium text-fg">Answered:</span> {r.reply}
        </p>
      )}
      {r.ledger && r.ledger.length > 0 && (
        <ul className="mt-1 space-y-0.5 text-xs text-muted">
          {r.ledger.map((e) => (
            <li key={e.id} className="break-words" data-class={e.class}>
              <span className="font-mono text-subtle">{e.id}</span>{' '}
              <span className={clsx(e.class === 'repeat' || e.class === 'caused' ? 'text-fg' : '')}>
                {CLASS[e.class]?.label.toLowerCase() ?? e.class}
              </span>
              {e.path && (
                <span className="ml-1.5 break-all font-mono">
                  {e.path}
                  {e.symbol ? ` · ${e.symbol}` : ''}
                </span>
              )}
              {e.why && <span className="ml-1.5">— {e.why}</span>}
            </li>
          ))}
        </ul>
      )}
    </li>
  )
}

function CommentRow({
  c,
  canDismiss,
  dismissing,
  onDismiss,
}: {
  c: ReviewComment
  canDismiss: boolean
  dismissing: boolean
  onDismiss: () => void
}) {
  const v = VERDICT[c.verdict] ?? { label: c.verdict }
  const href = safeUrl(c.url)
  return (
    <li className="px-3 py-2 text-[13px]" data-verdict={c.verdict}>
      <div className="flex items-start gap-2">
        <Badge color={v.color} className="mt-px shrink-0">
          {v.label}
        </Badge>
        <span className={clsx('min-w-0 flex-1', c.dealt && 'text-muted')}>
          <span className="font-medium">{c.author}</span>
          {c.path && (
            <span className="ml-1.5 break-all font-mono text-xs text-muted">
              {c.path}
              {c.line ? `:${c.line}` : ''}
            </span>
          )}{' '}
          <span className="break-words">{firstLine(c.body)}</span>
          {c.note && <span className="mt-0.5 block text-xs text-subtle">{c.note}</span>}
        </span>
        <span
          className="shrink-0 font-mono text-xs text-subtle"
          title="wp pr ack and wp pr fixed take this id"
        >
          {c.id}
        </span>
        {href && (
          <a
            href={href}
            target="_blank"
            rel="noreferrer"
            className="shrink-0 text-subtle hover:text-fg"
            aria-label={`Open ${c.id} on the forge`}
          >
            <ExternalLink className="size-3.5" />
          </a>
        )}
        {canDismiss && (
          <Button
            size="xs"
            variant="ghost"
            className="shrink-0"
            loading={dismissing}
            onClick={onDismiss}
            title="Nothing needs doing about it: stop it holding the merge"
          >
            Dismiss
          </Button>
        )}
      </div>
    </li>
  )
}
