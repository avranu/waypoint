import { useEffect, useMemo, useState, useSyncExternalStore } from 'react'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { FileStack } from 'lucide-react'
import { templatesQuery, type IssueTemplate } from '@/lib/templates'
import { q, useCreateIssue, type IssueInput } from '@/lib/queries'
import { parseQuickAdd, titleTooLong } from '@/lib/quickadd'
import { clearDraft, readDraft, useUnsaved, writeDraft } from '@/lib/drafts'
import { LongTitleHint, SaveError } from '@/components/issue/quickAdd'
import { isDelegated, workflowsQuery } from '@/lib/agents'
import { CategoryPicker, HumansOnlyLock, WorkflowPicker } from '@/components/issue/agentPickers'
import { Dialog } from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Switch, Textarea } from '@/components/ui/input'
import { Picker, Popover, usePopover } from '@/components/ui/popover'
import { Badge } from '@/components/ui/misc'
import { toast } from '@/components/ui/toast'
import { ProjectIcon } from '@/components/issue/icons'
import {
  AssigneePicker,
  DatePicker,
  LabelPicker,
  ParentPicker,
  PointsPicker,
  PriorityPicker,
  SprintPicker,
  StatusPicker,
  TypePicker,
} from '@/components/issue/pickers'
import { PRIORITIES, type IssueType, type Label, type Status, type UserRef } from '@/lib/types'
import { useLocal } from '@/lib/hooks'

type Defaults = Partial<IssueInput>

let state: { open: boolean; defaults: Defaults } = { open: false, defaults: {} }
const subs = new Set<() => void>()
const set = (s: typeof state) => {
  state = s
  subs.forEach((f) => f())
}

export function openCreateIssue(defaults: Defaults = {}) {
  set({ open: true, defaults })
}

export function useCreateIssueDialog() {
  return { open: openCreateIssue }
}

export function CreateIssueDialog() {
  const s = useSyncExternalStore(
    (cb) => {
      subs.add(cb)
      return () => subs.delete(cb)
    },
    () => state,
  )
  if (!s.open) return null
  return <Form defaults={s.defaults} onClose={() => set({ open: false, defaults: {} })} />
}

// What's typed in the dialog is kept until the issue is created (WP-68):
// closing it, a refresh or a crash brings it back next time it opens.
const DRAFT = 'new-issue'
type Draft = { title: string; description: string }

function Form({ defaults, onClose }: { defaults: Defaults; onClose: () => void }) {
  const navigate = useNavigate()
  const pathname = useRouterState({ select: (r) => r.location.pathname })
  const projects = (useQuery(q.projects()).data ?? []).filter(
    (p) => p.role !== 'viewer' && p.role !== 'guest' && !p.archived,
  )
  const users = useQuery(q.users()).data ?? []
  const [lastProject, setLastProject] = useLocal('last-project', '')
  const routeProject = pathname.match(/^\/p\/([^/]+)/)?.[1]
  const [projectKey, setProjectKey] = useState(
    defaults.project ?? routeProject ?? lastProject ?? projects[0]?.key ?? '',
  )
  useEffect(() => {
    if (!projectKey && projects[0]) setProjectKey(projects[0].key)
  }, [projects, projectKey])
  const project = useQuery({ ...q.project(projectKey), enabled: !!projectKey }).data
  const create = useCreateIssue()
  const [more, setMore] = useState(false)

  const [stored] = useState(() =>
    defaults.title || defaults.description ? null : readDraft<Draft>(DRAFT),
  )
  const [restored, setRestored] = useState(!!stored)
  const [text, setText] = useState<Draft>(
    stored ?? { title: defaults.title ?? '', description: defaults.description ?? '' },
  )
  const [kept, setKept] = useState(true)
  const [error, setError] = useState<string | null>(null)
  useUnsaved(!kept && !!(text.title.trim() || text.description.trim()))
  const edit = (patch: Partial<Draft>) => {
    const next = { ...text, ...patch }
    setText(next)
    setKept(writeDraft(DRAFT, next))
    setError(null)
  }
  const { title, description } = text
  const setTitle = (title: string) => edit({ title })
  const setDescription = (description: string) => edit({ description })
  const discard = () => {
    setText({ title: '', description: '' })
    clearDraft(DRAFT)
    setRestored(false)
    setKept(true)
  }
  const close = () => {
    if (kept && (title.trim() || description.trim()))
      toast('Kept what you wrote: it comes back the next time you start a new issue')
    onClose()
  }
  const [type, setType] = useState<IssueType>((defaults.type as IssueType) ?? 'task')
  const [status, setStatus] = useState<Status | null>(null)
  const [priority, setPriority] = useState(
    typeof defaults.priority === 'number' ? defaults.priority : 0,
  )
  const [assignees, setAssignees] = useState<string[]>(defaults.assignees ?? [])
  const [labels, setLabels] = useState<string[]>(defaults.labels ?? [])
  const [points, setPoints] = useState<number | null>(defaults.points ?? null)
  const [due, setDue] = useState<string | null>(defaults.due_date ?? null)
  const [parent, setParent] = useState<string | null>(defaults.parent ?? null)
  const [sprint, setSprint] = useState<string | null>(defaults.sprint ?? null)
  const [template, setTemplate] = useState<IssueTemplate | null>(null)
  const [agent, setAgent] = useState(defaults.agent ?? '')
  const [category, setCategory] = useState(defaults.category ?? '')
  const [workflow, setWorkflow] = useState<string | null>(null)
  const [draft, setDraft] = useState(!!defaults.draft)
  const workflows = useQuery({ ...workflowsQuery(projectKey), enabled: !!projectKey }).data ?? []
  const templates = useQuery({ ...templatesQuery(projectKey), enabled: !!projectKey }).data ?? []
  const tp = usePopover()
  const applyTemplate = (t: IssueTemplate) => {
    setTemplate(t)
    if (t.type) setType(t.type as IssueType)
    edit({
      title: !title.trim() && t.title ? t.title : title,
      description: !description.trim() && t.description ? t.description : description,
    })
    if (t.points != null && points == null) setPoints(t.points)
    if (t.labels.length) setLabels((cur) => [...new Set([...cur, ...t.labels])])
  }

  const defaultStatus =
    project?.statuses?.find((x) => x.id === project.settings.default_status_id) ??
    project?.statuses?.[0]
  const shownStatus =
    status ??
    (defaults.status
      ? project?.statuses?.find(
          (x) => x.name === defaults.status || String(x.id) === defaults.status,
        )
      : undefined) ??
    defaultStatus
  const parsed = useMemo(
    () =>
      parseQuickAdd(
        title,
        users.map((u) => u.username),
      ),
    [title, users],
  )
  const parentQ = useQuery({ ...q.issue(parent ?? ''), enabled: !!parent })

  const submit = () => {
    if (!parsed.title.trim() || !projectKey || create.isPending) return
    const p = parsed.input
    const input: IssueInput = {
      project: projectKey,
      title: parsed.title.trim(),
      description,
      type: p.type ?? type,
      status: shownStatus ? String(shownStatus.id) : undefined,
      priority: p.priority ?? priority,
      assignees: [...new Set([...assignees, ...(p.assignees ?? [])])],
      labels: [...new Set([...labels, ...(p.labels ?? [])])],
      points: p.points ?? points,
      due_date: p.due_date ?? due ?? undefined,
      parent: parent ?? undefined,
      sprint: sprint ?? undefined,
      template: template?.name,
      agent: p.agent ?? (agent || undefined),
      category: p.category ?? (category || undefined),
      workflow: workflow ?? undefined,
      draft: draft || undefined,
    }
    setLastProject(projectKey)
    setError(null)
    create.mutate(input, {
      onSuccess: (d) => {
        clearDraft(DRAFT)
        setRestored(false)
        toast(`Created ${d.draft ? 'draft ' : ''}${d.key}`, {
          href: `/issue/${d.key}`,
          action: {
            label: 'Open',
            run: () => navigate({ to: '/issue/$key', params: { key: d.key } }),
          },
        })
        if (more) setText({ title: '', description: '' })
        else onClose()
      },
      onError: (e) => setError(e instanceof Error ? e.message : String(e)),
    })
  }

  const pp = usePopover()
  const assigneeRefs: UserRef[] = assignees
    .map((n) => users.find((u) => u.username === n))
    .filter(Boolean) as UserRef[]
  const labelRefs: Label[] = labels.map(
    (n, i) =>
      project?.labels?.find((l) => l.name === n) ?? { id: -i - 1, name: n, color: '#64748b' },
  )

  return (
    <Dialog
      open
      onClose={close}
      wide
      title={
        <span className="flex items-center gap-2">
          <button
            ref={pp.anchor}
            onClick={pp.toggle}
            className="flex items-center gap-1.5 rounded-md px-1.5 py-0.5 text-[13px] font-medium hover:bg-hover"
          >
            {project && (
              <ProjectIcon
                icon={project.icon}
                color={project.color}
                name={project.name}
                size={18}
              />
            )}
            {project?.name ?? 'Choose project'}
          </button>
          <span className="text-subtle">›</span>
          <span className="text-[13px] font-normal text-muted">
            {draft ? 'New draft' : 'New issue'}
          </span>
          <Popover open={pp.open} onClose={pp.close} anchor={pp.anchor}>
            <Picker
              items={projects.map((p) => ({
                value: p.key,
                label: p.name,
                hint: p.key,
                icon: <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={18} />,
              }))}
              selected={[projectKey]}
              onClose={pp.close}
              onPick={(k) => {
                setProjectKey(k)
                setStatus(null)
                setParent(null)
                setSprint(null)
                setTemplate(null)
              }}
            />
          </Popover>
        </span>
      }
      footer={
        <>
          <label
            className="flex items-center gap-2 text-[13px] text-muted"
            title="Agents can't see a draft, anywhere, until you publish it"
          >
            <Switch checked={draft} onChange={setDraft} label="Draft" /> Draft
          </label>
          <label className="mr-auto flex items-center gap-2 text-[13px] text-muted">
            <Switch checked={more} onChange={setMore} label="Create more" /> Create more
          </label>
          <Button variant="ghost" onClick={close}>
            Cancel
          </Button>
          <Button
            variant="primary"
            onClick={submit}
            loading={create.isPending}
            disabled={!parsed.title.trim() || !projectKey}
          >
            {draft ? 'Save draft' : 'Create issue'}
          </Button>
        </>
      }
    >
      <div className="space-y-3 px-4 pb-2 pt-4">
        {restored && (title.trim() || description.trim()) && (
          <div className="flex items-center gap-2 rounded-lg bg-sunken px-3 py-2 text-xs text-muted">
            <span className="flex-1">Restored what you were writing.</span>
            <button className="font-medium text-fg hover:underline" onClick={discard}>
              Discard
            </button>
          </div>
        )}
        <input
          autoFocus
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              submit()
            }
          }}
          placeholder="Issue title"
          aria-label="Issue title"
          className="w-full bg-transparent text-lg font-semibold outline-none placeholder:text-subtle"
        />
        <LongTitleHint show={titleTooLong(parsed.title)} className="text-xs text-muted" />
        {parsed.tokens.length > 0 && (
          <div className="flex flex-wrap gap-1">
            {parsed.tokens.map((t, i) => (
              <Badge key={i} color="var(--wp-accent)">
                {t.text}
              </Badge>
            ))}
          </div>
        )}
        <Textarea
          autoGrow
          rows={3}
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
              e.preventDefault()
              submit()
            }
          }}
          placeholder="Add a description… (markdown)"
          className="border-none bg-transparent px-0 focus:ring-0"
        />
        {project && (
          <div className="flex flex-wrap items-center gap-1 border-t border-line pt-3">
            {shownStatus && (
              <StatusPicker
                statuses={project.statuses ?? []}
                value={shownStatus}
                onChange={setStatus}
              />
            )}
            <TypePicker value={(parsed.input.type as IssueType) ?? type} onChange={setType} />
            <PriorityPicker
              value={
                parsed.input.priority
                  ? PRIORITIES.indexOf(parsed.input.priority as (typeof PRIORITIES)[number])
                  : priority
              }
              onChange={setPriority}
            />
            <AssigneePicker
              value={assigneeRefs}
              onChange={setAssignees}
              members={project.members?.map((m) => m.user.id)}
              agent={parsed.input.agent ?? agent}
              onAgent={setAgent}
            />
            <HumansOnlyLock
              value={parsed.input.agent ?? agent}
              onChange={setAgent}
              people={assigneeRefs.length}
              agentOn={isDelegated(parsed.input.agent ?? agent)}
            />
            <PointsPicker value={parsed.input.points ?? points} onChange={setPoints} />
            <DatePicker value={due} onChange={setDue} placeholder="Due" />
            {(project.settings.mode === 'sprints' || (project.sprints?.length ?? 0) > 0) && (
              <SprintPicker
                sprints={project.sprints ?? []}
                value={project.sprints?.find((s) => s.name === sprint)?.id ?? null}
                onChange={setSprint}
              />
            )}
            <ParentPicker
              projectKey={project.key}
              value={parentQ.data ?? null}
              onChange={setParent}
            />
            <LabelPicker labels={project.labels ?? []} value={labelRefs} onChange={setLabels} />
            <CategoryPicker value={parsed.input.category ?? category} onChange={setCategory} />
            {workflows.length > 0 && (
              <WorkflowPicker
                projectKey={project.key}
                workflowId={workflows.find((w) => w.name === workflow)?.id ?? null}
                step={undefined}
                onWorkflow={setWorkflow}
              />
            )}
            {templates.length > 0 && (
              <>
                <button
                  ref={tp.anchor}
                  onClick={tp.toggle}
                  className="inline-flex h-7 items-center gap-1.5 rounded-md border border-dashed border-line-strong px-2 text-xs text-muted hover:bg-hover hover:text-fg"
                >
                  <FileStack className="size-3.5" />
                  {template
                    ? `${template.name}${template.subtasks.length ? ` · ${template.subtasks.length} sub-tasks` : ''}`
                    : 'Template'}
                </button>
                <Popover open={tp.open} onClose={tp.close} anchor={tp.anchor}>
                  <Picker
                    items={templates.map((t) => ({
                      value: t.name,
                      label: t.name,
                      hint: t.subtasks.length ? `${t.subtasks.length} sub-tasks` : undefined,
                    }))}
                    selected={template ? [template.name] : []}
                    onClose={tp.close}
                    placeholder="Template…"
                    onPick={(name) => {
                      const t = templates.find((x) => x.name === name)
                      if (t) applyTemplate(t)
                    }}
                  />
                </Popover>
              </>
            )}
          </div>
        )}
        <SaveError error={error} className="text-xs text-danger" />
        <p className="text-xs text-subtle">
          Tip: type <code>!high</code> <code>@name</code> <code>#tag</code> <code>~3</code>{' '}
          <code>due:fri</code> <code>/bug</code> in the title.
        </p>
      </div>
    </Dialog>
  )
}
