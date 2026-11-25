import { useEffect, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Activity,
  BarChart3,
  Brain,
  CalendarClock,
  FolderOpen,
  MessagesSquare,
  Plug,
  RefreshCw,
  Settings2,
  ShieldAlert,
  Snowflake,
  Sparkles,
  Wand2,
} from 'lucide-react'
import {
  assistantQuery,
  assistantsQuery,
  health,
  HEALTH_COLOR,
  HEALTH_LABEL,
  hermesQuery,
  useAssistantMutations,
  type Assistant,
} from '@/lib/assistant'
import { useAssistantLive } from '@/lib/assistant-live'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Empty, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { ChatTab } from './Chat'
import { ActivityTab } from './Activity'
import { SchedulesTab } from './Schedules'
import { SkillsTab } from './Skills'
import { MemoryTab } from './Memory'
import { ToolsTab } from './Tools'
import { FilesTab } from './Files'
import { UsageTab } from './Usage'
import { SettingsTab } from './Settings'
import type { AssistantSearch, AssistantTab } from './search'

const TABS: {
  id: AssistantTab
  label: string
  icon: ReactNode
  Component: (p: { assistant: Assistant }) => ReactNode
  /** fills the height itself (no page scroll, no width cap) */
  full?: boolean
}[] = [
  { id: 'chat', label: 'Chat', icon: <MessagesSquare />, Component: ChatTab, full: true },
  { id: 'activity', label: 'Activity', icon: <Activity />, Component: ActivityTab },
  { id: 'schedules', label: 'Schedules', icon: <CalendarClock />, Component: SchedulesTab },
  { id: 'skills', label: 'Skills', icon: <Wand2 />, Component: SkillsTab },
  { id: 'memory', label: 'Memory', icon: <Brain />, Component: MemoryTab },
  { id: 'tools', label: 'Tools', icon: <Plug />, Component: ToolsTab },
  { id: 'files', label: 'Files', icon: <FolderOpen />, Component: FilesTab, full: true },
  { id: 'usage', label: 'Usage', icon: <BarChart3 />, Component: UsageTab },
  { id: 'settings', label: 'Settings', icon: <Settings2 />, Component: SettingsTab },
]

/** /assistant: the first of your assistants, or how to connect one. */
export function AssistantsIndexPage() {
  const list = useQuery(assistantsQuery())
  const me = useQuery(q.me()).data
  const navigate = useNavigate()
  const mine = list.data?.filter((a) => a.mine) ?? []
  useEffect(() => {
    if (mine.length)
      navigate({ to: '/assistant/$id', params: { id: String(mine[0].id) }, replace: true })
  }, [mine, navigate])
  if (list.isPending || mine.length) return <Skeleton className="m-6 h-64" />
  return (
    <div className="flex-1 overflow-y-auto">
      <Empty
        icon={<Sparkles />}
        title="No assistant yet"
        action={
          me?.is_admin ? (
            <Link
              to="/admin"
              search={{ tab: 'assistants' }}
              className="text-[13px] font-medium text-accent hover:underline"
            >
              Connect one
            </Link>
          ) : undefined
        }
      >
        {me?.is_admin
          ? 'Connect a Hermes agent through its gate, and chat with it, schedule it and watch it from here.'
          : 'An admin can connect your Hermes agent.'}
      </Empty>
    </div>
  )
}

export function AssistantPage() {
  const { id: raw } = useParams({ from: '/app/assistant/$id' })
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const id = Number(raw)
  const a = useQuery({ ...assistantQuery(id), refetchInterval: 30_000 })
  const tab = TABS.find((t) => t.id === search.tab) ?? TABS[0]
  useEffect(() => {
    if (a.data) document.title = `${a.data.name} · ${tab.label} · Waypoint`
  }, [a.data, tab.label])
  if (a.isError)
    return (
      <div className="flex-1 overflow-y-auto">
        <Empty icon={<ShieldAlert />} title="Not your assistant">
          It doesn't exist, or it belongs to someone else.
        </Empty>
      </div>
    )
  if (!a.data) return <Skeleton className="m-6 h-64" />
  const assistant = a.data
  if (!assistant.mine)
    return (
      <div className="flex-1 overflow-y-auto">
        <Empty
          icon={<ShieldAlert />}
          title={`${assistant.name} is ${assistant.owner.display_name}'s`}
        >
          Only its owner can use it. You can change its connection under Admin → Assistants.
        </Empty>
      </div>
    )
  const go = (t: AssistantTab) =>
    navigate({
      search: (s: AssistantSearch) => ({
        tab: t === 'chat' ? undefined : t,
        s: t === 'chat' ? s.s : undefined,
      }),
    })
  const Tab = tab.Component
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <Header assistant={assistant} />
      <nav
        role="tablist"
        aria-label={`${assistant.name} sections`}
        className="no-scrollbar flex shrink-0 gap-1 overflow-x-auto border-b border-line px-2 sm:px-4"
      >
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            aria-selected={t.id === tab.id}
            onClick={() => go(t.id)}
            className={clsx(
              '-mb-px flex h-9 shrink-0 items-center gap-1.5 border-b-2 px-2.5 text-[13px] [&_svg]:size-3.5',
              t.id === tab.id
                ? 'border-accent font-medium text-fg'
                : 'border-transparent text-muted hover:text-fg',
            )}
          >
            {t.icon}
            {t.label}
          </button>
        ))}
      </nav>
      <Banners assistant={assistant} />
      {tab.full ? (
        <div role="tabpanel" className="flex min-h-0 flex-1 flex-col">
          <Tab key={tab.id} assistant={assistant} />
        </div>
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto pb-16">
          <div role="tabpanel" className="mx-auto w-full max-w-5xl px-4 pt-4 sm:px-6">
            <Tab key={tab.id} assistant={assistant} />
          </div>
        </div>
      )}
    </div>
  )
}

interface HermesStatus {
  version?: string
  gateway_state?: string | null
  active_sessions?: number
  overall?: string
}

function Header({ assistant }: { assistant: Assistant }) {
  const h = health(assistant)
  const live = useAssistantLive(assistant.id)
  const m = useAssistantMutations(assistant.id)
  const [freezing, setFreezing] = useState(false)
  const status = useQuery({
    ...hermesQuery<HermesStatus>(assistant.id, 'status', undefined, 10_000),
    refetchInterval: 60_000,
    enabled: assistant.state.reachable && !assistant.state.frozen,
    retry: false,
  }).data
  const working = status?.active_sessions ?? 0
  return (
    <header className="flex shrink-0 items-center justify-between gap-3 px-4 pb-2 pt-3 sm:px-6 sm:pt-4">
      <div className="flex min-w-0 items-center gap-3">
        <span className="relative flex size-9 shrink-0 items-center justify-center rounded-xl bg-accent-soft text-accent">
          <Sparkles className="size-5" />
          <span
            className={clsx(
              'absolute -bottom-0.5 -right-0.5 size-3 rounded-full border-2 border-bg',
              h === 'ok' && working > 0 && 'animate-pulse',
            )}
            style={{ background: HEALTH_COLOR[h] }}
            aria-hidden
          />
        </span>
        <div className="min-w-0">
          <h1 className="truncate text-lg font-semibold tracking-tight">{assistant.name}</h1>
          <div className="flex items-center gap-x-2 truncate text-[13px] text-muted">
            <span style={{ color: h === 'ok' ? undefined : HEALTH_COLOR[h] }}>
              {h === 'ok' && working > 0 ? `Working · ${working} active` : HEALTH_LABEL[h]}
            </span>
            {status?.version && (
              <span className="hidden text-subtle sm:inline">Hermes {status.version}</span>
            )}
            {live.state !== 'open' && h === 'ok' && (
              <span className="text-subtle">
                {live.state === 'connecting' ? 'chat connecting…' : 'chat offline'}
              </span>
            )}
          </div>
        </div>
      </div>
      <div className="flex shrink-0 items-center gap-1.5">
        <IconButton
          label="Check now"
          icon={<RefreshCw className={clsx('size-4', m.check.isPending && 'animate-spin')} />}
          onClick={() => m.check.mutate(undefined, { onError: toastError })}
        />
        <Button
          size="sm"
          variant={assistant.state.frozen ? 'subtle' : 'secondary'}
          icon={<Snowflake className="size-3.5" />}
          disabled={assistant.state.frozen || !assistant.state.reachable}
          onClick={() => setFreezing(true)}
          className={clsx(!assistant.state.frozen && 'text-danger')}
        >
          {assistant.state.frozen ? 'Frozen' : 'Freeze'}
        </Button>
      </div>
      <Confirm
        open={freezing}
        onClose={() => setFreezing(false)}
        danger
        title={`Freeze ${assistant.name}?`}
        confirmLabel="Freeze now"
        body={
          <>
            The gate cuts it off at once: no model, no tools, no messages. Only you, on Signal, can
            unfreeze it.
          </>
        }
        onConfirm={() =>
          m.freeze.mutate(undefined, {
            onSuccess: () => toast(`${assistant.name} is frozen`, { kind: 'success' }),
            onError: toastError,
          })
        }
      />
    </header>
  )
}

function Banners({ assistant }: { assistant: Assistant }) {
  const s = assistant.state
  if (!s.reachable && s.checked_at && !s.checked_at.startsWith('0001'))
    return (
      <div className="flex shrink-0 items-center gap-2 border-b border-danger/30 bg-danger/10 px-4 py-2 text-[13px] text-danger sm:px-6">
        <ShieldAlert className="size-4 shrink-0" />
        <span className="min-w-0 truncate">
          <b className="font-semibold">Not answering</b>
          {s.down_since && ` since ${relativeTime(s.down_since)}`}
          {s.error && <span className="text-danger/80"> · {s.error}</span>}
        </span>
      </div>
    )
  if (s.frozen)
    return (
      <div className="flex shrink-0 items-center gap-2 border-b border-danger/30 bg-danger/10 px-4 py-2 text-[13px] text-danger sm:px-6">
        <Snowflake className="size-4 shrink-0" />
        <span>
          <b className="font-semibold">{assistant.name} is frozen.</b> Reply “unfreeze” on Signal to
          let it run again.
        </span>
      </div>
    )
  if (!s.signal && s.reachable)
    return (
      <div className="flex shrink-0 items-center gap-2 border-b border-warn/30 bg-warn/10 px-4 py-2 text-[13px] sm:px-6">
        <ShieldAlert className="size-4 shrink-0 text-warn" />
        <span>
          <b className="font-semibold">Signal isn't connected at the gate.</b> Approvals can't reach
          you, so anything that needs one waits.
        </span>
      </div>
    )
  if (s.pending.length)
    return (
      <Link
        to="/assistant/$id"
        params={{ id: String(assistant.id) }}
        search={{ tab: 'activity', view: 'approvals' }}
        className="flex shrink-0 items-center gap-2 border-b border-warn/30 bg-warn/10 px-4 py-2 text-[13px] hover:bg-warn/15 sm:px-6"
      >
        <span className="rounded bg-warn/20 px-1.5 font-mono text-xs font-semibold text-warn">
          {s.pending[0].code}
        </span>
        <span className="min-w-0 flex-1 truncate">
          {s.pending.length === 1
            ? s.pending[0].summary
            : `${s.pending.length} actions are waiting for you`}
        </span>
        <span className="shrink-0 text-muted">Approve on Signal</span>
      </Link>
    )
  return null
}
