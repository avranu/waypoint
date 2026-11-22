// Usage → Tokens: what the sessions processed, by day and model; how much of
// the input came from the cache; and what that would cost on the API.

import { useMemo } from 'react'
import { DataTable, Legend, StackedAreaChart, viz } from '@/components/charts'
import { Section } from '@/components/ui/misc'
import { shortDate } from '@/lib/format'
import { usd } from '@/lib/activity'
import {
  FAMILIES,
  FAMILY_COLOR,
  fresh,
  modelFamily,
  pct,
  tok,
  type UsageDay,
  type UsageOverview,
} from '@/lib/usage-analytics'
import { Tile } from './shared'

/** Every day from the first to the last with data, zeros filled in. */
function dayRange(days: UsageDay[]): string[] {
  if (days.length === 0) return []
  const dates = days.map((d) => d.date).sort()
  const out: string[] = []
  const d = new Date(dates[0] + 'T00:00:00')
  const end = new Date(dates[dates.length - 1] + 'T00:00:00')
  for (let guard = 0; d <= end && guard < 400; guard++) {
    out.push(
      `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`,
    )
    d.setDate(d.getDate() + 1)
  }
  return out
}

export function TokensSection({ o }: { o: UsageOverview }) {
  const t = o.totals
  const dates = useMemo(() => dayRange(o.daily), [o.daily])
  const perFamily = useMemo(() => {
    const out: Record<string, number[]> = {}
    for (const f of FAMILIES) out[f] = dates.map(() => 0)
    for (const d of o.daily) {
      const i = dates.indexOf(d.date)
      if (i >= 0) out[modelFamily(d.model)][i] += fresh(d)
    }
    return out
  }, [o.daily, dates])
  const parts = useMemo(() => {
    const uncached = dates.map(() => 0)
    const writes = dates.map(() => 0)
    const reads = dates.map(() => 0)
    const cost = dates.map(() => 0)
    for (const d of o.daily) {
      const i = dates.indexOf(d.date)
      if (i < 0) continue
      uncached[i] += d.input
      writes[i] += d.cache_write
      reads[i] += d.cache_read
      cost[i] += d.cost_usd
    }
    return { uncached, writes, reads, cost }
  }, [o.daily, dates])
  const labels = dates.map((d) => shortDate(d))
  const families = FAMILIES.filter((f) => perFamily[f].some((v) => v > 0))
  const totalInput = t.input + t.cache_write + t.cache_read
  return (
    <div className="space-y-4">
      <dl className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        <Tile
          label="Fresh tokens"
          value={tok(fresh(t))}
          sub={`${tok(t.input)} in · ${tok(t.output)} out · ${tok(t.cache_write)} cached`}
          title="Uncached input, output and tokens written to the cache. Cache re-reads are counted apart."
        />
        <Tile
          label="Cache hit rate"
          value={pct(t.hit_rate)}
          sub={`${tok(t.cache_read)} of ${tok(totalInput)} input read from cache`}
          title="Input tokens served from the prompt cache, of all input"
        />
        <Tile
          label="API messages"
          title="Model responses (one per API call that carried usage), not turns: a single prompt can make dozens"
          value={tok(t.messages)}
          sub={`${o.sessions} session${o.sessions === 1 ? '' : 's'}`}
        />
        <Tile
          label="API-equivalent cost"
          value={usd(t.cost_usd)}
          sub={
            t.unpriced_tokens ? `${tok(t.unpriced_tokens)} tokens have no price` : 'at list prices'
          }
          title="What these tokens would cost on the API. A subscription isn't billed per token; use it to compare models and sessions."
        />
      </dl>

      <Section
        title="Tokens by day"
        helpId="usage:tokens"
        help={
          <p>
            Fresh tokens per day, stacked by model family: uncached input, output and cache writes.
            Cache re-reads aren’t in this chart, since every message re-reads its whole context from
            the cache and they would bury everything else; they are shown below. Costs are what the
            tokens would cost on the API at list prices
            {o.prices_edit ? ' (Admin → AI → Prices)' : ''}, not what a subscription bills.
          </p>
        }
      >
        <div className="space-y-3 p-4">
          {dates.length > 0 && families.length > 0 ? (
            <>
              <Legend items={families.map((f) => ({ name: f, color: FAMILY_COLOR[f] }))} />
              <StackedAreaChart
                ariaLabel="Fresh tokens per day by model family"
                labels={labels}
                unit="tokens"
                height={200}
                format={tok}
                series={families.map((f) => ({
                  name: f,
                  color: FAMILY_COLOR[f],
                  values: perFamily[f],
                }))}
              />
            </>
          ) : (
            <p className="py-6 text-center text-[13px] text-muted">No tokens in this period.</p>
          )}
        </div>
      </Section>

      <Section title="Cached and uncached input">
        <div className="space-y-3 p-4">
          {dates.length > 0 ? (
            <>
              <Legend
                items={[
                  { name: 'Read from cache', value: tok(t.cache_read), color: viz.aqua },
                  { name: 'Written to cache', value: tok(t.cache_write), color: viz.yellow },
                  { name: 'Uncached', value: tok(t.input), color: viz.orange },
                ]}
              />
              <StackedAreaChart
                ariaLabel="Input tokens per day: uncached, written to the cache, read from the cache"
                labels={labels}
                unit="tokens"
                height={170}
                format={tok}
                series={[
                  { name: 'Uncached', color: viz.orange, values: parts.uncached },
                  { name: 'Written to cache', color: viz.yellow, values: parts.writes },
                  { name: 'Read from cache', color: viz.aqua, values: parts.reads },
                ]}
              />
              <DataTable
                columns={['Day', 'Uncached', 'Cache write', 'Cache read', 'Hit rate', 'Cost']}
                rows={dates.map((d, i) => {
                  const all = parts.uncached[i] + parts.writes[i] + parts.reads[i]
                  return [
                    shortDate(d),
                    tok(parts.uncached[i]),
                    tok(parts.writes[i]),
                    tok(parts.reads[i]),
                    all > 0 ? `${Math.round((parts.reads[i] / all) * 100)}%` : '–',
                    usd(parts.cost[i]),
                  ]
                })}
              />
            </>
          ) : (
            <p className="py-6 text-center text-[13px] text-muted">No input in this period.</p>
          )}
        </div>
      </Section>
    </div>
  )
}
