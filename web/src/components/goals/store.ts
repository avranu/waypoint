import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { q, useSavePrefs } from '@/lib/queries'

function read(key: string): Set<string> {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(key) ?? '[]')
    return new Set(Array.isArray(v) ? v.map(String) : [])
  } catch {
    return new Set()
  }
}

/** A set of strings kept in localStorage under `key`; follows `key` when it changes. */
export function useStoredSet(key: string) {
  const [state, setState] = useState(() => ({ key, set: read(key) }))
  let set = state.set
  if (state.key !== key) {
    set = read(key)
    setState({ key, set })
  }
  const change = (f: (next: Set<string>) => void) =>
    setState((prev) => {
      const next = new Set(prev.key === key ? prev.set : read(key))
      f(next)
      try {
        localStorage.setItem(key, JSON.stringify([...next]))
      } catch {}
      return { key, set: next }
    })
  return {
    has: (v: string) => set.has(v),
    add: (v: string) => change((n) => void n.add(v)),
    remove: (v: string) => change((n) => void n.delete(v)),
    toggle: (v: string) =>
      change((n) => {
        if (!n.delete(v)) n.add(v)
      }),
  }
}

/** The cap the server keeps (internal/core users.go maxDismissedWarnings). */
const MAX_DISMISSED = 500

/** One warning on one goal, as stored: "NOTE_ID:what it says", at most 200 characters. */
export function warningKey(note: number, warning: string): string {
  return [...`${note}:${warning.split(':')[0]}`].slice(0, 200).join('')
}

/**
 * The wording warnings this person dismissed, kept on their profile. Ones
 * this browser kept before (per project in localStorage) move over once.
 */
export function useDismissedWarnings(project: string) {
  const me = useQuery(q.me()).data
  const save = useSavePrefs()
  const list = me?.prefs.dismissed_warnings ?? []
  const loaded = !!me
  useEffect(() => {
    if (!loaded) return
    const old = `wp:goals:dismissed-warnings:${project}`
    const kept = [...read(old)].map((k) => {
      const [id, w = ''] = k.split(':')
      return warningKey(Number(id), w)
    })
    try {
      localStorage.removeItem(old)
    } catch {}
    if (!kept.length) return
    save.mutate({ dismissed_warnings: merge(list, kept) })
  }, [loaded, project]) // eslint-disable-line react-hooks/exhaustive-deps
  return {
    has: (k: string) => list.includes(k),
    add: (k: string) => save.mutate({ dismissed_warnings: merge(list, [k]) }),
  }
}

function merge(list: string[], add: string[]): string[] {
  const next = [...list.filter((k) => !add.includes(k)), ...new Set(add)]
  return next.slice(-MAX_DISMISSED)
}
