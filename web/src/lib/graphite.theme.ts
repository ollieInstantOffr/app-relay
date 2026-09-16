import type { GraphiteTheme } from '../components/charts/graphite/Chart'

/**
 * Graphite chart theme, mapped onto the Relay tokens in styles/global.css.
 * Relay is light-only, so the dark tokens are kept for completeness but unused.
 */
export const graphiteTheme = {
  defaultMode: 'light',
  surface: 'transparent',
  modes: {
    light: {
      background: '#f7f7f5', // --bg
      surface: '#ffffff', // --surface
      border: 'rgba(0, 0, 0, 0.08)', // --hairline
      text: '#141414', // --ink
      muted: '#888888', // --ink-faint
      grid: 'rgba(0, 0, 0, 0.06)', // --hairline-soft
      track: '#f0f0ee', // --surface-2
    },
    dark: {
      background: '#0d0d0c',
      surface: '#161615',
      border: 'rgba(255, 255, 255, 0.08)',
      text: '#f2f2f0',
      muted: '#8f8f8a',
      grid: 'rgba(255, 255, 255, 0.07)',
      track: 'rgba(255, 255, 255, 0.05)',
    },
  },
  // ink, info slate, idle grey, danger
  palette: ['#141414', '#5b6b7f', '#b3b2ae', '#dc2626'],
  font: 'Geist Sans',
  radius: 6,
  stroke: 2,
  fill: 0.12,
  curve: 'linear',
  grid: true,
  legend: false,
  labels: true,
} as const satisfies GraphiteTheme

export type SeriesColor = (typeof graphiteTheme.palette)[number]

/** The theme with a different series palette (up to four colors, cycling). */
export function withPalette(palette: readonly string[]): GraphiteTheme {
  return { ...graphiteTheme, palette }
}
