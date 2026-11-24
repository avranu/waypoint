import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Pencil, Plus, Trash2 } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { relativeTime } from '@/lib/format'
import { modelPricesQuery, type ModelPrice } from '@/lib/usage-analytics'
import { Button, IconButton } from '@/components/ui/button'
import { Confirm, Dialog } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { Empty, LoadError, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'

const FIELDS = [
  ['input', 'Input'],
  ['output', 'Output'],
  ['cache_write_5m', 'Cache write (5 min)'],
  ['cache_write_1h', 'Cache write (1 h)'],
  ['cache_read', 'Cache read'],
] as const

export function PricesSection() {
  const qc = useQueryClient()
  const prices = useQuery(modelPricesQuery())
  const [editing, setEditing] = useState<ModelPrice | 'new' | null>(null)
  const [deleting, setDeleting] = useState<ModelPrice | null>(null)
  const del = useMutation({
    mutationFn: (model: string) => api.del(`/usage/prices?model=${enc(model)}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['usage'] }),
    onError: toastError,
  })
  const list = prices.data ?? []
  return (
    <Section
      title="Prices"
      className="mt-4"
      help={
        <>
          What a model’s tokens would cost on the API, in USD per million tokens. A price applies to
          every model id that starts with its name (the longest match wins), so{' '}
          <code className="font-mono text-xs">claude-opus-5-5</code> also prices dated versions of
          it. Costs on Agents → Usage use these; change a price and past usage is repriced. A price
          of 0 means unknown, and its tokens are flagged there.
        </>
      }
      actions={
        <Button
          size="sm"
          variant="primary"
          icon={<Plus className="size-3.5" />}
          onClick={() => setEditing('new')}
        >
          Add price
        </Button>
      }
    >
      {prices.isPending ? (
        <div className="p-4">
          <Skeleton className="h-24" />
        </div>
      ) : prices.isError ? (
        <LoadError what="Prices" error={prices.error} />
      ) : list.length === 0 ? (
        <Empty title="No prices">Add one to see what usage would cost on the API.</Empty>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-[13px]">
            <thead>
              <tr className="border-b border-line text-xs text-muted">
                <th className="px-4 py-1.5 text-left font-medium">Model</th>
                {FIELDS.map(([k, label]) => (
                  <th key={k} className="hidden px-2 py-1.5 text-right font-medium md:table-cell">
                    {label}
                  </th>
                ))}
                <th className="px-2 py-1.5 text-right font-medium md:hidden">In / out</th>
                <th className="w-20" />
              </tr>
            </thead>
            <tbody>
              {list.map((p) => (
                <tr key={p.model} className="border-b border-line last:border-0">
                  <td className="px-4 py-1.5">
                    <div className="font-mono text-xs">{p.model}</div>
                    {p.updated_at && (
                      <div className="text-[11px] text-subtle">
                        edited {relativeTime(p.updated_at)}
                      </div>
                    )}
                  </td>
                  {FIELDS.map(([k]) => (
                    <td
                      key={k}
                      className="hidden px-2 py-1.5 text-right tabular-nums md:table-cell"
                    >
                      {p[k] || '–'}
                    </td>
                  ))}
                  <td className="px-2 py-1.5 text-right tabular-nums md:hidden">
                    {p.input || '–'} / {p.output || '–'}
                  </td>
                  <td className="px-2 py-1.5 text-right">
                    <IconButton label={`Edit ${p.model}`} onClick={() => setEditing(p)}>
                      <Pencil className="size-3.5" />
                    </IconButton>
                    <IconButton label={`Remove ${p.model}`} onClick={() => setDeleting(p)}>
                      <Trash2 className="size-3.5" />
                    </IconButton>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <PriceDialog price={editing} onClose={() => setEditing(null)} />
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Remove the price of ${deleting?.model}?`}
        body="Usage of models it matched shows as having no price."
        confirmLabel="Remove"
        onConfirm={() => deleting && del.mutate(deleting.model)}
      />
    </Section>
  )
}

function PriceDialog({
  price,
  onClose,
}: {
  price: ModelPrice | 'new' | null
  onClose: () => void
}) {
  const qc = useQueryClient()
  const existing = price && price !== 'new' ? price : null
  const [model, setModel] = useState('')
  const [vals, setVals] = useState<Record<string, string>>({})
  useEffect(() => {
    if (!price) return
    setModel(existing?.model ?? '')
    setVals(Object.fromEntries(FIELDS.map(([k]) => [k, existing ? String(existing[k]) : ''])))
  }, [price]) // eslint-disable-line react-hooks/exhaustive-deps
  const save = useMutation({
    mutationFn: () =>
      api.put<ModelPrice>('/usage/prices', {
        model: model.trim(),
        ...Object.fromEntries(FIELDS.map(([k]) => [k, Number(vals[k] || 0)])),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['usage'] })
      onClose()
    },
    onError: toastError,
  })
  const valid =
    model.trim() !== '' &&
    !/\s/.test(model) &&
    FIELDS.every(
      ([k]) => vals[k] === '' || (Number(vals[k]) >= 0 && !Number.isNaN(Number(vals[k]))),
    )
  return (
    <Dialog
      open={!!price}
      onClose={onClose}
      title={existing ? `Price of ${existing.model}` : 'Add a price'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={!valid}
            loading={save.isPending}
            onClick={() => save.mutate()}
          >
            Save
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (valid) save.mutate()
        }}
      >
        <Field label="Model" hint="A model id or its start, e.g. claude-opus-5-5 or gpt-5.">
          <Input value={model} disabled={!!existing} onChange={(e) => setModel(e.target.value)} />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          {FIELDS.map(([k, label]) => (
            <Field key={k} label={`${label}, $ per million tokens`}>
              <Input
                inputMode="decimal"
                value={vals[k] ?? ''}
                onChange={(e) => setVals({ ...vals, [k]: e.target.value })}
                placeholder="0"
              />
            </Field>
          ))}
        </div>
      </form>
    </Dialog>
  )
}
