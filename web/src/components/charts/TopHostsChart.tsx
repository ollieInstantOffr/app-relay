// Generated with Graphite (renderer in ./graphite/Chart), adapted to Relay.
import { useMemo } from 'react'
import { Skeleton } from '../ui'
import { graphiteTheme } from '../../lib/graphite.theme'
import { Chart } from './graphite/Chart'
import type { TopHostsChartProps, TopHostsDatum } from './TopHostsChart.types'

const X_KEY = 'label' as const satisfies keyof TopHostsDatum
const VALUE_KEYS = ['requests'] as const satisfies readonly (keyof TopHostsDatum)[]

// Bars carry their own value labels; y-axis ticks would sit under the bar tracks.
const THEME = { ...graphiteTheme, grid: false }

/** Busiest proxy hosts by requests; the largest bar is drawn in ink. Presentational; owns no data fetching. */
export function TopHostsChart({
  data,
  loading = false,
  emptyState = null,
  onPointClick,
  formatValue,
  title = 'Top hosts',
  subtitle,
  headline,
  unit = 'req',
  className,
}: TopHostsChartProps) {
  const rows = useMemo(() => data.filter(Boolean), [data])

  if (loading) return <Skeleton height={260} />
  if (rows.length === 0) return <>{emptyState}</>

  return (
    <Chart
      type="bar"
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
