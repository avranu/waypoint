// The automation editor reads as a sentence: "When [the status changes] from
// [any status] to [Done], only if [type:bug], then [assign it to] [the
// reporter] and [add a comment] …". The triggers, the actions and their
// settings come from the catalog (GET /automation-catalog); short choices
// sit inline as chips, long text opens in a box under its step.

import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { CircleQuestionMark, FlaskConical, Plus, RefreshCw, X } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/misc'
import { RiskNote } from '@/components/Provenance'
import { Tip } from '@/components/ui/help'
import { toastError } from '@/components/ui/toast'
import { QueryHelp } from '@/pages/Search'
import { workflowsQuery } from '@/lib/agents'
import { catalogQuery, type TriggerDef } from '@/lib/automations'
import { runnersQuery } from '@/lib/runners'
import {
  type Automation,
  type AutomationTemplate,
  type PRFilter,
  type Project,
  type Status,
} from '@/lib/types'
import { CopyField } from '@/pages/settings/common'
import {
  addStep,
  autoName,
  blankDraft,
  defaultAction,
  defaultTrigger,
  draftFromAutomation,
  draftFromRule,
  draftProblems,
  dropOn,
  flatten,
  mapSteps,
  maxInputs,
  maxSteps,
  removeStep,
  reorder,
  stepIDs,
  toRule,
  uid,
  wrapStep,
  type DInput,
  type Draft,
} from './automation-draft'
import {
  Clause,
  CronPreview,
  Sentence,
  ParamBox,
  inlineInput,
  leadOf,
  type FieldEnv,
} from './automation-fields'
import { AddStep, StepsProvider, StepsSection, type StepsApi } from './AutomationSteps'
import { TestDialog } from './AutomationRuns'
import { PRFilterClause } from './PRFilterClause'

/** Create or edit an automation. Pass `automation` to edit, or `template` to
 * start a new one from a ready-made rule. Without a `project` it is an admin
 * automation, which belongs to no project. */
export function AutomationEditor({
  project,
  open,
  onClose,
  automation,
  template,
}: {
  project?: Project
  open: boolean
  onClose: () => void
  automation?: Automation | null
  template?: AutomationTemplate | null
}) {
  const admin = !project
  const qc = useQueryClient()
  const cat = useQuery(catalogQuery()).data
  const me = useQuery(q.me()).data
  const [d, setD] = useState<Draft>(blankDraft)
  const [tried, setTried] = useState(false)
  const [help, setHelp] = useState(false)
  const [addingCond, setAddingCond] = useState(false)
  const [testing, setTesting] = useState(false)
  // Saving a new webhook automation keeps the editor open to show its address.
  const [saved, setSaved] = useState<Automation | null>(null)
  const [saveError, setSaveError] = useState('')
  const current = saved ?? automation ?? null
  useEffect(() => {
    if (open) {
      setD(
        automation
          ? draftFromAutomation(automation)
          : template
            ? draftFromRule(`${template.name} (copy)`, true, template.rule)
            : blankDraft(),
      )
      setTried(false)
      setAddingCond(false)
      setSaved(null)
      setSaveError('')
    }
  }, [open, automation, template])
  const statuses = useMemo<Status[]>(
    () => [...(project?.statuses ?? [])].sort((a, b) => a.position - b.position),
    [project?.statuses],
  )
  const workflows = useQuery({ ...workflowsQuery(project?.key ?? ''), enabled: !!project }).data
  const stepNames = useMemo(
    () => [...new Set((workflows ?? []).flatMap((w) => w.steps.map((st) => st.name)))],
    [workflows],
  )

  const tdef = cat?.triggers.find((t) => t.name === d.trigger.event)
  const rule = cat ? toRule(d, cat) : null
  const problems = useMemo(() => (cat ? draftProblems(d, cat) : null), [d, cat])
  const ids = useMemo(() => stepIDs(d.steps), [d.steps])

  const save = useMutation({
    mutationFn: () => {
      const body = {
        name: d.name.trim() || autoName(rule!, statuses, cat!),
        rule,
        enabled: d.enabled,
      }
      if (current) return api.patch<Automation>(`/automations/${current.id}`, body)
      return admin
        ? api.post<Automation>('/admin/automations', body)
        : api.post<Automation>(`/projects/${enc(project.key)}/automations`, body)
    },
    onSuccess: (a) => {
      qc.invalidateQueries({ queryKey: ['automations'] })
      setSaveError('')
      if (!current && a.rule.trigger.event === 'webhook') setSaved(a)
      else onClose()
    },
    onError: (e) => {
      setSaveError(e instanceof Error ? e.message : 'Couldn’t save')
      toastError(e)
    },
  })
  const submit = () => {
    setTried(true)
    if (problems && !problems.any) save.mutate()
  }

  const t = d.trigger
  const setTrigger = (name: string, v: unknown) =>
    setD((x) => ({ ...x, trigger: { ...x.trigger, [name]: v } }))
  const chooseTrigger = (name: string) => {
    const def = cat?.triggers.find((x) => x.name === name)
    setD((x) => ({ ...x, trigger: def ? defaultTrigger(def) : { event: name } }))
  }
  const pr = !!tdef?.pr
  const issue = tdef?.issue ?? true
  const condShown = !pr && issue && (addingCond || d.cond !== '')
  const setPR = (p: Partial<PRFilter>) => setD((x) => ({ ...x, pr: { ...x.pr, ...p } }))

  const env: FieldEnv | null = cat
    ? {
        catalog: cat,
        project,
        statuses,
        stepNames,
        timeZone: d.timeZone,
        openQueryHelp: () => setHelp(true),
      }
    : null

  const stepsApi: StepsApi | null =
    cat && env
      ? {
          cat,
          env,
          trigger: tdef,
          steps: d.steps,
          inputs: d.inputs,
          ids,
          problems: problems?.steps ?? new Map(),
          tried,
          change: (id, fn) => setD((x) => ({ ...x, steps: mapSteps(x.steps, id, fn) })),
          remove: (id) => setD((x) => ({ ...x, steps: removeStep(x.steps, id) })),
          move: (id, by) => setD((x) => ({ ...x, steps: reorder(x.steps, id, (i) => i + by) })),
          dropOn: (a, o) => setD((x) => ({ ...x, steps: dropOn(x.steps, a, o) })),
          add: (parent, type) => {
            const def = cat.actions.find((a) => a.name === type)
            if (!def) return
            setD((x) => ({
              ...x,
              steps: addStep(x.steps, parent, { ...wrapStep(defaultAction(def)) }),
            }))
          },
        }
      : null

  const all = flatten(d.steps)
  const background =
    !!cat &&
    all.some((s) => cat.actions.find((a) => a.name === s.a.type)?.long || s.a.on_error === 'retry')
  const manual = t.event === 'manual'
  const onRunner =
    !!cat && all.some((s) => cat.actions.find((a) => a.name === s.a.type)?.runner === true)

  return (
    <Dialog
      open={open}
      // While query help is open on top, Escape should only close that.
      onClose={help ? () => setHelp(false) : testing ? () => setTesting(false) : onClose}
      wide
      title={
        current
          ? admin
            ? 'Edit admin automation'
            : 'Edit automation'
          : admin
            ? 'New admin automation'
            : 'New automation'
      }
      footer={
        <>
          <label className="mr-auto flex items-center gap-2 text-[13px] text-muted">
            <Switch
              checked={d.enabled}
              onChange={(v) => setD((x) => ({ ...x, enabled: v }))}
              label="Enabled"
            />
            Enabled
          </label>
          <Button
            variant="ghost"
            icon={<FlaskConical className="size-3.5" />}
            disabled={!current}
            title={current ? 'Walk the saved steps without changing anything' : 'Save to test'}
            onClick={() => setTesting(true)}
          >
            {current ? (
              'Test'
            ) : (
              <>
                <span className="sm:hidden">Test</span>
                <span className="hidden sm:inline">Save to test</span>
              </>
            )}
          </Button>
          <Button variant="ghost" onClick={onClose}>
            {saved ? 'Done' : 'Cancel'}
          </Button>
          <Button variant="primary" loading={save.isPending} onClick={submit} disabled={!cat}>
            {current ? 'Save' : 'Create'}
          </Button>
        </>
      }
    >
      {!cat || !env || !stepsApi || !rule || !problems ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-10" />
          <Skeleton className="h-40" />
        </div>
      ) : (
        <form
          className="space-y-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            submit()
          }}
        >
          {current?.risk && (
            <div className="space-y-1" data-testid="rule-agent-note">
              <RiskNote risk={current.risk} />
              <p className="text-xs text-subtle">
                What it sends agents and runs reads as this agent’s until you save the rule
                yourself.
              </p>
            </div>
          )}
          <Field label="Name" hint="Optional. Left blank, it's named after what it does.">
            <Input
              value={d.name}
              onChange={(e) => setD((x) => ({ ...x, name: e.target.value }))}
              placeholder={autoName(rule, statuses, cat)}
            />
          </Field>

          <section
            aria-label="Rule"
            className="space-y-3 rounded-xl border border-line bg-sunken/50 p-3 sm:p-4"
          >
            <Clause word={t.event === 'schedule' ? 'Run' : 'When'}>
              <Select
                variant="inline"
                value={t.event}
                onChange={(e) => chooseTrigger(e.target.value)}
                aria-label="Trigger"
              >
                {triggerOptions(cat.trigger_groups, cat.triggers, admin, t.event)}
              </Select>
              {tdef && <Sentence def={tdef} vals={t} set={setTrigger} env={env} />}
              {tdef && triggerTip(tdef, project?.key)}
            </Clause>
            {tdef && (
              <ParamBox
                def={tdef}
                vals={t}
                set={setTrigger}
                env={env}
                vars={[]}
                extra={
                  t.event === 'schedule' && t.every === 'cron' ? (
                    <CronPreview
                      expr={String(t.cron ?? '')}
                      tz={d.timeZone}
                      fallback={me?.timezone}
                    />
                  ) : t.event === 'webhook' ? (
                    <WebhookAddress automation={current} />
                  ) : undefined
                }
              />
            )}
            {tried && problems.trigger && (
              <p className="text-xs text-danger sm:pl-16">{problems.trigger}</p>
            )}

            {pr ? (
              <PRFilterClause filter={d.pr} event={t.event} set={setPR} />
            ) : condShown ? (
              <Clause word="only if">
                <Input
                  value={d.cond}
                  onChange={(e) => setD((x) => ({ ...x, cond: e.target.value }))}
                  onBlur={() => setAddingCond(false)}
                  autoFocus={addingCond}
                  placeholder="type:bug priority:>=high"
                  className={clsx(inlineInput, 'min-w-0 flex-1 basis-40 font-mono !text-[12px]')}
                  aria-label="Condition"
                />
                <span className="flex">
                  <IconButton
                    label="Query help"
                    size="xs"
                    onClick={() => setHelp(true)}
                    icon={<CircleQuestionMark className="size-3.5" />}
                  />
                  <IconButton
                    label="Remove the condition"
                    size="xs"
                    onClick={() => {
                      setD((x) => ({ ...x, cond: '' }))
                      setAddingCond(false)
                    }}
                    icon={<X className="size-3.5" />}
                  />
                </span>
              </Clause>
            ) : issue ? (
              <div className="sm:pl-16">
                <button
                  type="button"
                  onClick={() => setAddingCond(true)}
                  className="inline-flex items-center gap-1 text-xs text-accent hover:underline"
                >
                  <Plus className="size-3" />
                  Only if…
                </button>
              </div>
            ) : null}

            <StepsProvider value={stepsApi}>
              {d.steps.length === 0 ? (
                <Clause word="then">
                  <AddStep first onAdd={(type) => stepsApi.add(null, type)} />
                  {tried && <span className="text-xs text-danger">Add at least one action.</span>}
                </Clause>
              ) : (
                <>
                  <StepsSection />
                  <div className="sm:pl-16">
                    {d.steps.length < maxSteps ? (
                      <AddStep first={false} onAdd={(type) => stepsApi.add(null, type)} />
                    ) : (
                      <span className="text-xs text-subtle">
                        That’s the most steps there can be.
                      </span>
                    )}
                  </div>
                </>
              )}
            </StepsProvider>
            {problems.tooMany && tried && (
              <p className="text-xs text-danger sm:pl-16">{problems.tooMany}</p>
            )}
            {onRunner && <RunnerNote automation={current} />}
            {background && (
              <p className="text-xs text-subtle sm:pl-16">
                Runs in the background (long steps): a run carries on across restarts, and shows
                under Runs while it waits.
              </p>
            )}
          </section>

          {(manual || d.inputs.length > 0) && (
            <InputsEditor
              inputs={d.inputs}
              problems={problems.inputs}
              tried={tried}
              onChange={(inputs) => setD((x) => ({ ...x, inputs }))}
            />
          )}

          <MoreOptions d={d} setD={setD} manual={manual} />

          {saveError && (
            <p
              role="alert"
              className="rounded-lg bg-danger/10 px-3 py-2 text-[13px] text-danger"
              ref={(el) => el?.scrollIntoView({ block: 'nearest' })}
            >
              {saveError}
            </p>
          )}
        </form>
      )}
      <QueryHelp open={help} onClose={() => setHelp(false)} />
      {current && (
        <TestDialog
          automation={{ ...current, rule: current.rule }}
          open={testing}
          onClose={() => setTesting(false)}
        />
      )}
    </Dialog>
  )
}

/**
 * Whose runners the steps use (WP-D18): the person who last saved the
 * automation, so saving it as yourself moves it onto your runners.
 */
export function RunnerNote({ automation }: { automation: Automation | null }) {
  const me = useQuery(q.me()).data
  const mine = useQuery(runnersQuery()).data
  const saver = automation?.saved_by
  const other = !!automation && !!me && saver?.id !== me.id
  const total = other ? (automation?.runners?.total ?? 0) : (mine?.length ?? 0)
  const online = other
    ? (automation?.runners?.online ?? 0)
    : (mine?.filter((r) => r.online).length ?? 0)
  const who = other ? (saver ? saver.display_name || saver.username : 'nobody') : 'you'
  const problem = total === 0 || online === 0
  return (
    <p
      className={clsx(
        'rounded-lg px-2.5 py-1.5 text-xs sm:ml-16',
        problem ? 'bg-warn/10 text-fg' : 'bg-panel text-subtle',
      )}
      data-testid="runner-note"
    >
      {other ? (
        saver ? (
          <>
            Steps that run on a runner use <b>{who}</b>’s runners (who last saved it); saving it
            yourself moves it to yours.{' '}
          </>
        ) : (
          <>An agent saved this, so it has no runner to use: save it yourself to use yours. </>
        )
      ) : (
        <>
          Steps that run on a runner use <b>your</b> runners, because you save it.{' '}
        </>
      )}
      {saver === null && other ? null : total === 0 ? (
        <>
          {who === 'you' ? 'You have' : `${who} has`} no runner yet: add one under Settings →
          Runners, or these steps fail.
        </>
      ) : online === 0 ? (
        <>
          None of {who === 'you' ? 'your' : 'their'}{' '}
          {total === 1 ? 'runner is' : `${total} runners are`} online, so these steps wait until one
          is.
        </>
      ) : (
        <>
          {online} of {total} {total === 1 ? 'runner is' : 'runners are'} online.
        </>
      )}
    </p>
  )
}

/** The triggers, grouped; project automations don't offer the admin-only ones. */
function triggerOptions(groups: string[], triggers: TriggerDef[], admin: boolean, current: string) {
  const ok = (t: TriggerDef) => t.name === current || admin || !t.admin
  return (
    <>
      {groups.map((g) => {
        const list = triggers.filter((t) => t.group === g && ok(t))
        if (!list.length) return null
        return (
          <optgroup key={g} label={g}>
            {list.map((t) => (
              <option key={t.name} value={t.name}>
                {leadOf(t)}
              </option>
            ))}
          </optgroup>
        )
      })}
      {!triggers.some((t) => t.name === current) && (
        <option value={current}>{current.replace(/_/g, ' ')}</option>
      )}
    </>
  )
}

function triggerTip(def: TriggerDef, projectKey?: string) {
  switch (def.name) {
    case 'status_changed':
      return (
        <Tip>
          A category (like <b>any done status</b>) matches only when an issue enters or leaves it,
          not on moves within it.
        </Tip>
      )
    case 'stale':
      return <Tip>Runs once each time an issue goes quiet; any change restarts the count.</Tip>
    case 'due_soon':
      return <Tip>Runs once per issue and due date; 0 means on the day it’s due.</Tip>
    case 'schedule':
      return (
        <Tip>
          In the time zone under <b>More options</b>, else the project owner’s. With a condition, it
          runs on each matching issue; without one, pair it with <b>create an issue</b> for
          recurring work. A cron expression has five fields: minute, hour, day of month, month, day
          of week.
        </Tip>
      )
    case 'pr_command':
      return (
        <Tip>
          In a comment on a pull request of an attached repo, at the start of a line.{' '}
          <b>@waypoint full review</b> reviews again even at a head already reviewed;{' '}
          <b>@waypoint pause</b>, <b>resume</b> and <b>help</b> always work.
        </Tip>
      )
    case 'pr_updated':
      return (
        <Tip>
          Every pull request of the project’s repos with bots on (Integrations tab), whether or not
          it mentions an issue. A repeat delivery or an edited title doesn’t count; a new push does.
        </Tip>
      )
    case 'pr_opened':
    case 'pr_merged':
    case 'pr_closed':
    case 'linked_issue_closed':
      return (
        <Tip>
          Needs a repository (Integrations tab).
          {projectKey && <> Pull requests that mention {projectKey}-12 link up with that issue.</>}
        </Tip>
      )
  }
  return def.doc ? <Tip>{def.doc.replace(/`/g, '')}</Tip> : null
}

// ---- the webhook trigger's address ----

function WebhookAddress({ automation }: { automation: Automation | null }) {
  const qc = useQueryClient()
  const [url, setUrl] = useState(automation?.hook_url ?? '')
  const [confirm, setConfirm] = useState(false)
  useEffect(() => setUrl(automation?.hook_url ?? ''), [automation?.id, automation?.hook_url])
  const fresh = useMutation({
    mutationFn: () => api.post<Automation>(`/automations/${automation!.id}/webhook-address`),
    onSuccess: (a) => {
      setUrl(a.hook_url ?? '')
      qc.invalidateQueries({ queryKey: ['automations'] })
    },
    onError: toastError,
  })
  if (!automation)
    return (
      <p className="text-xs text-muted">
        The secret address appears here once you save. Anything that can send a request can start
        the automation with it.
      </p>
    )
  const absolute = url && url.startsWith('/') ? window.location.origin + url : url
  return (
    <div className="space-y-1.5">
      <div className="text-[13px] font-medium">Webhook address</div>
      <CopyField value={absolute || 'No address yet'} />
      <p className="text-xs text-muted">
        POST JSON to it; the body becomes <code className="font-mono">{'{{.Trigger.Data}}'}</code>.
        With a signing secret, requests must send{' '}
        <code className="font-mono">X-Hub-Signature-256</code>. The address is the credential:
        anyone who has it can start this automation.
      </p>
      <Button
        size="xs"
        variant="ghost"
        icon={<RefreshCw className="size-3" />}
        loading={fresh.isPending}
        onClick={() => setConfirm(true)}
      >
        Make a new address
      </Button>
      <Confirm
        open={confirm}
        onClose={() => setConfirm(false)}
        onConfirm={() => fresh.mutate()}
        title="Make a new address?"
        body="The old address stops working right away. Update whatever sends requests to it."
        confirmLabel="Make a new address"
      />
    </div>
  )
}

// ---- inputs ----

const INPUT_TYPES: { value: DInput['type']; label: string }[] = [
  { value: 'text', label: 'Text' },
  { value: 'number', label: 'Number' },
  { value: 'bool', label: 'Yes or no' },
  { value: 'select', label: 'A choice' },
]

function InputsEditor({
  inputs,
  problems,
  tried,
  onChange,
}: {
  inputs: DInput[]
  problems: Map<number, string>
  tried: boolean
  onChange: (v: DInput[]) => void
}) {
  const patch = (id: number, p: Partial<DInput>) =>
    onChange(inputs.map((i) => (i.uid === id ? { ...i, ...p } : i)))
  return (
    <section aria-label="Inputs" className="space-y-3 rounded-xl border border-line p-3 sm:p-4">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h3 className="text-[13px] font-semibold">Inputs</h3>
          <p className="text-xs text-muted">
            What <b>Run now</b> asks for. Steps read them as{' '}
            <code className="font-mono">{'{{.Inputs.name}}'}</code>.
          </p>
        </div>
        <Button
          size="xs"
          icon={<Plus className="size-3.5" />}
          disabled={inputs.length >= maxInputs}
          onClick={() =>
            onChange([
              ...inputs,
              {
                uid: uid(),
                name: '',
                label: '',
                type: 'text',
                default: '',
                optionsText: '',
                required: false,
              },
            ])
          }
        >
          Add an input
        </Button>
      </div>
      {inputs.map((i, n) => {
        const error = problems.get(i.uid)
        return (
          <div key={i.uid} className="space-y-2 rounded-lg border border-line bg-panel p-3">
            <div className="grid gap-2 sm:grid-cols-2">
              <Field label="Name">
                <Input
                  value={i.name}
                  onChange={(e) => patch(i.uid, { name: e.target.value.toLowerCase() })}
                  placeholder={`focus_area`}
                  spellCheck={false}
                  className="font-mono text-xs"
                  aria-label={`Input ${n + 1} name`}
                />
              </Field>
              <Field label="Label">
                <Input
                  value={i.label}
                  onChange={(e) => patch(i.uid, { label: e.target.value })}
                  placeholder="What to look at"
                />
              </Field>
              <Field label="Kind">
                <Select
                  value={i.type}
                  onChange={(e) => patch(i.uid, { type: e.target.value as DInput['type'] })}
                >
                  {INPUT_TYPES.map((t) => (
                    <option key={t.value} value={t.value}>
                      {t.label}
                    </option>
                  ))}
                </Select>
              </Field>
              <Field label="Default">
                {i.type === 'bool' ? (
                  <Select
                    value={i.default || 'false'}
                    onChange={(e) => patch(i.uid, { default: e.target.value })}
                  >
                    <option value="false">No</option>
                    <option value="true">Yes</option>
                  </Select>
                ) : (
                  <Input
                    type={i.type === 'number' ? 'number' : 'text'}
                    value={i.default}
                    onChange={(e) => patch(i.uid, { default: e.target.value })}
                  />
                )}
              </Field>
            </div>
            {i.type === 'select' && (
              <Field label="Choices, one per line">
                <Textarea
                  rows={3}
                  autoGrow
                  value={i.optionsText}
                  onChange={(e) => patch(i.uid, { optionsText: e.target.value })}
                  placeholder={'week\nmonth'}
                />
              </Field>
            )}
            <div className="flex items-center justify-between gap-3">
              {i.type === 'bool' ? (
                <span />
              ) : (
                <label className="flex items-center gap-2 text-[13px] text-muted">
                  <Switch
                    checked={i.required}
                    onChange={(v) => patch(i.uid, { required: v })}
                    label="Required"
                  />
                  Required
                </label>
              )}
              <Button
                size="xs"
                variant="ghost"
                icon={<X className="size-3.5" />}
                onClick={() => onChange(inputs.filter((x) => x.uid !== i.uid))}
              >
                Remove
              </Button>
            </div>
            {tried && error && <p className="text-xs text-danger">{error}</p>}
          </div>
        )
      })}
    </section>
  )
}

// ---- rule-wide options ----

const ZONES: string[] = (() => {
  try {
    return Intl.supportedValuesOf('timeZone')
  } catch {
    return []
  }
})()

function MoreOptions({
  d,
  setD,
  manual,
}: {
  d: Draft
  setD: (f: (x: Draft) => Draft) => void
  manual: boolean
}) {
  const [open, setOpen] = useState(!!(d.concurrency || d.timeZone || d.allowAgents))
  useEffect(() => {
    if (d.concurrency || d.timeZone || d.allowAgents) setOpen(true)
  }, [d.concurrency, d.timeZone, d.allowAgents])
  return (
    <div>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        className="text-xs text-accent hover:underline"
      >
        {open ? 'Fewer options' : 'More options'}
      </button>
      {open && (
        <div className="mt-2 grid gap-3 rounded-xl border border-line p-3 sm:grid-cols-2 sm:p-4">
          <Field
            label="If it’s already running"
            hint="What a new firing does while an earlier run of this automation is still going. Schedules skip by default."
          >
            <Select
              value={d.concurrency}
              onChange={(e) =>
                setD((x) => ({ ...x, concurrency: e.target.value as Draft['concurrency'] }))
              }
            >
              <option value="">Default</option>
              <option value="parallel">Run both at once</option>
              <option value="skip">Skip the new one</option>
              <option value="queue">Wait for the earlier one</option>
            </Select>
          </Field>
          <Field
            label="Time zone"
            hint="Where schedules run, as an IANA name like Europe/London. Empty uses the project owner’s."
          >
            <Input
              list="automation-zones"
              value={d.timeZone}
              onChange={(e) => setD((x) => ({ ...x, timeZone: e.target.value }))}
              placeholder="Project owner’s"
              spellCheck={false}
            />
            <datalist id="automation-zones">
              {ZONES.map((z) => (
                <option key={z} value={z} />
              ))}
            </datalist>
          </Field>
          {(manual || d.allowAgents) && (
            <div className="flex items-center justify-between gap-3 sm:col-span-2">
              <div>
                <div className="text-[13px] font-medium">Agents can run it</div>
                <div className="text-xs text-muted">
                  Off by default: only people press Run now. Turn on to let agents run it with{' '}
                  <code className="font-mono">wp automation run</code> or the API.
                </div>
              </div>
              <Switch
                checked={d.allowAgents}
                onChange={(v) => setD((x) => ({ ...x, allowAgents: v }))}
                label="Agents can run it"
              />
            </div>
          )}
        </div>
      )}
    </div>
  )
}
