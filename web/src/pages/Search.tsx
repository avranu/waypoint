import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookA,
  Bookmark,
  FileText,
  HelpCircle,
  Inbox,
  Lock,
  MessageSquare,
  Pencil,
  Search as SearchIcon,
  Trash2,
} from 'lucide-react'
import { api } from '@/lib/api'
import { pageQ, q } from '@/lib/queries'
import { useDebounced, useLocal } from '@/lib/hooks'
import { relativeTime } from '@/lib/format'
import { DESIGN_KIND_LABEL, hasFilters, searchAllQuery, snippetHTML } from '@/lib/knowledge'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog, Confirm } from '@/components/ui/dialog'
import { Field, Input, Select } from '@/components/ui/input'
import { Empty, PageHeader, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { IssueRow } from '@/components/issue/IssueRow'
import { useProjectsFor } from './Home'
import { IssueSheet } from './Project'
import { StatusIcon } from '@/components/issue/icons'
import { KindIcon } from '@/components/knowledge'
import type { Category, DesignKind, SearchGroup, SearchHit, SearchKind, View } from '@/lib/types'

const examples = [
  ['My open work', 'assignee:me is:open order:-priority'],
  ['Overdue', 'is:overdue'],
  ['Updated this week', 'updated:>=-7d order:-updated'],
  ['Done this week', 'is:done resolved:>=-7d order:-resolved'],
  ['Unestimated', 'is:open no:points type:story,task,bug'],
  ['Blocked', 'is:blocked'],
]

function Results({ query, onOpen }: { query: string; onOpen: (k: string) => void }) {
  const res = useQuery({
    ...pageQ.search(query),
    enabled: query.trim().length > 0,
    placeholderData: (p) => p,
  })
  const projects = useProjectsFor(res.data?.issues ?? [])
  if (!query.trim()) return null
  if (res.isError)
    return (
      <div className="mx-4 rounded-lg border border-danger/30 bg-danger/5 px-3 py-2 text-[13px] text-danger sm:mx-6">
        {res.error.message}
      </div>
    )
  if (!res.data)
    return (
      <div className="space-y-2 px-6">
        {Array.from({ length: 5 }, (_, i) => (
          <Skeleton key={i} className="h-8" />
        ))}
      </div>
    )
  if (res.data.issues.length === 0) return <Empty icon={<SearchIcon />} title="No issues match" />
  return (
    <div className="mx-4 overflow-hidden rounded-xl border border-line bg-panel sm:mx-6">
      <div className="border-b border-line px-4 py-2 text-xs text-subtle">
        {res.data.total} result{res.data.total === 1 ? '' : 's'}
        {res.data.total > res.data.issues.length && ` · showing ${res.data.issues.length}`}
      </div>
      {res.data.issues.map((i) => (
        <IssueRow
          key={i.key}
          issue={i}
          project={projects[i.project_key]}
          onOpen={onOpen}
          showProject
          showUpdated
        />
      ))}
    </div>
  )
}

export function SearchPage() {
  const { q: initial, mode: chosen } = useSearch({ from: '/app/search' })
  const navigate = useNavigate()
  const [text, setText] = useState(initial ?? '')
  const query = useDebounced(text, 250)
  const [sheet, setSheet] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [help, setHelp] = useState(false)
  // Everything unless the query uses filters, or the person picked a mode.
  const mode = chosen ?? (hasFilters(text) ? 'issues' : 'all')
  useEffect(() => {
    navigate({ to: '/search', search: { q: query, mode: chosen }, replace: true })
  }, [query]) // eslint-disable-line react-hooks/exhaustive-deps
  const setMode = (m: 'all' | 'issues') =>
    navigate({ to: '/search', search: { q: query, mode: m }, replace: true })
  useEffect(() => {
    document.title = 'Search · Waypoint'
  }, [])
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-5xl">
        <PageHeader
          title="Search"
          actions={
            <>
              <IconButton
                label="Query help"
                onClick={() => setHelp(true)}
                icon={<HelpCircle className="size-4" />}
              />
              <Button
                size="sm"
                icon={<Bookmark className="size-3.5" />}
                disabled={!text.trim()}
                onClick={() => setSaving(true)}
              >
                Save view
              </Button>
            </>
          }
        />
        <div className="px-4 sm:px-6">
          <div className="relative">
            <SearchIcon className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-subtle" />
            <Input
              autoFocus
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder={
                mode === 'all'
                  ? 'Search everything, or a code like R-28…'
                  : 'Text, or assignee:me is:open label:bug'
              }
              aria-label="Search"
              className={clsx('!h-11 pl-9 text-[13px]', mode === 'issues' && 'font-mono')}
            />
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-1.5">
            <div
              className="mr-1 flex rounded-lg bg-sunken p-0.5"
              role="group"
              aria-label="Search in"
            >
              {(
                [
                  ['all', 'Everything'],
                  ['issues', 'Issues'],
                ] as const
              ).map(([m, label]) => (
                <button
                  key={m}
                  onClick={() => setMode(m)}
                  aria-pressed={mode === m}
                  className={clsx(
                    'h-6 rounded-md px-2 text-xs',
                    mode === m ? 'bg-panel font-medium text-fg shadow-card' : 'text-muted',
                  )}
                >
                  {label}
                </button>
              ))}
            </div>
            {examples.map(([label, ex]) => (
              <button
                key={label}
                onClick={() => setText(ex)}
                className="rounded-full border border-line bg-panel px-2.5 py-1 text-xs text-muted hover:text-fg"
              >
                {label}
              </button>
            ))}
          </div>
        </div>
        <div className="mt-4">
          {mode === 'all' ? (
            <Everything query={query} onOpen={setSheet} />
          ) : (
            <Results query={query} onOpen={setSheet} />
          )}
        </div>
      </div>
      {sheet && <IssueSheet issueKey={sheet} onClose={() => setSheet(null)} />}
      <SaveViewDialog open={saving} onClose={() => setSaving(false)} query={text} />
      <QueryHelp open={help} onClose={() => setHelp(false)} />
    </div>
  )
}

const KINDS: { kind: SearchKind; param: string; label: string }[] = [
  { kind: 'issue', param: 'issues', label: 'Issues' },
  { kind: 'request', param: 'requests', label: 'Requests' },
  { kind: 'design', param: 'design', label: 'Design' },
  { kind: 'glossary', param: 'glossary', label: 'Glossary' },
  { kind: 'page', param: 'pages', label: 'Pages' },
  { kind: 'comment', param: 'comments', label: 'Comments' },
]

const GROUP_LABEL: Record<SearchKind, string> = {
  issue: 'Issues',
  request: 'Requests',
  design: 'Design notes',
  glossary: 'Glossary',
  page: 'Pages',
  comment: 'Comments and work logs',
}

/** Search across kinds (GET /search/all): grouped hits with highlighted snippets. */
function Everything({ query, onOpen }: { query: string; onOpen: (k: string) => void }) {
  const [kinds, setKinds] = useLocal<string[]>('search-kinds', [])
  const [archived, setArchived] = useLocal('search-archived', false)
  const [project, setProject] = useState('')
  const projects = useQuery(q.projects()).data ?? []
  const res = useQuery({
    ...searchAllQuery({
      q: query.trim(),
      kinds: kinds.length ? kinds.join(',') : undefined,
      project: project || undefined,
      archived,
      limit: 20,
    }),
    enabled: query.trim().length > 0,
    placeholderData: (p) => p,
  })
  const toggle = (k: string) =>
    setKinds(kinds.includes(k) ? kinds.filter((x) => x !== k) : [...kinds, k])
  const groups = (res.data?.groups ?? []).filter((g) => g.hits.length > 0)
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-1.5 px-4 sm:px-6">
        {KINDS.map((k) => (
          <button
            key={k.param}
            onClick={() => toggle(k.param)}
            aria-pressed={kinds.includes(k.param)}
            className={clsx(
              'h-6 rounded-full px-2.5 text-xs',
              kinds.includes(k.param)
                ? 'bg-accent-soft font-medium text-fg'
                : 'border border-line bg-panel text-muted hover:text-fg',
            )}
          >
            {k.label}
          </button>
        ))}
        <label className="ml-1 inline-flex items-center gap-1.5 text-xs text-muted">
          <input
            type="checkbox"
            checked={archived}
            onChange={(e) => setArchived(e.target.checked)}
            className="accent-[var(--wp-accent)]"
          />
          Include closed and archived
        </label>
        <span className="flex-1" />
        <Select
          aria-label="Project"
          value={project}
          onChange={(e) => setProject(e.target.value)}
          className="w-40"
        >
          <option value="">All projects</option>
          {projects.map((p) => (
            <option key={p.key} value={p.key}>
              {p.name}
            </option>
          ))}
        </Select>
      </div>
      {!query.trim() ? null : res.isError ? (
        <div className="mx-4 rounded-lg border border-danger/30 bg-danger/5 px-3 py-2 text-[13px] text-danger sm:mx-6">
          {res.error.message}
        </div>
      ) : !res.data ? (
        <div className="space-y-2 px-6">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-10" />
          ))}
        </div>
      ) : groups.length === 0 && !res.data.exact?.length ? (
        <Empty icon={<SearchIcon />} title="Nothing matches">
          {archived ? 'Try other words.' : 'Try other words, or include closed and archived items.'}
        </Empty>
      ) : (
        <>
          {!!res.data.exact?.length && (
            <HitGroup
              group={{ kind: 'issue', total: res.data.exact.length, hits: res.data.exact }}
              label="Best match"
              onOpen={onOpen}
            />
          )}
          {groups.map((g) => (
            <HitGroup key={g.kind} group={g} onOpen={onOpen} />
          ))}
        </>
      )}
    </div>
  )
}

function HitGroup({
  group: g,
  label,
  onOpen,
}: {
  group: SearchGroup
  label?: string
  onOpen: (k: string) => void
}) {
  return (
    <section className="mx-4 overflow-hidden rounded-xl border border-line bg-panel sm:mx-6">
      <h2 className="flex items-center justify-between border-b border-line px-4 py-2 text-xs font-semibold text-subtle">
        <span>{label ?? GROUP_LABEL[g.kind]}</span>
        <span className="font-normal">
          {g.total > g.hits.length ? `${g.hits.length} of ${g.total}` : g.total}
        </span>
      </h2>
      <ul>
        {g.hits.map((h) => (
          <Hit key={`${h.kind}:${h.id}`} hit={h} onOpen={onOpen} />
        ))}
      </ul>
    </section>
  )
}

function HitIcon({ hit: h }: { hit: SearchHit }) {
  const c = 'size-3.5 shrink-0 text-subtle'
  switch (h.kind) {
    case 'issue':
      return <StatusIcon category={(h.state || 'todo') as Category} className={c} />
    case 'design':
      return <KindIcon kind={h.sub ?? 'note'} />
    case 'glossary':
      return <BookA className={c} />
    case 'page':
      return <FileText className={c} />
    case 'request':
      return <Inbox className={c} />
  }
  return <MessageSquare className={c} />
}

function Hit({ hit: h, onOpen }: { hit: SearchHit; onOpen: (k: string) => void }) {
  const body = (
    <>
      <div className="flex min-w-0 items-center gap-2">
        <HitIcon hit={h} />
        {h.key && <span className="shrink-0 text-xs tabular-nums text-subtle">{h.key}</span>}
        <span className="min-w-0 flex-1 truncate text-[13px] font-medium">{h.title}</span>
        {h.state === 'proposed' && (
          <span className="shrink-0 rounded bg-warn/15 px-1 text-[10px] font-medium text-warn">
            proposed
          </span>
        )}
        {h.match === 'mention' && (
          <span className="shrink-0 rounded bg-sunken px-1 text-[10px] font-medium text-muted">
            mentions it
          </span>
        )}
        {h.state === 'locked' && (
          <Lock className="size-3 shrink-0 text-subtle" aria-label="Locked" />
        )}
        <span className="hidden shrink-0 text-xs text-subtle sm:inline">
          {[
            h.kind === 'design' ? DESIGN_KIND_LABEL[h.sub as DesignKind] : h.sub,
            h.project_key ?? (h.kind === 'glossary' || h.kind === 'design' ? 'global' : ''),
            relativeTime(h.updated_at),
          ]
            .filter(Boolean)
            .join(' · ')}
        </span>
      </div>
      {h.snippet && (
        <p
          className="ml-5.5 mt-0.5 line-clamp-2 text-xs text-muted"
          dangerouslySetInnerHTML={{ __html: snippetHTML(h.snippet) }}
        />
      )}
    </>
  )
  const cls = 'block px-4 py-2 text-left hover:bg-hover'
  if ((h.kind === 'issue' || h.kind === 'comment') && h.key)
    return (
      <li className="border-b border-line last:border-b-0">
        <button onClick={() => onOpen(h.key!)} className={clsx(cls, 'w-full')}>
          {body}
        </button>
      </li>
    )
  if (h.kind === 'request' && h.key)
    return (
      <li className="border-b border-line last:border-b-0">
        <Link to="/requests/$code" params={{ code: h.key }} className={cls}>
          {body}
        </Link>
      </li>
    )
  return (
    <li className="border-b border-line last:border-b-0">
      <Link
        to={
          h.kind === 'design'
            ? '/design/$id'
            : h.kind === 'glossary'
              ? '/glossary/$id'
              : '/pages/$id'
        }
        params={{ id: String(h.id) }}
        className={cls}
      >
        {body}
      </Link>
    </li>
  )
}

export function QueryHelp({ open, onClose }: { open: boolean; onClose: () => void }) {
  const help = useQuery({
    queryKey: ['queryHelp'],
    queryFn: () => api.get<string>('/help/query'),
    enabled: open,
    staleTime: Infinity,
  })
  return (
    <Dialog open={open} onClose={onClose} title="Query language" wide>
      <pre className="whitespace-pre-wrap p-4 font-mono text-xs leading-relaxed text-muted">
        {help.data ?? 'Loading…'}
      </pre>
    </Dialog>
  )
}

function SaveViewDialog({
  open,
  onClose,
  query,
  view,
}: {
  open: boolean
  onClose: () => void
  query: string
  view?: View
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [name, setName] = useState('')
  const [text, setText] = useState(query)
  useEffect(() => {
    if (open) {
      setName(view?.name ?? '')
      setText(view?.query ?? query)
    }
  }, [open, view, query])
  const save = useMutation({
    mutationFn: () =>
      view
        ? api.patch(`/views/${view.id}`, { name, query: text })
        : api.post<View>('/views', { name, query: text, pinned: true }),
    onSuccess: (v) => {
      qc.invalidateQueries({ queryKey: ['views'] })
      onClose()
      if (!view && v) navigate({ to: '/views/$id', params: { id: String((v as View).id) } })
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={view ? 'Edit view' : 'Save view'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!name.trim()}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save
          </Button>
        </>
      }
    >
      <div className="space-y-4 p-4">
        <Field label="Name" hint="Pinned views appear in the sidebar.">
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Urgent bugs"
          />
        </Field>
        <Field label="Query">
          <Input
            value={text}
            onChange={(e) => setText(e.target.value)}
            className="font-mono text-[13px]"
          />
        </Field>
      </div>
    </Dialog>
  )
}

export function ViewPage() {
  const { id } = useParams({ from: '/app/views/$id' })
  const navigate = useNavigate()
  const qc = useQueryClient()
  const view = (useQuery(q.views()).data ?? []).find((v) => v.id === Number(id))
  const [sheet, setSheet] = useState<string | null>(null)
  const [editing, setEditing] = useState(false)
  const [deleting, setDeleting] = useState(false)
  useEffect(() => {
    if (view) document.title = `${view.name} · Waypoint`
  }, [view])
  if (!view) return <Empty title="View not found" />
  const del = () =>
    api.del(`/views/${view.id}`).then(() => {
      qc.invalidateQueries({ queryKey: ['views'] })
      navigate({ to: '/' })
    }, toastError)
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-5xl">
        <PageHeader
          title={view.name}
          subtitle={<code className="text-xs">{view.query}</code>}
          actions={
            <>
              <IconButton
                label="Edit view"
                onClick={() => setEditing(true)}
                icon={<Pencil className="size-3.5" />}
              />
              <IconButton
                label="Delete view"
                onClick={() => setDeleting(true)}
                icon={<Trash2 className="size-3.5" />}
              />
            </>
          }
        />
        <Results
          query={view.query + (view.project_key ? ` project:${view.project_key}` : '')}
          onOpen={setSheet}
        />
      </div>
      {sheet && <IssueSheet issueKey={sheet} onClose={() => setSheet(null)} />}
      <SaveViewDialog
        open={editing}
        onClose={() => setEditing(false)}
        query={view.query}
        view={view}
      />
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        title={`Delete “${view.name}”?`}
        onConfirm={del}
      />
    </div>
  )
}
