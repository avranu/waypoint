import type { Attachment, IssueRef } from './types'
import type { DeliveryStage } from './delivery'

export interface CatchUpPR {
  repo: string
  number: number
  title: string
  url: string
  state: string
}

/** A page where the change shows: url is "" when a path has no environment to open it on. */
export interface TryLink {
  id: number
  url: string
  raw: string
}

export interface CatchUpItem {
  issue: IssueRef
  project: string
  status: string
  category: 'done' | 'canceled'
  /** whose work it is */
  agent: string
  resolved_at: string
  /** the agent's last comment, shortened */
  summary: string
  pr?: CatchUpPR
  /** review screenshots (wp shot) first, then what the agent looked at */
  shots: Attachment[]
  try: TryLink[]
  live?: DeliveryStage
  live_in?: string[]
}

export interface CatchUp {
  items: CatchUpItem[]
  total: number
}
