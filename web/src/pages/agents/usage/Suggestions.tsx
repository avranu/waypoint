import { Lightbulb, TriangleAlert } from 'lucide-react'
import type { UsageAdvice } from '@/lib/usage-analytics'
import { Section } from '@/components/ui/misc'
import { CopyButton } from './shared'

export function SuggestionsSection({ advice }: { advice: UsageAdvice[] }) {
  return (
    <Section
      title="Suggestions"
      helpId="usage:suggestions"
      help={
        <p>
          Computed by fixed rules from the numbers on this page, with no model involved: low cache
          hit rate in long sessions, tools whose output fills the context, premium models doing
          search-only subagent work, MCP servers and skills unused for 30 days (their descriptions
          cost context in every session), commands you are asked about or refused again and again,
          sessions that compacted many times, and a quota on pace to run out.
        </p>
      }
    >
      <ul className="divide-y divide-line">
        {advice.map((a) => (
          <li key={a.id} className="px-4 py-3">
            <div className="flex items-start gap-2">
              {a.level === 'warn' ? (
                <TriangleAlert
                  className="mt-0.5 size-4 shrink-0 text-warn"
                  aria-label="Worth acting on"
                />
              ) : (
                <Lightbulb className="mt-0.5 size-4 shrink-0 text-subtle" aria-label="Suggestion" />
              )}
              <div className="min-w-0 flex-1">
                <h4 className="text-[13px] font-semibold">{a.title}</h4>
                <ul className="mt-1 space-y-0.5 text-[13px] text-muted">
                  {a.evidence.map((e, i) => (
                    <li key={i} className="break-words">
                      {e}
                    </li>
                  ))}
                </ul>
                <p className="mt-1.5 text-[13px]">{a.action.text}</p>
                {a.action.snippet && (
                  <div className="mt-1.5">
                    <pre className="max-h-40 overflow-auto rounded-md bg-sunken px-2.5 py-2 font-mono text-xs">
                      {a.action.snippet}
                    </pre>
                    <CopyButton text={a.action.snippet} label="Copy allow rules" />
                  </div>
                )}
                <div className="mt-1">
                  <CopyButton text={requestText(a)} label="Copy as a request" />
                </div>
              </div>
            </div>
          </li>
        ))}
      </ul>
    </Section>
  )
}

/** A suggestion as text for a request to whoever can act on it. */
function requestText(a: UsageAdvice) {
  return [a.title, '', ...a.evidence.map((e) => `- ${e}`), '', a.action.text].join('\n')
}
