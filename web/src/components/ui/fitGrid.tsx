// Window-fitting pagination for lists that aren't <table>s: card grids, div
// rows and lists inside scrolling settings pages. Pairs with usePagination and
// Pagination from ./pagination (useFitRows there only measures table rows).
import { useLayoutEffect, useRef, useState, type ComponentProps, type RefObject } from 'react'
import { Button } from './controls'
import { Pagination } from './pagination'
import './fitGrid.css'

function scrollParent(el: HTMLElement): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const oy = getComputedStyle(p).overflowY
    if (oy === 'auto' || oy === 'scroll') return p
  }
  return null
}

/**
 * How many items of the list/grid `ref` fit between its top and the bottom of
 * the window. `ref` is the element whose direct children are the items (a CSS
 * grid, a flex column or plain block rows). Columns come from the computed grid
 * tracks; item height is the tallest item seen so far (only grows, so pages
 * don't flip size). `reserve` is the space needed below the items (pager, page
 * padding, anything rendered underneath).
 *
 * `viewport: true` is for lists inside a page that scrolls anyway (settings):
 * the count is what fits when the card is scrolled to the top of its scroll
 * container, so it doesn't change while scrolling.
 */
export function useFitGrid(
  ref: RefObject<HTMLElement | null>,
  { reserve = 72, min = 1, max = 500, itemHeight = 60, viewport = false }: {
    reserve?: number; min?: number; max?: number; itemHeight?: number; viewport?: boolean
  } = {},
): { rows: number; cols: number; pageSize: number } {
  const measured = useRef(itemHeight)
  const [fit, setFit] = useState(() => {
    const rows = typeof window === 'undefined' ? min : Math.min(max, Math.max(min, Math.floor((window.innerHeight - 280) / itemHeight)))
    return { rows, cols: 1 }
  })

  useLayoutEffect(() => {
    const calc = () => {
      const el = ref.current
      if (!el) return
      const style = getComputedStyle(el)
      const tracks = style.display.includes('grid') ? style.gridTemplateColumns.split(' ').filter((t) => t && t !== 'none').length : 1
      const cols = Math.max(1, tracks)
      const gap = parseFloat(style.rowGap) || 0
      for (const child of Array.from(el.children) as HTMLElement[]) {
        if (child.dataset.emptyRow || child.dataset.fitIgnore) continue
        if (child.offsetHeight > measured.current) measured.current = child.offsetHeight
      }
      let top = el.getBoundingClientRect().top
      if (viewport) {
        const scroller = scrollParent(el)
        if (scroller) {
          const card = (el.closest('.card') as HTMLElement | null) ?? el
          const chrome = el.getBoundingClientRect().top - card.getBoundingClientRect().top
          top = scroller.getBoundingClientRect().top + (parseFloat(getComputedStyle(scroller).paddingTop) || 0) + chrome
        }
      }
      const available = window.innerHeight - top - reserve
      const rows = Math.min(max, Math.max(min, Math.floor((available + gap) / (measured.current + gap))))
      setFit((prev) => (prev.rows === rows && prev.cols === cols ? prev : { rows, cols }))
    }
    calc()
    window.addEventListener('resize', calc)
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(calc)
    const el = ref.current
    const container = el?.closest('.page, .settings-page') ?? el?.parentElement
    if (ro && container) ro.observe(container)
    if (ro && el) ro.observe(el)
    return () => {
      window.removeEventListener('resize', calc)
      ro?.disconnect()
    }
  })

  return { ...fit, pageSize: Math.max(1, fit.rows * fit.cols) }
}

/** Height of an element (e.g. content under a table that `reserve` must leave room for). */
export function useElementHeight(ref: RefObject<HTMLElement | null>, fallback = 0): number {
  const [h, setH] = useState(fallback)
  useLayoutEffect(() => {
    const el = ref.current
    if (!el) return
    const calc = () => setH(el.offsetHeight)
    calc()
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(calc)
    ro?.observe(el)
    return () => ro?.disconnect()
  })
  return h
}

/**
 * "No … match." placeholder for a filtered list, with a button to clear the
 * filters. Renders a table row when `colSpan` is given, a div otherwise.
 */
export function NoMatches({ what, onClear, colSpan }: { what: string; onClear?: () => void; colSpan?: number }) {
  const body = (
    <>
      No {what} match.
      {onClear && (
        <Button variant="link" size="sm" onClick={onClear} style={{ marginLeft: 6 }}>
          Clear filters
        </Button>
      )}
    </>
  )
  if (colSpan !== undefined) {
    return (
      <tr data-empty-row="1">
        <td colSpan={colSpan} className="no-matches muted small">{body}</td>
      </tr>
    )
  }
  return <div data-empty-row="1" className="no-matches muted small">{body}</div>
}

/** Pagination for lists that aren't inside a table card (card grids): no top border. */
export function ListPager(props: ComponentProps<typeof Pagination>) {
  return (
    <div className="list-pager" data-fit-ignore="1">
      <Pagination {...props} />
    </div>
  )
}
