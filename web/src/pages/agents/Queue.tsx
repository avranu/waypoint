// Agents page, Queue tab (WP-D15): what each agent will take next, in
// order, and why other delegated work waits. Tasks follow the automatic
// order (one score: priority, due date, unblocks, delegated, bug, planned, size, age; WP-997) exactly as wp next reads
// it; people hold a task in a place of their own by dragging it there (WP-87):
// only that task is pinned, the rest keep flowing around it.

import { useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  TouchSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import {
  ArrowRight,
  ArrowUpToLine,
  Ban,
  Bot,
  Check,
  ChevronDown,
  ChevronsUp,
  CirclePause,
  CirclePlay,
  ExternalLink,
  GripVertical,
  Hand,
  Hourglass,
  ListPlus,
  Lock,
  MoreHorizontal,
  Pin,
  PinOff,
  Plus,
  Search,
  UserX,
  Workflow as WorkflowIcon,
} from 'lucide-react'
import {
  q,
  useHoldIssue,
  useStartTaskSession,
  useUpdateIssue,
  type TaskSessionPlace,
} from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { useMediaQuery } from '@/lib/hooks'
import { useReview } from '@/components/Provenance'
import {
  queueQuery,
  usePauseAgents,
  useQueueMove,
  useReleaseClaim,
  useResumeAgents,
  type AgentPause,
  type AgentQueue,
  type QueueItem,
  type WaitReason,
} from '@/lib/queue'
import { duration, plural, relativeTime, shortDate } from '@/lib/format'
import type { Issue } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Input, Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover, type MenuItem } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { HelpButton, HelpPanel, useHelp } from '@/components/ui/help'
import { PriorityIcon, StatusIcon } from '@/components/issue/icons'
import { PriorityPicker } from '@/components/issue/pickers'
import { AgentChip, StepChip } from '@/components/issue/agentPickers'
import type { HubTabProps } from './search'

/**
 * `embedded`: shown inside a project's Agents tab, whose header owns the
 * title, the explainer and the pause control; `filters.project` is fixed there.
 */
export function QueueTab({ filters, embedded = false }: HubTabProps & { embedded?: boolean }) {
  const queue = useQuery(queueQuery(filters.agent, filters.project))
  const projects = useQuery(q.projects()).data ?? []
  const help0 = useHelp('agents:queue')
  const help = embedded ? { ...help0, shown: false } : help0
  const [adding, setAdding] = useState(false)
  const data = queue.data
  const editable = (key: string) => {
    const role = projects.find((p) => p.key === key)?.role
    return role === 'owner' || role === 'editor'
  }
  const anyWork = data && data.working.length + data.next.length + data.waiting.length > 0
  return (
    <div className="space-y-4">
      <div className="space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <div className="flex min-w-0 items-center gap-1">
            {!embedded && (
              <>
                <h2 className="text-[15px] font-semibold tracking-tight">{queueTitle(data)}</h2>
                <HelpButton help={help} label="the queue" />
              </>
            )}
          </div>
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="primary"
              icon={<ListPlus className="size-3.5" />}
              onClick={() => setAdding(true)}
            >
              Add work
            </Button>
            {!embedded && <PauseControl spec={filters.agent} pauses={data?.pauses ?? []} />}
          </div>
        </div>
        <HelpPanel help={help}>
          Agents take the first task here they match with <code>wp next</code>. The order weighs
          priority, the due date (more as it nears), the tasks it unblocks, whether it was handed to
          agents, bugs, plans, small estimates and time waiting; each row says why. Drag a task to{' '}
          <b>pin</b> it: it stays ahead of the tasks you put it ahead of.
        </HelpPanel>
      </div>

      {data && (
        <Banners
          queue={data}
          embedded={embedded}
          name={(key) => projects.find((p) => p.key === key)?.name ?? key}
        />
      )}

      {queue.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-28" />
          <Skeleton className="h-48" />
        </div>
      ) : queue.isError ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<Ban />} title="The queue didn't load">
            {(queue.error as Error)?.message}
          </Empty>
        </div>
      ) : !anyWork ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty
            icon={<Bot />}
            title={
              filters.agent
                ? `Nothing for ${agentLabel(filters.agent)}`
                : 'Nothing is waiting for agents'
            }
          >
            Delegate an issue with <b>Add work</b>.
          </Empty>
        </div>
      ) : (
        data && (
          <>
            {data.working.length > 0 && <Working items={data.working} editable={editable} />}
            <UpNext
              queue={data}
              agent={filters.agent}
              project={filters.project}
              editable={editable}
            />
            {data.waiting.length > 0 && <Waiting items={data.waiting} editable={editable} />}
          </>
        )
      )}
      <AddWork
        open={adding}
        onClose={() => setAdding(false)}
        spec={filters.agent}
        project={filters.project}
      />
    </div>
  )
}

function queueTitle(q?: AgentQueue) {
  if (!q || !q.agent) return 'Every agent’s queue'
  if (q.exact && q.covers.length === 1) return `${agentLabel(q.covers[0])}’s queue`
  return `Queue for ${q.agent}`
}

// ---- pauses ----

function Banners({
  queue,
  name,
  embedded,
}: {
  queue: AgentQueue
  name: (key: string) => string
  embedded?: boolean
}) {
  const resume = useResumeAgents()
  if (!queue.pauses.length && !queue.paused_projects.length) return null
  return (
    <div className="space-y-2">
      {queue.pauses.map((p) => (
        <div
          key={p.id}
          role="status"
          className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border border-warn/40 bg-warn/10 px-3 py-2.5"
        >
          <CirclePause className="size-4 shrink-0 text-warn" />
          <div className="min-w-0 flex-1 text-[13px]">
            <span className="font-medium">{pauseWhat(p)} paused</span>
            <span className="text-muted">
              {' '}
              {p.by ? `by ${p.by.display_name || p.by.username} ` : ''}
              {relativeTime(p.created_at)}
              {p.note ? `: ${p.note}` : ''}
              {p.sessions ? ' · running sessions included' : ''}
            </span>
          </div>
          <Button
            size="sm"
            icon={<CirclePlay className="size-3.5" />}
            loading={resume.isPending}
            onClick={() =>
              resume.mutate(p.spec, { onSuccess: () => toast(`${pauseWhat(p)} take work again`) })
            }
          >
            Resume
          </Button>
        </div>
      ))}
      <PausedProjects keys={embedded ? [] : queue.paused_projects} name={name} />
    </div>
  )
}

const pauseWhat = (p: Pick<AgentPause, 'spec'>) =>
  p.spec === 'any' ? 'All your agents' : `Agents matching “${p.spec}”`

/** Pause (or resume) your agents matching the current filter: all of them without one. */
function PauseControl({ spec, pauses }: { spec: string; pauses: AgentPause[] }) {
  const pop = usePopover()
  const pause = usePauseAgents()
  const resume = useResumeAgents()
  const [note, setNote] = useState('')
  const target = spec || 'any'
  const own = pauses.find((p) => p.spec.toLowerCase() === target.toLowerCase())
  const what = pauseWhat({ spec: target })
  if (own)
    return (
      <Button
        size="sm"
        variant="ghost"
        icon={<CirclePlay className="size-3.5" />}
        loading={resume.isPending}
        onClick={() =>
          resume.mutate(own.spec, { onSuccess: () => toast(`${what} take work again`) })
        }
      >
        Resume
      </Button>
    )
  return (
    <>
      <Button
        ref={pop.anchor}
        size="sm"
        variant="ghost"
        icon={<CirclePause className="size-3.5" />}
        onClick={pop.toggle}
      >
        Pause
      </Button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end" width={320}>
        <form
          className="space-y-2.5 p-2"
          onSubmit={(e) => {
            e.preventDefault()
            pause.mutate(
              { spec: target, note },
              {
                onSuccess: () => {
                  toast(`${what} paused`)
                  setNote('')
                  pop.close()
                },
              },
            )
          }}
        >
          <div className="text-[13px] font-medium">Pause {what.toLowerCase()}?</div>
          <p className="text-xs text-muted">
            They stop taking new work with <code>wp next</code> and are told why. What they hold now
            stays theirs to finish.
          </p>
          <Input
            autoFocus
            value={note}
            maxLength={300}
            onChange={(e) => setNote(e.target.value)}
            placeholder="Why (optional, agents see it)"
            aria-label="Why"
          />
          <div className="flex justify-end gap-2">
            <Button type="button" size="sm" variant="ghost" onClick={pop.close}>
              Cancel
            </Button>
            <Button type="submit" size="sm" variant="primary" loading={pause.isPending}>
              Pause
            </Button>
          </div>
        </form>
      </Popover>
    </>
  )
}

// ---- working now ----

function Working({
  items,
  editable,
}: {
  items: QueueItem[]
  editable: (projectKey: string) => boolean
}) {
  const release = useReleaseClaim()
  const [confirm, setConfirm] = useState<QueueItem | null>(null)
  return (
    <Section title={`Working now · ${items.length}`}>
      <ul className="divide-y divide-line">
        {items.map((it) => (
          <li
            key={it.issue.key}
            className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2.5 sm:px-4"
          >
            <span className="flex size-7 shrink-0 items-center justify-center rounded-full bg-accent-soft text-accent">
              <Bot className="size-4 animate-pulse motion-reduce:animate-none" />
            </span>
            <div className="min-w-0 flex-1">
              <IssueTitle issue={it.issue} />
              <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
                <span className="font-medium text-muted">
                  {agentLabel(it.issue.claimed_by ?? '')}
                </span>
                {it.issue.step && <StepChip step={it.issue.step} />}
                {it.issue.claimed_at && <span>since {relativeTime(it.issue.claimed_at)}</span>}
                {!!it.seconds && <span>worked {duration(it.seconds)}</span>}
                {!!(it.input_tokens || it.output_tokens) && (
                  <span>{tokens((it.input_tokens ?? 0) + (it.output_tokens ?? 0))} tokens</span>
                )}
                <span className="truncate">{it.project_name}</span>
              </div>
            </div>
            {editable(it.issue.project_key) && (
              <Button
                size="xs"
                variant="ghost"
                icon={<Hand className="size-3" />}
                onClick={() => setConfirm(it)}
              >
                Take back
              </Button>
            )}
          </li>
        ))}
      </ul>
      <Confirm
        open={!!confirm}
        onClose={() => setConfirm(null)}
        title={`Take ${confirm?.issue.key} back from ${agentLabel(confirm?.issue.claimed_by ?? '')}?`}
        body="Its run ends and the task goes back in the queue, where the next agent that matches can take it. Anything the agent hasn't pushed stays on its machine."
        confirmLabel="Take back"
        danger={false}
        onConfirm={() =>
          confirm &&
          release.mutate(confirm.issue.key, {
            onSuccess: () => toast(`${confirm.issue.key} is back in the queue`),
          })
        }
      />
    </Section>
  )
}

// ---- up next ----

function UpNext({
  queue,
  agent,
  project,
  editable,
}: {
  queue: AgentQueue
  agent: string
  project: string
  editable: (projectKey: string) => boolean
}) {
  const move = useQueueMove(agent, project)
  const places = useQuery(q.taskSessionPlaces())
  const startSession = useStartTaskSession()
  const [starting, setStarting] = useState<{ issue: Issue; place: TaskSessionPlace } | null>(null)
  /** undefined: no Start button (not loaded yet) */
  const startFor = (i: Issue): StartProps | undefined =>
    places.isSuccess
      ? {
          place: places.data.find((p) => p.project_key === i.project_key),
          onStart: (place) => setStarting({ issue: i, place }),
        }
      : undefined
  const items = queue.next
  const ids = useMemo(() => items.map((i) => i.issue.key), [items])
  const [active, setActive] = useState<QueueItem | null>(null)
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 220, tolerance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const showSpec = !queue.exact
  const narrow = useMediaQuery('(max-width: 639px)')

  /** Holds key at place `position` (1 = first) of the order shown; the rest keep their automatic order. */
  const place = (key: string, position: number, order: string[]) => {
    const reorder = order.map((k, n) => {
      const x = items.find((i) => i.issue.key === k)!
      return { ...x, position: n + 1, pinned: x.pinned || k === key }
    })
    move.mutate({ key, position, reorder })
  }
  const onEnd = (e: DragEndEvent) => {
    setActive(null)
    if (!e.over || e.active.id === e.over.id) return
    const from = ids.indexOf(String(e.active.id))
    const to = ids.indexOf(String(e.over.id))
    if (from < 0 || to < 0) return
    place(String(e.active.id), to + 1, arrayMove(ids, from, to))
  }
  const onStart = (e: DragStartEvent) =>
    setActive(items.find((i) => i.issue.key === e.active.id) ?? null)
  // The pins at the head of the queue, one after another: Prioritize puts a
  // task right after them (WP-753).
  const head = items.findIndex((i) => !i.pinned)
  const pinsAtTop = head < 0 ? items.length : head
  const toTop = (key: string, n: number) => place(key, 1, arrayMove(ids, n, 0))
  const menu = (it: QueueItem, n: number): MenuItem[] => {
    const key = it.issue.key
    const out: MenuItem[] = []
    // The row has its own Move to top button, but not on a phone.
    if (n > 0 && narrow)
      out.push({ label: 'Move to top', icon: <ArrowUpToLine />, onSelect: () => toTop(key, n) })
    // Past the pins ahead of it: n counts this task's own pin out, as it
    // isn't among them.
    const after = n < pinsAtTop ? n : pinsAtTop
    // A task already right after them is pinned where it is. On a phone,
    // with no pins, Move to top above says the same thing.
    const moves = n > after || (n === after && !it.pinned)
    if (moves && !(narrow && after === 0 && n > 0))
      out.push({
        label: 'Prioritize',
        icon: <ChevronsUp />,
        hint: after ? `after ${plural(after, 'pin')}` : 'to the top',
        onSelect: () => place(key, after + 1, arrayMove(ids, n, after)),
      })
    if (it.pinned)
      out.push({
        label: 'Unpin: automatic order',
        icon: <PinOff />,
        onSelect: () => move.mutate({ key, unpin: true }),
      })
    return out
  }
  return (
    <Section title={`Up next · ${items.length}`}>
      {items.length === 0 ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted">Nothing ready to take.</p>
      ) : (
        <DndContext
          sensors={sensors}
          collisionDetection={closestCenter}
          onDragStart={onStart}
          onDragEnd={onEnd}
          onDragCancel={() => setActive(null)}
        >
          <SortableContext items={ids} strategy={verticalListSortingStrategy}>
            {/* No scroll anchoring in the list: the browser would keep the
                task you just moved in view and scroll the page after it (WP-753). */}
            <ol aria-label="Up next, in order" className="[overflow-anchor:none]">
              {items.map((it, n) => {
                const can = editable(it.issue.project_key)
                return (
                  <QueueRow
                    key={it.issue.key}
                    item={it}
                    editable={can}
                    showSpec={showSpec}
                    menu={can ? menu(it, n) : []}
                    onTop={
                      can && !narrow ? (n > 0 ? () => toTop(it.issue.key, n) : null) : undefined
                    }
                    start={can ? startFor(it.issue) : undefined}
                    onUnpin={() => move.mutate({ key: it.issue.key, unpin: true })}
                  />
                )
              })}
            </ol>
          </SortableContext>
          <DragOverlay>
            {active && <RowBody item={active} showSpec={showSpec} lifted />}
          </DragOverlay>
        </DndContext>
      )}
      <Confirm
        open={!!starting}
        onClose={() => setStarting(null)}
        title={`Start a session for ${starting?.issue.key}?`}
        body={starting && <StartBody issue={starting.issue} place={starting.place} />}
        confirmLabel="Start session"
        danger={false}
        onConfirm={() =>
          starting &&
          startSession.mutate(starting.issue.key, {
            onSuccess: (r) =>
              toast(
                `Starting ${r.key} on ${r.place.runner}. Open it there with claude attach ${r.key}`,
                {
                  kind: 'success',
                  href: `/runner-jobs/${r.job.id}`,
                  ms: 8000,
                },
              ),
            onError: toastError,
          })
        }
      />
    </Section>
  )
}

function QueueRow({
  item,
  editable,
  showSpec,
  menu,
  onTop,
  start,
  onUnpin,
}: {
  item: QueueItem
  editable: boolean
  showSpec: boolean
  menu: MenuItem[]
  onTop?: (() => void) | null
  start?: StartProps
  onUnpin: () => void
}) {
  const {
    attributes,
    listeners,
    setNodeRef,
    setActivatorNodeRef,
    transform,
    transition,
    isDragging,
  } = useSortable({
    id: item.issue.key,
    disabled: !editable,
  })
  // The whole row drags by pointer or touch; the keyboard drags from the
  // grip, so Enter and Space still reach the links and buttons in the row.
  return (
    <li
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={clsx(
        'group relative border-b border-line last:border-b-0',
        editable && 'cursor-grab active:cursor-grabbing',
        isDragging && 'opacity-30',
      )}
      {...(editable ? listeners : {})}
    >
      <RowBody
        item={item}
        showSpec={showSpec}
        editable={editable}
        menu={menu}
        onTop={onTop}
        start={start}
        onUnpin={onUnpin}
        handle={editable ? { ref: setActivatorNodeRef, ...attributes } : undefined}
      />
    </li>
  )
}

function RowBody({
  item: it,
  showSpec,
  editable,
  menu = [],
  onTop,
  start,
  onUnpin,
  handle,
  lifted,
}: {
  item: QueueItem
  showSpec: boolean
  editable?: boolean
  menu?: MenuItem[]
  /** the Move to top button; null keeps its room empty (the first task), undefined leaves it out */
  onTop?: (() => void) | null
  start?: StartProps
  onUnpin?: () => void
  handle?: { ref: (el: HTMLElement | null) => void } & Record<string, unknown>
  lifted?: boolean
}) {
  const update = useUpdateIssue()
  const i = it.issue
  return (
    <div
      className={clsx(
        'flex items-start gap-2 px-2 py-2.5 sm:items-center sm:px-3',
        it.pinned && 'bg-accent-soft/40',
        lifted && 'rounded-lg border border-line bg-raised shadow-pop',
      )}
    >
      <span
        {...handle}
        aria-label={handle ? `Drag ${i.key} to reorder` : undefined}
        className="flex w-9 shrink-0 items-center justify-end gap-0.5 self-stretch text-xs tabular-nums text-subtle sm:self-auto"
      >
        {(handle || lifted) && (
          <GripVertical className="size-3.5 opacity-40 group-hover:opacity-100 max-sm:hidden" />
        )}
        <span className={clsx('min-w-4 text-right', it.pinned && 'font-semibold text-accent')}>
          {it.position}
        </span>
      </span>
      <div className="min-w-0 flex-1">
        <IssueTitle issue={i} />
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
          <span
            className={clsx(
              'inline-flex items-center gap-1',
              it.pinned ? 'font-medium text-accent' : 'text-muted',
            )}
          >
            {it.pinned &&
              (editable && onUnpin ? (
                <button
                  type="button"
                  title="Pinned here. Unpin: back to the automatic order"
                  aria-label={`Unpin ${i.key}`}
                  onClick={onUnpin}
                  className="group/pin -m-1 rounded p-1 hover:bg-hover"
                >
                  <Pin className="size-3 group-hover/pin:hidden group-focus-visible/pin:hidden" />
                  <PinOff className="hidden size-3 group-hover/pin:block group-focus-visible/pin:block" />
                </button>
              ) : (
                <Pin className="size-3" />
              ))}
            {it.why}
          </span>
          {(showSpec || !i.agent) && <AgentChip spec={i.agent ?? ''} />}
          {it.takers && it.takers.length > 0 && showSpec && (
            <span title={it.takers.join(', ')}>
              {it.takers.length === 1
                ? agentLabel(it.takers[0])
                : `${it.takers.length} agents can take it`}
            </span>
          )}
          {i.step && (
            <StepChip step={it.step_no ? `${i.step} ${it.step_no}/${it.steps}` : i.step} />
          )}
          <span className="truncate">{it.project_name}</span>
          {it.delegated_by && (
            <span className="max-sm:hidden">
              {i.agent ? 'delegated' : 'added'} by{' '}
              {it.delegated_by.display_name || it.delegated_by.username}{' '}
              {relativeTime(it.delegated_at)}
            </span>
          )}
        </div>
      </div>
      {!lifted && (
        <div className="flex shrink-0 items-center gap-1">
          {i.due_date && (
            <span className="text-xs text-subtle max-sm:hidden">{shortDate(i.due_date)}</span>
          )}
          <PriorityPicker
            value={i.priority}
            iconOnly
            variant="bare"
            disabled={!editable}
            onChange={(p) =>
              update.mutate(
                {
                  key: i.key,
                  projectKey: i.project_key,
                  patch: { priority: p },
                  optimistic: { priority: p },
                },
                { onError: toastError },
              )
            }
          />
          {start && <StartButton issue={i} {...start} />}
          {onTop !== undefined && (
            <IconButton
              label={`Move ${i.key} to top`}
              title="Move to top"
              size="xs"
              variant="ghost"
              className={clsx(!onTop && 'invisible')}
              disabled={!onTop}
              onClick={() => onTop?.()}
            >
              <ArrowUpToLine className="size-3.5" />
            </IconButton>
          )}
          <RowMenu issue={i} items={menu} editable={!!editable} />
        </div>
      )}
    </div>
  )
}

interface StartProps {
  /** where your runner can start it; none: the button is off, saying why */
  place?: TaskSessionPlace
  onStart: (place: TaskSessionPlace) => void
}

/** Starts a fresh task session on your runner (WP-71), as wp start KEY --bg would. */
function StartButton({ issue, place, onStart }: { issue: Issue } & StartProps) {
  const why = place
    ? `Start a fresh Claude Code session for ${issue.key} on ${place.runner}`
    : `None of your runners can start ${issue.key}: run wp runner (with --allow for the checkout) on a machine where you work on ${issue.project_key}, or run wp start ${issue.key} in its checkout`
  return (
    <span title={why} className="inline-flex">
      <Button
        size="xs"
        variant="ghost"
        icon={<CirclePlay className="size-3.5" />}
        disabled={!place}
        aria-label={`Start a session for ${issue.key}`}
        onClick={() => place && onStart(place)}
      >
        <span className="max-sm:sr-only">Start</span>
      </Button>
    </span>
  )
}

function StartBody({ issue, place }: { issue: Issue; place: TaskSessionPlace }) {
  return (
    <div className="space-y-2">
      <p>
        A fresh Claude Code session starts in the background on <b>{place.runner}</b>, in{' '}
        <code className="break-all">{place.dir}</code>, bound to {issue.key}. It takes the task and
        does it; open it there with <code>claude attach {issue.key}</code>.
      </p>
      {!place.online && (
        <p className="text-warn">
          {place.runner} is offline right now; the session starts when its runner is back.
        </p>
      )}
    </div>
  )
}

function RowMenu({
  issue,
  items,
  editable,
}: {
  issue: Issue
  items: MenuItem[]
  editable: boolean
}) {
  const pop = usePopover()
  const update = useUpdateIssue()
  const navigate = useNavigate()
  const all: MenuItem[] = [
    ...items,
    ...(items.length ? [{ separator: true, label: '' }] : []),
    ...(editable
      ? [
          {
            label: 'Humans only',
            icon: <UserX />,
            onSelect: () =>
              update.mutate(
                { key: issue.key, projectKey: issue.project_key, patch: { agent: 'none' } },
                {
                  onSuccess: () => toast(`${issue.key} is for humans only now`),
                  onError: toastError,
                },
              ),
          },
        ]
      : []),
    {
      label: 'Open',
      icon: <ExternalLink />,
      onSelect: () => navigate({ to: '/issue/$key', params: { key: issue.key } }),
    },
  ]
  return (
    <>
      <IconButton
        ref={pop.anchor}
        label={`More for ${issue.key}`}
        size="xs"
        variant="ghost"
        onClick={pop.toggle}
      >
        <MoreHorizontal className="size-4" />
      </IconButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <MenuList items={all} onClose={pop.close} />
      </Popover>
    </>
  )
}

function IssueTitle({ issue }: { issue: Issue }) {
  return (
    <Link
      to="/issue/$key"
      params={{ key: issue.key }}
      className="flex min-w-0 items-center gap-1.5 text-[13px] hover:underline"
      title={issue.title}
    >
      <StatusIcon category={issue.status.category} color={issue.status.color} />
      <span className="shrink-0 font-mono text-xs text-muted">{issue.key}</span>
      <span className="truncate font-medium">{issue.title}</span>
    </Link>
  )
}

// ---- waiting ----

const WAITS: { reason: WaitReason; title: string; about: string }[] = [
  {
    reason: 'safety',
    title: 'Agent-written, not cleared',
    about:
      'An agent wrote it and no person has reviewed it. The safety screen clears it, or holds it for a person: reviewing it releases it (WP-ADR10).',
  },
  {
    reason: 'step',
    title: 'At a step for another agent',
    about: 'Its workflow step is for a different agent.',
  },
  { reason: 'claimed', title: 'Held by another agent', about: 'Another agent is working on it.' },
  {
    reason: 'project_paused',
    title: 'In a paused project',
    about: 'Agents take no new work in these projects.',
  },
  { reason: 'backlog', title: 'In the backlog', about: 'Agents take work once it’s in To Do.' },
  {
    reason: 'hold',
    title: 'On hold',
    about:
      'No one takes it until what it waits for happens (a date, tasks closing, a request answered, a review) or someone lifts it (WP-269).',
  },
  {
    reason: 'released',
    title: 'Given back',
    about:
      'An agent gave it back, so it isn’t handed out for a while, and for longer each time that repeats. A person’s comment ends the wait; naming it still takes it.',
  },
  {
    reason: 'objective',
    title: 'Waiting on its objective',
    about:
      'Its objective keeps it from being handed out now: Waypoint is reading a round, the objective needs a person, the next round isn’t due (rounds keep a least time apart, and wait longer after rounds that moved nothing), or at the spare pace it runs only on quota that would go unused (WP-927).',
  },
  {
    reason: 'blocked',
    title: 'Blocked',
    about: 'An open issue blocks it; it’s ready once that’s done.',
  },
  {
    reason: 'subtasks',
    title: 'Sub-issues first',
    about:
      'Its open sub-issues are for agents too: they take those first, and this once they’re done.',
  },
  {
    reason: 'review',
    title: 'Out for review',
    about:
      'Its pull request is open or merged, or it moved on to In Review: it waits on the merge or a person, not an agent.',
  },
]

function Waiting({
  items,
  editable,
}: {
  items: QueueItem[]
  editable: (projectKey: string) => boolean
}) {
  return (
    <Section title={`Waiting · ${items.length}`}>
      {WAITS.map((w) => {
        const list = items.filter((i) => i.reason === w.reason)
        if (!list.length) return null
        return (
          <div key={w.reason} className="border-b border-line last:border-b-0">
            <div className="flex flex-wrap items-baseline gap-x-2 px-3 pb-1 pt-2.5 sm:px-4">
              <h4 className="text-xs font-semibold" title={w.about}>
                {w.title} <span className="font-normal text-subtle">{list.length}</span>
              </h4>
            </div>
            <ul>
              {list.map((it) => (
                <WaitRow key={it.issue.key} item={it} editable={editable(it.issue.project_key)} />
              ))}
            </ul>
          </div>
        )
      })}
    </Section>
  )
}

function WaitRow({ item: it, editable }: { item: QueueItem; editable: boolean }) {
  const update = useUpdateIssue()
  const release = useReleaseClaim()
  const review = useReview()
  const hold = useHoldIssue()
  const qc = useQueryClient()
  const i = it.issue
  let fix: ReactNode = null
  if (editable && (it.reason === 'backlog' || it.reason === 'review'))
    fix = (
      <Button
        size="xs"
        variant="ghost"
        icon={<ArrowRight className="size-3" />}
        loading={update.isPending}
        onClick={() =>
          update.mutate(
            { key: i.key, projectKey: i.project_key, patch: { status: 'todo' } },
            {
              onSuccess: () => toast(`${i.key} is in To Do: agents can take it`),
              onError: toastError,
            },
          )
        }
      >
        Move to To Do
      </Button>
    )
  else if (editable && it.reason === 'safety')
    fix = (
      <Button
        size="xs"
        variant="ghost"
        icon={<Check className="size-3" />}
        loading={review.isPending}
        onClick={() =>
          review.mutate(
            { type: 'issue', ref: i.key },
            {
              onSuccess: () => {
                qc.invalidateQueries({ queryKey: ['agents'] })
                toast(`${i.key} is reviewed: agents can take it`)
              },
            },
          )
        }
      >
        Mark reviewed
      </Button>
    )
  else if (editable && it.reason === 'hold')
    fix = (
      <Button
        size="xs"
        variant="ghost"
        icon={<Hourglass className="size-3" />}
        loading={hold.isPending}
        onClick={() =>
          hold.mutate(
            { key: i.key, hold: null },
            { onSuccess: () => toast(`${i.key} is off hold`), onError: toastError },
          )
        }
      >
        Lift hold
      </Button>
    )
  else if (editable && it.reason === 'released')
    fix = (
      <Button
        size="xs"
        variant="ghost"
        icon={<Hourglass className="size-3" />}
        loading={hold.isPending}
        onClick={() =>
          hold.mutate(
            { key: i.key, hold: null },
            { onSuccess: () => toast(`${i.key} can go out again`), onError: toastError },
          )
        }
      >
        End the wait
      </Button>
    )
  else if (editable && it.reason === 'claimed')
    fix = (
      <Button
        size="xs"
        variant="ghost"
        icon={<Hand className="size-3" />}
        loading={release.isPending}
        onClick={() =>
          release.mutate(i.key, { onSuccess: () => toast(`${i.key} is back in the queue`) })
        }
      >
        Take back
      </Button>
    )
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 sm:px-4">
      <div className="min-w-0 flex-1">
        <IssueTitle issue={i} />
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
          {it.reason === 'blocked' && it.blockers?.length ? (
            <span className="inline-flex flex-wrap items-center gap-1">
              <Lock className="size-3" /> Blocked by
              {it.blockers.map((b) =>
                /^[A-Z][A-Z0-9]*-\d+$/.test(b) ? (
                  <Link
                    key={b}
                    to="/issue/$key"
                    params={{ key: b }}
                    className="font-mono text-muted hover:underline"
                  >
                    {b}
                  </Link>
                ) : (
                  <span key={b}>{b}</span>
                ),
              )}
            </span>
          ) : it.reason === 'step' ||
            it.reason === 'claimed' ||
            it.reason === 'review' ||
            it.reason === 'safety' ||
            it.reason === 'subtasks' ||
            it.reason === 'hold' ||
            it.reason === 'objective' ||
            it.reason === 'released' ? (
            // Only these say more than their group's heading does.
            <span className="text-muted">{it.detail}</span>
          ) : it.reason === 'backlog' ? (
            <span>{i.status.name}</span>
          ) : null}
          {it.pinned && (
            <Badge className="gap-0.5">
              <Pin className="size-2.5" /> pinned
            </Badge>
          )}
          <AgentChip spec={i.agent ?? ''} />
          {it.workflow && (
            <span className="inline-flex items-center gap-1">
              <WorkflowIcon className="size-3" /> {it.workflow}
            </span>
          )}
          <span className="truncate">{it.project_name}</span>
        </div>
      </div>
      {fix}
    </li>
  )
}

// ---- adding work ----

/** Finds an open issue not yet for agents and delegates it, optionally pinned to the top. */
function AddWork({
  open,
  onClose,
  spec,
  project,
}: {
  open: boolean
  onClose: () => void
  spec: string
  project: string
}) {
  const [text, setText] = useState('')
  const [to, setTo] = useState(spec || 'any')
  const [top, setTop] = useState(true)
  const [lastSpec, setLastSpec] = useState(spec)
  if (spec !== lastSpec) {
    setLastSpec(spec)
    setTo(spec || 'any')
  }
  const query = [text.trim(), 'is:open', '-is:agent'].filter(Boolean).join(' ')
  const results = useQuery({
    ...q.search(query, { project: project || undefined, limit: 12, order: 'updated' }),
    enabled: open,
  })
  const update = useUpdateIssue()
  const move = useQueueMove(spec, project)
  const add = (i: Issue) => {
    const patch: { agent: string; status?: string } = { agent: to.trim() || 'any' }
    if (i.status.category === 'backlog') patch.status = 'todo'
    update.mutate(
      { key: i.key, projectKey: i.project_key, patch },
      {
        onSuccess: () => {
          if (top) move.mutate({ key: i.key, top: true })
          toast(
            `${i.key} is for ${patch.agent === 'any' ? 'any agent' : patch.agent}${top ? ', first in the queue' : ''}`,
          )
        },
        onError: toastError,
      },
    )
  }
  const list = results.data?.issues ?? []
  return (
    <Dialog open={open} onClose={onClose} title="Add work for agents" wide>
      <div className="space-y-3 p-4">
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
          <label className="relative min-w-0 flex-1">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
            <Input
              autoFocus
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="Find an issue for people only…"
              aria-label="Find an issue"
              className="pl-8"
            />
          </label>
          <label className="flex items-center gap-2 text-[13px] text-muted">
            For
            <Input
              value={to}
              onChange={(e) => setTo(e.target.value)}
              aria-label="For agents matching"
              className="w-36"
            />
          </label>
        </div>
        <label className="flex items-center gap-2 text-[13px] text-muted">
          <Switch checked={top} onChange={setTop} label="Pin to the top" /> Pin it to the top of the
          queue
        </label>
        <ul className="max-h-[50vh] divide-y divide-line overflow-y-auto rounded-lg border border-line">
          {results.isLoading ? (
            <li className="p-3">
              <Skeleton className="h-8" />
            </li>
          ) : list.length === 0 ? (
            <li className="px-3 py-6 text-center text-[13px] text-muted">
              {text
                ? 'No open issue for people matches.'
                : 'No open issue is for people only here.'}
            </li>
          ) : (
            list.map((i) => (
              <li key={i.key} className="flex items-center gap-2 px-3 py-2">
                <PriorityIcon priority={i.priority} className="size-3.5 shrink-0 text-muted" />
                <span className="shrink-0 font-mono text-xs text-muted">{i.key}</span>
                <span className="min-w-0 flex-1 truncate text-[13px]">{i.title}</span>
                {i.status.category === 'backlog' && (
                  <span
                    className="text-xs text-subtle max-sm:hidden"
                    title="Moves to To Do when delegated"
                  >
                    backlog
                  </span>
                )}
                <Button size="xs" icon={<Plus className="size-3" />} onClick={() => add(i)}>
                  Add
                </Button>
              </li>
            ))
          )}
        </ul>
        <p className="text-xs text-subtle">
          Adding sets its <b>Agent</b> to “{to.trim() || 'any'}” and, if it’s in the backlog, moves
          it to To Do so agents can take it. {plural(list.length, 'issue')} shown.
        </p>
      </div>
    </Dialog>
  )
}

/** 12345 → "12.3k". */
function tokens(n: number) {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`
  return `${(n / 1_000_000).toFixed(1)}M`
}

/** One row for every paused project, opening to the list. */
function PausedProjects({ keys, name }: { keys: string[]; name: (key: string) => string }) {
  const [open, setOpen] = useState(false)
  if (!keys.length) return null
  return (
    <div className="rounded-xl border border-line bg-sunken px-3 py-2 text-[13px] text-muted">
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className="flex w-full items-center gap-2 text-left hover:text-fg"
      >
        <CirclePause className="size-4 shrink-0" />
        <span className="flex-1">
          Agents are paused in {keys.length === 1 ? name(keys[0]!) : `${keys.length} projects`}
        </span>
        {keys.length > 1 && <ChevronDown className={clsx('size-3.5', open && 'rotate-180')} />}
      </button>
      {open && keys.length > 1 && (
        <ul className="mt-2 space-y-1 pl-6">
          {keys.map((key) => (
            <li key={key}>
              <Link to="/p/$key/settings" params={{ key }} className="hover:text-fg">
                {name(key)}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
