// The words for each kind of relation, one place for the Summary card, the
// Relations list and the picker (WP-1196, WP-1036).

import type { RelationKind } from './types'

export const relLabel: Record<RelationKind, string> = {
  relates: 'Related to',
  blocks: 'Blocks',
  blocked_by: 'Blocked by',
  duplicates: 'Duplicates',
  duplicated_by: 'Duplicated by',
  follows: 'Follows up on',
  followed_by: 'Followed up by',
  supersedes: 'Supersedes',
  superseded_by: 'Superseded by',
  tension: 'In tension with',
}
