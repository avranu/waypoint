import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Bot, CalendarDays, ExternalLink, RefreshCw, Terminal } from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Field, Input } from '@/components/ui/input'
import { Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { CopyField, Help, Steps } from './common'

export function ConnectionsTab({ onCreateToken }: { onCreateToken: () => void }) {
  return (
    <>
      <CalendarSection />
      <CLISection />
      <AgentsSection onCreateToken={onCreateToken} />
    </>
  )
}

function CalendarSection() {
  const qc = useQueryClient()
  const cal = useQuery(q.calendar())
  const [confirm, setConfirm] = useState(false)
  const rotate = useMutation({
    mutationFn: () => api.post<{ url: string }>('/me/calendar/rotate'),
    onSuccess: (r) => {
      qc.setQueryData(['calendar'], r)
      toast('New calendar link created')
    },
    onError: toastError,
  })
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <CalendarDays className="size-4 text-subtle" /> Calendar feed
        </span>
      }
      helpId="settings:calendar"
      help={
        <>
          A private link with your assigned issues' due dates and your sprints, for Google, Apple or
          Outlook calendars. They refresh it about every 30 minutes.
        </>
      }
    >
      <div className="space-y-4 p-4">
        {cal.data ? (
          <CopyField value={cal.data.url} />
        ) : cal.isError ? (
          <p className="text-[13px] text-danger">Couldn't load your link: {cal.error.message}</p>
        ) : (
          <Skeleton className="h-9" />
        )}
        <div className="space-y-2">
          <div className="text-[13px] font-medium">Add it to Google Calendar</div>
          <Steps
            items={[
              'Copy the link above.',
              <>
                Open{' '}
                <a
                  href="https://calendar.google.com"
                  target="_blank"
                  rel="noreferrer"
                  className="text-accent hover:underline"
                >
                  Google Calendar
                </a>{' '}
                on a computer.
              </>,
              <>
                In the left sidebar, click <b className="font-medium text-fg">+</b> next to{' '}
                <b className="font-medium text-fg">Other calendars</b>, then{' '}
                <b className="font-medium text-fg">From URL</b>.
              </>,
              <>
                Paste the link and click <b className="font-medium text-fg">Add calendar</b>.
              </>,
            ]}
          />
        </div>
        <div className="flex flex-wrap items-center justify-between gap-2 border-t border-line pt-3">
          <span className="text-xs text-subtle">
            Anyone with the link can see these dates. Shared it by mistake? Make a new one.
          </span>
          <Button
            size="sm"
            icon={<RefreshCw className="size-3.5" />}
            loading={rotate.isPending}
            onClick={() => setConfirm(true)}
          >
            New link
          </Button>
        </div>
      </div>
      <Confirm
        open={confirm}
        onClose={() => setConfirm(false)}
        title="Replace the calendar link?"
        body="The current link stops working. You'll need to re-add the new one in your calendar app."
        confirmLabel="Replace link"
        onConfirm={() => rotate.mutate()}
      />
    </Section>
  )
}

function CLISection() {
  const origin = window.location.origin
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Terminal className="size-4 text-subtle" /> Command line
        </span>
      }
      helpId="settings:cli"
      help={
        <>
          <b>wp</b> works with issues and pages from a terminal, and lets coding agents take work
          meant for them (<b>wp next</b>, <b>wp done</b>). It installs to ~/.local/bin.
        </>
      }
    >
      <div className="space-y-4 p-4">
        <p className="text-[13px] text-muted">
          On a computer where you use Claude Code, one command installs it, signs you in, adds the
          session hooks and starts a runner, and keeps all of it up to date:
        </p>
        <CopyField value={`curl -fsSL ${origin}/setup | sh`} />
        <p className="text-[13px] text-muted">Or just the command line:</p>
        <Steps
          items={[
            <>
              Install it:
              <CopyField value={`curl -fsSL ${origin}/cli | sh`} className="mt-1.5" />
            </>,
            <>
              Sign in. It shows a code and opens this site to approve it; add{' '}
              <code className="font-mono">--agent provider/model</code> when an agent will use it.
              <CopyField value="wp auth login" className="mt-1.5" />
            </>,
          ]}
        />
      </div>
    </Section>
  )
}

function AgentsSection({ onCreateToken }: { onCreateToken: () => void }) {
  const origin = window.location.origin
  const [token, setToken] = useState('')
  const tok = token.trim() || '<token>'
  const cmd = `claude mcp add --transport http waypoint ${origin}/mcp --header "Authorization: Bearer ${tok}"`
  const json = JSON.stringify(
    {
      mcpServers: {
        waypoint: {
          type: 'http',
          url: `${origin}/mcp`,
          headers: { Authorization: `Bearer ${tok}` },
        },
      },
    },
    null,
    2,
  )
  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          <Bot className="size-4 text-subtle" /> AI agents & scripts
        </span>
      }
      helpId="settings:agents"
      help={<>Agents work with your access (or less, with a read-only token).</>}
    >
      <div className="space-y-4 p-4">
        <ul className="space-y-2 text-[13px] text-muted">
          <li>
            <b className="font-medium text-fg">MCP</b> — for Claude Code, Claude Desktop and other
            MCP clients. Endpoint:{' '}
            <code className="break-all rounded bg-sunken px-1 font-mono text-xs text-fg">
              {origin}/mcp
            </code>
          </li>
          <li>
            <b className="font-medium text-fg">REST API</b> — for scripts and anything else.
            Plain-text docs at{' '}
            <a
              href="/api/v1/help"
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-0.5 text-accent hover:underline"
            >
              /api/v1/help <ExternalLink className="size-3" />
            </a>
          </li>
        </ul>
        <div className="flex flex-col gap-2 rounded-lg bg-sunken/60 p-3 sm:flex-row sm:items-end">
          <Field
            label="Your token"
            hint="Only fills in the snippets below; it isn't saved."
            className="flex-1"
          >
            <Input
              value={token}
              onChange={(e) => setToken(e.target.value)}
              placeholder="Paste an API token"
              className="font-mono"
              autoComplete="off"
            />
          </Field>
          <Button onClick={onCreateToken}>Create a token</Button>
        </div>
        <div className="space-y-1.5">
          <div className="text-[13px] font-medium">Claude Code</div>
          <CopyField value={cmd} multiline />
        </div>
        <div className="space-y-1.5">
          <div className="text-[13px] font-medium">Other MCP clients</div>
          <Help className="text-xs">
            In the client's <code className="font-mono">mcp.json</code> (Claude Desktop, Cursor…):
          </Help>
          <CopyField value={json} multiline />
        </div>
      </div>
    </Section>
  )
}
