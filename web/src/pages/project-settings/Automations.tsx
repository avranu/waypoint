import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Copy,
  History,
  Pencil,
  Play,
  Plus,
  Power,
  Sparkles,
  Trash2,
  TriangleAlert,
  Zap,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { adminAutomationsQuery, catalogQuery, type Catalog } from '@/lib/automations'
import { relativeTime, plural } from '@/lib/format'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Switch } from '@/components/ui/input'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { RiskNote } from '@/components/Provenance'
import { toast, toastError } from '@/components/ui/toast'
import type { Action, Automation, AutomationTemplate, Project, Status } from '@/lib/types'
import { AutomationEditor } from './AutomationEditor'
import { RunsDialog, useRunNow } from './AutomationRuns'
import { DescribeRule } from './rules'
import { ReadOnlyNote, TabIntro, canEdit, isOwner } from './shared'

export function AutomationsTab({ project }: { project: Project }) {
  return <AutomationsPanel project={project} />
}

/** The automations of a project, or (without one) the admin automations that belong to none. */
export function AutomationsPanel({ project }: { project?: Project }) {
  const admin = !project
  const owner = admin || isOwner(project)
  const canRun = admin || canEdit(project)
  const qc = useQueryClient()
  const list = useQuery(admin ? adminAutomationsQuery() : q.automations(project.key))
  const meta = useQuery(q.automationTemplates()).data
  const cat = useQuery(catalogQuery()).data
  const listKey = admin ? ['automations', '@admin'] : ['automations', project.key]
  const [editing, setEditing] = useState<Automation | null>(null)
  const [creating, setCreating] = useState(false)
  const [copying, setCopying] = useState<AutomationTemplate | null>(null)
  const [deleting, setDeleting] = useState<Automation | null>(null)
  const [runs, setRuns] = useState<{ a: Automation; initial: number | null } | null>(null)
  const statuses = useMemo(
    () => [...(project?.statuses ?? [])].sort((a, b) => a.position - b.position),
    [project?.statuses],
  )
  const invalidate = () => qc.invalidateQueries({ queryKey: listKey })

  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api.patch<Automation>(`/automations/${id}`, { enabled }),
    onMutate: ({ id, enabled }) =>
      qc.setQueryData<Automation[]>(listKey, (l) =>
        l?.map((a) => (a.id === id ? { ...a, enabled } : a)),
      ),
    onError: toastError,
    onSettled: invalidate,
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/automations/${id}`),
    onError: toastError,
    onSettled: invalidate,
  })
  const addTemplate = useMutation({
    mutationFn: (t: AutomationTemplate) =>
      api.post<Automation>(
        admin ? '/admin/automations' : `/projects/${enc(project.key)}/automations`,
        {
          name: t.name,
          rule: t.rule,
          enabled: !t.off,
        },
      ),
    onSuccess: (a) =>
      toast(a.enabled ? `Turned on “${a.name}”` : `Added “${a.name}”, switched off`, {
        kind: 'success',
      }),
    onError: toastError,
    onSettled: invalidate,
  })

  const automations = list.data ?? []
  const wanted = useSearch({ strict: false }).automation
  const opened = useRef(0)
  useEffect(() => {
    const a =
      wanted && wanted !== opened.current && owner && automations.find((x) => x.id === wanted)
    if (a) {
      opened.current = wanted
      setEditing(a)
    }
  }, [wanted, owner, automations])
  const names = new Set(automations.map((a) => a.name.trim().toLowerCase()))
  const templates = (meta?.templates ?? []).filter(
    (t) => !!t.admin === admin && !names.has(t.name.trim().toLowerCase()),
  )

  return (
    <div className="space-y-6">
      <TabIntro
        title={admin ? 'Admin automations' : 'Automations'}
        actions={
          owner && (
            <Button
              variant="primary"
              size="sm"
              icon={<Plus className="size-3.5" />}
              onClick={() => setCreating(true)}
            >
              {admin ? 'New admin automation' : 'New automation'}
            </Button>
          )
        }
      >
        {admin
          ? 'Rules that belong to no project: they see every project’s events and the system’s (disk, backups, runners), and run steps.'
          : 'When something happens, on a schedule, or when you press Run now, run some steps.'}
      </TabIntro>
      {!owner && <ReadOnlyNote>Only project owners can change automations.</ReadOnlyNote>}

      {list.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
        </div>
      ) : automations.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<Zap />} title="No automations yet">
            Add a ready-made one below, or build your own.
          </Empty>
        </div>
      ) : (
        <div className="space-y-2">
          {automations.map((a) => (
            <AutomationCard
              key={a.id}
              a={a}
              owner={owner}
              canRun={canRun}
              statuses={project ? statuses : []}
              onEdit={() => setEditing(a)}
              onDelete={() => setDeleting(a)}
              onToggle={(enabled) => toggle.mutate({ id: a.id, enabled })}
              onRuns={(initial) => setRuns({ a, initial })}
            />
          ))}
        </div>
      )}

      {owner && templates.length > 0 && (
        <section>
          <div className="mb-2 flex items-center gap-1.5">
            <Sparkles className="size-3.5 text-accent" />
            <h3 className="text-[13px] font-semibold">Ready-made</h3>
          </div>
          <div className="grid gap-2 sm:grid-cols-2">
            {templates.map((t) => (
              <div
                key={t.name}
                className="flex flex-col gap-2 rounded-xl border border-line bg-panel p-3"
              >
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
                    <span className="text-[13px] font-medium">{t.name}</span>
                    {templateTags(t, cat).map((tag) => (
                      <Badge key={tag}>{tag}</Badge>
                    ))}
                  </div>
                  <div className="mt-0.5 text-xs text-muted">{t.description}</div>
                </div>
                <div className="flex items-center gap-1.5">
                  <span className="min-w-0 flex-1 truncate text-[11px] text-subtle">
                    <DescribeRuleShort t={t} />
                  </span>
                  <Button
                    size="xs"
                    variant="ghost"
                    icon={<Copy className="size-3" />}
                    onClick={() => setCopying(t)}
                  >
                    Copy and edit
                  </Button>
                  <Button
                    size="xs"
                    icon={<Power className="size-3" />}
                    loading={addTemplate.isPending && addTemplate.variables?.name === t.name}
                    onClick={() => addTemplate.mutate(t)}
                  >
                    {t.off ? 'Add, off' : 'Turn on'}
                  </Button>
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      <AutomationEditor
        project={project}
        open={creating || !!editing || !!copying}
        automation={editing}
        template={copying}
        onClose={() => {
          setCreating(false)
          setEditing(null)
          setCopying(null)
        }}
      />
      {runs && (
        <RunsDialog
          automation={automations.find((x) => x.id === runs.a.id) ?? runs.a}
          open
          initial={runs.initial}
          canRun={canRun}
          onClose={() => setRuns(null)}
        />
      )}
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        onConfirm={() => deleting && del.mutate(deleting.id)}
        title={`Delete “${deleting?.name}”?`}
        body="It stops running immediately. Anything it already did stays done, and its runs are deleted. To pause it instead, turn it off."
      />
    </div>
  )
}

function AutomationCard({
  a,
  owner,
  canRun,
  statuses,
  onEdit,
  onDelete,
  onToggle,
  onRuns,
}: {
  a: Automation
  owner: boolean
  canRun: boolean
  statuses: Status[]
  onEdit: () => void
  onDelete: () => void
  onToggle: (enabled: boolean) => void
  onRuns: (initial: number | null) => void
}) {
  const manual = a.rule.trigger.event === 'manual'
  const run = useRunNow(a, (id) => onRuns(id))
  return (
    <div
      className={clsx(
        'rounded-xl border border-line bg-panel p-3 transition-opacity sm:p-4',
        !a.enabled && 'opacity-70',
      )}
    >
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <button
              type="button"
              disabled={!owner}
              onClick={onEdit}
              className="text-left text-[13px] font-semibold enabled:hover:underline disabled:cursor-default"
            >
              {a.name}
            </button>
            {manual && a.rule.allow_agents && <Badge>Agents can run it</Badge>}
            {a.durable && <Badge>Runs in the background</Badge>}
          </div>
          <div className="mt-0.5">
            <DescribeRule rule={a.rule} statuses={statuses} />
          </div>
          <RiskNote risk={a.risk} className="mt-1" />
        </div>
        <Switch
          checked={a.enabled}
          disabled={!owner}
          label={a.enabled ? `Disable ${a.name}` : `Enable ${a.name}`}
          onChange={onToggle}
        />
      </div>
      <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-subtle">
        {a.run_count > 0 && <span>Ran {plural(a.run_count, 'time')}</span>}
        {a.last_run_at && <span>Last {relativeTime(a.last_run_at)}</span>}
        {!a.enabled && <span>Off</span>}
        <span className="flex-1" />
        <span className="-my-1 flex items-center gap-1">
          {manual && canRun && (
            <Button
              size="xs"
              variant="secondary"
              icon={<Play className="size-3" />}
              loading={run.pending}
              disabled={!a.enabled}
              title={a.enabled ? undefined : 'Turn it on to run it'}
              onClick={run.start}
            >
              Run now
            </Button>
          )}
          {canRun && (
            <Button
              size="xs"
              variant="ghost"
              icon={<History className="size-3" />}
              onClick={() => onRuns(null)}
            >
              Runs
            </Button>
          )}
          {owner && (
            <>
              <IconButton
                label="Edit"
                size="xs"
                onClick={onEdit}
                icon={<Pencil className="size-3.5" />}
              />
              <IconButton
                label="Delete"
                size="xs"
                onClick={onDelete}
                icon={<Trash2 className="size-3.5" />}
              />
            </>
          )}
        </span>
      </div>
      {a.last_error &&
        (canRun ? (
          <button
            type="button"
            onClick={() => onRuns(null)}
            className="mt-2 flex w-full items-start gap-1.5 rounded-lg bg-danger/10 px-2.5 py-1.5 text-left text-xs text-danger hover:bg-danger/15"
          >
            <TriangleAlert className="mt-px size-3.5 shrink-0" />
            <span className="min-w-0 break-words">Last run failed: {a.last_error}</span>
          </button>
        ) : (
          <div className="mt-2 flex items-start gap-1.5 rounded-lg bg-danger/10 px-2.5 py-1.5 text-xs text-danger">
            <TriangleAlert className="mt-px size-3.5 shrink-0" />
            <span className="min-w-0 break-words">Last run failed: {a.last_error}</span>
          </div>
        ))}
      {run.dialog}
    </div>
  )
}

/** What a ready-made automation needs or does that's worth knowing before turning it on. */
function templateTags(t: AutomationTemplate, cat?: Catalog): string[] {
  const steps: Action[] = []
  const walk = (l: Action[]) => l.forEach((s) => (steps.push(s), s.steps && walk(s.steps)))
  walk(t.rule.actions)
  const tags: string[] = []
  if (steps.some((s) => s.type === 'ask_model')) tags.push('Needs a model')
  if (steps.some((s) => cat?.actions.find((d) => d.name === s.type)?.runner))
    tags.push('Needs a runner')
  if (steps.some((s) => s.type === 'open_pr' || s.type === 'merge_pr' || s.type === 'fix_ci'))
    tags.push('Changes PRs')
  if (t.off) tags.push('Off until you turn it on')
  if (steps.some((s) => cat?.actions.find((d) => d.name === s.type)?.long)) tags.push('Waits')
  if (t.rule.trigger.event === 'manual') tags.push('Run now')
  return tags
}

function DescribeRuleShort({ t }: { t: AutomationTemplate }) {
  const n = t.rule.actions.length
  return (
    <>
      {t.default ? 'Default in new projects · ' : ''}
      {plural(n, 'step')}
    </>
  )
}
