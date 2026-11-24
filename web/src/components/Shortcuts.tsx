import { useEffect, useState } from 'react'
import { useHotkey } from '@/lib/hooks'
import { Dialog } from '@/components/ui/dialog'
import { Kbd } from '@/components/ui/misc'

const GROUPS: [string, [string[], string][]][] = [
  [
    'Anywhere',
    [
      [['⌘', 'K'], 'Search and commands'],
      [['/'], 'Search and commands'],
      [['C'], 'New issue'],
      [['D'], "New draft (agents can't see it)"],
      [['['], 'Collapse the sidebar'],
      [['?'], 'This list'],
    ],
  ],
  [
    'Board and list',
    [
      [['J'], 'Next issue (or ↓)'],
      [['K'], 'Previous issue (or ↑)'],
      [['H', 'L'], 'Previous / next column (or ← →)'],
      [['↵'], 'Open the issue (or O)'],
      [['X'], 'Select it, in the list'],
      [['F'], 'Filter'],
      [['Esc'], 'Leave the list'],
    ],
  ],
  [
    'Writing',
    [
      [['@'], 'Mention someone'],
      [['⌘', '↵'], 'Save or send'],
      [['Esc'], 'Cancel the edit'],
    ],
  ],
]

/** Opens the sheet from elsewhere (the command palette). */
export const openShortcuts = () => window.dispatchEvent(new Event('wp:shortcuts'))

export function Shortcuts() {
  const [open, setOpen] = useState(false)
  useHotkey('?', () => setOpen(true))
  useEffect(() => {
    const show = () => setOpen(true)
    window.addEventListener('wp:shortcuts', show)
    return () => window.removeEventListener('wp:shortcuts', show)
  }, [])
  return (
    <Dialog open={open} onClose={() => setOpen(false)} title="Keyboard shortcuts">
      <div className="grid gap-5 p-4 sm:grid-cols-2">
        {GROUPS.map(([title, rows]) => (
          <section key={title}>
            <h3 className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-subtle">
              {title}
            </h3>
            <dl className="space-y-1">
              {rows.map(([keys, what]) => (
                <div key={what + keys.join()} className="flex items-center gap-3 text-[13px]">
                  <dt className="flex w-16 shrink-0 gap-1">
                    {keys.map((k) => (
                      <Kbd key={k}>{k}</Kbd>
                    ))}
                  </dt>
                  <dd className="text-muted">{what}</dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Dialog>
  )
}
