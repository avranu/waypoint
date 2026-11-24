import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { Page } from './types'

export interface PageParams {
  project?: string // a key, or "none" for personal pages
  parent?: number // 0 for top level
  category?: string
  q?: string
  issue?: string
}

export const pagesQuery = (p: PageParams) =>
  queryOptions({
    queryKey: ['pages', p],
    queryFn: () => api.get<Page[]>(`/pages${qs({ ...p, limit: 500 })}`),
    staleTime: 30_000,
  })

export const pageQuery = (id: number) =>
  queryOptions({
    queryKey: ['page', id],
    queryFn: () => api.get<Page>(`/pages/${id}`),
  })

/** Arrange a flat page list into a tree by parent. */
export function pageTree(pages: Page[]) {
  const byParent = new Map<number | null, Page[]>()
  const ids = new Set(pages.map((p) => p.id))
  for (const p of pages) {
    const parent = p.parent_id && ids.has(p.parent_id) ? p.parent_id : null
    byParent.set(parent, [...(byParent.get(parent) ?? []), p])
  }
  return byParent
}
