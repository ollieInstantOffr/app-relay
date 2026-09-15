// Table helpers: pages sized to fit the browser window, pagination, search box
// and toolbar. Every data table uses these so they behave the same.
import { useEffect, useLayoutEffect, useRef, useState, type DependencyList, type ReactNode, type RefObject } from 'react'
import { Button, Input } from './controls'
import { Icon } from './Icon'

export function paginate<T>(items: T[], page: number, pageSize: number): T[] {
  return items.slice((page - 1) * pageSize, page * pageSize)
}

export function pageCount(total: number, pageSize: number): number {
  return Math.max(1, Math.ceil(total / Math.max(1, pageSize)))
}

/** True when any field contains the (lowercase-trimmed) search text. */
export function matchesSearch(search: string, ...fields: (string | number | null | undefined | false)[]): boolean {
  const needle = search.trim().toLowerCase()
  if (!needle) return true
  return fields.some((f) => f !== null && f !== undefined && f !== false && String(f).toLowerCase().includes(needle))
}

/**
 * How many table rows fit between the top of `ref` (a table card) and the
 * bottom of the window. `reserve` is the space needed below the rows: the
 * pager, page padding and anything rendered under the table. Recalculates on
 * window resize and layout changes; row height is measured from the table and
 * only ever grows, so page sizes don't flip between pages of uneven rows.
 */
export function useFitRows(
  ref: RefObject<HTMLElement | null>,
  { reserve = 72, min = 5, max = 500, rowHeight = 45 }: { reserve?: number; min?: number; max?: number; rowHeight?: number } = {},
): number {
  const measured = useRef(rowHeight)
  const [rows, setRows] = useState(() =>
    typeof window === 'undefined' ? min : Math.min(max, Math.max(min, Math.floor((window.innerHeight - 280) / rowHeight))),
  )

  useLayoutEffect(() => {
    const calc = () => {
      const el = ref.current
      if (!el) return
      el.querySelectorAll<HTMLElement>('tbody tr').forEach((tr) => {
        if (tr.offsetHeight > measured.current && !tr.dataset.emptyRow) measured.current = tr.offsetHeight
      })
      const top = el.getBoundingClientRect().top
      const head = el.querySelector('thead')?.getBoundingClientRect().height ?? 0
      const available = window.innerHeight - top - head - reserve
      const n = Math.min(max, Math.max(min, Math.floor(available / measured.current)))
      setRows((prev) => (prev === n ? prev : n))
    }
    calc()
    window.addEventListener('resize', calc)
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(calc)
    const container = ref.current?.closest('.page') ?? ref.current?.parentElement
    if (ro && container) ro.observe(container)
    return () => {
      window.removeEventListener('resize', calc)
      ro?.disconnect()
    }
  })

  return rows
}

/** Client-side pagination; goes back to page 1 when resetDeps change. */
export function usePagination<T>(items: T[], pageSize: number, resetDeps: DependencyList = []) {
  const [page, setPage] = useState(1)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => setPage(1), resetDeps)
  const pages = pageCount(items.length, pageSize)
  const current = Math.min(page, pages)
  return { page: current, pages, setPage, pageSize, total: items.length, rows: paginate(items, current, pageSize) }
}

export function Pagination({ page, pageSize, total, onPage, label = 'items' }: {
  page: number
  pageSize: number
  total: number
  onPage: (page: number) => void
  label?: string
}) {
  if (total === 0) return null
  const pages = pageCount(total, pageSize)
  const from = (page - 1) * pageSize + 1
  const to = Math.min(page * pageSize, total)
  return (
    <nav className="pagination" aria-label="Pagination">
      <span className="pagination-range">
        {from}–{to} of {total} {label}
      </span>
      <div className="spacer" />
      {pages > 1 && (
        <span className="pagination-page">
          Page {page} of {pages}
        </span>
      )}
      <Button size="sm" variant="ghost" disabled={page <= 1} onClick={() => onPage(page - 1)} aria-label="Previous page" title="Previous page">
        <Icon name="chevron" size={14} style={{ transform: 'rotate(90deg)' }} />
      </Button>
      <Button size="sm" variant="ghost" disabled={page >= pages} onClick={() => onPage(page + 1)} aria-label="Next page" title="Next page">
        <Icon name="chevron" size={14} style={{ transform: 'rotate(-90deg)' }} />
      </Button>
    </nav>
  )
}

/** Search box with a clear button; Escape clears it. */
export function SearchInput({ value, onChange, placeholder = 'Search', label, width = 240 }: {
  value: string
  onChange: (value: string) => void
  placeholder?: string
  label?: string
  width?: number
}) {
  return (
    <div className="search-input" style={{ width }}>
      <Icon name="search" size={14} />
      <Input
        inputSize="sm"
        value={value}
        placeholder={placeholder}
        aria-label={label ?? placeholder}
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            onChange('')
            e.currentTarget.blur()
          }
        }}
      />
      {value && (
        <button type="button" className="search-input-clear" onClick={() => onChange('')} aria-label="Clear search">
          <Icon name="close" size={12} />
        </button>
      )}
    </div>
  )
}

/** Search and filters on the left, actions on the right; wraps on narrow windows. */
export function TableToolbar({ children, actions }: { children?: ReactNode; actions?: ReactNode }) {
  return (
    <div className="table-toolbar">
      {children}
      <div className="spacer" />
      {actions}
    </div>
  )
}
