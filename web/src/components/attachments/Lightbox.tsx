import { useEffect, useRef } from 'react'
import { createPortal } from 'react-dom'
import { ChevronLeft, ChevronRight, Download, X } from 'lucide-react'
import { downloadURL, fileSize } from '@/lib/attachments'
import type { Attachment } from '@/lib/types'
import { useEscapeLayer } from '@/components/ui/layers'

/** A full-window view of one image, with arrows through the rest. */
export function Lightbox({
  images,
  index,
  onIndex,
  onClose,
}: {
  images: Attachment[]
  index: number
  onIndex: (i: number) => void
  onClose: () => void
}) {
  const close = useRef<HTMLButtonElement>(null)
  const f = images[index]
  const n = images.length
  useEscapeLayer(true, onClose)
  useEffect(() => {
    const prev = document.activeElement as HTMLElement | null
    close.current?.focus()
    return () => prev?.focus?.()
  }, [])
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (n < 2 || (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight')) return
      e.preventDefault()
      onIndex((index + (e.key === 'ArrowRight' ? 1 : -1) + n) % n)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [index, n, onIndex])
  const btn =
    'inline-flex size-10 items-center justify-center rounded-full bg-black/50 text-white hover:bg-black/70 focus-visible:outline-2 focus-visible:outline-white'
  return createPortal(
    <div
      role="dialog"
      aria-modal
      aria-label={f.caption || f.name}
      className="fixed inset-0 z-[80] flex flex-col bg-black/90 text-white"
      onClick={onClose}
    >
      <div className="flex items-center gap-2 p-3" onClick={(e) => e.stopPropagation()}>
        <div className="min-w-0 flex-1">
          <div className="truncate text-sm font-medium">{f.caption || f.name}</div>
          <div className="truncate text-xs text-white/60">
            {f.caption ? `${f.name} · ` : ''}
            {fileSize(f.size)}
            {n > 1 && ` · ${index + 1} of ${n}`}
          </div>
        </div>
        <a href={downloadURL(f)} aria-label={`Download ${f.name}`} title="Download" className={btn}>
          <Download className="size-4" />
        </a>
        <button ref={close} onClick={onClose} aria-label="Close" title="Close" className={btn}>
          <X className="size-4" />
        </button>
      </div>
      <div className="relative flex min-h-0 flex-1 items-center justify-center px-3 pb-3">
        <img
          src={f.url}
          alt={f.caption || f.name}
          className="max-h-full max-w-full rounded object-contain"
          onClick={(e) => e.stopPropagation()}
        />
        {n > 1 && (
          <>
            <button
              onClick={(e) => {
                e.stopPropagation()
                onIndex((index - 1 + n) % n)
              }}
              aria-label="Previous image"
              className={`${btn} absolute left-3 top-1/2 -translate-y-1/2`}
            >
              <ChevronLeft className="size-5" />
            </button>
            <button
              onClick={(e) => {
                e.stopPropagation()
                onIndex((index + 1) % n)
              }}
              aria-label="Next image"
              className={`${btn} absolute right-3 top-1/2 -translate-y-1/2`}
            >
              <ChevronRight className="size-5" />
            </button>
          </>
        )}
      </div>
    </div>,
    document.body,
  )
}
