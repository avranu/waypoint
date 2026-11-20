// Editing the assistant's own Markdown (memory files, its persona, skills):
// a plain text area with a rendered preview one click away, a guard that
// asks before navigating away from unsaved text, and a short "Saved" mark.

import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useBlocker } from '@tanstack/react-router'
import clsx from 'clsx'
import { Check, Eye, Pencil } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Textarea } from '@/components/ui/input'
import { SegmentedControl } from '@/components/ui/segmented'
import { Markdown } from '../Transcript'

export type EditorMode = 'write' | 'preview'

/**
 * Markdown source with a Write/Preview switch. `preview` renders the text
 * another way than plain Markdown (memory entries, a skill's frontmatter).
 * Ctrl/Cmd+S saves.
 */
export function MarkdownEditor({
  value,
  onChange,
  onSave,
  label,
  preview,
  hint,
  placeholder,
  minHeight = 'min-h-[16rem]',
  autoFocus,
}: {
  value: string
  onChange: (v: string) => void
  onSave?: () => void
  label: string
  preview?: (text: string) => ReactNode
  hint?: ReactNode
  placeholder?: string
  minHeight?: string
  autoFocus?: boolean
}) {
  const [mode, setMode] = useState<EditorMode>('write')
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
        <SegmentedControl
          label={`${label}: write or preview`}
          value={mode}
          onChange={setMode}
          textFrom="always"
          options={[
            { value: 'write', label: 'Write', icon: <Pencil className="size-3" /> },
            { value: 'preview', label: 'Preview', icon: <Eye className="size-3" /> },
          ]}
        />
        {hint && <span className="text-xs text-subtle">{hint}</span>}
      </div>
      {mode === 'write' ? (
        <Textarea
          aria-label={label}
          value={value}
          autoGrow
          autoFocus={autoFocus}
          spellCheck
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
              e.preventDefault()
              onSave?.()
            }
          }}
          className={clsx('resize-y font-mono !text-[13px]', minHeight)}
        />
      ) : (
        <div
          className={clsx(
            'overflow-x-auto rounded-lg border border-line bg-panel px-3 py-2',
            minHeight,
          )}
        >
          {value.trim() ? (
            preview ? (
              preview(value)
            ) : (
              <Markdown text={value} />
            )
          ) : (
            <p className="text-[13px] text-subtle">Nothing to preview.</p>
          )}
        </div>
      )}
    </div>
  )
}

/** Cancel and Save for an editor, with an error in place. */
export function EditorActions({
  onCancel,
  onSave,
  saving,
  dirty,
  error,
  saveLabel = 'Save',
  extra,
}: {
  onCancel: () => void
  onSave: () => void
  saving: boolean
  dirty: boolean
  error?: string | null
  saveLabel?: string
  extra?: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center justify-end gap-2">
      {error && (
        <p role="alert" className="mr-auto min-w-0 text-[13px] text-danger">
          {error}
        </p>
      )}
      {!error && extra && <div className="mr-auto min-w-0">{extra}</div>}
      <Button variant="ghost" onClick={onCancel} disabled={saving}>
        Cancel
      </Button>
      <Button variant="primary" onClick={onSave} loading={saving} disabled={!dirty}>
        {saveLabel}
      </Button>
    </div>
  )
}

/** A "Saved" mark that shows for a few seconds after flash(). */
export function useSavedFlash(ms = 3000) {
  const [on, setOn] = useState(false)
  const t = useRef<ReturnType<typeof setTimeout>>(undefined)
  useEffect(() => () => clearTimeout(t.current), [])
  const flash = () => {
    clearTimeout(t.current)
    setOn(true)
    t.current = setTimeout(() => setOn(false), ms)
  }
  return [on, flash] as const
}

export function SavedMark({ show }: { show: boolean }) {
  return (
    <span
      role="status"
      className={clsx(
        'inline-flex items-center gap-1 text-xs font-medium text-ok transition-opacity',
        show ? 'opacity-100' : 'opacity-0',
      )}
    >
      {show && (
        <>
          <Check className="size-3.5" /> Saved
        </>
      )}
    </span>
  )
}

// ---- leaving with unsaved text ----

// Editors with unsaved text, by key. One LeaveGuard per tab reads it, so two
// open editors ask once, not twice.
const dirty = new Set<string>()

/** Marks the editor `key` as holding unsaved text while `on`. */
export function useDirty(key: string, on: boolean) {
  useEffect(() => {
    if (on) dirty.add(key)
    else dirty.delete(key)
    return () => {
      dirty.delete(key)
    }
  }, [key, on])
}

/** Forgets `key`'s unsaved text now, before navigating away on purpose. */
export const releaseDirty = (key: string) => dirty.delete(key)

/** Asks before leaving (another tab, another page, closing it) with unsaved text. */
export function LeaveGuard() {
  const b = useBlocker({
    shouldBlockFn: () => dirty.size > 0,
    enableBeforeUnload: () => dirty.size > 0,
    withResolver: true,
  })
  return (
    <Confirm
      open={b.status === 'blocked'}
      onClose={() => b.reset?.()}
      onConfirm={() => {
        dirty.clear()
        b.proceed?.()
      }}
      title="Discard your changes?"
      body="What you've typed here isn't saved yet. If you leave, it's lost."
      confirmLabel="Discard"
      cancelLabel="Keep editing"
    />
  )
}
