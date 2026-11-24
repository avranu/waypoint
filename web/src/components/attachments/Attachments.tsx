import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { Download, FileText, Paperclip, Pencil, Play, Plus, Trash2 } from 'lucide-react'
import { api } from '@/lib/api'
import {
  attachmentsQuery,
  downloadURL,
  fileSize,
  refreshAttachments,
  uploadAttachment,
} from '@/lib/attachments'
import type { Attachment } from '@/lib/types'
import { relativeTime } from '@/lib/format'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'
import { Lightbox } from './Lightbox'
import { TextPreview } from './TextPreview'

export type { Attachment }

type Wrap = (title: string, actions: ReactNode, body: ReactNode) => ReactNode

const defaultWrap: Wrap = (title, actions, body) => (
  <section>
    <div className="mb-1.5 flex items-center justify-between">
      <h3 className="text-xs font-semibold uppercase tracking-wide text-subtle">{title}</h3>
      {actions}
    </div>
    {body}
  </section>
)

export function Attachments({
  target,
  files: given,
  editable,
  wrap = defaultWrap,
  startAdding = false,
}: {
  /** What the files belong to: issue:WP-12, page:12, design:3… */
  target: string
  /** Files already loaded with their target (an issue's); otherwise fetched. */
  files?: Attachment[]
  editable: boolean
  /** Renders the section chrome (title, actions) around the content. */
  wrap?: Wrap
  /** Opened from "+ File": the file chooser opens straight away. */
  startAdding?: boolean
}) {
  const qc = useQueryClient()
  const input = useRef<HTMLInputElement>(null)
  // Only when just asked: the click that asked still counts as the person's,
  // so the browser allows it.
  const asked = useRef(startAdding)
  useEffect(() => {
    if (startAdding && !asked.current && editable) input.current?.click()
    asked.current = startAdding
  }, [startAdding, editable])
  const loaded = useQuery({ ...attachmentsQuery(target), enabled: given === undefined })
  const files = given ?? loaded.data ?? []
  const [dragging, setDragging] = useState(false)
  const [deleting, setDeleting] = useState<Attachment | null>(null)
  const [captioning, setCaptioning] = useState<Attachment | null>(null)
  const [lightbox, setLightbox] = useState<number | null>(null)
  const [preview, setPreview] = useState<Attachment | null>(null)
  const upload = useMutation({
    mutationFn: async (list: File[]) => {
      for (const f of list) {
        try {
          await uploadAttachment(target, f)
        } catch (e) {
          toastError(e)
        }
      }
    },
    onSettled: () => refreshAttachments(qc, target),
  })
  const del = useMutation({
    mutationFn: (id: number) => api.del(`/attachments/${id}`),
    onSettled: () => refreshAttachments(qc, target),
    onError: toastError,
  })
  const caption = useMutation({
    mutationFn: (v: { id: number; caption: string }) =>
      api.patch<Attachment>(`/attachments/${v.id}`, { caption: v.caption }),
    onSettled: () => refreshAttachments(qc, target),
    onError: toastError,
  })
  if (!editable && files.length === 0) return null

  const images = files.filter((f) => f.kind === 'image')
  const videos = files.filter((f) => f.kind === 'video')
  const others = files.filter((f) => f.kind !== 'image' && f.kind !== 'video')
  const pick = (list: FileList | null) => list && list.length && upload.mutate([...list])
  const actions = editable && (
    <>
      <IconButton
        label={upload.isPending ? 'Uploading…' : 'Add file'}
        size="xs"
        onClick={() => input.current?.click()}
        disabled={upload.isPending}
        icon={upload.isPending ? <Spinner className="size-3.5" /> : <Plus className="size-3.5" />}
      />
      <input
        ref={input}
        type="file"
        multiple
        hidden
        aria-label="Choose files to attach"
        onChange={(e) => {
          pick(e.target.files)
          e.target.value = ''
        }}
      />
    </>
  )
  const itemActions = (f: Attachment, cls?: string) =>
    editable && (
      <span className={clsx('flex shrink-0 items-center', cls)}>
        <IconButton
          label={`Edit caption of ${f.name}`}
          size="xs"
          onClick={() => setCaptioning(f)}
          icon={<Pencil className="size-3" />}
        />
        <IconButton
          label={`Remove ${f.name}`}
          size="xs"
          onClick={() => setDeleting(f)}
          icon={<Trash2 className="size-3" />}
        />
      </span>
    )
  const body = (
    <div
      data-attachments={target}
      onDragOver={(e) => {
        if (!editable || !e.dataTransfer.types.includes('Files')) return
        e.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        if (!editable) return
        e.preventDefault()
        setDragging(false)
        pick(e.dataTransfer.files)
      }}
      className={clsx('rounded-lg', dragging && 'outline-dashed outline-2 outline-accent')}
    >
      {files.length === 0 ? (
        <button
          onClick={() => input.current?.click()}
          className="flex w-full items-center justify-center gap-2 rounded-lg border border-dashed border-line-strong px-3 py-4 text-xs text-subtle hover:bg-hover hover:text-fg"
        >
          <Paperclip className="size-3.5" /> Drop files here or click to attach
        </button>
      ) : (
        <div className="space-y-2">
          {images.length > 0 && (
            <ul className="grid grid-cols-2 gap-2 sm:grid-cols-[repeat(auto-fill,minmax(10rem,1fr))]">
              {images.map((f, i) => (
                <li key={f.id} className="group min-w-0">
                  <div className="relative overflow-hidden rounded-lg border border-line bg-sunken">
                    <button
                      onClick={() => setLightbox(i)}
                      title={f.caption || f.name}
                      aria-label={`View ${f.caption || f.name}`}
                      className="block aspect-square w-full"
                    >
                      <img
                        src={f.url}
                        alt={f.caption || f.name}
                        loading="lazy"
                        className="size-full object-cover"
                      />
                    </button>
                    {itemActions(
                      f,
                      'absolute right-1 top-1 rounded-md bg-raised/90 opacity-0 shadow-card group-hover:opacity-100 group-focus-within:opacity-100 max-sm:opacity-100',
                    )}
                  </div>
                  {f.caption && (
                    <p className="mt-0.5 truncate text-xs text-subtle" title={f.caption}>
                      {f.caption}
                    </p>
                  )}
                </li>
              ))}
            </ul>
          )}
          {videos.map((f) => (
            <figure key={f.id} className="group max-w-xl">
              <video
                src={f.url}
                controls
                preload="metadata"
                playsInline
                aria-label={f.caption || f.name}
                className="max-h-96 w-full rounded-lg border border-line bg-black"
              />
              <figcaption className="mt-0.5 flex items-center gap-2 text-xs text-subtle">
                <Play className="size-3 shrink-0" />
                <span className="min-w-0 flex-1 truncate" title={f.name}>
                  {f.caption || f.name}
                  <span className="ml-1.5">{fileSize(f.size)}</span>
                </span>
                <a
                  href={downloadURL(f)}
                  aria-label={`Download ${f.name}`}
                  title="Download"
                  className="inline-flex size-6 items-center justify-center rounded-md hover:bg-hover hover:text-fg"
                >
                  <Download className="size-3" />
                </a>
                {itemActions(f)}
              </figcaption>
            </figure>
          ))}
          {others.length > 0 && (
            <ul className="divide-y divide-line overflow-hidden rounded-lg border border-line">
              {others.map((f) => (
                <li key={f.id} className="group flex items-center gap-2.5 px-3 py-2 text-[13px]">
                  <FileText className="size-4 shrink-0 text-subtle" />
                  <div className="min-w-0 flex-1">
                    {f.kind === 'text' ? (
                      <button
                        onClick={() => setPreview(f)}
                        className="block max-w-full truncate text-left hover:underline"
                        title="Preview"
                      >
                        {f.name}
                      </button>
                    ) : (
                      <a href={downloadURL(f)} className="block truncate hover:underline">
                        {f.name}
                      </a>
                    )}
                    {f.caption && <div className="truncate text-xs text-muted">{f.caption}</div>}
                  </div>
                  <span className="hidden shrink-0 text-xs text-subtle sm:inline">
                    {fileSize(f.size)} · {relativeTime(f.created_at)}
                  </span>
                  <a
                    href={downloadURL(f)}
                    aria-label={`Download ${f.name}`}
                    title="Download"
                    className="inline-flex size-7 shrink-0 items-center justify-center rounded-md text-muted hover:bg-hover hover:text-fg"
                  >
                    <Download className="size-3.5" />
                  </a>
                  {itemActions(
                    f,
                    'sm:opacity-0 sm:group-hover:opacity-100 sm:group-focus-within:opacity-100',
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}
      {lightbox !== null && images.length > 0 && (
        <Lightbox
          images={images}
          index={Math.min(lightbox, images.length - 1)}
          onIndex={setLightbox}
          onClose={() => setLightbox(null)}
        />
      )}
      <TextPreview file={preview} onClose={() => setPreview(null)} />
      <CaptionDialog
        file={captioning}
        onClose={() => setCaptioning(null)}
        onSave={(v) => captioning && caption.mutate({ id: captioning.id, caption: v })}
      />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Remove ${deleting?.name}?`}
        body="The file is deleted for everyone. Links to it in text will stop working."
        confirmLabel="Remove"
        onConfirm={() => deleting && del.mutate(deleting.id)}
      />
    </div>
  )
  return wrap(files.length ? `Attachments · ${files.length}` : 'Attachments', actions, body)
}

function CaptionDialog({
  file,
  onClose,
  onSave,
}: {
  file: Attachment | null
  onClose: () => void
  onSave: (caption: string) => void
}) {
  const [value, setValue] = useState('')
  const [seen, setSeen] = useState<number | null>(null)
  if (file && file.id !== seen) {
    setSeen(file.id)
    setValue(file.caption ?? '')
  }
  if (!file && seen !== null) setSeen(null)
  const save = () => {
    onSave(value.trim())
    onClose()
  }
  return (
    <Dialog
      open={!!file}
      onClose={onClose}
      title="Caption"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" onClick={save}>
            Save
          </Button>
        </>
      }
    >
      <form
        className="space-y-2 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          save()
        }}
      >
        <p className="truncate text-xs text-subtle">{file?.name}</p>
        <Input
          autoFocus
          value={value}
          onChange={(e) => setValue(e.target.value)}
          maxLength={500}
          placeholder="What it shows"
          aria-label="Caption"
        />
      </form>
    </Dialog>
  )
}
