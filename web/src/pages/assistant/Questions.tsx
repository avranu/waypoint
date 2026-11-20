// What the agent is waiting on you for, in the chat (WP-ADR1): Hermes's
// own approval of a command, and its clarify questions. (The gate's
// approvals are different: those are answered on Signal.)

import { useState } from 'react'
import clsx from 'clsx'
import { HelpCircle, ShieldQuestion } from 'lucide-react'
import { answer, type OpenRequest } from '@/lib/assistant-live'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'

export function QuestionCard({ assistantId, r }: { assistantId: number; r: OpenRequest }) {
  return r.method === 'approval' ? (
    <ApprovalCard assistantId={assistantId} r={r} />
  ) : (
    <ClarifyCard assistantId={assistantId} r={r} />
  )
}

const CHOICE_LABEL: Record<string, string> = {
  once: 'Allow once',
  session: 'Allow in this chat',
  always: 'Always allow',
  deny: 'Deny',
}

function ApprovalCard({ assistantId, r }: { assistantId: number; r: OpenRequest }) {
  const [busy, setBusy] = useState('')
  const choices = r.params.choices?.length ? r.params.choices : ['once', 'deny']
  const pick = (choice: string) => {
    setBusy(choice)
    answer(assistantId, r.id, { choice }).catch((e) => {
      setBusy('')
      toastError(e)
    })
  }
  return (
    <div
      className="rounded-xl border border-warn/40 bg-warn/5 p-3"
      onKeyDown={(e) => {
        if (e.key === 'Enter' && !busy && (e.target as HTMLElement).tagName !== 'BUTTON')
          pick('once')
      }}
    >
      <div className="flex items-center gap-2 text-[13px] font-medium">
        <ShieldQuestion className="size-4 text-warn" />
        {r.params.description || `Run ${r.params.tool_name ?? 'this'}?`}
      </div>
      {r.params.command && (
        <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words rounded-md bg-sunken p-2 font-mono text-xs">
          {r.params.command}
        </pre>
      )}
      <div className="mt-2.5 flex flex-wrap gap-1.5">
        {choices.map((c, i) => (
          <Button
            key={c}
            size="sm"
            autoFocus={i === 0}
            variant={c === 'deny' ? 'ghost' : i === 0 ? 'primary' : 'secondary'}
            loading={busy === c}
            disabled={!!busy}
            onClick={() => pick(c)}
            className={clsx(c === 'deny' && 'text-danger')}
          >
            {CHOICE_LABEL[c] ?? c}
          </Button>
        ))}
      </div>
    </div>
  )
}

function ClarifyCard({ assistantId, r }: { assistantId: number; r: OpenRequest }) {
  const qs = r.params.questions ?? []
  const [answers, setAnswers] = useState<Record<string, string[]>>({})
  const [other, setOther] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)
  const value = (qid: string) => {
    const o = other[qid]?.trim()
    const picked = answers[qid] ?? []
    return o ? [...picked, o] : picked
  }
  const ready = qs.every((q) => value(q.qid).length > 0)
  const submit = (skip = false) => {
    setBusy(true)
    const out: Record<string, string | null> = {}
    for (const q of qs) out[q.qid] = skip ? null : value(q.qid).join(', ') || null
    answer(assistantId, r.id, skip ? {} : { answers: out }).catch((e) => {
      setBusy(false)
      toastError(e)
    })
  }
  return (
    <div className="space-y-3 rounded-xl border border-accent/40 bg-accent-soft/40 p-3">
      {qs.map((q) => (
        <div key={q.qid} className="space-y-1.5">
          <div className="flex items-start gap-2 text-[13px] font-medium">
            <HelpCircle className="mt-0.5 size-4 shrink-0 text-accent" />
            <span className="whitespace-pre-wrap">{q.question}</span>
          </div>
          {!!q.choices?.length && (
            <div className="flex flex-wrap gap-1.5 pl-6">
              {q.choices.map((c) => {
                const on = (answers[q.qid] ?? []).includes(c)
                return (
                  <button
                    key={c}
                    aria-pressed={on}
                    onClick={() =>
                      setAnswers((a) => ({
                        ...a,
                        [q.qid]: q.multi_select
                          ? on
                            ? (a[q.qid] ?? []).filter((x) => x !== c)
                            : [...(a[q.qid] ?? []), c]
                          : on
                            ? []
                            : [c],
                      }))
                    }
                    className={clsx(
                      'rounded-full border px-3 py-1 text-[13px]',
                      on
                        ? 'border-accent bg-accent text-accent-fg'
                        : 'border-line bg-panel hover:bg-hover',
                    )}
                  >
                    {c}
                  </button>
                )
              })}
            </div>
          )}
          <div className="pl-6">
            <Input
              value={other[q.qid] ?? ''}
              placeholder={q.choices?.length ? 'Something else…' : 'Your answer'}
              onChange={(e) => setOther((o) => ({ ...o, [q.qid]: e.target.value }))}
              onKeyDown={(e) => {
                if (e.key === 'Enter' && ready && !busy) submit()
              }}
              className="h-8 text-[13px]"
            />
          </div>
        </div>
      ))}
      <div className="flex gap-1.5 pl-6">
        <Button
          size="sm"
          variant="primary"
          disabled={!ready}
          loading={busy}
          onClick={() => submit()}
        >
          Answer
        </Button>
        <Button size="sm" variant="ghost" disabled={busy} onClick={() => submit(true)}>
          Skip
        </Button>
      </div>
    </div>
  )
}
