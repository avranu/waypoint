// Agents page, Audits tab (WP-348): audits people and agents asked for,
// newest first, narrowed by the page's project and a status here; a new one
// starts from the button.

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Plus, ShieldCheck } from 'lucide-react'
import { agentMatches } from '@/lib/agents'
import { auditsQuery, type AuditStatus } from '@/lib/code-audits'
import { Button } from '@/components/ui/button'
import { Empty, Skeleton } from '@/components/ui/misc'
import { Explainer } from '@/components/ui/help'
import { SegmentedControl } from '@/components/ui/segmented'
import { AuditDialog } from '@/components/AuditDialog'
import { AuditRow } from '@/components/audits'
import type { HubTabProps } from './search'

type Show = '' | 'active' | 'done' | 'failed'

export function AuditsTab({ filters }: HubTabProps) {
  const [show, setShow] = useState<Show>('')
  const [asking, setAsking] = useState(false)
  const list = useQuery(
    auditsQuery({ project: filters.project || undefined, status: show as AuditStatus, limit: 100 }),
  )
  // The page's agent filter: audits that agent asked for.
  const audits = (list.data ?? []).filter(
    (a) => !filters.agent || (!!a.agent && agentMatches(filters.agent, a.agent)),
  )
  return (
    <div className="space-y-4">
      <Explainer id="agents:audits" label="What an audit is">
        Another model (Grok 4.7 through Cursor unless you choose others, or the one an LLM gateway
        gateway picks for audits where one is set up) reads a checkout from your runner: what landed
        on a branch lately (the last week, say), a repository, a branch, a commit, a range or a pull
        request. It reports what it finds, looking hardest where you ask. It never changes anything:
        each finding is a claim until someone confirms, fixes or rejects it. A pull request’s
        findings are posted on it too, and Claude can be asked to check and fix them. Agents ask
        with <code>wp audit</code>; automations with the audit step, and the gallery has one that
        audits each week’s changes.
      </Explainer>
      <div className="flex flex-wrap items-center gap-2">
        <SegmentedControl
          label="Show"
          value={show}
          onChange={setShow}
          textFrom="always"
          options={[
            { value: '', label: 'All', icon: null },
            { value: 'active', label: 'Running', icon: null },
            { value: 'done', label: 'Done', icon: null },
            { value: 'failed', label: 'Failed', icon: null },
          ]}
        />
        <div className="flex-1" />
        <Button
          variant="primary"
          size="sm"
          icon={<Plus className="size-3.5" />}
          onClick={() => setAsking(true)}
        >
          New audit
        </Button>
      </div>
      {list.isPending ? (
        <div className="space-y-2">
          <Skeleton className="h-14" />
          <Skeleton className="h-14" />
          <Skeleton className="h-14" />
        </div>
      ) : list.isError ? (
        <p className="text-[13px] text-danger">
          Couldn’t load audits: {(list.error as Error).message}
        </p>
      ) : audits.length === 0 ? (
        <div className="rounded-xl border border-line">
          <Empty
            icon={<ShieldCheck />}
            title={show || filters.project || filters.agent ? 'No audits match' : 'No audits yet'}
            action={
              !show && (
                <Button size="sm" onClick={() => setAsking(true)}>
                  Ask for one
                </Button>
              )
            }
          >
            {!show &&
              'Have a model read the last week’s changes, a branch or a pull request and say what is wrong with it.'}
          </Empty>
        </div>
      ) : (
        <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel">
          {audits.map((a) => (
            <AuditRow key={a.id} audit={a} />
          ))}
        </ul>
      )}
      <AuditDialog
        open={asking}
        onClose={() => setAsking(false)}
        project={filters.project || undefined}
      />
    </div>
  )
}
