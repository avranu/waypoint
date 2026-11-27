import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookA,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Lock,
  LockOpen,
  MoreHorizontal,
  Plus,
  Search,
  Trash2,
  X,
} from 'lucide-react'
import { ApiError } from '@/lib/api'
import { q } from '@/lib/queries'
import { useDebounced, useLocal } from '@/lib/hooks'
import { dateTime, relativeTime } from '@/lib/format'
import {
  glossaryApi,
  glossaryEntryQuery,
  glossaryQuery,
  invalidateKnowledge,
  lineDiff,
  snippetHTML,
} from '@/lib/knowledge'
import type { GlossaryEntry, GlossaryRevision } from '@/lib/types'
import { Usage } from '@/components/Usage'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Input, Select, Switch } from '@/components/ui/input'
import { MarkdownField } from '@/components/editor/MarkdownField'
import { Empty, PageHeader, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { MarkdownEditor } from '@/components/issue/Description'
import { Tip } from '@/components/ui/help'
import { Links } from '@/components/Links'
import { WrittenBy } from '@/components/Provenance'
import {
  BlockTitle,
  DiffView,
  InlineTitle,
  Provenance,
  ScopeChip,
  ScopePicker,
  Who,
  useScopeOptions,
} from '@/components/knowledge'

/** /glossary: every term you can see, or one project's own (?project=). */
export function GlossaryPage() {
  const search = useSearch({ strict: false }) as { project?: string }
  const navigate = useNavigate()
  const projects = useQuery(q.projects()).data ?? []
  const project = search.project ?? ''
  useEffect(() => {
    document.title = 'Glossary · Waypoint'
  }, [])
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-3xl">
        <PageHeader title="Glossary" icon={<BookA className="size-5 text-subtle" />} />
        <div className="px-4 sm:px-6">
          <GlossaryList
            params={{ project: project || undefined, own: !!project && project !== 'global' }}
            filter={
              <Select
                aria-label="Project"
                value={project}
                onChange={(e) =>
                  navigate({
                    to: '/glossary',
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
            addTo={project && project !== 'global' ? project : undefined}
          />
        </div>
      </div>
    </div>
  )
}

/** /p/$key/glossary: the project's full glossary, inherited terms marked. */
export function ProjectGlossaryPage() {
  const { key } = useParams({ from: '/app/p/$key/glossary' })
  const [own, setOwn] = useLocal(`glossary-own-${key}`, true)
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-3xl px-3 pt-3 sm:px-6 sm:pt-5">
        <GlossaryList
          params={{ project: key, own }}
          project={key}
          addTo={key}
          filter={
            <label className="flex shrink-0 items-center gap-2 text-xs text-muted">
              <Switch checked={own} onChange={setOwn} label="Only this project's terms" />
              <span className="hidden sm:inline">Only this project's</span>
            </label>
          }
        />
      </div>
    </div>
  )
}

function GlossaryList({
  params,
  project,
  filter,
  addTo,
}: {
  params: { project?: string; own?: boolean }
  /** set on a project's tab: inherited entries are marked */
  project?: string
  filter: React.ReactNode
  /** the scope new terms default to */
  addTo?: string
}) {
  const [text, setText] = useState('')
  const debounced = useDebounced(text.trim(), 200)
  const [adding, setAdding] = useState(false)
  const list = useQuery({
    ...glossaryQuery({ ...params, q: debounced || undefined }),
    placeholderData: (p) => p,
  })
  const entries = list.data ?? []
  const scopes = useScopeOptions(project)
  const letters = useMemo(() => {
    const m = new Map<string, GlossaryEntry[]>()
    for (const e of entries) {
      const l = /[a-z]/i.test(e.term[0]) ? e.term[0].toUpperCase() : '#'
      m.set(l, [...(m.get(l) ?? []), e])
    }
    return [...m.entries()]
  }, [entries])
  return (
    <div className="space-y-3">
      <div className="flex items-center gap-2">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Search terms…"
            aria-label="Search the glossary"
            className="h-9 w-full rounded-lg border border-line bg-panel pl-8 pr-2 text-[13px] outline-none placeholder:text-subtle focus:border-accent"
          />
        </div>
        {filter}
        {scopes.length > 0 && (
          <Button
            variant="primary"
            size="sm"
            icon={<Plus className="size-3.5" />}
            onClick={() => setAdding(!adding)}
            aria-expanded={adding}
            className="shrink-0"
            aria-label="New term"
          >
            <span className="max-sm:hidden">New term</span>
          </Button>
        )}
      </div>
      {adding && (
        <QuickAdd
          defaultScope={addTo ?? scopes[0]?.value ?? 'global'}
          project={project}
          onClose={() => setAdding(false)}
        />
      )}
      <div className="wp-surface overflow-hidden rounded-xl border border-line bg-panel">
        {list.isPending ? (
          <div className="space-y-2 p-3">
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
            <Skeleton className="h-10" />
          </div>
        ) : list.isError ? (
          <p className="p-4 text-[13px] text-danger">{list.error.message}</p>
        ) : entries.length === 0 ? (
          <Empty icon={<BookA />} title={debounced ? 'No terms match' : 'No terms yet'}>
            {debounced
              ? 'Try fewer words.'
              : 'Define the words your team and agents use, so they mean the same thing to everyone.'}
          </Empty>
        ) : debounced ? (
          <ul>
            {entries.map((e) => (
              <EntryRow key={e.id} entry={e} project={project} />
            ))}
          </ul>
        ) : (
          letters.map(([letter, group]) => (
            <section key={letter}>
              <h2 className="sticky top-0 z-[1] border-b border-line bg-sunken/90 px-4 py-1 text-[11px] font-semibold text-subtle backdrop-blur">
                {letter}
              </h2>
              <ul>
                {group.map((e) => (
                  <EntryRow key={e.id} entry={e} project={project} />
                ))}
              </ul>
            </section>
          ))
        )}
      </div>
      {entries.length > 0 && (
        <p className="px-1 text-xs text-subtle">
          {entries.length} term{entries.length === 1 ? '' : 's'}
          {project &&
            entries.some((e) => e.inherited) &&
            ' · inherited terms show where they come from'}
        </p>
      )}
    </div>
  )
}

/** The definition's first line as plain text, for the list. */
const firstLine = (md: string) =>
  md
    .replace(/\[\[([^\]|]*)(?:\|([^\]]*))?\]\]/g, (_m, t, l) => l || t)
    .replace(/[#>*_`~]|\[|\]\([^)]*\)/g, '')
    .split('\n')
    .find((l) => l.trim()) ?? ''

function EntryRow({ entry: e, project }: { entry: GlossaryEntry; project?: string }) {
  return (
    <li className="border-b border-line last:border-b-0">
      <Link
        to="/glossary/$id"
        params={{ id: String(e.id) }}
        className={clsx('flex gap-3 px-4 py-2.5 hover:bg-hover', e.shadowed && 'opacity-60')}
      >
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 items-center gap-1.5">
            <span className={clsx('truncate font-semibold', e.shadowed && 'line-through')}>
              {e.term}
            </span>
            {e.locked && <Lock className="size-3 shrink-0 text-subtle" aria-label="Locked" />}
            {e.unreviewed && (
              <span
                className="shrink-0 text-[11px] font-medium text-warn"
                title={`Written by ${e.updated_agent}; no person has reviewed it`}
              >
                Not reviewed
              </span>
            )}
          </div>
          {e.snippet ? (
            <p
              className="line-clamp-2 text-[13px] text-muted"
              dangerouslySetInnerHTML={{ __html: snippetHTML(e.snippet) }}
            />
          ) : (
            <p className="truncate text-[13px] text-muted">{firstLine(e.definition)}</p>
          )}
        </div>
        <div className="flex shrink-0 flex-col items-end gap-1 pt-0.5">
          {(!project || e.inherited) && <ScopeChip scope={e} />}
          {e.last_used_at && (
            <span
              className="whitespace-nowrap text-[11px] text-subtle"
              title={`Looked up or named ${e.uses ?? 0} times in the last 90 days; last ${dateTime(e.last_used_at)}`}
            >
              {e.uses ? `used ${e.uses}× · ` : 'last used '}
              {relativeTime(e.last_used_at)}
            </span>
          )}
          {e.shadowed && (
            <span
              className="text-[10px] font-medium uppercase tracking-wide text-warn"
              title="Defined again nearer to this project; that definition wins"
            >
              overridden
            </span>
          )}
        </div>
      </Link>
    </li>
  )
}

function QuickAdd({
  defaultScope,
  project,
  onClose,
}: {
  defaultScope: string
  project?: string
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [term, setTerm] = useState('')
  const [definition, setDefinition] = useState('')
  const [aliases, setAliases] = useState('')
  const [scope, setScope] = useState(defaultScope)
  const create = useMutation({
    mutationFn: () =>
      glossaryApi.create({
        term: term.trim(),
        definition: definition.trim(),
        aliases: aliases
          .split(',')
          .map((a) => a.trim())
          .filter(Boolean),
        project: scope === 'global' ? '' : scope,
      }),
    onSuccess: (e) => {
      invalidateKnowledge(qc)
      toast(`Added “${e.term}”`, { href: `/glossary/${e.id}` })
      setTerm('')
      setDefinition('')
      setAliases('')
    },
    onError: toastError,
  })
  return (
    <form
      className="wp-surface space-y-2 rounded-xl border border-line bg-panel p-3"
      onSubmit={(e) => {
        e.preventDefault()
        if (term.trim()) create.mutate()
      }}
    >
      <div className="flex flex-col gap-2 sm:flex-row">
        <Input
          autoFocus
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          placeholder="Term"
          aria-label="Term"
          className="sm:w-48"
        />
        <Input
          value={aliases}
          onChange={(e) => setAliases(e.target.value)}
          placeholder="Aliases, comma separated"
          aria-label="Aliases"
          className="flex-1"
        />
      </div>
      <MarkdownField
        value={definition}
        onValueChange={setDefinition}
        placeholder="What it means (markdown; [[links]] to other terms, notes and issues)"
        ariaLabel="Definition"
        rows={3}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && term.trim()) {
            e.preventDefault()
            create.mutate()
          }
        }}
      />
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-subtle">In</span>
        <ScopePicker value={scope} onChange={setScope} project={project} label="Add to…" />
        <span className="flex-1" />
        <Button variant="ghost" size="sm" onClick={onClose}>
          Close
        </Button>
        <Button
          type="submit"
          variant="primary"
          size="sm"
          disabled={!term.trim()}
          loading={create.isPending}
        >
          Add term
        </Button>
      </div>
    </form>
  )
}

/** /glossary/$id */
export function GlossaryEntryPage() {
  const { id } = useParams({ from: '/app/glossary/$id' })
  const entry = useQuery(glossaryEntryQuery(Number(id)))
  const e = entry.data
  useEffect(() => {
    if (e) document.title = `${e.term} · Glossary · Waypoint`
  }, [e])
  if (entry.isPending) return <Skeleton className="m-6 h-64" />
  if (!e)
    return (
      <Empty icon={<BookA />} title="Term not found">
        {entry.error instanceof ApiError && entry.error.status === 404
          ? 'It may have been deleted, or it belongs to a project you can’t see.'
          : entry.error?.message}{' '}
        <Link to="/glossary" className="text-accent hover:underline">
          The glossary
        </Link>
      </Empty>
    )
  return <EntryView key={e.id} entry={e} />
}

function EntryView({ entry: e }: { entry: GlossaryEntry }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const menu = usePopover()
  const [deleting, setDeleting] = useState(false)
  const editable = e.can_edit
  const key = glossaryEntryQuery(e.id).queryKey
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => glossaryApi.update(e.id, body),
    onMutate: (body) => {
      const prev = qc.getQueryData<GlossaryEntry>(key)
      if (prev && ('locked' in body || 'aliases' in body || 'avoid' in body))
        qc.setQueryData<GlossaryEntry>(key, { ...prev, ...body })
      return { prev }
    },
    onSuccess: (ne) => {
      qc.setQueryData(key, ne)
      invalidateKnowledge(qc)
    },
    onError: (err, _b, ctx) => {
      if (ctx?.prev) qc.setQueryData(key, ctx.prev)
      toastError(err)
    },
  })
  const del = useMutation({
    mutationFn: () => glossaryApi.remove(e.id),
    onSuccess: async () => {
      toast(`Deleted “${e.term}”`)
      await navigate(
        e.project_key
          ? { to: '/p/$key/glossary', params: { key: e.project_key } }
          : { to: '/glossary' },
      )
      qc.removeQueries({ queryKey: glossaryEntryQuery(e.id).queryKey })
      qc.removeQueries({ queryKey: ['links', 'glossary', e.id] })
      invalidateKnowledge(qc)
    },
    onError: toastError,
  })
  return (
    <article className="mx-auto w-full max-w-3xl flex-1 space-y-5 overflow-y-auto px-4 pb-16 pt-3 sm:px-8 sm:pt-6">
      <div className="flex items-center gap-2 text-xs text-subtle">
        <nav aria-label="Breadcrumbs" className="flex min-w-0 items-center gap-1">
          {e.project_key ? (
            <Link
              to="/p/$key/glossary"
              params={{ key: e.project_key }}
              className="inline-flex items-center gap-1 hover:text-fg"
            >
              <ChevronLeft className="size-3.5" />
              {e.project_name ?? e.project_key} glossary
            </Link>
          ) : (
            <Link
              to="/glossary"
              search={{ project: 'global' }}
              className="inline-flex items-center gap-1 hover:text-fg"
            >
              <ChevronLeft className="size-3.5" />
              Global glossary
            </Link>
          )}
        </nav>
        <span className="ml-auto shrink-0">Updated {relativeTime(e.updated_at)}</span>
        {editable && (
          <>
            <IconButton
              ref={menu.anchor}
              label="Term actions"
              onClick={menu.toggle}
              icon={<MoreHorizontal className="size-4" />}
            />
            <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
              <MenuList
                onClose={menu.close}
                items={[
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
      <div className="space-y-2">
        <InlineTitle
          value={e.term}
          editable={editable}
          label="Term"
          onSave={(term) => save.mutate({ term })}
        />
        <div className="flex flex-wrap items-center gap-1">
          <ScopePicker
            value={e.project_key || 'global'}
            disabled={!editable}
            onChange={(v) => save.mutate({ project: v === 'global' ? '' : v })}
          />
          <LockRow entry={e} editable={editable} onChange={(locked) => save.mutate({ locked })} />
        </div>
        {(editable || e.aliases.length > 0 || (e.avoid?.length ?? 0) > 0) && (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
            <Aliases
              value={e.aliases}
              editable={editable}
              onChange={(aliases) => save.mutate({ aliases })}
            />
            <Aliases
              avoid
              value={e.avoid ?? []}
              editable={editable}
              onChange={(avoid) => save.mutate({ avoid })}
            />
          </div>
        )}
      </div>
      <MarkdownEditor
        value={e.definition}
        editable={editable}
        project={e.project_key}
        placeholder="Write the definition… (markdown; [[links]] to terms, notes, pages and issues)"
        draftKey={editable ? `glossary:${e.id}` : undefined}
        minRows={5}
        onSave={async (definition) => {
          await save.mutateAsync({ definition })
        }}
      />
      <Provenance
        created={{ user: e.created_by, agent: e.created_agent, at: e.created_at }}
        updated={{ user: e.updated_by, agent: e.updated_agent, at: e.updated_at }}
      />
      <WrittenBy
        agent={e.updated_agent}
        unreviewed={e.unreviewed}
        reviewedBy={e.reviewed_by}
        review={editable ? { type: 'glossary', ref: String(e.id) } : undefined}
        stateOnly
      />
      <Links at={{ type: 'glossary', id: e.id }} editable={editable} />
      <Usage type="glossary" id={e.id} name={e.term} />
      <History revisions={e.history ?? []} />
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        title={`Delete “${e.term}”?`}
        body="The term, its history and its links go. Wikilinks to it stay as plain text. This can't be undone."
        onConfirm={() => del.mutate()}
      />
    </article>
  )
}

function LockRow({
  entry: e,
  editable,
  onChange,
}: {
  entry: GlossaryEntry
  editable: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <span className="inline-flex items-center gap-0.5">
      <button
        type="button"
        disabled={!editable}
        aria-pressed={e.locked}
        aria-label={e.locked ? 'Unlock term' : 'Lock term'}
        onClick={() => onChange(!e.locked)}
        className={clsx(
          'inline-flex h-7 items-center gap-1 rounded-md px-2 text-[13px]',
          e.locked ? 'bg-accent-soft text-accent' : 'text-muted hover:bg-hover',
        )}
      >
        {e.locked ? <Lock className="size-3.5" /> : <LockOpen className="size-3.5" />}
        {e.locked ? 'Locked' : 'Unlocked'}
      </button>
      <Tip label="About locking">
        {e.locked ? (
          <>
            Agents can’t change or delete this term. People can still edit it.
            {e.locked_by && e.locked_at && (
              <>
                {' '}
                Locked by <Who user={e.locked_by} />{' '}
                <time title={dateTime(e.locked_at)}>{relativeTime(e.locked_at)}</time>.
              </>
            )}
          </>
        ) : (
          'Agents can edit, move or delete this term. Lock it once the wording is settled.'
        )}
      </Tip>
    </span>
  )
}

/** Alias chips; type and press Enter (or comma) to add one. */
function Aliases({
  value,
  editable,
  onChange,
  avoid,
}: {
  value: string[]
  editable: boolean
  onChange: (v: string[]) => void
  /** words to avoid rather than aliases */
  avoid?: boolean
}) {
  const noun = avoid ? 'word to avoid' : 'alias'
  const [text, setText] = useState('')
  const [adding, setAdding] = useState(false)
  const placeholder = value.length ? 'Add…' : `Add ${avoid ? 'a word to avoid' : 'an alias'}…`
  const add = () => {
    const a = text.trim().replace(/,$/, '')
    setText('')
    if (a && !value.some((x) => x.toLowerCase() === a.toLowerCase())) onChange([...value, a])
  }
  if (!editable && value.length === 0) return null
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <span className="text-xs text-subtle">{avoid ? 'Avoid' : 'Aliases'}</span>
      {(avoid || value.length > 0 || editable) && (
        <Tip label={avoid ? 'About words to avoid' : 'About aliases'}>
          {avoid
            ? 'Words people use for this that aren’t used here. Looking one up points to this term.'
            : 'Other names for this term. Looking one up leads here.'}
        </Tip>
      )}
      {value.map((a) => (
        <span
          key={a}
          className="inline-flex h-6 items-center gap-1 rounded-md bg-sunken pl-2 pr-1 text-[13px]"
        >
          <span className={clsx(avoid && 'line-through decoration-subtle')}>{a}</span>
          {editable && (
            <button
              onClick={() => onChange(value.filter((x) => x !== a))}
              aria-label={`Remove ${noun} ${a}`}
              className="flex size-4 items-center justify-center rounded text-subtle hover:bg-hover hover:text-fg"
            >
              <X className="size-3" />
            </button>
          )}
        </span>
      ))}
      {editable && !adding && (
        <button
          type="button"
          onClick={() => setAdding(true)}
          aria-label={`Add ${avoid ? 'a word to avoid' : 'an alias'}`}
          className="inline-flex h-6 items-center gap-0.5 rounded-md px-1.5 text-xs text-muted hover:bg-hover hover:text-fg"
        >
          <Plus className="size-3" />
          {avoid ? 'avoid' : 'alias'}
        </button>
      )}
      {editable && adding && (
        <input
          autoFocus
          value={text}
          onChange={(e) => (e.target.value.endsWith(',') ? add() : setText(e.target.value))}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault()
              add()
            } else if (e.key === 'Escape') {
              setText('')
              setAdding(false)
            } else if (e.key === 'Backspace' && !text && value.length) onChange(value.slice(0, -1))
          }}
          onBlur={() => {
            add()
            setAdding(false)
          }}
          placeholder={placeholder}
          aria-label={`Add ${avoid ? 'a word to avoid' : 'an alias'}`}
          style={{ width: `${Math.max(placeholder.length, text.length) + 2}ch` }}
          className="h-6 rounded-md bg-transparent px-1 text-[13px] outline-none placeholder:text-subtle focus:bg-hover"
        />
      )}
    </div>
  )
}

/** Every saved state of the entry, newest first, with what changed. */
function History({ revisions }: { revisions: GlossaryRevision[] }) {
  const [all, setAll] = useState(false)
  const [open, setOpen] = useState<number | null>(null)
  if (revisions.length === 0) return null
  const shown = all ? revisions : revisions.slice(0, 5)
  return (
    <section>
      <BlockTitle>History</BlockTitle>
      <ol className="overflow-hidden rounded-lg border border-line">
        {shown.map((r, i) => {
          const prev = revisions[i + 1] as GlossaryRevision | undefined
          const changes = prev
            ? [
                r.term !== prev.term && `renamed to “${r.term}”`,
                r.aliases.join(',') !== prev.aliases.join(',') && 'aliases',
                (r.avoid ?? []).join(',') !== (prev.avoid ?? []).join(',') && 'words to avoid',
                r.definition !== prev.definition && 'definition',
                r.locked !== prev.locked && (r.locked ? 'locked' : 'unlocked'),
              ].filter(Boolean)
            : ['created']
          const expanded = open === r.id
          return (
            <li key={r.id} className="border-b border-line last:border-b-0">
              <button
                onClick={() => setOpen(expanded ? null : r.id)}
                aria-expanded={expanded}
                className="flex w-full items-center gap-2 px-3 py-2 text-left text-[13px] hover:bg-hover"
              >
                {expanded ? (
                  <ChevronDown className="size-3.5 shrink-0 text-subtle" />
                ) : (
                  <ChevronRight className="size-3.5 shrink-0 text-subtle" />
                )}
                <span className="min-w-0 flex-1 truncate text-muted">
                  <Who user={r.user} agent={r.agent} />{' '}
                  {changes.length ? changes.join(', ') : 'saved'}
                </span>
                <time className="shrink-0 text-xs text-subtle" title={dateTime(r.at)}>
                  {relativeTime(r.at)}
                </time>
              </button>
              {expanded && (
                <div className="space-y-2 border-t border-line bg-sunken/40 px-3 py-2 text-[13px]">
                  <div>
                    <b className="font-semibold">{r.term}</b>
                    {r.aliases.length > 0 && (
                      <span className="text-subtle"> · also {r.aliases.join(', ')}</span>
                    )}
                    {(r.avoid?.length ?? 0) > 0 && (
                      <span className="text-subtle"> · not {r.avoid!.join(', ')}</span>
                    )}
                    {r.locked && <span className="text-subtle"> · locked</span>}
                  </div>
                  {prev && prev.definition !== r.definition ? (
                    <DiffView lines={lineDiff(prev.definition, r.definition)} />
                  ) : (
                    <pre className="whitespace-pre-wrap font-sans text-muted">
                      {r.definition || '(no definition)'}
                    </pre>
                  )}
                </div>
              )}
            </li>
          )
        })}
      </ol>
      {revisions.length > 5 && (
        <button onClick={() => setAll(!all)} className="mt-1 text-xs text-subtle hover:text-fg">
          {all ? 'Show fewer' : `Show all ${revisions.length}`}
        </button>
      )}
    </section>
  )
}
