// One of the assistant's files, shown for what it is: images as pictures,
// audio and video in a player, Markdown rendered (untrusted: no HTML, no
// remote images) or raw, JSON pretty, and any other text with line numbers.
// Text can be edited in place. HTML is only ever text here.

import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useBlocker } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowLeft,
  Braces,
  Code,
  Download,
  Eye,
  FileWarning,
  MoreHorizontal,
  Pencil,
  WrapText,
  X,
} from 'lucide-react'
import { hermes, iso } from '@/lib/assistant'
import { fileSize } from '@/lib/attachments'
import { dateTime, relativeTime } from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Empty, Kbd, Skeleton } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { Markdown } from '../Transcript'
import { useFileActions } from './actions'
import { KindIcon } from './Listing'
import {
  baseName,
  downloadURL,
  imageQuery,
  kindOf,
  listingsKey,
  MAX_IMAGE_PREVIEW,
  MAX_WRITE,
  parentOf,
  readsAsText,
  streamURL,
  textQuery,
  type FileEntry,
  type TextFile,
} from './model'

/** What Hermes reads of a text file (R:files.py:110). */
const PREVIEW_LIMIT = 512 * 1024

const mac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform)

export function FilePreview({
  id,
  path,
  entry,
  loading,
  wide,
  onClose,
  onDeleted,
}: {
  id: number
  path: string
  /** from the folder's listing; undefined while it loads or once it's gone */
  entry?: FileEntry
  loading: boolean
  wide: boolean
  onClose: () => void
  onDeleted: (e: FileEntry) => void
}) {
  const qc = useQueryClient()
  const kind = entry ? kindOf(entry) : 'other'
  const asText = !!entry && !entry.broken_link && readsAsText(kind)
  const text = useQuery({ ...textQuery(id, path, entry?.mtime ?? null), enabled: asText })
  const [draft, setDraft] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [discard, setDiscard] = useState(false)
  const actions = useFileActions(id, onDeleted)
  const menu = usePopover()
  const editing = draft !== null
  const dirty = editing && draft !== text.data?.text
  const t = text.data
  const editable =
    asText && !!t && !t.binary && !t.truncated && t.byteSize <= MAX_WRITE && !entry?.broken_link

  const blocker = useBlocker({
    shouldBlockFn: () => dirty,
    enableBeforeUnload: () => dirty,
    disabled: !dirty,
    withResolver: true,
  })

  const save = async (andClose: boolean) => {
    if (draft === null || saving) return
    if (!dirty) {
      if (andClose) setDraft(null)
      return
    }
    setSaving(true)
    try {
      const r = await hermes<{ byteSize: number }>(id, 'POST', 'fs/write-text', {
        path,
        content: draft,
      })
      qc.setQueryData<TextFile>(textQuery(id, path, entry?.mtime ?? null).queryKey, (old) =>
        old ? { ...old, text: draft, byteSize: r.byteSize } : old,
      )
      qc.invalidateQueries({ queryKey: listingsKey(id) })
      toast('Saved')
      if (andClose) setDraft(null)
    } catch (e) {
      toastError(e)
    } finally {
      setSaving(false)
    }
  }

  // Cmd/Ctrl+S saves while editing, wherever the focus is.
  useEffect(() => {
    if (!editing) return
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 's') {
        e.preventDefault()
        save(false)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })

  const name = entry?.name ?? baseName(path)
  const when = iso(entry?.mtime)
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <header className="flex h-11 shrink-0 items-center gap-1.5 border-b border-line pl-1 pr-2 sm:gap-2 sm:pl-3">
        {!wide && (
          <IconButton
            label={`Back to ${baseName(parentOf(path))}`}
            size="md"
            onClick={onClose}
            icon={<ArrowLeft className="size-4" />}
          />
        )}
        {entry && <KindIcon entry={entry} />}
        <div className="min-w-0 flex-1">
          <h2 className="truncate text-[13px] font-medium" title={path}>
            {name}
            {dirty && (
              <span
                className="ml-1.5 inline-block size-1.5 rounded-full bg-warn align-middle"
                aria-label="Unsaved changes"
              />
            )}
          </h2>
          {entry && !editing && (
            <div className="truncate text-[11px] text-subtle sm:hidden">
              {[entry.size != null && fileSize(entry.size), when && relativeTime(when)]
                .filter(Boolean)
                .join(' · ')}
            </div>
          )}
        </div>
        {entry && !editing && (
          <span
            className="hidden shrink-0 text-xs text-subtle sm:inline"
            title={when ? dateTime(when) : undefined}
          >
            {[entry.size != null && fileSize(entry.size), when && relativeTime(when)]
              .filter(Boolean)
              .join(' · ')}
          </span>
        )}
        {editing ? (
          <>
            <span className="hidden text-xs text-subtle md:inline">
              <Kbd>{mac ? '⌘' : 'Ctrl'}</Kbd> <Kbd>S</Kbd>
            </span>
            <Button
              size="sm"
              variant="ghost"
              className="h-9 sm:h-7"
              onClick={() => (dirty ? setDiscard(true) : setDraft(null))}
            >
              Cancel
            </Button>
            <Button
              size="sm"
              variant="primary"
              className="h-9 sm:h-7"
              loading={saving}
              onClick={() => save(true)}
            >
              Save
            </Button>
          </>
        ) : (
          entry && (
            <>
              {editable && (
                <Button
                  size="sm"
                  className="h-9 sm:h-7"
                  icon={<Pencil className="size-3.5" />}
                  onClick={() => setDraft(t!.text)}
                  aria-label="Edit"
                >
                  <span className="hidden sm:inline">Edit</span>
                </Button>
              )}
              {!entry.broken_link && (
                <a
                  href={downloadURL(id, path)}
                  download
                  aria-label="Download"
                  title="Download"
                  className="inline-flex size-9 items-center justify-center rounded-md text-muted hover:bg-hover hover:text-fg sm:size-7"
                >
                  <Download className="size-4" />
                </a>
              )}
              <IconButton
                ref={menu.anchor}
                label="More"
                size="md"
                className="sm:size-7"
                onClick={menu.toggle}
                icon={<MoreHorizontal className="size-4" />}
              />
              <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
                <MenuList
                  items={actions.items(entry).filter((m) => m.label !== 'Download')}
                  onClose={menu.close}
                />
              </Popover>
            </>
          )
        )}
        {wide && !editing && (
          <IconButton label="Close" size="sm" onClick={onClose} icon={<X className="size-4" />} />
        )}
      </header>

      <div className="flex min-h-0 flex-1 flex-col">
        {!entry ? (
          loading ? (
            <Skeleton className="m-4 h-48" />
          ) : (
            <Empty icon={<FileWarning />} title="This file isn't here anymore">
              {path}
            </Empty>
          )
        ) : entry.broken_link ? (
          <Empty icon={<FileWarning />} title="Broken link">
            It points to something that isn't there.
          </Empty>
        ) : editing ? (
          <textarea
            autoFocus
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            spellCheck={false}
            aria-label={`Contents of ${name}`}
            className="min-h-0 flex-1 resize-none bg-panel px-4 py-3 font-mono text-xs leading-5 text-fg outline-none"
          />
        ) : kind === 'image' ? (
          <ImageView id={id} entry={entry} />
        ) : kind === 'audio' || kind === 'video' ? (
          <MediaView id={id} entry={entry} video={kind === 'video'} />
        ) : asText ? (
          text.isPending ? (
            <div className="space-y-2 p-4">
              {[80, 64, 72, 40, 58].map((w, i) => (
                <div key={i} style={{ width: `${w}%` }}>
                  <Skeleton className="h-3.5" />
                </div>
              ))}
            </div>
          ) : text.isError ? (
            <NoPreview id={id} entry={entry} title="Couldn't read this file">
              {text.error.message}
            </NoPreview>
          ) : t!.binary ? (
            <NoPreview id={id} entry={entry} />
          ) : (
            <TextView id={id} entry={entry} file={t!} kind={kind} />
          )
        ) : (
          <NoPreview id={id} entry={entry} />
        )}
      </div>

      {actions.dialog}
      <Confirm
        open={discard}
        onClose={() => setDiscard(false)}
        onConfirm={() => setDraft(null)}
        title="Discard your changes?"
        body={`Your edits to ${name} aren't saved.`}
        confirmLabel="Discard"
        cancelLabel="Keep editing"
      />
      <Confirm
        open={blocker.status === 'blocked'}
        onClose={() => blocker.reset?.()}
        onConfirm={() => blocker.proceed?.()}
        title="Leave without saving?"
        body={`Your edits to ${name} aren't saved. If you leave, they're lost.`}
        confirmLabel="Leave"
        cancelLabel="Stay"
      />
    </div>
  )
}

// ---- kinds of preview ----

function DownloadButton({ id, path }: { id: number; path: string }) {
  return (
    <a
      href={downloadURL(id, path)}
      download
      className="inline-flex h-9 items-center gap-2 rounded-lg border border-line bg-panel px-3.5 text-sm font-medium shadow-card hover:bg-hover"
    >
      <Download className="size-4" /> Download
    </a>
  )
}

const KIND_LABEL: Partial<Record<ReturnType<typeof kindOf>, string>> = {
  pdf: 'PDF',
  archive: 'Archive',
  data: 'Data file',
  sheet: 'Spreadsheet',
  image: 'Image',
  audio: 'Audio',
  video: 'Video',
}

function NoPreview({
  id,
  entry,
  title = 'No preview',
  children,
}: {
  id: number
  entry: FileEntry
  title?: string
  children?: ReactNode
}) {
  return (
    <Empty
      icon={<KindIcon entry={entry} className="size-8" />}
      title={title}
      action={<DownloadButton id={id} path={entry.path} />}
    >
      {children ??
        [KIND_LABEL[kindOf(entry)], entry.size != null && fileSize(entry.size)]
          .filter(Boolean)
          .join(' · ')}
    </Empty>
  )
}

function ImageView({ id, entry }: { id: number; entry: FileEntry }) {
  const big = (entry.size ?? 0) > MAX_IMAGE_PREVIEW
  const img = useQuery({ ...imageQuery(id, entry.path, entry.mtime), enabled: !big })
  const [dims, setDims] = useState<string | null>(null)
  if (big) return <NoPreview id={id} entry={entry} title="Too big to preview" />
  if (img.isPending) return <Skeleton className="m-4 flex-1" />
  if (img.isError)
    return (
      <NoPreview id={id} entry={entry} title="Couldn't load this image">
        {img.error.message}
      </NoPreview>
    )
  // Only ever an image: an <img> doesn't run what's inside (SVG included).
  if (!img.data.data_url.startsWith('data:image/')) return <NoPreview id={id} entry={entry} />
  return (
    <div className="relative flex min-h-0 flex-1 items-center justify-center overflow-auto bg-sunken p-4 sm:p-6">
      <img
        src={img.data.data_url}
        alt={entry.name}
        onLoad={(e) =>
          setDims(`${e.currentTarget.naturalWidth} × ${e.currentTarget.naturalHeight}`)
        }
        className="max-h-full max-w-full rounded-md object-contain shadow-card [background:repeating-conic-gradient(var(--wp-line)_0_25%,var(--wp-panel)_0_50%)_0_0/16px_16px]"
      />
      {dims && (
        <span className="absolute bottom-2 right-3 rounded bg-panel/80 px-1.5 py-0.5 text-[11px] tabular-nums text-muted">
          {dims}
        </span>
      )}
    </div>
  )
}

function MediaView({ id, entry, video }: { id: number; entry: FileEntry; video: boolean }) {
  const [failed, setFailed] = useState(false)
  if (failed) return <NoPreview id={id} entry={entry} title="This browser can't play it" />
  const src = streamURL(id, entry.path)
  return (
    <div className="flex min-h-0 flex-1 items-center justify-center bg-sunken p-4 sm:p-6">
      {video ? (
        <video
          controls
          playsInline
          preload="metadata"
          src={src}
          onError={() => setFailed(true)}
          className="max-h-full max-w-full rounded-lg bg-black shadow-card"
        />
      ) : (
        <audio
          controls
          preload="metadata"
          src={src}
          onError={() => setFailed(true)}
          className="w-full max-w-md"
        />
      )}
    </div>
  )
}

type Mode = 'rendered' | 'raw'

function TextView({
  id,
  entry,
  file,
  kind,
}: {
  id: number
  entry: FileEntry
  file: TextFile
  kind: ReturnType<typeof kindOf>
}) {
  const [mode, setMode] = useState<Mode>('rendered')
  const [wrap, setWrap] = useLocal('assistant-files-wrap', false)
  const pretty = useMemo(() => {
    if (kind !== 'json' || file.truncated) return null
    try {
      return JSON.stringify(JSON.parse(file.text), null, 2)
    } catch {
      return null
    }
  }, [kind, file])
  const markdown = kind === 'markdown'
  const canRender = markdown || pretty !== null
  const rendered = canRender && mode === 'rendered'
  const shown = rendered && pretty !== null ? pretty : file.text
  return (
    <>
      <div className="flex shrink-0 items-center gap-2 border-b border-line px-2 py-1.5 sm:px-3">
        {canRender && (
          <SegmentedControl
            label="Show as"
            value={mode}
            onChange={setMode}
            textFrom="always"
            options={[
              markdown
                ? { value: 'rendered', label: 'Preview', icon: <Eye className="size-3.5" /> }
                : { value: 'rendered', label: 'Pretty', icon: <Braces className="size-3.5" /> },
              { value: 'raw', label: 'Raw', icon: <Code className="size-3.5" /> },
            ]}
          />
        )}
        {file.truncated && (
          <span className="min-w-0 truncate text-xs text-warn">
            First {fileSize(PREVIEW_LIMIT)} of {fileSize(file.byteSize)}
          </span>
        )}
        <span className="flex-1" />
        {!(rendered && markdown) && (
          <ToggleButton
            label="Wrap lines"
            pressed={wrap}
            onPressedChange={setWrap}
            icon={<WrapText className="size-4" />}
            className="size-9 sm:size-7"
          />
        )}
        {file.truncated && (
          <a
            href={downloadURL(id, entry.path)}
            download
            className="shrink-0 text-xs font-medium text-accent hover:underline"
          >
            Download all
          </a>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-auto bg-panel">
        {rendered && markdown ? (
          <Markdown text={file.text} className="mx-auto max-w-3xl px-4 py-4 sm:px-6" />
        ) : (
          <Lines text={shown} wrap={wrap} />
        )}
      </div>
    </>
  )
}

/** Text with line numbers that stay put while long lines scroll. */
function Lines({ text, wrap }: { text: string; wrap: boolean }) {
  const lines = useMemo(() => {
    const l = text.split('\n')
    if (l.length > 1 && l[l.length - 1] === '') l.pop()
    return l
  }, [text])
  const gutter = `${String(lines.length).length + 2}ch`
  return (
    <div className={clsx('py-2 font-mono text-xs leading-5', wrap ? 'w-full' : 'w-max min-w-full')}>
      {lines.map((line, i) => (
        <div key={i} className="flex hover:bg-hover">
          <span
            aria-hidden
            style={{ width: gutter }}
            className="sticky left-0 shrink-0 select-none bg-panel pr-3 text-right text-subtle"
          >
            {i + 1}
          </span>
          <span
            className={clsx(
              'min-w-0 pr-4 text-fg',
              wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre',
            )}
          >
            {line || ' '}
          </span>
        </div>
      ))}
    </div>
  )
}
