// Autopilot settings in one dialog (opened from the gear at the top of Agents →
// Autopilot, or a project's own gear): use what's left, your defaults, and
// each project's limits. "Use what's left" also has a switch on the page.

import { useState } from 'react'
import clsx from 'clsx'
import { Hourglass } from 'lucide-react'
import { useSetAutopilot } from '@/lib/queries'
import type { AutopilotLimits, AutopilotOverview, SpendPlan } from '@/lib/autopilot'
import { relativeTime, plural } from '@/lib/format'
import { AppLink } from '@/components/AppLink'
import { Dialog } from '@/components/ui/dialog'
import { SavedMark, useAutosave } from '@/components/ui/autosave'
import { Tip } from '@/components/ui/help'
import { Field, Input, Switch } from '@/components/ui/input'
import { toastError } from '@/components/ui/toast'
import { LimitsForm, saveFailed } from './LimitsForm'

/** The switch for use what's left: on the page (compact) and in the dialog. */
export function SpendSwitch({
  plan,
  effective,
  label,
}: {
  plan?: SpendPlan
  effective: AutopilotLimits
  label: string
}) {
  const set = useSetAutopilot()
  const on = !!(plan?.on ?? effective.spend_left)
  return (
    <Switch
      checked={on}
      disabled={set.isPending}
      label={label}
      onChange={(v) =>
        void set.mutateAsync({ limits: { spend_left: v }, merge: true }).catch((e) => toastError(e))
      }
    />
  )
}

/** The page's own "use what's left" toggle: the icon, a short name, the switch. */
export function SpendToggle({ data }: { data: AutopilotOverview }) {
  const plan = data.spend
  return (
    <div
      className="inline-flex h-8 items-center gap-2 rounded-lg border border-line bg-panel px-2.5 text-[13px]"
      title="Use what’s left: spend the week’s quota before it resets, over the week’s last hours"
    >
      <Hourglass className="size-3.5 text-muted" />
      <span className="hidden sm:inline">Use what’s left</span>
      <SpendSwitch plan={plan} effective={data.effective} label="Use what’s left" />
    </div>
  )
}

function SpendBlock({ data }: { data: AutopilotOverview }) {
  const { spend: plan, defaults, effective, waypoint } = data
  const set = useSetAutopilot()
  const on = !!(plan?.on ?? effective.spend_left)
  const saved = defaults.spend_at_once?.toString() ?? ''
  const [cap, setCap] = useState(saved)
  const [seen, setSeen] = useState(saved)
  if (seen !== saved) {
    setSeen(saved)
    setCap(saved)
  }
  const n = Number(cap)
  const capError =
    cap.trim() !== '' && (!Number.isInteger(n) || n < 1 || n > 20)
      ? 'Sessions at once must be a whole number from 1 to 20'
      : undefined
  const hours = effective.week_end_hours ?? 0
  const { done, flush } = useAutosave(
    cap.trim() || (waypoint.spend_at_once?.toString() ?? ''),
    saved || (waypoint.spend_at_once?.toString() ?? ''),
    !capError,
    null,
    () =>
      set
        .mutateAsync({
          limits: { spend_at_once: cap.trim() === '' ? waypoint.spend_at_once : n },
          merge: true,
        })
        .catch(saveFailed),
  )
  return (
    <section className="space-y-3 border-b border-line px-4 py-3" data-testid="autopilot-spend">
      <div className="flex items-center gap-2">
        <Hourglass className="size-4 text-muted" />
        <h3 className="text-[13px] font-semibold">Use what’s left</h3>
        <Tip label="About use what’s left">
          Quota left when the week resets is lost. With this on, over the week’s last{' '}
          {plural(hours, 'hour')} autopilot starts sessions of its own past each project’s sessions
          at once, ready tasks first, one at a time, while the week is on pace to end below 100%.
          While you use Claude Code it leaves you 10 points, except in the last hour. Your hours,
          days, spacing and daily count don’t apply then; the hourly and quota limits and the loop
          guard still hold.
        </Tip>
        <span className="ml-auto">
          <SpendSwitch
            plan={plan}
            effective={effective}
            label="Use what’s left at the week’s end"
          />
        </span>
      </div>
      {on && plan?.why && (
        <p
          className={clsx('text-[13px] leading-snug', plan.start ? 'text-fg' : 'text-muted')}
          data-testid="autopilot-spend-why"
        >
          {plan.why.charAt(0).toUpperCase() + plan.why.slice(1)}.
        </p>
      )}
      {on && (plan?.started ?? []).length > 0 && (
        <ul className="space-y-0.5 text-xs text-subtle" aria-label="Started at the week’s end">
          {plan!.started.map((x) => (
            <li key={x.step} className="flex flex-wrap gap-x-1.5">
              {x.task ? (
                <AppLink to={`/issue/${x.task.key}`} className="hover:text-fg hover:underline">
                  {x.task.key} {x.task.title}
                </AppLink>
              ) : (
                <span>{x.project}</span>
              )}
              <span>· {x.state}</span>
              <span>· {relativeTime(x.started_at)}</span>
            </li>
          ))}
        </ul>
      )}
      {on && (
        <div className="flex flex-wrap items-end gap-2">
          <Field
            label="Sessions at once"
            hint="Across all your projects, while it uses what’s left."
            className="w-40"
          >
            <Input
              id="spend-at-once"
              type="number"
              inputMode="numeric"
              min={1}
              max={20}
              step="1"
              value={cap}
              placeholder={waypoint.spend_at_once?.toString() ?? ''}
              onChange={(e) => setCap(e.target.value)}
              onBlur={flush}
              onKeyDown={(e) => e.key === 'Enter' && flush()}
              className="tabular-nums"
            />
          </Field>
          <SavedMark show={done} />
          {capError && <p className="w-full text-xs text-danger">{capError}</p>}
        </div>
      )}
    </section>
  )
}

/** '' is your defaults; else a project key. */
export function SettingsDialog({
  open,
  onClose,
  data,
  scope,
  onScope,
}: {
  open: boolean
  onClose: () => void
  data: AutopilotOverview
  scope: string
  onScope: (s: string) => void
}) {
  const set = useSetAutopilot()
  const project = scope ? data.projects.find((p) => p.project_key === scope) : undefined
  const tabs = [
    { key: '', label: 'Defaults', hint: 'Limits for every project, unless a project sets its own' },
    ...data.projects
      .filter((p) => p.enabled || p.project_key === scope)
      .map((p) => ({
        key: p.project_key,
        label: p.project_key,
        hint: `${p.project_name}: its own limits`,
      })),
  ]
  return (
    <Dialog open={open} onClose={onClose} title="Autopilot settings" className="sm:!max-w-4xl">
      <SpendBlock data={data} />
      <div className="px-4 pt-3">
        <div role="tablist" aria-label="Limits for" className="flex gap-1 overflow-x-auto pb-1">
          {tabs.map((t) => (
            <button
              key={t.key || 'defaults'}
              type="button"
              role="tab"
              aria-selected={scope === t.key}
              title={t.hint}
              onClick={() => onScope(t.key)}
              className={clsx(
                'h-7 shrink-0 rounded-md px-2.5 text-[13px] font-medium',
                t.key && 'font-mono text-xs',
                scope === t.key
                  ? 'bg-accent-soft text-accent'
                  : 'text-muted hover:bg-hover hover:text-fg',
              )}
            >
              {t.label}
            </button>
          ))}
        </div>
      </div>
      <div className="p-4">
        {scope === '' ? (
          <LimitsForm
            key="defaults"
            idPrefix="defaults"
            value={data.defaults}
            inherit={data.waypoint}
            saving={set.isPending}
            onSave={(limits) =>
              set
                .mutateAsync({ limits })
                .then((o) => o.defaults)
                .catch(saveFailed)
            }
            onReset={() => set.mutate({ limits: {}, reset: true }, { onError: toastError })}
          />
        ) : project ? (
          <LimitsForm
            key={project.project_key}
            idPrefix={`p-${project.project_key}`}
            value={project.limits}
            inherit={data.effective}
            saving={set.isPending}
            onSave={(limits) =>
              set
                .mutateAsync({ project: project.project_key, limits })
                .then((o) => o.projects.find((x) => x.project_key === project.project_key)?.limits)
                .catch(saveFailed)
            }
            onReset={() =>
              set.mutate(
                { project: project.project_key, limits: {}, reset: true },
                { onError: toastError },
              )
            }
          />
        ) : null}
      </div>
    </Dialog>
  )
}
