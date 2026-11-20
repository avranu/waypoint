// Pickers and chips for who work is for (agents), what kind of work it is
// (category), and where it is in its workflow (step). See WP-D7.

import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { Bot, Check, Lock, LockOpen, Shapes, Users, Workflow as WorkflowIcon } from 'lucide-react'
import { ToggleButton } from '@/components/ui/button'
import { Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { HUMANS_ONLY, agentLabel, isDelegated, whoOf, workflowsQuery } from '@/lib/agents'
import { ISSUE_CATEGORIES, type Workflow } from '@/lib/types'
import { clock, duration } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { PropButton, unsetText } from './pickers'

type Variant = 'pill' | 'row' | 'bare'

/**
 * "Humans only" (WP-1196): one lock that keeps agents off a task, stored as
 * issue.agent = 'none'. Handing a task to an agent is picked in the assignee
 * list, so this is the only agent control left beside it. It hides where it
 * would say nothing: locked with a person already on it, or unlocked with an
 * agent on it (picking an agent unlocks it; removing the person shows it again).
 */
export function HumansOnlyLock({
  value,
  onChange,
  people,
  agentOn,
  disabled,
}: {
  value: string
  onChange: (spec: string) => void
  /** how many people are assigned */
  people: number
  /** an agent is handed it or holds it */
  agentOn: boolean
  disabled?: boolean
}) {
  const locked = whoOf(value) === 'humans'
  if ((locked && people > 0) || (!locked && agentOn)) return null
  if (disabled)
    return locked ? (
      <span
        className="inline-flex h-7 items-center gap-1.5 px-2 text-xs text-muted"
        title="Humans only: agents never take it"
      >
        <Lock className="size-3.5" /> Humans only
      </span>
    ) : null
  return (
    <ToggleButton
      label="Humans only"
      hint={locked ? 'Agents never take it' : 'Lock it so agents never take it'}
      pressed={locked}
      onPressedChange={(on) => onChange(on ? HUMANS_ONLY : '')}
      icon={locked ? <Lock className="size-3.5" /> : <LockOpen className="size-3.5" />}
      text={locked ? 'Humans only' : undefined}
    />
  )
}

export function CategoryPicker({
  value,
  onChange,
  disabled,
  variant = 'pill',
  autoOpen,
}: {
  value: string
  onChange: (c: string) => void
  disabled?: boolean
  variant?: Variant
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const items: PickItem<string>[] = [
    { value: '', label: 'No category' },
    ...ISSUE_CATEGORIES.map((c) => ({ value: c, label: categoryLabel(c) })),
  ]
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title="Category"
      >
        <Shapes className={clsx('size-3.5', value ? 'text-muted' : 'text-subtle')} />
        <span className={clsx('truncate', !value && 'text-subtle')}>
          {value ? categoryLabel(value) : unsetText(variant, 'Category')}
        </span>
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor}>
        <Picker
          items={items}
          selected={[value]}
          placeholder="Kind of work…"
          onClose={p.close}
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

export const categoryLabel = (c: string) =>
  c === 'bugfix' ? 'Bug fix' : c === 'ops' ? 'Ops' : c[0].toUpperCase() + c.slice(1)

/** The issue's workflow and current step; pick a step, or another workflow. */
export function WorkflowPicker({
  projectKey,
  workflowId,
  step,
  onWorkflow,
  onStep,
  disabled,
  variant = 'pill',
  autoOpen,
}: {
  projectKey: string
  workflowId: number | null | undefined
  step: string | undefined
  onWorkflow: (name: string | null) => void
  /** omitted when there is no step to set yet (new issues start at the first) */
  onStep?: (name: string) => void
  disabled?: boolean
  variant?: Variant
  autoOpen?: boolean
}) {
  const p = usePopover(autoOpen)
  const workflows = useQuery(workflowsQuery(projectKey)).data ?? []
  const wf = workflows.find((w) => w.id === workflowId)
  const idx = wf
    ? wf.steps.findIndex((s) => s.name.toLowerCase() === (step ?? '').toLowerCase())
    : -1
  return (
    <>
      <PropButton
        anchor={p.anchor}
        onClick={p.toggle}
        disabled={disabled}
        variant={variant}
        title={wf ? `${wf.name}: ${step || 'no step'}` : 'Workflow'}
      >
        <WorkflowIcon className={clsx('size-3.5', wf ? 'text-accent' : 'text-subtle')} />
        {wf ? (
          <span className="flex min-w-0 items-center gap-1.5">
            <span className="truncate">{step || wf.name}</span>
            {idx >= 0 && (
              <span className="shrink-0 text-xs tabular-nums text-subtle">
                {idx + 1}/{wf.steps.length}
              </span>
            )}
          </span>
        ) : (
          <span className="text-subtle">{unsetText(variant, 'Workflow')}</span>
        )}
      </PropButton>
      <Popover open={p.open} onClose={p.close} anchor={p.anchor} width={300}>
        {wf && onStep && (
          <Stepper workflow={wf} step={step} onStep={(s) => (onStep(s), p.close())} />
        )}
        <div className={clsx('flex flex-col', wf && onStep && 'mt-1 border-t border-line pt-1')}>
          <div className="px-2.5 pb-1 pt-1.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
            {wf ? 'Change workflow' : 'Put on a workflow'}
          </div>
          {workflows.map((w) => (
            <button
              key={w.id}
              onClick={() => {
                if (w.id !== workflowId) onWorkflow(w.name)
                p.close()
              }}
              className="flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] hover:bg-hover sm:h-8"
            >
              <WorkflowIcon className="size-3.5 text-muted" />
              <span className="flex-1 truncate">{w.name}</span>
              <span className="text-xs text-subtle">{w.steps.length} steps</span>
              <Check
                className={clsx(
                  'size-3.5 text-accent',
                  w.id === workflowId ? 'opacity-100' : 'opacity-0',
                )}
              />
            </button>
          ))}
          {wf && (
            <button
              onClick={() => (onWorkflow(null), p.close())}
              className="flex h-9 items-center gap-2.5 rounded-lg px-2.5 text-left text-[13px] text-muted hover:bg-hover sm:h-8"
            >
              Take it off the workflow
            </button>
          )}
        </div>
      </Popover>
    </>
  )
}

/** The steps as a vertical stepper; done ones ticked, the current one marked. */
function Stepper({
  workflow,
  step,
  onStep,
}: {
  workflow: Workflow
  step?: string
  onStep: (s: string) => void
}) {
  const cur = workflow.steps.findIndex((s) => s.name.toLowerCase() === (step ?? '').toLowerCase())
  return (
    <ol className="flex flex-col py-1" aria-label={`${workflow.name} steps`}>
      {workflow.steps.map((s, i) => (
        <li key={s.name}>
          <button
            onClick={() => onStep(s.name)}
            aria-current={i === cur ? 'step' : undefined}
            className={clsx(
              'flex w-full items-start gap-2.5 rounded-lg px-2.5 py-1.5 text-left hover:bg-hover',
              i === cur && 'bg-accent-soft',
            )}
          >
            <span
              className={clsx(
                'mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full text-[10px] font-semibold',
                i < cur
                  ? 'bg-ok text-white'
                  : i === cur
                    ? 'bg-accent text-accent-fg'
                    : 'border border-line-strong text-subtle',
              )}
            >
              {i < cur ? <Check className="size-2.5" /> : i + 1}
            </span>
            <span className="min-w-0 flex-1">
              <span className="block text-[13px] font-medium">{s.name}</span>
              {s.instructions && (
                <span className="line-clamp-2 block text-xs text-subtle">{s.instructions}</span>
              )}
            </span>
            {s.agent && <span className="shrink-0 text-[11px] text-subtle">{s.agent}</span>}
          </button>
        </li>
      ))}
    </ol>
  )
}

/** A compact robot chip for cards and rows. */
export function AgentChip({ spec, claimed }: { spec: string; claimed?: string }) {
  const anyone = !claimed && whoOf(spec) === 'anyone'
  return (
    <span
      title={
        claimed
          ? `Being worked on by ${claimed}`
          : anyone
            ? 'Anyone can take this'
            : `For agents: ${spec}`
      }
      className={clsx(
        'inline-flex h-5 max-w-[9rem] items-center gap-1 rounded-md px-1.5 text-[11px] font-medium',
        claimed ? 'bg-accent-soft text-accent' : 'bg-sunken text-muted',
      )}
    >
      {anyone ? (
        <Users className="size-3 shrink-0" />
      ) : (
        <Bot
          className={clsx('size-3 shrink-0', claimed && 'animate-pulse motion-reduce:animate-none')}
        />
      )}
      <span className="truncate">{anyone ? 'Anyone' : agentLabel(claimed || spec)}</span>
    </span>
  )
}

export function StepChip({ step }: { step: string }) {
  return (
    <span
      title={`Workflow step: ${step}`}
      className="inline-flex h-5 max-w-[7rem] items-center gap-1 rounded-md bg-sunken px-1.5 text-[11px] text-muted"
    >
      <WorkflowIcon className="size-3 shrink-0" />
      <span className="truncate">{step}</span>
    </span>
  )
}

/** The agent working the issue right now, if its claim hasn't lapsed. */
export function activeClaim(issue: { claimed_by?: string; claimed_until?: string | null }) {
  return issue.claimed_by && issue.claimed_until && Date.parse(issue.claimed_until) > Date.now()
    ? issue.claimed_by
    : undefined
}

/**
 * Robot and step chips for an issue on a card or row; nothing for work
 * that isn't delegated to agents (Anyone's or humans'). A claimed issue shows its agent with ClaimedBy instead.
 */
export function AgentChips({
  issue,
}: {
  issue: { agent?: string; step?: string; claimed_by?: string; claimed_until?: string | null }
}) {
  const claimed = activeClaim(issue)
  const delegated = isDelegated(issue.agent)
  if ((!delegated || claimed) && !issue.step) return null
  return (
    <>
      {delegated && !claimed && <AgentChip spec={issue.agent!} />}
      {issue.step && <StepChip step={issue.step} />}
    </>
  )
}

/**
 * Seconds an agent has worked on its current claim: finished stretches plus
 * the running one (none while it's idle). Null when the server doesn't say.
 */
export function claimWorked(issue: {
  claimed_at?: string | null
  claim_worked?: number
  claim_working_since?: string | null
}) {
  if (issue.claim_worked === undefined && !issue.claim_working_since)
    return issue.claimed_at ? (Date.now() - Date.parse(issue.claimed_at)) / 1000 : null
  const running = issue.claim_working_since
    ? Math.max(0, (Date.now() - Date.parse(issue.claim_working_since)) / 1000)
    : 0
  return (issue.claim_worked ?? 0) + running
}

/**
 * The agent working on an issue, shown like an assignee, with how long it
 * has worked on it while its claim lasts; dimmed while it's idle (paused).
 */
export function ClaimedBy({
  issue,
  compact,
}: {
  issue: {
    claimed_by?: string
    claimed_until?: string | null
    claimed_at?: string | null
    claim_worked?: number
    claim_working_since?: string | null
  }
  /** phones: robot and timer, the name in the tooltip */
  compact?: boolean
}) {
  const claimed = activeClaim(issue)
  const working = !!issue.claim_working_since
  useTick(1000, !!claimed && working)
  if (!claimed) return null
  const worked = claimWorked(issue)
  return (
    <span
      title={`${claimed} ${working ? 'is working on this' : 'is idle (its clock is paused)'}${worked !== null ? `; worked ${duration(worked)}` : ''}; claimed until ${new Date(issue.claimed_until!).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`}
      className={clsx(
        'inline-flex h-5 max-w-[11rem] shrink-0 items-center gap-1 rounded-full px-1.5 text-[11px] font-medium',
        working ? 'bg-accent-soft text-accent' : 'bg-sunken text-muted',
      )}
    >
      <Bot
        className={clsx('size-3 shrink-0', working && 'animate-pulse motion-reduce:animate-none')}
      />
      <span className={clsx('truncate', compact && 'max-sm:hidden')}>{agentLabel(claimed)}</span>
      {worked !== null && <span className="tabular-nums">{clock(worked)}</span>}
    </span>
  )
}
