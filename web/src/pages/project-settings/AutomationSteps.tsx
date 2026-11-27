import { createContext, useContext, useState } from 'react'
import {
  DndContext,
  KeyboardSensor,
  PointerSensor,
  TouchSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
} from '@dnd-kit/core'
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from '@dnd-kit/sortable'
import { CSS } from '@dnd-kit/utilities'
import clsx from 'clsx'
import { ArrowDown, ArrowUp, GripVertical, Plus, TriangleAlert, X } from 'lucide-react'
import { Button, IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Popover, usePopover } from '@/components/ui/popover'
import { Tip } from '@/components/ui/help'
import { inputClass } from '@/components/ui/input'
import type { ActionDef, Catalog, Preset, TriggerDef } from '@/lib/automations'
import type { Action } from '@/lib/types'
import { defaultAction, nameableID, type DInput, type DStep } from './automation-draft'
import {
  Clause,
  InsertVars,
  ParamBox,
  Sentence,
  inlineInput,
  leadOf,
  type FieldEnv,
  type VarGroup,
} from './automation-fields'
import { restrictToVerticalAxis } from './dnd'

export interface StepsApi {
  cat: Catalog
  env: FieldEnv
  trigger?: TriggerDef
  steps: DStep[]
  inputs: DInput[]
  /** every step's name in templates */
  ids: Map<number, string>
  problems: Map<number, string>
  tried: boolean
  change: (uid: number, fn: (s: DStep) => DStep) => void
  remove: (uid: number) => void
  move: (uid: number, by: number) => void
  /** dropped on another step: take its place when they share a list */
  dropOn: (active: number, over: number) => void
  add: (parent: number | null, type: string) => void
}

const Ctx = createContext<StepsApi | null>(null)
export const StepsProvider = Ctx.Provider
const useSteps = () => useContext(Ctx)!

const flat = (steps: DStep[]): DStep[] => steps.flatMap((s) => [s, ...flat(s.kids)])

export function StepsSection() {
  const api = useSteps()
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 180, tolerance: 6 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  )
  const onDragEnd = (e: DragEndEvent) => {
    if (e.over && e.active.id !== e.over.id) api.dropOn(Number(e.active.id), Number(e.over.id))
  }
  if (api.steps.length === 0) return null
  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      modifiers={[restrictToVerticalAxis]}
      onDragEnd={onDragEnd}
    >
      <StepList list={api.steps} parent={null} />
    </DndContext>
  )
}

function StepList({ list, parent }: { list: DStep[]; parent: DStep | null }) {
  return (
    <SortableContext items={list.map((s) => s.uid)} strategy={verticalListSortingStrategy}>
      <ol className="space-y-3">
        {list.map((s, i) => (
          <StepRow key={s.uid} step={s} index={i} count={list.length} parent={parent} />
        ))}
      </ol>
    </SortableContext>
  )
}

/** The values a template in this step can use. */
function varsFor(api: StepsApi, step: DStep): VarGroup[] {
  const t = api.trigger
  const groups: VarGroup[] = []
  const here = [
    ...(!t || t.issue
      ? [
          { label: 'Issue key', value: '{{.Issue.Key}}' },
          { label: 'Issue title', value: '{{.Issue.Title}}' },
          { label: 'Issue link', value: '{{.URL}}' },
        ]
      : []),
    ...(t?.pr
      ? [
          { label: 'Pull request title', value: '{{.PR.Title}}' },
          { label: 'Pull request author', value: '{{.PR.Author}}' },
          { label: 'Repository', value: '{{.PR.Repo}}' },
          { label: 'Pull request link', value: '{{.URL}}' },
        ]
      : []),
    { label: 'Whoever triggered it', value: '{{.Actor.DisplayName}}' },
    { label: 'What happened', value: '{{.Trigger.Summary}}' },
    { label: 'Today’s date', value: '{{.Date}}' },
  ]
  groups.push({ title: 'This event', items: here })
  const data = (t?.provides ?? []).map((o) => ({
    label: o.doc || o.name,
    value: `{{.Trigger.Data.${o.name}}}`,
  }))
  if (t?.name === 'webhook')
    data.push({ label: 'The request’s body', value: '{{json .Trigger.Data}}' })
  if (data.length) groups.push({ title: 'From the trigger', items: data })
  const order = flat(api.steps)
  const earlier = order.slice(
    0,
    Math.max(
      0,
      order.findIndex((s) => s.uid === step.uid),
    ),
  )
  const outs = earlier.flatMap((s) => {
    const id = api.ids.get(s.uid) ?? ''
    const def = api.cat.actions.find((d) => d.name === s.a.type)
    if (!nameableID(id) || !def?.outputs?.length) return []
    return def.outputs.map((o) => ({
      label: `${id}: ${o.doc || o.name}`,
      value: `{{.Steps.${id}.${o.name}}}`,
    }))
  })
  if (outs.length) groups.push({ title: 'Earlier steps', items: outs })
  const inputs = api.inputs
    .filter((i) => i.name.trim())
    .map((i) => ({ label: i.label.trim() || i.name, value: `{{.Inputs.${i.name.trim()}}}` }))
  if (inputs.length) groups.push({ title: 'Inputs', items: inputs })
  return groups
}

function StepRow({
  step,
  index,
  count,
  parent,
}: {
  step: DStep
  index: number
  count: number
  parent: DStep | null
}) {
  const api = useSteps()
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: step.uid,
  })
  const a = step.a
  const def = api.cat.actions.find((d) => d.name === a.type)
  const set = (name: string, v: unknown) =>
    api.change(step.uid, (s) => ({ ...s, a: { ...s.a, [name]: v } }))
  const problem = api.tried ? api.problems.get(step.uid) : null
  const noIssue = def?.needs === 'issue' && !!api.trigger && !api.trigger.issue
  const gotifyServer = !a.user || !!a.url || !!a.token
  const retype = (type: string) => {
    const nd = api.cat.actions.find((d) => d.name === type)
    if (!nd) return
    api.change(step.uid, (s) => {
      const na = defaultAction(nd)
      for (const k of ['id', 'only_if', 'on_error', 'retries', 'retry_delay'] as const)
        if (s.a[k] !== undefined) na[k] = s.a[k] as never
      return { ...s, a: na, kids: type === 'repeat' ? s.kids : [] }
    })
  }
  return (
    <li
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={clsx(isDragging && 'relative z-10 rounded-lg bg-raised shadow-pop')}
    >
      <Clause
        word={index === 0 ? 'then' : 'and'}
        end={
          <>
            <IconButton
              {...attributes}
              {...listeners}
              label="Drag to reorder"
              size="xs"
              className="cursor-grab touch-none active:cursor-grabbing"
              icon={<GripVertical className="size-3.5" />}
            />
            <IconButton
              label="Move up"
              size="xs"
              className="max-sm:hidden"
              disabled={index === 0}
              onClick={() => api.move(step.uid, -1)}
              icon={<ArrowUp className="size-3.5" />}
            />
            <IconButton
              label="Move down"
              size="xs"
              className="max-sm:hidden"
              disabled={index === count - 1}
              onClick={() => api.move(step.uid, 1)}
              icon={<ArrowDown className="size-3.5" />}
            />
            <IconButton
              label="Remove action"
              size="xs"
              onClick={() => api.remove(step.uid)}
              icon={<X className="size-3.5" />}
            />
          </>
        }
      >
        <Select
          variant="inline"
          value={a.type}
          onChange={(e) => retype(e.target.value)}
          aria-label="Action"
        >
          {actionOptions(api.cat, a.type, api.trigger, !!parent)}
        </Select>
        {def && <Sentence def={def} vals={a} set={set} env={api.env} />}
        {def?.doc && <Tip label="About this action">{def.doc.replace(/`/g, '')}</Tip>}
      </Clause>
      {def && (
        <ParamBox
          def={def}
          vals={a}
          set={set}
          env={api.env}
          vars={varsFor(api, step)}
          hide={(p) =>
            a.type === 'gotify' && (p.name === 'url' || p.name === 'token') && !gotifyServer
          }
        />
      )}
      {!def && (
        <p className="mt-1 text-xs text-subtle sm:pl-16">
          This action isn’t known to this version of Waypoint; it’s kept as it is.
        </p>
      )}
      <StepOptions step={step} />
      {a.type === 'repeat' && (
        <div className="mt-2 space-y-2 border-l-2 border-line pl-2 sm:ml-16 sm:pl-3">
          {step.kids.length > 0 && <StepList list={step.kids} parent={step} />}
          <AddStep
            first={step.kids.length === 0}
            nested
            onAdd={(type) => api.add(step.uid, type)}
          />
        </div>
      )}
      {noIssue && (
        <p className="mt-1 flex items-start gap-1.5 text-xs text-warn sm:pl-16">
          <TriangleAlert className="mt-px size-3.5 shrink-0" />
          This changes an issue, and “{api.trigger!.label}” doesn’t come with one.
        </p>
      )}
      {problem && <p className="mt-1 text-xs text-danger sm:pl-16">{problem}</p>}
    </li>
  )
}

/** Grouped by the catalog; pull request actions only for pull request triggers. */
function actionOptions(
  cat: Catalog,
  current: string,
  trigger: TriggerDef | undefined,
  nested: boolean,
) {
  const ok = (d: ActionDef) =>
    d.name === current || ((d.needs !== 'pr' || !!trigger?.pr) && !(nested && d.name === 'repeat'))
  return (
    <>
      {cat.action_groups.map((g) => {
        const list = cat.actions.filter((d) => d.group === g && ok(d))
        if (!list.length) return null
        return (
          <optgroup key={g} label={g}>
            {list.map((d) => (
              <option key={d.name} value={d.name}>
                {leadOf(d)}
              </option>
            ))}
          </optgroup>
        )
      })}
      {!cat.actions.some((d) => d.name === current) && (
        <option value={current}>{current.replace(/_/g, ' ')}</option>
      )}
    </>
  )
}

type Panel = 'id' | 'only' | 'error' | null

function StepOptions({ step }: { step: DStep }) {
  const api = useSteps()
  const a = step.a
  const [open, setOpen] = useState<Panel>(null)
  const id = api.ids.get(step.uid) ?? ''
  const set = (patch: Partial<Action>) =>
    api.change(step.uid, (s) => ({ ...s, a: { ...s.a, ...patch } }))
  const toggle = (p: Panel) => setOpen((o) => (o === p ? null : p))
  const pill = (active: boolean, on: boolean) =>
    clsx(
      'inline-flex h-6 items-center gap-1 rounded-md px-1.5 text-xs transition-colors hover:bg-hover',
      on ? 'bg-hover text-fg' : active ? 'text-fg' : 'text-subtle',
    )
  const onError =
    a.on_error === 'continue'
      ? 'carry on if it fails'
      : a.on_error === 'retry'
        ? `retry ${a.retries ?? 3}×`
        : null
  return (
    <div className="mt-1 sm:pl-16">
      <div className="flex flex-wrap items-center gap-x-1 gap-y-0.5">
        <button
          type="button"
          className={pill(!!a.id, open === 'id')}
          aria-expanded={open === 'id'}
          onClick={() => toggle('id')}
          title="Later steps read this step’s results by its name"
        >
          name <span className="font-mono text-[11px]">{id}</span>
        </button>
        <button
          type="button"
          className={pill(!!a.only_if, open === 'only')}
          aria-expanded={open === 'only'}
          onClick={() => toggle('only')}
        >
          {a.only_if ? 'only run if …' : 'Only run if…'}
        </button>
        <button
          type="button"
          className={pill(!!onError, open === 'error')}
          aria-expanded={open === 'error'}
          onClick={() => toggle('error')}
        >
          {onError ?? 'If it fails…'}
        </button>
      </div>
      {open === 'id' && (
        <div className="mt-1 flex items-center gap-2 text-xs text-muted">
          <Input
            value={a.id ?? ''}
            onChange={(e) => set({ id: e.target.value.toLowerCase() })}
            placeholder={id}
            spellCheck={false}
            className={clsx(inlineInput, '!w-40 font-mono !text-[12px]')}
            aria-label="Step name"
            autoFocus
          />
          <span>
            Later steps use{' '}
            <code className="font-mono">{`{{.Steps.${nameableID(id) ? id : 'name'}.Text}}`}</code>
          </span>
        </div>
      )}
      {open === 'only' && <OnlyIfEditor step={step} onChange={(v) => set({ only_if: v })} />}
      {open === 'error' && (
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1.5 text-[13px] text-muted">
          <Select
            variant="inline"
            value={a.on_error ?? 'stop'}
            onChange={(e) => set({ on_error: e.target.value as Action['on_error'] })}
            aria-label="When it fails"
          >
            <option value="stop">stop the automation</option>
            <option value="continue">carry on with the next step</option>
            <option value="retry">try again</option>
          </Select>
          {a.on_error === 'retry' && (
            <>
              up to
              <Input
                type="number"
                min={1}
                max={5}
                value={a.retries ?? 3}
                onChange={(e) => set({ retries: Number(e.target.value) })}
                className={clsx(inlineInput, '!w-16')}
                aria-label="Tries"
              />
              times, waiting
              <Input
                value={a.retry_delay ?? ''}
                onChange={(e) => set({ retry_delay: e.target.value })}
                placeholder="30s"
                className={clsx(inlineInput, '!w-20')}
                aria-label="Wait between tries"
              />
            </>
          )}
        </div>
      )}
    </div>
  )
}

function OnlyIfEditor({ step, onChange }: { step: DStep; onChange: (v: string) => void }) {
  const api = useSteps()
  const pop = usePopover()
  const [pending, setPending] = useState<Preset | null>(null)
  const order = flat(api.steps)
  const earlier = order
    .slice(
      0,
      Math.max(
        0,
        order.findIndex((s) => s.uid === step.uid),
      ),
    )
    .map((s) => api.ids.get(s.uid) ?? '')
    .filter(nameableID)
  const pick = (t: string) => {
    onChange(t)
    setPending(null)
    pop.close()
  }
  return (
    <div className="mt-1 flex items-center gap-1.5">
      <Input
        value={step.a.only_if ?? ''}
        onChange={(e) => onChange(e.target.value)}
        placeholder={'{{.Previous.Failed}}'}
        spellCheck={false}
        autoFocus
        className={clsx(inlineInput, 'min-w-0 flex-1 basis-40 font-mono !text-[12px]')}
        aria-label="Only if"
      />
      <Button ref={pop.anchor} size="xs" variant="ghost" onClick={pop.toggle}>
        Ready-made
      </Button>
      <Popover
        open={pop.open}
        onClose={() => {
          pop.close()
          setPending(null)
        }}
        anchor={pop.anchor}
        width={300}
      >
        <div className="max-h-72 overflow-auto p-1">
          {pending ? (
            <>
              <div className="px-2 py-1 text-xs text-subtle">{pending.label}: which step?</div>
              {earlier.length === 0 && (
                <div className="px-2 py-2 text-xs text-subtle">There’s no earlier step yet.</div>
              )}
              {earlier.map((id) => (
                <button
                  key={id}
                  type="button"
                  onClick={() => pick(pending.template.replace('{step}', id))}
                  className="block w-full rounded-lg px-2 py-1.5 text-left font-mono text-[13px] hover:bg-hover"
                >
                  {id}
                </button>
              ))}
            </>
          ) : (
            api.cat.presets.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => (p.step ? setPending(p) : pick(p.template))}
                className="block w-full rounded-lg px-2 py-1.5 text-left hover:bg-hover"
              >
                <div className="text-[13px]">{p.label}</div>
                <div className="truncate font-mono text-[11px] text-subtle">{p.template}</div>
              </button>
            ))
          )}
        </div>
      </Popover>
      <InsertVars
        groups={varsFor(api, step)}
        onInsert={(v) => onChange((step.a.only_if ?? '') + v)}
      />
      <IconButton
        label="Remove the condition"
        size="xs"
        onClick={() => onChange('')}
        icon={<X className="size-3.5" />}
      />
    </div>
  )
}

export function AddStep({
  first,
  nested,
  onAdd,
}: {
  first: boolean
  nested?: boolean
  onAdd: (type: string) => void
}) {
  const api = useSteps()
  const pop = usePopover()
  const [filter, setFilter] = useState('')
  const cat = api.cat
  const pr = !!api.trigger?.pr
  const items = cat.actions.filter(
    (d) =>
      (d.needs !== 'pr' || pr) &&
      !(nested && d.name === 'repeat') &&
      (!filter ||
        (d.label + ' ' + d.name + ' ' + (d.doc ?? ''))
          .toLowerCase()
          .includes(filter.toLowerCase())),
  )
  return (
    <>
      <Button
        ref={pop.anchor}
        variant={first ? 'secondary' : 'ghost'}
        size="xs"
        icon={<Plus className="size-3.5" />}
        onClick={pop.toggle}
      >
        {first ? 'choose an action' : 'and then…'}
      </Button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} width={340}>
        <div className="p-1">
          <input
            autoFocus={!matchMedia('(max-width: 639px)').matches}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Find an action…"
            aria-label="Find an action"
            className={clsx(inputClass, 'mb-1 h-8 text-[13px]')}
          />
          <div className="max-h-80 overflow-auto">
            {cat.action_groups.map((g) => {
              const list = items.filter((d) => d.group === g)
              if (!list.length) return null
              return (
                <div key={g}>
                  <div className="px-2 pb-0.5 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                    {g}
                  </div>
                  {list.map((d) => (
                    <button
                      key={d.name}
                      type="button"
                      onClick={() => {
                        onAdd(d.name)
                        setFilter('')
                        pop.close()
                      }}
                      className="block w-full rounded-lg px-2 py-1.5 text-left hover:bg-hover"
                    >
                      <div className="flex items-center gap-1.5 text-[13px] font-medium">
                        {d.label}
                        {d.long && (
                          <span className="rounded bg-sunken px-1 text-[10px] font-normal text-subtle">
                            waits
                          </span>
                        )}
                      </div>
                      {d.doc && (
                        <div className="line-clamp-2 text-xs text-subtle">
                          {d.doc.replace(/`/g, '')}
                        </div>
                      )}
                    </button>
                  ))}
                </div>
              )
            })}
            {items.length === 0 && (
              <div className="px-3 py-4 text-center text-xs text-subtle">Nothing matches</div>
            )}
          </div>
        </div>
      </Popover>
    </>
  )
}
