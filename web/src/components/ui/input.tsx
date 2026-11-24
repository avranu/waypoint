import {
  Children,
  Fragment,
  forwardRef,
  isValidElement,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ChangeEvent,
  type InputHTMLAttributes,
  type ReactNode,
  type TextareaHTMLAttributes,
} from 'react'
import clsx from 'clsx'
import { Check, ChevronDown } from 'lucide-react'
import { Popover, usePopover } from './popover'
import { Tip } from './help'

/** The look of text fields; full width unless the caller sets a width. */
const fieldLook =
  'rounded-lg border border-line bg-panel px-3 text-sm text-fg placeholder:text-subtle transition-colors focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/20 disabled:opacity-60'
export const inputClass = 'w-full ' + fieldLook

/** inputClass, minus w-full when `className` already gives a width. */
function fieldClass(className?: string) {
  return /(^|\s)!?([a-z]+:)?(w-|flex-1|basis-)/.test(className ?? '') ? fieldLook : inputClass
}

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  function Input({ className, ...rest }, ref) {
    return <input ref={ref} className={clsx(fieldClass(className), 'h-9', className)} {...rest} />
  },
)

export const Textarea = forwardRef<
  HTMLTextAreaElement,
  TextareaHTMLAttributes<HTMLTextAreaElement> & { autoGrow?: boolean }
>(function Textarea({ className, autoGrow, onInput, ...rest }, ref) {
  const inner = useRef<HTMLTextAreaElement | null>(null)
  const grow = () => {
    const el = inner.current
    if (el && autoGrow) {
      const saved: [Element, number][] = []
      for (let p = el.parentElement; p; p = p.parentElement) {
        if (p.scrollTop) saved.push([p, p.scrollTop])
      }
      const root = document.scrollingElement
      if (root?.scrollTop) saved.push([root, root.scrollTop])
      el.style.height = 'auto'
      el.style.height = el.scrollHeight + 2 + 'px'
      for (const [p, top] of saved) p.scrollTop = top
    }
  }
  useEffect(grow, [rest.value, autoGrow])
  return (
    <textarea
      ref={(el) => {
        inner.current = el
        if (typeof ref === 'function') ref(el)
        else if (ref) ref.current = el
      }}
      onInput={(e) => {
        grow()
        onInput?.(e)
      }}
      className={clsx(fieldClass(className), 'py-2 leading-relaxed', className)}
      {...rest}
    />
  )
})

/**
 * A labelled control. `hint` is background that waits behind a (?) next to
 * the label; `note` is something to see right away (a warning, say).
 */
export function Field({
  label,
  hint,
  note,
  error,
  children,
  className,
}: {
  label: ReactNode
  hint?: ReactNode
  note?: ReactNode
  error?: string
  children: ReactNode
  className?: string
}) {
  const labelClass = 'text-[13px] font-medium leading-5'
  const outer = !hint && className
  const field = (
    <div className={clsx('space-y-1.5', outer)}>
      <label className="block space-y-1.5">
        <span className={clsx('block text-fg', labelClass)}>{label}</span>
        {children}
      </label>
      {error ? (
        <span className="block text-xs text-danger">{error}</span>
      ) : note ? (
        <span className="block text-xs text-subtle">{note}</span>
      ) : null}
    </div>
  )
  if (!hint) return field
  return (
    <div className={clsx('relative', className)}>
      {field}
      <div className={clsx('pointer-events-none absolute inset-x-0 top-0', labelClass)}>
        <span aria-hidden className="invisible">
          {label}
        </span>{' '}
        <span className="pointer-events-auto">
          <Tip>{hint}</Tip>
        </span>
      </div>
    </div>
  )
}

export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label?: string
  disabled?: boolean
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      disabled={disabled}
      onClick={() => onChange(!checked)}
      className={clsx(
        'relative inline-flex h-5 w-9 shrink-0 items-center rounded-full transition-colors disabled:opacity-50',
        checked ? 'bg-accent' : 'bg-line-strong',
      )}
    >
      <span
        className={clsx(
          'inline-block size-4 rounded-full bg-white shadow transition-transform',
          checked ? 'translate-x-[18px]' : 'translate-x-0.5',
        )}
      />
    </button>
  )
}

interface SelectOption {
  value: string
  label: string
  disabled?: boolean
  group?: string
}

/** Text of a React node, for option labels like {name} ({kind}). */
function nodeText(n: ReactNode): string {
  if (n == null || typeof n === 'boolean') return ''
  if (typeof n === 'string' || typeof n === 'number') return String(n)
  if (Array.isArray(n)) return n.map(nodeText).join('')
  if (isValidElement<{ children?: ReactNode }>(n)) return nodeText(n.props.children)
  return ''
}

/**
 * The <option>s (and <optgroup> labels) among a select's children. Only
 * those and fragments are read: options rendered by a component of their
 * own aren't seen, so build them with a plain function call instead.
 */
function readOptions(children: ReactNode, group?: string, out: SelectOption[] = []) {
  Children.forEach(children, (c) => {
    if (!isValidElement<Record<string, unknown>>(c)) return
    const p = c.props
    if (c.type === Fragment) readOptions(p.children as ReactNode, group, out)
    else if (c.type === 'optgroup') readOptions(p.children as ReactNode, String(p.label ?? ''), out)
    else if (c.type === 'option') {
      const label = nodeText(p.children as ReactNode)
      out.push({
        value: p.value === undefined ? label : String(p.value),
        label,
        disabled: !!p.disabled,
        group,
      })
    }
  })
  return out
}

/**
 * A dropdown with the API of a native <select> (`value`, <option> children,
 * `onChange` reading `e.target.value`), drawn as one of our popovers: it
 * opens instantly, looks the same everywhere, and is a sheet on phones.
 */
export function Select({
  className,
  children,
  value,
  onChange,
  disabled,
  title,
  id,
  'aria-label': ariaLabel,
  variant = 'field',
  icon,
}: React.SelectHTMLAttributes<HTMLSelectElement> & {
  /** `inline`: a chip that sits in a sentence ("When [status changes ▾]"). */
  variant?: 'field' | 'inline'
  /** Shown before the chosen option. */
  icon?: ReactNode
}) {
  const options = useMemo(() => readOptions(children), [children])
  const pop = usePopover()
  const listRef = useRef<HTMLDivElement>(null)
  const current = options.find((o) => o.value === String(value ?? '')) ?? options[0]
  const [active, setActive] = useState(0)
  const enabled = options.filter((o) => !o.disabled)
  const choose = (o: SelectOption) => {
    pop.close()
    pop.anchor.current?.focus()
    if (o.value === current?.value) return
    const target = { value: o.value }
    onChange?.({ target, currentTarget: target } as unknown as ChangeEvent<HTMLSelectElement>)
  }
  const open = () => {
    if (disabled) return
    setActive(Math.max(0, enabled.indexOf(current!)))
    pop.setOpen(true)
  }
  useEffect(() => {
    if (pop.open)
      listRef.current
        ?.querySelector<HTMLElement>(`[data-idx="${active}"]`)
        ?.scrollIntoView({ block: 'nearest' })
  }, [pop.open, active])
  const onKeyDown = (e: React.KeyboardEvent) => {
    if (!pop.open) {
      if (['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(e.key)) {
        e.preventDefault()
        open()
      }
      return
    }
    const last = enabled.length - 1
    const to =
      e.key === 'ArrowDown'
        ? Math.min(active + 1, last)
        : e.key === 'ArrowUp'
          ? Math.max(active - 1, 0)
          : e.key === 'Home'
            ? 0
            : e.key === 'End'
              ? last
              : null
    if (to !== null) {
      e.preventDefault()
      setActive(to)
    } else if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      if (enabled[active]) choose(enabled[active])
    } else if (e.key === 'Tab') {
      pop.close()
    } else if (e.key.length === 1) {
      const k = e.key.toLowerCase()
      const n = enabled.findIndex((o, i) => i > active && o.label.toLowerCase().startsWith(k))
      const m = n >= 0 ? n : enabled.findIndex((o) => o.label.toLowerCase().startsWith(k))
      if (m >= 0) setActive(m)
    }
  }
  const listId = useId()
  let lastGroup: string | undefined
  return (
    <>
      <button
        ref={pop.anchor}
        id={id}
        type="button"
        role="combobox"
        aria-haspopup="listbox"
        aria-expanded={pop.open}
        aria-controls={pop.open ? listId : undefined}
        aria-activedescendant={pop.open && enabled[active] ? `${listId}-${active}` : undefined}
        aria-label={ariaLabel}
        title={title}
        disabled={disabled}
        onClick={() => (pop.open ? pop.close() : open())}
        onKeyDown={onKeyDown}
        className={clsx(
          variant === 'inline'
            ? 'inline-flex h-7 max-w-full items-center gap-1 rounded-md bg-accent-soft px-2 text-left text-[13px] font-medium text-accent transition-colors hover:bg-accent/20 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 disabled:opacity-60'
            : [fieldClass(className), 'flex h-9 items-center gap-2 text-left'],
          'disabled:cursor-not-allowed',
          className,
        )}
      >
        {icon}
        <span className={clsx('min-w-0 truncate', variant === 'field' && 'flex-1')}>
          {current?.label ?? ''}
        </span>
        <ChevronDown
          className={clsx(
            'shrink-0',
            variant === 'inline' ? '-mr-0.5 size-3.5 opacity-70' : '-mr-1 size-4 text-subtle',
          )}
        />
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <div
          ref={listRef}
          id={listId}
          role="listbox"
          aria-label={ariaLabel}
          tabIndex={-1}
          onKeyDown={onKeyDown}
          className="flex max-h-80 flex-col outline-none"
        >
          {options.map((o) => {
            const idx = enabled.indexOf(o)
            const header = o.group && o.group !== lastGroup ? o.group : null
            lastGroup = o.group
            const selected = o.value === current?.value
            return (
              <Fragment key={(o.group ?? '') + '\u0000' + o.value}>
                {header && (
                  <div className="px-2.5 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                    {header}
                  </div>
                )}
                <button
                  type="button"
                  role="option"
                  id={idx >= 0 ? `${listId}-${idx}` : undefined}
                  aria-selected={selected}
                  aria-disabled={o.disabled || undefined}
                  disabled={o.disabled}
                  data-idx={idx}
                  onMouseEnter={() => idx >= 0 && setActive(idx)}
                  onClick={() => choose(o)}
                  className={clsx(
                    'flex h-9 w-full shrink-0 items-center gap-2 rounded-lg px-2.5 text-left text-[13px] disabled:opacity-40 sm:h-8',
                    idx === active && 'bg-hover',
                  )}
                >
                  <span className="flex-1 truncate">{o.label}</span>
                  <Check
                    className={clsx('size-3.5 text-accent', selected ? 'opacity-100' : 'opacity-0')}
                  />
                </button>
              </Fragment>
            )
          })}
        </div>
      </Popover>
    </>
  )
}
