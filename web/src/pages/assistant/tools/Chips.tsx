import { useId, useState } from 'react'
import clsx from 'clsx'
import { X } from 'lucide-react'

/**
 * A list of short strings typed as chips: Enter, comma or leaving the field
 * adds what's typed; Backspace in an empty field takes the last one back.
 */
export function Chips({
  value,
  onChange,
  label,
  placeholder,
  check,
  mono = true,
}: {
  value: string[]
  onChange: (v: string[]) => void
  label: string
  placeholder?: string
  /** an error for one entry, or undefined when it's fine */
  check?: (s: string) => string | undefined
  mono?: boolean
}) {
  const [text, setText] = useState('')
  const [error, setError] = useState('')
  const errId = useId()
  const add = () => {
    const parts = text
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean)
    if (!parts.length) return true
    for (const p of parts) {
      const e = check?.(p)
      if (e) {
        setError(`${p}: ${e}`)
        return false
      }
    }
    onChange([...value, ...parts.filter((p) => !value.includes(p))])
    setText('')
    setError('')
    return true
  }
  return (
    <div>
      <div
        className={clsx(
          'flex min-h-9 w-full flex-wrap items-center gap-1 rounded-lg border bg-panel px-1.5 py-1 transition-colors focus-within:border-accent focus-within:ring-2 focus-within:ring-accent/20',
          error ? 'border-danger' : 'border-line',
        )}
      >
        {value.map((v) => (
          <span
            key={v}
            className={clsx(
              'inline-flex h-6 max-w-full items-center gap-0.5 rounded-md bg-sunken pl-1.5 text-xs text-fg',
              mono && 'font-mono',
            )}
          >
            <span className="truncate">{v}</span>
            <button
              type="button"
              aria-label={`Remove ${v}`}
              onClick={() => onChange(value.filter((x) => x !== v))}
              className="flex size-6 shrink-0 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg"
            >
              <X className="size-3" />
            </button>
          </span>
        ))}
        <input
          aria-label={label}
          aria-invalid={!!error || undefined}
          aria-describedby={error ? errId : undefined}
          value={text}
          placeholder={value.length ? '' : placeholder}
          onChange={(e) => {
            setText(e.target.value)
            if (error) setError('')
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ',') {
              if (!text.trim()) return
              e.preventDefault()
              add()
            } else if (e.key === 'Backspace' && !text && value.length) {
              e.preventDefault()
              setText(value[value.length - 1])
              onChange(value.slice(0, -1))
            }
          }}
          onBlur={add}
          className={clsx(
            'h-7 min-w-[8rem] flex-1 bg-transparent px-1.5 text-sm text-fg placeholder:text-subtle focus:outline-none',
            mono && 'font-mono text-[13px]',
          )}
        />
      </div>
      {error && (
        <span id={errId} className="mt-1 block text-xs text-danger">
          {error}
        </span>
      )}
    </div>
  )
}
