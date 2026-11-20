// Admin → Email: the SMTP server Waypoint sends through (WP-7). Until it is
// set, automations can't send email and the editor doesn't offer it.

import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { q } from '@/lib/queries'
import { catalogQuery } from '@/lib/automations'
import {
  saveEmailSettings,
  sendTestEmail,
  type EmailSettings,
  type EmailSettingsInput,
  type SMTPSecurity,
} from '@/lib/email'
import { Button } from '@/components/ui/button'
import { Field, Input, Select } from '@/components/ui/input'
import { Badge, Section, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'

const DEFAULT_PORT: Record<SMTPSecurity, number> = { starttls: 587, tls: 465, none: 25 }

const toInput = (s: EmailSettings): EmailSettingsInput => ({
  host: s.host,
  port: s.port,
  security: s.security,
  username: s.username,
  password: '',
  from: s.from,
})

export function EmailTab() {
  const settings = useQuery(q.emailSettings())
  if (settings.isPending) return <Skeleton className="h-64" />
  if (!settings.data) return null
  return <EmailForm key={JSON.stringify(settings.data)} settings={settings.data} />
}

function EmailForm({ settings }: { settings: EmailSettings }) {
  const qc = useQueryClient()
  const [v, setV] = useState<EmailSettingsInput>(() => toInput(settings))
  const [testTo, setTestTo] = useState('')
  const set = (patch: Partial<EmailSettingsInput>) => setV((o) => ({ ...o, ...patch }))
  const saved = (s: EmailSettings, msg: string) => {
    qc.setQueryData(q.emailSettings().queryKey, s)
    void qc.invalidateQueries({ queryKey: catalogQuery().queryKey })
    toast(msg)
  }
  const save = useMutation({
    mutationFn: (input: EmailSettingsInput) => saveEmailSettings(input),
    onSuccess: (s) => saved(s, s.configured ? 'Email settings saved' : 'Email is off'),
    onError: toastError,
  })
  const test = useMutation({
    mutationFn: () => sendTestEmail(testTo.trim()),
    onSuccess: () => toast('Test email sent'),
    onError: toastError,
  })
  const changed = JSON.stringify(v) !== JSON.stringify(toInput(settings))
  return (
    <div className="space-y-6">
      <Section
        title={
          <span className="flex items-center gap-2">
            SMTP server
            {settings.configured ? <Badge color="var(--wp-ok)">On</Badge> : <Badge>Off</Badge>}
          </span>
        }
        helpId="admin:email"
        help={
          <p>
            The mail server Waypoint sends through. Once it is set, automations can{' '}
            <b>Send an email</b> to people (to the address on their account, if they can see the
            issue) or to any address.
          </p>
        }
      >
        <form
          className="grid gap-4 px-4 py-4 sm:grid-cols-2"
          onSubmit={(e) => {
            e.preventDefault()
            if (changed) save.mutate(v)
          }}
        >
          <Field label="Host">
            <Input
              value={v.host}
              placeholder="smtp.example.com"
              autoComplete="off"
              onChange={(e) => set({ host: e.target.value })}
            />
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Security">
              <Select
                value={v.security}
                onChange={(e) => {
                  const security = e.target.value as SMTPSecurity
                  // Follow the usual port unless someone typed their own.
                  const port = v.port === DEFAULT_PORT[v.security] ? DEFAULT_PORT[security] : v.port
                  set({ security, port })
                }}
              >
                <option value="starttls">STARTTLS</option>
                <option value="tls">TLS</option>
                <option value="none">None</option>
              </Select>
            </Field>
            <Field label="Port">
              <Input
                type="number"
                min={1}
                max={65535}
                value={v.port || ''}
                onChange={(e) => set({ port: Number(e.target.value) })}
              />
            </Field>
          </div>
          <Field label="Username" note="Leave empty if the server takes mail without a login.">
            <Input
              value={v.username}
              autoComplete="off"
              onChange={(e) => set({ username: e.target.value })}
            />
          </Field>
          <Field
            label="Password"
            note={
              settings.has_password && !v.clear_password ? (
                <>
                  A password is set; leave this empty to keep it.{' '}
                  <button
                    type="button"
                    className="text-accent hover:underline"
                    onClick={() => set({ password: '', clear_password: true })}
                  >
                    Remove it
                  </button>
                </>
              ) : undefined
            }
          >
            <Input
              type="password"
              value={v.password}
              autoComplete="new-password"
              placeholder={settings.has_password && !v.clear_password ? '••••••••' : ''}
              onChange={(e) => set({ password: e.target.value, clear_password: undefined })}
            />
          </Field>
          <Field
            label="From"
            className="sm:col-span-2"
            note="Who the email is from, like Waypoint <waypoint@example.com>."
          >
            <Input
              value={v.from}
              placeholder="Waypoint <waypoint@example.com>"
              onChange={(e) => set({ from: e.target.value })}
            />
          </Field>
          <div className="flex flex-wrap gap-2 sm:col-span-2">
            <Button
              type="submit"
              size="sm"
              variant="primary"
              disabled={!changed}
              loading={save.isPending}
            >
              Save
            </Button>
            {settings.configured && (
              <Button
                type="button"
                size="sm"
                loading={save.isPending}
                onClick={() =>
                  save.mutate({ ...toInput(settings), host: '', clear_password: true })
                }
              >
                Turn email off
              </Button>
            )}
          </div>
        </form>
      </Section>
      {settings.configured && (
        <Section title="Send a test">
          <form
            className="flex flex-wrap items-end gap-3 px-4 py-4"
            onSubmit={(e) => {
              e.preventDefault()
              test.mutate()
            }}
          >
            <Field label="To" note="Empty sends it to you." className="min-w-0 flex-1">
              <Input
                type="email"
                value={testTo}
                placeholder="you@example.com"
                onChange={(e) => setTestTo(e.target.value)}
              />
            </Field>
            <Button type="submit" size="sm" loading={test.isPending}>
              Send test email
            </Button>
          </form>
        </Section>
      )}
    </div>
  )
}
