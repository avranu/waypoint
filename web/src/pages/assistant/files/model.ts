import { queryOptions, type QueryClient } from '@tanstack/react-query'
import { hget, hermesQuery } from '@/lib/assistant'
import { qs } from '@/lib/api'

export interface FileEntry {
  name: string
  path: string
  is_directory: boolean
  /** null for folders and dangling links */
  size: number | null
  mtime: number | null
  mime_type: string | null
  broken_link?: boolean
}

/** Where the files start, and whether the person may go above it. */
export interface RootMeta {
  root: string | null
  locked_root: string | null
  can_change_path: boolean
}

export interface Listing extends RootMeta {
  path: string
  parent: string | null
  entries: FileEntry[]
}

export interface TextFile {
  binary: boolean
  byteSize: number
  language: string
  mimeType: string
  path: string
  text: string
  truncated: boolean
}

/** Hermes's limits (hermes-api.md §2.4). */
export const MAX_UPLOAD = 100 * 1024 * 1024
export const MAX_WRITE = 8 * 1024 * 1024
/** Images bigger than this are offered as downloads (they come back base64 in JSON). */
export const MAX_IMAGE_PREVIEW = 25 * 1024 * 1024

/** A folder's listing; no path means the files root. */
export const listingQuery = (id: number, path?: string) =>
  hermesQuery<Listing>(id, 'files', { path }, 10_000)

/** Every listing of this assistant's, for invalidation. */
export const listingsKey = (id: number) => ['assistant', id, 'files']

/** A file's text (the first 512 KB); `version` (its mtime) refetches it when it changes. */
export const textQuery = (id: number, path: string, version: number | null) =>
  queryOptions({
    queryKey: ['assistant', id, 'fs/read-text', path, version],
    queryFn: () => hget<TextFile>(id, 'fs/read-text', { path }),
    staleTime: 60_000,
  })

export const imageQuery = (id: number, path: string, version: number | null) =>
  queryOptions({
    queryKey: ['assistant', id, 'files/read', path, version],
    queryFn: () => hget<{ data_url: string; mime_type: string }>(id, 'files/read', { path }),
    staleTime: 60_000,
  })

/** An entry from any listing already fetched, to know a file from a folder without asking. */
export function seenEntry(qc: QueryClient, id: number, path: string): FileEntry | undefined {
  for (const [, l] of qc.getQueriesData<Listing>({ queryKey: listingsKey(id) })) {
    const e = l?.entries?.find((x) => x.path === path)
    if (e) return e
  }
}

/** The root, from any listing already fetched. */
export function seenRoot(qc: QueryClient, id: number): RootMeta | undefined {
  for (const [, l] of qc.getQueriesData<Listing>({ queryKey: listingsKey(id) }))
    if (l?.root !== undefined) return l
}

const api = (id: number) => `/api/v1/assistants/${id}/api`
export const downloadURL = (id: number, path: string) => `${api(id)}/files/download${qs({ path })}`
/** Audio and video, served inline with ranges. */
export const streamURL = (id: number, path: string) => `${api(id)}/files/stream${qs({ path })}`

export const trimSlash = (p: string) => (p.length > 1 ? p.replace(/\/+$/, '') : p)

export function parentOf(p: string): string {
  const t = trimSlash(p)
  const i = t.lastIndexOf('/')
  return i <= 0 ? '/' : t.slice(0, i)
}

export const joinPath = (dir: string, name: string) =>
  trimSlash(dir).replace(/\/$/, '') + '/' + name

export const baseName = (p: string) => trimSlash(p).split('/').pop() || '/'

/** A name a person typed for a new file or folder: one segment, nothing odd. */
export function badName(name: string): string | null {
  if (!name) return 'Give it a name.'
  if (name.includes('/') || name.includes('\\')) return "A name can't contain a slash."
  if (name === '.' || name === '..') return 'Pick another name.'
  if (name.length > 255) return 'That name is too long.'
  return null
}

/** "report (2).md" for "report.md", for keeping both on a clash. */
export function numbered(name: string, n: number): string {
  const dot = name.lastIndexOf('.')
  if (dot <= 0) return `${name} (${n})`
  return `${name.slice(0, dot)} (${n})${name.slice(dot)}`
}

export type Kind =
  | 'folder'
  | 'image'
  | 'audio'
  | 'video'
  | 'pdf'
  | 'markdown'
  | 'json'
  | 'code'
  | 'text'
  | 'sheet'
  | 'archive'
  | 'data'
  | 'other'

const EXT: Record<string, Kind> = {}
const put = (kind: Kind, exts: string) => exts.split(' ').forEach((e) => (EXT[e] = kind))
put('image', 'png jpg jpeg gif webp bmp svg ico avif')
// Hermes streams only these (R:files.py:618).
put('audio', 'mp3 wav ogg opus m4a flac')
put('video', 'mp4 webm mov mkv avi')
put('pdf', 'pdf')
put('markdown', 'md markdown mdx')
put('json', 'json')
put(
  'code',
  'py js mjs cjs ts tsx jsx go rs rb java kt swift c h cc cpp hpp cs php lua sh bash zsh fish ps1 sql css scss less html htm xml vue svelte toml yaml yml ini cfg conf env jsonl ndjson dockerfile makefile gradle tf proto graphql r pl ex exs erl hs ml clj scala dart zig nim',
)
put('text', 'txt log text rst org tex')
put('sheet', 'csv tsv xls xlsx ods')
put('archive', 'zip tar gz tgz bz2 xz 7z rar zst whl jar')
put(
  'data',
  'db sqlite sqlite3 db-wal db-shm wal shm lock pid sock bin pyc so dylib exe dll iso dmg woff woff2 ttf otf',
)

export function extOf(name: string): string {
  const n = name.toLowerCase()
  if (n === 'dockerfile' || n === 'makefile') return n
  const dot = n.lastIndexOf('.')
  return dot > 0 ? n.slice(dot + 1) : ''
}

export function kindOf(e: Pick<FileEntry, 'name' | 'is_directory' | 'mime_type'>): Kind {
  if (e.is_directory) return 'folder'
  const k = EXT[extOf(e.name)]
  if (k) return k
  const m = e.mime_type ?? ''
  if (m.startsWith('text/')) return 'text'
  if (m.startsWith('image/')) return 'image'
  return 'other'
}

/** Whether to ask Hermes for the text at all (it says when a file turns out binary). */
export const readsAsText = (k: Kind) =>
  k === 'markdown' || k === 'json' || k === 'code' || k === 'text' || k === 'sheet' || k === 'other'

export type SortKey = 'name' | 'modified' | 'size'
export interface Sort {
  key: SortKey
  desc: boolean
}

const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' })

/** Folders first, then by the chosen column; ties by name. */
export function sortEntries(list: FileEntry[], s: Sort): FileEntry[] {
  const dir = s.desc ? -1 : 1
  return [...list].sort((a, b) => {
    if (a.is_directory !== b.is_directory) return a.is_directory ? -1 : 1
    let c = 0
    if (s.key === 'modified') c = (a.mtime ?? 0) - (b.mtime ?? 0)
    else if (s.key === 'size') c = (a.size ?? -1) - (b.size ?? -1)
    if (c === 0) c = collator.compare(a.name, b.name)
    return c * dir
  })
}
