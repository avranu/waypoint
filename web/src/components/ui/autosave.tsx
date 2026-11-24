import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import clsx from 'clsx'
import { Check } from 'lucide-react'

/**
 * Saves a value by itself (no Save button): `key` identifies the value,
 * `saved` is what the server has, and `save` runs once the value is valid
 * and differs. With a `delay` it saves that long after the last change
 * (0: at once, for a select or switch); with `null` it waits for `flush`,
 * which a field calls on blur or Enter so half-typed numbers and queries
 * never go out. Whatever is still unsaved when the form goes away is saved
 * then. `done` is true for a moment after a save went through.
 */
export function useAutosave(
  key: string,
  saved: string,
  ok: boolean,
  delay: number | null,
  save: () => Promise<unknown>,
) {
  const [done, setDone] = useState(false)
  const latest = useRef({ save, pending: false })
  latest.current = { save, pending: ok && key !== saved }
  // One save at a time, so they reach the server, and their answers come
  // back, in the order they were made; a change made meanwhile goes next.
  const inflight = useRef<Promise<unknown> | null>(null)
  const flush = useCallback(function run() {
    if (inflight.current) {
      void inflight.current.then(run)
      return
    }
    const l = latest.current
    if (!l.pending) return
    l.pending = false
    inflight.current = l
      .save()
      .then(
        () => setDone(true),
        () => setDone(false),
      )
      .finally(() => {
        inflight.current = null
      })
  }, [])
  useEffect(() => {
    if (delay === null || !ok || key === saved) return
    const t = window.setTimeout(flush, delay)
    return () => window.clearTimeout(t)
  }, [key, saved, ok, delay, flush])
  useEffect(() => flush, [flush])
  useEffect(() => {
    if (!done) return
    const t = window.setTimeout(() => setDone(false), 1800)
    return () => window.clearTimeout(t)
  }, [done])
  return { done, flush }
}

/**
 * What a form sent, so its own saves coming back (an older one too, answered
 * late) aren't taken for a change made elsewhere, which would replace what
 * is being typed. `mine(k)` is true for one of the last 20 it sent.
 */
export function useSent() {
  const keys = useRef<string[]>([])
  return useMemo(
    () => ({
      add: (k: string) => {
        keys.current = [...keys.current.slice(-19), k]
      },
      mine: (k: string) => keys.current.includes(k),
    }),
    [],
  )
}

/** A quiet "Saved" that shows for a moment after an autosave. */
export function SavedMark({ show }: { show: boolean }) {
  return (
    <span
      role="status"
      aria-live="polite"
      className={clsx(
        'inline-flex items-center gap-1 text-xs text-subtle transition-opacity',
        show ? 'opacity-100' : 'opacity-0',
      )}
    >
      {show && (
        <>
          <Check className="size-3" /> Saved
        </>
      )}
    </span>
  )
}
