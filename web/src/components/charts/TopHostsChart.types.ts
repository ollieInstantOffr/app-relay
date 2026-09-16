import type { ReactNode } from 'react'
import { z } from 'zod'

/** Runtime schema — single source of truth; the TS type is inferred from it. */
export const TopHostsDatumSchema = z.object({
  label: z.string(),
  requests: z.number(),
})

export type TopHostsDatum = z.infer<typeof TopHostsDatumSchema>

export interface TopHostsChartProps {
  /** Rows to render. Fetch anywhere and pass in; the component never fetches. */
  readonly data: readonly TopHostsDatum[]
  /** Shows a skeleton instead of the chart. */
  readonly loading?: boolean
  /** Rendered when `data` is empty and not loading. */
  readonly emptyState?: ReactNode
  readonly onPointClick?: (datum: TopHostsDatum) => void
  readonly formatValue?: (value: number) => string
  /** Overrides the card title. */
  readonly title?: string
  /** Faint text after the title, e.g. the time window. */
  readonly subtitle?: string
  /** Unit after the headline number. */
  readonly unit?: string
  /** Replaces the computed headline number; false hides it. */
  readonly headline?: string | false
  readonly className?: string
}
