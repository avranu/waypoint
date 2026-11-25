import { useMemo, useState } from 'react'
import { useQueries, useQuery } from '@tanstack/react-query'
import { Plus, X } from 'lucide-react'
import { q, useSavePrefs } from '@/lib/queries'
import type { SessionPrefs, SessionSettings, User } from '@/lib/types'
import { Button, IconButton } from '@/components/ui/button'
import { Field, Input, Select } from '@/components/ui/input'
import { Section } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'

const repoRE = /^[a-z0-9._-]+(\/[a-z0-9._-]+)+$/

type Toggle = 'remote_control' | 'code_mode' | 'chrome'

const toggles: { key: Toggle; label: string; hint: string }[] = [
  {
    key: 'remote_control',
    label: 'Remote Control',
    hint: 'Lets you follow and steer the session from claude.ai or the Claude app. Autopilot turns it on unless Code Mode is on; Off leaves out the flag, so your own /config setting still applies.',
  },
  {
    key: 'code_mode',
    label: 'Code Mode',
    hint: 'Experimental: Claude writes code to call its tools. Claude Code turns it off when Remote Control is on and in background sessions (follow-ups), and asks once per machine before its first use.',
  },
  {
    key: 'chrome',
    label: 'Claude in Chrome',
    hint: 'Starts the session with the Chrome browser tools, or without them.',
  },
]

const efforts = ['low', 'medium', 'high', 'xhigh', 'max'] as const
const modes = [
  ['acceptEdits', 'Accept edits'],
  ['auto', 'Auto'],
  ['dontAsk', "Don't ask"],
  ['manual', 'Manual'],
  ['plan', 'Plan'],
] as const

function triState(v: boolean | undefined) {
  return v === undefined ? '' : v ? 'on' : 'off'
}

/** The six controls for one set of settings; `unset` names what an unset field means. */
function SettingsFields({
  value,
  unset,
  onChange,
}: {
  value: SessionSettings
  unset: string
  onChange: (next: SessionSettings) => void
}) {
  const set = <K extends keyof SessionSettings>(k: K, v: SessionSettings[K]) => {
    const next = { ...value }
    if (v === undefined || v === '') delete next[k]
    else next[k] = v
    onChange(next)
  }
  return (
    <div className="grid gap-3 p-4 sm:grid-cols-3">
      {toggles.map((t) => (
        <Field key={t.key} label={t.label} hint={t.hint}>
          <Select
            value={triState(value[t.key])}
            onChange={(e) =>
              set(t.key, e.target.value === '' ? undefined : e.target.value === 'on')
            }
          >
            <option value="">{unset}</option>
            <option value="on">On</option>
            <option value="off">Off</option>
          </Select>
        </Field>
      ))}
      <Field
        label="Model"
        hint="An alias (opus, sonnet) or a model id. A task's own model choice wins."
      >
        <Input
          key={value.model ?? ''}
          defaultValue={value.model ?? ''}
          placeholder={unset}
          maxLength={80}
          onBlur={(e) => {
            const v = e.target.value.trim()
            if (v !== (value.model ?? '')) set('model', v)
          }}
        />
      </Field>
      <Field label="Effort">
        <Select
          value={value.effort ?? ''}
          onChange={(e) => set('effort', e.target.value as SessionSettings['effort'])}
        >
          <option value="">{unset}</option>
          {efforts.map((v) => (
            <option key={v} value={v}>
              {v}
            </option>
          ))}
        </Select>
      </Field>
      <Field
        label="Permission mode"
        hint="Bypass permissions isn't offered: pass it by hand to the one session that needs it."
      >
        <Select
          value={value.permission_mode ?? ''}
          onChange={(e) =>
            set('permission_mode', e.target.value as SessionSettings['permission_mode'])
          }
        >
          <option value="">{unset}</option>
          {modes.map(([v, label]) => (
            <option key={v} value={v}>
              {label}
            </option>
          ))}
        </Select>
      </Field>
    </div>
  )
}

/** The repos of the projects this person sees, as owner/name, for the add box. */
function useKnownRepos(): string[] {
  const projects = useQuery(q.projects()).data ?? []
  const repos = useQueries({ queries: projects.map((p) => q.repos(p.key)) })
  return useMemo(() => {
    const all = new Set<string>()
    for (const r of repos) for (const x of r.data ?? []) all.add(x.repo.toLowerCase())
    return [...all].sort()
  }, [repos])
}

export function ClaudeCodeTab({ me }: { me: User }) {
  const savePrefs = useSavePrefs()
  const prefs: SessionPrefs = me.prefs.sessions ?? { all: {} }
  const [added, setAdded] = useState<string[]>([])
  const [repo, setRepo] = useState('')
  const known = useKnownRepos()
  const saved = Object.keys(prefs.repos ?? {}).sort()
  const shown = [...saved, ...added.filter((r) => !saved.includes(r))]
  const save = (next: SessionPrefs) => savePrefs.mutate({ sessions: next }, { onError: toastError })
  const saveRepo = (name: string, s: SessionSettings) => {
    const repos = { ...prefs.repos }
    if (Object.keys(s).length) repos[name] = s
    else delete repos[name]
    save({ ...prefs, repos })
  }
  const name = repo.trim().toLowerCase()
  const addable = repoRE.test(name) && !shown.includes(name)
  const add = () => {
    if (!addable) return
    setAdded((a) => [...a, name])
    setRepo('')
  }
  return (
    <>
      <Section
        title="All sessions"
        help={
          <>
            Claude Code settings for the sessions Waypoint starts for you: autopilot&apos;s,{' '}
            <code>wp start</code>, follow-ups and Start session, and <code>wp fanout</code>. Your
            runner applies them on its machine. Default leaves the session as it would be.
          </>
        }
      >
        <SettingsFields
          value={prefs.all}
          unset="Default"
          onChange={(all) => save({ ...prefs, all })}
        />
      </Section>
      <Section
        title="Repos"
        help="A repo's settings replace the ones above, one by one, for sessions in a checkout of that repo."
      >
        <div className="flex flex-col gap-2 p-4 sm:flex-row sm:items-end">
          <Field label="Add a repo" className="flex-1">
            <Input
              value={repo}
              list="claude-code-repos"
              placeholder="owner/name"
              onChange={(e) => setRepo(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') add()
              }}
            />
          </Field>
          <datalist id="claude-code-repos">
            {known
              .filter((r) => !shown.includes(r))
              .map((r) => (
                <option key={r} value={r} />
              ))}
          </datalist>
          <Button icon={<Plus />} disabled={!addable} onClick={add}>
            Add
          </Button>
        </div>
        {shown.length === 0 && (
          <p className="px-4 pb-4 text-[13px] text-subtle">No repo has settings of its own.</p>
        )}
      </Section>
      {shown.map((r) => (
        <Section
          key={r}
          title={<span className="font-mono">{r}</span>}
          actions={
            <IconButton
              icon={<X />}
              label={`Remove ${r}`}
              onClick={() => {
                setAdded((a) => a.filter((x) => x !== r))
                if (saved.includes(r)) saveRepo(r, {})
              }}
            />
          }
        >
          <SettingsFields
            value={prefs.repos?.[r] ?? {}}
            unset="As all sessions"
            onChange={(s) => saveRepo(r, s)}
          />
        </Section>
      ))}
    </>
  )
}
