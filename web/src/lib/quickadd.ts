// Quick-add syntax for typing an issue in one line:
//
//   Fix login crash !high @sam #frontend ~3 due:fri /bug
//
// !priority, @assignee, #tag, ~points, due:date (or ^date), /type,
// +agent (+any, +claude, +haiku), +humans, +anyone, cat:category.

import type { IssueInput } from './queries'
import { ISSUE_CATEGORIES } from './types'

const PRIO: Record<string, string> = {
  urgent: 'urgent',
  u: 'urgent',
  '1': 'urgent',
  p1: 'urgent',
  '!': 'urgent',
  high: 'high',
  h: 'high',
  '2': 'high',
  p2: 'high',
  medium: 'medium',
  med: 'medium',
  m: 'medium',
  '3': 'medium',
  p3: 'medium',
  low: 'low',
  l: 'low',
  '4': 'low',
  p4: 'low',
}
const TYPES = ['epic', 'story', 'task', 'bug', 'subtask']

export interface Parsed {
  input: Partial<IssueInput>
  title: string
  tokens: { kind: string; text: string }[]
}

export function parseQuickAdd(text: string, usernames: string[] = []): Parsed {
  const input: Partial<IssueInput> = {}
  const tokens: Parsed['tokens'] = []
  const words: string[] = []
  for (const w of text.split(/\s+/)) {
    if (!w) continue
    let m: RegExpMatchArray | null
    if ((m = w.match(/^!(\w+|!)$/)) && PRIO[m[1].toLowerCase()]) {
      input.priority = PRIO[m[1].toLowerCase()]
      tokens.push({ kind: 'priority', text: input.priority })
    } else if ((m = w.match(/^@([\w.-]+)$/))) {
      const name = m[1].toLowerCase()
      // An exact name, or a prefix that matches exactly one person (never a guess between two).
      const prefixed = usernames.filter((u) => u.toLowerCase().startsWith(name))
      const found =
        name === 'me'
          ? 'me'
          : (usernames.find((u) => u.toLowerCase() === name) ??
            (prefixed.length === 1 ? prefixed[0] : undefined))
      if (found) {
        input.assignees = [...(input.assignees ?? []), found]
        tokens.push({ kind: 'assignee', text: '@' + found })
      } else words.push(w)
    } else if ((m = w.match(/^#([\w.-]+)$/)) && !/^\d+$/.test(m[1])) {
      input.labels = [...(input.labels ?? []), m[1]]
      tokens.push({ kind: 'label', text: '#' + m[1] })
    } else if ((m = w.match(/^~(\d+(?:\.\d+)?)$/))) {
      input.points = parseFloat(m[1])
      tokens.push({ kind: 'points', text: m[1] + ' pts' })
    } else if ((m = w.match(/^(?:due:|\^)(.+)$/))) {
      input.due_date = m[1]
      tokens.push({ kind: 'due', text: 'due ' + m[1] })
    } else if ((m = w.match(/^\/(\w+)$/)) && TYPES.includes(m[1].toLowerCase())) {
      input.type = m[1].toLowerCase()
      tokens.push({ kind: 'type', text: input.type })
    } else if ((m = w.match(/^\+([\w.-]+)$/))) {
      // +agent: any agent; +humans: humans only; +anyone: the default; +claude…: a spec.
      const w2 = m[1].toLowerCase()
      input.agent =
        w2 === 'agent'
          ? 'any'
          : w2 === 'humans' || w2 === 'human'
            ? 'none'
            : w2 === 'anyone'
              ? ''
              : w2
      tokens.push({
        kind: 'agent',
        text:
          input.agent === 'any'
            ? 'any agent'
            : input.agent === 'none'
              ? 'humans only'
              : input.agent === ''
                ? 'anyone'
                : 'agent ' + input.agent,
      })
    } else if (
      (m = w.match(/^(?:cat|category):(\w+)$/i)) &&
      (ISSUE_CATEGORIES as readonly string[]).includes(m[1].toLowerCase())
    ) {
      input.category = m[1].toLowerCase()
      tokens.push({ kind: 'category', text: input.category })
    } else {
      words.push(w)
    }
  }
  const title = words.join(' ')
  return { input: { ...input, title }, title, tokens }
}

/** The server's title limit, in characters (maxTitleLen in internal/core/issues.go). */
export const TITLE_MAX = 500
/** Characters as the server counts them: code points, not UTF-16 units or bytes. */
export const utf8Len = (s: string) => [...s].length
export const titleTooLong = (s: string) => utf8Len(s.trim()) > TITLE_MAX

/**
 * Text too long for a title, typed where a title goes (WP-68): rather than
 * fail, its first sentence becomes the title (or its first words, when that
 * sentence is long too) and the whole text the description. This is the
 * fallback; the create is flagged title_is_text, and the server's title model
 * (WP-67) writes a short title when one is set up.
 */
export function splitLongTitle(text: string): { title: string; description?: string } {
  const t = text.trim()
  if (!titleTooLong(t)) return { title: t }
  const sentence = t.match(/^(.+?[.!?])(?:\s|$)/)?.[1]
  if (sentence && sentence.length <= 120) return { title: sentence, description: t }
  const head = t.slice(0, 80)
  const space = head.lastIndexOf(' ')
  return { title: (space > 40 ? head.slice(0, space) : head).trimEnd() + '…', description: t }
}
