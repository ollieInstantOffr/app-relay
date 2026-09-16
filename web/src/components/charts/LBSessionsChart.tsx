// Generated with Graphite (renderer in ./graphite/Chart), adapted to Relay.
import { useMemo } from 'react'
import { Skeleton } from '../ui'
import { withPalette } from '../../lib/graphite.theme'
import { Chart } from './graphite/Chart'
import type { LBSessionsChartProps, LBSessionsDatum } from './LBSessionsChart.types'

const X_KEY = 'label' as const satisfies keyof LBSessionsDatum
const SERIES_KEYS = ['sessions', 'errors'] as const satisfies readonly (keyof LBSessionsDatum)[]

// sessions ink, errors danger
const THEME = { ...withPalette(['#141414', '#dc2626', '#5b6b7f', '#b3b2ae']), legend: true, labels: false }

/** Load balancer session rate over time with errors overlaid. Presentational; owns no data fetching. */
export function LBSessionsChart({
  data,
  loading = false,
  emptyState = null,
  onPointClick,
  formatValue,
  title = 'Sessions / s',
  subtitle,
  headline,
  unit = '',
  className,
}: LBSessionsChartProps) {
  const rows = useMemo(() => data.filter(Boolean), [data])

  if (loading) return <Skeleton height={260} />
  if (rows.length === 0) return <>{emptyState}</>

  return (
    <Chart
      type="area"
      theme={THEME}
      data={rows}
      x={X_KEY}
      series={SERIES_KEYS}
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
