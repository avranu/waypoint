import { useEffect, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ChevronDown,
  CircleAlert,
  CircleCheck,
  Copy,
  GitBranch,
  History,
  LoaderCircle,
  Network,
  Link2,
  Pencil,
  Plus,
  RefreshCw,
  Send,
  Trash2,
  Webhook as WebhookIcon,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { copyText } from '@/lib/clipboard'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch } from '@/components/ui/input'
import { Badge, Empty } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { Explainer, Tip } from '@/components/ui/help'
import type { Forge, Project, ProjectRepo, Webhook } from '@/lib/types'
import { Steps } from '../settings/common'
import { WebhookDeliveries } from '@/components/WebhookDeliveries'
import { Chips } from '@/pages/assistant/tools/Chips'
import { List, ReadOnlyNote, TabIntro, isOwner } from './shared'

export function IntegrationsTab({ project }: { project: Project }) {
  const owner = isOwner(project)
  return (
    <div className="space-y-8">
      <TabIntro title="Integrations" />
      {!owner && <ReadOnlyNote />}
      <Repos project={project} owner={owner} />
      {owner && <Webhooks project={project} />}
    </div>
  )
}

// ---- repositories ----

/** What PATCH /projects/{key}/repos/{id} changes. */
type RepoPatch = Partial<
  Pick<
    ProjectRepo,
    | 'sync_issues'
    | 'post_comments'
    | 'bots'
    | 'review_gate'
    | 'gate_severity'
    | 'delete_merged_branches'
    | 'auto_merge'
    | 'auto_merge_into'
    | 'auto_merge_bases'
    | 'required_checks'
    | 'review_wait_minutes'
    | 'quiet_minutes'
    | 'external_reviewers'
  >
>

function Repos({ project, owner }: { project: Project; owner: boolean }) {
  const qc = useQueryClient()
  const repos = useQuery(q.repos(project.key)).data ?? []
  const forges = useQuery(q.forges()).data
  const [removing, setRemoving] = useState<ProjectRepo | null>(null)
  const [relinking, setRelinking] = useState<ProjectRepo | null>(null)
  const [rulesOf, setRulesOf] = useState<number | null>(null)
  const invalidate = () => qc.invalidateQueries({ queryKey: ['repos', project.key] })
  const base = `/projects/${enc(project.key)}/repos`

  const setSync = useMutation({
    mutationFn: ({ id, ...patch }: RepoPatch & { id: number }) => api.patch(`${base}/${id}`, patch),
    onMutate: ({ id, ...patch }) =>
      qc.setQueryData<ProjectRepo[]>(['repos', project.key], (l) =>
        l?.map((r) => (r.id === id ? { ...r, ...patch } : r)),
      ),
    onError: toastError,
    onSettled: invalidate,
  })
  const sync = useMutation({
    mutationFn: (id: number) => api.post(`${base}/${id}/sync`),
    onSuccess: () => {
      toast('Sync finished', { kind: 'success' })
      qc.invalidateQueries({ queryKey: ['board', project.key] })
      qc.invalidateQueries({ queryKey: ['backlog', project.key] })
    },
    onError: toastError,
    onSettled: invalidate,
  })
  const remove = useMutation({
    mutationFn: (id: number) => api.del(`${base}/${id}`),
    onError: toastError,
    onSettled: invalidate,
  })
  const installHook = useMutation({
    mutationFn: (id: number) => api.post(`${base}/${id}/hook`),
    onError: toastError,
    onSettled: invalidate,
  })
  // Repos whose webhook Waypoint couldn't add (or that predate it adding
  // them and haven't heard from the forge): those need the manual steps.
  const manual = repos.filter(
    (r) => !r.covered_by && (r.hook_state === 'failed' || (r.hook_state === '' && !r.hook_seen_at)),
  )
  const manualForges = (forges ?? []).filter(
    (f) => manual.some((r) => r.forge_id === f.id) && f.webhook_url,
  )

  return (
    <section className="space-y-3">
      <div className="flex items-center gap-1.5">
        <GitBranch className="size-4 text-muted" />
        <h3 className="text-[13px] font-semibold">Repositories</h3>
        <Tip>
          Pull requests and commits that mention an issue key like <b>{project.key}-12</b> (in the
          title, message or branch) link to it. Waypoint adds the forge's webhook when you link a
          repo. Automations can then move issues when PRs merge. With <b>Bots</b> on, automations
          review and act on its pull requests (see the Automations and Bots tabs). With the{' '}
          <b>Review gate</b> on, a <b>waypoint/review</b> status fails while the bot's findings are
          open; require it in the forge's branch protection. With <b>Merge automatically</b> on,
          every open pull request into the branches it covers is set to merge when ready (not
          drafts, forks, release PRs, or ones labelled do-not-merge or wip); pull requests into
          other branches, an agent's too, merge by hand.
        </Tip>
      </div>

      {repos.length > 0 && (
        <List>
          {repos.map((r) => (
            <div key={r.id} className="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2.5">
              <div className="min-w-0 flex-1 basis-48">
                <div className="truncate font-mono text-[13px]">{r.repo}</div>
                <div className="text-xs text-subtle">
                  {r.forge_name}
                  {r.sync_issues &&
                    (r.last_synced_at
                      ? ` · synced ${relativeTime(r.last_synced_at)}`
                      : ' · not synced yet')}
                </div>
                <HookStatus
                  repo={r}
                  owner={owner}
                  busy={installHook.isPending && installHook.variables === r.id}
                  onInstall={() => installHook.mutate(r.id)}
                />
              </div>
              <label
                className="flex items-center gap-2 text-xs text-muted"
                title="Import the repo's issues and keep them in sync"
              >
                <Switch
                  checked={r.sync_issues}
                  disabled={!owner || setSync.isPending}
                  onChange={(v) => setSync.mutate({ id: r.id, sync_issues: v })}
                  label="Sync issues"
                />
                Sync issues
              </label>
              <label
                className="flex items-center gap-2 text-xs text-muted"
                title="Automations review and act on its pull requests"
              >
                <Switch
                  checked={r.bots !== false}
                  disabled={!owner}
                  onChange={(v) => setSync.mutate({ id: r.id, bots: v })}
                  label="Bots on this repo's pull requests"
                />
                Bots
              </label>
              <div className="flex items-center gap-2 text-xs text-muted">
                <label
                  className="flex items-center gap-2"
                  title="Posts a waypoint/review status on its pull requests that fails while the bot's findings are open. Require it in the forge's branch protection."
                >
                  <Switch
                    checked={!!r.review_gate}
                    disabled={!owner || r.bots === false}
                    onChange={(v) => setSync.mutate({ id: r.id, review_gate: v })}
                    label="Review gate on this repo's pull requests"
                  />
                  Review gate
                </label>
                {r.review_gate && (
                  <Select
                    value={r.gate_severity || 'medium'}
                    disabled={!owner}
                    onChange={(e) =>
                      setSync.mutate({
                        id: r.id,
                        gate_severity: e.target.value as ProjectRepo['gate_severity'],
                      })
                    }
                    aria-label="Findings that block"
                    title="Findings at this severity or worse block the pull request"
                    className="w-36"
                  >
                    <option value="critical">P0 only</option>
                    <option value="high">P1 and above</option>
                    <option value="medium">P2 and above</option>
                    <option value="low">P3 and above</option>
                  </Select>
                )}
              </div>
              <label
                className="flex items-center gap-2 text-xs text-muted"
                title="Deletes the repo's merged branches on the forge once a day, and when a pull request whose work already landed is closed. Never the default branch, protected or release/* branches, a pull request's head or base, or one touched in the last two hours."
              >
                <Switch
                  checked={r.delete_merged_branches !== false}
                  disabled={!owner}
                  onChange={(v) => setSync.mutate({ id: r.id, delete_merged_branches: v })}
                  label="Delete this repo's merged branches on the forge"
                />
                Delete merged branches
              </label>
              <div className="flex items-center gap-2 text-xs text-muted">
                <label
                  className="flex items-center gap-2"
                  title="Sets every open pull request into the chosen branches to merge when it is ready. Not drafts, forks, release PRs, or ones labelled do-not-merge or wip."
                >
                  <Switch
                    checked={r.auto_merge}
                    disabled={!owner}
                    onChange={(v) => setSync.mutate({ id: r.id, auto_merge: v })}
                    label="Merge this repo's pull requests automatically"
                  />
                  Merge automatically
                </label>
                {r.auto_merge && (
                  <AutoMergeInto
                    repo={r}
                    owner={owner}
                    save={(patch) => setSync.mutate({ id: r.id, ...patch })}
                  />
                )}
              </div>
              <button
                type="button"
                aria-expanded={rulesOf === r.id}
                onClick={() => setRulesOf(rulesOf === r.id ? null : r.id)}
                className="flex items-center gap-1 text-xs text-muted hover:text-fg"
              >
                Merge rules
                <ChevronDown
                  className={clsx(
                    'size-3.5 transition-transform',
                    rulesOf === r.id && 'rotate-180',
                  )}
                />
              </button>
              {r.sync_issues && (
                <label
                  className="flex items-center gap-2 text-xs text-muted"
                  title="Posts comments made here to the matching forge issue. Keep it off for public repos and mirrors: everyone could read them."
                >
                  <Switch
                    checked={r.post_comments}
                    disabled={!owner || setSync.isPending}
                    onChange={(v) => setSync.mutate({ id: r.id, post_comments: v })}
                    label="Post Waypoint comments to this repo's issues"
                  />
                  Post comments back
                </label>
              )}
              {owner && (
                <div className="flex items-center gap-1">
                  {r.sync_issues && (
                    <Button
                      size="xs"
                      variant="secondary"
                      icon={
                        <RefreshCw
                          className={clsx(
                            'size-3',
                            sync.isPending && sync.variables === r.id && 'animate-spin',
                          )}
                        />
                      }
                      disabled={sync.isPending}
                      onClick={() => sync.mutate(r.id)}
                    >
                      Sync now
                    </Button>
                  )}
                  <IconButton
                    label={`Relink ${r.repo} to another repo or a new name`}
                    size="xs"
                    onClick={() => setRelinking(r)}
                    icon={<Link2 className="size-3.5" />}
                  />
                  <IconButton
                    label={`Remove ${r.repo}`}
                    size="xs"
                    onClick={() => setRemoving(r)}
                    icon={<Trash2 className="size-3.5" />}
                  />
                </div>
              )}
              {r.auto_merge && r.auto_merge_into === 'match' && (
                <AutoMergeBases
                  repo={r}
                  owner={owner}
                  save={(patch) => setSync.mutate({ id: r.id, ...patch })}
                />
              )}
              {rulesOf === r.id && (
                <MergeRules
                  repo={r}
                  owner={owner}
                  save={(patch) => setSync.mutate({ id: r.id, ...patch })}
                />
              )}
            </div>
          ))}
        </List>
      )}

      {owner &&
        forges &&
        (forges.length === 0 ? (
          <div className="rounded-xl border border-dashed border-line px-4 py-3 text-[13px] text-muted">
            No forge connected yet; an admin adds one in <b className="text-fg">Admin → Forges</b>.
          </div>
        ) : (
          <AddRepo project={project} forges={forges} />
        ))}

      {owner && manualForges.length > 0 && (
        <Explainer id="integrations:manual-hook" label="Adding the webhook by hand">
          <p className="mb-2">
            Where Waypoint couldn't add the webhook, add it on the forge (repo or organisation{' '}
            <b>Settings → Webhooks</b>):
          </p>
          <Steps
            items={[
              <div key="url" className="space-y-1">
                <div>
                  <b>Payload URL</b>:
                </div>
                {manualForges.map((f) => (
                  <CopyLine key={f.id} label={f.name} value={f.webhook_url} />
                ))}
              </div>,
              <>
                Content type <b>application/json</b>.
              </>,
              <>
                <b>Secret</b> (GitLab: <b>Secret token</b>): an admin can show it in{' '}
                <b>Admin → Forges</b>.
              </>,
              <>
                Events: <b>pull requests</b> (GitLab: merge requests), <b>issues</b>,{' '}
                <b>issue comments</b> and <b>pushes</b>.
              </>,
            ]}
          />
        </Explainer>
      )}

      {relinking && (
        <RelinkRepo
          project={project}
          repo={relinking}
          onClose={() => setRelinking(null)}
          onDone={invalidate}
        />
      )}

      <Confirm
        open={!!removing}
        onClose={() => setRemoving(null)}
        onConfirm={() => removing && remove.mutate(removing.id)}
        title={`Disconnect ${removing?.repo}?`}
        body="New pull requests and commits stop linking. Issues already imported, and existing links, stay."
        confirmLabel="Disconnect"
      />
    </section>
  )
}

/**
 * Points a linked repo at another repository on its forge, or at its new name:
 * settings, issue links and pull request history stay with the project (WP-154).
 */
function RelinkRepo({
  project,
  repo: r,
  onClose,
  onDone,
}: {
  project: Project
  repo: ProjectRepo
  onClose: () => void
  onDone: () => void
}) {
  const [to, setTo] = useState('')
  const [force, setForce] = useState(false)
  const path = to.trim().replace(/^\/|\/$/g, '')
  const save = useMutation({
    mutationFn: () =>
      api.post<ProjectRepo>(`/projects/${enc(project.key)}/repos/${r.id}/move`, {
        repo: path,
        relink: true,
        force,
      }),
    onSuccess: (now) => {
      toast(`Linked to ${now.repo}`, { kind: 'success' })
      onDone()
      onClose()
    },
    onError: toastError,
  })
  return (
    <Dialog
      open
      onClose={onClose}
      title={`Relink ${r.repo}`}
      footer={
        <Button
          loading={save.isPending}
          disabled={!/^[\w.-]+(\/[\w.-]+)+$/.test(path)}
          onClick={() => save.mutate()}
        >
          Relink
        </Button>
      }
    >
      <div className="space-y-3 p-4 text-[13px]">
        <p className="text-muted">
          The repo's settings, issue links and pull requests stay in this project under the new
          name. Use it when a repo was renamed before Waypoint noticed, or was deleted and made
          again.
        </p>
        <Input
          value={to}
          onChange={(e) => setTo(e.target.value)}
          placeholder="owner/name"
          className="font-mono text-[13px]"
          aria-label="New repository"
          autoFocus
        />
        <label className="flex items-center gap-2 text-xs text-muted">
          <Switch checked={force} onChange={setForce} label="Take over from other projects" />
          Also when other projects link it
        </label>
      </div>
    </Dialog>
  )
}

/** Branches a repo's release branches usually match, offered first. */
const RELEASE_BRANCHES = ['release/**']

/**
 * Which base branches Merge automatically arms pull requests into (WP-1041):
 * a repo can let its release branches merge by themselves while its default
 * branch (production) stays by hand.
 */
function AutoMergeInto({
  repo: r,
  owner,
  save,
}: {
  repo: ProjectRepo
  owner: boolean
  save: (patch: RepoPatch) => void
}) {
  const def = r.default_branch
  return (
    <Select
      value={r.auto_merge_into || 'any'}
      disabled={!owner}
      onChange={(e) => {
        const into = e.target.value as NonNullable<ProjectRepo['auto_merge_into']>
        // Branches matching nothing would arm nothing: start from the usual
        // release branches, which the field below changes.
        save(
          into === 'match' && !r.auto_merge_bases?.length
            ? { auto_merge_into: into, auto_merge_bases: RELEASE_BRANCHES }
            : { auto_merge_into: into },
        )
      }}
      aria-label="Branches Merge automatically covers"
      title="Pull requests into other branches, an agent's too, merge by hand"
      className="w-52"
    >
      <option value="any">Any branch</option>
      <option value="default">{def ? `Only ${def}` : 'Only the default branch'}</option>
      <option value="others">{def ? `All but ${def}` : 'All but the default branch'}</option>
      <option value="match">Branches matching…</option>
    </Select>
  )
}

/** The branch globs Merge automatically arms pull requests into. */
function AutoMergeBases({
  repo: r,
  owner,
  save,
}: {
  repo: ProjectRepo
  owner: boolean
  save: (patch: RepoPatch) => void
}) {
  const bases = r.auto_merge_bases ?? []
  return (
    <div className="w-full basis-full space-y-1.5 rounded-lg border border-line bg-sunken/40 p-3">
      <div className="text-[13px] font-medium text-fg">Merge automatically into</div>
      {owner ? (
        <Chips
          label="Branches Merge automatically covers"
          value={bases}
          placeholder="release/**"
          onChange={(v) => save({ auto_merge_bases: v })}
        />
      ) : (
        <div className="font-mono text-[13px] text-muted">{bases.join(', ') || 'None'}</div>
      )}
      <div className="text-xs text-subtle">
        Branch names or patterns: <code>*</code> stops at a <code>/</code>, <code>**</code> doesn't.
        {bases.length === 0 && ' With none, nothing merges automatically.'}
      </div>
    </div>
  )
}

/**
 * What every armed merge in a repo waits for, beyond its checks passing: the
 * required checks, and reviews settling (WP-133).
 */
function MergeRules({
  repo: r,
  owner,
  save,
}: {
  repo: ProjectRepo
  owner: boolean
  save: (patch: RepoPatch) => void
}) {
  const list = (
    label: string,
    value: string[],
    placeholder: string,
    key: 'required_checks' | 'external_reviewers',
  ) => (
    <div className="space-y-1.5">
      <div className="text-[13px] font-medium text-fg">{label}</div>
      {owner ? (
        <Chips
          label={label}
          value={value}
          placeholder={placeholder}
          onChange={(v) => save({ [key]: v })}
        />
      ) : (
        <div className="font-mono text-[13px] text-muted">{value.join(', ') || 'None'}</div>
      )}
    </div>
  )
  return (
    <div className="grid w-full basis-full gap-3 rounded-lg border border-line bg-sunken/40 p-3 sm:grid-cols-2">
      {list('Required checks', r.required_checks ?? [], 'local-ci', 'required_checks')}
      {list('External reviewers', r.external_reviewers ?? [], 'coderabbitai', 'external_reviewers')}
      <MinutesField
        label="Review wait"
        hint="How long reviewers get after a push. Reviews settle when every expected reviewer (Waypoint's review rules, the external reviewers) posted on the head, or this has passed."
        value={r.review_wait_minutes}
        disabled={!owner}
        onSave={(n) => save({ review_wait_minutes: n })}
      />
      <MinutesField
        label="Quiet"
        hint="It merges only once nothing new (a review or a comment) was posted for this long."
        value={r.quiet_minutes}
        disabled={!owner}
        onSave={(n) => save({ quiet_minutes: n })}
      />
    </div>
  )
}

function MinutesField({
  label,
  hint,
  value,
  disabled,
  onSave,
}: {
  label: string
  hint: string
  value: number
  disabled: boolean
  onSave: (n: number) => void
}) {
  const [text, setText] = useState(String(value))
  useEffect(() => setText(String(value)), [value])
  const n = Number(text)
  const valid = text.trim() !== '' && Number.isInteger(n) && n >= 0 && n <= 1440
  return (
    <Field label={label} hint={hint} error={valid ? undefined : 'From 0 to 1440 minutes'}>
      <div className="flex items-center gap-2">
        <Input
          type="number"
          inputMode="numeric"
          min={0}
          max={1440}
          value={text}
          disabled={disabled}
          onChange={(e) => setText(e.target.value)}
          onBlur={() => valid && n !== value && onSave(n)}
          onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
          className="w-24"
        />
        <span className="text-xs text-muted">minutes</span>
      </div>
    </Field>
  )
}

/** Whether the forge's webhook for a repo is in place, and getting through. */
function HookStatus({
  repo: r,
  owner,
  busy,
  onInstall,
}: {
  repo: ProjectRepo
  owner: boolean
  busy: boolean
  onInstall: () => void
}) {
  const seen = r.hook_seen_at ? `last delivery ${relativeTime(r.hook_seen_at)}` : null
  const action = (label: string) =>
    owner && (
      <>
        {' '}
        <button
          type="button"
          onClick={onInstall}
          disabled={busy}
          className="font-medium text-accent hover:underline disabled:opacity-60"
        >
          {label}
        </button>
      </>
    )
  // An icon, then text that wraps under itself rather than under the icon.
  const line = (icon: ReactNode, text: ReactNode, tone = 'text-subtle') => (
    <div className={clsx('mt-1 flex items-start gap-1.5 text-xs', tone)}>
      <span className="flex h-4 shrink-0 items-center">{icon}</span>
      <span className="min-w-0">{text}</span>
    </div>
  )
  if (r.covered_by || r.hook_state === 'covered')
    return line(
      <Network className="size-3.5 text-ok" />,
      <>
        Covered by the {r.covered_by ? <b className="font-medium">{r.covered_by}</b> : null}{' '}
        organisation webhook{seen ? ` · ${seen}` : ''}
      </>,
    )
  if (r.hook_state === 'pending' || busy)
    return (
      <div role="status">
        {line(<LoaderCircle className="size-3.5 animate-spin" />, 'Adding the webhook…')}
      </div>
    )
  if (r.hook_state === 'failed')
    return line(
      <CircleAlert className="size-3.5 text-danger" />,
      <>
        <b className="font-medium text-fg">Webhook not added.</b> {r.hook_note}
        {action('Try again')}
      </>,
      'text-muted',
    )
  if (r.hook_state === 'installed')
    return (
      <>
        {line(
          <CircleCheck className="size-3.5 text-ok" />,
          `Webhook added · ${seen ?? 'no deliveries yet'}`,
        )}
        {r.hook_note &&
          line(<CircleAlert className="size-3.5 text-warn" />, r.hook_note, 'text-muted')}
      </>
    )
  // Linked before Waypoint added webhooks itself. A working hook may be an
  // organisation's, and a repo one beside it would double every delivery.
  if (seen) return line(<CircleCheck className="size-3.5 text-ok" />, `Webhook working · ${seen}`)
  return (
    <div className="mt-1 text-xs text-subtle">
      No deliveries from a webhook yet.{action('Add the webhook')}
    </div>
  )
}

function CopyLine({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-center gap-2">
      <span className="shrink-0 font-medium text-fg">{label}</span>
      <code className="min-w-0 flex-1 truncate font-mono">{value}</code>
      <IconButton
        label="Copy"
        size="xs"
        onClick={() => copyText(value).then(() => toast('Copied'), toastError)}
        icon={<Copy className="size-3" />}
      />
    </div>
  )
}

function AddRepo({ project, forges }: { project: Project; forges: Forge[] }) {
  const qc = useQueryClient()
  const [forgeId, setForgeId] = useState(forges[0]?.id ?? 0)
  const [repo, setRepo] = useState('')
  const [syncIssues, setSyncIssues] = useState(false)
  // owner/name, or a GitLab group path like grp/sub/app; never `..`.
  const path = repo.trim().replace(/^\/|\/$/g, '')
  const valid =
    /^[\w.-]+(\/[\w.-]+)+$/.test(path) &&
    !path.split('/').some((seg) => seg === '..' || seg === '.')
  const add = useMutation({
    mutationFn: () =>
      api.post<ProjectRepo>(`/projects/${enc(project.key)}/repos`, {
        forge_id: forgeId,
        repo: repo.trim(),
        sync_issues: syncIssues,
      }),
    onSuccess: (r) => {
      toast(
        syncIssues
          ? `Linked ${r.repo}; adding the webhook and importing issues`
          : `Linked ${r.repo}; adding the webhook`,
        { kind: 'success' },
      )
      setRepo('')
    },
    onError: toastError,
    onSettled: () => qc.invalidateQueries({ queryKey: ['repos', project.key] }),
  })
  return (
    <form
      className="space-y-3 rounded-xl border border-line bg-panel p-3"
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) add.mutate()
      }}
    >
      <div className="flex flex-wrap gap-2">
        {forges.length > 1 && (
          <Select
            value={forgeId}
            onChange={(e) => setForgeId(Number(e.target.value))}
            className="w-40"
            aria-label="Forge"
          >
            {forges.map((f) => (
              <option key={f.id} value={f.id}>
                {f.name}
              </option>
            ))}
          </Select>
        )}
        <Input
          value={repo}
          onChange={(e) => setRepo(e.target.value)}
          placeholder={forges.length === 1 ? `owner/name on ${forges[0].name}` : 'owner/name'}
          className="min-w-0 flex-1 basis-full font-mono text-[13px] sm:basis-48"
          aria-label="Repository"
        />
        <Button
          type="submit"
          icon={<Plus className="size-4" />}
          disabled={!valid}
          loading={add.isPending}
        >
          Link repo
        </Button>
      </div>
      <label className="flex items-center gap-2 text-xs text-muted">
        <Switch checked={syncIssues} onChange={setSyncIssues} label="Import and sync issues" />
        Also sync its issues
      </label>
    </form>
  )
}

// ---- webhooks ----

const EVENT_PRESETS = [
  { value: '*', label: 'Everything' },
  { value: 'issue.*', label: 'Issues' },
  { value: 'external.*', label: 'Pull requests' },
  { value: 'sprint.*', label: 'Sprints' },
]

function Webhooks({ project }: { project: Project }) {
  const qc = useQueryClient()
  const hooks = useQuery(q.webhooks(project.key)).data ?? []
  const [editing, setEditing] = useState<Webhook | 'new' | null>(null)
  const [deleting, setDeleting] = useState<Webhook | null>(null)
  const [history, setHistory] = useState<Webhook | null>(null)
  const [results, setResults] = useState<
    Record<number, { ok: boolean; status: number; error?: string }>
  >({})
  const invalidate = () => qc.invalidateQueries({ queryKey: ['webhooks', project.key] })

  const toggle = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      api.patch<Webhook>(`/webhooks/${id}`, { enabled }),
    onMutate: ({ id, enabled }) =>
      qc.setQueryData<Webhook[]>(['webhooks', project.key], (l) =>
        l?.map((w) => (w.id === id ? { ...w, enabled } : w)),
      ),
    onError: toastError,
    onSettled: invalidate,
  })
  const test = useMutation({
    mutationFn: (id: number) =>
      api.post<{ ok: boolean; status: number; error?: string }>(`/webhooks/${id}/test`),
    onSuccess: (r, id) => setResults((x) => ({ ...x, [id]: r })),
    onError: toastError,
    onSettled: invalidate,
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/webhooks/${id}`),
    onError: toastError,
    onSettled: invalidate,
  })

  return (
    <section className="space-y-3">
      <div className="flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5">
          <WebhookIcon className="size-4 text-muted" />
          <h3 className="text-[13px] font-semibold">Webhooks</h3>
          <Tip>
            Waypoint POSTs JSON to these URLs when things happen here. With a secret, each request
            carries <b>X-Waypoint-Signature: sha256=…</b>, an HMAC of the body.
          </Tip>
        </div>
        <Button size="sm" icon={<Plus className="size-3.5" />} onClick={() => setEditing('new')}>
          Add webhook
        </Button>
      </div>

      {hooks.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty title="No webhooks">For chat, home automation or your own scripts.</Empty>
        </div>
      ) : (
        <List>
          {hooks.map((w) => {
            const r = results[w.id]
            const status = r?.status ?? w.last_status
            const error = r ? r.error : w.last_error
            const ok = r ? r.ok : !w.last_error && status !== null && status >= 200 && status < 300
            return (
              <div
                key={w.id}
                className={clsx('space-y-1.5 px-3 py-2.5', !w.enabled && 'opacity-70')}
              >
                <div className="flex items-start gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-1.5 truncate text-[13px] font-medium">
                      {w.name}
                      {w.has_secret && <Badge>signed</Badge>}
                    </div>
                    <div className="truncate font-mono text-xs text-subtle">{w.url}</div>
                  </div>
                  <Switch
                    checked={w.enabled}
                    label={w.enabled ? 'Disable webhook' : 'Enable webhook'}
                    onChange={(enabled) => toggle.mutate({ id: w.id, enabled })}
                  />
                </div>
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-subtle">
                  <span>
                    Events: <code className="font-mono text-muted">{w.events || '*'}</code>
                  </span>
                  {(status !== null || error) && (
                    <span className={ok ? 'text-ok' : 'text-danger'}>
                      {r ? 'Test: ' : 'Last: '}
                      {status ? `HTTP ${status}` : ''}
                      {error && error !== `HTTP ${status}` ? `${status ? ' · ' : ''}${error}` : ''}
                    </span>
                  )}
                  <span className="flex-1" />
                  <span className="-my-1 flex items-center gap-1">
                    <Button
                      size="xs"
                      variant="ghost"
                      icon={<Send className="size-3" />}
                      loading={test.isPending && test.variables === w.id}
                      onClick={() => test.mutate(w.id)}
                    >
                      Test
                    </Button>
                    <IconButton
                      label="Deliveries"
                      size="xs"
                      onClick={() => setHistory(w)}
                      icon={<History className="size-3.5" />}
                    />
                    <IconButton
                      label="Edit"
                      size="xs"
                      onClick={() => setEditing(w)}
                      icon={<Pencil className="size-3.5" />}
                    />
                    <IconButton
                      label="Delete"
                      size="xs"
                      onClick={() => setDeleting(w)}
                      icon={<Trash2 className="size-3.5" />}
                    />
                  </span>
                </div>
              </div>
            )
          })}
        </List>
      )}

      <WebhookDialog project={project} hook={editing} onClose={() => setEditing(null)} />
      <WebhookDeliveries hook={history} onClose={() => setHistory(null)} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        onConfirm={() => deleting && del.mutate(deleting.id)}
        title={`Delete webhook “${deleting?.name}”?`}
      />
    </section>
  )
}

function WebhookDialog({
  project,
  hook,
  onClose,
}: {
  project: Project
  hook: Webhook | 'new' | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const existing = hook && hook !== 'new' ? hook : null
  const [name, setName] = useState('')
  const [url, setUrl] = useState('')
  const [secret, setSecret] = useState('')
  const [clearSecret, setClearSecret] = useState(false)
  const [events, setEvents] = useState('*')
  useEffect(() => {
    if (hook) {
      setName(existing?.name ?? '')
      setUrl(existing?.url ?? '')
      setSecret('')
      setClearSecret(false)
      setEvents(existing?.events || '*')
    }
  }, [hook]) // eslint-disable-line react-hooks/exhaustive-deps
  const validUrl = /^https?:\/\/\S+$/.test(url.trim())
  const save = useMutation({
    mutationFn: () => {
      const body: Record<string, string> = {
        name: name.trim() || url.trim(),
        url: url.trim(),
        events: events.trim() || '*',
      }
      if (secret) body.secret = secret
      else if (clearSecret) body.secret = ''
      return existing
        ? api.patch<Webhook>(`/webhooks/${existing.id}`, body)
        : api.post<Webhook>(`/projects/${enc(project.key)}/webhooks`, body)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['webhooks', project.key] })
      onClose()
    },
    onError: toastError,
  })
  const parts = events
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  const togglePreset = (v: string) => {
    if (v === '*') return setEvents('*')
    const rest = parts.filter((p) => p !== '*')
    setEvents((rest.includes(v) ? rest.filter((p) => p !== v) : [...rest, v]).join(', ') || '*')
  }
  return (
    <Dialog
      open={!!hook}
      onClose={onClose}
      title={existing ? 'Edit webhook' : 'Add webhook'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!validUrl}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            {existing ? 'Save' : 'Add webhook'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (validUrl) save.mutate()
        }}
      >
        <Field
          label="URL"
          error={url && !validUrl ? 'Must start with http:// or https://' : undefined}
        >
          <Input
            autoFocus
            type="url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://example.com/hooks/waypoint"
          />
        </Field>
        <Field label="Name" hint="Optional. Defaults to the URL.">
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Home Assistant"
          />
        </Field>
        <Field
          label="Secret"
          hint={
            existing?.has_secret && !clearSecret
              ? 'A secret is set. Type a new one to replace it.'
              : 'Optional. Signs each payload with X-Waypoint-Signature: sha256=…'
          }
        >
          <Input
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            placeholder={existing?.has_secret ? '••••••••' : ''}
          />
        </Field>
        {existing?.has_secret && !secret && (
          <label className="-mt-2 flex items-center gap-2 text-xs text-muted">
            <input
              type="checkbox"
              checked={clearSecret}
              onChange={(e) => setClearSecret(e.target.checked)}
            />
            Remove the secret (stop signing)
          </label>
        )}
        <div className="space-y-1.5">
          <Field
            label="Events"
            hint="Comma-separated event kinds like issue.created, issue.status_changed, or a wildcard like issue.* or *."
          >
            <Input
              value={events}
              onChange={(e) => setEvents(e.target.value)}
              className="font-mono text-[13px]"
            />
          </Field>
          <div className="flex flex-wrap gap-1.5">
            {EVENT_PRESETS.map((p) => (
              <button
                key={p.value}
                type="button"
                onClick={() => togglePreset(p.value)}
                className={clsx(
                  'h-7 rounded-full border px-2.5 text-xs transition-colors',
                  parts.includes(p.value)
                    ? 'border-accent bg-accent-soft text-fg'
                    : 'border-line text-muted hover:bg-hover',
                )}
              >
                {p.label}
              </button>
            ))}
          </div>
        </div>
      </form>
    </Dialog>
  )
}
