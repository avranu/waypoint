import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearch } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  FilePlus,
  FileText,
  FolderTree,
  MoreHorizontal,
  PenLine,
  Search,
  Trash2,
} from 'lucide-react'
import { api, ApiError } from '@/lib/api'
import { pageQuery, pagesQuery, pageTree } from '@/lib/pages'
import { q, usePrefetch, usePublishPage } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { useDebounced } from '@/lib/hooks'
import { PAGE_CATEGORIES, type Page } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { CodeChip, WrittenBy } from '@/components/Provenance'
import { DraftBanner } from '@/components/Drafts'
import { PlanHeading, PlanStory } from '@/components/PlanStory'
import { Links } from '@/components/Links'
import { Usage } from '@/components/Usage'
import { Empty, Skeleton } from '@/components/ui/misc'
import { MenuList, Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { MarkdownEditor } from '@/components/issue/Description'
import { Attachments } from '@/components/attachments/Attachments'
import { ProjectIcon } from '@/components/issue/icons'
import { PropButton } from '@/components/issue/pickers'

const catLabel = (c: string) => c[0].toUpperCase() + c.slice(1)

/** /pages and /pages/$id */
export function PagesPage() {
  const params = useParams({ strict: false }) as { id?: string }
  const id = params.id ? Number(params.id) : null
  useEffect(() => {
    if (!id) document.title = 'Pages · Waypoint'
  }, [id])
  return (
    <div className="flex min-h-0 flex-1">
      <aside
        className={clsx(
          'min-h-0 w-full shrink-0 flex-col border-r border-line bg-panel md:flex md:w-72',
          id ? 'hidden' : 'flex',
        )}
      >
        <PageTree selected={id} />
      </aside>
      <main className={clsx('min-w-0 flex-1 overflow-y-auto', !id && 'hidden md:block')}>
        {id ? <PageView key={id} id={id} /> : <PagesHome />}
      </main>
    </div>
  )
}

/** /p/$key/pages: the project's pages as a tree; pages open in the Pages area. */
export function ProjectPagesPage() {
  const { key } = useParams({ from: '/app/p/$key/pages' })
  return (
    <div className="mx-auto w-full max-w-3xl flex-1 overflow-y-auto p-3 sm:p-6">
      <div className="wp-surface overflow-hidden rounded-xl border border-line">
        <PageTree selected={null} project={key} />
      </div>
    </div>
  )
}

function PagesHome() {
  return (
    <Empty icon={<FileText />} title="Pages">
      <span title="Keep them personal or in a project, nest them, and link them to issues">
        Plans, ideas, decisions and notes. Pick one on the left or start a new one.
      </span>
    </Empty>
  )
}

function useCreatePage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  return useMutation({
    mutationFn: (body: {
      title: string
      project?: string
      parent?: number
      category?: string
      draft?: boolean
    }) => api.post<Page>('/pages', body),
    onSuccess: (p) => {
      qc.invalidateQueries({ queryKey: ['pages'] })
      navigate({ to: '/pages/$id', params: { id: String(p.id) } })
    },
    onError: toastError,
  })
}

function PageTree({ selected, project }: { selected: number | null; project?: string }) {
  const search = useSearch({ strict: false }) as { q?: string; category?: string }
  const navigate = useNavigate()
  const [text, setText] = useState(search.q ?? '')
  const debounced = useDebounced(text.trim(), 200)
  const category = search.category ?? ''
  const projects = useQuery(q.projects()).data ?? []
  const all = useQuery({
    ...pagesQuery({ q: debounced || undefined, category: category || undefined, project }),
    placeholderData: (p) => p,
  })
  const create = useCreatePage()
  const pages = all.data ?? []
  const filtering = !!debounced || !!category
  const groups = useMemo(() => {
    const m = new Map<string, Page[]>()
    for (const p of pages) {
      const k = p.project_key ?? ''
      m.set(k, [...(m.get(k) ?? []), p])
    }
    return [...m.entries()].sort(([a], [b]) => (a === '' ? -1 : b === '' ? 1 : a.localeCompare(b)))
  }, [pages])
  const setCategory = (c: string) =>
    navigate({
      to: '.',
      search: (s: Record<string, unknown>) => ({ ...s, category: c || undefined }),
      replace: true,
    } as never)
  return (
    <>
      <div className="space-y-2 border-b border-line p-3">
        <div className="flex items-center gap-2">
          <h1 className="flex-1 text-[15px] font-semibold tracking-tight">
            {project ? 'Project pages' : 'Pages'}
          </h1>
          <Button
            size="sm"
            variant="primary"
            icon={<FilePlus className="size-3.5" />}
            loading={create.isPending}
            onClick={() => create.mutate({ title: 'Untitled', project })}
          >
            New
          </Button>
          <Button
            size="sm"
            icon={<PenLine className="size-3.5" />}
            disabled={create.isPending}
            title="A new draft page: agents can't see it until you publish it"
            onClick={() => create.mutate({ title: 'Untitled draft', project, draft: true })}
          >
            Draft
          </Button>
        </div>
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Search pages…"
            aria-label="Search pages"
            className="h-8 w-full rounded-lg border border-line bg-panel pl-8 pr-2 text-[13px] outline-none placeholder:text-subtle focus:border-accent"
          />
        </div>
        <div className="no-scrollbar -mx-1 flex gap-1 overflow-x-auto px-1">
          {['', ...PAGE_CATEGORIES].map((c) => (
            <button
              key={c || 'all'}
              onClick={() => setCategory(c)}
              aria-pressed={category === c}
              className={clsx(
                'h-6 shrink-0 rounded-full px-2 text-xs',
                category === c ? 'bg-accent-soft text-fg' : 'text-muted hover:bg-hover',
              )}
            >
              {c ? catLabel(c) : 'All'}
            </button>
          ))}
        </div>
      </div>
      <nav aria-label="Pages" className="min-h-0 flex-1 overflow-y-auto p-2">
        {all.isPending ? (
          <Skeleton className="h-24" />
        ) : pages.length === 0 ? (
          <p className="px-2 py-6 text-center text-[13px] text-subtle">
            {filtering ? 'No pages match.' : 'No pages yet.'}
          </p>
        ) : (
          groups.map(([key, list]) => {
            const proj = projects.find((p) => p.key === key)
            return (
              <section key={key || 'personal'} className="mb-2">
                {!project && (
                  <div className="flex items-center gap-1.5 px-2 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                    {proj && (
                      <ProjectIcon icon={proj.icon} color={proj.color} name={proj.name} size={14} />
                    )}
                    {proj ? proj.name : 'Personal'}
                  </div>
                )}
                <Branch pages={list} parent={null} selected={selected} flat={filtering} depth={0} />
              </section>
            )
          })
        )}
      </nav>
    </>
  )
}

function Branch({
  pages,
  parent,
  selected,
  flat,
  depth,
}: {
  pages: Page[]
  parent: number | null
  selected: number | null
  flat: boolean
  depth: number
}) {
  const tree = useMemo(() => pageTree(pages), [pages])
  const level = flat ? pages : (tree.get(parent) ?? [])
  return (
    <ul>
      {level.map((p) => (
        <TreeNode
          key={p.id}
          page={p}
          pages={pages}
          tree={tree}
          selected={selected}
          flat={flat}
          depth={depth}
        />
      ))}
    </ul>
  )
}

function TreeNode({
  page,
  pages,
  tree,
  selected,
  flat,
  depth,
}: {
  page: Page
  pages: Page[]
  tree: Map<number | null, Page[]>
  selected: number | null
  flat: boolean
  depth: number
}) {
  const kids = flat ? [] : (tree.get(page.id) ?? [])
  const [open, setOpen] = useState(depth < 1)
  return (
    <li>
      <div
        className={clsx(
          'group flex h-8 items-center gap-1 rounded-lg pr-2 text-[13px]',
          selected === page.id ? 'bg-accent-soft font-medium' : 'hover:bg-hover',
        )}
        style={{ paddingLeft: 4 + depth * 14 }}
      >
        {kids.length > 0 ? (
          <button
            onClick={() => setOpen(!open)}
            aria-label={open ? `Collapse ${page.title}` : `Expand ${page.title}`}
            aria-expanded={open}
            className="flex size-5 shrink-0 items-center justify-center rounded text-subtle hover:text-fg"
          >
            {open ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
          </button>
        ) : (
          <span className="size-5 shrink-0" />
        )}
        <Link
          to="/pages/$id"
          params={{ id: String(page.id) }}
          className="flex min-w-0 flex-1 items-center gap-1.5"
        >
          <FileText className="size-3.5 shrink-0 text-subtle" />
          <span className="truncate">{page.title}</span>
          {page.draft && (
            <PenLine
              className="size-3 shrink-0"
              style={{ color: 'var(--wp-warn)' }}
              aria-label="Draft"
            />
          )}
        </Link>
        {page.code ? (
          <span className="shrink-0 font-mono text-[11px] text-subtle" title={page.category}>
            {page.code}
          </span>
        ) : (
          page.category && <span className="shrink-0 text-[11px] text-subtle">{page.category}</span>
        )}
      </div>
      {open && kids.length > 0 && (
        <ul>
          {kids.map((k) => (
            <TreeNode
              key={k.id}
              page={k}
              pages={pages}
              tree={tree}
              selected={selected}
              flat={flat}
              depth={depth + 1}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

function PageView({ id }: { id: number }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const page = useQuery(pageQuery(id))
  const p = page.data
  const editable = p?.role === 'owner' || p?.role === 'editor'
  const save = useMutation({
    mutationFn: (body: Record<string, unknown>) => api.patch<Page>(`/pages/${id}`, body),
    onSuccess: (np) => {
      qc.setQueryData(pageQuery(id).queryKey, np)
      qc.invalidateQueries({ queryKey: ['pages'] })
    },
    onError: (e) => {
      toastError(e)
      qc.invalidateQueries({ queryKey: ['page', id] })
    },
  })
  const [deleting, setDeleting] = useState(false)
  const del = useMutation({
    mutationFn: () => api.del(`/pages/${id}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['pages'] })
      navigate({ to: '/pages' })
      toast('Page deleted')
    },
    onError: toastError,
  })
  const create = useCreatePage()
  const publish = usePublishPage()
  const menu = usePopover()
  useEffect(() => {
    if (p) document.title = `${p.title} · Waypoint`
  }, [p])
  if (page.isPending) return <Skeleton className="m-6 h-64" />
  if (page.isError && !(page.error instanceof ApiError && page.error.status === 404))
    return (
      <Empty icon={<FileText />} title="Couldn't load this page">
        {page.error.message}. Reload to try again.
      </Empty>
    )
  if (!p)
    return (
      <Empty icon={<FileText />} title="Page not found">
        It may have been deleted, or it's someone else's personal page.{' '}
        <Link to="/pages" className="text-accent hover:underline">
          All pages
        </Link>
      </Empty>
    )
  return (
    <article className="mx-auto w-full max-w-3xl space-y-4 px-4 pb-16 pt-3 sm:px-8 sm:pt-6">
      <div className="flex items-center gap-2 text-xs text-subtle">
        <Link to="/pages" className="inline-flex items-center gap-1 hover:text-fg md:hidden">
          <ChevronLeft className="size-3.5" /> Pages
        </Link>
        <nav aria-label="Breadcrumbs" className="hidden min-w-0 items-center gap-1 md:flex">
          {p.project_key ? (
            <Link to="/p/$key/pages" params={{ key: p.project_key }} className="hover:text-fg">
              {p.project_key}
            </Link>
          ) : (
            <span>Personal</span>
          )}
          {(p.ancestors ?? []).map((a) => (
            <span key={a.id} className="flex min-w-0 items-center gap-1">
              <ChevronRight className="size-3" />
              <Link
                to="/pages/$id"
                params={{ id: String(a.id) }}
                className="truncate hover:text-fg"
              >
                {a.title}
              </Link>
            </span>
          ))}
        </nav>
        <span className="ml-auto shrink-0">Updated {relativeTime(p.updated_at)}</span>
        {editable && (
          <>
            <IconButton
              ref={menu.anchor}
              label="Page actions"
              onClick={menu.toggle}
              icon={<MoreHorizontal className="size-4" />}
            />
            <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
              <MenuList
                onClose={menu.close}
                items={[
                  {
                    label: 'New sub-page',
                    icon: <FilePlus />,
                    onSelect: () => create.mutate({ title: 'Untitled', parent: p.id }),
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
      <div>
        <TitleInput
          value={p.title}
          editable={editable}
          onSave={(title) => save.mutate({ title })}
        />
        <WrittenBy
          agent={p.text_agent}
          unreviewed={p.unreviewed}
          reviewedBy={p.reviewed_by}
          review={editable ? { type: 'page', ref: String(p.id) } : undefined}
          className="mt-1"
        />
        {p.draft && (
          <DraftBanner
            what="page"
            className="mt-3"
            publishing={publish.isPending}
            onPublish={
              editable
                ? () =>
                    publish.mutate(p.id, {
                      onSuccess: () => toast('Published: agents can see it now'),
                      onError: toastError,
                    })
                : undefined
            }
          />
        )}
      </div>
      <div className="flex flex-wrap items-center gap-1">
        {p.code && (
          <span
            className="px-1"
            title={
              `Code ${p.code}: ${p.project_key ? `${p.project_key}-${p.code} from other projects` : 'among your personal pages'}` +
              (p.other_codes?.length ? `. Also answers to ${p.other_codes.join(', ')}` : '')
            }
          >
            <CodeChip code={p.code} />
          </span>
        )}
        <PageCategoryPicker
          value={p.category ?? ''}
          disabled={!editable}
          onChange={(c) => save.mutate({ category: c })}
        />
        <MovePicker page={p} disabled={!editable} onMove={(body) => save.mutate(body)} />
      </div>
      <PlanStory page={p} editable={editable} onSave={(story) => save.mutateAsync({ story })} />
      <PlanHeading page={p} />
      <MarkdownEditor
        value={p.body ?? ''}
        editable={editable}
        placeholder="Write the page… (markdown)"
        draftKey={editable ? `page:${p.id}` : undefined}
        autosave
        minRows={8}
        project={p.project_key}
        attachTo={editable ? `page:${p.id}` : undefined}
        onSave={async (body) => {
          await save.mutateAsync({ body })
        }}
      />
      <Attachments target={`page:${p.id}`} editable={editable} />
      {(p.children?.length ?? 0) > 0 && (
        <section>
          <h3 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
            Sub-pages
          </h3>
          <ul className="overflow-hidden rounded-lg border border-line">
            {p.children!.map((c) => (
              <li key={c.id} className="border-b border-line last:border-b-0">
                <Link
                  to="/pages/$id"
                  params={{ id: String(c.id) }}
                  className="flex items-center gap-2 px-3 py-2 text-[13px] hover:bg-hover"
                >
                  <FileText className="size-3.5 text-subtle" />
                  <span className="flex-1 truncate">{c.title}</span>
                  {c.code ? (
                    <span className="font-mono text-xs text-subtle" title={c.category}>
                      {c.code}
                    </span>
                  ) : (
                    c.category && <span className="text-xs text-subtle">{c.category}</span>
                  )}
                </Link>
              </li>
            ))}
          </ul>
        </section>
      )}
      <Links at={{ type: 'page', id: p.id }} editable={editable} />
      <Usage type="page" id={p.id} name={p.title} />
      <Confirm
        open={deleting}
        onClose={() => setDeleting(false)}
        title={`Delete “${p.title}”?`}
        body={
          p.child_count > 0
            ? `Its ${p.child_count} sub-page${p.child_count === 1 ? '' : 's'} will be deleted too. This can't be undone.`
            : "This can't be undone."
        }
        onConfirm={() => del.mutate()}
      />
    </article>
  )
}

function TitleInput({
  value,
  editable,
  onSave,
}: {
  value: string
  editable: boolean
  onSave: (v: string) => void
}) {
  const [draft, setDraft] = useState(value)
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => setDraft(value), [value])
  const commit = () => {
    const t = draft.trim()
    if (t && t !== value) onSave(t)
    else setDraft(value)
  }
  return (
    <input
      ref={ref}
      value={draft}
      readOnly={!editable}
      onChange={(e) => setDraft(e.target.value)}
      onBlur={commit}
      onKeyDown={(e) => {
        if (e.key === 'Enter') ref.current?.blur()
        if (e.key === 'Escape') {
          setDraft(value)
          requestAnimationFrame(() => ref.current?.blur())
        }
      }}
      aria-label="Page title"
      className="w-full bg-transparent text-2xl font-semibold tracking-tight outline-none"
    />
  )
}

function PageCategoryPicker({
  value,
  onChange,
  disabled,
}: {
  value: string
  onChange: (c: string) => void
  disabled?: boolean
}) {
  const pop = usePopover()
  const items: PickItem<string>[] = [
    { value: '', label: 'No category' },
    ...PAGE_CATEGORIES.map((c) => ({ value: c, label: catLabel(c) })),
  ]
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Category">
        <span className={clsx('h-2 w-2 rounded-full', value ? 'bg-accent' : 'bg-line-strong')} />
        <span className={clsx(!value && 'text-subtle')}>
          {value ? catLabel(value) : 'Category'}
        </span>
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={items}
          selected={[value]}
          onClose={pop.close}
          placeholder="Category…"
          onPick={(v) => v !== value && onChange(v)}
        />
      </Popover>
    </>
  )
}

/** Move a page: under another page, to the top of a project, or personal. */
function MovePicker({
  page,
  disabled,
  onMove,
}: {
  page: Page
  disabled?: boolean
  onMove: (b: Record<string, unknown>) => void
}) {
  const pop = usePopover()
  usePrefetch(pagesQuery({}), !disabled)
  const pages = useQuery({ ...pagesQuery({}), enabled: pop.open }).data ?? []
  const projects = (useQuery({ ...q.projects(), enabled: pop.open }).data ?? []).filter(
    (p) => p.role === 'owner' || p.role === 'editor',
  )
  const below = useMemo(() => {
    const tree = pageTree(pages)
    const out = new Set<number>([page.id])
    const walk = (id: number) => (tree.get(id) ?? []).forEach((c) => (out.add(c.id), walk(c.id)))
    walk(page.id)
    return out
  }, [pages, page.id])
  const items: PickItem<string>[] = [
    {
      value: 'personal',
      label: 'Personal (top level)',
      icon: <FolderTree className="size-3.5 text-subtle" />,
    },
    ...projects.map((p) => ({
      value: 'project:' + p.key,
      label: `${p.name} (top level)`,
      icon: <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={14} />,
    })),
    ...pages
      .filter((x) => !below.has(x.id))
      .map((x) => ({
        value: 'page:' + x.id,
        label: x.title,
        hint: x.project_key ?? 'personal',
        icon: <FileText className="size-3.5 text-subtle" />,
      })),
  ]
  const where = page.ancestors?.length
    ? `in ${page.ancestors[page.ancestors.length - 1].title}`
    : page.project_key
      ? `in ${page.project_key}`
      : 'personal'
  return (
    <>
      <PropButton anchor={pop.anchor} onClick={pop.toggle} disabled={disabled} title="Move">
        <FolderTree className="size-3.5 text-subtle" />
        <span className="truncate text-muted">{where}</span>
      </PropButton>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={items}
          selected={[]}
          placeholder="Move to…"
          onClose={pop.close}
          onPick={(v) => {
            if (v === 'personal') onMove({ project: null })
            else if (v.startsWith('project:')) onMove({ project: v.slice(8) })
            else onMove({ parent: Number(v.slice(5)) })
          }}
        />
      </Popover>
    </>
  )
}
