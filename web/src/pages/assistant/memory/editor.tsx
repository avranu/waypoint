import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useBlocker } from '@tanstack/react-router'
import clsx from 'clsx'
import { Check } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { MarkdownField } from '@/components/editor/MarkdownField'

// Hermes splits a memory file at a line holding only §; the editor would fold
// that line into the item or quote above it.
const keepsEntries = (md: string) => !/^§$/m.test(md)

/**
 * Formatted text (or its markdown source) for a memory file, persona or
 * skill. Ctrl/Cmd+S saves. Text with front matter opens as markdown.
 */
export function MarkdownEditor({
  value,
  onChange,
  onSave,
  label,
  hint,
  placeholder,
  rows = 10,
  autoFocus,
}: {
  value: string
  onChange: (v: string) => void
  onSave?: () => void
  label: string
  hint?: ReactNode
  placeholder?: string
  /** The box's least height, in lines of text. */
  rows?: number
  autoFocus?: boolean
}) {
  return (
    <div className="space-y-2">
      {hint && <p className="text-xs text-subtle">{hint}</p>}
      <MarkdownField
        ariaLabel={label}
        value={value}
        onValueChange={onChange}
        autoFocus={autoFocus}
        placeholder={placeholder}
        rows={rows}
        keeps={keepsEntries}
        onKeyDown={(e) => {
          if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 's') {
            e.preventDefault()
            onSave?.()
          }
        }}
      />
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
