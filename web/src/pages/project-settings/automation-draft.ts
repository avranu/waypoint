// What the automation editor edits: a draft of the rule, turned back into a
// rule when saved. The catalog says which settings each trigger and action
// has, so a draft keeps whatever it was given (old rules load and save
// unchanged) and saving drops only the settings that no longer apply.

import {
  paramShown,
  type ActionDef,
  type Catalog,
  type ParamDef,
  type TriggerDef,
} from '@/lib/automations'
import type {
  Action,
  Automation,
  ChainModel,
  Concurrency,
  PRFilter,
  Rule,
  RuleInput,
  Status,
  Trigger,
} from '@/lib/types'
import { CATEGORY_LABEL, fieldLabel } from './rules'
import { arrayMove } from '@dnd-kit/sortable'

/** A step in the editor; a repeat's steps are its kids. */
export interface DStep {
  uid: number
  a: Action
  kids: DStep[]
}

export interface DInput {
  uid: number
  name: string
  label: string
  type: RuleInput['type']
  default: string
  /** one per line */
  optionsText: string
  required: boolean
}

export interface Draft {
  name: string
  enabled: boolean
  trigger: Trigger
  cond: string
  pr: PRFilter
  steps: DStep[]
  inputs: DInput[]
  concurrency: '' | Concurrency
  timeZone: string
  allowAgents: boolean
}

let nextUid = 1
export const uid = () => nextUid++

export function wrapStep(a: Action): DStep {
  const { steps, ...rest } = a
  return { uid: uid(), a: { ...rest }, kids: (steps ?? []).map(wrapStep) }
}

export const maxSteps = 20
export const maxInputs = 10

export const blankDraft = (): Draft => ({
  name: '',
  enabled: true,
  trigger: { event: 'status_changed' },
  cond: '',
  pr: {},
  steps: [],
  inputs: [],
  concurrency: '',
  timeZone: '',
  allowAgents: false,
})

export const draftFromRule = (name: string, enabled: boolean, rule: Rule): Draft => ({
  name,
  enabled,
  trigger: { ...rule.trigger },
  cond: rule.if ?? '',
  pr: { ...(rule.pr ?? {}) },
  steps: rule.actions.map(wrapStep),
  inputs: (rule.inputs ?? []).map((i) => ({
    uid: uid(),
    name: i.name,
    label: i.label ?? '',
    type: i.type,
    default: i.default ?? '',
    optionsText: (i.options ?? []).join('\n'),
    required: !!i.required,
  })),
  concurrency: rule.concurrency ?? '',
  timeZone: rule.time_zone ?? '',
  allowAgents: !!rule.allow_agents,
})

export const draftFromAutomation = (a: Automation): Draft =>
  draftFromRule(a.name, a.enabled, a.rule)

// ---- defaults ----

/** What a new trigger starts with: the catalog's defaults for the settings it shows. */
export function defaultTrigger(def: TriggerDef): Trigger {
  const t: Trigger = { event: def.name }
  const params = def.params ?? []
  for (const p of params) if (p.default !== undefined && !p.when) t[p.name] = p.default
  for (const p of params)
    if (p.default !== undefined && p.when && paramShown(p, t)) t[p.name] = p.default
  if (def.name === 'pr_command') t.command = 'review'
  return t
}

/** What a new step starts with. */
export function defaultAction(def: ActionDef): Action {
  const type = def.name
  const a: Action = { type }
  switch (type) {
    case 'set_status':
      a.status = 'done'
      break
    case 'reopen_parent':
      a.status = 'in_progress'
      break
    case 'assign':
    case 'add_assignee':
    case 'start_timer':
    case 'stop_timer':
      a.user = 'actor'
      break
    case 'notify':
      a.user = 'assignees'
      break
    case 'set_priority':
      a.value = 'high'
      break
    case 'set_field':
      a.field = 'due_date'
      a.value = '+3d'
      break
    case 'add_to_sprint':
      a.value = 'active'
      break
    case 'create_subtasks':
      a.titles = []
      break
    case 'react':
      a.value = 'rocket'
      break
    case 'repeat':
      a.max = 3
      break
  }
  for (const p of def.params ?? [])
    if (a[p.name] === undefined && p.default !== undefined && !p.advanced && !p.when)
      a[p.name] = p.default
  return a
}

// ---- cleaning ----

function cleanValue(p: ParamDef, v: unknown): unknown {
  switch (p.type) {
    case 'number':
    case 'weekday': {
      if (v === '' || v === null || v === undefined) return undefined
      const n = typeof v === 'number' ? v : Number(v)
      return Number.isFinite(n) ? Math.round(n) : undefined
    }
    case 'bool':
      return v ? true : undefined
    case 'list':
    case 'globs': {
      const l = (Array.isArray(v) ? v : typeof v === 'string' ? v.split('\n') : [])
        .map((x) => String(x).trim())
        .filter(Boolean)
      return l.length ? l : undefined
    }
    case 'models': {
      const m = ((v as ChainModel[] | undefined) ?? []).filter((x) =>
        x.cli ? true : x.provider_id && x.model.trim(),
      )
      return m.length
        ? m.map((x) =>
            x.cli
              ? { provider_id: 0, model: (x.model ?? '').trim(), cli: x.cli }
              : { provider_id: x.provider_id, model: x.model.trim() },
          )
        : undefined
    }
    case 'agents': {
      const m = ((v as { cli: string; model?: string }[] | undefined) ?? []).filter((x) => x.cli)
      return m.length ? m.map((x) => ({ cli: x.cli, model: (x.model ?? '').trim() })) : undefined
    }
    case 'select':
    case 'field': {
      const s = typeof v === 'string' ? v.trim() : ''
      // "Anyone" is no filter at all.
      return s === '' || (p.name === 'by' && s === 'any') ? undefined : s
    }
    default: {
      const s = typeof v === 'string' ? v.trim() : v === undefined || v === null ? '' : String(v)
      return s === '' ? undefined : s
    }
  }
}

/** The settings a definition has that are shown and filled in; settings with a default but left alone take it. */
export function cleanParams(
  defs: ParamDef[],
  src: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const p of defs) {
    if (!paramShown(p, src)) continue
    let v = src[p.name]
    if ((v === undefined || v === '') && p.default !== undefined && !p.advanced) v = p.default
    const c = cleanValue(p, v)
    if (c !== undefined) out[p.name] = c
  }
  return out
}

export function cleanStep(s: DStep, cat: Catalog): Action {
  const a = s.a
  const out: Action = { type: a.type }
  if (a.id?.trim()) out.id = a.id.trim()
  if (a.only_if?.trim()) out.only_if = a.only_if.trim()
  if (a.on_error === 'continue') out.on_error = 'continue'
  if (a.on_error === 'retry') {
    out.on_error = 'retry'
    out.retries = Math.min(5, Math.max(1, Math.round(a.retries ?? 3)))
    if (a.retry_delay?.trim()) out.retry_delay = a.retry_delay.trim()
  }
  const def = cat.actions.find((d) => d.name === a.type)
  if (!def) {
    // Not in the catalog (an action from a newer version): leave it as it was.
    for (const [k, v] of Object.entries(a)) if (!(k in out) && v !== undefined) out[k] = v
  } else {
    Object.assign(out, cleanParams(def.params ?? [], a))
  }
  if (a.type === 'repeat') out.steps = s.kids.map((k) => cleanStep(k, cat))
  return out
}

const globList = (v?: string[]) => (v ?? []).map((x) => x.trim()).filter(Boolean)

/** The filter without empty parts; drafts and bots only matter for pr_updated. */
export function cleanPR(f: PRFilter, event: string): PRFilter | undefined {
  const out: PRFilter = {}
  if (event === 'pr_updated') {
    if (f.drafts) out.drafts = true
    if (f.bots) out.bots = true
  }
  const lists = [
    'bases',
    'skip_labels',
    'skip_heads',
    'authors',
    'skip_authors',
    'labels',
    'paths',
    'skip_paths',
    'repos',
  ] as const
  for (const k of lists) if (globList(f[k]).length) out[k] = globList(f[k])
  for (const k of ['min_changed_lines', 'max_changed_lines'] as const) {
    const n = f[k]
    if (typeof n === 'number' && Number.isFinite(n) && n > 0) out[k] = Math.round(n)
  }
  return Object.keys(out).length ? out : undefined
}

export function toRule(d: Draft, cat: Catalog): Rule {
  const def = cat.triggers.find((t) => t.name === d.trigger.event)
  const t: Trigger = def
    ? { event: def.name, ...cleanParams(def.params ?? [], d.trigger) }
    : { ...d.trigger }
  const rule: Rule = { trigger: t, actions: d.steps.map((s) => cleanStep(s, cat)) }
  if (def?.pr) {
    const pr = cleanPR(d.pr, t.event)
    if (pr) rule.pr = pr
  } else if (d.cond.trim() && (def?.issue ?? true)) rule.if = d.cond.trim()
  const inputs = d.inputs
    .filter((i) => i.name.trim())
    .map((i): RuleInput => {
      const r: RuleInput = { name: i.name.trim(), type: i.type }
      if (i.label.trim()) r.label = i.label.trim()
      if (i.default.trim()) r.default = i.default.trim()
      if (i.type === 'select') r.options = globList(i.optionsText.split('\n'))
      if (i.required) r.required = true
      return r
    })
  if (inputs.length) rule.inputs = inputs
  if (d.concurrency) rule.concurrency = d.concurrency
  if (d.timeZone.trim()) rule.time_zone = d.timeZone.trim()
  if (d.allowAgents) rule.allow_agents = true
  return rule
}

// ---- step ids ----

const usableID = /^[a-z][a-z0-9_]*$/

/** Every step's name in templates: its own id, or s1, s2 … (2.1, 2.2 … inside the repeat that is step 2). */
export function stepIDs(steps: DStep[]): Map<number, string> {
  const out = new Map<number, string>()
  steps.forEach((s, i) => {
    const id = s.a.id?.trim() || `s${i + 1}`
    out.set(s.uid, id)
    s.kids.forEach((k, j) => out.set(k.uid, k.a.id?.trim() || `${id}.${j + 1}`))
  })
  return out
}

/** Whether templates can name a step by this id (ids with a dot can't be written as .Steps.x). */
export const nameableID = (id: string) => usableID.test(id)

// ---- problems ----

const ID_RE = /^[a-z][a-z0-9_]{0,31}$/

function paramProblems(defs: ParamDef[], src: Record<string, unknown>, who: string): string | null {
  for (const p of defs) {
    if (!paramShown(p, src) || !(p.required || p.needed)) continue
    const v = p.type === 'bool' ? true : cleanValue(p, src[p.name] ?? p.default)
    if (v === undefined) return `${who}: fill in “${p.label}”`
  }
  return null
}

/** Light checks before sending; the server validates properly. */
export function stepProblem(s: DStep, cat: Catalog, nested = false): string | null {
  const a = s.a
  const def = cat.actions.find((d) => d.name === a.type)
  if (!def) return null
  const c = cleanStep(s, cat)
  const who = def.label
  const p = paramProblems(def.params ?? [], a, who)
  if (p) return p
  switch (a.type) {
    case 'comment':
    case 'pr_comment':
      if (!c.message) return 'Write the comment'
      break
    case 'set_field':
      if (!c.field) return 'Choose a field'
      break
    case 'webhook':
    case 'kestra':
      if (!/^https?:\/\/\S+$/.test(c.url ?? '')) return 'Enter an http(s) URL'
      break
    case 'gotify':
      if (c.url || c.token) {
        if (!(c.url && c.token)) return 'Gotify needs both a URL and an app token'
      } else if (!c.user) return 'Choose who to push to, or enter a URL and token'
      break
    case 'email':
      if (!c.user && !String((c as Record<string, unknown>).to ?? '').trim())
        return 'Choose who to email, or enter addresses'
      break
    case 'create_subtasks':
      if (!c.titles?.length) return 'Add at least one subtask title'
      break
    case 'create_issue':
      if (!c.title && !c.template) return 'Give a title or pick a template'
      break
    case 'repeat':
      if (nested) return 'A repeat can’t hold another repeat'
      if (s.kids.length === 0) return 'Add the steps to repeat'
      break
  }
  if (a.id?.trim() && !ID_RE.test(a.id.trim()))
    return 'Step names are lowercase letters, digits and _, like review_pr'
  if (
    a.on_error === 'retry' &&
    a.retry_delay?.trim() &&
    !/^\d+(\.\d+)?\s*(s|m|h|d|w)?$/i.test(a.retry_delay.trim())
  )
    return 'The retry delay is a duration like 30s or 5m'
  return null
}

export function triggerProblem(d: Draft, cat: Catalog): string | null {
  const def = cat.triggers.find((t) => t.name === d.trigger.event)
  if (!def) return null
  return paramProblems(def.params ?? [], d.trigger, def.label)
}

/** Problems by step uid (steps inside repeats too), plus ones about the rule as a whole. */
export function draftProblems(d: Draft, cat: Catalog) {
  const steps = new Map<number, string>()
  const ids = stepIDs(d.steps)
  const seen = new Map<string, number>()
  const walk = (list: DStep[], nested: boolean) => {
    for (const s of list) {
      let p = stepProblem(s, cat, nested)
      const id = ids.get(s.uid) ?? ''
      if (!p && seen.has(id)) p = `Two steps are called “${id}”`
      seen.set(id, s.uid)
      if (p) steps.set(s.uid, p)
      walk(s.kids, true)
    }
  }
  walk(d.steps, false)
  const inputs = new Map<number, string>()
  const names = new Set<string>()
  for (const i of d.inputs) {
    const n = i.name.trim()
    if (!n) continue
    if (!/^[a-z][a-z0-9_]{0,31}$/.test(n))
      inputs.set(i.uid, 'Names are lowercase letters, digits and _, like focus_area')
    else if (names.has(n)) inputs.set(i.uid, `Two inputs are called “${n}”`)
    else if (i.type === 'select' && globList(i.optionsText.split('\n')).length === 0)
      inputs.set(i.uid, 'A choice needs options to choose from')
    names.add(n)
  }
  const total = (l: DStep[]): number => l.reduce((n, s) => n + 1 + total(s.kids), 0)
  return {
    steps,
    inputs,
    trigger: triggerProblem(d, cat),
    empty: d.steps.length === 0,
    tooMany: total(d.steps) > 40 ? 'An automation can have at most 40 steps in all' : null,
    any:
      steps.size > 0 ||
      inputs.size > 0 ||
      d.steps.length === 0 ||
      !!triggerProblem(d, cat) ||
      total(d.steps) > 40,
  }
}

// ---- names ----

export function autoName(rule: Rule, statuses: Status[], cat: Catalog): string {
  const t = rule.trigger
  const st = (v: string) =>
    statuses.find((s) => s.name.toLowerCase() === v.toLowerCase())?.name ??
    CATEGORY_LABEL[v as keyof typeof CATEGORY_LABEL] ??
    v
  let when = cat.triggers.find((d) => d.name === t.event)?.label ?? t.event.replace(/_/g, ' ')
  if (t.event === 'status_changed' && t.to) when = `Moved to ${st(t.to)}`
  else if (t.event === 'status_changed' && t.from) when = `Moved out of ${st(t.from)}`
  else if (t.event === 'step_changed' && t.to) when = `Step moves to ${t.to}`
  else if (t.event === 'field_changed' && t.field) when = `${fieldLabel(t.field)} changes`
  const first = rule.actions[0]
  const label = first
    ? (cat.actions.find((d) => d.name === first.type)?.label ?? first.type.replace(/_/g, ' '))
    : ''
  return first ? `${when}: ${label.toLowerCase()}` : when
}

// ---- editing the draft's steps ----

export const mapSteps = (list: DStep[], id: number, fn: (s: DStep) => DStep): DStep[] =>
  list.map((s) => (s.uid === id ? fn(s) : { ...s, kids: mapSteps(s.kids, id, fn) }))

export const removeStep = (list: DStep[], id: number): DStep[] =>
  list.filter((s) => s.uid !== id).map((s) => ({ ...s, kids: removeStep(s.kids, id) }))

export const addStep = (list: DStep[], parent: number | null, step: DStep): DStep[] =>
  parent === null
    ? [...list, step]
    : list.map((s) =>
        s.uid === parent
          ? { ...s, kids: [...s.kids, step] }
          : { ...s, kids: addStep(s.kids, parent, step) },
      )

/** Move a step within the list that holds it (a drop on a step of another list does nothing). */
export function reorder(
  list: DStep[],
  active: number,
  to: (i: number, from: number) => number,
): DStep[] {
  const from = list.findIndex((s) => s.uid === active)
  if (from >= 0) {
    const i = to(from, from)
    return i < 0 || i >= list.length ? list : arrayMove(list, from, i)
  }
  return list.map((s) => ({ ...s, kids: reorder(s.kids, active, to) }))
}

export function dropOn(list: DStep[], active: number, over: number): DStep[] {
  const from = list.findIndex((s) => s.uid === active)
  let at = list.findIndex((s) => s.uid === over)
  // Dropped on a step inside a repeat: it lands by the repeat.
  if (from >= 0 && at < 0) at = list.findIndex((s) => s.kids.some((k) => k.uid === over))
  if (from >= 0 && at >= 0) return arrayMove(list, from, at)
  if (from >= 0 || at >= 0) return list
  return list.map((s) => ({ ...s, kids: dropOn(s.kids, active, over) }))
}

export const flatten = (list: DStep[]): DStep[] => list.flatMap((s) => [s, ...flatten(s.kids)])
