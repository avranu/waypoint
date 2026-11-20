// Who wrote something agents can write too, and whether a person has
// reviewed it (WP-D11). Agent-written text shows "Written by <agent>" and
// stays "Not reviewed" until someone marks it, or rewrites it.

import { Fragment } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Bot, Check, GitFork, ShieldAlert } from 'lucide-react'
import { api, enc, qs } from '@/lib/api'
import { agentLabel } from '@/lib/agents'
import { invalidateProject } from '@/lib/queries'
import { Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import type { Lineage, ProvenanceRisk, UserRef } from '@/lib/types'

export type ReviewType = 'issue' | 'comment' | 'close' | 'page' | 'glossary'

/** Marks agent-written content reviewed by the signed-in person. */
export function useReview() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ type, ref, project }: { type: ReviewType; ref: string; project?: string }) =>
      api.post<{ reviewed: number }>(`/review/${type}/${enc(ref)}${qs({ project })}`),
    onSuccess: (_, { type }) => {
      for (const key of type === 'glossary'
        ? ['glossary', 'unreviewed']
        : type === 'page'
          ? ['page', 'pages', 'unreviewed']
          : ['issue', 'comments', 'unreviewed', 'waiting'])
        qc.invalidateQueries({ queryKey: [key] })
      // An issue's close shows on boards and lists too (WP-272).
      if (type === 'issue' || type === 'close') {
        invalidateProject(qc)
      }
    },
    onError: toastError,
  })
}

/**
 * "Written by claude-opus-5-5 · Not reviewed · Mark reviewed", or
 * "… · Reviewed by Robin". Nothing for what a person wrote.
 */
export function WrittenBy({
  agent,
  unreviewed,
  reviewedBy,
  review,
  label = 'Written by',
  stateOnly,
  className,
}: {
  agent?: string
  unreviewed?: boolean
  reviewedBy?: UserRef | null
  /** what to review; absent when the viewer can't */
  review?: { type: ReviewType; ref: string; project?: string }
  label?: string
  /** just the review state, where the agent is named already */
  stateOnly?: boolean
  className?: string
}) {
  const mark = useReview()
  if (!agent) return null
  const sep = !stateOnly && <span aria-hidden>·</span>
  return (
    <span
      className={clsx(
        'inline-flex flex-wrap items-center gap-x-1.5 text-xs text-subtle',
        className,
      )}
    >
      {!stateOnly && (
        <span className="inline-flex min-w-0 items-center gap-1" title={`${label} ${agent}`}>
          <Bot className="size-3 shrink-0" />
          <span className="truncate">
            {label} {agentLabel(agent)}
          </span>
        </span>
      )}
      {unreviewed ? (
        <>
          {sep}
          <span className="font-medium text-warn">Not reviewed</span>
          {review && (
            <button
              type="button"
              disabled={mark.isPending}
              onClick={() => mark.mutate(review)}
              className="inline-flex items-center gap-0.5 rounded px-1 font-medium text-accent hover:bg-hover disabled:opacity-60"
            >
              <Check className="size-3" />
              Mark reviewed
            </button>
          )}
        </>
      ) : (
        reviewedBy && (
          <>
            {sep}
            <span>Reviewed by {reviewedBy.display_name}</span>
          </>
        )
      )}
    </span>
  )
}

/**
 * The marker on a card or row an agent closed (Done or Won't Do) that no
 * person has reviewed yet (WP-272). The issue stays closed; reviewing the
 * close, on the issue or in bulk from the list, clears it.
 */
export function CloseReviewBadge({ agent, className }: { agent?: string; className?: string }) {
  return (
    <span
      title={`Closed by ${agent ? agentLabel(agent) : 'an agent'}; no person has reviewed the close yet. Open it to mark it reviewed.`}
      className={clsx('shrink-0', className)}
    >
      <Badge>
        <Bot className="size-3" aria-hidden />
        Not reviewed
      </Badge>
    </span>
  )
}

/** A page's code (PL13) as a small mono chip. */
export function CodeChip({ code, className }: { code?: string; className?: string }) {
  if (!code) return null
  return (
    <span
      className={clsx(
        'inline-flex h-5 shrink-0 items-center rounded-md bg-sunken px-1.5 font-mono text-[11px] font-medium text-muted',
        className,
      )}
    >
      {code}
    </span>
  )
}

const riskFactor: Record<string, string> = {
  outside: 'text from outside',
  held: 'held by the safety screen',
  outside_pr: 'pull request from outside',
  agent_text: 'unreviewed agent text',
  agent_comments: 'unreviewed agent comments',
  agent_close: 'unreviewed agent close',
  ai_detected: 'reads as AI-written',
  agent_proposal: 'unapproved agent proposal',
  no_task: 'no task asked for it',
  no_goal: 'traces to no goal',
  loose_goal: 'loosely tied to a goal',
}

/**
 * "High risk · text from outside", when a thing's provenance risk isn't
 * low (WP-554); the full reasons show on hover and to screen readers.
 */
export function RiskNote({ risk, className }: { risk?: ProvenanceRisk; className?: string }) {
  if (!risk || risk.level === 'low') return null
  const reasons = risk.reasons ?? []
  const factors = [
    ...new Set(reasons.map((r) => riskFactor[r.factor] ?? (r.ref ? `${r.ref} at risk` : r.factor))),
  ]
  const high = risk.level === 'high'
  return (
    <p
      className={clsx('flex flex-wrap items-center gap-1.5 text-xs text-muted', className)}
      title={reasons.map((r) => r.text).join('\n')}
      data-testid="risk-note"
    >
      <Badge color={high ? 'var(--wp-danger)' : 'var(--wp-warn)'}>
        <ShieldAlert className="size-3" />
        {high ? 'High risk' : 'Medium risk'}
      </Badge>
      <span>{factors.join(', ')}</span>
      <span className="sr-only">: {reasons.map((r) => r.text).join('; ')}</span>
    </p>
  )
}

/**
 * "Written while working on WP-1 (agent, unreviewed) ← WP-0 · depth 2": the
 * tasks an agent wrote this text while working on, nearest first (WP-275).
 */
export function LineageNote({ lineage, className }: { lineage?: Lineage; className?: string }) {
  if (!lineage || lineage.chain.length === 0) return null
  return (
    <p
      className={clsx(
        'flex flex-wrap items-center gap-x-1 text-xs',
        lineage.held ? 'text-warn' : 'text-muted',
        className,
      )}
      title="The task the agent's session was working on when it wrote this, and the tasks behind that one. From depth 3 the safety screen holds it for a person."
      data-testid="lineage-note"
    >
      <GitFork className="size-3 shrink-0" />
      <span>Written while working on</span>
      {lineage.chain.map((l, n) => (
        <Fragment key={n}>
          {n > 0 && <span aria-label="which was written while working on">←</span>}
          {l.key ? (
            <Link
              to="/issue/$key"
              params={{ key: l.key }}
              className="font-medium text-fg hover:underline"
              title={l.title}
            >
              {l.key}
            </Link>
          ) : (
            <span>
              {l.gone ? 'a deleted task' : 'a task you can’t open'} (depth {l.depth})
            </span>
          )}
          {l.key && (
            <span className="text-subtle">
              (
              {l.agent
                ? `${agentLabel(l.agent)}, not reviewed`
                : l.reviewed
                  ? 'reviewed'
                  : 'a person’s'}
              )
            </span>
          )}
        </Fragment>
      ))}
      <span>· depth {lineage.depth}</span>
    </p>
  )
}
