import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from 'react'
import clsx from 'clsx'

type Variant = 'primary' | 'secondary' | 'ghost' | 'danger' | 'subtle'
type Size = 'xs' | 'sm' | 'md'

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  icon?: ReactNode
  loading?: boolean
}

const variants: Record<Variant, string> = {
  primary: 'bg-accent text-accent-fg hover:brightness-110 active:brightness-95 shadow-card',
  secondary: 'bg-panel text-fg border border-line hover:bg-hover shadow-card',
  ghost: 'text-muted hover:text-fg hover:bg-hover',
  subtle: 'bg-sunken text-fg hover:bg-hover',
  danger: 'bg-danger text-white hover:brightness-110',
}

const sizes: Record<Size, string> = {
  xs: 'h-6 px-2 text-xs gap-1 rounded-md',
  sm: 'h-7 px-2.5 text-[13px] gap-1.5 rounded-md',
  md: 'h-9 px-3.5 text-sm gap-2 rounded-lg',
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = 'secondary',
    size = 'md',
    icon,
    loading,
    className,
    children,
    disabled,
    type = 'button',
    ...rest
  },
  ref,
) {
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      className={clsx(
        'inline-flex select-none items-center justify-center whitespace-nowrap font-medium transition-[background,filter,color] disabled:pointer-events-none disabled:opacity-50',
        variants[variant],
        sizes[size],
        className,
      )}
      {...rest}
    >
      {loading ? <Spinner className="size-3.5" /> : icon}
      {children}
    </button>
  )
})

export const IconButton = forwardRef<HTMLButtonElement, ButtonProps & { label: string }>(
  function IconButton({ label, size = 'sm', className, variant = 'ghost', ...rest }, ref) {
    const dim = size === 'xs' ? 'size-6' : size === 'sm' ? 'size-7' : 'size-9'
    return (
      <Button
        ref={ref}
        aria-label={label}
        title={label}
        variant={variant}
        size={size}
        className={clsx(dim, '!px-0', className)}
        {...rest}
      />
    )
  },
)

/**
 * An on/off button for toolbars. The state is never spelled out in the label:
 * on is a filled accent chip, off is a plain muted icon, and `aria-pressed` carries it for
 * assistive tech. `label` names the thing (stable, so it reads as "label,
 * toggle button, pressed"); `hint` says what it does and shows with the state
 * in the tooltip. `text` is an optional short visible word (like "Agents") for
 * when the icon alone isn't enough; from below `textFrom` only the icon shows.
 */
export const ToggleButton = forwardRef<
  HTMLButtonElement,
  Omit<ButtonHTMLAttributes<HTMLButtonElement>, 'onChange' | 'title'> & {
    label: string
    pressed: boolean
    onPressedChange: (pressed: boolean) => void
    icon: ReactNode
    hint?: string
    text?: string
    /** Breakpoint from which `text` shows next to the icon (default: always). */
    textFrom?: 'sm' | 'md'
  }
>(function ToggleButton(
  { label, pressed, onPressedChange, icon, hint, text, textFrom, className, ...rest },
  ref,
) {
  return (
    <button
      ref={ref}
      type="button"
      aria-pressed={pressed}
      aria-label={label}
      title={`${label}: ${pressed ? 'on' : 'off'}${hint ? `. ${hint}` : ''} (click to turn ${pressed ? 'off' : 'on'})`}
      onClick={() => onPressedChange(!pressed)}
      className={clsx(
        'inline-flex h-7 shrink-0 select-none items-center justify-center gap-1.5 rounded-md text-xs font-medium transition-colors',
        text ? 'min-w-7 px-1.5' : 'size-7',
        pressed ? 'bg-accent-soft text-accent' : 'text-subtle hover:bg-hover hover:text-fg',
        className,
      )}
      {...rest}
    >
      {icon}
      {text && (
        <span
          className={clsx(
            textFrom === 'md' ? 'hidden md:inline' : textFrom === 'sm' ? 'hidden sm:inline' : '',
          )}
        >
          {text}
        </span>
      )}
    </button>
  )
})

export function Spinner({ className }: { className?: string }) {
  return (
    <svg
      className={clsx('animate-spin', className ?? 'size-4')}
      viewBox="0 0 24 24"
      fill="none"
      aria-hidden
    >
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeOpacity=".25" strokeWidth="3" />
      <path d="M21 12a9 9 0 0 0-9-9" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
    </svg>
  )
}
