import { useMutation, useQueryClient } from '@tanstack/react-query'
import { hermes, hermesQuery } from '@/lib/assistant'

export type Provenance = 'bundled' | 'hub' | 'agent' | 'external'

export interface Skill {
  name: string
  description: string
  category: string | null
  enabled: boolean
  /** times it was loaded or changed */
  usage: number
  provenance: Provenance
}

export interface SkillContent {
  name: string
  content: string
  path: string
}

export const skillsQuery = (id: number) => hermesQuery<Skill[]>(id, 'skills', undefined, 30_000)

export const skillContentQuery = (id: number, name: string) =>
  hermesQuery<SkillContent>(id, 'skills/content', { name })

export const PROVENANCE: Record<Provenance, { label: string; color?: string; hint: string }> = {
  agent: {
    label: 'Agent-written',
    color: 'var(--wp-accent)',
    hint: 'Written on this Hermes, by the agent or by hand',
  },
  hub: { label: 'Hub', color: 'var(--wp-ok)', hint: 'Installed from the skills hub' },
  external: { label: 'External', color: 'var(--wp-warn)', hint: 'From a folder outside Hermes' },
  bundled: { label: 'Built-in', hint: 'Comes with Hermes' },
}

const WORDS: Record<string, string> = {
  ai: 'AI',
  mcp: 'MCP',
  api: 'API',
  ui: 'UI',
  github: 'GitHub',
}

/** "autonomous-ai-agents" → "Autonomous AI agents". */
export function categoryLabel(c: string | null | undefined): string {
  if (!c) return 'Uncategorized'
  const s = c
    .split(/[-_\s]+/)
    .map((w) => WORDS[w.toLowerCase()] ?? w)
    .join(' ')
  return s.charAt(0).toUpperCase() + s.slice(1)
}

/** Hermes's rule for skill and category names. */
export const NAME_RE = /^[a-z0-9][a-z0-9._-]*$/
/** A new skill's description must fit the skill index Hermes puts in its prompt. */
export const DESCRIPTION_LIMIT = 60

export interface Frontmatter {
  /** top-level `key: value` lines other than name and description */
  meta: [string, string][]
  name?: string
  description?: string
  tags: string[]
  related: string[]
  body: string
}

const unquote = (v: string) => v.trim().replace(/^(['"])(.*)\1$/, '$2')
const list = (v: string | undefined) =>
  v
    ? v
        .replace(/^\[|\]$/g, '')
        .split(',')
        .map(unquote)
        .filter(Boolean)
    : []

/** SKILL.md's frontmatter, read just well enough to show it. */
export function splitFrontmatter(src: string): Frontmatter {
  const text = src.replace(/^﻿/, '')
  const out: Frontmatter = { meta: [], tags: [], related: [], body: text }
  if (!text.startsWith('---')) return out
  const end = /\n---[ \t]*(\n|$)/.exec(text.slice(3))
  if (!end) return out
  const fm = text.slice(3, end.index + 3)
  out.body = text.slice(end.index + 3 + end[0].length)
  for (const line of fm.split('\n')) {
    const top = /^([A-Za-z_][\w-]*):[ \t]*(.*)$/.exec(line)
    if (top) {
      const [, k, v] = top
      if (k === 'name') out.name = unquote(v)
      else if (k === 'description') out.description = unquote(v)
      else if (v.trim() && !v.trim().startsWith('|') && !v.trim().startsWith('>'))
        out.meta.push([
          k,
          unquote(v)
            .replace(/^\[|\]$/g, '')
            .replace(/,\s*/g, ', '),
        ])
      continue
    }
    const nested = /^\s+(tags|related_skills):[ \t]*(\[.*\])\s*$/.exec(line)
    if (nested) out[nested[1] === 'tags' ? 'tags' : 'related'] = list(nested[2])
  }
  return out
}

/** A new SKILL.md: frontmatter from the form, then the instructions. */
export function composeSkill(f: {
  name: string
  description: string
  author: string
  body: string
}) {
  return [
    '---',
    `name: ${f.name}`,
    `description: ${JSON.stringify(f.description.trim())}`,
    'version: 1.0.0',
    `author: ${JSON.stringify(f.author)}`,
    '---',
    '',
    f.body.trim(),
    '',
  ].join('\n')
}

export const BODY_TEMPLATE = `## When to use

-

## Steps

1.
`

export function useSkillMutations(id: number) {
  const qc = useQueryClient()
  const key = skillsQuery(id).queryKey
  return {
    /** Turns a skill on or off at once, and back if Hermes refuses. */
    toggle: useMutation({
      mutationFn: (b: { name: string; enabled: boolean }) => hermes(id, 'PUT', 'skills/toggle', b),
      onMutate: (b) => {
        void qc.cancelQueries({ queryKey: key })
        const before = qc.getQueryData<Skill[]>(key)
        qc.setQueryData<Skill[]>(key, (l) =>
          l?.map((s) => (s.name === b.name ? { ...s, enabled: b.enabled } : s)),
        )
        return { before }
      },
      onError: (_e, _b, ctx) => ctx?.before && qc.setQueryData(key, ctx.before),
      onSettled: () => qc.invalidateQueries({ queryKey: key }),
    }),
    save: useMutation({
      mutationFn: (b: { name: string; content: string }) => hermes(id, 'PUT', 'skills/content', b),
      onSuccess: (_r, b) => {
        qc.setQueryData<SkillContent>(skillContentQuery(id, b.name).queryKey, (c) =>
          c ? { ...c, content: b.content } : c,
        )
        qc.invalidateQueries({ queryKey: key })
      },
    }),
    create: useMutation({
      mutationFn: (b: { name: string; category?: string; content: string }) =>
        hermes<{ path: string }>(id, 'POST', 'skills', b),
      onSuccess: () => qc.invalidateQueries({ queryKey: key }),
    }),
    /** Moves a skill written on this Hermes to its archive (`hermes curator restore` brings it back). */
    archive: useMutation({
      mutationFn: (name: string) => hermes(id, 'DELETE', 'learning/node', { id: name }),
      onSuccess: () => qc.invalidateQueries({ queryKey: key }),
    }),
  }
}
