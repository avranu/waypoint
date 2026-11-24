import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  TouchSensor,
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
import {
  ArrowRight,
  ChevronDown,
  GripVertical,
  Inbox,
  MoreHorizontal,
  Play,
  Plus,
  Sparkles,
} from 'lucide-react'
import { api } from '@/lib/api'
import { invalidateProject, q, useCreateIssue, useUpdateIssue } from '@/lib/queries'
import { LongTitleHint, SaveError, useQuickAdd } from '@/components/issue/quickAdd'
import {
  QuickAddDetailsButton,
  isDetailsKey,
  openDetails,
} from '@/components/issue/QuickAddDetails'
import { points as fmtPoints, plural, shortDate } from '@/lib/format'
import { useAITask, useEstimateProject } from '@/lib/ai'
import { useLocal } from '@/lib/hooks'
import { pointerFirst, useMoveThrottle } from '@/lib/dnd'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog, Confirm } from '@/components/ui/dialog'
import { Field, Input, Textarea } from '@/components/ui/input'
import { Progress } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { IssueRow } from '@/components/issue/IssueRow'
import { TypeIcon } from '@/components/issue/icons'
import { FilterBar, useOpenIssue, useProject } from './Project'
import type { Backlog, Issue, Project, Sprint } from '@/lib/types'

const BACKLOG = 'backlog'

export function BacklogPage() {
  const project = useProject()
  const { key } = useParams({ from: '/app/p/$key/backlog' })
  const search = useSearch({ from: '/app/p/$key/backlog' })
  const agentQ = useAgentFilter(key, search.q ?? '')
  // Keep showing the last backlog while a new filter loads, instead of blanking the page.
  const data = useQuery({ ...q.backlog(key, agentQ), placeholderData: (p) => p }).data
  const [creating, setCreating] = useState(false)
  if (!data) return null
  const editable = project.role === 'owner' || project.role === 'editor'
  return (
    <div className="flex min-h-0 flex-1">
      <EpicPanel project={project} epics={data.epics} />
      <div className="min-w-0 flex-1 overflow-y-auto">
        <FilterBar
          project={project}
          placeholder="Filter the backlog…"
          extra={
            editable && (
              <>
                <EstimateAll projectKey={project.key} />
                {data.mode === 'sprints' && (
                  <Button
                    size="sm"
                    icon={<Plus className="size-3.5" />}
                    onClick={() => setCreating(true)}
                  >
                    <span className="hidden sm:inline">Create sprint</span>
                  </Button>
                )}
              </>
            )
          }
        />
        <Sections project={project} data={data} filter={agentQ} editable={editable} />
      </div>
      <SprintDialog project={project} open={creating} onClose={() => setCreating(false)} />
    </div>
  )
}

/** Asks the AI to estimate every open issue without points. */
function EstimateAll({ projectKey }: { projectKey: string }) {
  const enabled = useAITask('estimate')
  const run = useEstimateProject(projectKey)
  if (!enabled) return null
  return (
    <Button
      size="sm"
      icon={<Sparkles className="size-3.5" />}
      loading={run.isPending}
      title="Estimate open issues that have no points, with AI"
      onClick={() =>
        run.mutate(undefined, {
          onSuccess: ({ queued }) =>
            toast(
              queued
                ? `Estimating ${queued} ${queued === 1 ? 'issue' : 'issues'}; points fill in as they're ready`
                : 'Every open issue already has points',
            ),
          onError: toastError,
        })
      }
    >
      <span className="hidden sm:inline">Estimate</span>
    </Button>
  )
}

function EpicPanel({ project, epics }: { project: Project; epics: Issue[] }) {
  const navigate = useNavigate()
  const current = (useSearch({ strict: false }) as { q?: string }).q ?? ''
  const selected = current.match(/under:([A-Z0-9]+-\d+)/)?.[1]
  const [open, setOpen] = useLocal('epic-panel', true)
  const create = useCreateIssue()
  const [adding, setAdding] = useState('')
  if (!open)
    return (
      <button
        onClick={() => setOpen(true)}
        className="hidden w-8 shrink-0 border-r border-line text-[11px] text-subtle [writing-mode:vertical-rl] hover:text-fg lg:block"
      >
        Epics
      </button>
    )
  const pick = (k?: string) =>
    navigate({
      to: '.',
      search: (s: Record<string, unknown>) => {
        const rest = current.replace(/\s*under:[A-Z0-9]+-\d+/g, '').trim()
        return {
          ...s,
          q: [rest, k && k !== selected ? `under:${k}` : ''].filter(Boolean).join(' ') || undefined,
        }
      },
    } as never)
  return (
    <aside className="wp-surface hidden w-60 shrink-0 flex-col border-r border-line bg-panel/60 lg:flex">
      <div className="flex h-11 items-center justify-between px-3">
        <span className="text-xs font-semibold uppercase tracking-wide text-subtle">Epics</span>
        <button onClick={() => setOpen(false)} className="text-xs text-subtle hover:text-fg">
          Hide
        </button>
      </div>
      <div className="flex-1 space-y-0.5 overflow-y-auto px-2 pb-3">
        <button
          onClick={() => pick()}
          className={clsx(
            'flex h-8 w-full items-center rounded-lg px-2 text-left text-[13px] hover:bg-hover',
            !selected && 'bg-hover font-medium',
          )}
        >
          All issues
        </button>
        {epics.map((e) => (
          <button
            key={e.key}
            onClick={() => pick(e.key)}
            className={clsx(
              'w-full rounded-lg px-2 py-1.5 text-left hover:bg-hover',
              selected === e.key && 'bg-accent-soft',
            )}
          >
            <div className="flex items-center gap-1.5 text-[13px]">
              <TypeIcon type="epic" className="size-3" />
              <span className="truncate font-medium">{e.title}</span>
            </div>
            <div className="mt-1 flex items-center gap-2">
              <Progress
                value={e.child_done}
                max={e.child_count}
                className="flex-1"
                color="var(--wp-accent)"
              />
              <span className="text-[11px] tabular-nums text-subtle">
                {e.child_done}/{e.child_count}
              </span>
            </div>
          </button>
        ))}
        {project.role !== 'viewer' && project.role !== 'guest' && (
          <form
            onSubmit={(ev) => {
              ev.preventDefault()
              if (adding.trim())
                create.mutate(
                  { project: project.key, type: 'epic', title: adding.trim() },
                  { onSuccess: () => setAdding(''), onError: toastError },
                )
            }}
          >
            <input
              value={adding}
              onChange={(e) => setAdding(e.target.value)}
              placeholder="+ Add epic"
              aria-label="Add epic"
              className="h-8 w-full rounded-lg bg-transparent px-2 text-[13px] outline-none placeholder:text-subtle focus:bg-hover"
            />
          </form>
        )}
      </div>
    </aside>
  )
}

type Lists = Record<string, Issue[]>
const sectionId = (s: Sprint | null) => (s ? `sprint-${s.id}` : BACKLOG)

function Sections({
  project,
  data,
  filter,
  editable,
}: {
  project: Project
  data: Backlog
  filter: string
  editable: boolean
}) {
  const qc = useQueryClient()
  const open = useOpenIssue()
  const update = useUpdateIssue()
  const server = useMemo<Lists>(() => {
    const l: Lists = { [BACKLOG]: data.backlog }
    for (const s of data.sprints) l[sectionId(s.sprint)] = s.issues
    return l
  }, [data])
  const [lists, setLists] = useState<Lists>(server)
  const [active, setActive] = useState<Issue | null>(null)
  // A row dropped in another section snaps into place instead of flying back toward its origin first.
  const [crossed, setCrossed] = useState(false)
  const origin = useRef<string | null>(null)
  const throttle = useMoveThrottle()
  useEffect(() => {
    if (!active) setLists(server)
  }, [server, active])
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 220, tolerance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const find = (id: string | number): string | null => {
    if (typeof id === 'string' && (id === BACKLOG || id.startsWith('sprint-')) && lists[id])
      return id
    for (const [k, l] of Object.entries(lists)) if (l.some((i) => i.key === id)) return k
    return null
  }
  const onStart = (e: DragStartEvent) => {
    const s = find(e.active.id)
    origin.current = s
    setActive(s ? (lists[s].find((i) => i.key === e.active.id) ?? null) : null)
  }
  const onOver = (e: DragOverEvent) => {
    if (!e.over) return
    const from = find(e.active.id)
    const to = find(e.over.id)
    if (!from || !to || from === to) return
    const overId = e.over.id
    throttle(() =>
      setLists((l) => {
        // Work from the latest state: drag-over events can outpace renders.
        const src = Object.keys(l).find((k) => l[k].some((i) => i.key === e.active.id))
        const item = src ? l[src].find((i) => i.key === e.active.id) : undefined
        if (!src || !item || src === to || !l[to]) return l
        const target = l[to].filter((i) => i.key !== item.key)
        const idx = target.findIndex((i) => i.key === overId)
        target.splice(idx >= 0 ? idx : target.length, 0, item)
        return { ...l, [src]: l[src].filter((i) => i.key !== item.key), [to]: target }
      }),
    )
  }
  const onEnd = (e: DragEndEvent) => {
    const from = origin.current
    setActive(null)
    let sec = find(e.active.id)
    if (!e.over || !from || !sec) return setLists(server)
    // The last drag-over can be throttled away or not rendered yet: land in
    // the section under the pointer regardless.
    let base = lists
    const overSec = find(e.over.id)
    if (overSec && overSec !== sec && base[overSec]) {
      const item = base[sec].find((i) => i.key === e.active.id)!
      const target = base[overSec].filter((i) => i.key !== e.active.id)
      const oi = target.findIndex((i) => i.key === e.over!.id)
      target.splice(oi >= 0 ? oi : target.length, 0, item)
      base = { ...base, [sec]: base[sec].filter((i) => i.key !== e.active.id), [overSec]: target }
      sec = overSec
    }
    setCrossed(sec !== from)
    let list = base[sec]
    const oldIdx = list.findIndex((i) => i.key === e.active.id)
    const overIdx = list.findIndex((i) => i.key === e.over!.id)
    if (overIdx >= 0 && overIdx !== oldIdx) {
      list = [...list]
      const [m] = list.splice(oldIdx, 1)
      list.splice(overIdx, 0, m)
    }
    const idx = list.findIndex((i) => i.key === e.active.id)
    const issue = list[idx]
    const moved = from !== sec || (data.manual_order && idx !== oldIdx)
    if (!moved) return setLists(server)
    setLists({ ...base, [sec]: list })
    const sprint =
      sec === BACKLOG ? null : data.sprints.find((s) => sectionId(s.sprint) === sec)!.sprint
    const after = data.manual_order ? list[idx - 1]?.key : undefined
    const before = data.manual_order ? list[idx + 1]?.key : undefined
    qc.setQueryData<Backlog>(['backlog', project.key, filter], (b) =>
      b
        ? {
            ...b,
            backlog:
              sec === BACKLOG
                ? list
                : from === BACKLOG
                  ? b.backlog.filter((i) => i.key !== issue.key)
                  : b.backlog,
          }
        : b,
    )
    update.mutate(
      {
        key: issue.key,
        projectKey: project.key,
        patch: {
          ...(from !== sec
            ? data.mode === 'sprints'
              ? { sprint: sprint?.name ?? null }
              : {}
            : {}),
          ...(after ? { after } : before ? { before } : {}),
        },
      },
      { onError: (err) => (toastError(err), setLists(server)) },
    )
  }
  return (
    <DndContext
      sensors={sensors}
      collisionDetection={pointerFirst}
      onDragStart={onStart}
      onDragOver={onOver}
      onDragEnd={onEnd}
      onDragCancel={() => (setActive(null), setLists(server))}
    >
      <div className="space-y-4 px-3 pb-10 sm:px-5">
        {data.sprints.map(({ sprint }) => (
          <Section
            key={sprint.id}
            id={sectionId(sprint)}
            project={project}
            sprint={sprint}
            issues={lists[sectionId(sprint)] ?? []}
            onOpen={open}
            editable={editable}
          />
        ))}
        <Section
          id={BACKLOG}
          project={project}
          sprint={null}
          issues={lists[BACKLOG] ?? []}
          onOpen={open}
          editable={editable}
          mode={data.mode}
        />
      </div>
      <DragOverlay dropAnimation={crossed ? null : undefined}>
        {active ? (
          <div className="rounded-lg border border-accent bg-raised shadow-pop">
            <IssueRow issue={active} project={project} onOpen={() => {}} editable={false} />
          </div>
        ) : null}
      </DragOverlay>
    </DndContext>
  )
}

function Section({
  id,
  project,
  sprint,
  issues,
  onOpen,
  editable,
  mode,
}: {
  id: string
  project: Project
  sprint: Sprint | null
  issues: Issue[]
  onOpen: (k: string) => void
  editable: boolean
  mode?: string
}) {
  const { setNodeRef, isOver } = useDroppable({ id })
  const [collapsed, setCollapsed] = useLocal(`collapse-${project.key}-${id}`, false)
  const pts = issues.reduce((s, i) => s + (i.points ?? 0), 0)
  const unestimated = issues.filter((i) => i.points === null).length
  return (
    <section
      className={clsx(
        'overflow-hidden rounded-xl border bg-panel',
        sprint?.state === 'active' ? 'border-accent/50' : 'border-line',
      )}
    >
      <div className="flex min-h-11 flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 sm:px-4">
        <button
          onClick={() => setCollapsed(!collapsed)}
          className="flex items-center gap-1.5 text-[13px] font-semibold"
        >
          <ChevronDown
            className={clsx('size-4 text-subtle transition-transform', collapsed && '-rotate-90')}
          />
          {sprint ? sprint.name : 'Backlog'}
        </button>
        {sprint?.state === 'active' && (
          <span className="rounded-full bg-accent px-2 text-[11px] font-medium leading-5 text-accent-fg">
            Active
          </span>
        )}
        {sprint && (sprint.start_date || sprint.end_date) && (
          <span className="text-xs text-subtle">
            {shortDate(sprint.start_date)} – {shortDate(sprint.end_date)}
          </span>
        )}
        <span className="text-xs text-subtle">
          {plural(issues.length, 'issue')}
          {pts > 0 && ` · ${fmtPoints(pts)} pts`}
          {unestimated > 0 && issues.length > 0 && pts > 0 && ` · ${unestimated} unestimated`}
        </span>
        {sprint?.goal && (
          <span className="w-full truncate pl-5 text-xs text-muted sm:w-auto sm:pl-0">
            🎯 {sprint.goal}
          </span>
        )}
        <span className="flex-1" />
        {sprint && editable && <SprintActions project={project} sprint={sprint} />}
      </div>
      {!collapsed && (
        <div
          ref={setNodeRef}
          className={clsx(
            'min-h-12 border-t border-line transition-colors',
            isOver && 'bg-accent-soft',
          )}
        >
          <SortableContext items={issues.map((i) => i.key)} strategy={verticalListSortingStrategy}>
            {issues.map((i) => (
              <SortableRow
                key={i.key}
                issue={i}
                project={project}
                onOpen={onOpen}
                editable={editable}
                showPromote={!sprint && mode === 'kanban'}
              />
            ))}
          </SortableContext>
          {issues.length === 0 && (
            <div className="flex items-center justify-center gap-2 py-5 text-xs text-subtle">
              <Inbox className="size-3.5" />
              {sprint
                ? 'Drag issues here to plan this sprint'
                : mode === 'kanban'
                  ? 'Nothing parked. Ideas you’re not ready for go here.'
                  : 'Backlog is empty'}
            </div>
          )}
          {editable && <BacklogAdd project={project} sprint={sprint} kanban={mode === 'kanban'} />}
        </div>
      )}
    </section>
  )
}

function SortableRow({
  issue,
  project,
  onOpen,
  editable,
  showPromote,
}: {
  issue: Issue
  project: Project
  onOpen: (k: string) => void
  editable: boolean
  showPromote?: boolean
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: issue.key,
    disabled: !editable,
  })
  const update = useUpdateIssue()
  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={clsx('group relative flex items-center', isDragging && 'opacity-30')}
    >
      {editable && (
        <span className="pointer-events-none absolute left-0 z-[1] hidden h-full w-4 items-center justify-center text-subtle opacity-0 group-hover:opacity-100 sm:flex">
          <GripVertical className="size-3.5" />
        </span>
      )}
      <div
        className="min-w-0 flex-1"
        {...(editable ? { ...attributes, ...listeners, tabIndex: -1 } : {})}
      >
        <IssueRow issue={issue} project={project} onOpen={onOpen} editable={editable} />
      </div>
      {showPromote && editable && (
        <button
          title="Move to the board (To Do)"
          onClick={() =>
            update.mutate(
              { key: issue.key, projectKey: project.key, patch: { status: 'todo' } },
              { onSuccess: () => toast(`${issue.key} is on the board`), onError: toastError },
            )
          }
          className="absolute right-2 hidden h-7 items-center gap-1 rounded-md bg-raised px-2 text-xs text-muted shadow-card hover:text-fg group-hover:flex"
        >
          To board <ArrowRight className="size-3" />
        </button>
      )}
    </div>
  )
}

function BacklogAdd({
  project,
  sprint,
  kanban,
}: {
  project: Project
  sprint: Sprint | null
  kanban: boolean
}) {
  const add = useQuickAdd(`backlog:${project.key}:${sprint?.id ?? ''}`)
  const submit = () =>
    add.submit((p) => ({
      project: project.key,
      ...p.input,
      title: p.title,
      sprint: sprint?.name,
      status: kanban && !sprint ? 'backlog' : undefined,
    }))
  const details = () =>
    openDetails(add, {
      project: project.key,
      sprint: sprint?.name,
      status: kanban && !sprint ? 'backlog' : undefined,
    })
  return (
    <div className="border-t border-line">
      <form
        className="flex h-10 items-center gap-2 px-3 sm:px-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <Plus className="size-3.5 text-subtle" />
        <input
          value={add.text}
          readOnly={add.pending}
          onChange={(e) => add.setText(e.target.value)}
          placeholder={
            sprint ? `Add to ${sprint.name}…` : 'Add to backlog…  !high @me #tag ~3 /bug'
          }
          aria-label={sprint ? `Add to ${sprint.name}` : 'Add to backlog'}
          onKeyDown={(e) => {
            if (isDetailsKey(e)) {
              e.preventDefault()
              details()
            }
          }}
          className="h-full flex-1 bg-transparent text-[13px] outline-none placeholder:text-subtle"
        />
        {add.pending ? (
          <span className="text-xs text-subtle">Adding…</span>
        ) : (
          <QuickAddDetailsButton onOpen={details} />
        )}
      </form>
      <LongTitleHint show={add.long} className="-mt-1 px-3 pb-2 text-[11px] text-muted sm:px-4" />
      <SaveError error={add.error} className="-mt-1 px-3 pb-2 text-[11px] text-danger sm:px-4" />
    </div>
  )
}

function SprintActions({ project, sprint }: { project: Project; sprint: Sprint }) {
  const qc = useQueryClient()
  const m = usePopover()
  const [editing, setEditing] = useState(false)
  const [confirm, setConfirm] = useState<'delete' | 'close' | null>(null)
  const done = () => invalidateProject(qc, project.key)
  const start = useMutation({
    mutationFn: () => api.post(`/sprints/${sprint.id}/start`),
    onSuccess: () => (done(), toast(`${sprint.name} started`)),
    onError: toastError,
  })
  const close = useMutation({
    mutationFn: (to: string) => api.post(`/sprints/${sprint.id}/close`, { move_to: to }),
    onSuccess: () => (done(), toast(`${sprint.name} complete`)),
    onError: toastError,
  })
  const del = useMutation({
    mutationFn: () => api.del(`/sprints/${sprint.id}`),
    onSuccess: done,
    onError: toastError,
  })
  const hasActive = project.sprints?.some((s) => s.state === 'active')
  return (
    <div className="flex items-center gap-1">
      {sprint.state === 'planned' && !hasActive && (
        <Button
          size="sm"
          variant="primary"
          icon={<Play className="size-3" />}
          onClick={() => start.mutate()}
          loading={start.isPending}
        >
          Start
        </Button>
      )}
      {sprint.state === 'active' && (
        <Button size="sm" onClick={() => setConfirm('close')} loading={close.isPending}>
          Complete
        </Button>
      )}
      <IconButton
        ref={m.anchor}
        label="Sprint actions"
        onClick={m.toggle}
        icon={<MoreHorizontal className="size-4" />}
      />
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            { label: 'Edit sprint', onSelect: () => setEditing(true) },
            ...(sprint.state !== 'active'
              ? [{ label: 'Delete sprint', danger: true, onSelect: () => setConfirm('delete') }]
              : []),
          ]}
        />
      </Popover>
      <SprintDialog
        project={project}
        sprint={sprint}
        open={editing}
        onClose={() => setEditing(false)}
      />
      <Confirm
        open={confirm === 'delete'}
        onClose={() => setConfirm(null)}
        title={`Delete ${sprint.name}?`}
        body="Its issues go back to the backlog."
        onConfirm={() => del.mutate()}
      />
      <Confirm
        open={confirm === 'close'}
        onClose={() => setConfirm(null)}
        danger={false}
        confirmLabel="Complete sprint"
        title={`Complete ${sprint.name}?`}
        body={
          sprint.open_count
            ? `${plural(sprint.open_count, 'unfinished issue')} will move to the next sprint.`
            : 'All done!'
        }
        onConfirm={() => close.mutate('next')}
      />
    </div>
  )
}

function SprintDialog({
  project,
  sprint,
  open,
  onClose,
}: {
  project: Project
  sprint?: Sprint
  open: boolean
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [goal, setGoal] = useState('')
  const [start, setStart] = useState('')
  const [end, setEnd] = useState('')
  useEffect(() => {
    if (open) {
      setName(sprint?.name ?? '')
      setGoal(sprint?.goal ?? '')
      setStart(sprint?.start_date ?? '')
      setEnd(sprint?.end_date ?? '')
    }
  }, [open, sprint])
  const save = useMutation({
    mutationFn: () => {
      const body = {
        name: name || undefined,
        goal,
        ...(start ? { start_date: start } : {}),
        ...(end ? { end_date: end } : {}),
      }
      return sprint
        ? api.patch(`/sprints/${sprint.id}`, body)
        : api.post(`/projects/${project.key}/sprints`, body)
    },
    onSuccess: () => {
      invalidateProject(qc, project.key)
      onClose()
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={sprint ? `Edit ${sprint.name}` : 'New sprint'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={() => save.mutate()} loading={save.isPending}>
            {sprint ? 'Save' : 'Create sprint'}
          </Button>
        </>
      }
    >
      <div className="space-y-4 p-4">
        <Field label="Name" hint={sprint ? undefined : 'Leave blank for the next number'}>
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder={`${project.key} Sprint ${(project.sprints?.length ?? 0) + 1}`}
          />
        </Field>
        <Field label="Goal">
          <Textarea
            rows={2}
            value={goal}
            onChange={(e) => setGoal(e.target.value)}
            placeholder="What does this sprint achieve?"
          />
        </Field>
        <div className="grid grid-cols-2 gap-3">
          <Field label="Starts" hint={sprint ? undefined : 'Default: after the last sprint'}>
            <Input type="date" value={start} onChange={(e) => setStart(e.target.value)} />
          </Field>
          <Field
            label="Ends"
            hint={sprint ? undefined : `Default: ${project.settings.sprint_days} days`}
          >
            <Input type="date" value={end} onChange={(e) => setEnd(e.target.value)} />
          </Field>
        </div>
      </div>
    </Dialog>
  )
}
