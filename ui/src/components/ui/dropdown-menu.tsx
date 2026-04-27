'use client'
import * as React from 'react'
import { cn } from '../../lib/utils'

// Lightweight popover used for the Connect dropdown + account menu.
// Not full Radix — that would pull react-dropdown-menu and react-popover.
// One controlled state pair, click-outside dismiss, escape to close.

export function Popover({
  trigger,
  children,
  align = 'end',
  className,
}: {
  trigger: React.ReactNode
  children: React.ReactNode
  align?: 'start' | 'end' | 'center'
  className?: string
}) {
  const [open, setOpen] = React.useState(false)
  const ref = React.useRef<HTMLDivElement>(null)

  React.useEffect(() => {
    if (!open) return
    function onDocClick(e: MouseEvent) {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onDocClick)
    document.addEventListener('keydown', onKey)
    return () => {
      document.removeEventListener('mousedown', onDocClick)
      document.removeEventListener('keydown', onKey)
    }
  }, [open])

  const alignment =
    align === 'start' ? 'left-0' : align === 'center' ? 'left-1/2 -translate-x-1/2' : 'right-0'

  return (
    <div ref={ref} className="relative inline-block">
      <span onClick={() => setOpen((o) => !o)} className="contents">
        {trigger}
      </span>
      {open && (
        <div
          className={cn(
            'absolute z-50 mt-2 min-w-[12rem] rounded-md border border-border bg-popover p-1 text-popover-foreground shadow-lg',
            alignment,
            className
          )}
        >
          {children}
        </div>
      )}
    </div>
  )
}

export function PopoverItem({
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      type="button"
      className={cn(
        'relative flex w-full cursor-default select-none items-center gap-2 rounded-sm px-2 py-1.5 text-sm outline-none transition-colors hover:bg-accent hover:text-accent-foreground focus:bg-accent focus:text-accent-foreground',
        className
      )}
      {...props}
    />
  )
}
