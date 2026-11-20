import { useMemo, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  TouchSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  arrayMove,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { restrictToVerticalAxis } from './dnd'
import { CSS } from '@dnd-kit/utilities'
import clsx from 'clsx'
import { GripVertical, Plus, Trash2 } from 'lucide-react'
import { api, enc, qs } from '@/lib/api'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { StatusIcon } from '@/components/issue/icons'
import { CATEGORIES, type Category, type Project, type Status } from '@/lib/types'
import {
  ColorPicker,
  InlineText,
  List,
  ReadOnlyNote,
  TabIntro,
  isOwner,
  randomSwatch,
  useRefreshProject,
} from './shared'

export const CATEGORY_INFO: Record<Category, { label: string; help: string }> = {
  backlog: { label: 'Backlog', help: 'Parked ideas. Not on the board; they live in the Backlog.' },
  todo: { label: 'To do', help: 'Ready to pick up. Shown on the board.' },
  in_progress: {
    label: 'In progress',
    help: 'Being worked on. Entering or leaving it can start/stop timers.',
  },
  done: { label: 'Done', help: 'Finished. Counts as closed and as completed work.' },
  canceled: { label: 'Canceled', help: "Won't do. Counts as closed, but not as completed work." },
}

export function WorkflowTab({ project }: { project: Project }) {
  const owner = isOwner(project)
  const qc = useQueryClient()
  const refresh = useRefreshProject(project.key)
  const server = useMemo(
    () => [...(project.statuses ?? [])].sort((a, b) => a.position - b.position),
    [project.statuses],
  )
  const [order, setOrder] = useState<Status[] | null>(null)
  const list = order ?? server
  const [deleting, setDeleting] = useState<Status | null>(null)

  const patch = useMutation({
    mutationFn: ({
      id,
      body,
    }: {
      id: number
      body: Partial<Pick<Status, 'name' | 'color' | 'category' | 'position'>>
    }) => api.patch(`/statuses/${id}`, body),
    onError: toastError,
    onSettled: async () => {
      refresh()
      // Keep the optimistic order until the new one has arrived.
      await qc.refetchQueries({ queryKey: ['project', project.key] })
      setOrder(null)
    },
  })

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 180, tolerance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const onDragEnd = (e: DragEndEvent) => {
    if (!e.over || e.active.id === e.over.id) return
    const from = list.findIndex((s) => s.id === e.active.id)
    const to = list.findIndex((s) => s.id === e.over!.id)
    if (from < 0 || to < 0) return
    setOrder(arrayMove(list, from, to))
    patch.mutate({ id: Number(e.active.id), body: { position: to } })
  }

  return (
    <div className="space-y-5">
      <TabIntro title="Statuses">
        <p>
          Issues move through these in board order; drag to reorder. A status's category decides how
          it behaves:
        </p>
        <ul className="mt-1.5 space-y-1">
          {CATEGORIES.map((c) => (
            <li key={c} className="flex items-start gap-2">
              <StatusIcon category={c} className="mt-0.5 size-3.5 shrink-0" />
              <span>
                <b>{CATEGORY_INFO[c].label}</b>: {CATEGORY_INFO[c].help}
              </span>
            </li>
          ))}
        </ul>
        <p className="mt-1.5">
          Automations can target a category, so they survive renamed statuses.
        </p>
      </TabIntro>
      {!owner && <ReadOnlyNote />}

      <DndContext
        sensors={sensors}
        collisionDetection={closestCenter}
        modifiers={[restrictToVerticalAxis]}
        onDragEnd={onDragEnd}
      >
        <SortableContext items={list.map((s) => s.id)} strategy={verticalListSortingStrategy}>
          <List>
            {list.map((s) => (
              <StatusRow
                key={s.id}
                status={s}
                owner={owner}
                isDefault={project.settings.default_status_id === s.id}
                onPatch={(body) => patch.mutate({ id: s.id, body })}
                onDelete={list.length > 1 ? () => setDeleting(s) : undefined}
              />
            ))}
          </List>
        </SortableContext>
      </DndContext>

      {owner && <AddStatus project={project} />}

      <DeleteStatusDialog
        project={project}
        status={deleting}
        statuses={list}
        onClose={() => setDeleting(null)}
      />
    </div>
  )
}

function StatusRow({
  status,
  owner,
  isDefault,
  onPatch,
  onDelete,
}: {
  status: Status
  owner: boolean
  isDefault: boolean
  onPatch: (b: Partial<Pick<Status, 'name' | 'color' | 'category'>>) => void
  onDelete?: () => void
}) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: status.id,
    disabled: !owner,
  })
  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={clsx(
        'flex flex-wrap items-center gap-x-2 gap-y-1.5 bg-panel px-2 py-2 sm:flex-nowrap sm:px-3',
        isDragging && 'relative z-10 shadow-pop',
      )}
    >
      {owner && (
        <button
          {...attributes}
          {...listeners}
          aria-label={`Reorder ${status.name}`}
          className="flex h-8 w-6 shrink-0 cursor-grab touch-none items-center justify-center text-subtle hover:text-fg active:cursor-grabbing"
        >
          <GripVertical className="size-4" />
        </button>
      )}
      <StatusIcon category={status.category} color={status.color} className="size-4" />
      <InlineText
        value={status.name}
        onSave={(name) => onPatch({ name })}
        disabled={!owner}
        ariaLabel="Status name"
        className="min-w-0 flex-1 font-medium"
      />
      {isDefault && <Badge className="hidden sm:inline-flex">New issues</Badge>}
      <div className="ml-auto flex items-center gap-2 pl-8 sm:pl-0">
        <ColorPicker
          value={status.color}
          onChange={(color) => onPatch({ color })}
          disabled={!owner}
          label={`${status.name} color`}
        />
        <Select
          value={status.category}
          disabled={!owner}
          onChange={(e) => onPatch({ category: e.target.value as Category })}
          className="h-8 w-36 text-[13px]"
          aria-label="Category"
          title={CATEGORY_INFO[status.category].help}
        >
          {CATEGORIES.map((c) => (
            <option key={c} value={c}>
              {CATEGORY_INFO[c].label}
            </option>
          ))}
        </Select>
        {owner && (
          <IconButton
            label={onDelete ? `Delete ${status.name}` : 'A project needs at least one status'}
            disabled={!onDelete}
            onClick={onDelete}
            icon={<Trash2 className="size-3.5" />}
          />
        )}
      </div>
    </div>
  )
}

function AddStatus({ project }: { project: Project }) {
  const refresh = useRefreshProject(project.key)
  const [adding, setAdding] = useState(false)
  const [name, setName] = useState('')
  const [category, setCategory] = useState<Category>('todo')
  const add = useMutation({
    mutationFn: () =>
      api.post<Status>(`/projects/${enc(project.key)}/statuses`, {
        name: name.trim(),
        category,
        color: randomSwatch(),
      }),
    onSuccess: () => {
      setName('')
      refresh()
    },
    onError: toastError,
  })
  const close = () => {
    setAdding(false)
    setName('')
  }
  if (!adding)
    return (
      <Button variant="ghost" icon={<Plus className="size-4" />} onClick={() => setAdding(true)}>
        Add status
      </Button>
    )
  return (
    <form
      className="flex flex-wrap items-center gap-2"
      onSubmit={(e) => {
        e.preventDefault()
        if (name.trim()) add.mutate()
      }}
      onKeyDown={(e) => e.key === 'Escape' && !e.defaultPrevented && close()}
    >
      <Input
        autoFocus
        value={name}
        onChange={(e) => setName(e.target.value)}
        placeholder="Name, e.g. Waiting on parts"
        aria-label="New status name"
        className="min-w-0 flex-1 basis-56"
      />
      <Select
        value={category}
        onChange={(e) => setCategory(e.target.value as Category)}
        className="w-36"
        aria-label="Category"
      >
        {CATEGORIES.map((c) => (
          <option key={c} value={c}>
            {CATEGORY_INFO[c].label}
          </option>
        ))}
      </Select>
      <Button type="submit" variant="primary" disabled={!name.trim()} loading={add.isPending}>
        Add
      </Button>
      <Button type="button" variant="ghost" onClick={close}>
        Cancel
      </Button>
    </form>
  )
}

function DeleteStatusDialog({
  project,
  status,
  statuses,
  onClose,
}: {
  project: Project
  status: Status | null
  statuses: Status[]
  onClose: () => void
}) {
  const refresh = useRefreshProject(project.key)
  const others = statuses.filter((s) => s.id !== status?.id)
  const suggested = others.find((s) => s.category === status?.category) ?? others[0]
  const [moveTo, setMoveTo] = useState<number | null>(null)
  const target = moveTo ?? suggested?.id
  const del = useMutation({
    mutationFn: () => api.del(`/statuses/${status!.id}${qs({ move_to: target })}`),
    onSuccess: () => {
      refresh()
      setMoveTo(null)
      onClose()
    },
    onError: toastError,
  })
  const close = () => {
    setMoveTo(null)
    onClose()
  }
  return (
    <Dialog
      open={!!status}
      onClose={close}
      title={`Delete “${status?.name}”?`}
      footer={
        <>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button
            variant="danger"
            loading={del.isPending}
            disabled={!target}
            onClick={() => del.mutate()}
          >
            Delete status
          </Button>
        </>
      }
    >
      <div className="space-y-3 p-4">
        <Field
          label="Move its issues to"
          hint="Any issues in this status move there. Automations that name this status will need updating."
        >
          <Select value={target ?? ''} onChange={(e) => setMoveTo(Number(e.target.value))}>
            {others.map((s) => (
              <option key={s.id} value={s.id}>
                {s.name} ({CATEGORY_INFO[s.category].label})
              </option>
            ))}
          </Select>
        </Field>
      </div>
    </Dialog>
  )
}
