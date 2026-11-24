import { useSyncExternalStore } from 'react'
import { ApiError } from '@/lib/api'
import { joinPath, MAX_UPLOAD, numbered } from './model'

export type UploadStatus = 'queued' | 'uploading' | 'conflict' | 'error' | 'done'

export interface Upload {
  key: number
  assistant: number
  dir: string
  file: File
  /** the name it is stored under (numbered when keeping both) */
  name: string
  loaded: number
  status: UploadStatus
  error?: string
  overwrite: boolean
  /** keeping both: the next number to try on a clash */
  next?: number
}

/** At most this many at once; the rest wait their turn. */
const PARALLEL = 2

let items: Upload[] = []
let seq = 1
const aborts = new Map<number, () => void>()
const listeners = new Set<() => void>()
const refreshers = new Map<number, (dir: string) => void>()

function set(next: Upload[]) {
  items = next
  listeners.forEach((l) => l())
}

function patch(key: number, p: Partial<Upload>) {
  set(items.map((u) => (u.key === key ? { ...u, ...p } : u)))
}

function drop(key: number) {
  aborts.delete(key)
  set(items.filter((u) => u.key !== key))
}

export function useUploads(assistant: number): Upload[] {
  const all = useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => items,
  )
  return all.filter((u) => u.assistant === assistant)
}

/** Queue files for a folder; `refresh` runs for the folder after each one lands. */
export function enqueue(
  assistant: number,
  dir: string,
  files: File[],
  refresh: (dir: string) => void,
) {
  refreshers.set(assistant, refresh)
  const added: Upload[] = files.map((file) => ({
    key: seq++,
    assistant,
    dir,
    file,
    name: file.name,
    loaded: 0,
    overwrite: false,
    ...(file.size > MAX_UPLOAD
      ? { status: 'error' as const, error: 'Larger than 100 MB' }
      : { status: 'queued' as const }),
  }))
  set([...items, ...added])
  pump()
}

/** What to do about a name that's taken. */
export function resolve(key: number, choice: 'replace' | 'keep' | 'skip') {
  const u = items.find((x) => x.key === key)
  if (!u || u.status !== 'conflict') return
  if (choice === 'skip') return drop(key)
  if (choice === 'replace') patch(key, { status: 'queued', overwrite: true })
  else patch(key, { status: 'queued', name: numbered(u.file.name, 2), next: 3 })
  pump()
}

export function cancel(key: number) {
  aborts.get(key)?.()
  drop(key)
  pump()
}

export const dismiss = drop

function pump() {
  const running = items.filter((u) => u.status === 'uploading').length
  const waiting = items.filter((u) => u.status === 'queued').slice(0, PARALLEL - running)
  for (const u of waiting) start(u)
}

function start(u: Upload) {
  patch(u.key, { status: 'uploading', loaded: 0 })
  const form = new FormData()
  form.append('file', u.file, u.name)
  form.append('path', joinPath(u.dir, u.name))
  form.append('overwrite', u.overwrite ? 'true' : 'false')
  const xhr = new XMLHttpRequest()
  aborts.set(u.key, () => xhr.abort())
  xhr.open('POST', `/api/v1/assistants/${u.assistant}/api/files/upload-stream`)
  xhr.setRequestHeader('X-Waypoint-Client', 'web')
  xhr.withCredentials = true
  xhr.upload.onprogress = (e) => {
    if (e.lengthComputable) patch(u.key, { loaded: Math.min(e.loaded, u.file.size) })
  }
  const finish = (p: Partial<Upload>) => {
    aborts.delete(u.key)
    if (!items.some((x) => x.key === u.key)) return
    patch(u.key, p)
    pump()
  }
  xhr.onload = () => {
    if (xhr.status >= 200 && xhr.status < 300) {
      finish({ status: 'done', loaded: u.file.size })
      refreshers.get(u.assistant)?.(u.dir)
      setTimeout(() => drop(u.key), 2500)
      return
    }
    const err = new ApiError(xhr.status, message(xhr.responseText) || xhr.statusText)
    if (err.status === 409 && !u.overwrite) {
      if (u.next && u.next < 100)
        return finish({ status: 'queued', name: numbered(u.file.name, u.next), next: u.next + 1 })
      return finish({ status: 'conflict' })
    }
    finish({ status: 'error', error: err.message })
  }
  xhr.onerror = () => finish({ status: 'error', error: "Couldn't reach Waypoint" })
  xhr.send(form)
}

function message(text: string): string {
  try {
    const d = JSON.parse(text) as { detail?: unknown; error?: unknown }
    const v = d.detail ?? d.error
    return typeof v === 'string' ? v : v ? JSON.stringify(v) : ''
  } catch {
    return text.slice(0, 200)
  }
}
