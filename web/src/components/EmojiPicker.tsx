// Picks a project's icon: a grid of common emoji, plus a field that takes
// any emoji typed or pasted (the last whole emoji wins, so 👩‍💻 and flags
// stay intact).

import { useState } from 'react'
import clsx from 'clsx'
import { SmilePlus } from 'lucide-react'
import { Popover, usePopover } from '@/components/ui/popover'

const EMOJI = [
  '🚀',
  '📦',
  '🛠️',
  '🐛',
  '✨',
  '📱',
  '💻',
  '🌐',
  '🔒',
  '📊',
  '📈',
  '🧪',
  '🎨',
  '📝',
  '📚',
  '🗂️',
  '📅',
  '🏠',
  '🌱',
  '🔥',
  '⚡',
  '🎯',
  '🧭',
  '🏗️',
  '⚙️',
  '🤖',
  '🧠',
  '💡',
  '📣',
  '🎮',
  '🎵',
  '📷',
  '🛒',
  '💰',
  '✈️',
  '🚗',
  '🍳',
  '🏃',
  '❤️',
  '⭐',
  '🌙',
  '☀️',
  '🌊',
  '🍀',
  '🐳',
  '🦊',
  '🐙',
  '🧩',
]

/** The last whole emoji (grapheme) in some text, or ''. */
export function lastEmoji(text: string): string {
  const t = text.trim()
  if (!t) return ''
  if (typeof Intl !== 'undefined' && 'Segmenter' in Intl) {
    const parts = [...new Intl.Segmenter(undefined, { granularity: 'grapheme' }).segment(t)]
    return parts.at(-1)?.segment ?? ''
  }
  return [...t].slice(-1).join('')
}

export function EmojiPicker({
  value,
  onChange,
  label = 'Icon',
  disabled,
  className,
}: {
  value: string
  onChange: (emoji: string) => void
  label?: string
  disabled?: boolean
  className?: string
}) {
  const pop = usePopover()
  const [typed, setTyped] = useState('')
  const pick = (e: string) => {
    onChange(e)
    setTyped('')
    pop.close()
  }
  return (
    <>
      <button
        ref={pop.anchor}
        type="button"
        aria-label={value ? `${label}: ${value}` : `Choose ${label.toLowerCase()}`}
        aria-expanded={pop.open}
        disabled={disabled}
        onClick={pop.toggle}
        className={clsx(
          'flex h-9 w-12 items-center justify-center rounded-lg border border-line bg-panel text-lg transition-colors hover:bg-hover disabled:opacity-60',
          className,
        )}
      >
        {value || <SmilePlus className="size-4 text-subtle" />}
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <div className="w-full p-1 sm:w-72">
          <div className="grid grid-cols-8 gap-0.5">
            {EMOJI.map((e) => (
              <button
                key={e}
                type="button"
                aria-label={e}
                onClick={() => pick(e)}
                className={clsx(
                  'flex aspect-square items-center justify-center rounded-md text-xl hover:bg-hover',
                  e === value && 'bg-accent-soft',
                )}
              >
                {e}
              </button>
            ))}
          </div>
          <form
            className="mt-1.5 flex items-center gap-1.5 border-t border-line pt-1.5"
            onSubmit={(ev) => {
              // React bubbles this through the portal to any form around the picker.
              ev.preventDefault()
              ev.stopPropagation()
              const e = lastEmoji(typed)
              if (e) pick(e)
            }}
          >
            <input
              value={typed}
              onChange={(ev) => setTyped(ev.target.value)}
              onPaste={(ev) => {
                const e = lastEmoji(ev.clipboardData.getData('text'))
                if (e) {
                  ev.preventDefault()
                  pick(e)
                }
              }}
              placeholder="Or type / paste any emoji"
              aria-label="Any emoji"
              className="h-8 min-w-0 flex-1 rounded-md bg-transparent px-2 text-[13px] outline-none placeholder:text-subtle"
            />
            {typed.trim() && (
              <button
                type="submit"
                className="h-7 rounded-md px-2 text-xs text-accent hover:bg-hover"
              >
                Use
              </button>
            )}
            {value && !typed.trim() && (
              <button
                type="button"
                onClick={() => pick('')}
                className="h-7 rounded-md px-2 text-xs text-muted hover:bg-hover hover:text-fg"
              >
                Remove
              </button>
            )}
          </form>
        </div>
      </Popover>
    </>
  )
}
