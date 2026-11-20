// Admin → AI → Review stats (WP-371): how the review bot's findings held up,
// per reviewer model × strategy × prompt version and per verifier model, with
// the verifier on against off. wp pr review-stats prints the same.

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  aiq,
  type ReviewArmClass,
  type ReviewerStat,
  type ReviewJudged,
  type VerifierStat,
} from '@/lib/ai'
import { Select } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'

const pct = (f: number) => `${Math.round(100 * f)}%`
const n = (v: number) => v.toLocaleString()
const count = (v: number, one: string) => `${n(v)} ${one}${v === 1 ? '' : 's'}`

function Precision({ j }: { j: ReviewJudged }) {
  if (j.precision == null || j.low == null || j.high == null) return null
  return (
    <span className="font-medium text-fg">
      {pct(j.precision)}{' '}
      <span className="font-normal text-subtle">
        ({pct(j.low)}–{pct(j.high)})
      </span>
    </span>
  )
}

function Counts({ j }: { j: ReviewJudged }) {
  return (
    <span>
      {j.useful} useful · {j.false} false · {j.unjudged} unjudged
    </span>
  )
}

function Providers({ ps }: { ps?: string[] }) {
  if (!ps?.length) return null
  return <span className="text-subtle"> ({ps.join(', ')})</span>
}

export function ReviewStatsSection() {
  const [days, setDays] = useState(30)
  const q = useQuery(aiq.reviewStats(days))
  const d = q.data
  return (
    <Section
      title="Review stats"
      help={
        <>
          How the review bot's findings held up, by the model that raised them, the context strategy
          and the prompt version, and by the verifier that checked them. Useful: fixed, or taken by
          an agent; false: dismissed or withdrawn; a leftover audit's verdict counts once given.
          Precision is useful over useful and false, with its 90% interval. A drop is right when the
          dropped finding, posted anyway, turned out false.
        </>
      }
      actions={
        <Select
          variant="inline"
          aria-label="Review stats window"
          value={String(days)}
          onChange={(e) => setDays(Number(e.target.value))}
        >
          <option value="30">30 days</option>
          <option value="90">90 days</option>
          <option value="365">A year</option>
        </Select>
      }
      className="mt-4"
    >
      {q.isPending ? (
        <div className="p-4">
          <Skeleton className="h-24" />
        </div>
      ) : !d ? (
        <p className="px-4 py-3 text-xs text-subtle">The review stats couldn't be read.</p>
      ) : d.totals.findings === 0 ? (
        <p className="px-4 py-3 text-xs text-subtle" data-testid="review-stats">
          No review findings in the last {d.days} days.
        </p>
      ) : (
        <div data-testid="review-stats">
          <p className="flex flex-wrap gap-x-3 gap-y-0.5 px-4 py-2.5 text-xs text-muted">
            <span>
              {count(d.totals.findings, 'finding')} from {count(d.totals.runs, 'review')}
            </span>
            <Counts j={d.totals} />
            <Precision j={d.totals} />
            <span>
              {d.totals.evidence_dropped} dropped by the evidence check · {d.totals.withheld}{' '}
              withheld by the verifier
            </span>
          </p>
          <Heading>Reviewers</Heading>
          <ul className="divide-y divide-line">
            {d.reviewers.map((r) => (
              <ReviewerRow key={`${r.model}|${r.strategy}|${r.prompt_version}`} r={r} />
            ))}
          </ul>
          {d.verifiers.length > 0 && (
            <>
              <Heading>Verifiers</Heading>
              <ul className="divide-y divide-line">
                {d.verifiers.map((v) => (
                  <VerifierRow key={v.model} v={v} />
                ))}
              </ul>
            </>
          )}
          <Heading>Verifier on against off</Heading>
          <ul className="divide-y divide-line">
            <ClassRow name="Kept by the verifier" c={d.verify.verified} />
            <ClassRow name="No verifier" c={d.verify.skipped} />
          </ul>
        </div>
      )}
    </Section>
  )
}

function Heading({ children }: { children: string }) {
  return (
    <div className="border-t border-line px-4 pb-1 pt-2.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
      {children}
    </div>
  )
}

function ReviewerRow({ r }: { r: ReviewerStat }) {
  return (
    <li className="px-4 py-2" data-reviewer={r.model}>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
        <span className="min-w-0 break-all font-mono text-[12px]">{r.model || '(no model)'}</span>
        <Providers ps={r.providers} />
        <span className="text-subtle">
          {r.strategy || '–'} · {r.prompt_version || '–'}
        </span>
        <span className="ml-auto">
          <Precision j={r} />
        </span>
      </div>
      <div className="mt-0.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted">
        <span>{count(r.findings, 'finding')}</span>
        <Counts j={r} />
        {(r.evidence_dropped > 0 || r.withheld > 0) && (
          <span>
            dropped {r.evidence_dropped} evidence · {r.withheld} verifier
          </span>
        )}
        <span>
          {r.runs} {r.runs === 1 ? 'review' : 'reviews'} · {n(r.tokens)} tokens
          {r.tokens_per_useful > 0 && `, ${n(r.tokens_per_useful)} per useful`}
        </span>
      </div>
    </li>
  )
}

function VerifierRow({ v }: { v: VerifierStat }) {
  const posted = v.posted.useful + v.posted.false + v.posted.unjudged
  return (
    <li className="px-4 py-2" data-verifier={v.model}>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs">
        <span className="min-w-0 break-all font-mono text-[12px]">{v.model}</span>
        <Providers ps={v.providers} />
        <span className="ml-auto text-subtle">
          {v.runs} {v.runs === 1 ? 'review' : 'reviews'} · {n(v.tokens)} tokens
        </span>
      </div>
      <div className="mt-0.5 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-muted">
        <span>
          Kept: <Counts j={v.kept} />
          {v.kept.precision != null && ' · '}
          <Precision j={v.kept} />
        </span>
        <span>
          Dropped {v.dropped}, {posted} posted anyway: <Counts j={v.posted} />
          {v.drop_accuracy != null && v.drop_low != null && v.drop_high != null && (
            <>
              {' '}
              · drops right{' '}
              <span className="font-medium text-fg">
                {pct(v.drop_accuracy)}{' '}
                <span className="font-normal text-subtle">
                  ({pct(v.drop_low)}–{pct(v.drop_high)})
                </span>
              </span>
            </>
          )}
        </span>
      </div>
    </li>
  )
}

function ClassRow({ name, c }: { name: string; c: ReviewArmClass }) {
  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 px-4 py-2 text-xs text-muted">
      <span className="font-medium text-fg">{name}</span>
      <span>
        {count(c.findings, 'finding')} from {count(c.runs, 'review')}
      </span>
      <Counts j={c} />
      <Precision j={c} />
      {c.tokens > 0 && (
        <span>
          {n(c.tokens)} tokens{c.tokens_per_useful > 0 && `, ${n(c.tokens_per_useful)} per useful`}
        </span>
      )}
    </li>
  )
}
