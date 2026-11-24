// An agent's note stays proposed until a person approves it; only a person can approve.

import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  Archive,
  Check,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  ClipboardCheck,
  FilePlus,
  GitMerge,
  Lightbulb,
  Link2,
  Lock,
  MoreHorizontal,
  Plus,
  Search,
  ShieldCheck,
  Trash2,
  X,
} from 'lucide-react'
import { ApiError } from '@/lib/api'
import { q } from '@/lib/queries'
import { useDebounced, useLocal } from '@/lib/hooks'
import { dateTime, relativeTime } from '@/lib/format'
import { agentLabel } from '@/lib/agents'
import {
  DESIGN_KIND_LABEL,
  DESIGN_KIND_PLURAL,
  REL_LABEL,
  designApi,
  designNoteQuery,
  designQuery,
  designReviewQuery,
  lineDiff,
  snippetHTML,
  specQuery,
} from '@/lib/knowledge'
import {
  DESIGN_KINDS,
  DESIGN_STATUSES,
  type DesignKind,
  type DesignNote,
  type DesignRef,
  type DesignRevision,
  type DesignStatus,
  type KItem,
  type ReviewItem,
} from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input, Select, Switch, Textarea } from '@/components/ui/input'
import { Empty, PageHeader, Skeleton } from '@/components/ui/misc'
import { MenuList, Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { MarkdownEditor } from '@/components/issue/Description'
import { Attachments } from '@/components/attachments/Attachments'
import { PropButton } from '@/components/issue/pickers'
import { ItemIcon, ItemTitle, Links } from '@/components/Links'
import { Usage } from '@/components/Usage'
import {
  BlockTitle,
  DiffView,
  InlineTitle,
  KindIcon,
  ScopeChip,
  ScopePicker,
  Who,
  useScopeOptions,
} from '@/components/knowledge'

const noop = () => {}

/** /design: every note you can see, or one project's (?project=). */
export function DesignPage() {
  const search = useSearch({ strict: false }) as { project?: string }
  const navigate = useNavigate()
  const projects = useQuery(q.projects()).data ?? []
  const project = search.project ?? ''
  useEffect(() => {
    document.title = 'Design · Waypoint'
  }, [])
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-4xl">
        <PageHeader
          title="Design"
          icon={<Lightbulb className="size-5 text-subtle" />}
          actions={<ReviewLink />}
        />
        <div className="px-4 sm:px-6">
          <DesignList
            params={{ project: project || undefined, own: !!project && project !== 'global' }}
            addTo={project && project !== 'global' ? project : undefined}
            filter={
              <Select
                aria-label="Project"
                value={project}
                onChange={(e) =>
                  navigate({
                    to: '/design',
                    search: { project: e.target.value || undefined },
                    replace: true,
                  })
                }
                className="w-40 shrink-0"
              >
                <option value="">All projects</option>
                <option value="global">Global</option>
                {projects.map((p) => (
                  <option key={p.key} value={p.key}>
                    {p.name}
                  </option>
                ))}
              </Select>
            }
          />
        </div>
      </div>
    </div>
  )
}

/** /p/$key/design: the project's notes plus its parents' and global ones. */
export function ProjectDesignPage() {
  const { key } = useParams({ from: '/app/p/$key/design' })
  const [own, setOwn] = useLocal(`design-own-${key}`, true)
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-4xl px-3 pt-3 sm:px-6 sm:pt-5">
        <DesignList
          params={{ project: key, own }}
          project={key}
          addTo={key}
          filter={
            <label className="flex shrink-0 items-center gap-2 text-xs text-muted">
              <Switch checked={own} onChange={setOwn} label="Only this project's notes" />
              <span className="hidden sm:inline">Only this project's</span>
            </label>
          }
          extra={<ReviewLink project={key} />}
        />
      </div>
    </div>
  )
}

function ReviewLink({ project }: { project?: string }) {
  const n = useQuery(designReviewQuery(project ?? '')).data?.length ?? 0
  return (
    <Link
      to="/design/review"
      search={project ? { project } : {}}
      className={clsx(
        'inline-flex h-7 shrink-0 items-center gap-1.5 rounded-md px-2 text-[13px]',
        n ? 'bg-warn/15 font-medium text-warn' : 'text-muted hover:bg-hover hover:text-fg',
      )}
    >
      <ClipboardCheck className="size-3.5" />
      Review{n ? ` · ${n}` : ''}
    </Link>
  )
}

const STATUS_FILTERS = [
  { value: '', label: 'Active' },
  { value: 'archived', label: 'Archived' },
  { value: 'all', label: 'All' },
]

function DesignList({
  params,
  project,
  filter,
  extra,
  addTo,
}: {
  params: { project?: string; own?: boolean }
  /** set on a project's tab: inherited notes are marked */
  project?: string
  filter: ReactNode
  extra?: ReactNode
  addTo?: string
}) {
  const [text, setText] = useState('')
  const debounced = useDebounced(text.trim(), 200)
  const [status, setStatus] = useLocal('design-status', '')
  const [kind, setKind] = useState('')
  const [creating, setCreating] = useState(false)
  const scopes = useScopeOptions(project)
  const list = useQuery({
    ...designQuery({
      ...params,
      status: status || undefined,
      kind: kind || undefined,
      q: debounced || undefined,
    }),
    placeholderData: (p) => p,
  })
  const notes = list.data ?? []
  const flat = !!debounced || !!kind
  const { roots, children } = useMemo(() => {
    const ids = new Set(notes.map((n) => n.id))
    const children = new Map<number, DesignNote[]>()
    const roots = new Map<DesignKind, DesignNote[]>()
    for (const n of notes) {
      if (n.parent_id && ids.has(n.parent_id))
        children.set(n.parent_id, [...(children.get(n.parent_id) ?? []), n])
      else roots.set(n.kind, [...(roots.get(n.kind) ?? []), n])
    }
    return { roots, children }
  }, [notes])
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Search notes…"
            aria-label="Search design notes"
            className="h-9 w-full rounded-lg border border-line bg-panel pl-8 pr-2 text-[13px] outline-none placeholder:text-subtle focus:border-accent"
          />
        </div>
        {filter}
        {scopes.length > 0 && (
          <Button
            variant="primary"
            size="sm"
            icon={<Plus className="size-3.5" />}
            onClick={() => setCreating(true)}
            className="shrink-0"
            aria-label="New note"
          >
            <span className="max-sm:hidden">New note</span>
          </Button>
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <div className="flex rounded-lg bg-sunken p-0.5" role="group" aria-label="Status">
          {STATUS_FILTERS.map((s) => (
            <button
              key={s.value}
              onClick={() => setStatus(s.value)}
              aria-pressed={status === s.value}
              className={clsx(
                'h-6 rounded-md px-2 text-xs',
                status === s.value ? 'bg-panel font-medium text-fg shadow-card' : 'text-muted',
              )}
            >
              {s.label}
            </button>
          ))}
        </div>
        <div className="no-scrollbar -mx-1 order-last flex w-full min-w-0 gap-1 overflow-x-auto px-1 sm:order-none sm:w-auto sm:flex-1">
          {['', ...DESIGN_KINDS].map((k) => (
            <button
              key={k || 'all'}
              onClick={() => setKind(k)}
              aria-pressed={kind === k}
              className={clsx(
                'inline-flex h-6 shrink-0 items-center gap-1 rounded-full px-2 text-xs',
                kind === k ? 'bg-accent-soft text-fg' : 'text-muted hover:bg-hover',
              )}
            >
              {k && <KindIcon kind={k} className="size-3" />}
              {k ? DESIGN_KIND_PLURAL[k as DesignKind] : 'All kinds'}
            </button>
          ))}
        </div>
        {extra && <div className="ml-auto sm:ml-0">{extra}</div>}
      </div>
      <div className="wp-surface overflow-hidden rounded-xl border border-line bg-panel">
        {list.isPending ? (
          <div className="space-y-2 p-3">
            <Skeleton className="h-9" />
            <Skeleton className="h-9" />
            <Skeleton className="h-9" />
          </div>
        ) : list.isError ? (
          <p className="p-4 text-[13px] text-danger">{list.error.message}</p>
        ) : notes.length === 0 ? (
          <Empty
            icon={<Lightbulb />}
            title={debounced || kind || status ? 'No notes match' : 'No design notes yet'}
          >
            {debounced || kind || status
              ? 'Try another filter.'
              : 'Write down what you want: goals, the principles behind them, features and their screens, and the decisions you made. Agents read them as the spec.'}
          </Empty>
        ) : flat ? (
          <ul>
            {notes.map((n) => (
              <NoteRow key={n.id} note={n} project={project} depth={0} kids={[]} tree={children} />
            ))}
          </ul>
        ) : (
          DESIGN_KINDS.filter((k) => roots.has(k)).map((k) => (
            <section key={k}>
              <h2 className="flex items-center gap-1.5 border-b border-line bg-sunken/60 px-4 py-1.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                <KindIcon kind={k} className="size-3" />
                {DESIGN_KIND_PLURAL[k]}
              </h2>
              <ul>
                {roots.get(k)!.map((n) => (
                  <NoteRow
                    key={n.id}
                    note={n}
                    project={project}
                    depth={0}
                    kids={children.get(n.id) ?? []}
                    tree={children}
                  />
                ))}
              </ul>
            </section>
          ))
        )}
      </div>
      <NewNoteDialog
        open={creating}
        onClose={() => setCreating(false)}
        project={project}
        defaultScope={addTo ?? scopes[0]?.value ?? 'global'}
      />
    </div>
  )
}

/** "proposed" or "2 pending": the note has something waiting for a person. */
function PendingMark({ note }: { note: Pick<DesignNote, 'approved' | 'pending'> }) {
  if (note.approved && !note.pending) return null
  return (
    <span
      className="inline-flex h-[18px] shrink-0 items-center gap-1 rounded bg-warn/15 px-1.5 text-[10px] font-medium text-warn"
      title={
        note.approved
          ? `${note.pending} agent change${note.pending === 1 ? '' : 's'} waiting for a person`
          : 'Written by an agent; not approved yet'
      }
    >
      <span className="size-1.5 rounded-full bg-warn" />
      {note.approved ? `${note.pending} pending` : 'proposed'}
    </span>
  )
}

function NoteRow({
  note: n,
  project,
  depth,
  kids,
  tree,
}: {
  note: DesignNote
  project?: string
  depth: number
  kids: DesignNote[]
  tree: Map<number, DesignNote[]>
}) {
  const [open, setOpen] = useState(depth < 1)
  return (
    <li className="border-b border-line last:border-b-0">
      <div
        className="flex min-h-10 items-center gap-1.5 pr-3 hover:bg-hover"
        style={{ paddingLeft: 8 + depth * 18 }}
      >
        {kids.length > 0 ? (
          <button
            onClick={() => setOpen(!open)}
            aria-label={open ? `Collapse ${n.title}` : `Expand ${n.title}`}
            aria-expanded={open}
            className="flex size-5 shrink-0 items-center justify-center rounded text-subtle hover:text-fg"
          >
            {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
          </button>
        ) : (
          <span className="size-5 shrink-0" />
        )}
        <Link
          to="/design/$id"
          params={{ id: String(n.id) }}
          className="flex min-w-0 flex-1 items-center gap-2 py-2"
        >
          <KindIcon kind={n.kind} />
          <span className="min-w-0 flex-1">
            <span
              className={clsx(
                'block truncate text-[13px]',
                n.status !== 'active' && 'text-muted line-through decoration-subtle',
              )}
            >
              {n.code && <span className="mr-1.5 font-mono text-xs text-subtle">{n.code}</span>}
              {n.title}
            </span>
            {n.snippet && (
              <span
                className="line-clamp-1 text-xs text-subtle"
                dangerouslySetInnerHTML={{ __html: snippetHTML(n.snippet) }}
              />
            )}
          </span>
          {depth > 0 && (
            <span className="hidden shrink-0 text-[11px] text-subtle sm:inline">
              {DESIGN_KIND_LABEL[n.kind]}
            </span>
          )}
          {n.status !== 'active' && (
            <span className="shrink-0 text-[11px] text-subtle">{n.status}</span>
          )}
          <PendingMark note={n} />
          {(!project || n.inherited) && <ScopeChip scope={n} />}
        </Link>
      </div>
      {open && kids.length > 0 && (
        <ul className="border-t border-line">
          {kids.map((k) => (
            <NoteRow
              key={k.id}
              note={k}
              project={project}
              depth={depth + 1}
              kids={tree.get(k.id) ?? []}
              tree={tree}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

function NewNoteDialog({
  open,
  onClose,
  project,
  defaultScope,
  parent,
}: {
  open: boolean
  onClose: () => void
  project?: string
  defaultScope: string
  /** a sub-note: it lives where its parent does */
  parent?: DesignNote
}) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const [kind, setKind] = useState<DesignKind>('note')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [scope, setScope] = useState(defaultScope)
  const [parentId, setParentId] = useState('')
  useEffect(() => {
    if (!open) return
    setTitle('')
    setBody('')
    setScope(parent ? parent.project_key || 'global' : defaultScope)
    setParentId(parent ? String(parent.id) : '')
    setKind(parent?.kind === 'feature' ? 'screen' : parent ? 'note' : 'feature')
  }, [open]) // eslint-disable-line react-hooks/exhaustive-deps
  const candidates = useQuery({
    ...designQuery({ project: scope, own: scope !== 'global', status: 'all' }),
    enabled: open && !parent,
  }).data
  const create = useMutation({
    mutationFn: () =>
      designApi.create({
        title: title.trim(),
        kind,
        body: body.trim(),
        project: scope === 'global' ? '' : scope,
        parent: parentId ? Number(parentId) : undefined,
      }),
    onSuccess: (n) => {
      qc.invalidateQueries({ queryKey: ['design'] })
      onClose()
      navigate({ to: '/design/$id', params: { id: String(n.id) } })
    },
    onError: toastError,
  })
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={parent ? `New note under “${parent.title}”` : 'New design note'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!title.trim()}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            Create note
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (title.trim()) create.mutate()
        }}
      >
        <div className="grid gap-3 sm:grid-cols-[10rem_1fr]">
          <Field label="Kind">
            <Select value={kind} onChange={(e) => setKind(e.target.value as DesignKind)}>
              {DESIGN_KINDS.map((k) => (
                <option key={k} value={k}>
                  {DESIGN_KIND_LABEL[k]}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Title">
            <Input
              autoFocus
              value={title}
              onChange={(e) => setTitle(e.target.value)}
              placeholder="Agents pick up work by themselves"
            />
          </Field>
        </div>
        {!parent && (
          <div className="grid gap-3 sm:grid-cols-2">
            <Field label="Where">
              <div className="flex h-9 items-center rounded-lg border border-line px-1">
                <ScopePicker
                  value={scope}
                  onChange={(v) => (setScope(v), setParentId(''))}
                  project={project}
                  label="Put it in…"
                />
              </div>
            </Field>
            <Field label="Under">
              <Select value={parentId} onChange={(e) => setParentId(e.target.value)}>
                <option value="">Top level</option>
                {(candidates ?? []).map((n) => (
                  <option key={n.id} value={String(n.id)}>
                    {`${n.title} (${DESIGN_KIND_LABEL[n.kind].toLowerCase()})`}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
        )}
        <Field label="What you intend">
          <Textarea
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={6}
            autoGrow
            placeholder="Markdown. Link terms, notes and issues with [[wikilinks]]."
          />
        </Field>
        <p className="text-xs text-subtle">
          Notes you write are approved as written. Agents can add to them, but their additions and
          edits wait for a person.
        </p>
      </form>
    </Dialog>
  )
}

/** /design/$id */
export function DesignNotePage() {
  const { id } = useParams({ from: '/app/design/$id' })
  const note = useQuery(designNoteQuery(Number(id)))
  const n = note.data
  useEffect(() => {
    if (n) document.title = `${n.title} · Design · Waypoint`
  }, [n])
  if (note.isPending) return <Skeleton className="m-6 h-64" />
  if (!n)
    return (
      <Empty icon={<Lightbulb />} title="Note not found">
        {note.error instanceof ApiError && note.error.status === 404
          ? 'It may have been deleted, or it belongs to a project you can’t see.'
          : note.error?.message}{' '}
        <Link to="/design" className="text-accent hover:underline">
          All design notes
        </Link>
      </Empty>
    )
  return <NoteView key={n.id} note={n} />
}

/** Keep every cached copy of a note (and the lists and queue) in step after a change. */
function afterNoteChange(qc: QueryClient, n?: DesignNote) {
  if (n) qc.setQueryData(designNoteQuery(n.id).queryKey, n)
  qc.invalidateQueries({ queryKey: ['design'], predicate: (x) => x.queryKey[1] !== 'note' })
  qc.invalidateQueries({ queryKey: ['spec'] })
  qc.invalidateQueries({ queryKey: ['links'] })
  qc.invalidateQueries({ queryKey: ['searchAll'] })
}

function useReview(noteId: number) {
  const qc = useQueryClient()
  const key = designNoteQuery(noteId).queryKey
  return useMutation({
    mutationFn: (v: {
      rev: DesignRevision
      action: 'approve' | 'reject' | 'merge'
      note?: string
    }) => designApi.review(v.rev.id, v.action, v.note),
    onMutate: ({ rev, action }) => {
      const prev = qc.getQueryData<DesignNote>(key)
      if (prev) {
        const state =
          action === 'approve' ? 'approved' : action === 'reject' ? 'rejected' : 'merged'
        qc.setQueryData<DesignNote>(key, {
          ...prev,
          pending: Math.max(0, prev.pending - (rev.state === 'pending' ? 1 : 0)),
          proposals: prev.proposals?.filter((p) => p.id !== rev.id),
          additions: prev.additions?.map((a) => (a.id === rev.id ? { ...a, state } : a)),
        })
      }
      return { prev }
    },
    onSuccess: (n, v) => {
      afterNoteChange(qc, n)
      toast(
        v.action === 'merge'
          ? 'Merged into the note'
          : v.action === 'approve'
            ? 'Approved'
            : 'Rejected',
      )
    },
    onError: (e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(key, ctx.prev)
      toastError(e)
    },
  })
}

function NoteView({ note: n }: { note: DesignNote }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const search = useSearch({ strict: false }) as { tab?: string }
  const specData = useQuery(specQuery(String(n.id), n.project_key)).data
  const specEmpty = !!specData && specIsEmpty(specData)
  const tab = search.tab === 'spec' && !specEmpty ? 'spec' : 'note'
  const menu = usePopover()
  const [deleting, setDeleting] = useState(false)
  const [subNote, setSubNote] = useState(false)
  const people = n.can_approve
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => designApi.update(n.id, body),
    onMutate: (body) => {
      const key = designNoteQuery(n.id).queryKey
      const prev = qc.getQueryData<DesignNote>(key)
      if (prev && ('kind' in body || 'status' in body))
        qc.setQueryData<DesignNote>(key, { ...prev, ...body })
      return { prev }
    },
    onSuccess: (nn) => afterNoteChange(qc, nn),
    onError: (e, _b, ctx) => {
      if (ctx?.prev) qc.setQueryData(designNoteQuery(n.id).queryKey, ctx.prev)
      toastError(e)
    },
  })
  const approve = useMutation({
    mutationFn: () => designApi.approve(n.id, n.updated_at),
    onSuccess: (nn) => {
      afterNoteChange(qc, nn)
      toast('Approved: it’s part of the intent now')
    },
    onError: (e) => {
      if (e instanceof ApiError && e.status === 409) {
        qc.invalidateQueries({ queryKey: designNoteQuery(n.id).queryKey })
        toast(
          'The note changed while you were reading it. Here’s the latest; check it and approve again.',
          {
            kind: 'error',
            ms: 8000,
          },
        )
      } else toastError(e)
    },
  })
  const del = useMutation({
    mutationFn: () => designApi.remove(n.id),
    onSuccess: async () => {
      toast(`Deleted “${n.title}”`)
      // Leave first, so nothing on this page asks for the note again.
      await navigate(
        n.project_key
          ? { to: '/p/$key/design', params: { key: n.project_key } }
          : { to: '/design' },
      )
      qc.removeQueries({ queryKey: designNoteQuery(n.id).queryKey })
      qc.removeQueries({ queryKey: ['links', 'design', n.id] })
      afterNoteChange(qc)
    },
    onError: toastError,
  })
  const setTab = (t: string) =>
    navigate({
      to: '.',
      search: (s: Record<string, unknown>) => ({ ...s, tab: t === 'note' ? undefined : t }),
      replace: true,
    } as never)
  return (
    <article className="mx-auto w-full max-w-3xl flex-1 space-y-4 overflow-y-auto px-4 pb-16 pt-3 sm:px-8 sm:pt-6">
      <div className="flex items-center gap-2 text-xs text-subtle">
        <nav aria-label="Breadcrumbs" className="flex min-w-0 flex-1 items-center gap-1">
          {n.project_key ? (
            <Link
              to="/p/$key/design"
              params={{ key: n.project_key }}
              className="inline-flex shrink-0 items-center gap-1 hover:text-fg"
            >
              <ChevronLeft className="size-3.5 md:hidden" />
              Design
            </Link>
          ) : (
            <Link
              to="/design"
              search={{ project: 'global' }}
              className="inline-flex shrink-0 items-center gap-1 hover:text-fg"
            >
              <ChevronLeft className="size-3.5 md:hidden" />
              Global
            </Link>
          )}
          {(n.ancestors ?? []).map((a) => (
            <span key={a.id} className="flex min-w-0 items-center gap-1">
              <ChevronRight className="size-3 shrink-0" />
              <Link
                to="/design/$id"
                params={{ id: String(a.id) }}
                className="truncate hover:text-fg"
              >
                {a.title}
              </Link>
            </span>
          ))}
        </nav>
        {n.code && (
          <span
            className="shrink-0 font-mono text-subtle"
            title="Decision code: wp spec, [[links]] and wp design take it"
          >
            {n.project_key ? `${n.project_key}-${n.code}` : n.code}
          </span>
        )}
        <span className="hidden shrink-0 sm:inline" title={dateTime(n.updated_at)}>
          Updated {relativeTime(n.updated_at)}
        </span>
        {people && (
          <>
            <IconButton
              ref={menu.anchor}
              label="Note actions"
              onClick={menu.toggle}
              icon={<MoreHorizontal className="size-4" />}
            />
            <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
              <MenuList
                onClose={menu.close}
                items={[
                  { label: 'New sub-note', icon: <FilePlus />, onSelect: () => setSubNote(true) },
                  n.status === 'active'
                    ? {
                        label: 'Retire',
                        icon: <Archive />,
                        onSelect: () => save.mutate({ status: 'retired' }),
                      }
                    : {
                        label: 'Make active again',
                        icon: <Check />,
                        onSelect: () => save.mutate({ status: 'active' }),
                      },
                  { separator: true, label: '' },
                  {
                    label: 'Delete…',
                    icon: <Trash2 />,
                    danger: true,
                    onSelect: () => setDeleting(true),
                  },
                ]}
              />
            </Popover>
          </>
        )}
      </div>
      <InlineTitle
        value={n.title}
        editable={n.can_edit}
        label="Note title"
        onSave={(title) => save.mutate({ title })}
      />
      <div className="flex flex-wrap items-center gap-1">
        <KindPicker value={n.kind} disabled={!people} onChange={(kind) => save.mutate({ kind })} />
        <StatusPicker
          value={n.status}
          disabled={!people}
          onChange={(status) => save.mutate({ status })}
        />
        <ScopePicker
          value={n.project_key || 'global'}
          disabled={!people}
          onChange={(v) => save.mutate({ project: v === 'global' ? '' : v })}
        />
        <ParentPicker note={n} disabled={!people} onChange={(parent) => save.mutate({ parent })} />
      </div>
      <div className="flex gap-1 border-b border-line" role="tablist" hidden={specEmpty}>
        {[
          ['note', 'Note'],
          ['spec', 'Spec'],
        ].map(([t, label]) => (
          <button
            key={t}
            role="tab"
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            className={clsx(
              '-mb-px h-8 border-b-2 px-2.5 text-[13px]',
              tab === t
                ? 'border-accent font-medium text-fg'
                : 'border-transparent text-muted hover:text-fg',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {tab === 'spec' ? (
        <SpecView note={n} />
      ) : (
        <div className="space-y-6">
          <ProvenanceBanner
            note={n}
            onApprove={() => approve.mutate()}
            approving={approve.isPending}
          />
          {(n.proposals?.length ?? 0) > 0 && <Proposals note={n} />}
          <MarkdownEditor
            value={n.body ?? ''}
            editable={n.can_edit}
            project={n.project_key}
            placeholder="Write what you intend… (markdown; [[links]] to terms, notes and issues)"
            draftKey={n.can_edit ? `design:${n.id}` : undefined}
            minRows={8}
            attachTo={n.can_edit ? `design:${n.id}` : undefined}
            onSave={async (body) => {
              await save.mutateAsync({ body })
            }}
          />
          <Attachments target={`design:${n.id}`} editable={n.can_propose} />
          {(n.additions?.length ?? 0) > 0 && <Additions note={n} />}
          <SubNotes note={n} onAdd={people ? () => setSubNote(true) : undefined} />
          <Links at={{ type: 'design', id: n.id }} editable={n.can_edit || n.can_propose} />
          <Usage type="design" id={n.id} name={n.title} />
          <History note={n} />
        </div>
      )}
      <NewNoteDialog
        open={subNote}
        onClose={() => setSubNote(false)}
        parent={n}
        defaultScope={n.project_key || 'global'}
      />
      <DeleteNoteDialog
        open={deleting}
        note={n}
        onClose={() => setDeleting(false)}
        onRetire={() => save.mutate({ status: 'retired' })}
        onDelete={() => del.mutate()}
      />
    </article>
  )
}

function KindPicker({
  value,
  onChange,
  disabled,
}: {
  value: DesignKind
  onChange: (k: DesignKind) => void
  disabled?: boolean
}) {
  const pop = usePopover()
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Kind">
        <KindIcon kind={value} className="text-accent" />
        {DESIGN_KIND_LABEL[value]}
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={DESIGN_KINDS.map((k) => ({
            value: k,
            label: DESIGN_KIND_LABEL[k],
            icon: <KindIcon kind={k} />,
          }))}
          selected={[value]}
          placeholder="Kind…"
          onClose={pop.close}
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

const STATUS_HINT: Record<DesignStatus, string> = {
  active: 'what we intend now',
  superseded: 'replaced by another note',
  retired: 'no longer wanted',
}

function StatusPicker({
  value,
  onChange,
  disabled,
}: {
  value: DesignStatus
  onChange: (s: DesignStatus) => void
  disabled?: boolean
}) {
  const pop = usePopover()
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Status">
        <span
          className={clsx('size-2 rounded-full', value === 'active' ? 'bg-ok' : 'bg-line-strong')}
        />
        <span className={clsx(value !== 'active' && 'text-muted')}>
          {value[0].toUpperCase() + value.slice(1)}
        </span>
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={DESIGN_STATUSES.map((s) => ({
            value: s,
            label: s[0].toUpperCase() + s.slice(1),
            hint: STATUS_HINT[s],
          }))}
          selected={[value]}
          placeholder="Status…"
          onClose={pop.close}
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

/** Put the note under another note in the same place, or at the top. */
function ParentPicker({
  note: n,
  onChange,
  disabled,
}: {
  note: DesignNote
  onChange: (parent: number | null) => void
  disabled?: boolean
}) {
  const pop = usePopover()
  const notes = useQuery({
    ...designQuery({ project: n.project_key || 'global', own: true, status: 'all' }),
    enabled: pop.open,
  }).data
  const parent = n.ancestors?.[n.ancestors.length - 1]
  const below = useMemo(() => {
    const out = new Set<number>([n.id])
    let grew = true
    while (grew) {
      grew = false
      for (const x of notes ?? [])
        if (x.parent_id && out.has(x.parent_id) && !out.has(x.id)) {
          out.add(x.id)
          grew = true
        }
    }
    return out
  }, [notes, n.id])
  const items: PickItem<number>[] = [
    { value: 0, label: 'Top level' },
    ...(notes ?? [])
      .filter((x) => !below.has(x.id))
      .map((x) => ({
        value: x.id,
        label: x.title,
        hint: DESIGN_KIND_LABEL[x.kind],
        icon: <KindIcon kind={x.kind} />,
      })),
  ]
  if (disabled && !parent) return null
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Parent note">
        <ChevronRight className="size-3.5 text-subtle" />
        <span className="max-w-48 truncate text-muted">
          {parent ? `Under ${parent.title}` : 'Top level'}
        </span>
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={items}
          selected={[parent?.id ?? 0]}
          placeholder="Put it under…"
          onClose={pop.close}
          onPick={(v) => v !== (parent?.id ?? 0) && onChange(v || null)}
        />
      </Popover>
    </>
  )
}

/** Approved by a person, or proposed by an agent and waiting for one. */
function ProvenanceBanner({
  note: n,
  onApprove,
  approving,
}: {
  note: DesignNote
  onApprove: () => void
  approving: boolean
}) {
  if (!n.approved)
    return (
      <div className="flex flex-wrap items-center gap-3 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2.5">
        <AlertTriangle className="size-4 shrink-0 text-warn" />
        <div className="min-w-0 flex-1 text-[13px]">
          <div className="font-medium">
            Proposed by {n.created_agent ? agentLabel(n.created_agent) : 'an agent'}{' '}
            <time title={dateTime(n.created_at)}>{relativeTime(n.created_at)}</time> — not approved
            yet
          </div>
          <div className="text-xs text-muted">
            Agents treat it as a suggestion, not as what people want, until someone approves it.
          </div>
        </div>
        {n.can_approve && (
          <Button
            size="sm"
            variant="primary"
            icon={<ShieldCheck className="size-3.5" />}
            loading={approving}
            onClick={onApprove}
          >
            Approve
          </Button>
        )}
      </div>
    )
  const byAgent = !!n.created_agent
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-subtle">
      {n.requested && !n.approved_by ? (
        <Link2 className="size-3.5 shrink-0" />
      ) : (
        <ShieldCheck className="size-3.5 shrink-0 text-ok" />
      )}
      <span>
        {n.requested && !n.approved_by ? (
          <>
            Written for{' '}
            <Link
              to="/issue/$key"
              params={{ key: n.requested.issue }}
              className="font-medium text-fg hover:underline"
            >
              {n.requested.issue}
            </Link>
            , which <Who user={n.requested.by} /> filed
          </>
        ) : (
          <>
            Approved by <Who user={n.approved_by} />
          </>
        )}
        {n.approved_at && !(n.requested && !n.approved_by) && (
          <>
            {' '}
            <time title={dateTime(n.approved_at)}>{relativeTime(n.approved_at)}</time>
          </>
        )}
      </span>
      {byAgent && (
        <span>
          · {n.requested ? 'written' : 'proposed'} by{' '}
          <Who agent={n.created_agent} user={n.created_by} />{' '}
          <time title={dateTime(n.created_at)}>{relativeTime(n.created_at)}</time>
        </span>
      )}
      {n.updated_at !== n.created_at && (
        <span>
          · last changed by <Who user={n.updated_by} agent={n.updated_agent} />{' '}
          <time title={dateTime(n.updated_at)}>{relativeTime(n.updated_at)}</time>
        </span>
      )}
    </div>
  )
}

const STATE_BADGE: Record<string, { label: string; className: string; title?: string }> = {
  approved: { label: 'approved', className: 'bg-ok/15 text-ok' },
  pending: {
    label: 'pending',
    className: 'bg-warn/15 text-warn',
    title: 'Unapproved agent work: waiting for a person',
  },
  merged: {
    label: 'merged',
    className: 'bg-accent-soft text-accent',
    title: 'Merged into the body',
  },
  rejected: { label: 'rejected', className: 'bg-sunken text-muted' },
  applied: {
    label: 'applied',
    className: 'bg-sunken text-muted',
    title:
      'An agent’s change that took effect without a person: to its own unapproved note, or to one written for a person’s task',
  },
}

function StateBadge({ state }: { state: string }) {
  const b = STATE_BADGE[state] ?? { label: state, className: 'bg-sunken text-muted' }
  return (
    <span
      title={b.title}
      className={clsx(
        'inline-flex h-[18px] shrink-0 items-center rounded px-1.5 text-[10px] font-medium uppercase tracking-wide',
        b.className,
      )}
    >
      {b.label}
    </span>
  )
}

/** Reject, with an optional note for whoever proposed it. */
function RejectButton({
  onReject,
  size = 'xs',
}: {
  onReject: (note: string) => void | Promise<unknown>
  size?: 'xs' | 'sm'
}) {
  const pop = usePopover()
  const [note, setNote] = useState('')
  return (
    <>
      <Button
        ref={pop.anchor}
        size={size}
        variant="ghost"
        icon={<X className="size-3.5" />}
        onClick={pop.toggle}
      >
        Reject
      </Button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <form
          className="w-full space-y-2 p-2 sm:w-72"
          onSubmit={(e) => {
            e.preventDefault()
            const done = () => {
              setNote('')
              pop.close()
            }
            const r = onReject(note.trim())
            if (r instanceof Promise) r.then(done, () => {})
            else done()
          }}
        >
          <Textarea
            autoFocus
            value={note}
            onChange={(e) => setNote(e.target.value)}
            rows={3}
            placeholder="Why? (optional; the agent sees it)"
            aria-label="Reason for rejecting"
            className="text-[13px]"
          />
          <div className="flex justify-end gap-2">
            <Button size="xs" variant="ghost" onClick={pop.close}>
              Cancel
            </Button>
            <Button size="xs" variant="danger" type="submit">
              Reject
            </Button>
          </div>
        </form>
      </Popover>
    </>
  )
}

function RevisionHead({ rev, children }: { rev: DesignRevision; children?: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
      <span>
        <Who user={rev.author} agent={rev.agent} />{' '}
        <time title={dateTime(rev.created_at)}>{relativeTime(rev.created_at)}</time>
      </span>
      <StateBadge state={rev.state} />
      {rev.state === 'pending' && rev.kind === 'addition' && (
        <span className="text-warn">unapproved agent addition</span>
      )}
      {children}
    </div>
  )
}

/** Appended text: approved, waiting, or merged into the body. */
function Additions({ note: n }: { note: DesignNote }) {
  const review = useReview(n.id)
  const list = n.additions ?? []
  return (
    <section>
      <BlockTitle>Additions</BlockTitle>
      <ul className="space-y-2">
        {list.map((a) => (
          <li
            key={a.id}
            className={clsx(
              'rounded-lg border px-3 py-2',
              a.state === 'pending' ? 'border-warn/40 bg-warn/5' : 'border-line',
              a.state === 'merged' && 'opacity-70',
            )}
          >
            <RevisionHead rev={a}>
              {!a.reviewed_by && a.state === 'applied' && a.review_note && (
                <span>· {a.review_note}</span>
              )}
              {a.reviewed_by && a.state !== 'pending' && (
                <span>
                  · {a.state} by <Who user={a.reviewed_by} />
                  {a.review_note ? `: “${a.review_note}”` : ''}
                </span>
              )}
            </RevisionHead>
            <div className="mt-1">
              <MarkdownEditor
                value={a.body}
                editable={false}
                onSave={noop}
                project={n.project_key}
              />
            </div>
            {n.can_approve &&
              (a.state === 'pending' || a.state === 'approved' || a.state === 'applied') && (
                <div className="mt-1.5 flex flex-wrap items-center justify-end gap-1">
                  {a.state === 'pending' && (
                    <>
                      <RejectButton
                        onReject={(note) => review.mutateAsync({ rev: a, action: 'reject', note })}
                      />
                      <Button
                        size="xs"
                        icon={<Check className="size-3.5" />}
                        onClick={() => review.mutate({ rev: a, action: 'approve' })}
                      >
                        Approve
                      </Button>
                    </>
                  )}
                  <Button
                    size="xs"
                    variant={a.state === 'pending' ? 'primary' : 'secondary'}
                    icon={<GitMerge className="size-3.5" />}
                    onClick={() => review.mutate({ rev: a, action: 'merge' })}
                    title="Append it to the note's body"
                  >
                    Merge into body
                  </Button>
                </div>
              )}
          </li>
        ))}
      </ul>
    </section>
  )
}

/** An edit's change to the title and body, against the note as it is now. */
function EditDiff({ note: n, rev }: { note: DesignNote; rev: DesignRevision }) {
  const titleChanged = rev.title !== undefined && rev.title !== '' && rev.title !== n.title
  return (
    <div className="space-y-1.5">
      {titleChanged && (
        <div className="text-[13px]">
          <span className="text-subtle">Title: </span>
          <span className="text-muted line-through decoration-danger/50">{n.title}</span>
          {' → '}
          <span className="font-medium">{rev.title}</span>
        </div>
      )}
      <DiffView lines={lineDiff(n.body ?? '', rev.body)} />
    </div>
  )
}

/** Pending edits: a full new title and body, applied only when a person approves. */
function Proposals({ note: n }: { note: DesignNote }) {
  const review = useReview(n.id)
  return (
    <section>
      <BlockTitle>Proposed edits</BlockTitle>
      <ul className="space-y-2">
        {(n.proposals ?? []).map((p) => (
          <li key={p.id} className="space-y-2 rounded-lg border border-warn/40 bg-warn/5 px-3 py-2">
            <RevisionHead rev={p}>
              <span className="text-warn">proposed edit</span>
            </RevisionHead>
            {p.stale && (
              <div className="flex items-start gap-1.5 text-xs text-warn">
                <AlertTriangle className="mt-px size-3.5 shrink-0" />
                Proposed against an older version: approving replaces changes made since. Compare
                carefully.
              </div>
            )}
            <EditDiff note={n} rev={p} />
            {n.can_approve && (
              <div className="flex justify-end gap-1">
                <RejectButton
                  onReject={(note) => review.mutateAsync({ rev: p, action: 'reject', note })}
                />
                <Button
                  size="xs"
                  variant="primary"
                  icon={<Check className="size-3.5" />}
                  onClick={() => review.mutate({ rev: p, action: 'approve' })}
                >
                  Approve edit
                </Button>
              </div>
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

function SubNotes({ note: n, onAdd }: { note: DesignNote; onAdd?: () => void }) {
  const kids = n.children ?? []
  if (!kids.length && !onAdd) return null
  return (
    <section>
      <BlockTitle
        actions={
          onAdd && (
            <button
              onClick={onAdd}
              className="inline-flex items-center gap-1 text-xs text-subtle hover:text-fg"
            >
              <Plus className="size-3.5" /> Add sub-note
            </button>
          )
        }
      >
        Sub-notes
      </BlockTitle>
      {kids.length ? (
        <ul className="overflow-hidden rounded-lg border border-line">
          {kids.map((c) => (
            <li key={c.id} className="border-b border-line last:border-b-0">
              <Link
                to="/design/$id"
                params={{ id: String(c.id) }}
                className="flex items-center gap-2 px-3 py-2 text-[13px] hover:bg-hover"
              >
                <KindIcon kind={c.kind} />
                <span
                  className={clsx(
                    'min-w-0 flex-1 truncate',
                    c.status !== 'active' && 'text-muted line-through',
                  )}
                >
                  {c.title}
                </span>
                {!c.approved && <PendingMark note={{ approved: false, pending: 0 }} />}
                <span className="text-xs text-subtle">{DESIGN_KIND_LABEL[c.kind]}</span>
              </Link>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-[13px] text-subtle">
          None yet. A feature’s screens, decisions and requirements go here.
        </p>
      )}
    </section>
  )
}

/** What a revision was, in words: "proposed an edit", "added", … */
function revisionVerb(r: DesignRevision) {
  if (r.kind === 'addition')
    return (r.state === 'approved' && !r.agent) || r.state === 'applied'
      ? 'added'
      : 'proposed an addition'
  if (r.review_note?.startsWith('merged addition')) return 'wrote an addition merged into the body'
  if (r.state === 'applied') return r.review_note ? 'edited' : 'edited (before approval)'
  if (r.state === 'approved' && !r.reviewed_by) return 'edited'
  return 'proposed an edit'
}

/** Every revision, newest first: the audit trail of who intended what, when. */
function History({ note: n }: { note: DesignNote }) {
  const revs = n.history ?? []
  const [all, setAll] = useState(false)
  const [open, setOpen] = useState<number | null>(null)
  const byId = useMemo(() => new Map(revs.map((r) => [r.id, r])), [revs])
  if (!revs.length) return null
  const shown = all ? revs : revs.slice(0, 8)
  return (
    <section>
      <BlockTitle>History</BlockTitle>
      <ol className="overflow-hidden rounded-lg border border-line">
        {shown.map((r) => {
          const expanded = open === r.id
          const base = r.base_id ? byId.get(r.base_id) : undefined
          const reviewedLate =
            r.reviewed_by &&
            r.reviewed_at &&
            Math.abs(Date.parse(r.reviewed_at) - Date.parse(r.created_at)) > 1000
          return (
            <li key={r.id} className="border-b border-line last:border-b-0">
              <button
                onClick={() => setOpen(expanded ? null : r.id)}
                aria-expanded={expanded}
                className="flex w-full items-start gap-2 px-3 py-2 text-left text-[13px] hover:bg-hover"
              >
                {expanded ? (
                  <ChevronDown className="mt-0.5 size-3.5 shrink-0 text-subtle" />
                ) : (
                  <ChevronRight className="mt-0.5 size-3.5 shrink-0 text-subtle" />
                )}
                <span className="min-w-0 flex-1 text-muted">
                  <Who user={r.author} agent={r.agent} /> {revisionVerb(r)}
                  {reviewedLate && (
                    <span className="text-subtle">
                      {' '}
                      · {r.state} by <Who user={r.reviewed_by} />{' '}
                      <time title={dateTime(r.reviewed_at!)}>{relativeTime(r.reviewed_at!)}</time>
                    </span>
                  )}
                  {r.review_note && !r.review_note.startsWith('merged addition') && (
                    <span className="text-subtle"> · “{r.review_note}”</span>
                  )}
                </span>
                <StateBadge state={r.state} />
                <time
                  className="w-20 shrink-0 text-right text-xs text-subtle"
                  title={dateTime(r.created_at)}
                >
                  {relativeTime(r.created_at)}
                </time>
              </button>
              {expanded && (
                <div className="space-y-2 border-t border-line bg-sunken/40 px-3 py-2 text-[13px]">
                  {r.kind === 'edit' && r.title && <div className="font-medium">{r.title}</div>}
                  {r.kind === 'edit' && base ? (
                    <DiffView lines={lineDiff(base.body, r.body)} />
                  ) : (
                    <pre className="max-h-80 overflow-auto whitespace-pre-wrap font-sans text-muted">
                      {r.body || '(empty)'}
                    </pre>
                  )}
                  <div className="text-xs text-subtle">
                    #{r.id} · {dateTime(r.created_at)}
                    {r.reviewed_at && ` · reviewed ${dateTime(r.reviewed_at)}`}
                  </div>
                </div>
              )}
            </li>
          )
        })}
      </ol>
      {revs.length > 8 && (
        <button onClick={() => setAll(!all)} className="mt-1 text-xs text-subtle hover:text-fg">
          {all ? 'Show fewer' : `Show all ${revs.length}`}
        </button>
      )}
    </section>
  )
}

function DeleteNoteDialog({
  open,
  note: n,
  onClose,
  onRetire,
  onDelete,
}: {
  open: boolean
  note: DesignNote
  onClose: () => void
  onRetire: () => void
  onDelete: () => void
}) {
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Delete “${n.title}”?`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          {n.status === 'active' && (
            <Button
              autoFocus
              onClick={() => {
                onRetire()
                onClose()
              }}
            >
              Retire instead
            </Button>
          )}
          <Button
            variant="danger"
            onClick={() => {
              onDelete()
              onClose()
            }}
          >
            Delete
          </Button>
        </>
      }
    >
      <div className="space-y-2 px-4 py-4 text-sm text-muted">
        <p>
          Deleting removes the note, its additions and its history for good
          {n.child_count
            ? `; its ${n.child_count} sub-note${n.child_count === 1 ? '' : 's'} move to the top level`
            : ''}
          .
        </p>
        <p>
          <b className="font-medium text-fg">Retiring keeps the record</b>: the note leaves the
          active list and agents stop treating it as intent, but you can still see what was meant
          and when.
        </p>
      </div>
    </Dialog>
  )
}

/** What `wp spec` adds to the note: its parts, related notes, terms, tasks, changes and files. */
function specIsEmpty(s: {
  parts: unknown[]
  related: unknown[]
  terms: unknown[]
  issues: unknown[]
  changes: unknown[]
  files: unknown[]
  others: unknown[]
}) {
  return (
    !s.parts.length &&
    !s.related.length &&
    !s.terms.length &&
    !s.issues.length &&
    !s.changes.length &&
    !s.files.length &&
    !s.others.length
  )
}

function SpecView({ note: n }: { note: DesignNote }) {
  const spec = useQuery(specQuery(String(n.id), n.project_key))
  const s = spec.data
  const empty = !!s && specIsEmpty(s)
  return (
    <div className="space-y-5">
      {!s ? (
        spec.isError ? (
          <p className="text-[13px] text-danger">{spec.error.message}</p>
        ) : (
          <Skeleton className="h-64" />
        )
      ) : empty ? (
        <p className="text-[13px] text-muted">Nothing beyond the note yet.</p>
      ) : (
        <>
          {s.parts.length > 0 && (
            <SpecSection title="Parts">
              {s.parts.map((p) => (
                <details key={p.id} className="rounded-lg border border-line px-3 py-2">
                  <summary className="flex cursor-pointer items-center gap-2 text-[13px]">
                    <KindIcon kind={p.kind} />
                    <Link
                      to="/design/$id"
                      params={{ id: String(p.id) }}
                      className="min-w-0 flex-1 truncate hover:underline"
                    >
                      {p.title}
                    </Link>
                    <PendingMark note={p} />
                    <span className="text-xs text-subtle">{DESIGN_KIND_LABEL[p.kind]}</span>
                  </summary>
                  <div className="mt-1">
                    <MarkdownEditor value={p.body ?? ''} editable={false} onSave={noop} />
                  </div>
                </details>
              ))}
            </SpecSection>
          )}
          {s.related.length > 0 && (
            <SpecSection title="Related">
              <ul className="overflow-hidden rounded-lg border border-line">
                {s.related.map((r) => (
                  <li
                    key={`${r.rel}${r.direction}${r.note.id}`}
                    className="border-b border-line px-3 py-2 text-[13px] last:border-b-0"
                  >
                    <div className="flex items-center gap-2">
                      <KindIcon kind={r.note.kind} />
                      <span className="shrink-0 text-xs text-subtle">
                        {REL_LABEL[r.rel as keyof typeof REL_LABEL]?.[r.direction] ?? r.rel}
                      </span>
                      <Link
                        to="/design/$id"
                        params={{ id: String(r.note.id) }}
                        className="min-w-0 flex-1 truncate hover:underline"
                      >
                        {r.note.title}
                      </Link>
                    </div>
                    {r.summary && (
                      <p className="mt-0.5 line-clamp-2 text-xs text-muted">{r.summary}</p>
                    )}
                  </li>
                ))}
              </ul>
            </SpecSection>
          )}
          {s.terms.length > 0 && (
            <SpecSection title="Terms">
              <dl className="overflow-hidden rounded-lg border border-line">
                {s.terms.map((t) => (
                  <div key={t.id} className="border-b border-line px-3 py-2 last:border-b-0">
                    <dt className="flex items-center gap-1.5 text-[13px] font-semibold">
                      <Link
                        to="/glossary/$id"
                        params={{ id: String(t.id) }}
                        className="hover:underline"
                      >
                        {t.term}
                      </Link>
                      {t.locked && <Lock className="size-3 text-subtle" aria-label="Locked" />}
                    </dt>
                    <dd className="line-clamp-3 text-[13px] text-muted">{t.definition}</dd>
                  </div>
                ))}
              </dl>
            </SpecSection>
          )}
          {s.issues.length > 0 && (
            <SpecSection title="Tasks">
              <ItemList items={s.issues} />
            </SpecSection>
          )}
          {s.changes.length > 0 && (
            <SpecSection title="Some PRs and commits">
              <ItemList items={s.changes} />
            </SpecSection>
          )}
          {s.files.length > 0 && (
            <SpecSection title="Files">
              <ul className="space-y-0.5 font-mono text-[12px] text-muted">
                {s.files.map((f) => (
                  <li key={f}>{f}</li>
                ))}
              </ul>
            </SpecSection>
          )}
          {s.others.length > 0 && (
            <SpecSection title="Other notes that matched">
              <ul className="flex flex-wrap gap-1.5">
                {s.others.map((o: DesignRef) => (
                  <li key={o.id}>
                    <Link
                      to="/design/$id"
                      params={{ id: String(o.id) }}
                      className="inline-flex h-6 items-center gap-1 rounded-md bg-sunken px-2 text-xs hover:bg-hover"
                    >
                      <KindIcon kind={o.kind} className="size-3" />
                      {o.title}
                    </Link>
                  </li>
                ))}
              </ul>
            </SpecSection>
          )}
        </>
      )}
    </div>
  )
}

function SpecSection({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="space-y-1.5">
      <BlockTitle>{title}</BlockTitle>
      {children}
    </section>
  )
}

function ItemList({ items }: { items: KItem[] }) {
  return (
    <ul className="overflow-hidden rounded-lg border border-line">
      {items.map((it) => (
        <li
          key={`${it.type}:${it.id ?? it.ref}`}
          className="flex min-h-9 items-center gap-2 border-b border-line px-3 py-1.5 text-[13px] last:border-b-0"
        >
          <ItemIcon item={it} />
          <ItemTitle item={it} />
        </li>
      ))}
    </ul>
  )
}

/** /design/review: proposed notes, pending additions and edits, oldest first. */
export function DesignReviewPage() {
  const search = useSearch({ strict: false }) as { project?: string }
  const project = search.project ?? ''
  const qc = useQueryClient()
  const queue = useQuery(designReviewQuery(project))
  const items = queue.data ?? []
  useEffect(() => {
    document.title = 'Design review · Waypoint'
  }, [])
  const act = useMutation({
    mutationFn: (v: { item: ReviewItem; action: 'approve' | 'reject' | 'merge'; note?: string }) =>
      v.item.revision
        ? designApi.review(v.item.revision.id, v.action, v.note)
        : designApi.approve(v.item.note.id, v.item.note_updated_at),
    onMutate: ({ item }) => {
      const key = designReviewQuery(project).queryKey
      const prev = qc.getQueryData<ReviewItem[]>(key)
      qc.setQueryData<ReviewItem[]>(key, (old) => old?.filter((x) => x !== item))
      return { prev }
    },
    onSuccess: (n) => afterNoteChange(qc, n),
    onError: (e, _v, ctx) => {
      if (ctx?.prev) qc.setQueryData(designReviewQuery(project).queryKey, ctx.prev)
      toastError(e)
    },
  })
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-3xl">
        <PageHeader
          title="Design review"
          icon={<ClipboardCheck className="size-5 text-subtle" />}
          actions={
            <Link
              to={project ? '/p/$key/design' : '/design'}
              params={project ? { key: project } : undefined}
              className="text-[13px] text-muted hover:text-fg"
            >
              All notes
            </Link>
          }
        />
        <div className="space-y-2 px-4 sm:px-6">
          {queue.isPending ? (
            <>
              <Skeleton className="h-24" />
              <Skeleton className="h-24" />
            </>
          ) : queue.isError ? (
            <p className="text-[13px] text-danger">{queue.error.message}</p>
          ) : items.length === 0 ? (
            <Empty icon={<Check />} title="Nothing waiting">
              When agents propose notes, add to approved ones or suggest edits, they wait here for
              you.
            </Empty>
          ) : (
            items.map((it) => (
              <ReviewCard
                key={`${it.note.id}:${it.revision?.id ?? 'note'}`}
                item={it}
                onAct={(action, note) => act.mutate({ item: it, action, note })}
              />
            ))
          )}
        </div>
      </div>
    </div>
  )
}

function ReviewCard({
  item: it,
  onAct,
}: {
  item: ReviewItem
  onAct: (action: 'approve' | 'reject' | 'merge', note?: string) => void
}) {
  const r = it.revision
  const what = !r ? 'Proposed note' : r.kind === 'addition' ? 'Addition' : 'Proposed edit'
  return (
    <article className="wp-surface space-y-2 rounded-xl border border-line bg-panel p-3">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <KindIcon kind={it.note.kind} />
        <Link
          to="/design/$id"
          params={{ id: String(it.note.id) }}
          className="min-w-0 flex-1 truncate text-[13px] font-medium hover:underline"
        >
          {it.note.title}
        </Link>
        <ScopeChip
          scope={{ project_id: it.note.project_key ? 1 : null, project_key: it.note.project_key }}
        />
      </div>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
        <span className="rounded bg-warn/15 px-1.5 py-px text-[10px] font-medium uppercase tracking-wide text-warn">
          {what}
        </span>
        <span>
          by <Who user={r?.author} agent={it.agent || r?.agent} />{' '}
          <time title={dateTime(it.created_at)}>{relativeTime(it.created_at)}</time>
        </span>
        {r?.stale && (
          <span className="inline-flex items-center gap-1 text-warn">
            <AlertTriangle className="size-3" /> against an older version
          </span>
        )}
      </div>
      {r && (
        <div className="max-h-48 overflow-hidden rounded-lg bg-sunken/50 px-3 py-2">
          {r.kind === 'edit' && r.title && <div className="text-[13px] font-medium">{r.title}</div>}
          <MarkdownEditor
            value={r.body}
            editable={false}
            onSave={noop}
            project={it.note.project_key}
          />
        </div>
      )}
      <div className="flex flex-wrap items-center justify-end gap-1">
        {r?.kind === 'edit' && (
          <Link
            to="/design/$id"
            params={{ id: String(it.note.id) }}
            className="mr-auto text-xs text-accent hover:underline"
          >
            See the change on the note
          </Link>
        )}
        {r && <RejectButton onReject={(note) => onAct('reject', note)} />}
        {r?.kind === 'addition' && (
          <Button size="xs" icon={<GitMerge className="size-3.5" />} onClick={() => onAct('merge')}>
            Merge into body
          </Button>
        )}
        <Button
          size="xs"
          variant="primary"
          icon={<Check className="size-3.5" />}
          onClick={() => onAct('approve')}
        >
          Approve
        </Button>
      </div>
    </article>
  )
}
