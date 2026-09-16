// Generated with Graphite (renderer in ./graphite/Chart), adapted to Relay.
import { useMemo } from 'react'
import { Skeleton } from '../ui'
import { withPalette } from '../../lib/graphite.theme'
import { Chart } from './graphite/Chart'
import type { ResponseMixChartProps, ResponseMixDatum } from './ResponseMixChart.types'

const X_KEY = 'name' as const satisfies keyof ResponseMixDatum
const VALUE_KEYS = ['requests'] as const satisfies readonly (keyof ResponseMixDatum)[]

// success ink, 4xx warn, 5xx danger
const THEME = withPalette(['#141414', '#d97706', '#dc2626', '#b3b2ae'])

/** Share of successful, 4xx and 5xx responses. Presentational; owns no data fetching. */
export function ResponseMixChart({
  data,
  loading = false,
  emptyState = null,
  onPointClick,
  formatValue,
  title = 'Responses',
  subtitle,
  headline,
  unit = 'req',
  className,
}: ResponseMixChartProps) {
  const rows = useMemo(() => data.filter(Boolean), [data])

  if (loading) return <Skeleton height={260} />
  if (rows.length === 0) return <>{emptyState}</>

  return (
    <Chart
      type="donut"
      theme={THEME}
      data={rows}
      x={X_KEY}
      value={VALUE_KEYS}
      title={title}
      subtitle={subtitle}
      headline={headline}
      unit={unit}
      onPointClick={onPointClick}
      formatValue={formatValue}
      className={className}
      role="img"
      aria-label={title}
    />
  )
}
