// Owner: slice hosts. Geo-block section: the country database behind it.
import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../../../lib/api'
import { dateTime } from '../../../lib/format'
import { useRole } from '../../../lib/queries'
import { Button, Callout, useToast } from '../../../components/ui'

export interface GeoIPStatus {
  /** dbip | maxmind | '' (not downloaded yet) */
  source: '' | 'dbip' | 'maxmind'
  path?: string
  updatedAt?: string
  builtAt?: string
  hosts: number
  countries: string[]
  downloading: boolean
  lastError?: string
  lastAttemptAt?: string
}

export const geoipKey = ['geoip'] as const

export function useGeoIPStatus() {
  return useQuery({
    queryKey: geoipKey,
    queryFn: () => api.get<GeoIPStatus>('/api/geoip'),
    refetchInterval: (q) => (q.state.data?.downloading ? 2000 : false),
  })
}

export default function GeoIPInfo() {
  const st = useGeoIPStatus().data
  const { isAdmin } = useRole()
  const qc = useQueryClient()
  const toast = useToast()
  const [busy, setBusy] = useState(false)

  const download = async () => {
    setBusy(true)
    try {
      qc.setQueryData(geoipKey, await api.post<GeoIPStatus>('/api/geoip/update'))
      toast.success('Country database downloaded', 'Apply to start blocking.')
    } catch (err) {
      toast.error(err, 'Download failed')
      qc.invalidateQueries({ queryKey: geoipKey })
    } finally {
      setBusy(false)
    }
  }

  if (!st) return null
  if (!st.source) {
    return (
      <Callout
        tone={st.lastError ? 'warn' : 'info'}
        actions={isAdmin ? <Button size="sm" loading={busy || st.downloading} onClick={download}>{st.lastError ? 'Try again' : 'Download now'}</Button> : undefined}
      >
        {st.lastError
          ? `The country database couldn’t be downloaded: ${st.lastError}. Geo-blocking is skipped until it works.`
          : 'Relay downloads the free country database (about 4 MB) when you apply, and updates it every month.'}
      </Callout>
    )
  }
  const when = st.builtAt ?? st.updatedAt
  return (
    <div className="small faint">
      Local and private addresses always get in · Country data:{' '}
      {st.source === 'maxmind' ? (
        'your MaxMind GeoLite2 file'
      ) : (
        <>
          DB-IP Lite{when ? ` from ${dateTime(when)}` : ''}, updated monthly ·{' '}
          <a href="https://db-ip.com" target="_blank" rel="noreferrer" style={{ textDecoration: 'underline' }}>IP Geolocation by DB-IP</a>
        </>
      )}
    </div>
  )
}
