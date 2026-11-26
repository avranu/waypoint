import type {
  Objective,
  ObjectiveAction,
  ObjectiveBlockedWhy,
  ObjectiveCheck,
  ObjectiveHealth,
  ObjectiveKind,
  ObjectiveRoundOutcome,
} from './types'

export const HEALTH: Record<ObjectiveHealth, { label: string; tone: string }> = {
  proposed: { label: 'Proposed', tone: 'var(--wp-warn)' },
  working: { label: 'Working', tone: 'var(--wp-accent)' },
  ready: { label: 'Ready', tone: 'var(--wp-accent)' },
  waiting: { label: 'Waiting', tone: 'var(--wp-subtle)' },
  reading: { label: 'Reading', tone: 'var(--wp-info)' },
  blocked: { label: 'Needs you', tone: 'var(--wp-danger)' },
  stuck: { label: 'Stuck', tone: 'var(--wp-danger)' },
  met: { label: 'Met', tone: 'var(--wp-ok)' },
  ended: { label: 'Ended', tone: 'var(--wp-subtle)' },
}

/** Health words a list filters by, in the order a person cares about them. */
export const HEALTH_ORDER: ObjectiveHealth[] = [
  'proposed',
  'blocked',
  'stuck',
  'reading',
  'working',
  'ready',
  'waiting',
  'met',
  'ended',
]

/** Waiting on a person: a proposal to read, a block to clear, a stall to look at. */
export function needsPerson(o: Pick<Objective, 'health'>): boolean {
  return o.health === 'proposed' || o.health === 'blocked' || o.health === 'stuck'
}

export const KIND: Record<ObjectiveKind, { label: string; hint: string }> = {
  until: {
    label: 'Until a check is met',
    hint: 'A command is read after every round; the task closes when its reading reaches your target.',
  },
  each: {
    label: 'Better by an amount every round',
    hint: 'Each round is read against where it started (2x faster, 10% fewer). It is never met for good.',
  },
  judged: {
    label: 'Until a reviewer finds nothing',
    hint: 'A model that didn’t do the work reads the repository against your words after every round.',
  },
  loop: {
    label: 'A loop, no check',
    hint: 'Every round does one useful step. Nothing is read; it runs until you end it or its rounds run out.',
  },
}

export const OUTCOME: Record<ObjectiveRoundOutcome, string> = {
  reading: 'being read',
  met: 'met',
  progress: 'better, not met yet',
  worked: 'work landed, not met yet',
  none: 'no better',
  empty: 'nothing landed, so it is owed again',
  unmeasured: 'moved on without a reading',
  loop: 'done',
  diagnosed: 'diagnosed: no code, the next round reads it',
}

export const SOURCE: Record<string, string> = {
  agent: 'the worker’s own',
  runner: 'Waypoint, on your runner',
  judge: 'Waypoint’s reviewer',
  baseline: 'Waypoint, before round 1',
}

/** The readings the worker took itself: kept and shown, never deciding. */
export function workerReadings(o: Objective): ObjectiveCheck[] {
  return (o.checks ?? []).filter((c) => !c.decides && c.source === 'agent')
}

/** Seconds as the duration the server reads back: 86400 → "1d", 5400 → "90m". */
export function everyText(seconds: number): string {
  if (!seconds) return '0'
  if (seconds % 86400 === 0) return `${seconds / 86400}d`
  if (seconds % 3600 === 0) return `${seconds / 3600}h`
  return `${Math.max(1, Math.round(seconds / 60))}m`
}

/** What is theirs to do about a block, in words (the server's own sentence says what is wrong). */
export const BLOCKED_ADVICE: Record<ObjectiveBlockedWhy, string> = {
  repo: 'Attach a repository to the project (project settings), then read again.',
  person:
    'Connect a runner of yours that takes objective jobs, then read again: the commands will run on your runner.',
  check:
    'Change the check so it gives a reading, then read again; or start round 1 without a starting point.',
  touched:
    'The round changed a file the check runs, so its reading isn’t trusted yet. Accept the reading, set it aside and move on, or end the objective.',
  reading: 'Read again, or move on without a reading.',
  diagnosis:
    'The diagnosis needs you, as the worker asked. Read it above, then go on with it (the next round reads it), change what is measured, or end the objective.',
  rounds: 'It has used all its rounds. Change it to give it more rounds, or end it.',
}

export interface Steer {
  action: ObjectiveAction
  label: string
  /** what it does, for the title and the screen reader */
  hint: string
}

/**
 * The steering controls that fit where an objective in effect stands, as
 * core's ObjectiveGo allows them. End is separate: always there, confirmed.
 */
export function steers(o: Objective): Steer[] {
  if (o.state !== 'active') return []
  const out: Steer[] = []
  const reading = o.phase === 'baseline' || o.phase === 'verdict'
  const blocked = o.phase === 'blocked'
  const why = o.phase_why
  const read = reading || (blocked && why !== 'rounds' && why !== 'diagnosis')
  if (read)
    out.push({
      action: 'retry',
      label: 'Read again',
      hint: 'Waypoint takes the reading again',
    })
  if (blocked && why === 'touched')
    out.push({
      action: 'accept',
      label: 'Accept the reading',
      hint: 'Count the reading even though the round changed what the check runs',
    })
  if (blocked && why === 'diagnosis')
    out.push(
      {
        action: 'accept',
        label: 'Go on with the diagnosis',
        hint: 'The next round starts as a work round, and reads the diagnosis',
      },
      {
        action: 'next',
        label: 'Start the next round now',
        hint: 'Leave the diagnosis waiting and hand the round out now',
      },
    )
  if (blocked && (why === 'check' || why === 'touched' || why === 'reading'))
    out.push({
      action: 'next',
      label: why === 'check' ? 'Start without a reading' : 'Move on without a reading',
      hint:
        why === 'check'
          ? 'Round 1 goes ahead without a starting point'
          : 'Leave this round unread and go on to the next',
    })
  else if (reading && !blocked)
    out.push({
      action: 'next',
      label: 'Move on without a reading',
      hint: 'Leave this round unread and go on to the next',
    })
  else if (!blocked && !o.phase && o.next_at && new Date(o.next_at).getTime() > Date.now())
    out.push({
      action: 'next',
      label: `Start round ${o.round} now`,
      hint: 'End the wait: the round is handed out now',
    })
  return out
}
