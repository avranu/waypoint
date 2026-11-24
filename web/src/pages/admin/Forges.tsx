import { useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  CircleAlert,
  CircleCheck,
  FolderSync,
  Globe,
  GitBranch,
  KeyRound,
  LoaderCircle,
  MoreHorizontal,
  Network,
  Pencil,
  Plus,
  Trash2,
  User,
  UserCheck,
  X,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import type { DiscoveredRepo, Forge, ForgeKind as Kind, ForgePerson, OrgHook } from '@/lib/types'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { Explainer, Tip } from '@/components/ui/help'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { safeUrl } from '@/lib/format'
import { CopyField, Help, Steps } from '../settings/common'
import { ASK_ABOVE, DiscoverPicker, projectsMade } from './DiscoverRepos'
import { KitSourceSection } from './KitSource'

const NO_REPOS: DiscoveredRepo[] = []

const KIND_LABEL: Record<Kind, string> = { github: 'GitHub', forgejo: 'Forgejo', gitlab: 'GitLab' }
const DEFAULT_URL: Partial<Record<Kind, string>> = {
  github: 'https://github.com',
  gitlab: 'https://gitlab.com',
}

export function ForgesTab() {
  const qc = useQueryClient()
  const forges = useQuery(q.forges())
  // Discovery, and turning on automatic projects, look first: more than a
  // few new projects and the admin picks which repos get one.
  const [picking, setPicking] = useState<{
    forge: Forge
    repos: DiscoveredRepo[]
    enable: boolean
  } | null>(null)
  const attach = useMutation({
    mutationFn: async (v: { forge: Forge; repos: string[]; skip: string[]; enable: boolean }) => {
      const r =
        v.repos.length || v.skip.length
          ? await api.post<{ added: number }>(`/forges/${v.forge.id}/discover`, {
              repos: v.repos,
              skip: v.skip,
            })
          : { added: 0 }
      if (v.enable) await api.patch<Forge>(`/forges/${v.forge.id}`, { auto_projects: true })
      return r
    },
    onSuccess: (r, v) => {
      setPicking(null)
      toast(
        r.added
          ? `Attached ${r.added} new ${r.added === 1 ? 'repository' : 'repositories'} from ${v.forge.name}`
          : v.enable
            ? `Automatic projects are on for ${v.forge.name}`
            : `No new repositories on ${v.forge.name}`,
      )
    },
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['projects'] })
      qc.invalidateQueries({ queryKey: ['forges'] })
    },
  })
  const discover = async (forge: Forge, enable = false) => {
    let repos: DiscoveredRepo[]
    try {
      repos = await qc.fetchQuery(q.discoverPreview(forge.id))
    } catch (e) {
      toastError(e)
      if (enable)
        toast(`Automatic projects stay off for ${forge.name} until its repositories can be listed`)
      return
    }
    const offered = repos.filter((r) => !r.ignored)
    if (projectsMade(offered) > ASK_ABOVE || offered.length < repos.length)
      setPicking({ forge, repos, enable })
    else attach.mutate({ forge, repos: offered.map((r) => r.repo), skip: [], enable })
  }
  const unignore = useMutation({
    mutationFn: async (v: { forge: Forge; repo: string }) => {
      await api.post(`/forges/${v.forge.id}/discover/unignore`, { repo: v.repo })
      return qc.fetchQuery(q.discoverPreview(v.forge.id))
    },
    onSuccess: (repos, v) =>
      setPicking((p) => (p && p.forge.id === v.forge.id ? { ...p, repos } : p)),
    onError: toastError,
  })
  const [editing, setEditing] = useState<Forge | 'new' | null>(null)
  const [deleting, setDeleting] = useState<Forge | null>(null)
  const [secret, setSecret] = useState<{ forge: Forge; secret: string } | null>(null)
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/forges/${id}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['forges'] }),
    onError: toastError,
  })
  const reveal = useMutation({
    mutationFn: (f: Forge) =>
      api
        .get<{ webhook_secret: string }>(`/forges/${f.id}/secret`)
        .then((r) => ({ forge: f, secret: r.webhook_secret })),
    onSuccess: setSecret,
    onError: toastError,
  })
  const list = (forges.data ?? []) as Forge[]
  return (
    <>
      <Section
        title="Forges"
        help={
          <>
            Connect GitHub, GitLab or Forgejo/Gitea so PRs, commits and issues that mention a key
            like <b>WEB-12</b> link up. Projects pick repos in their settings, or turn on{' '}
            <b>automatic projects</b> for one per repo, synced both ways.
          </>
        }
        actions={
          <Button
            size="sm"
            variant="primary"
            icon={<Plus className="size-3.5" />}
            onClick={() => setEditing('new')}
          >
            Connect forge
          </Button>
        }
      >
        {forges.isPending ? (
          <div className="p-4">
            <Skeleton className="h-14" />
          </div>
        ) : list.length === 0 ? (
          <Empty icon={<GitBranch />} title="No forges connected" />
        ) : (
          <ul className="divide-y divide-line">
            {list.map((f) => (
              <ForgeRow
                key={f.id}
                forge={f}
                onEdit={() => setEditing(f)}
                onDelete={() => setDeleting(f)}
                onSecret={() => reveal.mutate(f)}
                onDiscover={() => discover(f)}
              />
            ))}
          </ul>
        )}
      </Section>
      <Explainer id="admin:forge-webhook" label="Forge webhooks">
        <p className="mb-2">
          When a project links a repo, Waypoint adds the repo's webhook itself with the forge's
          token; the project's Integrations tab shows whether it worked. For automatic projects, add
          an organisation webhook under the forge. Set one up by hand only without a token:
        </p>
        <Steps
          items={[
            <>
              Open the repo or org{' '}
              <b className="font-medium text-fg">Settings → Webhooks → Add webhook</b>.
            </>,
            <>
              Set the <b className="font-medium text-fg">Payload URL</b> to the forge's webhook URL
              shown above.
            </>,
            <>
              Content type: <b className="font-medium text-fg">application/json</b>.
            </>,
            <>
              <b className="font-medium text-fg">Secret</b>: use “Show webhook secret” from the
              forge's menu.
            </>,
            <>
              Events: <b className="font-medium text-fg">Pull requests</b>,{' '}
              <b className="font-medium text-fg">Issues</b>,{' '}
              <b className="font-medium text-fg">Issue comments</b> and{' '}
              <b className="font-medium text-fg">Pushes</b>.
            </>,
          ]}
        />
        <p className="mt-2">
          On <b>GitLab</b>: <b>Settings → Webhooks</b>, secret in <b>Secret token</b>, with Push,
          Comments, Issues and Merge request events. Confidential issues aren't imported.
        </p>
      </Explainer>
      {list.length > 0 && <KitSourceSection />}
      <ForgeDialog
        forge={editing}
        onClose={() => setEditing(null)}
        onAutoOn={(f) => discover(f, true)}
      />
      <DiscoverPicker
        forge={picking?.forge ?? null}
        repos={picking?.repos ?? NO_REPOS}
        busy={attach.isPending || unignore.isPending}
        onUnignore={(repo) => picking && unignore.mutate({ forge: picking.forge, repo })}
        onCancel={() => {
          if (picking?.enable) toast(`Automatic projects stay off for ${picking.forge.name}`)
          setPicking(null)
        }}
        onConfirm={(repos, skip) =>
          picking && attach.mutate({ forge: picking.forge, repos, skip, enable: picking.enable })
        }
      />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Disconnect ${deleting?.name}?`}
        body="Repos attached to projects through this forge are detached, and its webhooks will be rejected. Existing links on issues stay."
        confirmLabel="Disconnect"
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
      <Dialog
        open={!!secret}
        onClose={() => setSecret(null)}
        title={`Webhook secret · ${secret?.forge.name ?? ''}`}
        footer={
          <Button variant="primary" onClick={() => setSecret(null)}>
            Done
          </Button>
        }
      >
        {secret && (
          <div className="space-y-3 p-4">
            <Help>
              Paste this into the “Secret” field of the webhook on {KIND_LABEL[secret.forge.kind]}
              {secret.forge.kind === 'gitlab' ? ' (GitLab calls it the “Secret token”)' : ''}.
            </Help>
            <CopyField value={secret.secret} />
          </div>
        )}
      </Dialog>
    </>
  )
}

function ForgeRow({
  forge: f,
  onEdit,
  onDelete,
  onSecret,
  onDiscover,
}: {
  forge: Forge
  onEdit: () => void
  onDelete: () => void
  onSecret: () => void
  onDiscover: () => void
}) {
  const m = usePopover()
  const [addingOrg, setAddingOrg] = useState(false)
  return (
    <li className="space-y-2 px-4 py-3">
      <div className="flex items-center gap-3">
        <GitBranch className="size-4 shrink-0 text-subtle" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="truncate text-[13px] font-medium">{f.name}</span>
            <Badge>{KIND_LABEL[f.kind] ?? f.kind}</Badge>
            {f.auto_projects && <Badge color="var(--wp-accent)">automatic projects</Badge>}
            {(!!f.mirror_private?.length || !!f.mirror_account) && <Badge>mirrored</Badge>}
            {f.has_token ? (
              <Badge color="var(--wp-ok)">token set</Badge>
            ) : (
              <Badge color="var(--wp-warn)">no token</Badge>
            )}
            {f.has_bot_token && <Badge>bot account</Badge>}
          </div>
          <a
            href={safeUrl(f.base_url)}
            target="_blank"
            rel="noreferrer"
            className="mt-0.5 block truncate text-xs text-subtle hover:underline"
          >
            {f.base_url}
          </a>
        </div>
        <IconButton
          ref={m.anchor}
          label={`Actions for ${f.name}`}
          onClick={m.toggle}
          icon={<MoreHorizontal className="size-4" />}
        />
        <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
          <MenuList
            onClose={m.close}
            items={[
              { label: 'Edit', icon: <Pencil />, onSelect: onEdit },
              { label: 'Show webhook secret', icon: <KeyRound />, onSelect: onSecret },
              {
                label: 'Discover repositories',
                icon: <FolderSync />,
                onSelect: onDiscover,
                disabled: !f.has_token,
              },
              {
                label: `Add ${ORG_WORD[f.kind] ?? 'organisation'} webhook`,
                icon: <Network />,
                onSelect: () => setAddingOrg(true),
                disabled: !f.has_token,
              },
              { separator: true, label: '' },
              { label: 'Disconnect…', icon: <Trash2 />, danger: true, onSelect: onDelete },
            ]}
          />
        </Popover>
      </div>
      <div className="sm:pl-7">
        <div className="mb-1 text-xs text-subtle">Webhook URL</div>
        <CopyField value={f.webhook_url} />
      </div>
      <ForgePeople forge={f} />
      <OrgHooks forge={f} adding={addingOrg} setAdding={setAddingOrg} />
    </li>
  )
}

/**
 * The forge's logins that are Waypoint people (WP-279). Anyone else's issues
 * and comments are text from outside: no agent or model sees them until a
 * person approves them. Adding a login lets in what waited from it. Agents
 * can hold a person's forge credentials, so a login's text counts as
 * unreviewed agent text unless only its person writes as it (WP-282). An
 * agent's own account is listed with no person: its text is agent text,
 * never anyone's own (WP-283).
 */
function ForgePeople({ forge: f }: { forge: Forge }) {
  const qc = useQueryClient()
  const people = f.people ?? []
  const users = useQuery(q.users()).data ?? []
  const [adding, setAdding] = useState(false)
  const [login, setLogin] = useState('')
  const [kind, setKind] = useState<'person' | 'agent'>('person')
  const [user, setUser] = useState('')
  const box = useRef<HTMLInputElement>(null)
  const listId = useId()
  const save = useMutation({
    mutationFn: (list: ForgePerson[]) =>
      api.put<ForgePerson[]>(`/forges/${f.id}/people`, {
        people: list.map((p) => ({
          login: p.login,
          user: p.user?.username ?? '',
          agent: !p.user,
          person_only: p.person_only,
        })),
      }),
    onSuccess: () => {
      // The form stays for the next login; Cancel closes it.
      setLogin('')
      box.current?.focus()
    },
    onError: toastError,
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['forges'] })
      qc.invalidateQueries({ queryKey: ['waiting'] })
    },
  })
  const name = login.trim().replace(/^@/, '')
  const who = kind === 'agent' ? null : users.find((u) => u.username === user)
  const ready = !!name && (kind === 'agent' || !!who)
  const taken = people.some((p) => p.login.toLowerCase() === name.toLowerCase())
  return (
    <div className="sm:pl-7">
      <div className="mb-1 flex items-center gap-1 text-xs text-subtle">
        People
        <Tip>
          Logins on {f.name} that are Waypoint people, and agents' own accounts. Issues and comments
          anyone else writes there are text from outside: no agent or model sees them until a person
          approves them. Adding a login lets in what waited from it. The login of the forge's token
          is its connector's. What an agent's account writes is unreviewed agent text, screened
          before agents take it. Agents can hold a person's forge credentials too, so a person's
          login counts the same way unless you mark it as written by its person only.
        </Tip>
      </div>
      {people.length > 0 && (
        <ul className="mb-1.5 flex flex-wrap gap-1.5">
          {people.map((p) => (
            <li
              key={p.login}
              className="inline-flex max-w-full items-center gap-1 rounded-md border border-line bg-panel py-0.5 pl-2 pr-0.5 text-xs"
            >
              <span className="truncate font-mono">{p.login}</span>
              {p.user ? (
                <>
                  <span className="truncate text-subtle">
                    is {p.user.display_name || p.user.username}
                  </span>
                  <ToggleButton
                    label={`Only ${p.user.display_name || p.user.username} writes as ${p.login}`}
                    hint="Off: agents may hold its credentials, so its text counts as unreviewed agent text"
                    pressed={p.person_only}
                    onPressedChange={(only) =>
                      save.mutate(
                        people.map((x) => (x.login === p.login ? { ...x, person_only: only } : x)),
                      )
                    }
                    disabled={save.isPending}
                    icon={<UserCheck className="size-3" />}
                    className="!size-5 !min-w-5"
                  />
                </>
              ) : (
                <span className="inline-flex items-center gap-1 truncate pr-1 text-subtle">
                  <Bot className="size-3 shrink-0" aria-hidden />
                  agent
                </span>
              )}
              <IconButton
                size="xs"
                label={`Stop listing ${p.login}`}
                icon={<X className="size-3" />}
                disabled={save.isPending}
                onClick={() => save.mutate(people.filter((x) => x.login !== p.login))}
              />
            </li>
          ))}
        </ul>
      )}
      {people.length === 0 && !adding && (
        <p className="mb-1 flex items-center gap-1 text-xs text-muted">
          <Globe className="size-3 shrink-0" aria-hidden />
          Nobody yet: everything written on this forge waits for a person's approval.
        </p>
      )}
      {adding ? (
        <form
          className="flex flex-wrap items-center gap-2"
          onSubmit={(e) => {
            e.preventDefault()
            if (ready && !taken)
              save.mutate([...people, { login: name, user: who ?? null, person_only: false }])
          }}
        >
          <Input
            ref={box}
            autoFocus
            value={login}
            onChange={(e) => setLogin(e.target.value)}
            placeholder={`login on ${f.name}`}
            aria-label="Forge login"
            className="h-8 min-w-0 flex-1 basis-32 font-mono text-[13px]"
          />
          <SegmentedControl<'person' | 'agent'>
            label="Whose account"
            value={kind}
            onChange={setKind}
            textFrom="always"
            options={[
              {
                value: 'person',
                label: 'Person',
                icon: <User className="size-3.5" />,
                hint: "a Waypoint person's login",
              },
              {
                value: 'agent',
                label: 'Agent',
                icon: <Bot className="size-3.5" />,
                hint: "an agent's or bot's own account: its text is unreviewed agent text",
              },
            ]}
          />
          {kind === 'person' && (
            <Input
              list={listId}
              value={user}
              onChange={(e) => setUser(e.target.value)}
              placeholder="Waypoint username"
              aria-label="Waypoint person"
              className="h-8 min-w-0 flex-1 basis-32 text-[13px]"
            />
          )}
          <datalist id={listId}>
            {users.map((u) => (
              <option key={u.id} value={u.username}>
                {u.display_name}
              </option>
            ))}
          </datalist>
          <Button size="sm" type="submit" disabled={!ready || taken} loading={save.isPending}>
            Add
          </Button>
          <Button size="sm" variant="ghost" onClick={() => setAdding(false)}>
            Cancel
          </Button>
          {taken && (
            <span className="basis-full text-xs text-subtle">{name} is listed already.</span>
          )}
        </form>
      ) : (
        <button
          type="button"
          onClick={() => setAdding(true)}
          className="inline-flex items-center gap-1 text-xs font-medium text-accent hover:underline"
        >
          <Plus className="size-3" /> Add a login
        </button>
      )}
    </div>
  )
}

const ORG_WORD: Record<Kind, string> = {
  github: 'organisation',
  forgejo: 'organisation',
  gitlab: 'group',
}

/**
 * Webhooks on whole organisations (GitLab: groups): one brings every repo
 * in it, linked or not, which is what automatic projects need.
 */
function OrgHooks({
  forge: f,
  adding,
  setAdding,
}: {
  forge: Forge
  adding: boolean
  setAdding: (v: boolean) => void
}) {
  const qc = useQueryClient()
  const hooks = useQuery(q.orgHooks(f.id)).data ?? []
  const [org, setOrg] = useState('')
  const box = useRef<HTMLInputElement>(null)
  const listId = useId()
  // Asks the forge, so only once someone starts adding one.
  const orgs = useQuery({ ...q.forgeOrgs(f.id), enabled: adding && f.has_token })
  const word = ORG_WORD[f.kind] ?? 'organisation'
  const invalidate = () => qc.invalidateQueries({ queryKey: ['orgHooks', f.id] })
  const add = useMutation({
    mutationFn: (name: string) => api.post<OrgHook>(`/forges/${f.id}/org-hooks`, { org: name }),
    onSuccess: () => {
      setOrg('')
      box.current?.focus()
    },
    onError: toastError,
    onSettled: invalidate,
  })
  const remove = useMutation({
    mutationFn: (h: OrgHook) => api.del(`/forges/${f.id}/org-hooks/${h.id}`),
    onError: toastError,
    onSettled: () => {
      invalidate()
      qc.invalidateQueries({ queryKey: ['repos'] })
    },
  })
  // Shown when there's one, while adding, and for automatic projects
  // (which need one); otherwise "Add … webhook" waits in the forge's menu.
  if (hooks.length === 0 && !adding && !(f.auto_projects && f.has_token)) return null
  const taken = new Set(hooks.map((h) => h.org.toLowerCase()))
  const offered = (orgs.data ?? []).filter((o) => !taken.has(o.toLowerCase()))
  const name = org.trim().replace(/^\/|\/$/g, '')
  const valid = f.kind === 'gitlab' ? /^[\w.-]+(\/[\w.-]+)*$/.test(name) : /^[\w.-]+$/.test(name)
  return (
    <div className="sm:pl-7">
      <div className="mb-1 flex items-center gap-1 text-xs text-subtle">
        {word === 'group' ? 'Group webhooks' : 'Organisation webhooks'}
        <Tip>
          One webhook for every repo in an {word}, including repos nobody has linked yet (what
          automatic projects need). Its repos then get no webhook of their own, so nothing arrives
          twice.
          {f.kind === 'gitlab' && ' GitLab group webhooks need GitLab Premium.'}
        </Tip>
      </div>
      {hooks.length > 0 && (
        <ul className="mb-1.5 space-y-1">
          {hooks.map((h) => (
            <OrgHookRow
              key={h.id}
              hook={h}
              busy={(add.isPending && add.variables === h.org) || remove.isPending}
              onRetry={() => add.mutate(h.org)}
              onRemove={() => remove.mutate(h)}
            />
          ))}
        </ul>
      )}
      {f.has_token &&
        (adding ? (
          <form
            className="flex flex-wrap items-center gap-2"
            onSubmit={(e) => {
              e.preventDefault()
              if (valid) add.mutate(name)
            }}
          >
            <Input
              ref={box}
              autoFocus
              list={listId}
              value={org}
              onChange={(e) => setOrg(e.target.value)}
              placeholder={orgs.isFetching ? `Looking up ${word}s…` : `${word} name`}
              aria-label={`${word} name`}
              className="h-8 min-w-0 flex-1 basis-40 font-mono text-[13px]"
            />
            <datalist id={listId}>
              {offered.map((o) => (
                <option key={o} value={o} />
              ))}
            </datalist>
            <Button size="sm" type="submit" disabled={!valid} loading={add.isPending}>
              Add
            </Button>
            <Button size="sm" variant="ghost" onClick={() => setAdding(false)}>
              Cancel
            </Button>
            {orgs.isError && (
              <span className="basis-full text-xs text-subtle">
                Couldn't list them; type the name.
              </span>
            )}
          </form>
        ) : (
          <button
            type="button"
            onClick={() => setAdding(true)}
            className="inline-flex items-center gap-1 text-xs font-medium text-accent hover:underline"
          >
            <Plus className="size-3" /> Add {word} webhook
          </button>
        ))}
    </div>
  )
}

function OrgHookRow({
  hook: h,
  busy,
  onRetry,
  onRemove,
}: {
  hook: OrgHook
  busy: boolean
  onRetry: () => void
  onRemove: () => void
}) {
  const seen = h.seen_at ? `last delivery ${relativeTime(h.seen_at)}` : 'no deliveries yet'
  let icon: ReactNode, text: ReactNode
  if (h.state === 'pending') {
    icon = <LoaderCircle className="size-3.5 animate-spin text-subtle" />
    text = 'Adding…'
  } else if (h.state === 'failed') {
    icon = <CircleAlert className="size-3.5 text-danger" />
    text = (
      <>
        Not added. {h.note}{' '}
        <button
          type="button"
          onClick={onRetry}
          disabled={busy}
          className="font-medium text-accent hover:underline disabled:opacity-60"
        >
          Try again
        </button>
      </>
    )
  } else {
    icon = <CircleCheck className="size-3.5 text-ok" />
    text = (
      <>
        Added · {seen}
        {h.note && <span className="block text-warn">{h.note}</span>}
      </>
    )
  }
  return (
    <li className="flex items-start gap-2 rounded-lg border border-line px-2.5 py-1.5">
      <div className="min-w-0 flex-1">
        <div className="truncate font-mono text-[13px]">{h.org}</div>
        <div
          className={clsx(
            'mt-0.5 flex items-start gap-1.5 text-xs',
            h.state === 'failed' ? 'text-muted' : 'text-subtle',
          )}
          role={h.state === 'pending' ? 'status' : undefined}
        >
          <span className="flex h-4 shrink-0 items-center">{icon}</span>
          <span className="min-w-0">{text}</span>
        </div>
      </div>
      <IconButton
        label={`Remove the ${h.org} webhook`}
        size="xs"
        disabled={busy}
        onClick={onRemove}
        icon={<X className="size-3.5" />}
      />
    </li>
  )
}

function ForgeDialog({
  forge,
  onClose,
  onAutoOn,
}: {
  forge: Forge | 'new' | null
  onClose: () => void
  /** automatic projects were asked for: look at what they'd make first */
  onAutoOn: (f: Forge) => void
}) {
  const qc = useQueryClient()
  const isNew = forge === 'new'
  const existing = forge && forge !== 'new' ? forge : null
  const [kind, setKind] = useState<Kind>('github')
  const [auto, setAuto] = useState(false)
  const [name, setName] = useState('')
  const [baseURL, setBaseURL] = useState('')
  const [token, setToken] = useState('')
  const [botToken, setBotToken] = useState('')
  const [clearBot, setClearBot] = useState(false)
  const [secret, setSecret] = useState('')
  const [mirror, setMirror] = useState('')
  const [mirrorAccount, setMirrorAccount] = useState('')
  useEffect(() => {
    if (!forge) return
    setKind(existing?.kind ?? 'github')
    setAuto(!!existing?.auto_projects)
    setName(existing?.name ?? '')
    setBaseURL(existing?.base_url ?? '')
    setToken('')
    setBotToken('')
    setClearBot(false)
    setSecret('')
    setMirror((existing?.mirror_private ?? []).join('\n'))
    setMirrorAccount(existing?.mirror_account ?? '')
  }, [forge]) // eslint-disable-line react-hooks/exhaustive-deps
  // Turning automatic projects on with a token: saved off, then on once
  // the admin has seen what discovery would make.
  const turningOn = auto && !existing?.auto_projects && !!(token.trim() || existing?.has_token)
  const mirrorPrivate = mirror.split(/\s+/).filter(Boolean)
  const save = useMutation({
    mutationFn: () => {
      if (existing) {
        const p: Record<string, string | boolean | string[]> = {
          name: name.trim(),
          base_url: baseURL.trim(),
          auto_projects: auto && !turningOn,
          mirror_private: mirrorPrivate,
          mirror_account: mirrorAccount.trim(),
        }
        if (token.trim()) p.token = token.trim()
        if (botToken.trim()) p.bot_token = botToken.trim()
        else if (clearBot) p.bot_token = ''
        if (secret.trim()) p.webhook_secret = secret.trim()
        return api.patch<Forge>(`/forges/${existing.id}`, p)
      }
      return api.post<Forge>('/forges', {
        kind,
        name: name.trim(),
        base_url: baseURL.trim(),
        token: token.trim(),
        bot_token: botToken.trim(),
        webhook_secret: secret.trim(),
        auto_projects: auto && !turningOn,
        mirror_private: mirrorPrivate,
        mirror_account: mirrorAccount.trim(),
      })
    },
    onSuccess: (f) => {
      qc.invalidateQueries({ queryKey: ['forges'] })
      onClose()
      if (turningOn) onAutoOn(f)
    },
    onError: toastError,
  })
  const needsURL = kind === 'forgejo' && !baseURL.trim()
  return (
    <Dialog
      open={!!forge}
      onClose={onClose}
      title={isNew ? 'Connect a forge' : `Edit ${existing?.name ?? 'forge'}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={needsURL}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            {isNew ? 'Connect' : 'Save'}
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (!needsURL) save.mutate()
        }}
      >
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="Kind">
            <Select
              value={kind}
              disabled={!isNew}
              onChange={(e) => setKind(e.target.value as Kind)}
            >
              <option value="github">GitHub</option>
              <option value="gitlab">GitLab</option>
              <option value="forgejo">Forgejo / Gitea</option>
            </Select>
          </Field>
          <Field label="Name" hint="Defaults to the host name.">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={kind === 'forgejo' ? 'git.example.com' : KIND_LABEL[kind]}
            />
          </Field>
        </div>
        <Field
          label="Base URL"
          hint={
            kind === 'github'
              ? 'Leave blank for github.com.'
              : kind === 'gitlab'
                ? 'Leave blank for gitlab.com, or your self-hosted GitLab.'
                : 'Your Forgejo server, e.g. https://git.example.com'
          }
        >
          <Input
            type="url"
            value={baseURL}
            onChange={(e) => setBaseURL(e.target.value)}
            placeholder={DEFAULT_URL[kind] ?? 'https://git.example.com'}
          />
        </Field>
        <Field
          label="Access token"
          hint={
            existing
              ? existing.has_token
                ? 'A token is set. Leave blank to keep it.'
                : 'No token set yet.'
              : kind === 'github'
                ? 'A fine-grained token that can read the repos’ contents, issues and pull requests, and manage webhooks (Webhooks: read and write) so Waypoint adds them itself. Write access to issues too, if projects sync them.'
                : kind === 'gitlab'
                  ? 'A personal or group access token with the api scope, from someone with the Maintainer role, so Waypoint can add webhooks and sync issues.'
                  : 'An access token with write:repository, from a repo admin, so Waypoint can add webhooks (and sync issues and comments).'
          }
        >
          <Input
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
        <Field
          label="Bot token"
          hint={
            existing?.has_bot_token && !clearBot ? (
              <>
                A bot account’s token is set; reviews and bot comments post as that account. Leave
                blank to keep it, or{' '}
                <button
                  type="button"
                  className="text-accent hover:underline"
                  onClick={() => setClearBot(true)}
                >
                  remove it
                </button>
                .
              </>
            ) : (
              'Optional: a separate account for reviews and comments, so people can tell them apart. Without it, the access token posts them.'
            )
          }
        >
          <Input
            type="password"
            value={botToken}
            onChange={(e) => setBotToken(e.target.value)}
            autoComplete="off"
            className="font-mono"
            placeholder={clearBot ? 'Removed when you save' : undefined}
          />
        </Field>
        <Field
          label="Webhook secret"
          hint={
            existing ? 'Leave blank to keep the current secret.' : 'Leave blank to generate one.'
          }
        >
          <Input
            value={secret}
            onChange={(e) => setSecret(e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
        <label className="flex items-start gap-3 rounded-lg border border-line px-3 py-2.5">
          <Switch checked={auto} onChange={setAuto} label="Automatic projects" />
          <span className="text-[13px]">
            <span className="font-medium">Automatic projects</span>
            <span className="block text-xs text-muted">
              Every repository this forge reports (by webhook, or that its token can see) gets a
              project with issues and comments synced, nested under a project for its organisation.
              A repository mirrored on another forge joins the existing project.
            </span>
          </span>
        </label>
        <Field
          label="Its mirror's account"
          hint="Only if a tool mirrors this forge's repositories to another forge and copies the reviews there back here: the login it posts those copies as. Only that account's copies count as the reviewer's, such as a bot's findings. Leave empty when nothing mirrors this forge."
        >
          <Input
            value={mirrorAccount}
            onChange={(e) => setMirrorAccount(e.target.value)}
            autoComplete="off"
            className="font-mono"
          />
        </Field>
        <Field
          label="Kept off its mirror"
          hint="Only if a tool copies this forge's repositories and their reviews to another forge: the paths it keeps private there, one glob per line (** crosses folders). Findings on them stay out of review text it would copy. On Forgejo, a repository's .forgejo/sync.toml private list adds more. Leave empty when nothing mirrors this forge."
        >
          <Textarea
            value={mirror}
            onChange={(e) => setMirror(e.target.value)}
            rows={2}
            className="font-mono"
            placeholder={'**/*.sops.env\n.forgejo/**'}
          />
        </Field>
        <button type="submit" className="hidden" />
      </form>
    </Dialog>
  )
}
