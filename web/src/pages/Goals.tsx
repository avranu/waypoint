// A project's goals (WP-382, WP-ADR50): what people intend, in plain words,
// and how the work traces back to it. People write goals in a line; Waypoint
// does the upkeep: it flags a goal that says how instead of what, guesses
// which goal untraced work serves (one click links it), and holds agents'
// links and goals until a person stands behind them.
//
// Wide screens put the goal tree on the left and what waits for a person on
// the right; narrow ones stack the queues first. Every suggestion is
// answered the same way (components/goals/actions.tsx).

import { useMemo, useRef, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  Archive,
  BellOff,
  ChevronDown,
  ChevronRight,
  Check,
  CircleCheck,
  CircleDot,
  ExternalLink,
  Layers,
  Link2,
  MoreHorizontal,
  Pencil,
  Plus,
  Scale,
  Sparkles,
  Target,
  X,
} from 'lucide-react'
import {
  designApi,
  goalApi,
  goalWorkQuery,
  goalsQuery,
  groundGroupOf,
  groupCounts,
  invalidateKnowledge,
} from '@/lib/knowledge'
import { useDebounced } from '@/lib/hooks'
import {
  GROUNDINGS,
  type DriftFlag,
  type GoalLink,
  type GoalView,
  type GoalsOverview,
  type Grounding,
  type KItem,
  type UntracedItem,
} from '@/lib/types'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { Explainer } from '@/components/ui/help'
import { toast, toastError } from '@/components/ui/toast'
import { ItemIcon, ItemTitle } from '@/components/Links'
import { AgentName } from '@/components/knowledge'
import { vizClass } from '@/components/charts'
import { GROUP_COLOR, GoalLinkText, GroundingBar, GroundingChip } from '@/components/goals'
import { ChoiceButtons, GoalPicker, type GoalOption } from '@/components/goals/actions'
import { GroundingLegend, WeekChart } from '@/components/goals/overview'
import { useDismissedWarnings, useStoredSet, warningKey } from '@/components/goals/store'

export function ProjectGoalsPage() {
  const { key } = useParams({ from: '/app/p/$key/goals' })
  const goals = useQuery(goalsQuery(key))
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-[1680px] space-y-4 px-3 pt-3 sm:px-6 sm:pt-5">
        {goals.isPending ? (
          <div className="space-y-3">
            <Skeleton className="h-8 w-48" />
            <Skeleton className="h-24" />
            <Skeleton className="h-40" />
          </div>
        ) : goals.isError ? (
          <div
            role="alert"
            className="flex flex-wrap items-center gap-2 rounded-xl border border-line bg-panel p-4 text-[13px]"
          >
            <span className="text-danger">Couldn’t load the goals: {goals.error.message}</span>
            <Button size="xs" variant="subtle" onClick={() => void goals.refetch()}>
              Try again
            </Button>
          </div>
        ) : (
          <GoalsBody o={goals.data} />
        )}
      </div>
    </div>
  )
}

function useRefresh() {
  const qc = useQueryClient()
  return () => invalidateKnowledge(qc)
}

function GoalsBody({ o }: { o: GoalsOverview }) {
  const [adding, setAdding] = useState(false)
  const active = o.goals.length > 0
  const approvedGoals = useMemo<GoalOption[]>(
    () =>
      o.goals
        .filter((g) => g.note.approved || g.note.requested)
        .map((g) => ({ id: g.note.id, title: g.note.title })),
    [o.goals],
  )
  return (
    <>
      <header className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="flex items-center gap-2 text-lg font-semibold tracking-tight">
          <Target className="size-5 text-subtle" aria-hidden /> Goals
        </h1>
        <div className="flex flex-wrap items-center gap-2">
          <Explainer id="goals:how" label="How goals work">
            <div className="space-y-1.5 text-[13px]">
              <p>
                Goals say what {o.project_name} is for, in plain words; tasks, pages, pull requests
                and code trace back to them through links. A link you make counts as yours; an
                agent’s link and Waypoint’s guesses wait for you to link or reject them.
              </p>
              <p>
                A model reads your tasks only when you click <b>Suggest goals</b> here or{' '}
                <b>Suggest</b> on a task; what it proposes counts once you approve it. Agents read
                the same from <code>wp goals</code> and <code>wp trace</code>.
              </p>
            </div>
          </Explainer>
          {o.can_approve && <SuggestGoals project={o.project} />}
          {o.can_edit && active && !adding && (
            <Button
              size="sm"
              variant="primary"
              icon={<Plus className="size-3.5" />}
              onClick={() => setAdding(true)}
            >
              Add goal
            </Button>
          )}
        </div>
      </header>
      {o.can_edit && (adding || !active) && (
        <AddGoal
          project={o.project}
          onDone={() => setAdding(false)}
          onAdded={() => setAdding(true)}
          autoFocus={adding}
        />
      )}
      {!active && !o.can_edit && (
        <Empty icon={<Target />} title="No goals yet">
          A person with edit rights on {o.project_name} can add the first one.
        </Empty>
      )}
      {active && <Overview o={o} />}
      {active && <Needs o={o} />}
      {active && (
        <div className="grid items-start gap-4 lg:grid-cols-[minmax(0,1.35fr)_minmax(0,1fr)]">
          <div className="order-2 min-w-0 lg:order-1">
            <GoalList o={o} />
          </div>
          <div className="order-1 min-w-0 space-y-4 lg:order-2">
            {o.drift_flags.length > 0 && (
              <DriftToAnswer flags={o.drift_flags} canApprove={o.can_approve} />
            )}
            {o.links.length > 0 && (
              <WaitingLinks links={o.links} canApprove={o.can_approve} goals={approvedGoals} />
            )}
            <Untraced o={o} goals={approvedGoals} />
            {o.unapproved_count > 0 && (
              <Unapproved count={o.unapproved_count} project={o.project} />
            )}
          </div>
        </div>
      )}
    </>
  )
}

/**
 * Asks the model for goals from people's untraced tasks and clearer words
 * for flagged goals, now (the weekly check does it too). Everything it
 * proposes shows below for one click each.
 */
function SuggestGoals({ project }: { project: string }) {
  const refresh = useRefresh()
  const run = useMutation({
    mutationFn: () => goalApi.suggest(project),
    onSuccess: (r) => {
      const parts = [
        r.goals.length && `${r.goals.length} ${r.goals.length === 1 ? 'goal' : 'goals'}`,
        r.rewordings &&
          `clearer words for ${r.rewordings} ${r.rewordings === 1 ? 'goal' : 'goals'}`,
      ].filter(Boolean)
      const by = r.model ? ` (${r.model} read ${r.read} ${r.read === 1 ? 'task' : 'tasks'})` : ''
      toast(
        parts.length
          ? `Waypoint suggests ${parts.join(' and ')}${by}: approve or dismiss each below.`
          : (r.note ?? 'Nothing new to suggest.'),
        { ms: 8000 },
      )
      refresh()
    },
    onError: toastError,
  })
  return (
    <Button
      size="sm"
      icon={<Sparkles className="size-3.5" />}
      loading={run.isPending}
      onClick={() => run.mutate()}
      title="Waypoint reads the tasks people wrote that no goal covers yet and proposes goals in plain words; nothing counts until you approve it"
    >
      {run.isPending ? 'Reading your tasks…' : 'Suggest goals'}
    </Button>
  )
}

const EXAMPLES = [
  'Travellers find a route in one tap',
  'Nobody loses work when the server restarts',
  'A new teammate ships a change on day one',
]

function AddGoal({
  project,
  parent,
  onDone,
  onAdded,
  autoFocus,
  compact,
}: {
  project: string
  parent?: GoalView
  onDone?: () => void
  /** Runs after each add, so a parent that shows the box only while empty keeps it open. */
  onAdded?: () => void
  autoFocus?: boolean
  compact?: boolean
}) {
  const refresh = useRefresh()
  const [title, setTitle] = useState('')
  const ref = useRef<HTMLInputElement>(null)
  const add = useMutation({
    mutationFn: (t: string) =>
      designApi.create({ title: t, kind: 'goal', project, parent: parent?.note.id }),
    onSuccess: (n) => {
      setTitle('')
      toast(`Added the goal “${n.title}”`)
      refresh()
      onAdded?.()
      // The box stays for the next goal; Esc or Cancel closes it.
      ref.current?.focus()
    },
    onError: toastError,
  })
  const submit = () => {
    const t = title.trim()
    if (t && !add.isPending) add.mutate(t)
  }
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        submit()
      }}
      className={clsx(!compact && 'rounded-xl border border-line bg-panel p-3')}
    >
      <div className="flex flex-col gap-2 sm:flex-row">
        <Input
          ref={ref}
          value={title}
          autoFocus={autoFocus}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') onDone?.()
          }}
          placeholder={parent ? 'A narrower outcome it needs' : `An outcome, e.g. ${EXAMPLES[0]}`}
          aria-label={parent ? `A goal under ${parent.note.title}` : 'Add goal'}
          className="h-9"
        />
        <div className="flex shrink-0 gap-2">
          <Button type="submit" variant="primary" loading={add.isPending} disabled={!title.trim()}>
            Add goal
          </Button>
          {onDone && (compact || parent) && (
            <Button type="button" variant="ghost" onClick={onDone}>
              Cancel
            </Button>
          )}
        </div>
      </div>
    </form>
  )
}

function pct(share: number) {
  return `${Math.round(share * 100)}%`
}

function Overview({ o }: { o: GoalsOverview }) {
  const open = GROUNDINGS.reduce((n, g) => n + (o.totals[g] ?? 0), 0) + o.tracing
  const grounded = o.totals.person_goal ?? 0
  const d = o.drift
  return (
    <Section
      title="Open work"
      actions={
        o.tracing > 0 && (
          <span
            className="flex items-center gap-1.5 text-xs text-subtle"
            data-testid="goals-tracing"
            title={`Following links for ${o.tracing} new ${o.tracing === 1 ? 'task' : 'tasks'}: its parent, the goals and notes it links to, and the items that mention it. No AI model is involved; the counts update in a minute or so.`}
          >
            <Spinner className="size-3.5" />
            {o.tracing}
          </span>
        )
      }
    >
      <div className="grid gap-x-8 gap-y-4 px-4 py-3 md:grid-cols-2">
        {open === 0 ? (
          <p className="text-[13px] text-subtle">No open tasks.</p>
        ) : (
          <div className="min-w-0 space-y-2.5">
            <div
              className="flex items-baseline gap-2"
              title={`${grounded} of ${open} open tasks trace to an approved goal through links people made`}
            >
              <span className="text-2xl font-semibold tabular-nums leading-none">{grounded}</span>
              <span className="text-sm tabular-nums text-muted">/ {open} open</span>
              <Badge>{Math.round((grounded / open) * 100)}%</Badge>
            </div>
            <GroundingBar counts={o.totals} className="h-2.5" />
            <GroundingLegend counts={o.totals} />
          </div>
        )}
        {d && <WeekChart d={d} />}
      </div>
    </Section>
  )
}

/** Where to look first: what waits for a person, each a jump to its place. */
function Needs({ o }: { o: GoalsOverview }) {
  const suggested = o.goals.filter((g) => !g.note.approved && !g.note.requested).length
  const chips = [
    { id: 'goals-suggested', n: suggested, text: 'to approve' },
    { id: 'goals-drift', n: o.drift_flags.length, text: 'to answer' },
    { id: 'goals-links', n: o.links.length, text: 'to check' },
    { id: 'goals-untraced', n: o.untraced_total, text: 'not traced' },
  ].filter((c) => c.n > 0)
  if (chips.length === 0) return null
  return (
    <nav aria-label="Jump to" className="flex flex-wrap gap-1.5">
      {chips.map((c) => (
        <button
          key={c.id}
          type="button"
          onClick={() =>
            document.getElementById(c.id)?.scrollIntoView({ behavior: 'smooth', block: 'start' })
          }
          className="inline-flex h-6 items-center gap-1 rounded-md bg-sunken px-2 text-xs text-muted hover:bg-hover hover:text-fg"
        >
          <b className="font-semibold tabular-nums text-fg">{c.n}</b> {c.text}
        </button>
      ))}
    </nav>
  )
}

function GoalList({ o }: { o: GoalsOverview }) {
  const folded = useStoredSet(`wp:goals:folded:${o.project}`)
  const dismissed = useDismissedWarnings(o.project)
  const rows: { g: GoalView; kids: number }[] = []
  let skipBelow: number | null = null
  o.goals.forEach((g, i) => {
    if (skipBelow !== null && g.depth > skipBelow) return
    skipBelow = null
    let kids = 0
    for (let j = i + 1; j < o.goals.length && o.goals[j].depth > g.depth; j++) kids++
    rows.push({ g, kids })
    if (kids && folded.has(String(g.note.id))) skipBelow = g.depth
  })
  const firstSuggested = o.goals.find((g) => !g.note.approved && !g.note.requested)?.note.id
  return (
    <Section
      title={
        <span className="flex items-center gap-1.5">
          Goals <Badge>{o.goals.length}</Badge>
        </span>
      }
    >
      <ul className="divide-y divide-line">
        {rows.map(({ g, kids }) => (
          <GoalRow
            key={g.note.id}
            g={g}
            o={o}
            kids={kids}
            folded={folded.has(String(g.note.id))}
            onFold={() => folded.toggle(String(g.note.id))}
            dismissed={dismissed}
            anchor={g.note.id === firstSuggested}
          />
        ))}
      </ul>
    </Section>
  )
}

function Stat({ icon, n, tip }: { icon: ReactNode; n: number; tip: string }) {
  return (
    <span
      title={tip}
      aria-label={tip}
      className={clsx('inline-flex items-center gap-1 tabular-nums', n === 0 && 'opacity-50')}
    >
      <span className="text-subtle" aria-hidden>
        {icon}
      </span>
      {n}
    </span>
  )
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`

function GoalRow({
  g,
  o,
  kids,
  folded,
  onFold,
  dismissed,
  anchor,
}: {
  g: GoalView
  o: GoalsOverview
  kids: number
  folded: boolean
  onFold: () => void
  dismissed: ReturnType<typeof useDismissedWarnings>
  anchor: boolean
}) {
  const refresh = useRefresh()
  const n = g.note
  const menu = usePopover()
  const navigate = useNavigate()
  const [sub, setSub] = useState(false)
  const [work, setWork] = useState(false)
  const [draft, setDraft] = useState<string | null>(null)
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => designApi.update(n.id, body),
    onSuccess: refresh,
    onError: toastError,
  })
  const approve = useMutation({
    mutationFn: () => designApi.approve(n.id, n.updated_at),
    onSuccess: () => {
      toast(`Approved “${n.title}”`)
      refresh()
    },
    onError: toastError,
  })
  const review = useMutation({
    mutationFn: ({ rid, action }: { rid: number; action: 'approve' | 'reject' }) =>
      designApi.review(rid, action),
    onSuccess: refresh,
    onError: toastError,
  })
  const c = g.coverage
  const suggested = !n.approved && !n.requested
  const rw = g.rewording
  const commit = () => {
    const t = draft?.trim()
    if (save.isPending) return
    if (!t || t === n.title) return setDraft(null)
    // The field stays until the save lands, so a failure keeps what was typed.
    save.mutate({ title: t }, { onSuccess: () => setDraft(null) })
  }
  const warnings = (g.wording ?? []).filter((w) => !dismissed.has(warningKey(n.id, w)))
  const indent = Math.min(g.depth, 3) * 1.25
  return (
    <li
      className={clsx('px-3 py-2.5', suggested && 'bg-warn/5')}
      data-testid="goal-row"
      id={anchor ? 'goals-suggested' : undefined}
    >
      <div className="flex items-start gap-1" style={{ paddingLeft: `${indent}rem` }}>
        {kids > 0 ? (
          <IconButton
            label={folded ? `Show ${plural(kids, 'goal')} under it` : 'Fold the goals under it'}
            size="xs"
            aria-expanded={!folded}
            onClick={onFold}
            icon={folded ? <ChevronRight className="size-4" /> : <ChevronDown className="size-4" />}
          />
        ) : (
          <span className="size-6 shrink-0" aria-hidden />
        )}
        <div className="flex min-w-0 flex-1 flex-wrap items-center gap-x-3 gap-y-1.5 pt-0.5">
          <div className="flex min-w-0 flex-[1_1_15rem] flex-wrap items-center gap-x-2 gap-y-1">
            {draft !== null ? (
              <Input
                value={draft}
                autoFocus
                onChange={(e) => setDraft(e.target.value)}
                onBlur={commit}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') commit()
                  if (e.key === 'Escape') setDraft(null)
                }}
                aria-label="Goal"
                className="h-8 min-w-0 flex-1 text-[14px] font-medium"
              />
            ) : (
              <button
                type="button"
                disabled={!n.can_edit}
                onClick={() => setDraft(n.title)}
                title={n.can_edit ? 'Click to reword' : undefined}
                className="min-w-0 text-left text-[14px] font-medium leading-snug enabled:hover:text-accent disabled:cursor-text"
              >
                {n.title}
              </button>
            )}
            {folded && kids > 0 && (
              <Badge className="cursor-default">
                <span title={`${plural(kids, 'goal')} folded under it`}>+{kids}</span>
              </Badge>
            )}
            {suggested && (
              <span className="inline-flex shrink-0 items-center gap-1 rounded bg-warn/15 px-1.5 text-[11px] font-medium text-warn">
                Suggested
                {n.created_agent && (
                  <>
                    {' '}
                    by <AgentName agent={n.created_agent} className="text-warn" />
                  </>
                )}
              </span>
            )}
            {n.requested && (
              <span
                className="shrink-0 text-[11px] text-subtle"
                title={`It counts as approved: ${n.requested.by?.display_name || 'a person'} asked for it in ${n.requested.issue}, and this goal carries that task out`}
              >
                requested in{' '}
                <Link
                  to="/issue/$key"
                  params={{ key: n.requested.issue }}
                  className="hover:text-fg hover:underline"
                >
                  {n.requested.issue}
                </Link>
                {n.requested.by && (
                  <> by {n.requested.by.display_name || n.requested.by.username}</>
                )}
              </span>
            )}
            {warnings.map((w) => (
              <span
                key={w}
                title={w.charAt(0).toUpperCase() + w.slice(1)}
                className="inline-flex h-5 shrink-0 items-center gap-1 rounded bg-warn/10 pl-1.5 pr-0.5 text-[11px] font-medium text-warn"
              >
                <AlertTriangle className="size-3" aria-hidden />
                {(() => {
                  const t = w.split(':')[0]
                  return t.charAt(0).toUpperCase() + t.slice(1)
                })()}
                {n.can_edit && !rw && (
                  <button
                    type="button"
                    aria-label="Reword"
                    title="Reword"
                    onClick={() => setDraft(n.title)}
                    className="flex size-4 items-center justify-center rounded hover:bg-warn/20"
                  >
                    <Pencil className="size-3" />
                  </button>
                )}
                <button
                  type="button"
                  aria-label="Dismiss this warning"
                  title="Dismiss this warning"
                  onClick={() => dismissed.add(warningKey(n.id, w))}
                  className="flex size-4 items-center justify-center rounded hover:bg-warn/20"
                >
                  <X className="size-3" />
                </button>
              </span>
            ))}
          </div>
          {!suggested && (
            <div className="flex shrink-0 items-center gap-3 text-xs text-muted">
              <GroundingBar counts={c.by_grounding} className="w-24 sm:w-32" />
              <Stat
                icon={<Layers className="size-3.5" />}
                n={c.items}
                tip={`${plural(c.items, 'item')}: tasks, pages and notes that trace to it`}
              />
              <Stat icon={<CircleDot className="size-3.5" />} n={c.open} tip={`${c.open} open`} />
              <Stat
                icon={<CircleCheck className="size-3.5" />}
                n={c.finished_week}
                tip={`${c.finished_week} finished this week`}
              />
              {!!c.tension && (
                <Stat
                  icon={<Scale className="size-3.5" />}
                  n={c.tension}
                  tip={`${c.tension} open in tension with it`}
                />
              )}
            </div>
          )}
          {suggested && n.can_approve && (
            <ChoiceButtons
              yesLabel="Approve"
              yesHint="Approve it as one of the project's goals"
              noHint="It isn't a goal: drop it (it's retired, not deleted)"
              busy={approve.isPending || save.isPending}
              onYes={() => approve.mutate()}
              onNo={() => save.mutate({ status: 'retired' })}
            />
          )}
        </div>
        <div className="flex shrink-0 items-center">
          {o.can_edit && (n.approved || !!n.requested) && (
            <IconButton
              label="Link tasks…"
              size="xs"
              onClick={() => setWork(!work)}
              icon={<Link2 className="size-4" />}
            />
          )}
          <IconButton
            ref={menu.anchor}
            label={`More for ${n.title}`}
            size="xs"
            onClick={menu.toggle}
            icon={<MoreHorizontal className="size-4" />}
          />
        </div>
        <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
          <MenuList
            onClose={menu.close}
            items={[
              {
                label: 'Open note',
                icon: <ExternalLink className="size-3.5" />,
                onSelect: () => void navigate({ to: '/design/$id', params: { id: String(n.id) } }),
              },
              ...(n.can_edit
                ? [
                    {
                      label: 'Reword',
                      icon: <Pencil className="size-3.5" />,
                      onSelect: () => setDraft(n.title),
                    },
                    {
                      label: 'Add a goal under it',
                      icon: <Plus className="size-3.5" />,
                      onSelect: () => setSub(true),
                    },
                    {
                      label: 'Retire',
                      icon: <Archive className="size-3.5" />,
                      onSelect: () => save.mutate({ status: 'retired' }),
                      danger: true,
                    },
                  ]
                : []),
            ]}
          />
        </Popover>
      </div>
      {(g.why || rw || work || sub) && (
        <div className="mt-1.5 space-y-1.5" style={{ paddingLeft: `${indent + 1.75}rem` }}>
          {g.why && <p className="text-xs text-muted">{g.why}</p>}
          {rw && (
            <div
              className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-md bg-sunken px-2 py-1.5 text-xs"
              data-testid="goal-rewording"
            >
              <span
                className="min-w-0 text-muted"
                title={
                  rw.agent?.startsWith('waypoint/') ? 'Waypoint suggests' : 'An agent suggests'
                }
              >
                <Sparkles className="mr-1 inline size-3 align-[-2px] text-accent" aria-hidden />
                <span className="font-medium text-fg">“{rw.title}”</span>
              </span>
              {n.can_approve && (
                <ChoiceButtons
                  yesLabel="Use it"
                  noLabel="Keep mine"
                  yesHint="Take these words for the goal"
                  noHint="Keep the goal's current words"
                  busy={review.isPending}
                  onYes={() => review.mutate({ rid: rw.revision_id, action: 'approve' })}
                  onNo={() => review.mutate({ rid: rw.revision_id, action: 'reject' })}
                />
              )}
            </div>
          )}
          {work && <LinkWork goal={n.id} project={o.project} onClose={() => setWork(false)} />}
          {sub && (
            <AddGoal
              project={o.project}
              parent={g}
              compact
              autoFocus
              onDone={() => setSub(false)}
            />
          )}
        </div>
      )}
    </li>
  )
}

/**
 * The open tasks likeliest to serve a goal, to tick and link in one go, so
 * nobody opens every task to say which goal it serves (WP-679). A search
 * finds any open task by key or words.
 */
function LinkWork({
  goal,
  project,
  onClose,
}: {
  goal: number
  project: string
  onClose: () => void
}) {
  const refresh = useRefresh()
  const [q, setQ] = useState('')
  const dq = useDebounced(q.trim(), 250)
  const list = useQuery(goalWorkQuery(goal, project, dq))
  const [picked, setPicked] = useState<Record<string, KItem>>({})
  const n = Object.keys(picked).length
  const link = useMutation({
    // Each task links on its own; the ones that fail stay ticked to retry.
    mutationFn: async (items: [string, KItem][]) => {
      const res = await Promise.allSettled(items.map(([, it]) => goalApi.link(itemEnd(it), goal)))
      const linked = items.filter((_, i) => res[i].status === 'fulfilled').map(([k]) => k)
      const failed = res.find((r): r is PromiseRejectedResult => r.status === 'rejected')
      return { linked, failed: res.length - linked.length, error: failed?.reason as unknown }
    },
    onSuccess: ({ linked, failed, error }) => {
      setPicked((p) => {
        const next = { ...p }
        for (const k of linked) delete next[k]
        return next
      })
      const what = (c: number) => `${c} ${c === 1 ? 'task' : 'tasks'}`
      if (linked.length) toast(`Linked ${what(linked.length)} to the goal`)
      if (failed) toastError(error ?? new Error(`Couldn’t link ${what(failed)}`))
    },
    onError: toastError,
    onSettled: refresh,
  })
  const toggle = (it: KItem) =>
    setPicked((p) => {
      const k = it.key ?? String(it.id)
      const next = { ...p }
      if (next[k]) delete next[k]
      else next[k] = it
      return next
    })
  const items = list.data?.items ?? []
  return (
    <div
      className="space-y-2 rounded-lg border border-line bg-sunken/40 p-2.5"
      data-testid="goal-work"
    >
      <div className="flex flex-wrap items-center gap-2">
        <Input
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Find a task by key or words"
          aria-label="Find a task"
          className="h-8 w-full min-w-0 text-[13px] sm:w-auto sm:flex-1"
          autoFocus
        />
        <Button
          size="xs"
          variant="primary"
          icon={<Link2 className="size-3" />}
          disabled={n === 0}
          loading={link.isPending}
          onClick={() => link.mutate(Object.entries(picked))}
        >
          {n ? `Link ${n} ${n === 1 ? 'task' : 'tasks'}` : 'Link'}
        </Button>
        <Button size="xs" variant="ghost" onClick={onClose}>
          Done
        </Button>
      </div>
      <p className="text-xs text-subtle">
        {dq
          ? 'Open tasks matching your search.'
          : 'Open tasks that share words with this goal, likeliest first. Tick the ones it covers.'}
      </p>
      {list.isPending ? (
        <Skeleton className="h-16" />
      ) : list.isError ? (
        <p role="alert" className="text-xs text-danger">
          Couldn’t list tasks: {list.error.message}
        </p>
      ) : items.length === 0 ? (
        <p className="text-xs text-subtle">
          {dq
            ? 'No open task matches.'
            : 'No open task looks like it serves this goal; search for one.'}
        </p>
      ) : (
        <ul className="max-h-80 divide-y divide-line overflow-y-auto">
          {items.map((w) => {
            const k = w.item.key ?? String(w.item.id)
            return (
              <li key={k}>
                <label className="flex cursor-pointer items-start gap-2 py-1.5 text-[13px]">
                  <input
                    type="checkbox"
                    checked={!!picked[k]}
                    disabled={link.isPending}
                    onChange={() => toggle(w.item)}
                    className="mt-1 accent-[var(--wp-accent)]"
                    aria-label={`${w.item.key} ${w.item.title}`}
                  />
                  <span className="min-w-0 flex-1">
                    <span className="flex min-w-0 items-center gap-1.5">
                      <ItemIcon item={w.item} />
                      <span className="shrink-0 text-xs text-subtle">{w.item.key}</span>
                      <span className="min-w-0 truncate">{w.item.title}</span>
                    </span>
                    {(w.reason || (w.grounding && w.grounding !== 'none')) && (
                      <span className="flex flex-wrap items-center gap-1.5 pl-5 text-xs text-subtle">
                        {w.grounding && w.grounding !== 'none' && (
                          <GroundingChip grounding={w.grounding} />
                        )}
                        {w.reason}
                      </span>
                    )}
                  </span>
                </label>
              </li>
            )
          })}
        </ul>
      )}
      {list.data && list.data.matched > items.length && (
        <p className="text-xs text-subtle">
          Showing the best {items.length} of {list.data.matched}; search to find others.
        </p>
      )}
    </div>
  )
}

function basisText(l: { basis: string; agent?: string; score?: number }): string {
  if (l.basis === 'inferred')
    return `Waypoint's guess${l.score !== undefined ? ` (${pct(l.score)} alike)` : ''}`
  return l.agent ? `Linked by an agent (${l.agent})` : 'Linked by an agent'
}

/** The cap on a queue, and the button that shows the rest. */
/** How many links the server lists at once (goalListMax in internal/core/goals.go). */
const LIST_MAX = 60

function useCap<T>(items: T[], cap = 5) {
  const [all, setAll] = useState(false)
  const shown = all ? items : items.slice(0, cap)
  const more = items.length - shown.length
  const footer =
    items.length > cap ? (
      <button
        type="button"
        onClick={() => setAll(!all)}
        className="w-full border-t border-line px-4 py-2 text-xs text-muted hover:bg-hover hover:text-fg"
      >
        {all ? 'Show fewer' : `Show ${more} more`}
      </button>
    ) : null
  return { shown, footer }
}

function QueueTitle({ text, n, tip }: { text: string; n: number | string; tip: string }) {
  return (
    <span className="flex items-center gap-1.5" title={tip}>
      {text} <Badge>{n}</Badge>
    </span>
  )
}

/** What a group is, as a small chip (the agent-made kinds are one: Agent). */
function GroupChip({ grounding, title }: { grounding: Grounding; title?: string }) {
  const group = groundGroupOf(grounding)
  const color = GROUP_COLOR[group.id]
  return (
    <span
      title={title ?? group.hint}
      className={clsx(
        'inline-flex h-5 shrink-0 items-center gap-1 rounded-md px-1.5 text-[11px] font-medium',
        vizClass,
      )}
      style={{
        background: `color-mix(in oklab, ${color} 15%, transparent)`,
        color: `color-mix(in oklab, ${color} 75%, var(--wp-fg))`,
      }}
    >
      <span aria-hidden className="size-1.5 rounded-full" style={{ background: color }} />
      {group.label}
    </span>
  )
}

function QueueRow({
  item,
  meta,
  actions,
  note,
}: {
  item: KItem
  meta?: ReactNode
  actions?: ReactNode
  note?: ReactNode
}) {
  return (
    <li className="space-y-1.5 px-4 py-2.5 text-[13px]">
      <div className="flex min-w-0 items-center gap-1.5">
        <ItemIcon item={item} />
        <ItemTitle item={item} />
      </div>
      {meta && (
        <div className="flex min-w-0 flex-wrap items-center gap-1.5 pl-5 text-xs text-muted">
          {meta}
        </div>
      )}
      {note}
      {actions && <div className="pl-5">{actions}</div>}
    </li>
  )
}

function DriftToAnswer({ flags, canApprove }: { flags: DriftFlag[]; canApprove: boolean }) {
  const refresh = useRefresh()
  const answer = useMutation({
    mutationFn: ({ id, to }: { id: number; to: 'intent' | 'stop' }) => goalApi.answerDrift(id, to),
    onSuccess: refresh,
    onError: toastError,
  })
  const cap = useCap(flags, 3)
  return (
    <div id="goals-drift">
      <Section
        title={
          <QueueTitle
            text="Drift"
            n={flags.length}
            tip="A model read this work against your goals and thinks it drifts from them. It is only a model's read: it doesn't hold up the merge, and your answer decides what counts. Stop this holds a pull request that hasn't merged, and the work's open tasks."
          />
        }
      >
        <ul className="divide-y divide-line" data-testid="goal-drift">
          {cap.shown.map((f) => (
            <li key={f.id} className="space-y-1.5 px-4 py-2.5 text-[13px]">
              <div className="font-medium break-words">{f.headline}</div>
              {f.pr && (
                <a
                  href={f.pr.url}
                  target="_blank"
                  rel="noreferrer"
                  className="block break-words text-xs text-muted hover:underline"
                >
                  {f.pr.repo}#{f.pr.number} {f.pr.title}
                  {f.pr.merged ? ' · merged' : ''}
                </a>
              )}
              <p className="line-clamp-2 text-xs break-words text-muted" title={f.reason}>
                {f.reason}
              </p>
              {f.items.map((it) => (
                <div key={`${it.type}:${it.id}`} className="flex min-w-0 items-center gap-1.5">
                  <ItemIcon item={it} />
                  <ItemTitle item={it} />
                </div>
              ))}
              <p className="text-xs text-subtle">
                {f.answer === 'stop' ? 'You said to stop this. ' : ''}
                {f.model ? `A model's read (${f.model})` : "A model's read"}
              </p>
              {canApprove && (
                <ChoiceButtons
                  yesLabel="Fine"
                  yesHint="Fine: record it as intent"
                  noLabel="Stop this"
                  noHint="Hold the pull request and the work's open tasks"
                  busy={answer.isPending}
                  onYes={() => answer.mutate({ id: f.id, to: 'intent' })}
                  onNo={
                    f.answer !== 'stop' ? () => answer.mutate({ id: f.id, to: 'stop' }) : undefined
                  }
                />
              )}
            </li>
          ))}
        </ul>
        {cap.footer}
      </Section>
    </div>
  )
}

function WaitingLinks({
  links,
  canApprove,
  goals,
}: {
  links: GoalLink[]
  canApprove: boolean
  goals: GoalOption[]
}) {
  const refresh = useRefresh()
  const confirm = useMutation({
    mutationFn: (id: number) => goalApi.confirm(id),
    onSuccess: refresh,
    onError: toastError,
  })
  const dismiss = useMutation({
    mutationFn: (id: number) => goalApi.dismiss(id),
    onSuccess: refresh,
    onError: toastError,
  })
  const other = useMutation({
    mutationFn: async ({ l, goal }: { l: GoalLink; goal: number }) => {
      await goalApi.link(itemEnd(l.item), goal)
      await goalApi.dismiss(l.link_id)
    },
    onSettled: refresh,
    onError: toastError,
  })
  // Not a bulk confirm: a person can't have read them all, so each keeps
  // counting as the agent's or Waypoint's; this only stops asking.
  // The server lists at most this many; the rest wait behind them.
  const capped = links.length >= LIST_MAX
  const all = useMutation({
    mutationFn: () => goalApi.acknowledge(links.map((l) => l.link_id)),
    onSuccess: (r) => {
      toast(
        `Stopped asking about ${r.acknowledged} ${r.acknowledged === 1 ? 'link' : 'links'}; they still count as unconfirmed${capped ? '. More are waiting.' : ''}`,
      )
      refresh()
    },
    onError: (e) => {
      toastError(e)
      refresh()
    },
  })
  const cap = useCap(links)
  const busy = confirm.isPending || dismiss.isPending || other.isPending || all.isPending
  return (
    <div id="goals-links">
      <Section
        title={
          <QueueTitle
            text="Links to check"
            n={capped ? `${links.length}+` : links.length}
            tip="An agent linked these to a goal, or Waypoint guessed the link. Link one to stand behind it: it counts as yours. Drop removes the link; Other goal… moves it."
          />
        }
        actions={
          canApprove &&
          links.length > 1 && (
            <IconButton
              label={`Stop asking about ${capped ? `the first ${links.length}` : 'all'} of these (they still count as unconfirmed)`}
              size="xs"
              disabled={busy}
              onClick={() => all.mutate()}
              icon={<BellOff className="size-3.5" />}
            />
          )
        }
      >
        <ul className="divide-y divide-line">
          {cap.shown.map((l) => (
            <QueueRow
              key={l.link_id}
              item={l.item}
              meta={
                <>
                  <GroupChip
                    grounding={l.basis === 'inferred' ? 'inferred_goal' : 'agent_goal'}
                    title={basisText(l)}
                  />
                  <span>serves</span>
                  <GoalLinkText goal={l.goal} />
                </>
              }
              note={
                linkNote(l) && (
                  <p className="line-clamp-1 pl-5 text-xs text-subtle" title={linkNote(l)}>
                    {linkNote(l)}
                  </p>
                )
              }
              actions={
                canApprove && (
                  <ChoiceButtons
                    busy={busy}
                    yesHint="Stand behind this link: it counts as yours"
                    noHint="It doesn't serve this goal: drop the link"
                    onYes={() => confirm.mutate(l.link_id)}
                    onNo={() => dismiss.mutate(l.link_id)}
                    other={
                      <GoalPicker
                        goals={goals.filter((g) => g.id !== l.goal.id)}
                        label="Other goal…"
                        busy={busy}
                        onPick={(goal) => other.mutate({ l, goal })}
                      />
                    }
                  />
                )
              }
            />
          ))}
        </ul>
        {cap.footer}
      </Section>
    </div>
  )
}

const GUESS_PREFIX = "Waypoint's guess: "

/** The note under a waiting link: why Waypoint guessed it, or the agent's own words. */
function linkNote(l: { basis: string; note?: string }): string {
  if (!l.note) return ''
  if (l.basis === 'inferred' && l.note.startsWith(GUESS_PREFIX)) {
    const why = l.note.slice(GUESS_PREFIX.length)
    return why.charAt(0).toUpperCase() + why.slice(1)
  }
  return `“${l.note}”`
}

function Untraced({ o, goals }: { o: GoalsOverview; goals: GoalOption[] }) {
  const [filter, setFilter] = useState<string>('')
  const counts = useMemo(() => {
    const m: Partial<Record<Grounding, number>> = {}
    for (const u of o.untraced) m[u.grounding] = (m[u.grounding] ?? 0) + 1
    return m
  }, [o.untraced])
  const kinds = groupCounts(counts)
  const filtered = filter
    ? o.untraced.filter((u) => groundGroupOf(u.grounding).id === filter)
    : o.untraced
  const cap = useCap(filtered)
  return (
    <div id="goals-untraced">
      <Section
        title={
          <QueueTitle
            text="Not traced to an approved goal"
            n={o.untraced_total}
            tip="Open work no approved goal covers through links. Link what serves a goal in one click; what serves none may be drift, or a goal nobody wrote down yet."
          />
        }
      >
        {o.untraced.length === 0 ? (
          <Empty icon={<Check />} title="Everything open traces to an approved goal." />
        ) : (
          <>
            {kinds.length > 1 && (
              <div className="flex flex-wrap gap-1.5 border-b border-line px-4 py-2">
                <FilterChip on={filter === ''} onClick={() => setFilter('')}>
                  All {o.untraced.length}
                </FilterChip>
                {kinds.map(({ group, n }) => (
                  <FilterChip
                    key={group.id}
                    on={filter === group.id}
                    onClick={() => setFilter(filter === group.id ? '' : group.id)}
                  >
                    {group.label} {n}
                  </FilterChip>
                ))}
              </div>
            )}
            <ul className="divide-y divide-line">
              {cap.shown.map((u) => (
                <UntracedRow
                  key={`${u.item.type}:${u.item.key ?? u.item.id}`}
                  u={u}
                  o={o}
                  goals={goals}
                />
              ))}
            </ul>
            {cap.footer}
            {o.untraced_total > o.untraced.length && (
              <p className="border-t border-line px-4 py-2 text-xs text-subtle">
                {o.untraced.length} of {o.untraced_total} ·{' '}
                <Link
                  to="/p/$key/list"
                  params={{ key: o.project }}
                  search={{ q: 'goal:none is:open' }}
                  className="underline-offset-2 hover:text-fg hover:underline"
                >
                  goal:none
                </Link>
              </p>
            )}
          </>
        )}
      </Section>
    </div>
  )
}

function FilterChip({
  on,
  onClick,
  children,
}: {
  on: boolean
  onClick: () => void
  children: ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={on}
      className={clsx(
        'h-6 rounded-md px-2 text-xs tabular-nums',
        on ? 'bg-accent-soft font-medium text-fg' : 'text-muted hover:bg-hover hover:text-fg',
      )}
    >
      {children}
    </button>
  )
}

function UntracedRow({ u, o, goals }: { u: UntracedItem; o: GoalsOverview; goals: GoalOption[] }) {
  const refresh = useRefresh()
  const link = useMutation({
    mutationFn: (goal: number) => goalApi.link(itemEnd(u.item), goal),
    onSuccess: refresh,
    onError: toastError,
  })
  const notThis = useMutation({
    mutationFn: (goal: number) => goalApi.dismissMatch(itemEnd(u.item), goal),
    onSuccess: refresh,
    onError: toastError,
  })
  const create = useMutation({
    mutationFn: async (title: string) => {
      const g = await designApi.create({ title, kind: 'goal', project: o.project })
      await goalApi.link(itemEnd(u.item), g.id)
    },
    onSuccess: refresh,
    onError: toastError,
  })
  const busy = link.isPending || notThis.isPending || create.isPending
  const cand = u.candidate
  const picker = (
    <GoalPicker
      goals={goals}
      label={cand ? 'Other goal…' : 'Link to a goal…'}
      busy={busy}
      onPick={(id) => link.mutate(id)}
      onCreate={(title) => create.mutate(title)}
    />
  )
  return (
    <QueueRow
      item={u.item}
      meta={
        <>
          <GroupChip grounding={u.grounding} title={o.words[u.grounding]} />
          {u.agent && (
            <span className="inline-flex items-center gap-1 text-subtle" title="Written by">
              <AgentName agent={u.agent} />
            </span>
          )}
          {cand && (
            <span className="inline-flex min-w-0 flex-wrap items-center gap-1" title={cand.reason}>
              <span className="text-subtle">maybe serves</span>
              <GoalLinkText goal={cand.goal} />
            </span>
          )}
        </>
      }
      actions={
        o.can_edit && (
          <ChoiceButtons
            busy={busy}
            yesHint={cand ? `It serves “${cand.goal.title}”: link it` : undefined}
            noHint={cand ? 'It doesn’t serve this goal: stop suggesting it' : undefined}
            onYes={cand?.goal.id ? () => link.mutate(cand.goal.id!) : undefined}
            onNo={cand?.goal.id ? () => notThis.mutate(cand.goal.id!) : undefined}
            other={picker}
          />
        )
      }
    />
  )
}

/**
 * Decisions and other design notes no person approved yet: they aren't
 * goals, so the tab only says how many wait and where to review them.
 */
function Unapproved({ count, project }: { count: number; project: string }) {
  return (
    <p
      className="px-1 text-xs text-subtle"
      data-testid="goals-unapproved"
      title="Work resting on them gets extra scrutiny until a person approves them"
    >
      {count} {count === 1 ? 'proposed note awaits' : 'proposed notes await'} approval.{' '}
      <Link
        to="/design/review"
        search={{ project }}
        className="text-muted underline-offset-2 hover:text-fg hover:underline"
      >
        Review
      </Link>
    </p>
  )
}

function itemEnd(it: KItem) {
  return it.type === 'issue' ? { type: it.type, key: it.key } : { type: it.type, id: it.id }
}
