// How Waypoint reaches the assistant: its door, owner and login, the
// Waypoint token the agent uses, and a test that goes all the way through.
// Admins change these under Admin → Assistants.

import { useEffect, useRef } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { CircleCheck, CircleX, PlugZap } from 'lucide-react'
import {
  health,
  HEALTH_COLOR,
  HEALTH_LABEL,
  useAssistantMutations,
  type Assistant,
} from '@/lib/assistant'
import { q } from '@/lib/queries'
import { relativeTime } from '@/lib/format'
import { Button, Spinner } from '@/components/ui/button'
import { Avatar, Badge, Section } from '@/components/ui/misc'
import { CopyField } from '../../settings/common'
import { Rows } from './common'
import { gateAgentUrl } from '../tools/connections'

const STEP_LABEL: Record<string, string> = {
  gate: 'The door and the gate',
  hermes: 'Signing in to Hermes',
}

/** The result of POST /assistants/{id}/test, step by step; `auto` runs it at once. */
export function TestSteps({ id, auto }: { id: number; auto?: boolean }) {
  const test = useAssistantMutations(id).test
  const ran = useRef(false)
  useEffect(() => {
    if (auto && !ran.current) {
      ran.current = true
      test.mutate()
    }
  }, [auto, test])
  return (
    <div className="space-y-2">
      {!auto && (
        <Button
          icon={<PlugZap className="size-4" />}
          loading={test.isPending}
          onClick={() => test.mutate()}
        >
          Test connection
        </Button>
      )}
      {auto && test.isPending && (
        <div className="flex items-center gap-2 text-[13px] text-muted">
          <Spinner className="size-4" /> Testing the connection…
        </div>
      )}
      {test.isError && (
        <p className="text-[13px] text-danger">
          {test.error instanceof Error ? test.error.message : String(test.error)}
        </p>
      )}
      {test.data && (
        <ul className="space-y-1.5" aria-live="polite">
          {test.data.map((s) => (
            <li key={s.name} className="flex items-start gap-2 text-[13px]">
              {s.ok ? (
                <CircleCheck className="mt-0.5 size-4 shrink-0 text-ok" />
              ) : (
                <CircleX className="mt-0.5 size-4 shrink-0 text-danger" />
              )}
              <span className="min-w-0">
                <span className={clsx(!s.ok && 'font-medium')}>{STEP_LABEL[s.name] ?? s.name}</span>
                {s.error && (
                  <span className="block break-words text-xs text-danger">{s.error}</span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function ConnectionSection({ assistant: a }: { assistant: Assistant }) {
  const me = useQuery(q.me()).data
  const h = health(a)
  return (
    <>
      <Section
        title="Connection"
        actions={
          me?.is_admin && (
            <Link
              to="/admin"
              search={{ tab: 'assistants' }}
              className="text-[13px] font-medium text-accent hover:underline"
            >
              Change in Admin
            </Link>
          )
        }
      >
        <Rows
          rows={[
            ['Door', <CopyField key="d" value={a.url} />],
            [
              'Gate, for the agent',
              gateAgentUrl(a) ? (
                <span key="g" className="font-mono text-xs">
                  {gateAgentUrl(a)}
                </span>
              ) : (
                <span className="text-muted">Not set: connections can't be added from here</span>
              ),
            ],
            [
              'Certificate',
              a.has_ca ? (
                'Its own CA certificate'
              ) : (
                <span className="text-muted">The system's roots</span>
              ),
            ],
            [
              'Owner',
              <span key="o" className="flex items-center gap-2">
                <Avatar user={a.owner} size={20} />
                {a.owner.display_name || a.owner.username}
              </span>,
            ],
            [
              'Hermes login',
              <span key="u" className="font-mono text-xs">
                {a.username}
              </span>,
            ],
            [
              'Waypoint token',
              a.token_id ? (
                <span className="flex flex-wrap items-center gap-2">
                  {a.token || `#${a.token_id}`}
                  {a.agent && <Badge>acts as {a.agent}</Badge>}
                </span>
              ) : (
                <span className="text-muted">None</span>
              ),
            ],
            [
              'Last check',
              <span key="c" className="flex flex-wrap items-center gap-2">
                <Badge color={HEALTH_COLOR[h]}>{HEALTH_LABEL[h]}</Badge>
                {a.state.checked_at && !a.state.checked_at.startsWith('0001') && (
                  <span className="text-subtle">{relativeTime(a.state.checked_at)}</span>
                )}
                {a.state.error && <span className="break-words text-danger">{a.state.error}</span>}
              </span>,
            ],
          ]}
        />
        <div className="border-t border-line p-4">
          <TestSteps id={a.id} />
        </div>
      </Section>
    </>
  )
}
