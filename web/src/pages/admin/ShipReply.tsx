import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RotateCcw } from 'lucide-react'
import { q } from '@/lib/queries'
import { renderShipReply, saveShipReply } from '@/lib/ship-reply'
import { Button } from '@/components/ui/button'
import { Field, Textarea } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

export function ShipReplySection() {
  const qc = useQueryClient()
  const tmpl = useQuery(q.shipReply())
  const [text, setText] = useState<string | null>(null)
  const value = text ?? tmpl.data?.reply ?? ''
  const save = useMutation({
    mutationFn: (v: string) => saveShipReply(v),
    onSuccess: (r) => {
      qc.setQueryData(q.shipReply().queryKey, r)
      setText(null)
      toast(r.custom ? 'Reply template saved' : 'Reply template back to the default')
    },
    onError: toastError,
  })
  const changed = !!tmpl.data && value.trim() !== tmpl.data.reply
  return (
    <Section
      title="Agents’ reply after wp ship"
      help={
        <>
          When an agent ships a pull request, <code className="font-mono text-xs">wp ship</code>{' '}
          ends with this template, and the agent ends its turn with it filled in: a short wrap-up
          instead of a long one.
        </>
      }
      className="mt-4"
    >
      {tmpl.isPending ? (
        <div className="p-4">
          <Skeleton className="h-32" />
        </div>
      ) : !tmpl.data ? null : (
        <form
          className="space-y-2 px-4 py-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (changed) save.mutate(value)
          }}
        >
          <Field
            label="Template"
            note={
              <>
                <code className="font-mono">{'{task}'}</code> is the task’s key (else the pull
                request’s title), <code className="font-mono">{'{pr}'}</code> its number and{' '}
                <code className="font-mono">{'{url}'}</code> its link. The agent fills in the{' '}
                <code className="font-mono">{'<…>'}</code> parts.
              </>
            }
          >
            <Textarea
              value={value}
              rows={6}
              autoGrow
              maxLength={2000}
              className="font-mono text-xs"
              onChange={(e) => setText(e.target.value)}
              placeholder={tmpl.data.default}
            />
          </Field>
          <div className="flex flex-wrap gap-2">
            <Button
              type="submit"
              size="sm"
              variant="primary"
              disabled={!changed}
              loading={save.isPending}
            >
              Save
            </Button>
            {(tmpl.data.custom || changed) && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                icon={<RotateCcw className="size-3.5" />}
                onClick={() => (changed ? setText(null) : save.mutate(''))}
              >
                {changed ? 'Undo' : 'Use the default'}
              </Button>
            )}
          </div>
          <div className="rounded-lg border border-line bg-sunken px-3 py-2">
            <div className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-subtle">
              What the agent sees
            </div>
            <pre className="whitespace-pre-wrap font-mono text-xs text-fg [overflow-wrap:anywhere]">
              {renderShipReply(value.trim() ? value : tmpl.data.default)}
            </pre>
          </div>
        </form>
      )}
    </Section>
  )
}
