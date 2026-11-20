// The assistant page's URL state (WP-ADR1), apart from the page so the
// router can validate it without loading it.

export type AssistantTab =
  'chat' | 'activity' | 'schedules' | 'skills' | 'memory' | 'tools' | 'files' | 'usage' | 'settings'

export const ASSISTANT_TABS: AssistantTab[] = [
  'chat',
  'activity',
  'schedules',
  'skills',
  'memory',
  'tools',
  'files',
  'usage',
  'settings',
]

export interface AssistantSearch {
  tab?: AssistantTab
  /** Chat: the stored session shown (absent: a new conversation). */
  s?: string
  /** Chat: text to start a new conversation with (from an issue, the palette). */
  ask?: string
  /** Chat: the issue a new conversation is about. */
  issue?: string
  /** Chat: send `ask` at once rather than leaving it in the box. */
  send?: '1'
  /** Files: the folder or file shown. */
  path?: string
  /** A tab's own sub-view (activity: timeline, approvals, audit, logs). */
  view?: string
}

const str = (v: unknown) => (typeof v === 'string' && v ? v : undefined)

export function validateAssistantSearch(s: Record<string, unknown>): AssistantSearch {
  return {
    tab: ASSISTANT_TABS.includes(s.tab as AssistantTab) ? (s.tab as AssistantTab) : undefined,
    s: str(s.s),
    ask: str(s.ask),
    issue: str(s.issue),
    send: s.send === '1' || s.send === 1 ? '1' : undefined,
    path: str(s.path),
    view: str(s.view),
  }
}
