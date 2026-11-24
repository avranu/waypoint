import { useEffect, useMemo, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Archive,
  Bot,
  Check,
  Columns3,
  FileCog,
  Link2,
  Repeat,
  Server,
  Sparkles,
  TriangleAlert,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { invalidateProject, q } from '@/lib/queries'
import { projectTree } from '@/lib/agents'
import { BACKGROUNDS, PROJECT_COLORS, backgroundCSS, setProjectPreview } from '@/lib/theme'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { useAITask } from '@/lib/ai'
import { Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { ProjectIcon, StatusIcon } from '@/components/issue/icons'
import { EmojiPicker } from '@/components/EmojiPicker'
import { Tip } from '@/components/ui/help'
import type { Project, ProjectSettings } from '@/lib/types'
import { ReadOnlyNote, TabIntro, isOwner } from './shared'
import { LookSection } from './Look'
import { ProjectAutopilotTextSection } from '@/components/AutopilotWording'
import { ProjectStalledJobsSection } from '@/components/StalledJobsSettings'

const KEY_RE = /^[A-Z][A-Z0-9]{1,9}$/

interface Form {
  name: string
  key: string
  description: string
  icon: string
  color: string
  background: string
  parent_id: number | null
  settings: ProjectSettings
}

const fromProject = (p: Project): Form => ({
  name: p.name,
  key: p.key,
  description: p.description,
  icon: p.icon,
  color: p.color,
  background: p.background,
  parent_id: p.parent_id ?? null,
  settings: (({ autopilot_prompt: _p, autopilot_done: _d, stalled_jobs: _s, ...rest }) => rest)(
    p.settings,
  ),
})

export function GeneralTab({ project }: { project: Project }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const owner = isOwner(project)
  const aiEstimates = useAITask('estimate')
  const aiTitles = useAITask('title')
  const aiPrep = useAITask('prep')
  const projects = useQuery(q.projects()).data ?? []
  const parentChoices = useMemo(() => {
    const below = new Set([project.id])
    for (let grew = true; grew;) {
      grew = false
      for (const p of projects)
        if (p.parent_id && below.has(p.parent_id) && !below.has(p.id))
          (below.add(p.id), (grew = true))
    }
    return projectTree(projects).filter((n) => !below.has(n.project.id))
  }, [projects, project.id])
  const [form, setForm] = useState<Form>(() => fromProject(project))
  const initial = useMemo(() => JSON.stringify(fromProject(project)), [project])
  const [base, setBase] = useState(initial)
  if (base !== initial) {
    setBase(initial)
    setForm(fromProject(project))
  }
  const dirty = JSON.stringify(form) !== initial
  useEffect(() => {
    setProjectPreview(project.key, { color: form.color, background: form.background.trim() })
  }, [project.key, form.color, form.background])
  useEffect(() => () => setProjectPreview(project.key, null), [project.key])
  const set = <K extends keyof Form>(k: K, v: Form[K]) => setForm((f) => ({ ...f, [k]: v }))
  const setS = <K extends keyof ProjectSettings>(k: K, v: ProjectSettings[K]) =>
    setForm((f) => ({ ...f, settings: { ...f.settings, [k]: v } }))

  const keyChanged = form.key !== project.key
  const keyError =
    form.key && !KEY_RE.test(form.key)
      ? 'Use 2–10 capital letters or digits, starting with a letter.'
      : undefined
  const nameError = !form.name.trim() ? 'A project needs a name.' : undefined

  const save = useMutation({
    mutationFn: () => {
      const s = form.settings
      return api.patch<Project>(`/projects/${enc(project.key)}`, {
        name: form.name.trim(),
        ...(keyChanged ? { key: form.key } : {}),
        description: form.description,
        icon: form.icon,
        color: form.color,
        background: form.background.trim(),
        ...(form.parent_id !== (project.parent_id ?? null)
          ? { parent: projects.find((p) => p.id === form.parent_id)?.key ?? null }
          : {}),
        settings: {
          mode: s.mode,
          sprint_days: Math.max(1, Math.round(s.sprint_days || 14)),
          velocity_weeks: Math.max(1, Math.round(s.velocity_weeks || 6)),
          default_points: Math.max(0, s.default_points || 0),
          done_visible_days: Math.max(1, Math.round(s.done_visible_days || 14)),
          default_status_id: s.default_status_id,
          auto_estimate: !!s.auto_estimate,
          auto_title: !!s.auto_title,
          auto_prep: !!s.auto_prep,
          auto_relate: !!s.auto_relate,
          audit_prs: !!s.audit_prs,
          ci_runners: !!s.ci_runners,
          agents_paused: !!s.agents_paused,
          no_shared_setup: !!s.no_shared_setup,
          no_recurring_checks: !!s.no_recurring_checks,
        } satisfies ProjectSettings,
      })
    },
    onSuccess: (p) => {
      qc.setQueryData(['project', p.key], p)
      invalidateProject(qc, project.key)
      toast('Settings saved', { kind: 'success' })
      if (p.key !== project.key) {
        invalidateProject(qc, p.key)
        navigate({
          to: '/p/$key/settings',
          params: { key: p.key },
          search: { tab: 'general' },
          replace: true,
        })
      }
    },
    onError: toastError,
  })

  const statuses = [...(project.statuses ?? [])].sort((a, b) => a.position - b.position)
  const customBg = /^https?:\/\//.test(form.background) ? form.background : ''
  const s = form.settings

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault()
        if (dirty && !keyError && !nameError) save.mutate()
      }}
      className="space-y-5 pb-24"
    >
      <TabIntro title="General" />
      {!owner && <ReadOnlyNote />}
      <fieldset disabled={!owner} className="space-y-5">
        <Section title="Identity">
          <div className="space-y-4 p-4">
            <div className="flex items-end gap-3">
              <ProjectIcon icon={form.icon} color={form.color} name={form.name || '?'} size={40} />
              <Field label="Name" error={nameError} className="flex-1">
                <Input value={form.name} onChange={(e) => set('name', e.target.value)} />
              </Field>
              <div className="space-y-1.5">
                <div className="text-[13px] font-medium leading-5">Icon</div>
                <EmojiPicker value={form.icon} onChange={(e) => set('icon', e)} disabled={!owner} />
              </div>
            </div>
            <Field
              label="Key"
              error={keyError}
              hint={`The prefix of issue keys, like ${project.key}-12.`}
              note={
                keyChanged &&
                !keyError && (
                  <span className="text-warn">
                    Issues become {form.key}-1, {form.key}-2…; old keys keep working.
                  </span>
                )
              }
            >
              <Input
                value={form.key}
                onChange={(e) =>
                  set(
                    'key',
                    e.target.value
                      .toUpperCase()
                      .replace(/[^A-Z0-9]/g, '')
                      .slice(0, 10),
                  )
                }
                className="max-w-40 font-mono uppercase"
              />
            </Field>
            <Field label="Parent project" hint="Nests this project under another in the sidebar.">
              <Select
                value={form.parent_id ?? ''}
                onChange={(e) => set('parent_id', e.target.value ? Number(e.target.value) : null)}
                className="max-w-72"
              >
                <option value="">None</option>
                {parentChoices.map(({ project: p, depth }) => (
                  <option key={p.id} value={p.id}>
                    {'\u00a0\u00a0'.repeat(depth) + p.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Field label="Description" hint="Shown on the home page.">
              <Textarea
                rows={2}
                autoGrow
                value={form.description}
                onChange={(e) => set('description', e.target.value)}
                placeholder="What is this project for?"
              />
            </Field>
            <div>
              <div className="mb-1.5 text-[13px] font-medium">Color</div>
              <div className="flex flex-wrap gap-2">
                {PROJECT_COLORS.map((c) => (
                  <button
                    key={c}
                    type="button"
                    aria-label={c}
                    onClick={() => set('color', c)}
                    className={clsx(
                      'size-7 rounded-full transition-transform',
                      form.color === c &&
                        'scale-110 ring-2 ring-fg ring-offset-2 ring-offset-panel',
                    )}
                    style={{ background: c }}
                  />
                ))}
              </div>
            </div>
          </div>
        </Section>

        <LookSection project={project} owner={owner} dirty={dirty} />

        <Section title="Background">
          <div className="space-y-3 p-4">
            <div className="grid grid-cols-4 gap-2 sm:grid-cols-8">
              {BACKGROUNDS.map((b) => (
                <button
                  key={b.id || 'none'}
                  type="button"
                  onClick={() => set('background', b.id)}
                  className={clsx(
                    'group flex flex-col items-center gap-1 rounded-lg p-1 text-[11px] text-muted transition-colors hover:bg-hover',
                    form.background === b.id && 'text-fg',
                  )}
                >
                  <span
                    className={clsx(
                      'relative block aspect-[4/3] w-full overflow-hidden rounded-md border bg-panel',
                      form.background === b.id
                        ? 'border-accent ring-2 ring-accent/30'
                        : 'border-line',
                    )}
                  >
                    <span className="absolute inset-0" style={{ background: b.css }} />
                    {form.background === b.id && (
                      <Check
                        className="absolute right-1 top-1 size-3 text-accent"
                        strokeWidth={3}
                      />
                    )}
                  </span>
                  {b.label}
                </button>
              ))}
            </div>
            <Field
              label="Or an image URL"
              hint="Any https:// image, shown behind the project's pages."
            >
              <div className="flex items-center gap-2">
                <Input
                  value={customBg}
                  onChange={(e) => set('background', e.target.value)}
                  placeholder="https://…/photo.jpg"
                  type="url"
                />
                {customBg && (
                  <span
                    className="h-9 w-14 shrink-0 rounded-md border border-line"
                    style={{ background: backgroundCSS(customBg).replace(' fixed', '') }}
                  />
                )}
              </div>
            </Field>
          </div>
        </Section>

        <Section title="How you work">
          <div className="space-y-4 p-4">
            <div className="grid grid-cols-2 gap-2">
              {(
                [
                  ['kanban', Columns3, 'Kanban', 'Continuous flow.'],
                  ['sprints', Repeat, 'Sprints', 'Time-boxed, with velocity.'],
                ] as const
              ).map(([m, Icon, label, desc]) => (
                <button
                  key={m}
                  type="button"
                  onClick={() => setS('mode', m)}
                  className={clsx(
                    'rounded-xl border p-3 text-left transition-colors',
                    s.mode === m ? 'border-accent bg-accent-soft' : 'border-line hover:bg-hover',
                  )}
                >
                  <Icon className="mb-1.5 size-4 text-accent" />
                  <div className="text-[13px] font-medium">{label}</div>
                  <div className="text-xs text-muted">{desc}</div>
                </button>
              ))}
            </div>
            <div className="grid gap-4 sm:grid-cols-2">
              {s.mode === 'sprints' && (
                <Field label="Sprint length" hint="Default length of new sprints.">
                  <NumberInput
                    value={s.sprint_days}
                    onChange={(v) => setS('sprint_days', v)}
                    min={1}
                    max={90}
                    suffix="days"
                  />
                </Field>
              )}
              <Field
                label="Keep finished work on the board"
                hint="Done issues leave the board after this long."
              >
                <NumberInput
                  value={s.done_visible_days}
                  onChange={(v) => setS('done_visible_days', v)}
                  min={1}
                  max={365}
                  suffix="days"
                />
              </Field>
              <Field label="New issues start in" hint="For issues created without a status.">
                <div className="relative">
                  <Select
                    value={String(s.default_status_id || '')}
                    onChange={(e) => setS('default_status_id', Number(e.target.value))}
                    className="pl-8"
                  >
                    {!s.default_status_id && <option value="">Automatic</option>}
                    {statuses.map((st) => (
                      <option key={st.id} value={st.id}>
                        {st.name}
                      </option>
                    ))}
                  </Select>
                  {(() => {
                    const st = statuses.find((x) => x.id === s.default_status_id)
                    return st ? (
                      <StatusIcon
                        category={st.category}
                        color={st.color}
                        className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2"
                      />
                    ) : null
                  })()}
                </div>
              </Field>
            </div>
          </div>
          <div className="flex items-center gap-3 border-t border-line p-4">
            <Bot className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              Agents may take new work here
              <Tip>
                Off pauses every agent in this project: wp next hands out nothing new here, and
                agents finish what they hold. The <b>Agents</b> page shows the work waiting.
              </Tip>
            </div>
            <Switch
              label="Agents may take new work here"
              checked={!s.agents_paused}
              onChange={(v) => setS('agents_paused', !v)}
            />
          </div>
        </Section>

        <Section title="Forecasts">
          <div className="grid gap-4 p-4 sm:grid-cols-2">
            <Field label="Average over the last" hint="Recent weeks used to estimate your pace.">
              <NumberInput
                value={s.velocity_weeks}
                onChange={(v) => setS('velocity_weeks', v)}
                min={1}
                max={52}
                suffix="weeks"
              />
            </Field>
            <Field
              label="Points for unestimated issues"
              hint="0 uses the average of your estimated issues."
            >
              <NumberInput
                value={s.default_points}
                onChange={(v) => setS('default_points', v)}
                min={0}
                max={100}
                step={0.5}
                suffix="pts"
              />
            </Field>
          </div>
          {aiEstimates && (
            <div className="flex items-center gap-3 border-t border-line p-4">
              <Sparkles className="size-4 shrink-0 text-muted" />
              <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
                Estimate issues automatically
                <Tip>
                  The AI estimate model fills in points for new issues, and every hour for open ones
                  still missing them, guided by this project's past estimates. Points someone sets
                  always win.
                </Tip>
              </div>
              <Switch
                label="Estimate issues automatically"
                checked={!!s.auto_estimate}
                onChange={(v) => setS('auto_estimate', v)}
              />
            </div>
          )}
          {aiTitles && (
            <div className="flex items-center gap-3 border-t border-line p-4">
              <Sparkles className="size-4 shrink-0 text-muted" />
              <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
                Write titles for long text automatically
                <Tip>
                  When a new issue's title is a sentence or more, the AI title model writes a short
                  title and the text you typed becomes the description. If the model fails, the text
                  stays as typed.
                </Tip>
              </div>
              <Switch
                label="Write titles for long text automatically"
                checked={!!s.auto_title}
                onChange={(v) => setS('auto_title', v)}
              />
            </div>
          )}
          {aiPrep && (
            <div className="flex items-center gap-3 border-t border-line p-4">
              <Bot className="size-4 shrink-0 text-muted" />
              <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
                Prepare tasks for agents
                <Tip>
                  Before an agent takes one of the next tasks in the queue, a free model reads it
                  with the code a runner gathered from your checkout and points at what the work
                  needs: functions, files, earlier tasks, glossary terms and what the task leaves
                  open. It gives no advice. The agent starts there instead of searching. Agents →
                  Usage compares what tasks cost with and without it.
                </Tip>
              </div>
              <Switch
                label="Prepare tasks for agents"
                checked={!!s.auto_prep}
                onChange={(v) => setS('auto_prep', v)}
              />
            </div>
          )}
          <div className="flex items-center gap-3 border-t border-line p-4">
            <Link2 className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              Relate new issues automatically
              <Tip>
                Links each new issue to open ones its text says it waits for or blocks (“blocked by
                WP-12”, “after WP-12”), ones in this project with the same title, and ones that
                share most of their words. No model is asked.
              </Tip>
            </div>
            <Switch
              label="Relate new issues automatically"
              checked={!!s.auto_relate}
              onChange={(v) => setS('auto_relate', v)}
            />
          </div>
          <div className="flex items-center gap-3 border-t border-line p-4">
            <Bot className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              Audit agents’ pull requests
              <Tip>
                When a pull request by a member of this project opens, and when Waypoint marks it
                ready at a head no audit has read, a model audits it on a runner. Its medium and
                worse findings become babysitting work, like review comments, and critical and high
                ones hold the merge until fixed or closed with a verdict. An audit that fails or
                runs out of plan holds nothing.
              </Tip>
            </div>
            <Switch
              label="Audit agents’ pull requests"
              checked={!!s.audit_prs}
              onChange={(v) => setS('audit_prs', v)}
            />
          </div>
          <div className="flex items-center gap-3 border-t border-line p-4">
            <Server className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              Owns the CI runners
              <Tip>
                When a CI job keeps failing because of its runner (a full disk, Docker gone, DNS),
                Waypoint keeps re-running it and tracks the fault in a task here, one per forge and
                kind of fault. Agents read and report on it; they don’t change a host. Off
                everywhere, the task goes to the failing repo’s own project.
              </Tip>
            </div>
            <Switch
              label="Owns the CI runners"
              checked={!!s.ci_runners}
              onChange={(v) => setS('ci_runners', v)}
            />
          </div>
          <div className="flex items-center gap-3 border-t border-line p-4">
            <FileCog className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              Keep this project’s repos in line
              <Tip>
                Waypoint publishes the shared instructions from Agents → Setup into each of this
                project’s repos through a pull request that merges itself. Off, its repos are still
                read and checked there, but get no shared-instructions pull requests from it.
              </Tip>
            </div>
            <Switch
              label="Keep this project’s repos in line"
              checked={!s.no_shared_setup}
              onChange={(v) => setS('no_shared_setup', !v)}
            />
          </div>
          <div className="flex items-center gap-3 border-t border-line p-4">
            <Repeat className="size-4 shrink-0 text-muted" />
            <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
              File recurring mistakes as checks
              <Tip>
                When the same kind of mistake comes up three times in two weeks, Waypoint files one
                task to make it a check, with the instances listed. At most two tasks a day. Off, it
                files none here.
              </Tip>
            </div>
            <Switch
              label="File recurring mistakes as checks"
              checked={!s.no_recurring_checks}
              onChange={(v) => setS('no_recurring_checks', !v)}
            />
          </div>
        </Section>
      </fieldset>

      <ProjectAutopilotTextSection projectKey={project.key} canEdit={owner} />
      <ProjectStalledJobsSection projectKey={project.key} canEdit={owner} />

      {owner && <ArchiveSection project={project} />}
      {owner && <DangerZone project={project} />}

      {owner && dirty && (
        <div className="pb-safe sticky bottom-3 z-10 flex items-center justify-between gap-3 rounded-xl border border-line bg-raised px-4 py-2.5 shadow-pop animate-up">
          <span className="text-[13px] text-muted">You have unsaved changes</span>
          <div className="flex gap-2">
            <Button variant="ghost" size="sm" onClick={() => setForm(fromProject(project))}>
              Discard
            </Button>
            <Button
              variant="primary"
              size="sm"
              type="submit"
              loading={save.isPending}
              disabled={!!keyError || !!nameError}
            >
              Save changes
            </Button>
          </div>
        </div>
      )}
    </form>
  )
}

function NumberInput({
  value,
  onChange,
  min,
  max,
  step = 1,
  suffix,
}: {
  value: number
  onChange: (v: number) => void
  min: number
  max: number
  step?: number
  suffix: string
}) {
  return (
    <div className="flex items-center gap-2">
      <Input
        type="number"
        inputMode="decimal"
        value={Number.isFinite(value) ? value : ''}
        min={min}
        max={max}
        step={step}
        onChange={(e) => onChange(e.target.value === '' ? NaN : Number(e.target.value))}
        className="w-24 tabular-nums"
      />
      <span className="text-[13px] text-muted">{suffix}</span>
    </div>
  )
}

function ArchiveSection({ project }: { project: Project }) {
  const qc = useQueryClient()
  const archive = useMutation({
    mutationFn: (archived: boolean) =>
      api.patch<Project>(`/projects/${enc(project.key)}`, { archived }),
    onSuccess: (p) => {
      qc.setQueryData(['project', p.key], p)
      qc.invalidateQueries({ queryKey: ['projects'] })
      toast(p.archived ? 'Project archived' : 'Project restored', { kind: 'success' })
    },
    onError: toastError,
  })
  return (
    <Section>
      <div className="flex items-center gap-3 p-4">
        <Archive className="size-4 shrink-0 text-muted" />
        <div className="flex min-w-0 flex-1 items-center gap-1 text-[13px] font-medium">
          Archived
          <Tip>Hidden from the sidebar and home page. Nothing is deleted; restore it any time.</Tip>
        </div>
        <Switch
          label="Archived"
          checked={project.archived}
          disabled={archive.isPending}
          onChange={(v) => archive.mutate(v)}
        />
      </div>
    </Section>
  )
}

function DangerZone({ project }: { project: Project }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState('')
  const del = useMutation({
    mutationFn: () => api.del(`/projects/${enc(project.key)}`),
    onSuccess: () => {
      setOpen(false)
      qc.removeQueries({ queryKey: ['project', project.key] })
      qc.invalidateQueries({ queryKey: ['projects'] })
      toast(`Deleted ${project.name}`)
      navigate({ to: '/' })
    },
    onError: toastError,
  })
  return (
    <section className="rounded-xl border border-danger/40 bg-panel">
      <div className="flex flex-wrap items-center gap-3 p-4">
        <TriangleAlert className="size-4 shrink-0 text-danger" />
        <div className="min-w-0 flex-1">
          <div className="text-[13px] font-medium">Delete this project</div>
          <div className="text-xs text-muted">Removes everything in it, for good.</div>
        </div>
        <Button
          variant="secondary"
          size="sm"
          className="!text-danger"
          onClick={() => {
            setTyped('')
            setOpen(true)
          }}
        >
          Delete project…
        </Button>
      </div>
      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        title={`Delete ${project.name}?`}
        footer={
          <>
            <Button variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button
              variant="danger"
              disabled={typed.trim().toUpperCase() !== project.key}
              loading={del.isPending}
              onClick={() => del.mutate()}
            >
              Delete forever
            </Button>
          </>
        }
      >
        <form
          className="space-y-3 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (typed.trim().toUpperCase() === project.key) del.mutate()
          }}
        >
          <p className="text-[13px] text-muted">
            This deletes {project.open_issues > 0 ? `${project.open_issues} open issues and ` : ''}
            everything else in the project. Consider archiving instead.
          </p>
          <Field
            label={
              <>
                Type <span className="font-mono">{project.key}</span> to confirm
              </>
            }
          >
            <Input
              autoFocus
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              className="font-mono uppercase"
              autoComplete="off"
            />
          </Field>
        </form>
      </Dialog>
    </section>
  )
}
