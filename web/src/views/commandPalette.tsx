// ⌘K command palette (Priority 6) — lightweight quick-nav, zero dependencies.
//
// Opens with Ctrl+K / Cmd+K (or by clicking the header searchbox), lists
// navigation + write actions composed by App.tsx (write actions are only
// included when the authed role has the matching permission, so a viewer
// never sees create/execute/decide entries), filters as you type and is
// fully keyboard driven: ↑/↓ move, ↵ runs, Esc closes.
//
// Implementation notes:
// - State is reset on CLOSE (inside the single close() handler), so reopening
//   always starts fresh without a setState-in-effect (react-hooks v7 rule).
// - The active row is clamped as derived state during render, not via effect.

import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

export type PaletteAction = {
  id: string
  label: string
  hint?: string
  keywords?: string
  run: () => void
}

type CommandPaletteProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  actions: PaletteAction[]
}

function filterActions(actions: PaletteAction[], query: string): PaletteAction[] {
  const needle = query.trim().toLowerCase()
  if (!needle) return actions
  return actions.filter((action) =>
    `${action.label} ${action.hint ?? ''} ${action.keywords ?? ''}`.toLowerCase().includes(needle),
  )
}

export function CommandPalette({ open, onOpenChange, actions }: CommandPaletteProps) {
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(0)
  const inputRef = useRef<HTMLInputElement>(null)
  // Latest onOpenChange without re-registering the global listener.
  const onOpenChangeRef = useRef(onOpenChange)

  useEffect(() => {
    onOpenChangeRef.current = onOpenChange
  }, [onOpenChange])

  // Single close path: resets input state so the next open starts fresh.
  const close = useCallback(() => {
    setQuery('')
    setActiveIndex(0)
    onOpenChangeRef.current(false)
  }, [])

  // Global shortcut: Ctrl/Cmd+K toggles the palette from anywhere.
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
        event.preventDefault()
        if (open) close()
        else onOpenChangeRef.current(true)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [open, close])

  // Focus the input whenever the palette opens (pure DOM side effect).
  useEffect(() => {
    if (open) inputRef.current?.focus()
  }, [open])

  const filtered = useMemo(() => filterActions(actions, query), [actions, query])
  // Derived clamp: the highlight stays inside the filtered list without state.
  const active = Math.min(activeIndex, Math.max(filtered.length - 1, 0))

  useEffect(() => {
    if (!open) return
    document.getElementById(`palette-option-${active}`)?.scrollIntoView({ block: 'nearest' })
  }, [active, open])

  const runAction = (action: PaletteAction) => {
    close()
    action.run()
  }

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') {
      event.preventDefault()
      setActiveIndex((current) => (filtered.length === 0 ? 0 : (current + 1) % filtered.length))
    } else if (event.key === 'ArrowUp') {
      event.preventDefault()
      setActiveIndex((current) => (filtered.length === 0 ? 0 : (current - 1 + filtered.length) % filtered.length))
    } else if (event.key === 'Enter') {
      event.preventDefault()
      const action = filtered[active]
      if (action) runAction(action)
    } else if (event.key === 'Escape') {
      event.preventDefault()
      close()
    }
  }

  if (!open) return null

  return (
    <div
      className="palette-overlay"
      role="presentation"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget) close()
      }}
    >
      <div className="palette-panel" role="dialog" aria-modal="true" aria-label="Command palette">
        <div className="palette-input">
          <span className="search-icon" aria-hidden="true">
            ⌕
          </span>
          <input
            ref={inputRef}
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={onInputKeyDown}
            placeholder="Type a command or search…"
            aria-label="Search commands"
            aria-controls="palette-listbox"
            aria-activedescendant={filtered[active] ? `palette-option-${active}` : undefined}
            spellCheck={false}
            autoComplete="off"
          />
        </div>

        <div className="palette-list" id="palette-listbox" role="listbox" aria-label="Commands">
          {filtered.length === 0 ? (
            <div className="palette-empty">No matching commands — try “agents”, “approvals” or “usage”.</div>
          ) : (
            filtered.map((action, index) => (
              <button
                key={action.id}
                id={`palette-option-${index}`}
                type="button"
                role="option"
                aria-selected={index === active}
                className={index === active ? 'palette-option active' : 'palette-option'}
                onMouseMove={() => setActiveIndex(index)}
                onClick={() => runAction(action)}
              >
                <span>{action.label}</span>
                {action.hint ? <span className="palette-hint">{action.hint}</span> : null}
              </button>
            ))
          )}
        </div>

        <div className="palette-footer">
          <span>
            <kbd>↑</kbd>
            <kbd>↓</kbd> navigate
          </span>
          <span>
            <kbd>↵</kbd> select
          </span>
          <span>
            <kbd>esc</kbd> close
          </span>
        </div>
      </div>
    </div>
  )
}
