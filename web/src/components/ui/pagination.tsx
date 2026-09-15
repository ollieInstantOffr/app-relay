// Pagination for client-side tables: "1–25 of 80", page x of y, previous / next.
import { Button } from './controls'
import { Icon } from './Icon'

export function paginate<T>(items: T[], page: number, pageSize: number): T[] {
  return items.slice((page - 1) * pageSize, page * pageSize)
}

export function pageCount(total: number, pageSize: number): number {
  return Math.max(1, Math.ceil(total / pageSize))
}

export function Pagination({ page, pageSize, total, onPage, label = 'items' }: {
  page: number
  pageSize: number
  total: number
  onPage: (page: number) => void
  label?: string
}) {
  const pages = pageCount(total, pageSize)
  if (total <= pageSize) return null
  const from = (page - 1) * pageSize + 1
  const to = Math.min(page * pageSize, total)
  return (
    <nav className="pagination" aria-label="Pagination">
      <span className="pagination-range">
        {from}–{to} of {total} {label}
      </span>
      <div className="spacer" />
      <span className="pagination-page">
        Page {page} of {pages}
      </span>
      <Button size="sm" variant="ghost" disabled={page <= 1} onClick={() => onPage(page - 1)} aria-label="Previous page" title="Previous page">
        <Icon name="chevron" size={14} style={{ transform: 'rotate(90deg)' }} />
      </Button>
      <Button size="sm" variant="ghost" disabled={page >= pages} onClick={() => onPage(page + 1)} aria-label="Next page" title="Next page">
        <Icon name="chevron" size={14} style={{ transform: 'rotate(-90deg)' }} />
      </Button>
    </nav>
  )
}
