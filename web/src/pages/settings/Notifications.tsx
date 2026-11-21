import { useEffect, useRef, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { Eye, EyeOff, Send } from 'lucide-react'
import { api } from '@/lib/api'
import { useSavePrefs } from '@/lib/queries'
import type { User } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Field, Input } from '@/components/ui/input'
import { Section } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import { CheckRow, Help, Steps } from './common'
import { PushDevice } from './PushDevice'

const KINDS: [string, string, string][] = [
  ['assigned', 'Assigned to you', 'Someone assigns you an issue'],
  ['mentioned', 'Mentions', 'Someone @mentions you in a comment or description'],
  ['commented', 'Comments', 'New comments on issues you watch'],
  ['status', 'Status changes', 'Issues you watch move to another status'],
  ['shared', 'Shared with you', 'Someone shares an issue with you'],
  ['due', 'Due dates', 'Your issues are due soon or overdue'],
  ['pr', 'Pull requests', 'Linked pull requests are opened, merged or closed'],
  ['automation', 'Automations', 'Automation rules that notify you'],
  ['unpushed', 'Unpushed commits', 'Your session ends leaving commits its pull request hasn’t got'],
  ['audit', 'Audits', 'An audit you asked for ends, with its findings'],
  ['autopilot', 'Autopilot', 'Autopilot paused by a limit, or unable to move a session on'],
  [
    'risky',
    'Risky commands',
    'An agent runs a high-risk command: a force push to main, production, another machine, a disk',
  ],
  [
    'ci',
    'CI health',
    'A runner or the CI queue stops making progress, or a host goes quiet (admins)',
  ],
  [
    'deploy',
    'Deploys',
    'A production or staging environment of a project you own runs behind its branch: a release or deploy didn’t happen',
  ],
  [
    'objective',
    'Objectives',
    'An objective is proposed for you to read, met, ended, needs you, or has gone rounds without getting nearer',
  ],
  ['goals', 'Goals', 'The weekly check finds work drifting from a project’s goals'],
  ['sandbox', 'Sandbox', 'Someone changes the sandbox of a project you own, or of a machine'],
  [
    'server',
    'Server health',
    'A scheduled job keeps failing, hangs or stops running, or an error keeps being logged (admins)',
  ],
  [
    'collector',
    'Collectors',
    'Fix traces, proofs or prep scores have waited two days with nothing collected (admins)',
  ],
  [
    'safety',
    'Safety drill',
    'The weekly drill finds the task screen letting a bad task through (admins)',
  ],
]

export function NotificationsTab({ me }: { me: User }) {
  const savePrefs = useSavePrefs()
  const [url, setUrl] = useState(me.prefs.gotify_url ?? '')
  const [token, setToken] = useState(me.prefs.gotify_token ?? '')
  const [show, setShow] = useState(false)
  // What was typed stays until it is saved: a refetch, or the revert after a
  // failed save, mustn't put the old value back under the person's hands.
  const edited = useRef(false)
  useEffect(() => {
    if (edited.current) return
    setUrl(me.prefs.gotify_url ?? '')
    setToken(me.prefs.gotify_token ?? '')
  }, [me.prefs.gotify_url, me.prefs.gotify_token])
  const dirty =
    url.trim() !== (me.prefs.gotify_url ?? '') || token.trim() !== (me.prefs.gotify_token ?? '')
  const configured = !!(me.prefs.gotify_url && me.prefs.gotify_token)
  const muted = me.prefs.gotify_muted ?? []

  const test = useMutation({
    mutationFn: async () => {
      if (dirty) {
        await savePrefs.mutateAsync({ gotify_url: url.trim(), gotify_token: token.trim() })
        edited.current = false
      }
      return api.post('/me/gotify/test')
    },
    onSuccess: () => toast('Test sent. Check your phone.'),
    onError: toastError,
  })

  const toggle = (kind: string, on: boolean) => {
    const next = on ? muted.filter((k) => k !== kind) : [...new Set([...muted, kind])]
    savePrefs.mutate({ gotify_muted: next }, { onError: toastError })
  }

  return (
    <>
      <PushDevice me={me} kinds={KINDS} />
      <Section title="Push notifications with Gotify">
        <form
          className="space-y-4 p-4"
          onSubmit={(e) => {
            e.preventDefault()
            if (dirty)
              savePrefs.mutate(
                { gotify_url: url.trim(), gotify_token: token.trim() },
                {
                  onSuccess: () => {
                    edited.current = false
                    toast('Saved')
                  },
                  onError: toastError,
                },
              )
          }}
        >
          <Help>
            Everything shows up in your Inbox. For your phone too, connect a{' '}
            <a
              href="https://gotify.net"
              target="_blank"
              rel="noreferrer"
              className="text-accent hover:underline"
            >
              Gotify
            </a>{' '}
            server.
          </Help>
          <Steps
            items={[
              'In Gotify, open Apps and create an app called “Waypoint”.',
              'Copy its token and paste it below with your server’s address.',
              'Save, then send a test.',
            ]}
          />
          <Field label="Gotify server URL">
            <Input
              type="url"
              value={url}
              onChange={(e) => ((edited.current = true), setUrl(e.target.value))}
              placeholder="https://gotify.example.com"
            />
          </Field>
          <Field label="App token">
            <div className="relative">
              <Input
                type={show ? 'text' : 'password'}
                value={token}
                onChange={(e) => ((edited.current = true), setToken(e.target.value))}
                placeholder="AbCdEf123…"
                autoComplete="off"
                className="pr-10 font-mono"
              />
              <IconButton
                label={show ? 'Hide token' : 'Show token'}
                onClick={() => setShow(!show)}
                className="absolute right-1 top-1"
                icon={show ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
              />
            </div>
          </Field>
          <div className="flex flex-wrap justify-end gap-2">
            <Button
              icon={<Send className="size-3.5" />}
              disabled={!url.trim() || !token.trim()}
              loading={test.isPending}
              onClick={() => test.mutate()}
            >
              Send test
            </Button>
            <Button
              type="submit"
              variant="primary"
              disabled={!dirty}
              loading={savePrefs.isPending && !test.isPending}
            >
              Save
            </Button>
          </div>
        </form>
      </Section>
      <Section title="What goes to Gotify">
        <div className="divide-y divide-line">
          {KINDS.map(([kind, label, hint]) => (
            <CheckRow
              key={kind}
              label={label}
              hint={hint}
              checked={!muted.includes(kind)}
              disabled={!configured}
              onChange={(on) => toggle(kind, on)}
            />
          ))}
        </div>
        {!configured && (
          <div className="border-t border-line px-4 py-2.5 text-xs text-subtle">
            Connect Gotify above to choose what gets pushed.
          </div>
        )}
      </Section>
    </>
  )
}
