// A pull request's audits (WP-348): the ones asked for, newest first, and a
// button to ask for another.

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ShieldCheck } from 'lucide-react'
import { q } from '@/lib/queries'
import { auditsQuery } from '@/lib/code-audits'
import type { PRDetail } from '@/lib/pulls'
import { Button } from '@/components/ui/button'
import { LoadError } from '@/components/ui/misc'
import { AuditDialog } from '@/components/AuditDialog'
import { AuditRow } from '@/components/audits'

export function PullAudits({ pr }: { pr: PRDetail }) {
  const [asking, setAsking] = useState(false)
  const list = useQuery(auditsQuery({ pr_id: pr.id, limit: 20 }))
  const projects = useQuery(q.projects()).data ?? []
  const canAsk = projects.some((p) => p.role === 'owner' || p.role === 'editor')
  const audits = list.data ?? []
  if (!audits.length && !canAsk) return null
  return (
    <section aria-label="Audits" className="space-y-2">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="text-[13px] font-semibold">Audits</h3>
        {canAsk && (
          <Button
            size="xs"
            className="ml-auto"
            icon={<ShieldCheck className="size-3.5" />}
            onClick={() => setAsking(true)}
            title="A model on your runner reads what it changes and reports defects; it changes nothing"
          >
            Audit
          </Button>
        )}
      </div>
      {list.isError ? (
        <LoadError what="Audits" error={list.error} />
      ) : audits.length === 0 ? (
        <p className="text-xs text-subtle">
          None yet. An audit has a model read what this changes and post what it finds.
        </p>
      ) : (
        <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line">
          {audits.map((a) => (
            <AuditRow key={a.id} audit={a} onPR />
          ))}
        </ul>
      )}
      <AuditDialog
        open={asking}
        onClose={() => setAsking(false)}
        pr={{ id: pr.id, repo: pr.repo, number: pr.number, title: pr.title }}
      />
    </section>
  )
}
