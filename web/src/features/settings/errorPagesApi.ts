// Error pages & maintenance mode: page catalogue and the live preview query (Settings → Error pages, host drawer).
import { useMemo } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { api, ApiError, errorMessage } from '../../lib/api'
import type { ErrorPage, ErrorPageKey, ErrorPagePreview, ErrorPagePreviewRequest, ErrorPagesSettings } from '../../lib/types'
import { useDebounced } from '../hosts/lib'

export const ERROR_PAGE_KEYS: ErrorPageKey[] = ['403', '404', '429', '500', '502', '503', '504', 'maintenance']

/** Max size of a page's custom HTML (the server enforces the same limit). */
export const MAX_HTML_BYTES = 100 * 1024

export const ERROR_PAGE_INFO: Record<ErrorPageKey, { label: string; when: string; title: string; message: string }> = {
  '403': { label: '403', when: 'Blocked by an access list, location rule or geo-block', title: 'Access denied', message: "You don't have permission to open this page." },
  '404': { label: '404', when: 'No host or location matches the address', title: 'Page not found', message: "We couldn't find what you were looking for." },
  '429': { label: '429', when: 'A visitor went over the rate limit', title: 'Too many requests', message: 'Please wait a moment and try again.' },
  '500': { label: '500', when: 'Something went wrong inside the proxy', title: 'Something went wrong', message: 'Please try again in a little while.' },
  '502': { label: '502', when: "The app can't be reached or refused the connection", title: 'App unavailable', message: "The app behind this address isn't responding right now." },
  '503': { label: '503', when: 'The app is temporarily unavailable', title: 'Temporarily unavailable', message: 'Please try again in a few minutes.' },
  '504': { label: '504', when: 'The app took too long to answer', title: 'Timed out', message: 'The app took too long to respond. Please try again.' },
  maintenance: { label: 'Maintenance', when: 'Hosts in maintenance mode · sent as 503', title: 'Down for maintenance', message: "We're doing some work and will be back shortly." },
}

/** Fills missing pages/fields so forms never see undefined. */
export function normalizeErrorPages(s: ErrorPagesSettings): ErrorPagesSettings {
  const pages = {} as Record<ErrorPageKey, ErrorPage>
  for (const k of ERROR_PAGE_KEYS) {
    const p = s.pages?.[k]
    pages[k] = { title: p?.title ?? '', message: p?.message ?? '', ...(p?.html ? { html: p.html } : {}) }
  }
  return { enabled: !!s.enabled, brandName: s.brandName ?? '', accentColor: s.accentColor ?? '', pages }
}

export const HEX_COLOR = /^#[0-9a-f]{6}$/i

export function byteLength(s: string): number {
  return new TextEncoder().encode(s).length
}

export interface ErrorPagePreviewState {
  html?: string
  loading: boolean
  error?: string
}

/** Debounced POST /api/preview/error-page (the exact page Relay will serve). */
export function useErrorPagePreview(req: ErrorPagePreviewRequest | null, enabled = true): ErrorPagePreviewState {
  const json = useMemo(() => (req ? JSON.stringify(req) : ''), [req])
  const debounced = useDebounced(json, 300)
  const q = useQuery({
    queryKey: ['error-pages', 'preview', debounced],
    queryFn: () => api.post<ErrorPagePreview>('/api/preview/error-page', JSON.parse(debounced)),
    enabled: enabled && debounced !== '',
    retry: false,
    staleTime: Infinity,
    gcTime: 60_000,
    placeholderData: keepPreviousData,
  })
  if (!enabled || !req) return { loading: false }
  if (q.isError) {
    const fields = q.error instanceof ApiError ? q.error.fields : undefined
    const first = fields ? Object.values(fields)[0] : undefined
    return { loading: false, error: first || errorMessage(q.error) }
  }
  return { html: q.data?.html, loading: json !== debounced || q.isFetching }
}
