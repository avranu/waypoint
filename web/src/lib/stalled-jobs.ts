// A project's own stall policy, set by its owners, overrides the admins' policy for every project.

import { api, enc } from './api'
import type { StallPolicy } from './types'

export type { StallPolicy }

export interface StallPolicies {
  policy: StallPolicy
  default: StallPolicy
  /** set where asked: the project's own, or the admins' for every project */
  custom: boolean
  /** a project's only: what applies without its own */
  inherited?: StallPolicy
}

/** Every project's; null goes back to the default. */
export const saveStallPolicy = (policy: StallPolicy | null) =>
  api.put<StallPolicies>('/admin/stalled-jobs', { policy })

/** A project's own; null uses every project's. */
export const saveProjectStallPolicy = (key: string, policy: StallPolicy | null) =>
  api.patch(`/projects/${enc(key)}`, { settings: { stalled_jobs: policy } })

export const STALL_FLOOR = 5
export const STALL_MAX = 24 * 60

/** Why the times can't be saved, as the server checks them; undefined: they can. */
export function stallPolicyError(p: StallPolicy): string | undefined {
  const inRange = (v: number, lo: number) => Number.isInteger(v) && v >= lo && v <= STALL_MAX
  if (!inRange(p.min_age, STALL_FLOOR) || !inRange(p.quiet, STALL_FLOOR))
    return `Ages and stall times are ${STALL_FLOOR}–${STALL_MAX} minutes.`
  if (!inRange(p.skip_timeout, 0)) return `The timeout is 0–${STALL_MAX} minutes.`
  if (p.early !== 0 && (!inRange(p.early, STALL_FLOOR) || p.early > p.quiet))
    return `Telling early needs ${STALL_FLOOR} minutes at least and no more than the stall time; 0 turns it off.`
  if (p.cold_wake !== 0 && !inRange(p.cold_wake, p.quiet))
    return 'Waking a cold session needs at least the stall time; 0 turns it off.'
  return undefined
}

export const samePolicy = (a: StallPolicy, b: StallPolicy) =>
  a.off === b.off &&
  a.min_age === b.min_age &&
  a.quiet === b.quiet &&
  a.skip_timeout === b.skip_timeout &&
  a.early === b.early &&
  a.cold_wake === b.cold_wake &&
  a.fixed === b.fixed
