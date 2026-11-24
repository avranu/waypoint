import { useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  CalendarDays,
  CircleSlash,
  CornerLeftUp,
  Hash,
  Timer as TimerIcon,
  Tag,
  X,
} from 'lucide-react'
import { Popover, Picker, usePopover, type PickItem } from '@/components/ui/popover'
import { Avatar, AvatarStack } from '@/components/ui/misc'
import { EstimatePanel } from './AI'
import { issuePickItem } from './IssuePeek'
import { PriorityIcon, StatusIcon, TypeIcon, typeLabel } from './icons'
import { q, usePrefetch } from '@/lib/queries'
import { dueState, points as fmtPoints, shortDate, todayISO } from '@/lib/format'
import { useDebounced } from '@/lib/hooks'
import {
  AGENT_SUGGESTIONS,
  agentLabel,
  agentMatches,
  isDelegated,
  knownAgentsQuery,
} from '@/lib/agents'
import {
  ISSUE_TYPES,
  PRIORITIES,
  type IssueRef,
  type IssueType,
  type Label,
  type Sprint,
  type Status,
  type UserRef,
} from '@/lib/types'

/** Agents the assignee list offers: any agent, families with more than one, agents seen, then suggestions. */
export function useAgentItems(value: string, enabled: boolean): PickItem<string>[] {
  const delegated = isDelegated(value)
  const known = useQuery({ ...knownAgentsQuery(), enabled: enabled || delegated }).data ?? []
  return useMemo(() => {
    const reach = (spec: string) => {
      if (!known.length) return 'agent'
      const n = known.filter((a) => agentMatches(spec, a.identity)).length
      return n ? `${n} agent${n === 1 ? '' : 's'}` : 'none seen yet'
    }
    const base: PickItem<string>[] = [
      { value: 'any', label: 'Any agent', icon: <Bot className="size-3.5" />, hint: reach('any') },
    ]
    const seen = new Set(base.map((b) => b.value))
    const add = (it: PickItem<string>) => {
      if (seen.has(it.value)) return
      seen.add(it.value)
      base.push(it)
    }
    if (delegated)
      add({
        value,
        label: agentLabel(value),
        icon: <Bot className="size-3.5" />,
        hint: reach(value),
      })
    const families = new Map<string, number>()
    for (const a of known) families.set(a.family, (families.get(a.family) ?? 0) + 1)
    for (const [f, n] of families)
      if (n > 1)
        add({
          value: f,
          label: f,
          icon: <Bot className="size-3.5 text-muted" />,
          hint: `${n} agents`,
        })
    for (const a of known)
      add({
        value: a.identity,
        label: a.identity,
        icon: <Bot className="size-3.5 text-muted" />,
        hint: 'agent',
        keywords: [a.family, ...a.models].join(' '),
      })
    for (const s of AGENT_SUGGESTIONS)
      add({ value: s, label: s, icon: <Bot className="size-3.5 text-muted" />, hint: reach(s) })
    return base
  }, [value, delegated, known])
}

/** The clickable face of a property. */
export function PropButton({
  anchor,
  onClick,
  children,
  className,
  disabled,
  title,
  variant = 'pill',
}: {
  anchor?: React.Ref<HTMLButtonElement>
  onClick?: (e: React.MouseEvent) => void
  children: ReactNode
  className?: string
  disabled?: boolean
  title?: string
  variant?: 'pill' | 'row' | 'bare'
}) {
  return (
    <button
      ref={anchor}
      type="button"
      title={title}
      disabled={disabled}
      onClick={(e) => {
        e.stopPropagation()
        e.preventDefault()
        onClick?.(e)
      }}
      className={clsx(
        'inline-flex min-w-0 items-center gap-1.5 text-[13px] transition-colors disabled:cursor-default',
        variant === 'pill' && 'h-7 rounded-md px-2 hover:bg-hover disabled:hover:bg-transparent',
        variant === 'row' &&
          'h-8 w-full rounded-md px-2 text-left hover:bg-hover disabled:hover:bg-transparent',
        variant === 'bare' && 'rounded hover:bg-hover',
        className,
      )}
    >
      {children}
    </button>
  )
}

type Variant = 'pill' | 'row' | 'bare'

/**
 * An unset field's face: its own name on a pill ("Due", "Tags"), "None" in a
 * labelled row where the name is already beside it (WP-1196).
 */
export function unsetText(variant: Variant | undefined, name: string) {
  return variant === 'row' ? 'None' : name
}

export function StatusPicker({
  statuses,
  value,
  onChange,
  disabled,
  variant = 'pill',
  iconOnly,
  openChildren = 0,
}: {
  statuses: Status[]
  value: Status
  onChange: (s: Status) => void
  disabled?: boolean
  variant?: Variant
  iconOnly?: boolean
  /** its sub-tasks still open: a Done status says it can't be picked yet */
  openChildren?: number
}) {
  const p = usePopover()
  const items: PickItem<number>[] = statuses.map((s, i) => ({
    value: s.id,
    label: s.name,
    icon: <StatusIcon category={s.category} color={s.color} />,
    // The server refuses to finish a parent while a sub-task is open; this hint only warns before the click.
    hint:
      s.category === 'done' && openChildren > 0 && value.category !== 'done'
        ? `${openChildren} sub-task${openChildren === 1 ? '' : 's'} open`
        : i < 9
          ? String(i + 1)
          : undefined,
    keywords: s.category.replace('_', ' '),
  }))
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title={value.name}
      >
        <StatusIcon category={value.category} color={value.color} />
        {!iconOnly && <span className="truncate">{value.name}</span>}
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value.id]}
          placeholder="Change status…"
          onClose={p.close}
          onPick={(id) => {
            const s = statuses.find((x) => x.id === id)
            if (s && s.id !== value.id) onChange(s)
          }}
        />
      </Popover>
    </>
  )
}

export function PriorityPicker({
  value,
  onChange,
  disabled,
  variant = 'pill',
  iconOnly,
}: {
  value: number
  onChange: (p: number) => void
  disabled?: boolean
  variant?: Variant
  iconOnly?: boolean
}) {
  const p = usePopover()
  const items: PickItem<number>[] = [4, 3, 2, 1, 0].map((n) => ({
    value: n,
    label: n === 0 ? 'No priority' : PRIORITIES[n][0].toUpperCase() + PRIORITIES[n].slice(1),
    icon: <PriorityIcon priority={n} className={clsx('size-3.5', n !== 4 && 'text-muted')} />,
  }))
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title={`Priority: ${PRIORITIES[value]}`}
      >
        <PriorityIcon priority={value} className={clsx('size-3.5', value !== 4 && 'text-muted')} />
        {!iconOnly && (
          <span className={clsx('truncate', value === 0 && 'text-subtle')}>
            {value === 0
              ? unsetText(variant, 'Priority')
              : PRIORITIES[value][0].toUpperCase() + PRIORITIES[value].slice(1)}
          </span>
        )}
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value]}
          placeholder="Set priority…"
          onClose={p.close}
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

export function TypePicker({
  value,
  onChange,
  disabled,
  variant = 'pill',
}: {
  value: IssueType
  onChange: (t: IssueType) => void
  disabled?: boolean
  variant?: Variant
}) {
  const p = usePopover()
  const items: PickItem<string>[] = ISSUE_TYPES.map((t) => ({
    value: t,
    label: typeLabel(t),
    icon: <TypeIcon type={t} />,
  }))
  return (
    <>
      <PropButton anchor={p.anchor} onClick={p.toggle} disabled={disabled} variant={variant}>
        <TypeIcon type={value} />
        <span>{typeLabel(value)}</span>
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value]}
          onClose={p.close}
          onPick={(v) => v !== value && onChange(v as IssueType)}
          placeholder="Issue type…"
        />
      </Popover>
    </>
  )
}

/**
 * Who is on it: people, and (with onAgent) agents in the same list, since
 * handing a task to an agent is assigning it (WP-1196). Agent values are
 * issue.agent specs; picking the chosen one again hands it back to anyone.
 */
export function AssigneePicker({
  value,
  onChange,
  disabled,
  variant = 'pill',
  members,
  compact,
  agent,
  onAgent,
}: {
  value: UserRef[]
  onChange: (usernames: string[]) => void
  disabled?: boolean
  variant?: Variant
  members?: number[]
  compact?: boolean
  /** the issue's agent spec ('' anyone, 'none' humans only, else delegated) */
  agent?: string
  onAgent?: (spec: string) => void
}) {
  const p = usePopover()
  const users = useQuery({ ...q.users(), enabled: p.open || !!value.length }).data ?? []
  const me = useQuery(q.me()).data
  const delegated = isDelegated(agent)
  const agentItems = useAgentItems(agent ?? '', p.open && !!onAgent)
  const items: PickItem<string>[] = useMemo(() => {
    const sorted = [...users].sort((a, b) =>
      a.id === me?.id ? -1 : b.id === me?.id ? 1 : a.display_name.localeCompare(b.display_name),
    )
    return [
      ...sorted.map((u) => ({
        value: u.username,
        label: u.id === me?.id ? `${u.display_name} (me)` : u.display_name,
        icon: <Avatar user={u} size={20} />,
        keywords: u.username,
        hint: members && !members.includes(u.id) ? 'not in project' : undefined,
      })),
      ...(onAgent ? agentItems.map((a) => ({ ...a, value: AGENT_PREFIX + a.value })) : []),
    ]
  }, [users, me, members, onAgent, agentItems])
  const selected = [
    ...value.map((u) => u.username),
    ...(delegated && onAgent ? [AGENT_PREFIX + agent] : []),
  ]
  const people = value.map((u) => u.username)
  const agentFace = delegated && (
    <span className="flex min-w-0 items-center gap-1 text-accent" title={`For agents: ${agent}`}>
      <Bot className="size-3.5 shrink-0" />
      {!compact && <span className="truncate">{agentLabel(agent!)}</span>}
    </span>
  )
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title="Assignees"
      >
        {value.length === 0 ? (
          agentFace || (
            <>
              <Avatar user={null} size={18} />
              {!compact && <span className="text-subtle">{unsetText(variant, 'Assignee')}</span>}
            </>
          )
        ) : value.length === 1 && !compact && !delegated ? (
          <>
            <Avatar user={value[0]} size={18} />
            <span className="truncate">{value[0].display_name}</span>
          </>
        ) : (
          <>
            <AvatarStack users={value} size={18} />
            {agentFace}
          </>
        )}
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          multi
          items={items}
          selected={selected}
          placeholder={onAgent ? 'Assign to a person or agent…' : 'Assign to…'}
          onPick={(u, on) => {
            if (u.startsWith(AGENT_PREFIX)) onAgent?.(on ? u.slice(AGENT_PREFIX.length) : '')
            else onChange(on ? [...people, u] : people.filter((x) => x !== u))
          }}
        />
      </Popover>
    </>
  )
}

const AGENT_PREFIX = 'agent:'

export function LabelPicker({
  labels,
  value,
  onChange,
  disabled,
  variant = 'pill',
  autoOpen,
}: {
  labels: Label[]
  value: Label[]
  onChange: (names: string[]) => void
  disabled?: boolean
  variant?: Variant
  /** open the picker on mount (chosen from a "More" menu) */
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const selected = value.map((l) => l.name)
  const others = useQuery({ ...q.tags(), enabled: p.open }).data ?? []
  const items: PickItem<string>[] = useMemo(() => {
    const seen = new Set(labels.map((l) => l.name.toLowerCase()))
    return [
      ...labels.map((l) => ({
        value: l.name,
        label: l.name,
        icon: <span className="size-2.5 rounded-full" style={{ background: l.color }} />,
      })),
      ...others
        .filter((t) => !seen.has(t.name.toLowerCase()))
        .map((t) => ({
          value: t.name,
          label: t.name,
          hint: 'other projects',
          icon: (
            <span className="size-2.5 rounded-full opacity-60" style={{ background: t.color }} />
          ),
        })),
    ]
  }, [labels, others])
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        className="flex-wrap !h-auto min-h-7 py-1"
      >
        {value.length === 0 ? (
          <>
            <Tag className="size-3.5 text-subtle" />
            <span className="text-subtle">{unsetText(variant, 'Tags')}</span>
          </>
        ) : (
          value.map((l) => <LabelChip key={l.id} label={l} />)
        )}
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          multi
          items={items}
          selected={selected}
          placeholder="Tags…"
          onPick={(n, on) => onChange(on ? [...selected, n] : selected.filter((x) => x !== n))}
          onCreate={(t) => onChange([...selected, t])}
          createLabel={(t) => `Create tag “${t}”`}
        />
      </Popover>
    </>
  )
}

export function LabelChip({ label, onRemove }: { label: Label; onRemove?: () => void }) {
  return (
    <span
      className="inline-flex h-5 items-center gap-1 rounded-full border px-2 text-[11px] font-medium"
      style={{
        borderColor: `color-mix(in oklab, ${label.color} 45%, transparent)`,
        color: `color-mix(in oklab, ${label.color} 75%, var(--wp-fg))`,
      }}
    >
      <span className="size-1.5 rounded-full" style={{ background: label.color }} />
      {label.name}
      {onRemove && (
        <button onClick={onRemove} aria-label={`Remove ${label.name}`}>
          <X className="size-3" />
        </button>
      )}
    </span>
  )
}

export function SprintPicker({
  sprints,
  value,
  valueName,
  onChange,
  disabled,
  variant = 'pill',
  autoOpen,
}: {
  sprints: Sprint[]
  value: number | null
  valueName?: string
  onChange: (name: string | null) => void
  disabled?: boolean
  variant?: Variant
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const open = sprints.filter((s) => s.state !== 'closed')
  const label = sprints.find((s) => s.id === value)?.name ?? (value ? valueName : undefined)
  const items: PickItem<number | null>[] = [
    {
      value: null,
      label: 'Backlog (no sprint)',
      icon: <CircleSlash className="size-3.5 text-subtle" />,
    },
    ...open.map((s) => ({
      value: s.id,
      label: s.name,
      hint: s.state === 'active' ? 'active' : s.start_date ? shortDate(s.start_date) : undefined,
    })),
  ]
  return (
    <>
      <PropButton anchor={p.anchor} onClick={p.toggle} disabled={disabled} variant={variant}>
        <TimerIcon className="size-3.5 text-subtle" />
        <span className={clsx('truncate', !label && 'text-subtle')}>
          {label ?? unsetText(variant, 'Sprint')}
        </span>
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value]}
          placeholder="Move to sprint…"
          onClose={p.close}
          onPick={(id) =>
            onChange(id === null ? null : (open.find((s) => s.id === id)?.name ?? null))
          }
        />
      </Popover>
    </>
  )
}

export function ParentPicker({
  projectKey,
  value,
  onChange,
  exclude,
  disabled,
  variant = 'pill',
  autoOpen,
}: {
  projectKey: string
  value?: IssueRef | null
  onChange: (key: string | null) => void
  /** keys that can't be the parent: the issue itself and its direct subtasks */
  exclude?: string[]
  disabled?: boolean
  variant?: Variant
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const [text, setText] = useState('')
  const debounced = useDebounced(text, 150)
  const parentSearch = (t: string) =>
    q.search(`${t} is:open ${t ? '' : 'type:epic,story order:-updated'}`.trim(), {
      project: projectKey,
      limit: 30 + (exclude?.length ?? 0),
    })
  usePrefetch(parentSearch(''), !disabled)
  const search = useQuery({
    ...parentSearch(debounced.trim()),
    enabled: p.open,
    placeholderData: (prev) => prev,
  })
  const results = search.data
  const items: PickItem<string | null>[] = [
    ...(value
      ? [
          {
            value: null,
            fixed: true,
            label: 'Remove parent',
            icon: <X className="size-3.5 text-subtle" />,
          },
        ]
      : []),
    ...(results?.issues ?? []).filter((i) => !exclude?.includes(i.key)).map(issuePickItem),
  ]
  return (
    <>
      <PropButton anchor={p.anchor} onClick={p.toggle} disabled={disabled} variant={variant}>
        {value ? (
          <>
            <TypeIcon type={value.type} />
            <span className="truncate">
              <span className="text-subtle">{value.key}</span> {value.title}
            </span>
          </>
        ) : (
          <>
            <CornerLeftUp className="size-3.5 text-subtle" />
            <span className="text-subtle">{unsetText(variant, 'Parent')}</span>
          </>
        )}
      </PropButton>
      <Popover open={p.open} onClose={() => (p.close(), setText(''))} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value?.key ?? null]}
          placeholder="Search for a parent…"
          onClose={p.close}
          onSearch={setText}
          searching={text !== debounced || search.isPlaceholderData}
          wide
          onPick={(k) => onChange(k)}
        />
      </Popover>
    </>
  )
}

export function DatePicker({
  value,
  onChange,
  placeholder = 'Date',
  disabled,
  variant = 'pill',
  closed,
  icon,
  autoOpen,
}: {
  value: string | null
  onChange: (d: string | null) => void
  placeholder?: string
  disabled?: boolean
  variant?: Variant
  closed?: boolean
  icon?: ReactNode
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const state = dueState(value, closed)
  const quick: [string, string][] = [
    ['Today', todayISO()],
    ['Tomorrow', todayISO(1)],
    ['In 3 days', todayISO(3)],
    ['Next week', todayISO(7)],
    ['In 2 weeks', todayISO(14)],
  ]
  return (
    <>
      <PropButton anchor={p.anchor} onClick={p.toggle} disabled={disabled} variant={variant}>
        {icon ?? (
          <CalendarDays
            className={clsx(
              'size-3.5',
              state === 'overdue' ? 'text-danger' : state === 'today' ? 'text-warn' : 'text-subtle',
            )}
          />
        )}
        <span
          className={clsx(
            'truncate',
            !value && 'text-subtle',
            state === 'overdue' && 'text-danger',
            state === 'today' && 'text-warn',
          )}
        >
          {value ? shortDate(value) : unsetText(variant, placeholder)}
        </span>
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <div className="flex w-full flex-col gap-1 p-1 sm:w-56">
          <input
            type="date"
            value={value ?? ''}
            onChange={(e) => {
              onChange(e.target.value || null)
              p.close()
            }}
            className="h-9 rounded-lg border border-line bg-panel px-2 text-[13px]"
          />
          {quick.map(([label, d]) => (
            <button
              key={label}
              onClick={() => {
                onChange(d)
                p.close()
              }}
              className="flex h-8 items-center justify-between rounded-lg px-2.5 text-[13px] hover:bg-hover"
            >
              {label}
              <span className="text-xs text-subtle">{shortDate(d)}</span>
            </button>
          ))}
          {value && (
            <button
              onClick={() => {
                onChange(null)
                p.close()
              }}
              className="flex h-8 items-center gap-2 rounded-lg px-2.5 text-[13px] text-muted hover:bg-hover"
            >
              <X className="size-3.5" /> Clear
            </button>
          )}
        </div>
      </Popover>
    </>
  )
}

const FIB = [0.5, 1, 2, 3, 5, 8, 13, 21]

export function PointsPicker({
  value,
  onChange,
  disabled,
  variant = 'pill',
  estimateKey,
  autoOpen,
}: {
  value: number | null
  onChange: (v: number | null) => void
  disabled?: boolean
  variant?: Variant
  /** An existing issue's key: offers an AI estimate inside the picker. */
  estimateKey?: string
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const [custom, setCustom] = useState('')
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title="Story points"
      >
        <Hash className="size-3.5 text-subtle" />
        <span className={clsx(value === null && 'text-subtle')}>
          {value === null ? unsetText(variant, 'Points') : `${fmtPoints(value)} pts`}
        </span>
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <div className="w-full p-1.5 sm:w-56">
          <div className="grid grid-cols-4 gap-1">
            {FIB.map((n) => (
              <button
                key={n}
                onClick={() => {
                  onChange(n)
                  p.close()
                }}
                className={clsx(
                  'h-9 rounded-lg text-[13px] font-medium hover:bg-hover',
                  value === n ? 'bg-accent-soft text-accent' : 'bg-sunken',
                )}
              >
                {n}
              </button>
            ))}
          </div>
          <form
            className="mt-1.5 flex gap-1"
            onSubmit={(e) => {
              e.preventDefault()
              const n = parseFloat(custom)
              if (!isNaN(n)) {
                onChange(n)
                setCustom('')
                p.close()
              }
            }}
          >
            <input
              inputMode="decimal"
              value={custom}
              onChange={(e) => setCustom(e.target.value)}
              placeholder="Other…"
              className="h-8 w-full rounded-lg border border-line bg-panel px-2 text-[13px]"
            />
            {value !== null && (
              <button
                type="button"
                onClick={() => {
                  onChange(null)
                  p.close()
                }}
                className="h-8 rounded-lg px-2 text-[13px] text-muted hover:bg-hover"
              >
                Clear
              </button>
            )}
          </form>
          {estimateKey && (
            <EstimatePanel
              issueKey={estimateKey}
              onApply={(n) => {
                onChange(n)
                p.close()
              }}
            />
          )}
        </div>
      </Popover>
    </>
  )
}
