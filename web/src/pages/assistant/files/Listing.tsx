// One folder of the assistant's files: where it is (breadcrumbs from the
// root), what's in it (folders first, sortable, filterable, dotfiles on
// request), and what can be added to it (uploads, by button or by dropping
// files on the list, and new folders).

import { useEffect, useMemo, useRef, useState, type DragEvent, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQueryClient, type UseQueryResult } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowDown,
  ArrowUp,
  ArrowUpDown,
  Check,
  ChevronRight,
  Eye,
  File as FileIcon,
  FileArchive,
  FileAudio,
  FileBraces,
  FileCode,
  FileImage,
  FileSpreadsheet,
  FileText,
  FileVideo,
  Folder,
  FolderPlus,
  FolderX,
  HardDrive,
  House,
  Link2Off,
  MoreHorizontal,
  Pencil,
  RefreshCw,
  Search,
  Database,
  Upload as UploadIcon,
  X,
} from 'lucide-react'
import { ApiError } from '@/lib/api'
import { hermes, iso, type Assistant } from '@/lib/assistant'
import { fileSize } from '@/lib/attachments'
import { dateTime, plural, relativeTime } from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { Button, IconButton, Spinner, ToggleButton } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { Empty, Progress, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { toast, toastError } from '@/components/ui/toast'
import { useFileActions } from './actions'
import {
  badName,
  baseName,
  joinPath,
  kindOf,
  listingsKey,
  sortEntries,
  trimSlash,
  type FileEntry,
  type Kind,
  type Listing,
  type Sort,
  type SortKey,
} from './model'
import { cancel, dismiss, enqueue, resolve, useUploads, type Upload } from './uploads'

const KIND_ICON: Record<Kind, ReactNode> = {
  folder: <Folder />,
  image: <FileImage />,
  audio: <FileAudio />,
  video: <FileVideo />,
  pdf: <FileText />,
  markdown: <FileText />,
  json: <FileBraces />,
  code: <FileCode />,
  text: <FileText />,
  sheet: <FileSpreadsheet />,
  archive: <FileArchive />,
  data: <Database />,
  other: <FileIcon />,
}

export function KindIcon({ entry, className }: { entry: FileEntry; className?: string }) {
  const k = kindOf(entry)
  return (
    <span
      aria-hidden
      className={clsx(
        'flex shrink-0 items-center justify-center [&>svg]:size-full',
        k === 'folder' ? 'text-accent' : 'text-subtle',
        className ?? 'size-4',
      )}
    >
      {entry.broken_link ? <Link2Off /> : KIND_ICON[k]}
    </span>
  )
}

const SORTS: { key: SortKey; label: string }[] = [
  { key: 'name', label: 'Name' },
  { key: 'modified', label: 'Modified' },
  { key: 'size', label: 'Size' },
]

export function FolderPane({
  assistant,
  listing,
  selected,
  onGo,
  onDeleted,
}: {
  assistant: Assistant
  listing: UseQueryResult<Listing>
  /** the file open beside the list */
  selected?: string
  /** to a folder; undefined is the root */
  onGo: (path?: string) => void
  onDeleted: (e: FileEntry) => void
}) {
  const id = assistant.id
  const qc = useQueryClient()
  const data = listing.data
  const dir = data?.path
  const [filter, setFilter] = useState('')
  const [hidden, setHidden] = useLocal('assistant-files-hidden', false)
  const [sort, setSort] = useLocal<Sort>('assistant-files-sort', { key: 'name', desc: false })
  const [newFolder, setNewFolder] = useState(false)
  const [dragging, setDragging] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLUListElement>(null)
  const actions = useFileActions(id, onDeleted)
  const uploads = useUploads(id)

  // A new folder starts unfiltered, at the top.
  useEffect(() => {
    setFilter('')
    listRef.current?.parentElement?.scrollTo({ top: 0 })
  }, [dir])

  const rows = useMemo(() => {
    let list = data?.entries ?? []
    if (!hidden) list = list.filter((e) => !e.name.startsWith('.'))
    const f = filter.trim().toLowerCase()
    if (f) list = list.filter((e) => e.name.toLowerCase().includes(f))
    return sortEntries(list, sort)
  }, [data, hidden, filter, sort])
  const hiddenCount = useMemo(
    () => (data?.entries ?? []).filter((e) => e.name.startsWith('.')).length,
    [data],
  )

  const refresh = (d: string) =>
    qc.invalidateQueries({ queryKey: listingsKey(id), predicate: (q) => listed(q.queryKey, d) })
  const upload = (files: File[]) => {
    if (!dir || !files.length) return
    enqueue(id, dir, files, refresh)
  }
  const onDrop = (e: DragEvent) => {
    e.preventDefault()
    setDragging(false)
    const items = Array.from(e.dataTransfer.items ?? [])
    const folders = items.filter((i) => i.webkitGetAsEntry?.()?.isDirectory).length
    if (folders) toast(`Folders can't be uploaded; drop the files in them instead.`)
    const files = Array.from(e.dataTransfer.files).filter(
      (_, i) => !items[i]?.webkitGetAsEntry?.()?.isDirectory,
    )
    upload(files)
  }
  const canDrop = !!dir && !listing.isError
  const isFiles = (e: DragEvent) => e.dataTransfer.types.includes('Files')

  // Arrow keys walk the rows; Backspace goes up; Delete asks to delete.
  const onKeyDown = (e: React.KeyboardEvent) => {
    const t = e.target as HTMLElement
    if (t.closest('input, textarea')) return
    const list = Array.from(listRef.current?.querySelectorAll<HTMLElement>('[data-row]') ?? [])
    const at = list.indexOf(t)
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      const to = at < 0 ? 0 : at + (e.key === 'ArrowDown' ? 1 : -1)
      list[Math.max(0, Math.min(list.length - 1, to))]?.focus()
    } else if (e.key === 'Backspace' && data?.parent && !e.metaKey) {
      e.preventDefault()
      onGo(data.parent === data.root ? undefined : data.parent)
    } else if (e.key === 'Delete' && at >= 0) {
      const entry = rows.find((r) => r.path === list[at].dataset.path)
      if (entry) {
        e.preventDefault()
        actions.askDelete(entry)
      }
    }
  }

  return (
    <div
      className="relative flex min-h-0 flex-1 flex-col"
      onDragEnter={(e) => canDrop && isFiles(e) && setDragging(true)}
      onDragOver={(e) => {
        if (!canDrop || !isFiles(e)) return
        e.preventDefault()
        e.dataTransfer.dropEffect = 'copy'
        if (!dragging) setDragging(true)
      }}
      onDragLeave={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node)) setDragging(false)
      }}
      onDrop={(e) => canDrop && onDrop(e)}
      onKeyDown={onKeyDown}
    >
      <div className="flex h-11 shrink-0 items-center gap-1 border-b border-line pl-2 pr-2 sm:pl-3">
        <Breadcrumbs listing={data} failed={listing.isError} onGo={onGo} />
        <IconButton
          label="New folder"
          size="md"
          className="sm:size-7"
          disabled={!dir}
          onClick={() => setNewFolder(true)}
          icon={<FolderPlus className="size-4" />}
        />
        <Button
          size="sm"
          variant="primary"
          className="h-9 sm:h-7"
          disabled={!dir}
          onClick={() => fileInput.current?.click()}
          icon={<UploadIcon className="size-3.5" />}
          aria-label="Upload files"
        >
          <span className="hidden sm:inline">Upload</span>
        </Button>
        <input
          ref={fileInput}
          type="file"
          multiple
          hidden
          onChange={(e) => {
            upload(Array.from(e.target.files ?? []))
            e.target.value = ''
          }}
        />
      </div>
      <div className="flex shrink-0 items-center gap-1 border-b border-line px-2 py-1.5 sm:px-3">
        <div className="relative min-w-0 flex-1">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <Input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape' && filter) {
                e.stopPropagation()
                setFilter('')
              } else if (e.key === 'ArrowDown') {
                e.preventDefault()
                listRef.current?.querySelector<HTMLElement>('[data-row]')?.focus()
              }
            }}
            placeholder="Filter this folder"
            aria-label="Filter this folder"
            className="h-9 w-full pl-8 sm:h-8"
          />
        </div>
        <SortMenu sort={sort} onSort={setSort} />
        <ToggleButton
          label="Hidden files"
          hint={hiddenCount ? `${plural(hiddenCount, 'hidden item')} here` : undefined}
          pressed={hidden}
          onPressedChange={setHidden}
          icon={<Eye className="size-4" />}
          className="size-9 sm:size-7"
        />
        <IconButton
          label="Refresh"
          size="md"
          className="sm:size-7"
          onClick={() => listing.refetch()}
          icon={<RefreshCw className={clsx('size-4', listing.isFetching && 'animate-spin')} />}
        />
      </div>

      <div className="@container min-h-0 flex-1 overflow-y-auto">
        {listing.isPending ? (
          <ListSkeleton />
        ) : listing.isError ? (
          <ListError error={listing.error} onTop={() => onGo(undefined)} />
        ) : (
          <>
            <ColumnHeads sort={sort} onSort={setSort} />
            <ul ref={listRef} aria-label={`Files in ${baseName(dir ?? '/')}`}>
              {rows.map((e) => (
                <Row
                  key={e.path}
                  assistant={assistant}
                  entry={e}
                  rootPath={data?.root ?? null}
                  selected={e.path === selected}
                  menu={actions.items(e, () => onGo(e.path))}
                />
              ))}
            </ul>
            {!rows.length && (
              <EmptyFolder
                filter={filter.trim()}
                hiddenOnly={!hidden && hiddenCount > 0 && !filter.trim()}
                onClearFilter={() => setFilter('')}
                onShowHidden={() => setHidden(true)}
                onUpload={() => fileInput.current?.click()}
              />
            )}
            {rows.length > 0 && (
              <div className="px-3 py-3 text-center text-xs text-subtle @lg:text-left">
                {summary(rows)}
              </div>
            )}
          </>
        )}
      </div>

      <Uploads uploads={uploads} onGo={onGo} here={dir} />

      {dragging && (
        <div className="pointer-events-none absolute inset-1 z-10 flex items-center justify-center rounded-xl border-2 border-dashed border-accent bg-accent-soft backdrop-blur-[1px]">
          <div className="flex items-center gap-2 rounded-lg bg-panel px-3 py-2 text-sm font-medium shadow-pop">
            <UploadIcon className="size-4 text-accent" />
            <span className="flex min-w-0 gap-1">
              Drop to upload to <span className="max-w-48 truncate">{baseName(dir ?? '/')}</span>
            </span>
          </div>
        </div>
      )}
      {actions.dialog}
      {dir && (
        <NewFolderDialog
          open={newFolder}
          onClose={() => setNewFolder(false)}
          id={id}
          dir={dir}
          taken={data?.entries.map((e) => e.name) ?? []}
          onMade={() => refresh(dir)}
        />
      )}
    </div>
  )
}

/** Whether a listing query's key is for folder `dir`. */
function listed(key: readonly unknown[], dir: string): boolean {
  const p = (key[3] as { path?: string } | undefined)?.path
  return p === undefined || trimSlash(p) === trimSlash(dir)
}

function summary(rows: FileEntry[]): string {
  const folders = rows.filter((r) => r.is_directory).length
  const files = rows.length - folders
  const bytes = rows.reduce((n, r) => n + (r.is_directory ? 0 : (r.size ?? 0)), 0)
  const parts = []
  if (folders) parts.push(plural(folders, 'folder'))
  if (files) parts.push(`${plural(files, 'file')}, ${fileSize(bytes)}`)
  return parts.join(' · ')
}

// ---- where we are ----

function Breadcrumbs({
  listing,
  failed,
  onGo,
}: {
  listing?: Listing
  failed: boolean
  onGo: (p?: string) => void
}) {
  const ref = useRef<HTMLOListElement>(null)
  const [editing, setEditing] = useState(false)
  const path = listing?.path
  const root = listing?.locked_root ?? '/'
  const crumbs = useMemo(() => {
    if (!path) return []
    const r = trimSlash(root)
    const rest = path.startsWith(r) ? path.slice(r.length) : path
    const out = [{ name: listing?.locked_root ? baseName(r) : '/', path: r }]
    let at = r === '/' ? '' : r
    for (const seg of rest.split('/').filter(Boolean)) {
      at += '/' + seg
      out.push({ name: seg, path: at })
    }
    return out
  }, [path, root, listing?.locked_root])
  // Long paths keep the folder you're in visible.
  useEffect(() => {
    const el = ref.current
    if (el) el.scrollLeft = el.scrollWidth
  }, [crumbs])
  if (editing && listing?.can_change_path)
    return <GoToPath start={path ?? ''} onGo={onGo} onDone={() => setEditing(false)} />
  if (!listing)
    return failed ? (
      <div className="flex-1">
        <IconButton
          label="Back to the top"
          size="md"
          className="sm:size-7"
          onClick={() => onGo(undefined)}
          icon={<House className="size-3.5" />}
        />
      </div>
    ) : (
      <div className="flex-1">
        <Skeleton className="h-4 w-40" />
      </div>
    )
  const top = (p: string) => (p === listing.root ? undefined : p)
  return (
    <nav aria-label="Folder" className="flex min-w-0 flex-1 items-center">
      <ol ref={ref} className="no-scrollbar flex min-w-0 items-center overflow-x-auto">
        {crumbs.map((c, i) => {
          const last = i === crumbs.length - 1
          return (
            <li key={c.path} className="flex shrink-0 items-center">
              {i > 0 && <ChevronRight aria-hidden className="size-3.5 shrink-0 text-subtle" />}
              <button
                type="button"
                aria-current={last ? 'location' : undefined}
                title={c.path}
                onClick={() => !last && onGo(top(c.path))}
                className={clsx(
                  'flex h-9 max-w-48 items-center gap-1.5 rounded-md px-1.5 text-[13px] sm:h-7',
                  last ? 'font-medium text-fg' : 'text-muted hover:bg-hover hover:text-fg',
                )}
              >
                {i === 0 &&
                  (listing.locked_root ? (
                    <House className="size-3.5 shrink-0" />
                  ) : (
                    <HardDrive className="size-3.5 shrink-0" />
                  ))}
                {(i > 0 || listing.locked_root) && <span className="truncate">{c.name}</span>}
                {i === 0 && !listing.locked_root && <span className="sr-only">Top</span>}
              </button>
            </li>
          )
        })}
      </ol>
      {listing.can_change_path && (
        <IconButton
          label="Go to a folder by path"
          size="xs"
          className="ml-0.5 shrink-0"
          onClick={() => setEditing(true)}
          icon={<Pencil className="size-3" />}
        />
      )}
    </nav>
  )
}

function GoToPath({
  start,
  onGo,
  onDone,
}: {
  start: string
  onGo: (p?: string) => void
  onDone: () => void
}) {
  const [v, setV] = useState(start)
  return (
    <form
      className="flex min-w-0 flex-1 items-center gap-1"
      onSubmit={(e) => {
        e.preventDefault()
        const p = v.trim()
        if (p) onGo(trimSlash(p))
        onDone()
      }}
    >
      <Input
        autoFocus
        value={v}
        onChange={(e) => setV(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            e.stopPropagation()
            onDone()
          }
        }}
        onBlur={onDone}
        aria-label="Folder path"
        className="h-8 flex-1 font-mono text-xs"
      />
    </form>
  )
}

// ---- the rows ----

function ColumnHeads({ sort, onSort }: { sort: Sort; onSort: (s: Sort) => void }) {
  const head = (key: SortKey, label: string, className: string) => {
    const on = sort.key === key
    return (
      <button
        type="button"
        onClick={() => onSort({ key, desc: on ? !sort.desc : key !== 'name' })}
        aria-label={`Sort by ${label.toLowerCase()}`}
        className={clsx(
          'flex h-7 items-center gap-1 text-[11px] font-medium uppercase tracking-wide hover:text-fg',
          on ? 'text-muted' : 'text-subtle',
          className,
        )}
      >
        {label}
        {on && (sort.desc ? <ArrowDown className="size-3" /> : <ArrowUp className="size-3" />)}
      </button>
    )
  }
  return (
    <div className="sticky top-0 z-[1] hidden items-center gap-3 border-b border-line bg-bg pl-10 pr-12 @lg:flex">
      {head('name', 'Name', 'flex-1')}
      {head('size', 'Size', 'w-20 justify-end')}
      {head('modified', 'Modified', 'w-28')}
    </div>
  )
}

function Row({
  assistant,
  entry,
  rootPath,
  selected,
  menu,
}: {
  assistant: Assistant
  entry: FileEntry
  rootPath: string | null
  selected: boolean
  menu: ReturnType<ReturnType<typeof useFileActions>['items']>
}) {
  const pop = usePopover()
  const when = iso(entry.mtime)
  const size = entry.is_directory || entry.size == null ? '' : fileSize(entry.size)
  return (
    <li className="group relative">
      <Link
        to="/assistant/$id"
        params={{ id: String(assistant.id) }}
        search={{ tab: 'files', path: entry.path === rootPath ? undefined : entry.path }}
        data-row
        data-path={entry.path}
        aria-current={selected ? 'page' : undefined}
        title={entry.broken_link ? `${entry.name} (broken link)` : entry.name}
        className={clsx(
          'flex min-h-12 items-center gap-3 py-1.5 pl-3 pr-12 outline-none focus-visible:bg-hover focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-accent/40 @lg:min-h-9 @lg:py-0',
          selected ? 'bg-accent-soft' : 'hover:bg-hover',
        )}
      >
        <KindIcon entry={entry} className={clsx('size-4', entry.broken_link && '!text-warn')} />
        <span className="min-w-0 flex-1">
          <span
            className={clsx(
              'block truncate text-[13px]',
              selected ? 'font-medium text-fg' : 'text-fg',
              entry.broken_link && 'text-muted line-through',
            )}
          >
            {entry.name}
          </span>
          <span className="block truncate text-xs text-subtle @lg:hidden">
            {[size, when && relativeTime(when)].filter(Boolean).join(' · ')}
          </span>
        </span>
        <span className="hidden w-20 shrink-0 text-right text-xs tabular-nums text-muted @lg:block">
          {size}
        </span>
        <span
          className="hidden w-28 shrink-0 truncate text-xs text-muted @lg:block"
          title={when ? dateTime(when) : undefined}
        >
          {when ? relativeTime(when) : ''}
        </span>
      </Link>
      <div className="absolute inset-y-0 right-1.5 flex items-center">
        <IconButton
          ref={pop.anchor}
          label={`More for ${entry.name}`}
          size="md"
          className={clsx(
            'sm:size-7',
            !pop.open &&
              'sm:opacity-0 sm:focus-visible:opacity-100 sm:group-focus-within:opacity-100 sm:group-hover:opacity-100',
          )}
          onClick={pop.toggle}
          icon={<MoreHorizontal className="size-4" />}
        />
      </div>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <MenuList items={menu} onClose={pop.close} />
      </Popover>
    </li>
  )
}

function SortMenu({ sort, onSort }: { sort: Sort; onSort: (s: Sort) => void }) {
  const pop = usePopover()
  const label = SORTS.find((s) => s.key === sort.key)!.label
  return (
    <>
      <IconButton
        ref={pop.anchor}
        label={`Sort: ${label}${sort.desc ? ', descending' : ''}`}
        size="md"
        className="sm:size-7"
        onClick={pop.toggle}
        icon={<ArrowUpDown className="size-4" />}
      />
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
        <MenuList
          onClose={pop.close}
          items={[
            ...SORTS.map((s) => ({
              label: s.label,
              checked: sort.key === s.key,
              onSelect: () => onSort({ key: s.key, desc: sort.desc }),
            })),
            { separator: true, label: '' },
            {
              label:
                sort.key === 'name'
                  ? 'A to Z'
                  : sort.key === 'size'
                    ? 'Smallest first'
                    : 'Oldest first',
              checked: !sort.desc,
              onSelect: () => onSort({ ...sort, desc: false }),
            },
            {
              label:
                sort.key === 'name'
                  ? 'Z to A'
                  : sort.key === 'size'
                    ? 'Largest first'
                    : 'Newest first',
              checked: sort.desc,
              onSelect: () => onSort({ ...sort, desc: true }),
            },
          ]}
        />
      </Popover>
    </>
  )
}

function ListSkeleton() {
  return (
    <div className="space-y-1 p-3" aria-busy>
      {[60, 45, 70, 38, 52, 64, 41].map((w, i) => (
        <div key={i} className="flex h-9 items-center gap-3">
          <Skeleton className="size-4" />
          <div style={{ width: `${w}%` }}>
            <Skeleton className="h-3.5" />
          </div>
        </div>
      ))}
    </div>
  )
}

function ListError({ error, onTop }: { error: Error; onTop: () => void }) {
  const status = error instanceof ApiError ? error.status : 0
  return (
    <Empty
      icon={<FolderX />}
      title={
        status === 404
          ? "There's no folder here"
          : status === 403
            ? "This folder can't be opened"
            : "Couldn't list this folder"
      }
      action={
        <Button size="sm" onClick={onTop} icon={<House className="size-3.5" />}>
          Back to the top
        </Button>
      }
    >
      {error.message}
    </Empty>
  )
}

function EmptyFolder({
  filter,
  hiddenOnly,
  onClearFilter,
  onShowHidden,
  onUpload,
}: {
  filter: string
  hiddenOnly: boolean
  onClearFilter: () => void
  onShowHidden: () => void
  onUpload: () => void
}) {
  if (filter)
    return (
      <Empty
        icon={<Search />}
        title={`Nothing here matches “${filter}”`}
        action={
          <Button size="sm" onClick={onClearFilter}>
            Clear filter
          </Button>
        }
      />
    )
  if (hiddenOnly)
    return (
      <Empty
        icon={<Eye />}
        title="Only hidden files here"
        action={
          <Button size="sm" onClick={onShowHidden}>
            Show hidden files
          </Button>
        }
      />
    )
  return (
    <Empty
      icon={<Folder />}
      title="This folder is empty"
      action={
        <Button size="sm" onClick={onUpload} icon={<UploadIcon className="size-3.5" />}>
          Upload files
        </Button>
      }
    >
      <span className="hidden sm:inline">Or drop files here.</span>
    </Empty>
  )
}

// ---- adding ----

function NewFolderDialog({
  open,
  onClose,
  id,
  dir,
  taken,
  onMade,
}: {
  open: boolean
  onClose: () => void
  id: number
  dir: string
  taken: string[]
  onMade: () => void
}) {
  const [name, setName] = useState('')
  const [busy, setBusy] = useState(false)
  const [touched, setTouched] = useState(false)
  useEffect(() => {
    if (open) {
      setName('')
      setTouched(false)
    }
  }, [open])
  const n = name.trim()
  const error = badName(n) ?? (taken.includes(n) ? 'Something here already has that name.' : null)
  const submit = async () => {
    setTouched(true)
    if (error) return
    setBusy(true)
    try {
      await hermes(id, 'POST', 'files/mkdir', { path: joinPath(dir, n) })
      toast(`Made ${n}`)
      onMade()
      onClose()
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="New folder"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={busy} onClick={submit}>
            Make folder
          </Button>
        </>
      }
    >
      <form
        className="p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <Field
          label="Name"
          error={touched && n ? (error ?? undefined) : undefined}
          note={`In ${dir}`}
        >
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            spellCheck={false}
            autoComplete="off"
          />
        </Field>
      </form>
    </Dialog>
  )
}

/** Uploads under way, newest last, and any name clash waiting for an answer. */
function Uploads({
  uploads,
  here,
  onGo,
}: {
  uploads: Upload[]
  here?: string
  onGo: (p?: string) => void
}) {
  const clash = uploads.find((u) => u.status === 'conflict')
  if (!uploads.length) return null
  return (
    <div className="max-h-48 shrink-0 overflow-y-auto border-t border-line bg-panel">
      <ul aria-label="Uploads" className="divide-y divide-line">
        {uploads.map((u) => (
          <li key={u.key} className="flex items-center gap-3 px-3 py-2">
            <span className="flex size-4 shrink-0 items-center justify-center">
              {u.status === 'done' ? (
                <Check className="size-4 text-ok" />
              ) : u.status === 'error' ? (
                <X className="size-4 text-danger" />
              ) : u.status === 'uploading' ? (
                <Spinner className="size-3.5 text-muted" />
              ) : (
                <UploadIcon className="size-3.5 text-subtle" />
              )}
            </span>
            <div className="min-w-0 flex-1">
              <div className="flex items-baseline gap-2 text-[13px]">
                <span className="min-w-0 truncate">{u.name}</span>
                {u.dir !== here && (
                  <button
                    type="button"
                    onClick={() => onGo(u.dir)}
                    className="shrink-0 truncate text-xs text-subtle hover:text-fg hover:underline"
                  >
                    in {baseName(u.dir)}
                  </button>
                )}
              </div>
              {u.status === 'error' ? (
                <div className="truncate text-xs text-danger">{u.error}</div>
              ) : u.status === 'conflict' ? (
                <div className="text-xs text-warn">Already there</div>
              ) : (
                <Progress
                  value={u.loaded}
                  max={u.file.size || 1}
                  className="mt-1"
                  color={u.status === 'done' ? 'var(--wp-ok)' : 'var(--wp-accent)'}
                />
              )}
            </div>
            <span className="w-12 shrink-0 text-right text-xs tabular-nums text-subtle">
              {u.status === 'uploading' && u.file.size
                ? `${Math.round((u.loaded / u.file.size) * 100)}%`
                : u.status === 'queued'
                  ? 'Waiting'
                  : ''}
            </span>
            {u.status !== 'done' && (
              <IconButton
                label={u.status === 'error' ? 'Dismiss' : 'Cancel upload'}
                size="md"
                className="sm:size-7"
                onClick={() => (u.status === 'error' ? dismiss(u.key) : cancel(u.key))}
                icon={<X className="size-4" />}
              />
            )}
          </li>
        ))}
      </ul>
      <ClashDialog
        upload={clash}
        more={uploads.filter((u) => u.status === 'conflict').length - 1}
      />
    </div>
  )
}

function ClashDialog({ upload, more }: { upload?: Upload; more: number }) {
  return (
    <Dialog
      open={!!upload}
      onClose={() => upload && resolve(upload.key, 'skip')}
      title="Replace the file?"
      footer={
        upload && (
          <>
            <Button variant="ghost" className="mr-auto" onClick={() => resolve(upload.key, 'skip')}>
              Skip
            </Button>
            <Button onClick={() => resolve(upload.key, 'keep')}>Keep both</Button>
            <Button variant="danger" onClick={() => resolve(upload.key, 'replace')}>
              Replace
            </Button>
          </>
        )
      }
    >
      {upload && (
        <div className="space-y-2 px-4 py-4 text-sm text-muted">
          <p>
            <span className="break-all font-medium text-fg">{upload.name}</span> is already in{' '}
            <span className="break-all">{baseName(upload.dir)}</span>. Replacing it overwrites the
            assistant's copy.
          </p>
          {more > 0 && (
            <p className="text-xs text-subtle">
              {plural(more, 'more clash', 'more clashes')} after this.
            </p>
          )}
        </div>
      )}
    </Dialog>
  )
}
