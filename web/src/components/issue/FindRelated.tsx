// Open issues worth linking to this one, from the issues' own words: what
// its text says it waits for, the same title, much the same words (WP-296).
// Adding one or saying it isn't related teaches the ranking (WP-958).

import { useState } from 'react'
import { Sparkles, X } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { useSuggestRelations } from '@/lib/queries'
import type { RelationSuggestion } from '@/lib/types'
import { Button, IconButton, Spinner } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Badge } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'

const confidenceColor = { low: 'var(--wp-warn)', medium: 'var(--wp-accent)', high: 'var(--wp-ok)' }

const kindLabel: Record<string, string> = {
  relates: 'relates to',
  blocks: 'blocks',
  blocked_by: 'is blocked by',
  duplicates: 'duplicates',
}

/** Suggests open issues this one connects to; add with one click. */
export function FindRelated({ issueKey, onAdded }: { issueKey: string; onAdded: () => void }) {
  const [open, setOpen] = useState(false)
  const [added, setAdded] = useState<string[]>([])
  const [dismissed, setDismissed] = useState<string[]>([])
  const suggested = useSuggestRelations(issueKey)
  const add = (s: RelationSuggestion) =>
    api.post(`/issues/${enc(issueKey)}/relations`, { kind: s.kind, issue: s.key }).then(() => {
      setAdded((a) => [...a, s.key])
      onAdded()
    }, toastError)
  const dismiss = (s: RelationSuggestion) =>
    api
      .post(`/issues/${enc(issueKey)}/relations/suggested/dismiss`, { issue: s.key })
      .then(() => setDismissed((d) => [...d, s.key]), toastError)
  const undo = (s: RelationSuggestion) =>
    api
      .post(`/issues/${enc(issueKey)}/relations/suggested/dismiss`, { issue: s.key, undo: true })
      .then(() => setDismissed((d) => d.filter((k) => k !== s.key)), toastError)
  const list = suggested.data?.suggestions ?? []
  return (
    <>
      <IconButton
        label="Find related"
        size="xs"
        onClick={() => {
          setAdded([])
          setDismissed([])
          setOpen(true)
          suggested.mutate(undefined, { onError: (e) => (setOpen(false), toastError(e)) })
        }}
        disabled={suggested.isPending}
        icon={<Sparkles className="size-3.5" />}
      />
      <Dialog
        open={open}
        onClose={() => setOpen(false)}
        title="Related issues"
        footer={
          <Button variant="primary" onClick={() => setOpen(false)}>
            Done
          </Button>
        }
      >
        <div className="p-4">
          {suggested.isPending ? (
            <div className="flex items-center gap-2 text-[13px] text-subtle">
              <Spinner className="size-3.5" /> Looking through open issues…
            </div>
          ) : list.length === 0 ? (
            <p className="text-[13px] text-muted">Nothing clearly related among the open issues.</p>
          ) : (
            <ul className="space-y-1.5">
              {list.map((s) => (
                <li
                  key={s.key}
                  className="flex items-start gap-2.5 rounded-lg px-2 py-1.5 hover:bg-hover"
                >
                  <div className="min-w-0 flex-1 text-[13px]">
                    <div>
                      <span className="text-subtle">{kindLabel[s.kind] ?? s.kind}</span>{' '}
                      <span className="font-mono text-xs text-muted">{s.key}</span> {s.title}
                    </div>
                    <div className="text-xs text-subtle">{s.reason}</div>
                  </div>
                  <Badge color={confidenceColor[s.confidence]}>{s.confidence}</Badge>
                  {dismissed.includes(s.key) ? (
                    <>
                      <span className="pt-1 text-xs text-subtle">Not related</span>
                      <Button size="xs" onClick={() => undo(s)}>
                        Undo
                      </Button>
                    </>
                  ) : (
                    <>
                      <Button size="xs" disabled={added.includes(s.key)} onClick={() => add(s)}>
                        {added.includes(s.key) ? 'Added' : 'Add'}
                      </Button>
                      {!added.includes(s.key) && (
                        <IconButton
                          label="Not related"
                          title="Not related: don't suggest it again"
                          size="xs"
                          onClick={() => dismiss(s)}
                          icon={<X className="size-3.5" />}
                        />
                      )}
                    </>
                  )}
                </li>
              ))}
            </ul>
          )}
        </div>
      </Dialog>
    </>
  )
}
