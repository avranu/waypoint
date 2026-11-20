// The audit log (WP-D15): what people decided about agents' work and about
// Waypoint itself, newest first, a page at a time.

import { infiniteQueryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { UserRef } from './types'

export type AuditCategory = 'approval' | 'delegation' | 'control' | 'settings' | 'access'

export interface AuditEntry {
  id: number
  at: string
  /** The person; null when a forge reported someone outside Waypoint. */
  user: UserRef | null
  /** A forge login, when a forge reported the decision. */
  actor?: string
  /** An agent acting through the person's token. */
  agent?: string
  /** The agent the decision is about (delegated to, whose work it approved). */
  about_agent?: string
  action: string
  category: AuditCategory
  project: { key: string; name: string } | null
  target_type?: string
  target_id: number | null
  target?: string
  /** An in-app path (/issue/WP-12) or a forge URL. */
  url?: string
  detail?: string
}

export interface AuditPage {
  entries: AuditEntry[]
  next_before?: number
}

export interface AuditFilters {
  project?: string
  user?: string
  category?: AuditCategory | ''
  agent?: string
  days?: number
}

const params = (f: AuditFilters) => ({
  project: f.project,
  user: f.user,
  category: f.category,
  agent: f.agent,
  days: f.days,
})

export const auditQuery = (f: AuditFilters) =>
  infiniteQueryOptions({
    queryKey: ['audit', params(f)],
    queryFn: ({ pageParam }) =>
      api.get<AuditPage>(
        `/audit${qs({ ...params(f), before: pageParam || undefined, limit: 50 })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) => last.next_before || undefined,
    staleTime: 30_000,
  })

/** The same entries as a spreadsheet (up to 10,000). */
export const auditCSVUrl = (f: AuditFilters) => `/api/v1/audit.csv${qs(params(f))}`
