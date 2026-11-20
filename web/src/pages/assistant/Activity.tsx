// The assistant's Activity tab (WP-ADR1): what it is doing, what it has
// done, and what waits on you. Four views, kept in the URL (`view`): now
// and a timeline, the gate's approvals, its raw audit, and the logs.

import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { History, ScrollText, ShieldCheck, TerminalSquare } from 'lucide-react'
import { approvalsQuery, type Assistant } from '@/lib/assistant'
import { SegmentedControl } from '@/components/ui/segmented'
import type { AssistantSearch } from './search'
import { NowView } from './activity/Now'
import { ApprovalsView } from './activity/Approvals'
import { AuditView } from './activity/Audit'
import { LogsView } from './activity/Logs'

type View = 'now' | 'approvals' | 'audit' | 'logs'

const VIEWS: View[] = ['now', 'approvals', 'audit', 'logs']

export function ActivityTab({ assistant }: { assistant: Assistant }) {
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const view: View = VIEWS.includes(search.view as View) ? (search.view as View) : 'now'
  const pending = useQuery({ ...approvalsQuery(assistant.id, 50), refetchInterval: 10_000 }).data
    ?.pending.length
  const go = (v: View) =>
    navigate({
      search: (s: AssistantSearch) => ({ ...s, view: v === 'now' ? undefined : v }),
      replace: true,
    })
  return (
    <div className="space-y-4">
      <SegmentedControl
        label="Activity view"
        value={view}
        onChange={go}
        textFrom="always"
        className="h-9 [&>button]:h-8 [&>button]:px-2.5"
        options={[
          { value: 'now', label: 'Now', icon: <History className="size-3.5" /> },
          {
            value: 'approvals',
            label: pending ? `Approvals · ${pending}` : 'Approvals',
            icon: <ShieldCheck className="size-3.5" />,
          },
          { value: 'audit', label: 'Audit', icon: <ScrollText className="size-3.5" /> },
          { value: 'logs', label: 'Logs', icon: <TerminalSquare className="size-3.5" /> },
        ]}
      />
      {view === 'now' && <NowView assistant={assistant} />}
      {view === 'approvals' && <ApprovalsView assistant={assistant} />}
      {view === 'audit' && <AuditView assistant={assistant} />}
      {view === 'logs' && <LogsView assistant={assistant} />}
    </div>
  )
}
