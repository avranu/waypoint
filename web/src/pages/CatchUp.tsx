import { useEffect, useMemo, useRef, useState, type ClipboardEvent } from 'react'
import { Link, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  ArrowLeft,
  ArrowRight,
  Bug,
  CircleCheckBig,
  ExternalLink,
  GitPullRequest,
  ImagePlus,
  Loader2,
  MonitorUp,
  Undo2,
  X,
} from 'lucide-react'
import { catchUpQuery, useNotFinished, useReportProblem, useReviewCloses } from '@/lib/queries'
import type { CatchUpItem } from '@/lib/catch-up'
import { STAGES } from '@/lib/delivery'
import { agentLabel } from '@/lib/agents'
import { issueTarget, uploadAttachment } from '@/lib/attachments'
import { renderMarkdown } from '@/lib/markdown'
import { plural, relativeTime, safeUrl } from '@/lib/format'
import { useHotkey } from '@/lib/hooks'
import { StageIcon } from '@/components/delivery'
import { Lightbox } from '@/components/attachments/Lightbox'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Textarea } from '@/components/ui/input'
import { MarkdownField } from '@/components/editor/MarkdownField'
import { Badge, Empty, Kbd, PageHeader, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

export function CatchUpPage() {
  const { project } = useSearch({ from: '/app/catch-up' })
  const query = useQuery(catchUpQuery(project))
  const [index, setIndex] = useState(0)
  const items = query.data?.items ?? []
  const n = items.length
  const at = Math.min(index, Math.max(n - 1, 0))
  const it = items[at] as CatchUpItem | undefined
  useEffect(() => {
    document.title = 'Catch up · Waypoint'
  }, [])
  const [dialog, setDialog] = useState<'' | 'not-finished' | 'problem'>('')
  const prev = () => setIndex(Math.max(at - 1, 0))
  const next = () => setIndex(Math.min(at + 1, n - 1))
  const free = () => !document.querySelector('[role="dialog"]')
  useHotkey('ArrowLeft', () => free() && prev(), { enabled: n > 1 })
  useHotkey('ArrowRight', () => free() && next(), { enabled: n > 1 })
  const total = query.data?.total ?? 0

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-4xl">
        <PageHeader
          icon={<CircleCheckBig className="size-5 text-accent" />}
          title={project ? `Catch up · ${project}` : 'Catch up'}
          subtitle={
            <span className="inline-flex items-center gap-1.5">
              {query.data
                ? total
                  ? `${plural(total, 'finished task')} no one has looked at`
                  : 'You’ve seen everything agents finished'
                : 'What agents finished'}
              {query.isFetching && <Loader2 className="size-3 animate-spin" aria-label="Reading" />}
            </span>
          }
          actions={
            n > 1 ? (
              <div className="flex items-center gap-1">
                <Button
                  variant="ghost"
                  size="sm"
                  icon={<ArrowLeft />}
                  onClick={prev}
                  disabled={at === 0}
                  aria-label="Previous"
                />
                <span className="min-w-14 text-center text-[13px] tabular-nums text-muted">
                  {at + 1} of {n}
                </span>
                <Button
                  variant="ghost"
                  size="sm"
                  icon={<ArrowRight />}
                  onClick={next}
                  disabled={at >= n - 1}
                  aria-label="Next"
                />
              </div>
            ) : undefined
          }
        />
        <div className="px-4 pb-6 sm:px-6">
          {query.isPending ? (
            <div className="space-y-3">
              <Skeleton className="h-8" />
              <Skeleton className="h-48" />
            </div>
          ) : !query.data ? (
            <Empty icon={<CircleCheckBig />} title="Couldn’t load what agents finished" />
          ) : !it ? (
            <Empty icon={<CircleCheckBig />} title="All caught up">
              When an agent finishes a task, it shows here until you’ve looked at it.
            </Empty>
          ) : (
            <>
              <Slide key={it.issue.key} it={it} />
              <Actions it={it} onDialog={setDialog} />
              {n > 1 && (
                <p className="mt-3 hidden text-center text-xs text-subtle sm:block">
                  <Kbd>←</Kbd> <Kbd>→</Kbd> to move between tasks
                </p>
              )}
              {total > n && (
                <p className="mt-2 text-center text-xs text-subtle">
                  Showing the oldest {n}; the rest come after these.
                </p>
              )}
              <NotFinishedDialog
                key={`nf-${it.issue.key}`}
                it={it}
                open={dialog === 'not-finished'}
                onClose={() => setDialog('')}
              />
              <ProblemDialog
                key={`pr-${it.issue.key}`}
                it={it}
                open={dialog === 'problem'}
                onClose={() => setDialog('')}
              />
            </>
          )}
        </div>
      </div>
    </div>
  )
}

function Slide({ it }: { it: CatchUpItem }) {
  const html = useMemo(
    () => (it.summary ? renderMarkdown(it.summary, { project: it.project }) : ''),
    [it.summary, it.project],
  )
  const [shown, setShown] = useState<number | null>(null)
  const stage = it.live ? STAGES.find((s) => s.stage === it.live) : undefined
  const pr = it.pr
  return (
    <article
      className="rounded-xl border border-line bg-panel shadow-card"
      data-testid="catch-up-item"
    >
      <header className="border-b border-line px-4 py-3 sm:px-5">
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted">
          <Link
            to="/issue/$key"
            params={{ key: it.issue.key }}
            className="font-mono hover:underline"
          >
            {it.issue.key}
          </Link>
          {it.agent && <Badge>{agentLabel(it.agent)}</Badge>}
          {it.category === 'canceled' && <Badge color="var(--wp-warn)">{it.status}</Badge>}
          <span>finished {relativeTime(it.resolved_at)}</span>
        </div>
        <h2 className="mt-1 text-[17px] font-semibold leading-snug">
          <Link to="/issue/$key" params={{ key: it.issue.key }} className="hover:underline">
            {it.issue.title}
          </Link>
        </h2>
        {(pr || stage) && (
          <div className="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
            {pr && (
              <a
                href={safeUrl(pr.url)}
                target="_blank"
                rel="noreferrer"
                className="inline-flex min-w-0 items-center gap-1.5 text-muted hover:text-fg"
              >
                <GitPullRequest className="size-4 shrink-0" />
                <span className="truncate">
                  {pr.repo}#{pr.number}
                </span>
                <span className="text-subtle">{pr.state}</span>
              </a>
            )}
            {stage && it.live && (
              <Link
                to="/live"
                className="inline-flex items-center gap-1.5 text-muted hover:text-fg"
              >
                <StageIcon stage={it.live} decorative />
                {it.live === 'live' && it.live_in?.length
                  ? `Live in ${it.live_in.join(', ')}`
                  : stage.title}
              </Link>
            )}
          </div>
        )}
      </header>

      {it.try.length > 0 && (
        <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-3 sm:px-5">
          <span className="text-[13px] font-medium">Try it</span>
          {it.try.map((t) =>
            t.url ? (
              <a
                key={t.id}
                href={safeUrl(t.url)}
                target="_blank"
                rel="noreferrer"
                className="inline-flex max-w-full items-center gap-1 rounded-md bg-sunken px-2 py-1 text-[13px] text-accent hover:bg-hover"
              >
                <span className="truncate">{t.url.replace(/^https?:\/\//, '')}</span>
                <ExternalLink className="size-3.5 shrink-0" />
              </a>
            ) : (
              <span
                key={t.id}
                className="max-w-full truncate rounded-md bg-sunken px-2 py-1 font-mono text-xs text-muted"
                title="No production or staging environment has a URL to open this on"
              >
                {t.raw}
              </span>
            ),
          )}
        </div>
      )}

      <div className="px-4 py-4 sm:px-5">
        {html ? (
          <div className="prose-wp text-[14px]" dangerouslySetInnerHTML={{ __html: html }} />
        ) : (
          <p className="text-[14px] text-muted">The agent left no summary.</p>
        )}
      </div>

      {it.shots.length > 0 && (
        <div className="border-t border-line px-4 py-4 sm:px-5">
          <div className="grid gap-3 sm:grid-cols-2">
            {it.shots.map((a, i) => (
              <figure key={a.id} className="min-w-0">
                <button
                  type="button"
                  onClick={() => setShown(i)}
                  className="block w-full overflow-hidden rounded-lg border border-line bg-sunken"
                  aria-label={`Open ${a.caption || a.name}`}
                >
                  <img
                    src={a.url}
                    alt={a.caption || a.name}
                    loading="lazy"
                    className="max-h-72 w-full object-cover object-top"
                  />
                </button>
                {a.caption && (
                  <figcaption className="mt-1 truncate text-xs text-muted">{a.caption}</figcaption>
                )}
              </figure>
            ))}
          </div>
        </div>
      )}
      {shown !== null && (
        <Lightbox
          images={it.shots}
          index={shown}
          onIndex={setShown}
          onClose={() => setShown(null)}
        />
      )}
    </article>
  )
}

function Actions({
  it,
  onDialog,
}: {
  it: CatchUpItem
  onDialog: (d: 'not-finished' | 'problem') => void
}) {
  const review = useReviewCloses()
  return (
    <div className="sticky bottom-0 mt-3 flex flex-wrap items-center gap-2 border-t border-line bg-bg py-3 sm:static sm:border-0 sm:bg-transparent sm:py-0">
      <Button
        variant="primary"
        icon={<CircleCheckBig />}
        loading={review.isPending}
        onClick={() =>
          review.mutate(
            { issues: [it.issue.key], projectKey: it.project },
            {
              onSuccess: () => toast(`${it.issue.key} reviewed`, { kind: 'success' }),
              onError: toastError,
            },
          )
        }
      >
        Looks good
      </Button>
      <Button variant="secondary" icon={<Undo2 />} onClick={() => onDialog('not-finished')}>
        Not finished
      </Button>
      <Button variant="ghost" icon={<Bug />} onClick={() => onDialog('problem')}>
        Report a problem
      </Button>
    </div>
  )
}

function NotFinishedDialog({
  it,
  open,
  onClose,
}: {
  it: CatchUpItem
  open: boolean
  onClose: () => void
}) {
  const [note, setNote] = useState('')
  const send = useNotFinished()
  const submit = () =>
    send.mutate(
      { key: it.issue.key, note: note.trim(), projectKey: it.project },
      {
        onSuccess: () => {
          toast(`${it.issue.key} reopened`, { kind: 'success' })
          setNote('')
          onClose()
        },
        onError: toastError,
      },
    )
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title={`Send ${it.issue.key} back`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={send.isPending} onClick={submit}>
            Reopen it
          </Button>
        </>
      }
    >
      <div className="p-4">
        <p className="mb-2 text-[13px] text-muted">
          It goes back to To do with your note as a comment, for whoever picks it up next.
        </p>
        <MarkdownField
          autoFocus
          rows={4}
          value={note}
          onValueChange={setNote}
          issueKey={it.issue.key}
          placeholder="What’s missing (optional)"
          ariaLabel="What’s missing"
          onKeyDown={(e) => {
            if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !send.isPending) submit()
          }}
        />
      </div>
    </Dialog>
  )
}

interface Picture {
  file: File
  id: number
}

let nextPictureId = 0

/** A thumbnail of a picked file, drawn on a canvas so no URL is ever built from it. */
function Thumb({ file, label }: { file: File; label: string }) {
  const ref = useRef<HTMLCanvasElement>(null)
  useEffect(() => {
    let gone = false
    createImageBitmap(file)
      .then((bmp) => {
        const c = ref.current
        const g = c?.getContext('2d')
        if (gone || !c || !g) return bmp.close()
        const scale = Math.max(c.width / bmp.width, c.height / bmp.height)
        g.drawImage(bmp, 0, 0, bmp.width * scale, bmp.height * scale)
        bmp.close()
      })
      .catch(() => {})
    return () => {
      gone = true
    }
  }, [file])
  return (
    <canvas
      ref={ref}
      width={256}
      height={160}
      role="img"
      aria-label={label}
      className="h-20 w-32 rounded-md border border-line"
    />
  )
}

function ProblemDialog({
  it,
  open,
  onClose,
}: {
  it: CatchUpItem
  open: boolean
  onClose: () => void
}) {
  const [note, setNote] = useState('')
  const [pictures, setPictures] = useState<Picture[]>([])
  const [busy, setBusy] = useState(false)
  const [capturing, setCapturing] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)
  const report = useReportProblem()
  const canCapture = typeof navigator !== 'undefined' && !!navigator.mediaDevices?.getDisplayMedia

  const add = (files: File[]) => {
    const images = files.filter((f) => f.type.startsWith('image/'))
    if (images.length)
      setPictures((p) => [...p, ...images.map((file) => ({ file, id: nextPictureId++ }))])
  }
  const remove = (i: number) => setPictures((p) => p.filter((_, j) => j !== i))
  const reset = () => {
    setPictures([])
    setNote('')
  }
  const onPaste = (e: ClipboardEvent) => {
    const files = Array.from(e.clipboardData.files)
    if (files.some((f) => f.type.startsWith('image/'))) {
      e.preventDefault()
      add(files)
    }
  }
  const capture = async () => {
    setCapturing(true)
    try {
      const file = await grabScreen()
      if (file) add([file])
      else toast('The screen had no picture to take yet. Try again.', { kind: 'error' })
    } catch (e) {
      if (!(e instanceof DOMException && e.name === 'NotAllowedError')) toastError(e)
    } finally {
      setCapturing(false)
    }
  }
  const submit = async () => {
    setBusy(true)
    try {
      const bug = await report.mutateAsync({
        key: it.issue.key,
        note: note.trim(),
        projectKey: it.project,
      })
      const failed: string[] = []
      for (const p of pictures) {
        try {
          await uploadAttachment(issueTarget(bug.key), p.file)
        } catch {
          failed.push(p.file.name)
        }
      }
      toast(
        failed.length
          ? `Filed ${bug.key}, but ${plural(failed.length, 'picture')} didn’t upload`
          : `Filed ${bug.key}`,
        { kind: failed.length ? 'error' : 'success', href: `/issue/${bug.key}` },
      )
      reset()
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
      title={`Report a problem with ${it.issue.key}`}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" loading={busy} onClick={submit}>
            File it
          </Button>
        </>
      }
    >
      <div className="p-4">
        <p className="mb-2 text-[13px] text-muted">
          A new bug, linked to {it.issue.key}. The first line is its title; with no text it’s named
          after the task.
        </p>
        <Textarea
          autoFocus
          rows={4}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          onPaste={onPaste}
          placeholder="What’s wrong (optional; paste a screenshot here too)"
          aria-label="What’s wrong"
        />
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {canCapture && (
            <Button
              variant="secondary"
              size="sm"
              icon={<MonitorUp />}
              loading={capturing}
              onClick={capture}
            >
              Capture screen
            </Button>
          )}
          <Button
            variant="secondary"
            size="sm"
            icon={<ImagePlus />}
            onClick={() => fileInput.current?.click()}
          >
            Add a picture
          </Button>
          <input
            ref={fileInput}
            type="file"
            accept="image/*"
            multiple
            hidden
            onChange={(e) => {
              add(Array.from(e.target.files ?? []))
              e.target.value = ''
            }}
          />
        </div>
        {pictures.length > 0 && (
          <ul className="mt-3 flex flex-wrap gap-2" aria-label="Pictures">
            {pictures.map((p, i) => (
              <li key={p.id} className="relative">
                <Thumb file={p.file} label={`Picture ${i + 1}`} />
                <button
                  type="button"
                  onClick={() => remove(i)}
                  className="absolute right-1 top-1 rounded bg-bg/90 p-0.5 text-muted hover:text-fg"
                  aria-label={`Remove ${p.file.name}`}
                >
                  <X className="size-3.5" />
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Dialog>
  )
}

/** One frame of a screen, window or tab the person picks, as a PNG. */
async function grabScreen(): Promise<File | null> {
  const stream = await navigator.mediaDevices.getDisplayMedia({ video: true, audio: false })
  try {
    const video = document.createElement('video')
    video.muted = true
    video.srcObject = stream
    await video.play()
    await new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r)))
    const canvas = document.createElement('canvas')
    canvas.width = video.videoWidth
    canvas.height = video.videoHeight
    canvas.getContext('2d')?.drawImage(video, 0, 0)
    const blob = await new Promise<Blob | null>((r) => canvas.toBlob(r, 'image/png'))
    return blob ? new File([blob], `screen-${Date.now()}.png`, { type: 'image/png' }) : null
  } finally {
    stream.getTracks().forEach((t) => t.stop())
  }
}
