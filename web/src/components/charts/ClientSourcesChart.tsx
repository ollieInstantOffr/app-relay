// Generated with Graphite (renderer in ./graphite/Chart), adapted to Relay.
import { useMemo } from 'react'
import { Skeleton } from '../ui'
import { withPalette } from '../../lib/graphite.theme'
import { Chart } from './graphite/Chart'
import type { ClientSourcesChartProps, ClientSourcesDatum } from './ClientSourcesChart.types'

const X_KEY = 'name' as const satisfies keyof ClientSourcesDatum
const VALUE_KEYS = ['requests'] as const satisfies readonly (keyof ClientSourcesDatum)[]

// LAN ink, VPN slate, internet grey, blocked danger
const THEME = withPalette(['#141414', '#5b6b7f', '#b3b2ae', '#dc2626'])

/** Requests by where clients came from: LAN, VPN, internet, blocked. Presentational; owns no data fetching. */
export function ClientSourcesChart({
  data,
  loading = false,
  emptyState = null,
  onPointClick,
  formatValue,
  title = 'Client sources',
  subtitle,
  headline,
  unit = 'req',
  className,
}: ClientSourcesChartProps) {
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
