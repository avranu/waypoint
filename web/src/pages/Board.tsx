import { createContext, useContext, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  TouchSensor,
  useDraggable,
  useDroppable,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragOverEvent,
  type DragStartEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import clsx from 'clsx'
import { useAgentFilter } from '@/lib/agents'
import { CalendarRange, ChevronDown, ChevronRight, Flag, ListTree, Plus } from 'lucide-react'
import { api } from '@/lib/api'
import { q, useUpdateIssue, invalidateProject, type IssuePatch } from '@/lib/queries'
import { readDraft } from '@/lib/drafts'
import { LongTitleHint, SaveError, useQuickAdd } from '@/components/issue/quickAdd'
import {
  QuickAddDetailsButton,
  isDetailsKey,
  openDetails,
} from '@/components/issue/QuickAddDetails'
import { points as fmtPoints, plural, shortDate } from '@/lib/format'
import { useListNav, useLocal, useMediaQuery } from '@/lib/hooks'
import { pointerFirst, useMoveThrottle } from '@/lib/dnd'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Avatar, Empty, Progress } from '@/components/ui/misc'
import { Select } from '@/components/ui/input'
import { Confirm } from '@/components/ui/dialog'
import { toast, toastError } from '@/components/ui/toast'
import { IssueCard } from '@/components/issue/IssueCard'
import { StatusIcon } from '@/components/issue/icons'
import { FilterBar, useOpenIssue, useProject } from './Project'
import {
  isClosed,
  PRIORITIES,
  type Board,
  type Issue,
  type Project,
  type Sprint,
  type UserRef,
} from '@/lib/types'

export function BoardPage() {
  const project = useProject()
  const { key } = useParams({ from: '/app/p/$key/board' })
  const search = useSearch({ from: '/app/p/$key/board' })
  const agentQ = useAgentFilter(key, search.q ?? '')
  const board = useQuery({ ...q.board(key, agentQ), placeholderData: (p) => p }).data
  const [group, setGroup] = useLocal<Group>(`board-lanes:${key}`, 'none')
  const [nest, setNest] = useLocal(`board-nest:${key}`, true)
  const [unfolded, setUnfolded] = useLocal<string[]>(`board-subtasks-open:${key}`, [])
  const folds: Folds = {
    nest,
    open: new Set(unfolded),
    toggle: (k) =>
      setUnfolded(unfolded.includes(k) ? unfolded.filter((x) => x !== k) : [...unfolded, k]),
  }
  if (!board) return null
  return (
    <>
      <FilterBar
        project={project}
        placeholder="Filter the board… (try label:bug or type:bug)"
        extra={
          <div className="flex items-center gap-1">
            <ToggleButton
              label="Nest subtasks"
              hint="Fold subtasks into their parent's card when they share its column"
              pressed={nest}
              onPressedChange={setNest}
              icon={<ListTree className="size-3.5" />}
              text="Subtasks"
              textFrom="md"
            />
            <GroupBy value={group} onChange={setGroup} />
          </div>
        }
      />
      {board.mode === 'sprints' && board.sprint && (
        <SprintBar project={project} sprint={board.sprint} />
      )}
      {board.mode === 'sprints' && !board.sprint ? (
        <Empty
          icon={<Flag />}
          title="No active sprint"
          action={
            <Link to="/p/$key/backlog" params={{ key }}>
              <Button variant="primary">Plan a sprint in the backlog</Button>
            </Link>
          }
        >
          This project works in sprints. Start one from the backlog and its issues will appear here.
        </Empty>
      ) : (
        <NestContext.Provider value={folds}>
          <Columns project={project} board={board} filter={search.q ?? ''} group={group} />
        </NestContext.Provider>
      )}
    </>
  )
}

function SprintBar({ project, sprint }: { project: Project; sprint: Sprint }) {
  const qc = useQueryClient()
  const [closing, setClosing] = useState(false)
  const close = useMutation({
    mutationFn: () => api.post(`/sprints/${sprint.id}/close`, { move_to: 'next' }),
    onSuccess: () => {
      invalidateProject(qc, project.key)
      toast(`${sprint.name} complete — unfinished work moved to the next sprint`)
    },
    onError: toastError,
  })
  const daysLeft = sprint.end_date
    ? Math.ceil((new Date(sprint.end_date + 'T23:59:59').getTime() - Date.now()) / 86400000)
    : null
  return (
    <div className="wp-surface-blur mx-3 mb-1 flex flex-wrap items-center gap-x-4 gap-y-1 rounded-xl border border-line bg-panel/80 px-3 py-2 text-[13px] backdrop-blur sm:mx-5">
      <span className="font-semibold">{sprint.name}</span>
      {sprint.goal && <span className="hidden truncate text-muted md:inline">{sprint.goal}</span>}
      <span className="flex items-center gap-1 text-muted">
        <CalendarRange className="size-3.5" />
        {shortDate(sprint.start_date)} – {shortDate(sprint.end_date)}
        {daysLeft !== null && (
          <span
            className={clsx('ml-1', daysLeft < 0 ? 'text-danger' : daysLeft <= 2 && 'text-warn')}
          >
            · {daysLeft < 0 ? `${-daysLeft}d over` : `${daysLeft}d left`}
          </span>
        )}
      </span>
      <span className="flex items-center gap-2 text-muted">
        <Progress
          value={sprint.done_points || sprint.issue_count - sprint.open_count}
          max={sprint.points || sprint.issue_count}
          className="w-24"
        />
        {sprint.points
          ? `${fmtPoints(sprint.done_points)}/${fmtPoints(sprint.points)} pts`
          : `${sprint.issue_count - sprint.open_count}/${sprint.issue_count}`}
      </span>
      <span className="flex-1" />
      {project.role !== 'viewer' && project.role !== 'guest' && (
        <Button
          size="sm"
          variant="ghost"
          onClick={() => setClosing(true)}
          loading={close.isPending}
        >
          Complete sprint
        </Button>
      )}
      <Confirm
        open={closing}
        onClose={() => setClosing(false)}
        danger={false}
        title={`Complete ${sprint.name}?`}
        confirmLabel="Complete sprint"
        body={
          sprint.open_count
            ? `${plural(sprint.open_count, 'unfinished issue')} will move to the next sprint (created if needed).`
            : 'Everything is done. Nice.'
        }
        onConfirm={() => close.mutate()}
      />
    </div>
  )
}

// Cards live in cells: one per (swimlane, status). Without swimlanes there is
// a single lane, ''.
type Cells = Record<string, Issue[]>
type Group = 'none' | 'assignee' | 'epic' | 'priority'
interface Lane {
  id: string
  label: React.ReactNode
  count: number
}

const cellId = (lane: string, status: number) => `cell|${lane}|${status}`
const parseCell = (id: string) => {
  const [, lane, status] = id.split('|')
  return { lane, status: Number(status) }
}

function laneOf(i: Issue, group: Group): string {
  switch (group) {
    case 'assignee':
      return i.assignees[0]?.username ?? ''
    case 'epic':
      return i.parent?.type === 'epic' ? i.parent.key : ''
    case 'priority':
      return String(i.priority)
  }
  return ''
}

function buildLanes(board: Board, group: Group): Lane[] {
  if (group === 'none') return [{ id: '', label: '', count: 0 }]
  const lanes = new Map<string, Lane>()
  for (const c of board.columns)
    for (const i of c.issues) {
      const id = laneOf(i, group)
      const lane = lanes.get(id) ?? { id, label: laneLabel(i, group, id), count: 0 }
      lane.count++
      lanes.set(id, lane)
    }
  const list = [...lanes.values()]
  if (group === 'priority') return list.sort((x, y) => Number(y.id) - Number(x.id))
  return list.sort((x, y) =>
    !x.id !== !y.id
      ? x.id
        ? -1
        : 1
      : group === 'assignee'
        ? String(x.label).localeCompare(String(y.label))
        : 0,
  )
}

function laneLabel(i: Issue, group: Group, id: string): React.ReactNode {
  switch (group) {
    case 'assignee':
      return id ? i.assignees[0].display_name : 'Unassigned'
    case 'epic':
      return id ? `${i.parent!.key} ${i.parent!.title}` : 'No epic'
    case 'priority':
      return PRIORITIES[Number(id)] === 'none' ? 'No priority' : cap(PRIORITIES[Number(id)])
  }
  return ''
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1)

function buildCells(board: Board, group: Group): Cells {
  const out: Cells = {}
  for (const lane of buildLanes(board, group))
    for (const c of board.columns) out[cellId(lane.id, c.status.id)] = []
  for (const c of board.columns)
    for (const i of c.issues) out[cellId(laneOf(i, group), c.status.id)].push(i)
  return out
}

/** The swimlane picker shown in the filter bar. */
export function GroupBy({ value, onChange }: { value: Group; onChange: (g: Group) => void }) {
  return (
    <Select
      aria-label="Swimlanes"
      value={value}
      onChange={(e) => onChange(e.target.value as Group)}
      className="!h-8 w-auto text-[13px]"
    >
      <option value="none">No swimlanes</option>
      <option value="assignee">Lanes: Assignee</option>
      <option value="epic">Lanes: Epic</option>
      <option value="priority">Lanes: Priority</option>
    </Select>
  )
}

function Columns({
  project,
  board,
  filter,
  group,
}: {
  project: Project
  board: Board
  filter: string
  group: Group
}) {
  const qc = useQueryClient()
  const open = useOpenIssue()
  const update = useUpdateIssue()
  const mobile = useMediaQuery('(max-width: 767px)')
  const editable = project.role === 'owner' || project.role === 'editor'
  const lanes = useMemo(() => buildLanes(board, group), [board, group])
  const fromServer = useMemo(() => buildCells(board, group), [board, group])
  const [cells, setCells] = useState<Cells>(fromServer)
  const [active, setActive] = useState<Issue | null>(null)
  const [collapsed, setCollapsed] = useLocal<string[]>(
    `lanes-collapsed:${project.key}:${group}`,
    [],
  )
  const { nest } = useContext(NestContext)
  const [crossed, setCrossed] = useState(false)
  const dragOrigin = useRef<string | null>(null)
  const throttle = useMoveThrottle()
  const boardRef = useRef<HTMLDivElement>(null)
  useListNav(boardRef, { onOpen: open })
  useEffect(() => {
    if (!active) setCells(fromServer)
  }, [fromServer, active])

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 220, tolerance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )

  const findCell = (id: string | number, c: Cells = cells): string | null => {
    if (typeof id === 'string' && id.startsWith('cell|')) return id
    for (const [cid, list] of Object.entries(c)) if (list.some((i) => i.key === id)) return cid
    return null
  }

  const onStart = (e: DragStartEvent) => {
    const cell = findCell(e.active.id)
    dragOrigin.current = cell
    setActive(cell !== null ? (cells[cell].find((i) => i.key === e.active.id) ?? null) : null)
  }

  const onOver = (e: DragOverEvent) => {
    const { active: a, over } = e
    if (!over) return
    const from = findCell(a.id)
    const to = findCell(over.id)
    if (from === null || to === null || from === to) return
    const overId = over.id
    throttle(() =>
      setCells((c) => {
        // Work from the latest state: drag-over events can outpace renders.
        const src = findCell(a.id, c)
        const item = src !== null ? c[src].find((i) => i.key === a.id) : undefined
        if (src === null || !item || src === to || !c[to]) return c
        const target = c[to].filter((i) => i.key !== item.key)
        const overIdx = target.findIndex((i) => i.key === overId)
        target.splice(overIdx >= 0 ? overIdx : target.length, 0, item)
        return { ...c, [src]: c[src].filter((i) => i.key !== item.key), [to]: target }
      }),
    )
  }

  const lanePatch = (laneID: string): { patch: IssuePatch; optimistic: Partial<Issue> } => {
    switch (group) {
      case 'assignee': {
        const user = qc.getQueryData<UserRef[]>(['users'])?.find((u) => u.username === laneID)
        return {
          patch: { assignees: laneID ? [laneID] : [] },
          optimistic: { assignees: user ? [user] : [] },
        }
      }
      case 'priority':
        return { patch: { priority: Number(laneID) }, optimistic: { priority: Number(laneID) } }
      case 'epic': {
        const epic = board.columns
          .flatMap((c) => c.issues)
          .find((i) => i.parent?.key === laneID)?.parent
        return {
          patch: { parent: laneID || null },
          optimistic: { parent: laneID ? epic : undefined },
        }
      }
    }
    return { patch: {}, optimistic: {} }
  }

  const onEnd = (e: DragEndEvent) => {
    const { active: a, over } = e
    const origin = dragOrigin.current
    setActive(null)
    if (!over || origin === null) return setCells(fromServer)
    let cell = findCell(a.id)
    if (cell === null) return setCells(fromServer)
    // The last drag-over can be throttled away or not rendered yet: land in
    // the cell under the pointer regardless.
    let base = cells
    const overCell = findCell(over.id)
    if (overCell !== null && overCell !== cell && base[overCell]) {
      const item = base[cell].find((i) => i.key === a.id)!
      const target = base[overCell].filter((i) => i.key !== a.id)
      const oi = target.findIndex((i) => i.key === over.id)
      target.splice(oi >= 0 ? oi : target.length, 0, item)
      base = { ...base, [cell]: base[cell].filter((i) => i.key !== a.id), [overCell]: target }
      cell = overCell
    }
    setCrossed(cell !== origin)
    // A subtask is dropped "on" its parent's card whenever it is let go in place:
    // that means no move, not "take the parent's place".
    const parentKey = base[cell].find((i) => i.key === a.id)?.parent?.key
    const overKey = nest && over.id === parentKey && findCell(over.id) === cell ? a.id : over.id
    let list = base[cell]
    const oldIdx = list.findIndex((i) => i.key === a.id)
    const overIdx = list.findIndex((i) => i.key === overKey)
    if (overIdx >= 0 && oldIdx !== overIdx) {
      list = [...list]
      const [moved] = list.splice(oldIdx, 1)
      list.splice(overIdx, 0, moved)
    }
    const idx = list.findIndex((i) => i.key === a.id)
    const issue = list[idx]
    const to = parseCell(cell)
    const from = parseCell(origin)
    const status = board.columns.find((c) => c.status.id === to.status)!.status
    const statusChanged = from.status !== to.status
    const laneChanged = from.lane !== to.lane
    const lane = laneChanged ? lanePatch(to.lane) : { patch: {}, optimistic: {} }
    // Before and after keys come from the list the server also orders.
    const before = list[idx + 1]?.key
    const after = list[idx - 1]?.key
    const moved = statusChanged || laneChanged || oldIdx !== idx
    const next = {
      ...base,
      [cell]: list.map((i) => (i.key === issue.key ? { ...i, status, ...lane.optimistic } : i)),
    }
    setCells(next)
    if (!moved) return
    qc.setQueryData<Board>(['board', project.key, filter], (b) =>
      b
        ? {
            ...b,
            columns: b.columns.map((c) => ({
              ...c,
              issues: lanes.flatMap((l) => next[cellId(l.id, c.status.id)] ?? []),
            })),
          }
        : b,
    )
    update.mutate(
      {
        key: issue.key,
        projectKey: project.key,
        patch: {
          ...(statusChanged ? { status: String(status.id) } : {}),
          ...lane.patch,
          ...(after ? { after } : before ? { before } : {}),
        },
        optimistic: {
          ...(statusChanged
            ? { status, resolved_at: isClosed(status.category) ? new Date().toISOString() : null }
            : {}),
          ...lane.optimistic,
        },
      },
      { onError: (err) => (toastError(err), setCells(fromServer)) },
    )
  }

  const toggleLane = (id: string) =>
    setCollapsed(collapsed.includes(id) ? collapsed.filter((x) => x !== id) : [...collapsed, id])

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerFirst}
      onDragStart={onStart}
      onDragOver={onOver}
      onDragEnd={onEnd}
      onDragCancel={() => (setActive(null), setCells(fromServer))}
    >
      {group === 'none' ? (
        <div
          ref={boardRef}
          className={clsx(
            'flex min-h-0 flex-1 gap-3 overflow-x-auto px-3 pb-4 pt-1 sm:px-5',
            mobile && 'snap-x snap-mandatory',
          )}
        >
          {board.columns.map((c) => (
            <Column
              key={c.status.id}
              id={cellId('', c.status.id)}
              project={project}
              status={c.status}
              issues={cells[cellId('', c.status.id)] ?? []}
              onOpen={open}
              editable={editable}
              mobile={mobile}
            />
          ))}
        </div>
      ) : (
        <div ref={boardRef} className="min-h-0 flex-1 overflow-auto px-3 pb-4 pt-1 sm:px-5">
          <div className="w-max min-w-full space-y-2">
            <div className="wp-surface-blur sticky top-0 z-[3] flex gap-3 rounded-xl bg-panel/90 backdrop-blur">
              {board.columns.map((c) => {
                const n = lanes.reduce(
                  (s, l) => s + (cells[cellId(l.id, c.status.id)]?.length ?? 0),
                  0,
                )
                return (
                  <div
                    key={c.status.id}
                    className={clsx(
                      'flex h-9 shrink-0 items-center gap-2 px-3',
                      mobile ? 'w-[78vw]' : 'w-[300px]',
                    )}
                  >
                    <StatusIcon category={c.status.category} color={c.status.color} />
                    <span className="text-[13px] font-semibold">{c.status.name}</span>
                    <span className="text-xs tabular-nums text-subtle">{n}</span>
                  </div>
                )
              })}
            </div>
            {lanes.map((l) => {
              const shut = collapsed.includes(l.id)
              return (
                <section key={l.id} aria-label={String(l.label)} className="wp-surface rounded-xl">
                  <button
                    onClick={() => toggleLane(l.id)}
                    aria-expanded={!shut}
                    className="sticky left-0 flex h-8 max-w-[90vw] items-center gap-1.5 px-2 text-[13px] font-medium"
                  >
                    <ChevronDown
                      className={clsx(
                        'size-3.5 text-subtle transition-transform',
                        shut && '-rotate-90',
                      )}
                    />
                    {group === 'assignee' && l.id && (
                      <Avatar
                        user={qc
                          .getQueryData<UserRef[]>(['users'])
                          ?.find((u) => u.username === l.id)}
                        size={18}
                      />
                    )}
                    <span className="truncate">{l.label}</span>
                    <span className="text-xs font-normal tabular-nums text-subtle">{l.count}</span>
                  </button>
                  {!shut && (
                    <div className="flex gap-3 pb-1.5">
                      {board.columns.map((c) => (
                        <Cell
                          key={c.status.id}
                          id={cellId(l.id, c.status.id)}
                          issues={cells[cellId(l.id, c.status.id)] ?? []}
                          onOpen={open}
                          editable={editable}
                          mobile={mobile}
                        />
                      ))}
                    </div>
                  )}
                </section>
              )
            })}
          </div>
        </div>
      )}
      <DragOverlay
        dropAnimation={crossed ? null : { duration: 160, easing: 'cubic-bezier(0.2, 0.8, 0.2, 1)' }}
      >
        {active ? <IssueCard issue={active} onOpen={() => {}} dragging /> : null}
      </DragOverlay>
    </DndContext>
  )
}

/** One (lane, status) cell of a swimlaned board. */
function Cell({
  id,
  issues,
  onOpen,
  editable,
  mobile,
}: {
  id: string
  issues: Issue[]
  onOpen: (k: string) => void
  editable: boolean
  mobile: boolean
}) {
  const { setNodeRef, isOver } = useDroppable({ id })
  const { top, kids } = useNested(issues)
  return (
    <div
      ref={setNodeRef}
      data-nav-col={parseCell(id).status}
      className={clsx(
        'flex min-h-14 shrink-0 flex-col gap-2 rounded-xl p-1.5 transition-colors',
        mobile ? 'w-[78vw]' : 'w-[300px]',
        isOver ? 'bg-accent-soft' : 'bg-sunken/60',
      )}
    >
      <SortableContext items={top.map((i) => i.key)} strategy={verticalListSortingStrategy}>
        {top.map((i) => (
          <SortableCard key={i.key} issue={i} kids={kids} onOpen={onOpen} disabled={!editable} />
        ))}
      </SortableContext>
    </div>
  )
}

function Column({
  id,
  project,
  status,
  issues,
  onOpen,
  editable,
  mobile,
}: {
  id: string
  project: Project
  status: Board['columns'][number]['status']
  issues: Issue[]
  onOpen: (k: string) => void
  editable: boolean
  mobile: boolean
}) {
  const { setNodeRef, isOver } = useDroppable({ id })
  const pts = issues.reduce((s, i) => s + (i.points ?? 0), 0)
  const { top, kids } = useNested(issues)
  const [adding, setAdding] = useState(() => !!readDraft(quickAddKey(project.key, status.id)))
  return (
    <section
      data-col={status.id}
      data-nav-col={status.id}
      className={clsx(
        'wp-surface flex shrink-0 flex-col rounded-xl',
        mobile ? 'w-[86vw] snap-center' : 'w-[300px]',
      )}
    >
      <div className="flex h-9 items-center gap-2 px-1.5">
        <StatusIcon category={status.category} color={status.color} />
        <span className="text-[13px] font-semibold">{status.name}</span>
        <span className="text-xs tabular-nums text-subtle">{issues.length}</span>
        {pts > 0 && (
          <span className="text-xs tabular-nums text-subtle">· {fmtPoints(pts)} pts</span>
        )}
        <span className="flex-1" />
        {editable && (
          <IconButton
            label={`Add to ${status.name}`}
            size="xs"
            onClick={() => setAdding(true)}
            icon={<Plus className="size-3.5" />}
          />
        )}
      </div>
      <div
        ref={setNodeRef}
        className={clsx(
          'flex min-h-24 flex-1 flex-col gap-2 overflow-y-auto rounded-xl p-1.5 transition-colors',
          isOver ? 'bg-accent-soft' : 'bg-sunken/60',
        )}
      >
        {adding && (
          <QuickAdd
            project={project}
            status={status.id}
            top={issues[0]?.key}
            onDone={() => setAdding(false)}
          />
        )}
        <SortableContext items={top.map((i) => i.key)} strategy={verticalListSortingStrategy}>
          {top.map((i) => (
            <SortableCard key={i.key} issue={i} kids={kids} onOpen={onOpen} disabled={!editable} />
          ))}
        </SortableContext>
        {issues.length === 0 && !adding && (
          <div
            onClick={editable ? () => setAdding(true) : undefined}
            className={clsx(
              'flex flex-1 items-center justify-center py-6 text-xs text-subtle',
              editable && 'cursor-pointer rounded-lg hover:bg-hover hover:text-fg',
            )}
          >
            Nothing here
          </div>
        )}
        {editable && !adding && (
          <button
            onClick={() => setAdding(true)}
            className="flex h-8 items-center gap-1.5 rounded-lg px-2 text-[13px] text-subtle hover:bg-hover hover:text-fg"
          >
            <Plus className="size-3.5" /> Add issue
          </button>
        )}
      </div>
    </section>
  )
}

function SortableCard({
  issue,
  kids,
  onOpen,
  disabled,
}: {
  issue: Issue
  kids: Kids
  onOpen: (k: string) => void
  disabled: boolean
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: issue.key,
    disabled,
  })
  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={clsx(isDragging && 'opacity-30')}
      {...attributes}
      {...listeners}
    >
      <IssueCard issue={issue} onOpen={onOpen} />
      <Subtasks parent={issue} kids={kids} onOpen={onOpen} disabled={disabled} />
    </div>
  )
}

// Subtasks that share their parent's column fold into its card (WP-69): a
// "3 subtasks" strip under the card opens them inline. One in another column
// than its parent stays a card of its own, so its status is never hidden.
type Kids = Map<string, Issue[]>
interface Folds {
  nest: boolean
  /** keys of the parents whose subtasks are showing */
  open: Set<string>
  toggle: (parentKey: string) => void
}
const NestContext = createContext<Folds>({ nest: false, open: new Set(), toggle: () => {} })

/** A cell's cards split into those shown in it and, per parent, the subtasks folded under. */
function useNested(issues: Issue[]): { top: Issue[]; kids: Kids } {
  const { nest } = useContext(NestContext)
  return useMemo(() => {
    const kids: Kids = new Map()
    if (!nest) return { top: issues, kids }
    const here = new Set(issues.map((i) => i.key))
    const top: Issue[] = []
    for (const i of issues) {
      const p = i.parent
      if (p && p.type !== 'epic' && here.has(p.key))
        kids.set(p.key, [...(kids.get(p.key) ?? []), i])
      else top.push(i)
    }
    return { top, kids }
  }, [issues, nest])
}

function Subtasks({
  parent,
  kids,
  onOpen,
  disabled,
}: {
  parent: Issue
  kids: Kids
  onOpen: (k: string) => void
  disabled: boolean
}) {
  const { open, toggle } = useContext(NestContext)
  const list = kids.get(parent.key)
  if (!list?.length) return null
  const shown = open.has(parent.key)
  const id = `subtasks-${parent.key}`
  return (
    <div className="mt-1">
      <button
        type="button"
        aria-expanded={shown}
        aria-controls={id}
        onClick={() => toggle(parent.key)}
        // Space and Enter belong to this button, not to the card's drag handle around it.
        onKeyDown={(e) => e.stopPropagation()}
        className="flex h-6 max-w-full items-center gap-1 rounded-md px-1 text-[11px] text-muted hover:bg-hover hover:text-fg"
      >
        <ChevronRight
          className={clsx('size-3 shrink-0 transition-transform', shown && 'rotate-90')}
        />
        <span className="truncate">
          {list.length === parent.child_count
            ? plural(list.length, 'subtask')
            : `${list.length} of ${parent.child_count} subtasks`}
        </span>
        <span className="shrink-0 tabular-nums text-subtle">
          · {parent.child_done}/{parent.child_count} done
        </span>
      </button>
      {shown && (
        <div id={id} className="ml-2 mt-1 flex flex-col gap-1.5 border-l border-line pl-2">
          {list.map((c) => (
            <NestedCard key={c.key} issue={c} kids={kids} onOpen={onOpen} disabled={disabled} />
          ))}
        </div>
      )}
    </div>
  )
}

/** A folded-in subtask: draggable to other columns, but not a drop target (its parent's card is). */
function NestedCard({
  issue,
  kids,
  onOpen,
  disabled,
}: {
  issue: Issue
  kids: Kids
  onOpen: (k: string) => void
  disabled: boolean
}) {
  const { attributes, listeners, setNodeRef, isDragging } = useDraggable({
    id: issue.key,
    disabled,
  })
  return (
    <div
      ref={setNodeRef}
      className={clsx(isDragging && 'opacity-30')}
      {...attributes}
      {...listeners}
    >
      <IssueCard issue={issue} onOpen={onOpen} />
      <Subtasks parent={issue} kids={kids} onOpen={onOpen} disabled={disabled} />
    </div>
  )
}

function QuickAdd({
  project,
  status,
  top,
  onDone,
  sprint,
}: {
  project: Project
  status?: number
  /** Key of the column's first issue: new ones are ranked above it, where the box is. */
  top?: string
  onDone: () => void
  sprint?: string
}) {
  const add = useQuickAdd(quickAddKey(project.key, status))
  // The issue just added is the new top until the board reloads, so a quick
  // second Enter lands above it rather than beside it.
  const added = useRef<string | undefined>(undefined)
  useEffect(() => {
    added.current = undefined
  }, [top])
  const submit = () => {
    if (!add.text.trim()) return add.pending ? undefined : onDone()
    void add
      .submit((p) => ({
        project: project.key,
        ...p.input,
        title: p.title,
        status: status ? String(status) : undefined,
        sprint,
        before: added.current ?? top,
      }))
      .then((d) => {
        if (d) added.current = d.key
      })
  }
  const details = () =>
    openDetails(
      add,
      { project: project.key, status: status ? String(status) : undefined, sprint },
      onDone,
    )
  return (
    <div className="rounded-lg border border-accent bg-raised p-2 shadow-card">
      <textarea
        autoFocus
        rows={2}
        value={add.text}
        readOnly={add.pending}
        onChange={(e) => add.setText(e.target.value)}
        onBlur={() => !add.text.trim() && !add.pending && onDone()}
        onFocus={(e) => e.currentTarget.setSelectionRange(1e9, 1e9)}
        onKeyDown={(e) => {
          if (isDetailsKey(e)) {
            e.preventDefault()
            details()
          } else if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault()
            submit()
          } else if (e.key === 'Escape') {
            add.setText('')
            onDone()
          }
        }}
        placeholder="What needs doing?  !high @me #tag ~3"
        aria-label="New issue title"
        className="w-full resize-none bg-transparent text-[13px] outline-none placeholder:text-subtle"
      />
      <LongTitleHint show={add.long} className="mb-1 text-[11px] text-muted" />
      <SaveError error={add.error} className="mb-1 text-[11px] text-danger" />
      <div className="flex items-center justify-between text-[11px] text-subtle">
        <span>↵ to add · ⌘↵ details · esc to discard</span>
        {add.pending ? <span>Adding…</span> : <QuickAddDetailsButton onOpen={details} />}
      </div>
    </div>
  )
}

/** Where a column's unsent quick-add text is kept. */
const quickAddKey = (project: string, status?: number) => `board:${project}:${status ?? ''}`
