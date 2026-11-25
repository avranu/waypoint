import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { KItem, UserRef } from './types'

export type UsedType = 'glossary' | 'page' | 'design'
export type UseKind = 'lookup' | 'mention' | 'avoided'

export interface KnowledgeUse {
  kind: UseKind
  at: string
  user?: UserRef
  agent?: string
  /** what named it: the term or alias, the word to avoid, the page code */
  word?: string
  /** where the text is (none for lookups) */
  source?: KItem
  /** the text was a comment on the source issue */
  comment?: boolean
  /** overviews: the item used */
  item?: KItem
}

export interface ItemUsage {
  counts: { lookups: number; mentions: number; avoided: number; people: number; agents: number }
  last_used_at: string | null
  uses: KnowledgeUse[]
}

export interface UsedItem {
  item: KItem
  lookups: number
  mentions: number
  avoided: number
  last_used_at: string
}

export interface AvoidedWord {
  entry: KItem
  word: string
  count: number
  latest: KnowledgeUse
}

export interface KnowledgeOverview {
  totals: { lookups: number; mentions: number; avoided: number; items_used: number }
  top: UsedItem[]
  avoided: AvoidedWord[]
  lookups: KnowledgeUse[]
  unused: { item: KItem; last_used_at: string | null }[]
  unused_total: number
}

export const itemUsageQuery = (type: UsedType, id: number) =>
  queryOptions({
    queryKey: ['usage', type, id],
    queryFn: () => api.get<ItemUsage>(`/knowledge/usage${qs({ type, id })}`),
    staleTime: 30_000,
  })

export const knowledgeOverviewQuery = (f: { agent: string; project: string; days: number }) =>
  queryOptions({
    queryKey: ['usage', 'overview', f],
    queryFn: () =>
      api.get<KnowledgeOverview>(
        `/agents/knowledge${qs({ agent: f.agent || undefined, project: f.project || undefined, days: f.days })}`,
      ),
    staleTime: 30_000,
  })
