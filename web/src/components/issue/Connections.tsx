import { useEffect, useRef, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ExternalLink as ExtIcon,
  CircleDot,
  CornerDownLeft,
  GitCommit,
  GitPullRequest,
  Link2,
  Network,
  Plus,
  RefreshCw,
  X,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import { invalidateProject, q } from '@/lib/queries'
import { useDebounced } from '@/lib/hooks'
import { Button, IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Avatar, Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { safeUrl } from '@/lib/format'
import { StatusIcon, TypeIcon } from './icons'
import { FindRelated } from './FindRelated'
import { PRBadge } from './IssueCard'
import { relLabel } from '@/lib/relations'
import type { IssueDetail, Relation, RelationKind } from '@/lib/types'

/** What a kind means, for the picker's tooltip (WP-1036). */
const relHint: Partial<Record<RelationKind, string>> = {
  follows: 'More work on what the other did',
  supersedes: "Replaces the other's plan or work: undoing it is intended",
  tension: 'The two pull against each other: whoever takes either should weigh both',
}

/** Kinds a follow-up can be narrowed to (WP-1036). */
const subkinds: Partial<Record<RelationKind, NonNullable<Relation['subkind']>[]>> = {
  follows: ['fix', 'expansion'],
  followed_by: ['fix', 'expansion'],
}
const subkindNote: Record<NonNullable<Relation['subkind']>, string> = {
  fix: 'Mends something the earlier work got wrong or missed',
  expansion: 'Does more of what the earlier work did',
}

/** Who stands behind a relation, for its tooltip. */
function madeBy(r: Relation): string {
  const who = r.created_agent || r.created_by?.display_name || r.created_by?.username
  switch (r.basis) {
    case 'person':
      return who ? `Linked by ${who}` : 'Linked by a person'
    case 'agent':
      return `Linked by an agent${who ? ` (${who})` : ''}`
    case 'automation':
      return `Linked by an automation${who ? ` (${who})` : ''}`
    case 'inferred':
      return r.score != null
        ? `Waypoint found it (${Math.round(r.score * 100)}% sure)`
        : 'Waypoint found it'
  }
  return ''
}

/** How Waypoint knew a follow-up it recorded itself (WP-255). */
const foundNote: Record<NonNullable<Relation['found']>, [string, string]> = {
  session: ['filed during it', 'An agent filed this while working on the other task'],
  files: ['same files', "Its pull request changed mostly the same files as the other's"],
  after: [
    'after it was done',
    'Related to the other task, which was done before this one was filed',
  ],
}

function useRefresh(issue: IssueDetail) {
  const qc = useQueryClient()
  return () => {
    qc.invalidateQueries({ queryKey: ['issue', issue.key] })
    qc.invalidateQueries({ queryKey: ['history', issue.key] })
    invalidateProject(qc, issue.project_key)
  }
}

type Wrap = (actions: ReactNode, body: ReactNode) => ReactNode

/** Relations; the header actions (Find related, Add) go to the caller's section chrome. */
export function RelationsBlock({
  issue,
  editable,
  wrap,
  startAdding = false,
}: {
  issue: IssueDetail
  editable: boolean
  wrap: Wrap
  /** Opened from "+ Relation": the add box shows, focused, straight away. */
  startAdding?: boolean
}) {
  const refresh = useRefresh(issue)
  const [adding, setAdding] = useState(startAdding)
  const remove = (id: number) =>
    api.del(`/issues/${enc(issue.key)}/relations/${id}`).then(refresh, toastError)
  const groups = Object.entries(
    issue.relations.reduce<Record<string, Relation[]>>(
      (acc, r) => ((acc[r.kind] ??= []).push(r), acc),
      {},
    ),
  )
  const actions = (
    <span className="flex items-center gap-0.5">
      <Link
        to="/graph"
        search={{ ref: issue.key }}
        title="Connections: everything that relates to it, nearest first"
        aria-label="Connections map"
        className="inline-flex size-6 items-center justify-center rounded-md text-muted hover:bg-hover hover:text-fg"
      >
        <Network className="size-3.5" />
      </Link>
      {editable && (
        <>
          <FindRelated issueKey={issue.key} onAdded={refresh} />
          <IconButton
            label="Add relation"
            size="xs"
            onClick={() => setAdding(true)}
            icon={<Plus className="size-3.5" />}
          />
        </>
      )}
    </span>
  )
  const body =
    !groups.length && !adding ? null : (
      <div className="space-y-2">
        {groups.map(([kind, rels]) => (
          <div key={kind}>
            <div className="mb-1 text-xs font-medium text-subtle">
              {relLabel[kind as RelationKind] ?? kind.replace(/_/g, ' ')}
            </div>
            {rels.map((r) => (
              <div
                key={r.id}
                title={madeBy(r) || undefined}
                className="group flex h-8 items-center gap-2 rounded-md px-1.5 text-[13px] hover:bg-hover"
              >
                <StatusIcon category={r.state} />
                <TypeIcon type={r.issue.type} />
                <Link to="/issue/$key" params={{ key: r.issue.key }} className="min-w-0 truncate">
                  <span className="text-subtle">{r.issue.key}</span>{' '}
                  <span
                    className={clsx(
                      (r.state === 'done' || r.state === 'canceled') && 'line-through text-muted',
                    )}
                  >
                    {r.issue.title}
                  </span>
                </Link>
                {r.subkind && (
                  <Badge
                    title={subkindNote[r.subkind]}
                    color={r.subkind === 'fix' ? 'var(--wp-warn)' : 'var(--wp-info)'}
                    className="shrink-0"
                  >
                    {r.subkind}
                  </Badge>
                )}
                {r.found && (
                  <Badge title={foundNote[r.found][1]} className="shrink-0">
                    {foundNote[r.found][0]}
                  </Badge>
                )}
                <span className="flex-1" />
                {editable && (
                  <IconButton
                    label="Remove relation"
                    size="xs"
                    className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 max-sm:opacity-100"
                    onClick={() => remove(r.id)}
                    icon={<X className="size-3.5" />}
                  />
                )}
              </div>
            ))}
          </div>
        ))}
        {adding && <AddRelation issue={issue} onAdded={refresh} onClose={() => setAdding(false)} />}
      </div>
    )
  return wrap(actions, body)
}

const KEY = /^[A-Za-z][A-Za-z0-9]*-\d+$/

/** Words that name a relation at the start of the add box, longest first where they share a start. */
const PHRASES: [RegExp, RelationKind, Relation['subkind']?][] = [
  [/^fixed by\b/i, 'followed_by', 'fix'],
  [/^(fix(es)?|mends?)\b/i, 'follows', 'fix'],
  [/^expanded by\b/i, 'followed_by', 'expansion'],
  [/^(expands?|extends?)\b/i, 'follows', 'expansion'],
  [/^(follows( up( on)?)?|follow-?up (of|to|on))\b/i, 'follows'],
  [/^(followed( up)? by|follow-?ups?)\b/i, 'followed_by'],
  [/^blocked by\b/i, 'blocked_by'],
  [/^(blocks|blocking)\b/i, 'blocks'],
  [/^duplicated by\b/i, 'duplicated_by'],
  [/^(duplicates|duplicate of|dup(e|licate)?)\b/i, 'duplicates'],
  [/^(superseded by|replaced by)\b/i, 'superseded_by'],
  [/^(supersedes|replaces)\b/i, 'supersedes'],
  [/^(tension( with)?|conflicts?( with)?|clash(es)?( with)?)\b/i, 'tension'],
  [/^(relates( to)?|related( to)?)\b/i, 'relates'],
]

/**
 * Reads the add box: an optional relation word ("fix", "blocked by",
 * "duplicates", …) then a task key or words to search for.
 */
export function parseRelation(text: string): {
  kind?: RelationKind
  subkind?: Relation['subkind']
  rest: string
} {
  const t = text.trimStart()
  for (const [re, kind, subkind] of PHRASES) {
    const m = re.exec(t)
    if (m && t.length > m[0].length && /[\s:]/.test(t[m[0].length]))
      return { kind, subkind, rest: t.slice(m[0].length).replace(/^[\s:]+/, '') }
  }
  return { rest: t }
}

/**
 * The add box (WP-1196): type "fix WP-12" or "blocked by login page" and
 * press Enter; arrows pick among the matches. It stays open for the next one.
 * The kind can be picked with the mouse too.
 */
function AddRelation({
  issue,
  onAdded,
  onClose,
}: {
  issue: IssueDetail
  onAdded: () => void
  onClose: () => void
}) {
  const [picked, setPicked] = useState<RelationKind>('relates')
  const [pickedSub, setPickedSub] = useState<Relation['subkind'] | ''>('')
  const [text, setText] = useState('')
  const [active, setActive] = useState(0)
  const [moved, setMoved] = useState(false)
  const [waiting, setWaiting] = useState(false)
  const [busy, setBusy] = useState(false)
  const typed = parseRelation(text)
  const kind = typed.kind ?? picked
  const subkind = typed.kind ? (typed.subkind ?? '') : pickedSub
  const query = typed.rest.trim()
  const d = useDebounced(query, 150)
  const linked = new Set([
    issue.key,
    ...issue.relations
      .filter(
        (r) =>
          kind === 'relates' ||
          (r.kind === kind && (!subkind || !subkinds[kind] || r.subkind === subkind)),
      )
      .map((r) => r.issue.key),
  ])
  const search = useQuery({
    ...q.search(d || 'order:-updated', { limit: 6 + linked.size, near: issue.project_key }),
    placeholderData: (prev) => prev,
  })
  const matches = (search.data?.issues ?? []).filter((i) => !linked.has(i.key)).slice(0, 6)
  const at = Math.min(active, Math.max(0, matches.length - 1))
  const answered = d === query && !search.isFetching && !search.isPlaceholderData
  const add = (key: string) => {
    if (busy) return
    setBusy(true)
    api
      .post(`/issues/${enc(issue.key)}/relations`, {
        kind,
        issue: key.toUpperCase(),
        subkind: subkinds[kind] ? subkind : '',
      })
      .then(() => {
        setText(text.slice(0, text.length - typed.rest.length))
        setActive(0)
        setMoved(false)
        onAdded()
      }, toastError)
      .finally(() => setBusy(false))
  }
  useEffect(() => {
    if (!waiting || !answered) return
    setWaiting(false)
    const pick = moved && matches[at] ? matches[at] : matches[0]
    if (pick) add(pick.key)
  }, [waiting, answered])
  const choose = (k: RelationKind) => {
    setPicked(k)
    setPickedSub('')
    if (typed.kind) setText(typed.rest)
  }
  return (
    <div className="space-y-1 pt-1">
      <form
        className="flex flex-wrap items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          if (moved && matches[at]) add(matches[at].key)
          else if (KEY.test(query)) add(query)
          else if (!answered) setWaiting(true)
          else if (matches[at]) add(matches[at].key)
        }}
      >
        <Select
          value={kind}
          onChange={(e) => choose(e.target.value as RelationKind)}
          title={relHint[kind]}
          aria-label="Kind of relation"
          className="!h-8 w-36 text-[13px]"
        >
          {Object.entries(relLabel).map(([k, l]) => (
            <option key={k} value={k} title={relHint[k as RelationKind]}>
              {l}
            </option>
          ))}
        </Select>
        {subkinds[kind] && (
          <Select
            value={subkind}
            onChange={(e) => {
              setPicked(kind)
              setPickedSub(e.target.value as Relation['subkind'] | '')
              if (typed.kind) setText(typed.rest)
            }}
            title={subkind ? subkindNote[subkind] : undefined}
            aria-label="Kind of follow-up"
            className="!h-8 w-28 text-[13px]"
          >
            <option value="">Any</option>
            {subkinds[kind].map((k) => (
              <option key={k} value={k} title={subkindNote[k]}>
                {k === 'fix' ? 'Fix' : 'Expansion'}
              </option>
            ))}
          </Select>
        )}
        <Input
          autoFocus
          value={text}
          readOnly={busy}
          onChange={(e) => {
            setText(e.target.value)
            setActive(0)
            setMoved(false)
            setWaiting(false)
          }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown' && matches.length) {
              e.preventDefault()
              setActive((at + 1) % matches.length)
              setMoved(true)
            } else if (e.key === 'ArrowUp' && matches.length) {
              e.preventDefault()
              setActive((at - 1 + matches.length) % matches.length)
              setMoved(true)
            } else if (e.key === 'Escape') {
              e.preventDefault()
              onClose()
            }
          }}
          aria-label="Task to relate"
          role="combobox"
          aria-autocomplete="list"
          aria-expanded={matches.length > 0}
          aria-controls={`relate-${issue.key}`}
          aria-activedescendant={matches[at] ? `relate-${issue.key}-${matches[at].key}` : undefined}
          placeholder="fix WP-12, blocked by …, or search"
          title="Start with a relation (fix, expands, blocks, blocked by, duplicates, replaces, tension, related) or pick one on the left, then a key or words; Enter adds it"
          className="!h-8 min-w-48 flex-1 text-[13px]"
        />
        <IconButton
          label="Done adding"
          size="xs"
          onClick={onClose}
          icon={<X className="size-3.5" />}
        />
      </form>
      {matches.length > 0 && (
        <ul
          id={`relate-${issue.key}`}
          role="listbox"
          aria-label="Matching tasks"
          className={clsx(
            'overflow-hidden rounded-lg border border-line',
            (text !== '' && d !== query) || search.isPlaceholderData ? 'opacity-60' : '',
          )}
        >
          {matches.map((i, n) => (
            <li
              key={i.key}
              id={`relate-${issue.key}-${i.key}`}
              role="option"
              aria-selected={n === at}
              onMouseEnter={() => setActive(n)}
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => add(i.key)}
              className={clsx(
                'flex h-8 cursor-pointer items-center gap-2 px-2 text-[13px]',
                n === at && 'bg-hover',
              )}
            >
              <StatusIcon category={i.status.category} color={i.status.color} />
              <TypeIcon type={i.type} />
              <span className="min-w-0 flex-1 truncate">
                <span className="text-subtle">{i.key}</span> {i.title}
              </span>
              {n === at && <CornerDownLeft className="size-3 shrink-0 text-subtle" />}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function linkIcon(kind: string) {
  if (kind === 'pull') return <GitPullRequest className="size-3.5" />
  if (kind === 'commit') return <GitCommit className="size-3.5" />
  if (kind === 'issue') return <CircleDot className="size-3.5" />
  return <Link2 className="size-3.5" />
}

export function LinksBlock({
  issue,
  editable,
  wrap,
  startAdding = false,
}: {
  issue: IssueDetail
  editable: boolean
  wrap: Wrap
  /** Opened from "+ Link": the URL box shows, focused, straight away. */
  startAdding?: boolean
}) {
  const refresh = useRefresh(issue)
  const [url, setUrl] = useState('')
  const [adding, setAdding] = useState(startAdding)
  const box = useRef<HTMLInputElement>(null)
  const add = useMutation({
    mutationFn: () => api.post(`/issues/${enc(issue.key)}/links`, { url: url.trim() }),
    onSuccess: () => {
      setUrl('')
      box.current?.focus()
      refresh()
      setTimeout(refresh, 1500)
    },
    onError: toastError,
  })
  const remove = (id: number) =>
    api.del(`/issues/${enc(issue.key)}/links/${id}`).then(refresh, toastError)
  const sync = useMutation({
    mutationFn: () => api.post(`/issues/${enc(issue.key)}/links/refresh`),
    onSuccess: refresh,
    onError: toastError,
  })
  const actions = editable && (
    <span className="flex items-center gap-0.5">
      {issue.links.some((l) => l.number) && (
        <IconButton
          label="Refresh link status"
          size="xs"
          onClick={() => sync.mutate()}
          icon={<RefreshCw className={clsx('size-3.5', sync.isPending && 'animate-spin')} />}
        />
      )}
      <IconButton
        label="Add link"
        size="xs"
        onClick={() => setAdding(true)}
        icon={<Plus className="size-3.5" />}
      />
    </span>
  )
  const body =
    !issue.links.length && !adding ? null : (
      <div className="space-y-1">
        {issue.links.map((l) => (
          <div
            key={l.id}
            className="group flex h-8 items-center gap-2 rounded-md px-1.5 text-[13px] hover:bg-hover"
          >
            <span className="text-muted">{linkIcon(l.kind)}</span>
            <a
              href={safeUrl(l.url)}
              target="_blank"
              rel="noreferrer"
              className="min-w-0 flex-1 truncate"
            >
              {l.repo && l.number ? (
                <span className="text-subtle">
                  {l.repo}#{l.number}{' '}
                </span>
              ) : null}
              {l.title || l.url.replace(/^https?:\/\//, '')}
            </a>
            {l.kind === 'pull' && <PRBadge state={l.state} />}
            {l.kind === 'issue' && l.state && (
              <span
                className={clsx(
                  'text-[11px] font-medium',
                  l.state === 'closed' ? 'text-subtle' : 'text-ok',
                )}
              >
                {l.state}
              </span>
            )}
            <ExtIcon className="size-3 text-subtle" />
            {editable && (
              <IconButton
                label="Remove link"
                size="xs"
                className="opacity-0 group-hover:opacity-100 focus-visible:opacity-100 max-sm:opacity-100"
                onClick={() => remove(l.id)}
                icon={<X className="size-3.5" />}
              />
            )}
          </div>
        ))}
        {editable && adding && (
          <form
            className="flex items-center gap-2 pt-1"
            onSubmit={(e) => {
              e.preventDefault()
              if (url.trim()) add.mutate()
            }}
          >
            <Input
              ref={box}
              autoFocus
              value={url}
              onKeyDown={(e) => e.key === 'Escape' && (setUrl(''), setAdding(false))}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="Paste a PR, issue, or any URL"
              className="!h-8 text-[13px]"
            />
            <Button size="sm" type="submit" disabled={!url.trim()} loading={add.isPending}>
              Link
            </Button>
          </form>
        )}
      </div>
    )
  return wrap(actions, body)
}

export function SharePanel({ issue, onClose }: { issue: IssueDetail; onClose: () => void }) {
  const refresh = useRefresh(issue)
  const users = useQuery(q.users()).data ?? []
  const project = useQuery(q.project(issue.project_key)).data
  const members = new Set(project?.members?.map((m) => m.user.id))
  const [user, setUser] = useState('')
  const [role, setRole] = useState<'editor' | 'viewer'>('editor')
  const candidates = users.filter(
    (u) => !members.has(u.id) && !issue.shares.some((s) => s.user.id === u.id),
  )
  const share = (username: string, r: string) =>
    api
      .put(`/issues/${enc(issue.key)}/shares/${enc(username)}`, { role: r })
      .then(refresh, toastError)
  const unshare = (username: string) =>
    api.del(`/issues/${enc(issue.key)}/shares/${enc(username)}`).then(refresh, toastError)
  return (
    <div className="w-full space-y-3 p-2 sm:w-80">
      <div>
        <div className="text-[13px] font-semibold">Share {issue.key}</div>
        <div className="text-xs text-muted">
          People you share with can see this issue and everything under it, without joining the
          project.
        </div>
      </div>
      {issue.shares.length > 0 && (
        <ul className="space-y-1">
          {issue.shares.map((s) => (
            <li key={s.user.id} className="flex items-center gap-2 text-[13px]">
              <Avatar user={s.user} size={22} />
              <span className="flex-1 truncate">{s.user.display_name}</span>
              <Select
                value={s.role}
                onChange={(e) => share(s.user.username, e.target.value)}
                className="!h-7 w-24 text-xs"
              >
                <option value="editor">Can edit</option>
                <option value="viewer">Can view</option>
              </Select>
              <IconButton
                label="Stop sharing"
                size="xs"
                onClick={() => unshare(s.user.username)}
                icon={<X className="size-3.5" />}
              />
            </li>
          ))}
        </ul>
      )}
      {candidates.length > 0 ? (
        <form
          className="flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault()
            if (user) share(user, role).then(() => setUser(''))
          }}
        >
          <Select
            value={user}
            onChange={(e) => setUser(e.target.value)}
            className="!h-8 flex-1 text-[13px]"
          >
            <option value="">Choose someone…</option>
            {candidates.map((u) => (
              <option key={u.id} value={u.username}>
                {u.display_name}
              </option>
            ))}
          </Select>
          <Select
            value={role}
            onChange={(e) => setRole(e.target.value as 'editor' | 'viewer')}
            className="!h-8 w-24 text-[13px]"
          >
            <option value="editor">Edit</option>
            <option value="viewer">View</option>
          </Select>
          <Button size="sm" variant="primary" type="submit" disabled={!user}>
            Share
          </Button>
        </form>
      ) : (
        <div className="text-xs text-subtle">
          Everyone else is already a project member or has access.
        </div>
      )}
      <div className="flex justify-end">
        <Button size="sm" variant="ghost" onClick={onClose}>
          Done
        </Button>
      </div>
    </div>
  )
}
