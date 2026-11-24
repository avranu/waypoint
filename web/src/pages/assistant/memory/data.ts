// Entry edits and deletes go through Hermes's own lock.

import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { ApiError, enc } from '@/lib/api'
import { hermes, hermesQuery, hget } from '@/lib/assistant'

/** Hermes's built-in memory files; `user` is USER.md ("profile" in its graph). */
export type MemoryTarget = 'memory' | 'user'

export const MEMORY_FILES: Record<MemoryTarget, { file: string; source: string }> = {
  memory: { file: 'MEMORY.md', source: 'memory' },
  user: { file: 'USER.md', source: 'profile' },
}

/** Hermes separates the entries of a memory file with a line holding only §. */
export const ENTRY_DELIMITER = '\n§\n'

/** A memory file's entries, the way Hermes's memory store reads them. */
export function parseEntries(raw: string): string[] {
  return raw
    .replace(/^﻿/, '')
    .split(ENTRY_DELIMITER)
    .map((e) => e.trim())
    .filter(Boolean)
}

/** Characters as Hermes counts them against the file's limit (code points). */
export const memoryChars = (entries: string[]) => [...entries.join(ENTRY_DELIMITER)].length

export interface MemoryProvider {
  name: string
  description: string
  available: boolean
  configured: boolean
  status: 'ready' | 'needs_config' | 'unavailable' | 'missing'
}

export interface MemoryStatus {
  /** "" is the built-in memory alone. */
  active: string
  providers: MemoryProvider[]
  builtin_files: { memory: number; user: number }
}

export interface MemoryConfig {
  memory_enabled?: boolean
  user_profile_enabled?: boolean
  memory_char_limit?: number
  user_char_limit?: number
}

export interface LearningNode {
  id: string
  label: string
  kind: 'skill' | 'memory'
  memorySource?: string
  timestamp: number | null
}

export interface MemoryCard {
  source: string
  timestamp: number | null
  title: string
  /** the entry, cut at 1200 characters */
  body: string
  fingerprint: string
}

export interface LearningGraph {
  nodes: LearningNode[]
  memory: MemoryCard[]
}

export interface TextFile {
  text: string
  /** the file isn't there (never written, or cleared) */
  missing: boolean
}

export const memoryStatusQuery = (id: number) => hermesQuery<MemoryStatus>(id, 'memory')

export const configQuery = (id: number) =>
  hermesQuery<{ memory?: MemoryConfig } & Record<string, unknown>>(id, 'config')

/** HERMES_HOME: the folder of Hermes's config.yaml. */
export const homeQuery = (id: number) =>
  queryOptions({
    ...hermesQuery<{ path: string; yaml: string }>(id, 'config/raw', undefined, 10 * 60_000),
    select: (r: { path: string }) => r.path.replace(/\/[^/]*$/, '') || '/',
  })

export const currentProfileQuery = (id: number) =>
  queryOptions({
    ...hermesQuery<{ active: string; current: string }>(id, 'profiles/active', undefined, 60_000),
    select: (r: { current: string }) => r.current || 'default',
  })

export const soulQuery = (id: number, profile: string) =>
  hermesQuery<{ content: string; exists: boolean }>(id, `profiles/${enc(profile)}/soul`)

export const graphQuery = (id: number) => hermesQuery<LearningGraph>(id, 'learning/graph')

/** A text file through Hermes's fs API; a missing file reads as empty. */
export const textFileQuery = (id: number, path: string) =>
  queryOptions({
    queryKey: ['assistant', id, 'fs/read-text', { path }],
    queryFn: () => readText(id, path),
    staleTime: 10_000,
  })

async function readText(id: number, path: string): Promise<TextFile> {
  try {
    const r = await hget<{ text: string; binary: boolean }>(id, 'fs/read-text', { path })
    return { text: r.binary ? '' : r.text, missing: false }
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return { text: '', missing: true }
    throw e
  }
}

export const memoryPath = (home: string, t: MemoryTarget) =>
  `${home}/memories/${MEMORY_FILES[t].file}`

/**
 * The learning-graph id of each entry, or null where the graph and the file
 * disagree (the file changed since the graph was drawn).
 */
export function entryIds(graph: LearningGraph | undefined, t: MemoryTarget, entries: string[]) {
  const source = MEMORY_FILES[t].source
  const nodes = graph?.nodes.filter((n) => n.kind === 'memory' && n.memorySource === source) ?? []
  const cards = graph?.memory.filter((c) => c.source === source) ?? []
  return entries.map((e, i) =>
    nodes[i] && cards[i]?.body && e.startsWith(cards[i].body) ? nodes[i].id : null,
  )
}

/** When a memory file last changed, from the graph (epoch seconds). */
export function fileUpdated(graph: LearningGraph | undefined, t: MemoryTarget) {
  const c = graph?.memory.find((m) => m.source === MEMORY_FILES[t].source)
  return c?.timestamp ?? null
}

export function useMemoryMutations(id: number) {
  const qc = useQueryClient()
  const refresh = () =>
    Promise.all(
      ['fs/read-text', 'learning/graph', 'memory'].map((p) =>
        qc.invalidateQueries({ queryKey: ['assistant', id, p] }),
      ),
    )
  return {
    /** Writes a whole file, making its folder if it isn't there yet. */
    write: useMutation({
      mutationFn: async ({ path, content }: { path: string; content: string }) => {
        try {
          return await hermes(id, 'POST', 'fs/write-text', { path, content })
        } catch (e) {
          if (!(e instanceof ApiError) || !/parent directory/i.test(e.message)) throw e
          await hermes(id, 'POST', 'files/mkdir', { path: path.replace(/\/[^/]*$/, '') })
          return hermes(id, 'POST', 'fs/write-text', { path, content })
        }
      },
      onSuccess: refresh,
    }),
    /** Fetches a file as it is now (to append to it, or to notice it changed). */
    fresh: (path: string) =>
      qc.fetchQuery({ ...textFileQuery(id, path), staleTime: 0 }).then((f) => f.text),
    editEntry: useMutation({
      mutationFn: (b: { id: string; content: string }) => hermes(id, 'PUT', 'learning/node', b),
      onSuccess: refresh,
    }),
    deleteEntry: useMutation({
      mutationFn: (node: string) => hermes(id, 'DELETE', 'learning/node', { id: node }),
      onSuccess: refresh,
    }),
    reset: useMutation({
      mutationFn: ({ target, profile }: { target: MemoryTarget; profile: string }) =>
        hermes(id, 'POST', 'memory/reset', { target }, { profile }),
      onSuccess: refresh,
    }),
    saveSoul: useMutation({
      mutationFn: ({ profile, content }: { profile: string; content: string }) =>
        hermes(id, 'PUT', `profiles/${enc(profile)}/soul`, { content }),
      onSuccess: (_, v) =>
        qc.setQueryData(soulQuery(id, v.profile).queryKey, { content: v.content, exists: true }),
    }),
  }
}
