import { useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Check, ChevronDown, Sparkles, Target, X } from 'lucide-react'
import {
  GROUNDING_DETAIL,
  GROUNDING_LABEL,
  groupCounts,
  type GroundGroup,
  type GroundGroupId,
  designApi,
  designQuery,
  goalApi,
  invalidateKnowledge,
  itemHref,
  traceQuery,
} from '@/lib/knowledge'
import { agentLabel } from '@/lib/agents'
import {
  type Grounding,
  type ItemTrace,
  type KItem,
  type TaskGoalSuggestion,
  type TraceHop,
} from '@/lib/types'
import { vizClass } from '@/components/charts'
import { toastError } from '@/components/ui/toast'
import { Skeleton } from '@/components/ui/misc'
import { Picker, Popover, usePopover } from '@/components/ui/popover'

/** A color per grounding: people's goals green, agents' blue, a guess paler, none red. */
export const GROUNDING_COLOR: Record<Grounding, string> = {
  person_goal: 'var(--wp-ok)',
  agent_goal: 'var(--viz-1)',
  inferred_goal: 'color-mix(in oklab, var(--viz-1) 45%, var(--wp-sunken))',
  proposed_goal: 'var(--wp-warn)',
  person_request: 'color-mix(in oklab, var(--wp-ok) 45%, var(--wp-sunken))',
  none: 'var(--wp-danger)',
}

export function GroundingChip({
  grounding,
  title,
  className,
}: {
  grounding: Grounding
  title?: string
  className?: string
}) {
  const color = GROUNDING_COLOR[grounding]
  return (
    <span
      title={
        title ??
        (GROUNDING_DETAIL[grounding] !== GROUNDING_LABEL[grounding]
          ? GROUNDING_DETAIL[grounding]
          : undefined)
      }
      className={clsx(
        'inline-flex h-5 shrink-0 items-center gap-1 rounded-md px-1.5 text-[11px] font-medium',
        vizClass,
        className,
      )}
      style={{
        background: `color-mix(in oklab, ${color} 15%, transparent)`,
        color: `color-mix(in oklab, ${color} 75%, var(--wp-fg))`,
      }}
    >
      <span aria-hidden className="size-1.5 rounded-full" style={{ background: color }} />
      {GROUNDING_LABEL[grounding]}
    </span>
  )
}

/** The color of each group (the bar, the legend and the week chart share it). */
export const GROUP_COLOR: Record<GroundGroupId, string> = {
  approved: GROUNDING_COLOR.person_goal,
  asked: GROUNDING_COLOR.person_request,
  agent: GROUNDING_COLOR.agent_goal,
  none: GROUNDING_COLOR.none,
}

/** "Agent: 84 tasks (70 agent's link, 7 guessed goal)": the group, and what it holds when it holds several kinds. */
export function groupTitle(group: GroundGroup, counts: Partial<Record<Grounding, number>>) {
  const n = group.members.reduce((t, m) => t + (counts[m] ?? 0), 0)
  const parts =
    group.members.length > 1
      ? group.members.filter((m) => counts[m]).map((m) => `${counts[m]} ${GROUNDING_DETAIL[m]}`)
      : []
  return `${group.label}: ${n} ${n === 1 ? 'task' : 'tasks'}${parts.length ? ` (${parts.join(', ')})` : ''}`
}

/**
 * Counts as one stacked bar in the legend's order. Hover a segment for its
 * group and count; the legend (once, at the top of the list) says what the
 * colors mean.
 */
export function GroundingBar({
  counts,
  className,
}: {
  counts: Partial<Record<Grounding, number>>
  className?: string
}) {
  const groups = groupCounts(counts)
  if (groups.length === 0)
    return <div className={clsx('h-2 rounded-full bg-sunken', className)} aria-hidden />
  return (
    <div
      role="img"
      aria-label={groups.map(({ group, n }) => `${n} ${group.label.toLowerCase()}`).join(', ')}
      className={clsx('flex h-2 gap-[2px] overflow-hidden rounded-full', vizClass, className)}
    >
      {groups.map(({ group, n }) => (
        <div
          key={group.id}
          title={groupTitle(group, counts)}
          className="h-full first:rounded-l-full last:rounded-r-full"
          style={{ flexGrow: n, flexBasis: 0, background: GROUP_COLOR[group.id] }}
        />
      ))}
    </div>
  )
}

/** Who stands behind a hop: a person (who made or confirmed it), an agent, or Waypoint's guess. */
function hopBy(h: TraceHop) {
  switch (h.basis) {
    case 'person':
      return h.by ? `by ${h.by.display_name}` : 'by a person'
    case 'inferred':
      return 'guessed by Waypoint'
  }
  return h.agent ? `by agent ${agentLabel(h.agent)}` : 'by an agent'
}

/** The first link on a route no person stands behind: the one to confirm. */
const looseHop = (hops: TraceHop[]) => hops.find((h) => h.basis !== 'person' && h.link_id)

/**
 * The line under an issue's title: the goal it serves and how sure that is,
 * or that it serves none (with Waypoint's best guess to link in one click).
 * Someone who can edit the task picks its goal here, or asks the model for
 * one (WP-679), even in a project with no goals yet.
 */
export function GoalTrace({
  issueKey,
  project,
  scoped,
  editable,
  className,
}: {
  issueKey: string
  project: string
  /** an approved goal covers the issue's project (the issue says so) */
  scoped: boolean
  editable: boolean
  className?: string
}) {
  const qc = useQueryClient()
  const trace = useQuery({ ...traceQuery(issueKey), enabled: scoped })
  const [open, setOpen] = useState(false)
  const done = () => {
    invalidateKnowledge(qc)
    void qc.invalidateQueries({ queryKey: ['issue', issueKey] })
  }
  const confirm = useMutation({
    mutationFn: (id: number) => goalApi.confirm(id),
    onError: toastError,
    onSettled: done,
  })
  const dismiss = useMutation({
    mutationFn: (id: number) => goalApi.dismiss(id),
    onError: toastError,
    onSettled: done,
  })
  const link = useMutation({
    mutationFn: (goal: number) => goalApi.link({ type: 'issue', key: issueKey }, goal),
    onError: toastError,
    onSettled: done,
  })
  const notThis = useMutation({
    mutationFn: (goal: number) => goalApi.dismissMatch({ type: 'issue', key: issueKey }, goal),
    onError: toastError,
    onSettled: done,
  })
  if (!scoped)
    return editable ? (
      <div
        className={clsx('flex flex-wrap items-center gap-1.5 text-xs text-muted', className)}
        data-testid="goal-trace"
      >
        <Target className="size-3.5 shrink-0 text-subtle" aria-hidden />
        <span className="text-subtle">No goal yet</span>
        <GoalPick issueKey={issueKey} project={project} onDone={done} />
      </div>
    ) : null
  if (trace.isPending)
    return (
      <div
        className={clsx('flex items-center gap-1.5 text-xs text-subtle', className)}
        data-testid="goal-trace-loading"
      >
        <Target className="size-3.5 shrink-0" aria-hidden />
        <span>Tracing to goals…</span>
        <Skeleton className="h-3 w-40" />
      </div>
    )
  if (trace.isError)
    return (
      <div
        className={clsx('flex flex-wrap items-center gap-1.5 text-xs text-muted', className)}
        role="alert"
      >
        <Target className="size-3.5 shrink-0 text-subtle" aria-hidden />
        <span>Couldn’t trace this task to a goal: {trace.error.message}</span>
        <button
          type="button"
          onClick={() => void trace.refetch()}
          className="rounded px-1 text-subtle hover:bg-hover hover:text-fg"
        >
          Try again
        </button>
      </div>
    )
  const t: ItemTrace = trace.data
  if (!t.scoped && t.routes.length === 0 && !editable) return null
  const best = t.routes[0]
  const loose = best && looseHop(best.hops)
  const busy = confirm.isPending || dismiss.isPending || link.isPending || notThis.isPending
  const small =
    'inline-flex items-center gap-0.5 rounded px-1 text-subtle hover:bg-hover hover:text-fg'
  return (
    <div className={clsx('text-xs text-muted', className)} data-testid="goal-trace">
      <div className="flex flex-wrap items-center gap-1.5">
        <Target className="size-3.5 shrink-0 text-subtle" aria-hidden />
        {best ? (
          <>
            <span>Serves</span>
            <GoalLinkText goal={best.goal} />
            <GroundingChip grounding={best.grounding} title={t.words} />
            {t.routes.length > 1 && <span className="text-subtle">+{t.routes.length - 1}</span>}
            <button
              type="button"
              onClick={() => setOpen(!open)}
              aria-expanded={open}
              className={small}
            >
              how
              <ChevronDown className={clsx('size-3 transition-transform', open && 'rotate-180')} />
            </button>
            {editable && loose?.link_id && (
              <>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => confirm.mutate(loose.link_id!)}
                  className={small}
                  title="Link it: stand behind this link, so it counts as yours"
                >
                  <Check className="size-3" /> Link
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => dismiss.mutate(loose.link_id!)}
                  className={small}
                  title="It doesn't serve this goal: drop the link"
                >
                  <X className="size-3" /> Drop
                </button>
              </>
            )}
            {editable && (
              <GoalPick
                issueKey={issueKey}
                project={project}
                onDone={done}
                linked={t.routes
                  .filter(
                    (r) =>
                      r.hops.length === 1 &&
                      r.hops[0].kind === 'serves' &&
                      r.hops[0].basis === 'person',
                  )
                  .map((r) => r.goal.id)}
              />
            )}
          </>
        ) : (
          <>
            <GroundingChip grounding={t.grounding} title={t.words} />
            {t.candidate && editable && (
              <span className="inline-flex flex-wrap items-center gap-1">
                <span>· maybe</span>
                <GoalLinkText goal={t.candidate.goal} />
                <span className="text-subtle">({t.candidate.reason})</span>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => link.mutate(t.candidate!.goal.id!)}
                  className={small}
                >
                  <Check className="size-3" /> Link
                </button>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => notThis.mutate(t.candidate!.goal.id!)}
                  className={small}
                  title="It doesn't serve this goal: stop suggesting it"
                >
                  <X className="size-3" /> Drop
                </button>
              </span>
            )}
            {editable && <GoalPick issueKey={issueKey} project={project} onDone={done} />}
          </>
        )}
      </div>
      {open && best && (
        <ol className="ml-5 mt-1 space-y-0.5 border-l border-line pl-3 text-subtle">
          {best.hops.map((h, i) => (
            <li key={i}>
              <span className="text-muted">{h.from.key ?? h.from.title}</span> {h.kind}{' '}
              <span className="text-muted">{h.to.key ?? h.to.title}</span>
              {hopBy(h) && <span> · {hopBy(h)}</span>}
            </li>
          ))}
          <li>
            {t.person
              ? 'A person wrote or approved this task.'
              : t.person_hops !== null
                ? `Nearest person: ${t.person_hops} ${t.person_hops === 1 ? 'step' : 'steps'} up.`
                : 'No person anywhere on the way.'}{' '}
            {t.person_touches > 0 &&
              `${t.person_touches} human ${t.person_touches === 1 ? 'influence' : 'influences'} on the way.`}
          </li>
        </ol>
      )}
    </div>
  )
}

export function GoalLinkText({ goal }: { goal: KItem }) {
  const href = itemHref(goal)
  return href ? (
    <Link to={href} className="min-w-0 truncate font-medium text-fg hover:underline">
      {goal.title}
    </Link>
  ) : (
    <span className="font-medium text-fg">{goal.title}</span>
  )
}

/**
 * Says which goal a task serves: pick one (or name a new one), or ask the
 * model, whose answer waits here for a click (WP-679).
 */
function GoalPick({
  issueKey,
  project,
  onDone,
  linked,
}: {
  issueKey: string
  project: string
  onDone: () => void
  /**
   * goals a person already said the task serves: picking one again would do
   * nothing. Other links to a goal stay, since picking it makes them a person's.
   */
  linked?: (number | undefined)[]
}) {
  const pick = usePopover()
  const goals = useQuery({ ...designQuery({ project, kind: 'goal' }), enabled: pick.open })
  const [hint, setHint] = useState<TaskGoalSuggestion | null>(null)
  const made = useRef(new Map<string, number>())
  const item = { type: 'issue' as const, key: issueKey }
  const link = useMutation({
    mutationFn: async (g: { id?: number; title?: string }) => {
      let id = g.id ?? made.current.get(g.title!)
      if (!id) {
        id = (await designApi.create({ title: g.title!, kind: 'goal', project })).id
        made.current.set(g.title!, id)
      }
      await goalApi.link(item, id)
    },
    onSuccess: () => setHint(null),
    onError: toastError,
    onSettled: onDone,
  })
  const suggest = useMutation({
    mutationFn: () => goalApi.suggestForTask(issueKey),
    onSuccess: setHint,
    onError: toastError,
  })
  const notThis = useMutation({
    mutationFn: (goal: number) => goalApi.dismissMatch(item, goal),
    onSuccess: () => setHint(null),
    onError: toastError,
    onSettled: onDone,
  })
  const busy = link.isPending || suggest.isPending || notThis.isPending
  const small =
    'inline-flex items-center gap-0.5 rounded px-1 text-subtle hover:bg-hover hover:text-fg disabled:opacity-60'
  const approved = (goals.data ?? []).filter(
    (n) => n.status === 'active' && (n.approved || !!n.requested) && !linked?.includes(n.id),
  )
  const named = hint?.goal?.title ?? hint?.title
  return (
    <>
      <button
        ref={pick.anchor}
        type="button"
        disabled={busy}
        onClick={pick.toggle}
        className={small}
      >
        Link to a goal…
      </button>
      <button
        type="button"
        disabled={busy}
        onClick={() => suggest.mutate()}
        className={small}
        title="An AI model reads this task and names the goal it serves; nothing is linked until you click Link"
      >
        <Sparkles className="size-3" /> {suggest.isPending ? 'Asking…' : 'Suggest'}
      </button>
      <Popover open={pick.open} onClose={pick.close} anchor={pick.anchor} align="start" width={320}>
        <Picker
          items={approved.map((g) => ({
            value: g.id,
            label: g.title,
            icon: <Target className="size-3.5 text-subtle" />,
          }))}
          selected={[]}
          placeholder={goals.isPending ? 'Loading goals…' : 'Which goal does it serve?'}
          onPick={(id) => {
            pick.close()
            if (id !== null) link.mutate({ id })
          }}
          onCreate={(title) => {
            pick.close()
            link.mutate({ title })
          }}
          createLabel={(t) => `Add goal: “${t}”`}
          onClose={pick.close}
        />
      </Popover>
      {hint && (
        <span
          className="flex w-full flex-wrap items-center gap-1 rounded-md bg-sunken px-2 py-1"
          data-testid="goal-hint"
        >
          {named ? (
            <>
              <Sparkles className="size-3 shrink-0 text-accent" aria-hidden />
              <span className="text-subtle">{hint.goal ? 'Serves' : 'Add goal:'}</span>
              <span className="font-medium text-fg">{named}</span>
              {hint.why && <span className="text-subtle">· {hint.why}</span>}
              <button
                type="button"
                disabled={busy}
                onClick={() => link.mutate({ id: hint.goal?.id, title: hint.title })}
                className={small}
              >
                <Check className="size-3" /> Link
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => (hint.goal?.id ? notThis.mutate(hint.goal.id) : setHint(null))}
                className={small}
                title="Wrong goal: stop suggesting it"
              >
                <X className="size-3" /> Drop
              </button>
            </>
          ) : (
            <>
              <span className="text-subtle">{hint.note ?? 'No suggestion.'}</span>
              <button type="button" onClick={() => setHint(null)} className={small}>
                <X className="size-3" /> Close
              </button>
            </>
          )}
          {hint.model && <span className="ml-auto text-[11px] text-subtle">by {hint.model}</span>}
        </span>
      )}
    </>
  )
}
