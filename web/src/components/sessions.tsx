import clsx from 'clsx'
import { BIG_CONTEXT, type AgentSession, type SessionState } from '@/lib/sessions'
import { agentLabel } from '@/lib/agents'
import { tok } from '@/lib/usage-analytics'

export const STATE_LABEL: Record<SessionState, string> = {
  working: 'Working',
  waiting: 'Waiting for you',
  idle: 'Idle',
  ended: 'Ended',
}

const dotClass: Record<SessionState, string> = {
  working: 'bg-ok',
  waiting: 'bg-warn',
  idle: 'bg-subtle',
  ended: 'bg-line-strong',
}

/** A coloured dot for a session's state; working ones breathe. */
export function StateDot({
  state,
  className,
  label = true,
}: {
  state: SessionState
  className?: string
  /** read out by screen readers (the dot alone says nothing) */
  label?: boolean
}) {
  return (
    <span
      role={label ? 'img' : undefined}
      aria-label={label ? STATE_LABEL[state] : undefined}
      title={STATE_LABEL[state]}
      className={clsx(
        'inline-block size-2 shrink-0 rounded-full',
        dotClass[state],
        state === 'working' && 'animate-pulse',
        className,
      )}
    />
  )
}

/** The pill form of a session's state. */
export function StatePill({
  session: s,
}: {
  session: Pick<AgentSession, 'state' | 'wait_reason'>
}) {
  return (
    <span
      className={clsx(
        'inline-flex h-5 shrink-0 items-center gap-1.5 rounded-md px-1.5 text-[11px] font-medium',
        s.state === 'working' && 'bg-ok/15 text-ok',
        s.state === 'waiting' && 'bg-warn/15 text-warn',
        s.state === 'idle' && 'bg-sunken text-muted',
        s.state === 'ended' && 'bg-sunken text-subtle',
      )}
      title={
        s.state === 'waiting' && s.wait_reason ? s.wait_reason.replaceAll('_', ' ') : undefined
      }
    >
      <StateDot state={s.state} label={false} className="size-1.5" />
      {STATE_LABEL[s.state]}
    </span>
  )
}

/** What a session is called: its title, else the task it holds, else where it runs. */
export function sessionName(s: AgentSession) {
  return s.title || (s.issue ? `${s.issue.key} ${s.issue.title}` : `Session on ${s.machine}`)
}

/** A model as people say it: claude-opus-5-5 → opus 5.5. */
export function modelLabel(model?: string) {
  if (!model) return ''
  const m = model.replace(/^claude-/, '').replace(/\[.*\]$/, '')
  const parts = m.split('-')
  const family = parts[0]
  const ver = parts.slice(1).filter((p) => /^\d+$/.test(p))
  return ver.length ? `${family} ${ver.join('.')}` : m
}

export function sessionTokens(s: Pick<AgentSession, 'input_tokens' | 'output_tokens'>) {
  const n = s.input_tokens + s.output_tokens
  return n > 0 ? `${tok(n)} tokens` : ''
}

export function sessionAgent(s: Pick<AgentSession, 'agent' | 'model'>) {
  return [agentLabel(s.agent), modelLabel(s.model)].filter(Boolean).join(' · ')
}

/** Marks a task session (WP-34): a launcher bound it to its tasks from birth,
 * and Waypoint keeps it on them. Sessions started by hand get no mark. */
export function TaskSessionChip({ s }: { s: Pick<AgentSession, 'kind' | 'tasks'> }) {
  if (s.kind !== 'task') return null
  const tasks = s.tasks ?? []
  return (
    <span
      className="whitespace-nowrap rounded-full bg-accent-soft px-1.5 text-[11px] font-medium text-accent"
      title={`Task session: started for ${tasks.join(', ') || 'its tasks'}; it ends once they are done`}
      data-testid="session-kind"
    >
      task session
      {tasks.length > 0 && ` · ${tasks[0]}${tasks.length > 1 ? ` +${tasks.length - 1}` : ''}`}
    </span>
  )
}

/** The context its last model call carried; past BIG_CONTEXT in warning colour
 * while it still runs. */
export function ContextSize({ s }: { s: Pick<AgentSession, 'context_tokens' | 'state' | 'kind'> }) {
  if (!s.context_tokens) return null
  const big = s.context_tokens > BIG_CONTEXT && s.state !== 'ended' && s.kind !== 'task'
  const carried = `Its last model call carried ${s.context_tokens.toLocaleString()} tokens of context`
  return (
    <span
      className={clsx('tabular-nums', big && 'text-warn')}
      title={big ? `${carried}: the next task is cheaper in a fresh session` : carried}
      data-testid="session-context"
    >
      {tok(s.context_tokens)} context
    </span>
  )
}
