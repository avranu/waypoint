// Delivery (WP-389): where recent work stands on its way to production.
// The server places each pull request (and each task an agent holds without
// one) at a stage and says what it waits on; see internal/core/delivery.go.

export type DeliveryStage =
  'working' | 'blocked' | 'review' | 'stuck' | 'merged' | 'live' | 'untracked'

export interface DeliveryEnv {
  id: number
  project: string
  name: string
  kind: string
  status: string
  repo: string
  branch: string
  commit: string
  commit_url?: string
  version: string
  deployed_at: string | null
  deploying: boolean
  /** listed merged pull requests it doesn't have yet */
  missing: number
  unknown: number
  behind: boolean
}

export interface DeliveryTask {
  id: number
  key: string
  title: string
  status: string
  category: string
}

export interface DeliveryPR {
  id: number
  repo: string
  number: number
  title: string
  url: string
  state: string
  ci?: string
  gate?: string
  armed?: boolean
  babysit?: string
  opened_at: string | null
  merged_at: string | null
}

export interface DeliveryRun {
  environment: number
  project: string
  name: string
  kind: string
  in: boolean
  unknown?: boolean
  since?: string
}

export interface DeliveryItem {
  stage: DeliveryStage
  detail: string
  tasks: DeliveryTask[]
  agent?: string
  pr?: DeliveryPR
  envs: DeliveryRun[]
  at: string
  queue_ahead?: number
}

export interface Delivery {
  environments: DeliveryEnv[]
  items: DeliveryItem[]
}

/** The stages people read, in the order the page shows them. */
export const STAGES: { stage: DeliveryStage; title: string; hint: string }[] = [
  {
    stage: 'stuck',
    title: 'Needs someone',
    hint: 'Its pull request is stuck until a person or agent acts.',
  },
  { stage: 'merged', title: 'Merged, not live yet', hint: 'Merged; waiting for a deploy.' },
  {
    stage: 'review',
    title: 'On its way',
    hint: 'Its pull request is moving: CI, review, the merge queue.',
  },
  {
    stage: 'working',
    title: 'Being worked on',
    hint: 'An agent holds the task.',
  },
  { stage: 'blocked', title: 'Blocked', hint: 'An open task blocks it.' },
  { stage: 'live', title: 'Live', hint: 'Deployed: you can try it.' },
  { stage: 'untracked', title: 'Merged, not tracked', hint: 'No environment deploys its branch.' },
]

/** The item for a task, if recent work includes it. */
export const itemForTask = (d: Delivery | undefined, key: string) =>
  d?.items.find((it) => it.tasks.some((t) => t.key === key))
