import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ChevronLeft,
  CircleOff,
  LayoutList,
  Plus,
  Search,
  Sparkles,
  ToggleRight,
  Wand2,
} from 'lucide-react'
import type { Assistant } from '@/lib/assistant'
import { plural } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Input, Switch } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toastError } from '@/components/ui/toast'
import { LeaveGuard } from './memory/editor'
import { NewSkill } from './skills/NewSkill'
import { SkillPage } from './skills/SkillPage'
import {
  categoryLabel,
  PROVENANCE,
  skillsQuery,
  useSkillMutations,
  type Skill,
} from './skills/data'
import type { AssistantSearch } from './search'

type Filter = 'all' | 'on' | 'off' | 'agent'

// Where the list was scrolled, to come back to it from a skill.
let listScroll = 0

export function SkillsTab({ assistant }: { assistant: Assistant }) {
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const top = useRef<HTMLDivElement>(null)
  const open = search.path
  const creating = search.view === 'new'
  const go = (s: Pick<AssistantSearch, 'path' | 'view'>) => {
    if (!open && !creating) listScroll = scroller(top.current)?.scrollTop ?? 0
    navigate({ search: (cur: AssistantSearch) => ({ tab: cur.tab, ...s }) })
  }
  useLayoutEffect(() => {
    scroller(top.current)?.scrollTo({ top: open || creating ? 0 : listScroll })
  }, [open, creating])
  const back = (
    <Link
      to="/assistant/$id"
      params={{ id: String(assistant.id) }}
      search={{ tab: 'skills' }}
      className="-ml-1 inline-flex h-8 items-center gap-1 rounded-md px-1 text-[13px] text-muted hover:text-fg"
    >
      <ChevronLeft className="size-4" />
      Skills
    </Link>
  )
  return (
    <div ref={top}>
      <LeaveGuard />
      {creating ? (
        <NewSkill
          assistant={assistant}
          back={back}
          onCancel={() => go({})}
          onCreated={(name) => go({ path: name })}
        />
      ) : open ? (
        <SkillPage
          key={open}
          assistant={assistant}
          name={open}
          back={back}
          onArchived={() => go({})}
        />
      ) : (
        <SkillList assistant={assistant} onNew={() => go({ view: 'new' })} />
      )}
    </div>
  )
}

function scroller(el: HTMLElement | null): HTMLElement | null {
  for (let p = el?.parentElement; p; p = p.parentElement)
    if (/(auto|scroll)/.test(getComputedStyle(p).overflowY)) return p
  return null
}

function SkillList({ assistant, onNew }: { assistant: Assistant; onNew: () => void }) {
  const id = assistant.id
  const skills = useQuery(skillsQuery(id))
  const m = useSkillMutations(id)
  const [q, setQ] = useState('')
  const [filter, setFilter] = useState<Filter>('all')
  const all = skills.data ?? []
  const agent = all.filter((s) => s.provenance === 'agent').length
  const off = all.filter((s) => !s.enabled).length
  useEffect(() => {
    if (filter === 'agent' && skills.data && !agent) setFilter('all')
  }, [filter, agent, skills.data])

  const groups = useMemo(() => {
    const words = q.toLowerCase().split(/\s+/).filter(Boolean)
    const shown = all.filter(
      (s) =>
        (filter === 'all' ||
          (filter === 'on' && s.enabled) ||
          (filter === 'off' && !s.enabled) ||
          (filter === 'agent' && s.provenance === 'agent')) &&
        words.every((w) =>
          `${s.name} ${s.description} ${s.category ?? ''}`.toLowerCase().includes(w),
        ),
    )
    const by = new Map<string, Skill[]>()
    for (const s of shown) {
      const k = s.category ?? ''
      by.set(k, [...(by.get(k) ?? []), s])
    }
    return [...by.entries()]
      .sort(([a], [b]) => (a ? (b ? a.localeCompare(b) : -1) : 1))
      .map(([c, list]) => ({
        category: c,
        skills: list.sort(
          (a, b) =>
            Number(b.provenance === 'agent') - Number(a.provenance === 'agent') ||
            a.name.localeCompare(b.name),
        ),
      }))
  }, [all, q, filter])

  const toggle = (s: Skill, enabled: boolean) =>
    m.toggle.mutate({ name: s.name, enabled }, { onError: toastError })

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-2">
        <div className="relative min-w-0 flex-1 basis-56">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-subtle" />
          <Input
            type="search"
            aria-label="Search skills"
            placeholder="Search skills"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            onKeyDown={(e) => e.key === 'Escape' && q && (e.stopPropagation(), setQ(''))}
            className="w-full pl-8"
          />
        </div>
        <SegmentedControl
          label="Show"
          value={filter}
          onChange={setFilter}
          options={[
            { value: 'all', label: 'All', icon: <LayoutList className="size-3.5" /> },
            { value: 'on', label: 'On', icon: <ToggleRight className="size-3.5" /> },
            {
              value: 'off',
              label: `Off${off ? ` (${off})` : ''}`,
              icon: <CircleOff className="size-3.5" />,
            },
            ...(agent
              ? [
                  {
                    value: 'agent' as const,
                    label: `Agent-written (${agent})`,
                    icon: <Sparkles className="size-3.5" />,
                  },
                ]
              : []),
          ]}
        />
        <Button variant="primary" icon={<Plus className="size-4" />} onClick={onNew}>
          New skill
        </Button>
      </div>

      {skills.isPending ? (
        <div className="space-y-4">
          {[0, 1].map((i) => (
            <div key={i} className="space-y-3 rounded-xl border border-line bg-panel p-4">
              <Skeleton className="h-4 w-32" />
              <Skeleton className="h-10 w-full" />
              <Skeleton className="h-10 w-full" />
            </div>
          ))}
        </div>
      ) : skills.isError ? (
        <Empty icon={<Wand2 />} title="Couldn't load skills">
          {skills.error.message}
        </Empty>
      ) : all.length === 0 ? (
        <Empty
          icon={<Wand2 />}
          title="No skills yet"
          action={
            <Button variant="primary" icon={<Plus className="size-4" />} onClick={onNew}>
              New skill
            </Button>
          }
        >
          {assistant.name} writes its own as it learns, or you can write one.
        </Empty>
      ) : groups.length === 0 ? (
        <Empty icon={<Search />} title="No skills match">
          <button
            className="text-accent hover:underline"
            onClick={() => (setQ(''), setFilter('all'))}
          >
            Show all {plural(all.length, 'skill')}
          </button>
        </Empty>
      ) : (
        groups.map((g) => (
          <Section
            key={g.category}
            title={
              <span className="flex items-center gap-2">
                {categoryLabel(g.category)}
                <span className="font-normal text-subtle">{g.skills.length}</span>
              </span>
            }
          >
            <ul className="divide-y divide-line">
              {g.skills.map((s) => (
                <SkillRow key={s.name} assistant={assistant} skill={s} onToggle={toggle} />
              ))}
            </ul>
          </Section>
        ))
      )}
    </div>
  )
}

function SkillRow({
  assistant,
  skill: s,
  onToggle,
}: {
  assistant: Assistant
  skill: Skill
  onToggle: (s: Skill, enabled: boolean) => void
}) {
  const p = PROVENANCE[s.provenance]
  return (
    <li
      className={clsx(
        'flex items-center gap-1 pr-2 transition-colors hover:bg-hover',
        s.provenance === 'agent' && 'border-l-2 border-l-accent',
      )}
    >
      <Link
        to="/assistant/$id"
        params={{ id: String(assistant.id) }}
        search={{ tab: 'skills', path: s.name }}
        className={clsx(
          'flex min-w-0 flex-1 flex-col gap-0.5 py-2.5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40',
          s.provenance === 'agent' ? 'pl-[14px]' : 'pl-4',
        )}
      >
        <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
          <span
            className={clsx(
              'truncate text-[13px] font-medium',
              !s.enabled && 'text-muted line-through decoration-subtle/60',
            )}
          >
            {s.name}
          </span>
          {s.provenance !== 'bundled' && (
            <span title={p.hint} className="shrink-0">
              <Badge color={p.color}>
                {s.provenance === 'agent' && <Sparkles className="size-3" />}
                {p.label}
              </Badge>
            </span>
          )}
          {s.usage > 0 && (
            <span className="shrink-0 text-[11px] tabular-nums text-subtle" title="Times used">
              used {s.usage}×
            </span>
          )}
        </span>
        {s.description && (
          <span className={clsx('line-clamp-2 text-xs', s.enabled ? 'text-muted' : 'text-subtle')}>
            {s.description}
          </span>
        )}
      </Link>
      <label className="flex size-11 shrink-0 cursor-pointer items-center justify-center">
        <Switch
          label={`Let ${assistant.name} use ${s.name}`}
          checked={s.enabled}
          onChange={(v) => onToggle(s, v)}
        />
      </label>
    </li>
  )
}
