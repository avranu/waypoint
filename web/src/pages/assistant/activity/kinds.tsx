import type { ReactNode } from 'react'
import {
  Ban,
  CalendarClock,
  CheckCircle2,
  CircleAlert,
  CircleCheckBig,
  Clock,
  Compass,
  Hand,
  KeyRound,
  MessageCircle,
  MessagesSquare,
  Plug,
  ShieldCheck,
  Snowflake,
  Sun,
  Wrench,
  XCircle,
} from 'lucide-react'
import type { AuditEntry } from '@/lib/assistant'

export type Tone = 'ok' | 'warn' | 'danger' | 'accent' | 'muted'

export const TONE_CLASS: Record<Tone, string> = {
  ok: 'bg-ok/10 text-ok',
  warn: 'bg-warn/10 text-warn',
  danger: 'bg-danger/10 text-danger',
  accent: 'bg-accent-soft text-accent',
  muted: 'bg-sunken text-muted',
}

/** The timeline's filter chips. `waypoint` is hidden unless asked for. */
export type Group =
  'conversations' | 'schedules' | 'tools' | 'approvals' | 'gate' | 'signal' | 'waypoint'

export const GROUPS: { id: Group; label: string }[] = [
  { id: 'conversations', label: 'Conversations' },
  { id: 'schedules', label: 'Schedules' },
  { id: 'tools', label: 'Tools' },
  { id: 'approvals', label: 'Approvals' },
  { id: 'gate', label: 'Gate' },
  { id: 'signal', label: 'Signal' },
]

export interface Described {
  icon: ReactNode
  tone: Tone
  group: Group
  text: ReactNode
  /** A second, quieter line. */
  sub?: string
}

const str = (v: unknown): string =>
  v === null || v === undefined ? '' : typeof v === 'string' ? v : JSON.stringify(v)

const Code = ({ children }: { children: ReactNode }) => (
  <span className="rounded bg-sunken px-1 font-mono text-[12px]">{children}</span>
)
const B = ({ children }: { children: ReactNode }) => <span className="font-medium">{children}</span>

/** Arguments as one short line: key=value, key=value. */
export function argsLine(args: unknown, max = 160): string {
  if (!args || typeof args !== 'object') return str(args).slice(0, max)
  const s = Object.entries(args as Record<string, unknown>)
    .map(([k, v]) => `${k}=${typeof v === 'string' ? v : JSON.stringify(v)}`)
    .join(', ')
  return s.length > max ? s.slice(0, max - 1) + '…' : s
}

const who = (by: unknown) => {
  const b = str(by)
  if (!b) return ''
  if (b === 'signal') return ' on Signal'
  if (b === 'waypoint') return ' from Waypoint'
  if (b === 'gatectl') return ' with gatectl'
  return ` by ${b}`
}

export function describe(e: AuditEntry): Described {
  const f = e as Record<string, unknown>
  const code = str(f.code)
  switch (e.kind) {
    case 'tool':
      return {
        icon: <Wrench />,
        tone: 'muted',
        group: 'tools',
        text: (
          <>
            Used <Code>{str(f.tool)}</Code>
          </>
        ),
        sub: argsLine(f.args) || undefined,
      }
    case 'conn_tool':
      return {
        icon: <Plug />,
        tone: 'muted',
        group: 'tools',
        text: (
          <>
            Called <Code>{str(f.tool)}</Code> on <B>{str(f.conn)}</B>
          </>
        ),
        sub: argsLine(f.args) || undefined,
      }
    case 'conn': {
      const status = Number(f.status)
      const what = f.method
        ? `${str(f.method)} ${str(f.path)}`
        : Array.isArray(f.methods)
          ? (f.methods as unknown[]).map(str).join(', ')
          : ''
      return {
        icon: <Plug />,
        tone: status >= 400 ? 'warn' : 'muted',
        group: 'tools',
        text: (
          <>
            <B>{str(f.conn)}</B> answered {what && <Code>{what}</Code>}
          </>
        ),
        sub: f.status !== undefined ? `HTTP ${str(f.status)}` : undefined,
      }
    }
    case 'conn_refused':
      return {
        icon: <Ban />,
        tone: 'danger',
        group: 'tools',
        text: (
          <>
            Refused a call to <B>{str(f.conn)}</B>
          </>
        ),
        sub: str(f.reason) || undefined,
      }
    case 'conn_failed':
      return {
        icon: <CircleAlert />,
        tone: 'danger',
        group: 'tools',
        text: (
          <>
            A call to <B>{str(f.conn)}</B> failed
          </>
        ),
        sub: str(f.error) || undefined,
      }
    case 'waypoint':
      return {
        icon: <Compass />,
        tone: Number(f.status) >= 400 ? 'warn' : 'muted',
        group: 'tools',
        text: <>Used Waypoint</>,
        sub: [
          Array.isArray(f.calls) ? (f.calls as unknown[]).map(str).join(', ') : str(f.calls),
          f.status !== undefined ? `HTTP ${str(f.status)}` : '',
        ]
          .filter(Boolean)
          .join(' · '),
      }
    case 'waypoint_failed':
      return {
        icon: <Compass />,
        tone: 'danger',
        group: 'tools',
        text: <>A call to Waypoint failed</>,
        sub: str(f.error) || undefined,
      }
    case 'approval_requested':
      return {
        icon: <Hand />,
        tone: 'warn',
        group: 'approvals',
        text: (
          <>
            Asked to {str(f.summary) || <Code>{str(f.tool)}</Code>} <Code>{code}</Code>
          </>
        ),
        sub: str(f.reason) || (f.tool ? str(f.tool) : undefined),
      }
    case 'approval_granted':
      return {
        icon: <CheckCircle2 />,
        tone: 'ok',
        group: 'approvals',
        text: (
          <>
            Approved <Code>{code}</Code>
            {who(f.by)}
          </>
        ),
      }
    case 'approval_denied':
      return {
        icon: <XCircle />,
        tone: 'danger',
        group: 'approvals',
        text: (
          <>
            Denied <Code>{code}</Code>
            {who(f.by)}
          </>
        ),
      }
    case 'approval_expired':
      return {
        icon: <Clock />,
        tone: 'muted',
        group: 'approvals',
        text: (
          <>
            <Code>{code}</Code> expired unanswered
          </>
        ),
      }
    case 'approved_action_done':
      return {
        icon: <CircleCheckBig />,
        tone: 'ok',
        group: 'approvals',
        text: (
          <>
            Did approved action <Code>{code}</Code>
          </>
        ),
        sub: str(f.result).slice(0, 200) || undefined,
      }
    case 'approved_action_failed':
      return {
        icon: <CircleAlert />,
        tone: 'danger',
        group: 'approvals',
        text: (
          <>
            Approved action <Code>{code}</Code> failed
          </>
        ),
        sub: str(f.error) || undefined,
      }
    case 'freeze':
      return {
        icon: <Snowflake />,
        tone: 'danger',
        group: 'gate',
        text: <>Frozen{who(f.by)}</>,
      }
    case 'unfreeze':
      return {
        icon: <Sun />,
        tone: 'ok',
        group: 'gate',
        text: <>Unfrozen{who(f.by)}</>,
      }
    case 'connection_added':
    case 'connection_changed':
    case 'connection_updated':
    case 'connection_removed': {
      const verb = {
        connection_added: 'added',
        connection_changed: 'changed',
        connection_updated: 'given new credentials',
        connection_removed: 'removed',
      }[e.kind]
      return {
        icon: e.kind === 'connection_updated' ? <KeyRound /> : <Plug />,
        tone: e.kind === 'connection_removed' ? 'warn' : 'accent',
        group: 'gate',
        text: (
          <>
            Connection <B>{str(f.conn)}</B> {verb}
            {who(f.by)}
          </>
        ),
        sub: f.url ? str(f.url) : undefined,
      }
    }
    case 'control':
      return {
        icon: <ShieldCheck />,
        tone: 'muted',
        group: 'waypoint',
        text: (
          <>
            Waypoint called the gate <Code>{`${str(f.method)} ${str(f.path)}`}</Code>
          </>
        ),
      }
    case 'control_refused':
      return {
        icon: <Ban />,
        tone: 'danger',
        group: 'gate',
        text: (
          <>
            Refused a gate call <Code>{`${str(f.method)} ${str(f.path)}`}</Code>
          </>
        ),
      }
    case 'hermes_api':
      return {
        icon: <Compass />,
        tone: f.allowed === false ? 'danger' : 'muted',
        group: 'waypoint',
        text: (
          <>
            {f.allowed === false ? 'Refused Waypoint' : 'Waypoint'}{' '}
            <Code>{`${str(f.method)} ${str(f.uri)}`}</Code>
          </>
        ),
      }
    case 'signal_send':
      return {
        icon: <MessageCircle />,
        tone: 'muted',
        group: 'signal',
        text: <>Sent a Signal message</>,
        sub: f.chars !== undefined ? `${str(f.chars)} characters` : undefined,
      }
    case 'signal_control':
      return {
        icon: <MessageCircle />,
        tone: 'accent',
        group: 'signal',
        text: (
          <>
            You sent the gate <Code>{str(f.text)}</Code>
          </>
        ),
      }
    case 'signal_dropped_stranger':
      return {
        icon: <Ban />,
        tone: 'warn',
        group: 'signal',
        text: <>Ignored a Signal message from a stranger</>,
        sub: str(f.source) || undefined,
      }
    case 'signal_rpc_refused':
      return {
        icon: <Ban />,
        tone: 'warn',
        group: 'signal',
        text: (
          <>
            Refused a Signal call <Code>{str(f.method)}</Code>
          </>
        ),
        sub: str(f.reason) || undefined,
      }
    case 'notify_skipped':
    case 'notify_failed':
      return {
        icon: <MessagesSquare />,
        tone: e.kind === 'notify_failed' ? 'danger' : 'warn',
        group: 'signal',
        text: <>Couldn’t tell you on Signal</>,
        sub: str(f.error || f.reason) || undefined,
      }
    default:
      return {
        icon: e.kind.startsWith('cron') ? <CalendarClock /> : <ShieldCheck />,
        tone: e.kind.endsWith('failed') ? 'danger' : 'muted',
        group: e.kind.startsWith('signal') ? 'signal' : 'gate',
        text: <span className="font-mono text-[12px]">{e.kind}</span>,
        sub: argsLine(
          Object.fromEntries(Object.entries(f).filter(([k]) => k !== 'ts' && k !== 'kind')),
        ),
      }
  }
}

/** The gate writes "2026-10-22T15:19:47+0000"; Date wants "+00:00". */
export function auditTime(ts: string): number {
  const t = Date.parse(ts.replace(/([+-]\d\d)(\d\d)$/, '$1:$2'))
  return Number.isNaN(t) ? 0 : t
}
