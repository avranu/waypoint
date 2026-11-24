import { useMemo, useRef, useState } from 'react'
import { useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { AGENT_SUGGESTIONS, HUMANS_ONLY, useAgentFilter } from '@/lib/agents'
import { categoryLabel } from '@/components/issue/agentPickers'
import {
  Bot,
  Check,
  CheckSquare,
  ChevronDown,
  ListTree,
  Lock,
  LockOpen,
  Square,
  X,
} from 'lucide-react'
import { pageQ, useBulkUpdate, useReviewCloses, type IssuePatch } from '@/lib/queries'
import { Button, IconButton } from '@/components/ui/button'
import { Empty, LoadError, Skeleton } from '@/components/ui/misc'
import { Select, Switch } from '@/components/ui/input'
import { Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { IssueRow } from '@/components/issue/IssueRow'
import { PriorityIcon, StatusIcon, TypeIcon } from '@/components/issue/icons'
import { Avatar } from '@/components/ui/misc'
import { FilterBar, useOpenIssue, useProject } from './Project'
import { ISSUE_CATEGORIES, PRIORITIES, type Issue, type Project } from '@/lib/types'
import { useListNav } from '@/lib/hooks'

type Group = 'none' | 'status' | 'assignee' | 'parent' | 'type' | 'priority' | 'sprint' | 'tree'

export function ListPage() {
  const project = useProject()
  const { key } = useParams({ from: '/app/p/$key/list' })
  const search = useSearch({ from: '/app/p/$key/list' })
  const navigate = useNavigate()
  const group = (search.group as Group) ?? 'status'
  const showDone = !!search.done
  const agentQ = useAgentFilter(key, search.q ?? '')
  const res = useQuery({
    ...pageQ.list(key, agentQ, showDone),
    placeholderData: (p) => p,
  })
  const setSearch = (patch: Record<string, unknown>) =>
    navigate({
      to: '.',
      search: (s: Record<string, unknown>) => ({ ...s, ...patch }),
      replace: true,
    } as never)
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const open = useOpenIssue()
  const issues = res.data?.issues ?? []
  const editable = project.role === 'owner' || project.role === 'editor'

  const toggle = (k: string) => {
    const s = new Set(selected)
    if (s.has(k)) s.delete(k)
    else s.add(k)
    setSelected(s)
  }
  const onOpen = (k: string) => (selected.size ? toggle(k) : open(k))
  const listRef = useRef<HTMLDivElement>(null)
  useListNav(listRef, { onOpen: open, onSelect: editable ? toggle : undefined })

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <FilterBar
        project={project}
        placeholder="Filter… (e.g. type:bug priority:>=high label:frontend)"
        extra={
          <div className="flex shrink-0 items-center gap-2">
            <Select
              value={group}
              onChange={(e) => setSearch({ group: e.target.value })}
              className="!h-8 w-auto text-[13px]"
            >
              <option value="status">Group: Status</option>
              <option value="tree">Hierarchy</option>
              <option value="assignee">Group: Assignee</option>
              <option value="parent">Group: Parent</option>
              <option value="type">Group: Type</option>
              <option value="priority">Group: Priority</option>
              {project.settings.mode === 'sprints' && <option value="sprint">Group: Sprint</option>}
              <option value="none">No grouping</option>
            </Select>
            <label className="hidden items-center gap-2 text-[13px] text-muted sm:flex">
              <Switch
                checked={showDone}
                onChange={(v) => setSearch({ done: v || undefined })}
                label="Show completed"
              />
              Completed
            </label>
          </div>
        }
      />
      <div ref={listRef} className="wp-surface min-h-0 flex-1 overflow-y-auto pb-24">
        {res.isPending ? (
          <div className="space-y-2 p-5">
            {Array.from({ length: 8 }, (_, i) => (
              <Skeleton key={i} className="h-8" />
            ))}
          </div>
        ) : res.isError && issues.length === 0 ? (
          <LoadError what="The issues" error={res.error} />
        ) : issues.length === 0 ? (
          <Empty icon={<ListTree />} title="No issues match">
            {showDone
              ? 'Try a different filter.'
              : 'Nothing open matches. Toggle “Completed” to include finished work.'}
          </Empty>
        ) : group === 'tree' ? (
          <Tree
            issues={issues}
            project={project}
            onOpen={onOpen}
            selected={selected}
            onSelect={editable ? toggle : undefined}
          />
        ) : (
          <Grouped
            issues={issues}
            project={project}
            group={group}
            onOpen={onOpen}
            selected={selected}
            onSelect={editable ? toggle : undefined}
          />
        )}
        <div className="px-5 py-3 text-xs text-subtle">
          {res.data && `${res.data.total} issue${res.data.total === 1 ? '' : 's'}`}
          {!showDone && (
            <button
              className="ml-2 text-accent hover:underline"
              onClick={() => setSearch({ done: true })}
            >
              Show completed
            </button>
          )}
        </div>
      </div>
      {selected.size > 0 && (
        <BulkBar
          project={project}
          keys={[...selected]}
          closesToReview={issues
            .filter((i) => selected.has(i.key) && i.close_unreviewed)
            .map((i) => i.key)}
          onClear={() => setSelected(new Set())}
        />
      )}
    </div>
  )
}

function SelectBox({ on, onClick }: { on: boolean; onClick: () => void }) {
  return (
    <button
      onClick={(e) => {
        e.stopPropagation()
        onClick()
      }}
      className={clsx(
        'flex size-6 shrink-0 items-center justify-center text-subtle hover:text-fg',
        !on && 'opacity-0 group-hover/sel:opacity-100 max-sm:opacity-60',
      )}
      aria-label={on ? 'Deselect' : 'Select'}
    >
      {on ? <CheckSquare className="size-4 text-accent" /> : <Square className="size-4" />}
    </button>
  )
}

function Row({
  issue,
  project,
  onOpen,
  selected,
  onSelect,
  depth,
  expanded,
  onToggle,
}: {
  issue: Issue
  project: Project
  onOpen: (k: string) => void
  selected: Set<string>
  onSelect?: (k: string) => void
  depth?: number
  expanded?: boolean
  onToggle?: () => void
}) {
  return (
    <div className="group/sel relative flex items-center">
      {onSelect && (
        <div className="absolute left-0 z-[1] pl-0.5">
          <SelectBox on={selected.has(issue.key)} onClick={() => onSelect(issue.key)} />
        </div>
      )}
      <div className="min-w-0 flex-1 pl-4">
        <IssueRow
          issue={issue}
          project={project}
          onOpen={onOpen}
          selected={selected.has(issue.key)}
          depth={depth}
          expanded={expanded}
          onToggle={onToggle}
          showUpdated
        />
      </div>
    </div>
  )
}

function groupOf(i: Issue, g: Group): [string, string] {
  switch (g) {
    case 'status':
      return [String(i.status.position).padStart(3, '0') + i.status.name, i.status.name]
    case 'assignee':
      return i.assignees.length
        ? [i.assignees[0].display_name, i.assignees[0].display_name]
        : ['~', 'Unassigned']
    case 'parent':
      return i.parent ? [i.parent.key, `${i.parent.key} ${i.parent.title}`] : ['~', 'No parent']
    case 'type':
      return [i.type, i.type[0].toUpperCase() + i.type.slice(1)]
    case 'priority':
      return [
        String(9 - i.priority),
        i.priority
          ? PRIORITIES[i.priority][0].toUpperCase() + PRIORITIES[i.priority].slice(1)
          : 'No priority',
      ]
    case 'sprint':
      return i.sprint_name ? [i.sprint_name, i.sprint_name] : ['~', 'Backlog']
  }
  return ['', '']
}

function groupIcon(i: Issue, g: Group) {
  switch (g) {
    case 'status':
      return <StatusIcon category={i.status.category} color={i.status.color} />
    case 'assignee':
      return <Avatar user={i.assignees[0] ?? null} size={18} />
    case 'type':
      return <TypeIcon type={i.type} />
    case 'priority':
      return <PriorityIcon priority={i.priority} className="size-3.5 text-muted" />
    case 'parent':
      return i.parent ? <TypeIcon type={i.parent.type} /> : null
  }
  return null
}

function Grouped({
  issues,
  project,
  group,
  onOpen,
  selected,
  onSelect,
}: {
  issues: Issue[]
  project: Project
  group: Group
  onOpen: (k: string) => void
  selected: Set<string>
  onSelect?: (k: string) => void
}) {
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set())
  const groups = useMemo(() => {
    if (group === 'none') return [{ sort: '', label: '', items: issues }]
    const m = new Map<string, { sort: string; label: string; items: Issue[] }>()
    for (const i of issues) {
      const [sort, label] = groupOf(i, group)
      if (!m.has(sort)) m.set(sort, { sort, label, items: [] })
      m.get(sort)!.items.push(i)
    }
    return [...m.values()].sort((a, b) => a.sort.localeCompare(b.sort))
  }, [issues, group])
  return (
    <div>
      {groups.map((g) => (
        <div key={g.sort}>
          {g.label && (
            <button
              onClick={() => {
                const s = new Set(collapsed)
                if (s.has(g.sort)) s.delete(g.sort)
                else s.add(g.sort)
                setCollapsed(s)
              }}
              className="sticky top-0 z-[2] flex h-9 w-full items-center gap-2 border-b border-line bg-sunken/95 px-4 text-[13px] font-medium backdrop-blur sm:px-5"
            >
              <ChevronDown
                className={clsx(
                  'size-3.5 text-subtle transition-transform',
                  collapsed.has(g.sort) && '-rotate-90',
                )}
              />
              {groupIcon(g.items[0], group)}
              <span className="truncate">{g.label}</span>
              <span className="text-xs font-normal text-subtle">{g.items.length}</span>
            </button>
          )}
          {!collapsed.has(g.sort) &&
            g.items.map((i) => (
              <Row
                key={i.key}
                issue={i}
                project={project}
                onOpen={onOpen}
                selected={selected}
                onSelect={onSelect}
              />
            ))}
        </div>
      ))}
    </div>
  )
}

function Tree({
  issues,
  project,
  onOpen,
  selected,
  onSelect,
}: {
  issues: Issue[]
  project: Project
  onOpen: (k: string) => void
  selected: Set<string>
  onSelect?: (k: string) => void
}) {
  const [expanded, setExpanded] = useState<Set<number>>(
    () => new Set(issues.filter((i) => i.type === 'epic').map((i) => i.id)),
  )
  const { roots, children } = useMemo(() => {
    const ids = new Set(issues.map((i) => i.id))
    const children = new Map<number, Issue[]>()
    const roots: Issue[] = []
    for (const i of issues) {
      if (i.parent_id && ids.has(i.parent_id)) {
        if (!children.has(i.parent_id)) children.set(i.parent_id, [])
        children.get(i.parent_id)!.push(i)
      } else roots.push(i)
    }
    return { roots, children }
  }, [issues])
  const render = (i: Issue, depth: number): React.ReactNode[] => {
    const kids = children.get(i.id) ?? []
    const isOpen = expanded.has(i.id)
    const out: React.ReactNode[] = [
      <Row
        key={i.key}
        issue={kids.length ? i : { ...i, child_count: 0 }}
        project={project}
        onOpen={onOpen}
        selected={selected}
        onSelect={onSelect}
        depth={depth}
        expanded={isOpen}
        onToggle={() => {
          const s = new Set(expanded)
          if (s.has(i.id)) s.delete(i.id)
          else s.add(i.id)
          setExpanded(s)
        }}
      />,
    ]
    if (isOpen) for (const k of kids) out.push(...render(k, depth + 1))
    return out
  }
  return (
    <div>
      <div className="flex h-8 items-center gap-3 border-b border-line px-5 text-xs text-subtle">
        <button
          className="hover:text-fg"
          onClick={() => setExpanded(new Set(issues.map((i) => i.id)))}
        >
          Expand all
        </button>
        <button className="hover:text-fg" onClick={() => setExpanded(new Set())}>
          Collapse all
        </button>
      </div>
      {roots.flatMap((r) => render(r, 0))}
    </div>
  )
}

function BulkBar({
  project,
  keys,
  closesToReview,
  onClear,
}: {
  project: Project
  keys: string[]
  /** the selected issues agents closed whose close no person reviewed (WP-272) */
  closesToReview: string[]
  onClear: () => void
}) {
  const bulk = useBulkUpdate()
  const reviewCloses = useReviewCloses()
  const [descendants, setDescendants] = useState(true)
  const apply = (patch: IssuePatch, label: string) =>
    bulk.mutate(
      { issues: keys, patch, descendants, projectKey: project.key },
      {
        onSuccess: (r) => {
          toast(`${label}: ${r.updated.length} issue${r.updated.length === 1 ? '' : 's'} updated`)
          onClear()
        },
        onError: toastError,
      },
    )
  return (
    <div className="animate-up pb-safe absolute inset-x-3 bottom-3 z-20 flex flex-wrap items-center gap-2 rounded-2xl border border-line bg-raised px-3 py-2 shadow-pop sm:inset-x-auto sm:left-1/2 sm:-translate-x-1/2">
      <span className="text-[13px] font-medium">{keys.length} selected</span>
      <BulkPicker
        label="Status"
        items={(project.statuses ?? []).map((s) => ({
          value: String(s.id),
          label: s.name,
          icon: <StatusIcon category={s.category} color={s.color} />,
        }))}
        onPick={(v) => apply({ status: v }, 'Status')}
      />
      <BulkPicker
        label="Priority"
        items={[4, 3, 2, 1, 0].map((p) => ({
          value: String(p),
          label: PRIORITIES[p],
          icon: <PriorityIcon priority={p} className="size-3.5 text-muted" />,
        }))}
        onPick={(v) => apply({ priority: Number(v) }, 'Priority')}
      />
      {project.settings.mode === 'sprints' && (
        <BulkPicker
          label="Sprint"
          items={[
            { value: '', label: 'Backlog' },
            ...(project.sprints ?? [])
              .filter((s) => s.state !== 'closed')
              .map((s) => ({ value: s.name, label: s.name })),
          ]}
          onPick={(v) => apply({ sprint: v || null }, 'Sprint')}
        />
      )}
      {}
      <BulkPicker
        label="Assign"
        items={[
          { value: '', label: 'Unassigned' },
          ...(project.members ?? []).map((m) => ({
            value: m.user.username,
            label: m.user.display_name,
            icon: <Avatar user={m.user} size={18} />,
          })),
          {
            value: 'agent:any',
            label: 'Any agent',
            icon: <Bot className="size-3.5" />,
            hint: 'agent',
          },
          ...AGENT_SUGGESTIONS.map((a) => ({
            value: `agent:${a}`,
            label: a,
            icon: <Bot className="size-3.5 text-muted" />,
            hint: 'agent',
          })),
        ]}
        onPick={(v) =>
          v.startsWith('agent:')
            ? apply({ agent: v.slice(6) }, 'Agent')
            : apply({ assignees: v ? [v] : [] }, 'Assignee')
        }
      />
      <BulkPicker
        label="Humans only"
        items={[
          { value: HUMANS_ONLY, label: 'Humans only', icon: <Lock className="size-3.5" /> },
          { value: '', label: 'Agents may take them', icon: <LockOpen className="size-3.5" /> },
        ]}
        onPick={(v) => apply({ agent: v }, v ? 'Humans only' : 'Unlocked')}
      />
      <BulkPicker
        label="Category"
        items={[
          { value: '', label: 'No category' },
          ...ISSUE_CATEGORIES.map((c) => ({ value: c, label: categoryLabel(c) })),
        ]}
        onPick={(v) => apply({ category: v }, 'Category')}
      />
      {closesToReview.length > 0 && (
        <Button
          size="sm"
          disabled={reviewCloses.isPending}
          title="Mark agents' closes of the selected issues reviewed"
          onClick={() =>
            reviewCloses.mutate(
              { issues: closesToReview, projectKey: project.key },
              {
                onSuccess: (r) => {
                  toast(`Reviewed ${r.reviewed} close${r.reviewed === 1 ? '' : 's'}`)
                  onClear()
                },
                onError: toastError,
              },
            )
          }
        >
          <Check className="size-3.5" />
          Review {closesToReview.length} close{closesToReview.length === 1 ? '' : 's'}
        </Button>
      )}
      <label
        className="flex items-center gap-1.5 text-xs text-muted"
        title="Also apply to everything beneath the selected issues"
      >
        <Switch checked={descendants} onChange={setDescendants} label="Include sub-issues" />+
        sub-issues
      </label>
      <IconButton label="Clear selection" onClick={onClear} icon={<X className="size-4" />} />
    </div>
  )
}

function BulkPicker({
  label,
  items,
  onPick,
}: {
  label: string
  items: PickItem<string>[]
  onPick: (v: string) => void
}) {
  const p = usePopover()
  return (
    <>
      <Button ref={p.anchor} size="sm" onClick={p.toggle}>
        {label}
      </Button>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[]}
          onClose={p.close}
          onPick={onPick}
          placeholder={`Set ${label.toLowerCase()}…`}
        />
      </Popover>
    </>
  )
}
