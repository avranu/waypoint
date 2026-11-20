// A SKILL.md as it reads: its frontmatter as a line of facts and tags,
// links to the skills it names as related, then the instructions.

import { Link } from '@tanstack/react-router'
import { Badge } from '@/components/ui/misc'
import { Markdown } from '../Transcript'
import { splitFrontmatter } from './data'

export function SkillDoc({
  assistantId,
  content,
  known,
}: {
  assistantId: number
  content: string
  /** names of installed skills, to link related ones */
  known?: Set<string>
}) {
  const fm = splitFrontmatter(content)
  const facts = fm.meta.filter(([k]) => !['metadata'].includes(k))
  return (
    <div className="space-y-3">
      {(facts.length > 0 || fm.tags.length > 0) && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-xs text-muted">
          {facts.map(([k, v]) => (
            <span key={k}>
              <span className="text-subtle">{k.replace(/_/g, ' ')}</span> {v}
            </span>
          ))}
          {fm.tags.map((t) => (
            <Badge key={t}>{t}</Badge>
          ))}
        </div>
      )}
      {fm.related.length > 0 && (
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs">
          <span className="text-subtle">Related</span>
          {fm.related.map((r) =>
            known?.has(r) ? (
              <Link
                key={r}
                to="/assistant/$id"
                params={{ id: String(assistantId) }}
                search={{ tab: 'skills', path: r }}
                className="font-medium text-accent hover:underline"
              >
                {r}
              </Link>
            ) : (
              <span key={r} className="text-muted">
                {r}
              </span>
            ),
          )}
        </div>
      )}
      {fm.body.trim() ? (
        <Markdown text={fm.body} />
      ) : (
        <p className="text-[13px] text-subtle">No instructions yet.</p>
      )}
    </div>
  )
}
