import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  Compass,
  FolderTree,
  Globe,
  ListChecks,
  Monitor,
  Puzzle,
  Scale,
  StickyNote,
  Target,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { dateTime, relativeTime } from '@/lib/format'
import { canWrite, projectChain, scopeLabel, type DiffLine } from '@/lib/knowledge'
import type { DesignKind, KScope, Project, UserRef } from '@/lib/types'
import { Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { Avatar } from '@/components/ui/misc'
import { ProjectIcon } from '@/components/issue/icons'
import { PropButton } from '@/components/issue/pickers'

/** "WAYP" or "global": where an entry or note lives. */
export function ScopeChip({ scope, className }: { scope: KScope; className?: string }) {
  const global = !scope.project_id
  return (
    <span
      title={global ? 'Global: applies to every project' : `From ${scope.project_name ?? ''}`}
      className={clsx(
        'inline-flex h-[18px] shrink-0 items-center gap-1 rounded px-1 text-[10px] font-medium uppercase tracking-wide',
        global ? 'bg-sunken text-muted' : 'bg-accent-soft text-fg',
        className,
      )}
    >
      {global && <Globe className="size-2.5" />}
      {scopeLabel(scope)}
    </span>
  )
}

/** An agent's short name with a robot, like an assignee. */
export function AgentName({ agent, className }: { agent: string; className?: string }) {
  return (
    <span className={clsx('inline-flex min-w-0 items-center gap-1', className)} title={agent}>
      <Bot className="size-3.5 shrink-0 text-accent" />
      <span className="truncate">{agentLabel(agent)}</span>
    </span>
  )
}

/** Who did something: the agent if there was one (on the person's behalf), else the person. */
export function Who({ user, agent }: { user?: UserRef | null; agent?: string }) {
  if (agent)
    return (
      <span
        className="inline-flex items-center gap-1 align-bottom"
        title={user ? `${agent}, for ${user.display_name}` : agent}
      >
        <Bot className="size-3.5 shrink-0 text-accent" />
        <b className="font-medium text-fg">{agentLabel(agent)}</b>
      </span>
    )
  return <b className="font-medium text-fg">{user?.display_name ?? 'Waypoint'}</b>
}

/** A person's avatar, or a bot mark for an agent; the name is its tooltip. */
export function WhoAvatar({
  user,
  agent,
  size = 18,
}: {
  user?: UserRef | null
  agent?: string
  size?: number
}) {
  if (agent)
    return (
      <span
        title={user ? `${agentLabel(agent)}, for ${user.display_name}` : agentLabel(agent)}
        style={{ width: size, height: size }}
        className="inline-flex shrink-0 items-center justify-center rounded-full bg-accent-soft text-accent"
      >
        <Bot style={{ width: size * 0.65, height: size * 0.65 }} />
      </span>
    )
  return <Avatar user={user} size={size} />
}

/** "Written by Robin, Oct 12 · changed by agent claude 2 hours ago" */
export function Provenance({
  created,
  updated,
}: {
  created: { user?: UserRef; agent?: string; at: string; verb?: string }
  updated?: { user?: UserRef; agent?: string; at: string }
}) {
  const changed =
    updated && Math.abs(Date.parse(updated.at) - Date.parse(created.at)) > 1000 ? updated : null
  return (
    <div className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-xs text-subtle">
      <span>
        {created.verb ?? 'Written'} by <Who user={created.user} agent={created.agent} />,{' '}
        <time title={dateTime(created.at)}>{relativeTime(created.at)}</time>
      </span>
      {changed && (
        <span>
          · last changed by <Who user={changed.user} agent={changed.agent} />{' '}
          <time title={dateTime(changed.at)}>{relativeTime(changed.at)}</time>
        </span>
      )}
    </div>
  )
}

/** A big title that saves on Enter or blur; Esc puts it back. */
export function InlineTitle({
  value,
  editable,
  onSave,
  label,
  className,
}: {
  value: string
  editable: boolean
  onSave: (v: string) => void
  label: string
  className?: string
}) {
  const [draft, setDraft] = useState(value)
  const ref = useRef<HTMLTextAreaElement>(null)
  useEffect(() => setDraft(value), [value])
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    el.style.height = 'auto'
    el.style.height = `${el.scrollHeight}px`
  }, [draft])
  const commit = () => {
    const t = draft.trim()
    if (t && t !== value) onSave(t)
    else setDraft(value)
  }
  return (
    <textarea
      ref={ref}
      rows={1}
      value={draft}
      readOnly={!editable}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') {
          e.preventDefault()
          ref.current?.blur()
        }
        if (e.key === 'Escape') {
          setDraft(value)
          requestAnimationFrame(() => ref.current?.blur())
        }
      }}
      aria-label={label}
      className={clsx(
        'block w-full resize-none overflow-hidden bg-transparent text-2xl font-semibold tracking-tight outline-none',
        className,
      )}
    />
  )
}

/**
 * Where a new or moved item can live: a project chain (a project and its
 * parents) when `project` is given, else every project the person can write
 * to; global for admins.
 */
export function useScopeOptions(project?: string) {
  const projects = useQuery(q.projects()).data ?? []
  const admin = !!useQuery(q.me()).data?.is_admin
  return useMemo(() => {
    const list: Project[] = (project ? projectChain(projects, project) : projects).filter(canWrite)
    const items: PickItem<string>[] = list.map((p, i) => ({
      value: p.key,
      label: p.name,
      hint: project ? (i === 0 ? 'this project' : 'parent') : p.key,
      icon: <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={14} />,
    }))
    if (admin)
      items.push({
        value: 'global',
        label: 'Global',
        hint: 'every project',
        icon: <Globe className="size-3.5 text-subtle" />,
      })
    return items
  }, [projects, project, admin])
}

/** A button showing an item's scope that opens a picker of scopes. */
export function ScopePicker({
  value,
  onChange,
  project,
  disabled,
  label = 'Move to…',
}: {
  /** a project key, or "global" */
  value: string
  onChange: (v: string) => void
  project?: string
  disabled?: boolean
  label?: string
}) {
  const pop = usePopover()
  const items = useScopeOptions(project)
  const cur = items.find((i) => i.value === value)
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Scope">
        {value === 'global' ? (
          <Globe className="size-3.5 text-subtle" />
        ) : (
          <FolderTree className="size-3.5 text-subtle" />
        )}
        <span className="truncate text-muted">
          {value === 'global' ? 'Global' : (cur?.label ?? value)}
        </span>
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={items}
          selected={[value]}
          placeholder={label}
          onClose={pop.close}
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

/** A section heading in the style of the issue page's blocks. */
export function BlockTitle({ children, actions }: { children: ReactNode; actions?: ReactNode }) {
  return (
    <div className="mb-1.5 flex min-h-6 items-center justify-between gap-2">
      <h3 className="text-xs font-semibold uppercase tracking-wide text-subtle">{children}</h3>
      {actions}
    </div>
  )
}

/** A line diff: removed lines red, added green, unchanged runs folded to a little context. */
export function DiffView({ lines, context = 2 }: { lines: DiffLine[]; context?: number }) {
  const keep = lines.map((_l, i) =>
    lines.slice(Math.max(0, i - context), i + context + 1).some((x) => x.op !== ' '),
  )
  if (!lines.some((l) => l.op !== ' '))
    return <p className="text-xs text-subtle">No change to the text.</p>
  const out: ReactNode[] = []
  let skipped = 0
  lines.forEach((l, i) => {
    if (!keep[i]) {
      skipped++
      return
    }
    if (skipped)
      out.push(
        <div key={`s${i}`} className="px-2 text-subtle">
          ⋯ {skipped} unchanged line{skipped === 1 ? '' : 's'}
        </div>,
      )
    skipped = 0
    out.push(
      <div
        key={i}
        className={clsx(
          'whitespace-pre-wrap px-2',
          l.op === '+' && 'bg-ok/10 text-fg',
          l.op === '-' && 'bg-danger/10 text-muted line-through decoration-danger/40',
        )}
      >
        <span aria-hidden className="mr-2 inline-block w-2 select-none text-subtle">
          {l.op === ' ' ? '' : l.op}
        </span>
        <span className="sr-only">
          {l.op === '+' ? 'added: ' : l.op === '-' ? 'removed: ' : ''}
        </span>
        {l.text || ' '}
      </div>,
    )
  })
  if (skipped)
    out.push(
      <div key="end" className="px-2 text-subtle">
        ⋯ {skipped} unchanged line{skipped === 1 ? '' : 's'}
      </div>,
    )
  return (
    <div className="overflow-x-auto rounded-md border border-line bg-panel py-1 font-mono text-[12px] leading-relaxed">
      {out}
    </div>
  )
}

const KIND_ICON: Record<DesignKind, typeof Target> = {
  goal: Target,
  principle: Compass,
  feature: Puzzle,
  screen: Monitor,
  decision: Scale,
  requirement: ListChecks,
  note: StickyNote,
}

/** A design note's kind as an icon. */
export function KindIcon({ kind, className }: { kind: string; className?: string }) {
  const Icon = KIND_ICON[kind as DesignKind] ?? StickyNote
  return <Icon className={clsx('size-3.5 shrink-0 text-subtle', className)} aria-hidden />
}
