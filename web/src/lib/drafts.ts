// Unsent text kept in this browser (WP-68). Quick-add boxes, the new-issue
// dialog and the issue title and description editors write what's typed
// here as it's typed and clear it once the server has it, so a refresh, a
// crash or a closed tab doesn't lose it. When storage refuses (private
// mode, full), the text is registered as unsaved instead and UnsavedGuard
// asks before leaving; so does a save still in flight when the page unloads.

import { useEffect, useState, useSyncExternalStore } from 'react'

const PREFIX = 'wp:draft:'
// Drafts nobody came back to for a month are dropped on read.
const MAX_AGE_MS = 30 * 24 * 3600 * 1000

const isEmpty = (v: unknown): boolean =>
  v == null ||
  (typeof v === 'string' && !v.trim()) ||
  (typeof v === 'object' && Object.values(v as object).every(isEmpty))

export function readDraft<T>(key: string): T | null {
  try {
    const s = localStorage.getItem(PREFIX + key)
    if (s === null) return null
    const d = JSON.parse(s) as { v: T; at: number }
    if (Date.now() - d.at > MAX_AGE_MS) {
      localStorage.removeItem(PREFIX + key)
      return null
    }
    return d.v
  } catch {
    return null
  }
}

/** Stores (or, when empty, removes) a draft; false if storage refused it. */
export function writeDraft(key: string, v: unknown): boolean {
  try {
    if (isEmpty(v)) localStorage.removeItem(PREFIX + key)
    else localStorage.setItem(PREFIX + key, JSON.stringify({ v, at: Date.now() }))
    return true
  } catch {
    return false
  }
}

export const clearDraft = (key: string) => writeDraft(key, null)

/**
 * State mirrored to a draft as it changes; starts from the stored draft if
 * there is one (restored is then true). A null key keeps it in memory only.
 */
export function useDraft<T>(key: string | null, initial: T) {
  const load = (k: string | null) => {
    const d = k ? readDraft<T>(k) : null
    return { key: k, v: d ?? initial, restored: d !== null, kept: true }
  }
  const [s, setS] = useState(() => load(key))
  // A new key (another project, another issue) loads its own draft.
  let cur = s
  if (s.key !== key) {
    cur = load(key)
    setS(cur)
  }
  const set = (v: T) => {
    const kept = key ? writeDraft(key, v) : true
    setS({ key, v, restored: cur.restored && !isEmpty(v), kept })
  }
  useUnsaved(!cur.kept && !isEmpty(cur.v))
  return [cur.v, set, { restored: cur.restored }] as const
}

// ---- what can't be kept ----

let unsaved = 0
let inflight = 0
const subs = new Set<() => void>()
const changed = () => subs.forEach((f) => f())

/** True while there's text on screen that no draft holds. */
export const hasUnsaved = () => unsaved > 0
/** True while a save is on its way to the server. */
export const hasInflight = () => inflight > 0

export function useAnyUnsaved() {
  return useSyncExternalStore(
    (cb) => (subs.add(cb), () => subs.delete(cb)),
    () => unsaved > 0 || inflight > 0,
  )
}

/** Registers text on screen that no draft holds, for UnsavedGuard. */
export function useUnsaved(dirty: boolean) {
  useEffect(() => {
    if (!dirty) return
    unsaved++
    changed()
    return () => {
      unsaved--
      changed()
    }
  }, [dirty])
}

/** Marks a save as in flight until the promise settles. */
export function tracked<T>(p: Promise<T>): Promise<T> {
  inflight++
  changed()
  return p.finally(() => {
    inflight--
    changed()
  })
}
