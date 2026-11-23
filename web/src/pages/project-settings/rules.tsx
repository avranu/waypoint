// Automation vocabulary: friendly names for triggers and actions, which
// fields each action uses, and a plain-English rendering of a rule.

import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { chainText, shortModel } from '@/components/ModelChain'
import { catalogQuery, phraseParts, type Catalog, type ParamDef } from '@/lib/automations'
import {
  CATEGORIES,
  PRIORITIES,
  type Action,
  type Category,
  type Rule,
  type Status,
} from '@/lib/types'

export interface TriggerMeta {
  label: string
  /** "When …" clause without the "When". */
  phrase: string
  group: 'Issues' | 'Time' | 'Code' | 'Pull requests' | 'Sprints'
}

export const TRIGGERS: Record<string, TriggerMeta> = {
  created: { label: 'Issue created', phrase: 'an issue is created', group: 'Issues' },
  status_changed: { label: 'Status changes', phrase: 'the status changes', group: 'Issues' },
  assigned: { label: 'Someone is assigned', phrase: 'someone is assigned', group: 'Issues' },
  unassigned: { label: 'Someone is unassigned', phrase: 'someone is unassigned', group: 'Issues' },
  commented: { label: 'Someone comments', phrase: 'someone comments', group: 'Issues' },
  field_changed: { label: 'A field changes', phrase: 'a field changes', group: 'Issues' },
  children_done: {
    label: 'All sub-issues done',
    phrase: 'all sub-issues are done',
    group: 'Issues',
  },
  reopened: { label: 'Issue reopened', phrase: 'an issue is reopened', group: 'Issues' },
  step_changed: {
    label: 'Step changes',
    phrase: 'the workflow step changes',
    group: 'Issues',
  },
  due_soon: { label: 'Due date is near', phrase: 'an issue is due', group: 'Time' },
  stale: { label: 'Issue goes quiet', phrase: 'an open issue goes quiet', group: 'Time' },
  schedule: { label: 'On a schedule', phrase: 'on a schedule', group: 'Time' },
  pr_opened: { label: 'Linked PR opened', phrase: 'a linked pull request opens', group: 'Code' },
  pr_merged: {
    label: 'Linked PR merged',
    phrase: 'a linked pull request is merged',
    group: 'Code',
  },
  pr_closed: {
    label: 'Linked PR closed',
    phrase: 'a linked pull request is closed',
    group: 'Code',
  },
  linked_issue_closed: {
    label: 'Linked forge issue closed',
    phrase: 'a linked forge issue is closed',
    group: 'Code',
  },
  pr_updated: {
    label: 'Pull request opened or pushed to',
    phrase: 'a pull request is opened, pushed to, marked ready or reopened',
    group: 'Pull requests',
  },
  pr_command: {
    label: 'Review asked for',
    phrase: 'someone comments “@waypoint review” on a pull request',
    group: 'Pull requests',
  },
  sprint_started: { label: 'Sprint started', phrase: 'a sprint starts', group: 'Sprints' },
  sprint_closed: { label: 'Sprint closed', phrase: 'a sprint is closed', group: 'Sprints' },
}

export const triggerMeta = (event: string): TriggerMeta =>
  TRIGGERS[event] ?? {
    label: event.replace(/_/g, ' '),
    phrase: event.replace(/_/g, ' '),
    group: 'Issues',
  }

/** Triggers about a pull request rather than an issue (WP-D14). */
export const PR_TRIGGERS = ['pr_updated', 'pr_command', 'pr_labeled', 'review_requested']
export const isPRTrigger = (event: string) => PR_TRIGGERS.includes(event)

/** Emoji every forge accepts as a reaction. */
export const REACTIONS: { value: string; emoji: string; label: string }[] = [
  { value: 'eyes', emoji: '👀', label: 'eyes' },
  { value: '+1', emoji: '👍', label: 'thumbs up' },
  { value: '-1', emoji: '👎', label: 'thumbs down' },
  { value: 'rocket', emoji: '🚀', label: 'rocket' },
  { value: 'heart', emoji: '❤️', label: 'heart' },
  { value: 'confused', emoji: '😕', label: 'confused' },
]

export const CATEGORY_LABEL: Record<Category, string> = {
  backlog: 'Backlog',
  todo: 'To do',
  in_progress: 'In progress',
  done: 'Done',
  canceled: 'Canceled',
}

export const isCategory = (v: string): v is Category =>
  (CATEGORIES as string[]).includes(v.replace(/ /g, '_').toLowerCase())

export const fieldLabel = (f: string) => f.replace(/_/g, ' ')

// ---- actions ----

export type ActionField =
  | 'status'
  | 'field'
  | 'value'
  | 'user'
  | 'title'
  | 'message'
  | 'url'
  | 'token'
  | 'priority'
  | 'titles'
  | 'body'
  | 'template'
  | 'issue_type'
  | 'labels'
  | 'due'
  | 'models'
  | 'focus'
  | 'force'
  | 'max_findings'

export interface ActionMeta {
  label: string
  /** How the action reads in a sentence: "then [move it to] Done". */
  verb: string
  fields: ActionField[]
  group: 'Issue' | 'People' | 'Notify' | 'Integrations' | 'Time' | 'Pull request'
}

export const ACTIONS: Record<string, ActionMeta> = {
  set_status: { label: 'Move to status', verb: 'move it to', fields: ['status'], group: 'Issue' },
  set_priority: {
    label: 'Set priority',
    verb: 'set its priority to',
    fields: ['value'],
    group: 'Issue',
  },
  set_field: { label: 'Set a field', verb: 'set', fields: ['field', 'value'], group: 'Issue' },
  add_label: { label: 'Add tag', verb: 'add the tag', fields: ['value'], group: 'Issue' },
  remove_label: { label: 'Remove tag', verb: 'remove the tag', fields: ['value'], group: 'Issue' },
  add_to_sprint: {
    label: 'Add to sprint',
    verb: 'put it in the sprint',
    fields: ['value'],
    group: 'Issue',
  },
  create_subtasks: {
    label: 'Create subtasks',
    verb: 'create subtasks',
    fields: ['titles'],
    group: 'Issue',
  },
  create_issue: {
    label: 'Create an issue',
    verb: 'create an issue',
    fields: ['template', 'title', 'body', 'issue_type', 'user', 'labels', 'due'],
    group: 'Issue',
  },
  set_parent_status: {
    label: 'Move parent to status',
    verb: 'move its parent to',
    fields: ['status'],
    group: 'Issue',
  },
  reopen_parent: {
    label: 'Reopen closed parent',
    verb: 'reopen its closed parent, to',
    fields: ['status'],
    group: 'Issue',
  },
  set_children_status: {
    label: 'Move open sub-issues',
    verb: 'move its open sub-issues to',
    fields: ['status'],
    group: 'Issue',
  },
  assign: { label: 'Assign', verb: 'assign it to', fields: ['user'], group: 'People' },
  add_assignee: {
    label: 'Add assignee',
    verb: 'add an assignee:',
    fields: ['user'],
    group: 'People',
  },
  comment: { label: 'Add a comment', verb: 'add a comment', fields: ['message'], group: 'People' },
  notify: {
    label: 'Send a notification',
    verb: 'notify',
    fields: ['user', 'message'],
    group: 'Notify',
  },
  gotify: {
    label: 'Push to Gotify',
    verb: 'send a Gotify push to',
    fields: ['user', 'url', 'token', 'title', 'message', 'priority'],
    group: 'Notify',
  },
  email: {
    label: 'Send an email',
    verb: 'email',
    fields: ['user', 'title', 'message'],
    group: 'Notify',
  },
  webhook: {
    label: 'Call a webhook',
    verb: 'call a webhook',
    fields: ['url', 'body'],
    group: 'Integrations',
  },
  kestra: {
    label: 'Run a Kestra flow',
    verb: 'run a Kestra flow',
    fields: ['url', 'body'],
    group: 'Integrations',
  },
  review: {
    label: 'Review it with a model',
    verb: 'review it with',
    fields: ['models', 'focus', 'force', 'max_findings'],
    group: 'Pull request',
  },
  pr_comment: {
    label: 'Comment on the pull request',
    verb: 'comment on it',
    fields: ['message'],
    group: 'Pull request',
  },
  react: {
    label: 'React with an emoji',
    verb: 'react with',
    fields: ['value'],
    group: 'Pull request',
  },
  start_timer: { label: 'Start timer', verb: 'start a timer for', fields: ['user'], group: 'Time' },
  stop_timer: { label: 'Stop timer', verb: 'stop the timer of', fields: ['user'], group: 'Time' },
}

export const actionMeta = (type: string): ActionMeta =>
  ACTIONS[type] ?? {
    label: type.replace(/_/g, ' '),
    verb: type.replace(/_/g, ' '),
    fields: [],
    group: 'Issue',
  }

export const SET_FIELDS: { value: string; label: string; hint: string }[] = [
  { value: 'points', label: 'Points', hint: 'A number; empty clears it.' },
  {
    value: 'due_date',
    label: 'Due date',
    hint: 'YYYY-MM-DD, or relative like +3d, +2w; empty clears it.',
  },
  {
    value: 'start_date',
    label: 'Start date',
    hint: 'YYYY-MM-DD, or relative like today, +1d; empty clears it.',
  },
  { value: 'type', label: 'Type', hint: 'epic, story, task, bug or subtask.' },
  { value: 'sprint', label: 'Sprint', hint: 'A sprint name, active, next, or empty for none.' },
  { value: 'title', label: 'Title', hint: 'A template, e.g. [Bug] {{.Issue.Title}}' },
]

// ---- plain-English rendering ----

const B = ({ children }: { children: ReactNode }) => (
  <b className="font-semibold text-fg">{children}</b>
)

function statusPhrase(v: string, statuses: Status[]): ReactNode {
  const parts = v
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  return parts.map((p, i) => {
    const st = statuses.find((s) => s.name.toLowerCase() === p.toLowerCase())
    const label = st
      ? st.name
      : isCategory(p)
        ? `any ${CATEGORY_LABEL[p.replace(/ /g, '_').toLowerCase() as Category]} status`
        : p
    return (
      <span key={i}>
        {i > 0 && ' or '}
        <B>{label}</B>
      </span>
    )
  })
}

function userPhrase(u: string | undefined, type: string): string {
  switch ((u ?? '').toLowerCase()) {
    case '':
      return type === 'gotify' ? '' : 'the person who triggered it'
    case 'actor':
    case 'me':
      return 'the person who triggered it'
    case 'reporter':
      return 'the reporter'
    case 'assignees':
      return 'the assignees'
    case 'watchers':
      return 'the watchers'
    case 'none':
      return 'nobody'
  }
  return '@' + u
}

const quote = (s: string, n = 40) => `“${s.length > n ? s.slice(0, n - 1) + '…' : s}”`

function hostOf(url = ''): string {
  try {
    return new URL(url).host
  } catch {
    return url || 'a URL'
  }
}

/** A setting's value as it reads in a sentence. */
function valueNode(p: ParamDef | undefined, v: unknown, statuses: Status[]): ReactNode {
  if (v === undefined || v === null || v === '' || (Array.isArray(v) && v.length === 0)) {
    // A choice whose empty value means something ("any").
    const none = p?.type === 'select' ? p.options?.find((o) => o.value === '') : undefined
    return <B>{none ? none.label.toLowerCase() : '…'}</B>
  }
  if (p?.type === 'status') return statusPhrase(String(v), statuses)
  if (p?.type === 'secret') return <B>••••</B>
  if (p?.type === 'models') return <B>{chainText(v as ChainModelList)}</B>
  if (Array.isArray(v)) return <B>{v.join(', ')}</B>
  const opt = p?.options?.find((o) => o.value === String(v))
  const text = opt ? opt.label : String(v)
  if (p?.type === 'weekday') return <B>{WEEKDAYS[Number(v)] ?? text}</B>
  if (p?.type === 'url') return <B>{hostOf(text)}</B>
  return <B>{text.length > 40 ? text.slice(0, 39) + '…' : text}</B>
}

type ChainModelList = { provider_id: number; model: string; cli?: string }[]

/** A catalog phrase with the step's values in it: "move it to Done". */
function phraseNode(
  phrase: string,
  defs: ParamDef[] | undefined,
  vals: Record<string, unknown>,
  statuses: Status[],
): ReactNode {
  return phraseParts(phrase).map((part, i) =>
    'lit' in part ? (
      <span key={i}>{part.lit}</span>
    ) : (
      <span key={i}>
        {valueNode(
          defs?.find((p) => p.name === part.param),
          vals[part.param],
          statuses,
        )}
      </span>
    ),
  )
}

export function describeAction(a: Action, statuses: Status[], cat?: Catalog): ReactNode {
  const st = a.status ? statusPhrase(a.status, statuses) : <B>…</B>
  switch (a.type) {
    case 'set_status':
      return <>move it to {st}</>
    case 'set_parent_status':
      return <>move its parent to {st}</>
    case 'reopen_parent':
      return <>reopen its parent (to {a.status ? st : <B>In progress</B>})</>
    case 'set_children_status':
      return <>move its open sub-issues to {st}</>
    case 'set_priority':
      return (
        <>
          set priority to <B>{a.value || '…'}</B>
        </>
      )
    case 'set_field':
      return a.value ? (
        <>
          set {fieldLabel(a.field || 'a field')} to <B>{a.value}</B>
        </>
      ) : (
        <>clear {fieldLabel(a.field || 'a field')}</>
      )
    case 'assign':
      return a.user === 'none' ? (
        <>unassign everyone</>
      ) : (
        <>
          assign <B>{userPhrase(a.user, a.type)}</B>
        </>
      )
    case 'add_assignee':
      return (
        <>
          add <B>{userPhrase(a.user, a.type)}</B> as an assignee
        </>
      )
    case 'add_label':
      return (
        <>
          add tag <B>{a.value || '…'}</B>
        </>
      )
    case 'remove_label':
      return (
        <>
          remove tag <B>{a.value || '…'}</B>
        </>
      )
    case 'comment':
      return <>comment {a.message ? quote(a.message) : '…'}</>
    case 'notify':
      return (
        <>
          notify <B>{userPhrase(a.user || 'actor', a.type)}</B>
        </>
      )
    case 'gotify':
      return a.url ? (
        <>
          push to Gotify at <B>{hostOf(a.url)}</B>
        </>
      ) : (
        <>
          push to <B>{userPhrase(a.user || 'assignees', a.type)}</B> on Gotify
        </>
      )
    case 'email': {
      const to = String((a as Record<string, unknown>).to ?? '').trim()
      const who = a.user ? userPhrase(a.user, a.type) : ''
      return (
        <>
          email <B>{[who, to].filter(Boolean).join(' and ') || '…'}</B>
        </>
      )
    }
    case 'webhook':
      return (
        <>
          POST to <B>{hostOf(a.url)}</B>
        </>
      )
    case 'kestra':
      return (
        <>
          run Kestra flow <B>{kestraFlow(a.url)}</B>
        </>
      )
    case 'start_timer':
      return (
        <>
          start a timer for <B>{userPhrase(a.user, a.type)}</B>
        </>
      )
    case 'stop_timer':
      return (
        <>
          stop <B>{userPhrase(a.user, a.type)}</B>'s timer
        </>
      )
    case 'add_to_sprint':
      return a.value === 'none' ? (
        <>remove it from its sprint</>
      ) : (
        <>
          add it to the <B>{a.value || '…'}</B> sprint
        </>
      )
    case 'create_issue':
      return a.template ? (
        <>
          create an issue from the <B>{a.template}</B> template
        </>
      ) : (
        <>
          create <B>{a.title || 'an issue'}</B>
        </>
      )
    case 'review': {
      const n = a.models?.length ?? 0
      return (
        <>
          review it with{' '}
          <B>
            {n === 0
              ? 'the review models'
              : n === 1
                ? shortModel(a.models![0].model)
                : plural(n, 'model')}
          </B>
          {a.focus && <> ({quote(a.focus, 30)})</>}
        </>
      )
    }
    case 'pr_comment':
      return <>comment {a.message ? quote(a.message) : '…'} on it</>
    case 'react':
      return (
        <>
          react with <B>{REACTIONS.find((r) => r.value === a.value)?.emoji ?? a.value ?? '…'}</B>
        </>
      )
    case 'create_subtasks': {
      const n = a.titles?.filter((t) => t.trim()).length ?? 0
      return (
        <>
          create{' '}
          <B>
            {n} subtask{n === 1 ? '' : 's'}
          </B>
        </>
      )
    }
  }
  const def = cat?.actions.find((d) => d.name === a.type)
  if (def) {
    if (a.type === 'repeat')
      return (
        <>
          repeat {plural(a.steps?.length ?? 0, 'step')}
          {a.until ? (
            <>
              {' '}
              until{' '}
              <code className="rounded bg-sunken px-1 py-0.5 font-mono text-[12px] text-fg">
                {String(a.until)}
              </code>
            </>
          ) : (
            <> up to {String(a.max ?? 3)} times</>
          )}
        </>
      )
    return <>{phraseNode(def.phrase, def.params, a, statuses)}</>
  }
  return <>{actionMeta(a.type).label.toLowerCase()}</>
}

function kestraFlow(url = ''): string {
  const m = url.match(/executions\/webhook\/([^/]+)\/([^/]+)/)
  return m ? `${m[1]}/${m[2]}` : hostOf(url)
}

export function describeTrigger(r: Rule, statuses: Status[], cat?: Catalog): ReactNode {
  const t = r.trigger
  if (t.event === 'status_changed') {
    return (
      <>
        When status changes{t.from && <> from {statusPhrase(t.from, statuses)}</>}
        {t.to && <> to {statusPhrase(t.to, statuses)}</>}
      </>
    )
  }
  if (t.event === 'step_changed')
    return (
      <>
        When the workflow step changes
        {t.from && (
          <>
            {' '}
            from <B>{t.from}</B>
          </>
        )}
        {t.to && (
          <>
            {' '}
            to <B>{t.to}</B>
          </>
        )}
      </>
    )
  if (t.event === 'field_changed')
    return t.field ? (
      <>
        When <B>{fieldLabel(t.field)}</B> changes
      </>
    ) : (
      <>When any field changes</>
    )
  if (t.event === 'due_soon')
    return t.days ? (
      <>
        When an issue is due within <B>{plural(t.days, 'day')}</B>
      </>
    ) : (
      <>When an issue is due today</>
    )
  if (t.event === 'stale')
    return (
      <>
        When an open issue hasn't changed for <B>{plural(t.days ?? 0, 'day')}</B>
      </>
    )
  if (t.event === 'schedule') return <>{scheduleText(t)}</>
  if (t.event === 'pr_command')
    return t.command ? (
      <>
        When someone comments <B>@waypoint {t.command}</B> on a pull request
      </>
    ) : (
      <>
        When someone gives <B>@waypoint</B> a command on a pull request
      </>
    )
  if (t.event === 'pr_updated')
    return t.on === 'new' ? (
      <>When a pull request is opened, marked ready or reopened</>
    ) : (
      <>When a pull request is opened or pushed to</>
    )
  const def = cat?.triggers.find((d) => d.name === t.event)
  if (def) return <>When {phraseNode(def.phrase, def.params, t, statuses)}</>
  return <>When {triggerMeta(t.event).phrase}</>
}

/** What a pull request filter leaves in or out, for the rule's sentence;
 * drafts and bots only matter when a pull request is opened or pushed to. */
export function describePRFilter(f: Rule['pr'], event = 'pr_updated'): string[] {
  const out: string[] = []
  const or = (l: string[]) => l.join(' or ')
  if (event === 'pr_updated' && !f?.drafts) out.push('not drafts')
  if (f?.authors?.length) out.push(`by ${or(f.authors)}`)
  else if (event === 'pr_updated' && !f?.bots) out.push('not by bots')
  if (f?.repos?.length) out.push(`in ${or(f.repos)}`)
  if (f?.bases?.length) out.push(`into ${or(f.bases)}`)
  if (f?.labels?.length) out.push(`labelled ${f.labels.join(' and ')}`)
  if (f?.paths?.length) out.push(`touching ${or(f.paths)}`)
  const min = f?.min_changed_lines
  const max = f?.max_changed_lines
  if (min && max) out.push(`${min}–${max} changed lines`)
  else if (min) out.push(`at least ${min} changed lines`)
  else if (max) out.push(`at most ${max} changed lines`)
  if (f?.skip_authors?.length) out.push(`not by ${or(f.skip_authors)}`)
  if (f?.skip_labels?.length) out.push(`not labelled ${f.skip_labels.join(', ')}`)
  if (f?.skip_heads?.length) out.push(`not from ${f.skip_heads.join(', ')}`)
  if (f?.skip_paths?.length) out.push(`not only touching ${or(f.skip_paths)}`)
  return out
}

export const WEEKDAYS = [
  'Sunday',
  'Monday',
  'Tuesday',
  'Wednesday',
  'Thursday',
  'Friday',
  'Saturday',
]

const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`

function ordinal(n: number) {
  const s = ['th', 'st', 'nd', 'rd'][n % 100 > 10 && n % 100 < 14 ? 0 : n % 10] ?? 'th'
  return `${n}${s}`
}

export function scheduleText(t: Rule['trigger']): string {
  const at = t.at ?? '09:00'
  switch (t.every) {
    case 'cron':
      return `On the schedule ${t.cron ? String(t.cron) : '…'}`
    case 'weekday':
      return `Every weekday at ${at}`
    case 'week':
      return `Every ${WEEKDAYS[t.weekday ?? 1]} at ${at}`
    case 'month':
      return `On the ${ordinal(t.month_day ?? 1)} of every month at ${at}`
    default:
      return `Every day at ${at}`
  }
}

/** "When status changes to In Progress, if is:unassigned → assign the person who triggered it" */
export function DescribeRule({ rule, statuses }: { rule: Rule; statuses: Status[] }) {
  const cat = useQuery(catalogQuery()).data
  const isPR =
    cat?.triggers.find((t) => t.name === rule.trigger.event)?.pr ?? isPRTrigger(rule.trigger.event)
  return (
    <span className="text-[13px] leading-relaxed text-muted">
      {describeTrigger(rule, statuses, cat)}
      {isPR && describePRFilter(rule.pr, rule.trigger.event).length > 0 && (
        <> ({describePRFilter(rule.pr, rule.trigger.event).join('; ')})</>
      )}
      {rule.if && !isPR && (
        <>
          , if{' '}
          <code className="rounded bg-sunken px-1 py-0.5 font-mono text-[12px] text-fg">
            {rule.if}
          </code>
        </>
      )}
      <span className="px-1 text-subtle">→</span>
      {rule.actions.length === 0
        ? 'do nothing yet'
        : rule.actions.map((a, i) => (
            <span key={i}>
              {i > 0 && (i === rule.actions.length - 1 ? ', then ' : ', ')}
              {describeAction(a, statuses, cat)}
            </span>
          ))}
    </span>
  )
}

export { PRIORITIES }
