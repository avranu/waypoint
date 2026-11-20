// Picking an allow rule for a refused call (WP-233, WP-ADR114): WP-233's
// narrow suggestions, another rule typed, or (when approving a permission
// request) none. Used on a permission request and on refused calls in the
// log (WP-232).

import type { ReactNode } from 'react'
import clsx from 'clsx'
import { Check } from 'lucide-react'
import type { PermissionRule } from '@/lib/requests'
import { Input } from '@/components/ui/input'

export const ruleNone = ''
export const ruleOwn = '\u0000own'

/** The rule picked: a suggestion, the typed one, or undefined for none. */
export function pickedRule(value: string, own: string): string | undefined {
  return (value === ruleOwn ? own.trim() : value) || undefined
}

export function RuleChoice({
  rules,
  repo,
  tool,
  value,
  own,
  onChange,
  onOwn,
  none,
}: {
  rules: PermissionRule[]
  /** the repo whose .claude/settings.json gets the rule; without one, only "No rule" */
  repo?: string
  tool?: string
  value: string
  own: string
  onChange: (v: string) => void
  onOwn: (v: string) => void
  /** offer "No rule", with this hint */
  none?: string
}) {
  const opts: { v: string; title: ReactNode; hint: string }[] = rules.map((x) => ({
    v: x.rule,
    title: <code className="font-mono text-[12px] break-all">{x.rule}</code>,
    hint: `From now on: ${x.covers}.`,
  }))
  if (repo)
    opts.push({
      v: ruleOwn,
      title: 'Another rule',
      hint: 'A Bash, WebFetch or MCP allow rule for this tool.',
    })
  if (none !== undefined) opts.push({ v: ruleNone, title: 'No rule', hint: none })
  return (
    <div className="space-y-1.5">
      {repo && (
        <p className="text-[13px] text-muted">
          A rule is added to <code className="font-mono">{repo}</code>’s{' '}
          <code className="font-mono">.claude/settings.json</code> by a pull request that merges
          once its checks pass.
        </p>
      )}
      <div role="radiogroup" aria-label="Allow from now on" className="grid gap-1.5">
        {opts.map((o) => {
          const on = value === o.v
          return (
            <button
              key={o.v || 'none'}
              type="button"
              role="radio"
              aria-checked={on}
              onClick={() => onChange(o.v)}
              className={clsx(
                'flex items-start gap-2.5 rounded-lg border px-3 py-2 text-left',
                on ? 'border-accent/60 bg-accent-soft' : 'border-line bg-panel hover:bg-hover',
              )}
            >
              <span
                className={clsx(
                  'mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border',
                  on ? 'border-accent bg-accent text-accent-fg' : 'border-line-strong',
                )}
              >
                {on && <Check className="size-3" />}
              </span>
              <span className="min-w-0 text-[13px]">
                <span className="block font-medium">{o.title}</span>
                <span className="block text-muted">{o.hint}</span>
              </span>
            </button>
          )
        })}
      </div>
      {value === ruleOwn && (
        <Input
          aria-label="Allow rule"
          placeholder={tool === 'Bash' ? 'Bash(make lint *)' : `${tool}(…)`}
          value={own}
          onChange={(e) => onOwn(e.target.value)}
          className="font-mono text-[13px]"
        />
      )}
    </div>
  )
}
