import type { ReactNode } from 'react'
import { z } from 'zod'

/** Runtime schema — single source of truth; the TS type is inferred from it. */
export const ResponseMixDatumSchema = z.object({
  name: z.string(),
  requests: z.number(),
})

export type ResponseMixDatum = z.infer<typeof ResponseMixDatumSchema>

export interface ResponseMixChartProps {
  /** Rows to render. Fetch anywhere and pass in; the component never fetches. */
  readonly data: readonly ResponseMixDatum[]
  /** Shows a skeleton instead of the chart. */
  readonly loading?: boolean
  /** Rendered when `data` is empty and not loading. */
  readonly emptyState?: ReactNode
  readonly onPointClick?: (datum: ResponseMixDatum) => void
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
