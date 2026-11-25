import { useQuery } from '@tanstack/react-query'
import { aiq, usePinReviewArm, type ReviewArm, type ReviewArms } from '@/lib/ai'
import { relativeTime } from '@/lib/format'
import { Select } from '@/components/ui/input'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'

const STATE: Record<ReviewArm['state'], { label: string; color?: string }> = {
  in_use: { label: 'In use', color: 'var(--wp-ok)' },
  exploring: { label: 'Exploring', color: 'var(--wp-accent)' },
  benched: { label: 'Benched', color: 'var(--wp-warn)' },
  pinned: { label: 'Pinned' },
  unavailable: { label: 'Unavailable' },
}

const pct = (f: number) => `${Math.round(100 * f)}%`
const num = (f: number) => (Number.isInteger(f) ? String(f) : f.toFixed(1))

export function ReviewArmsSection() {
  const q = useQuery(aiq.reviewArms())
  const d = q.data
  return (
    <Section
      title="Review arms"
      help={
        <>
          A review runs one way of several: the diff alone or with the changed files, with the
          verifier checking its findings or not. Each review draws one by how many of its findings
          held up (fixed, or taken by an agent) against how many were dismissed, for the tokens it
          spends. Findings count half after 30 days and from the previous prompt, and not after 90
          days. An arm explores until it has {d?.explore_under ?? 15} judged findings; one whose
          findings hold up under {pct(d?.bench_bound ?? 0.2)} of the time is benched, and still
          drawn on 1 review in {d?.benched_odds ?? 20}.
          {d?.gateway && ' The LLM gateway picks the model.'}
        </>
      }
      className="mt-4"
    >
      {q.isPending ? (
        <div className="p-4">
          <Skeleton className="h-24" />
        </div>
      ) : !d ? (
        <p className="px-4 py-3 text-xs text-subtle">The review arms couldn't be read.</p>
      ) : (
        <div data-testid="review-arms">
          <ul className="divide-y divide-line">
            {d.arms.map((a) => (
              <ArmRow key={a.key} arm={a} />
            ))}
          </ul>
          <Budget budget={d.budget} />
          {d.recall.length > 0 && <Recall recall={d.recall} />}
        </div>
      )}
    </Section>
  )
}

function ArmRow({ arm: a }: { arm: ReviewArm }) {
  const pin = usePinReviewArm()
  const st = STATE[a.state]
  const judged = a.useful + a.false_positives
  return (
    <li className="px-4 py-2.5" data-arm={a.key} data-state={a.state}>
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
        <span className="text-[13px] font-medium">{a.label}</span>
        <Badge color={st.color}>
          {a.state === 'pinned' ? (a.pin === 'off' ? 'Pinned off' : 'Pinned on') : st.label}
        </Badge>
        <div className="ml-auto">
          <Select
            variant="inline"
            aria-label={`Pin ${a.label}`}
            value={a.pin ?? ''}
            disabled={pin.isPending || a.state === 'unavailable'}
            onChange={(e) =>
              pin.mutate(
                { arm: a.key, pin: e.target.value as '' | 'on' | 'off' },
                { onError: toastError },
              )
            }
          >
            <option value="">Drawn</option>
            <option value="on">Pinned on</option>
            <option value="off">Pinned off</option>
          </Select>
        </div>
      </div>
      <p className="mt-0.5 text-xs text-muted">{a.why}</p>
      <div className="mt-1 flex flex-wrap gap-x-3 gap-y-0.5 text-xs text-subtle">
        <span>
          {num(a.useful)} held up · {num(a.false_positives)} dismissed
          {judged > 0 && ` · about ${pct(a.precision)} (at most ${pct(a.upper)})`}
        </span>
        <span>
          {a.runs} {a.runs === 1 ? 'review' : 'reviews'} in 30 days
          {a.last_run && `, last ${relativeTime(a.last_run)}`}
        </span>
        {a.tokens > 0 && (
          <span>
            {Math.round(a.tokens).toLocaleString()} tokens a review
            {a.cost > 1.005 && ` (${a.cost.toFixed(2)}×)`}
          </span>
        )}
      </div>
      {a.models.length > 0 && (
        <ul className="mt-1 space-y-0.5">
          {a.models.map((m) => (
            <li key={m.model} className="flex items-center gap-3 text-[11px] text-subtle">
              <span className="min-w-0 flex-1 truncate font-mono">{m.model}</span>
              <span className="shrink-0">
                {m.useful} held up · {m.false_positives} dismissed
              </span>
            </li>
          ))}
        </ul>
      )}
    </li>
  )
}

function Budget({ budget: b }: { budget: ReviewArms['budget'] }) {
  return (
    <p className="border-t border-line px-4 py-2.5 text-xs text-muted">
      With the changed files, a review adds up to {b.chars.toLocaleString()} characters of their
      text
      {b.model && b.context_length ? (
        <>
          , sized to <span className="font-mono">{b.model}</span>, the model that answered the last
          review ({b.context_length.toLocaleString()} tokens of context).
        </>
      ) : (
        <>; the last review model's context size isn't known, so the set amount applies.</>
      )}
    </p>
  )
}

function Recall({ recall }: { recall: ReviewArms['recall'] }) {
  return (
    <div className="border-t border-line px-4 py-2.5">
      <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
        With a second reviewer, past 90 days
      </div>
      <ul className="space-y-1">
        {recall.map((r) => (
          <li key={r.model} className="flex flex-wrap items-center gap-x-3 text-xs">
            <span className="min-w-0 flex-1 truncate font-mono">{r.model}</span>
            <span className="text-muted">
              {r.runs} {r.runs === 1 ? 'review' : 'reviews'} · found {r.alone} alone ({r.alone_held}{' '}
              held up) · missed {r.missed_held} that held up
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}
