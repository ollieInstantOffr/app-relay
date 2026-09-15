// Owner: slice ops. Settings → Backup & restore: off-site copies to S3 and S3-compatible storage.
import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { useSaveSettings } from '../../lib/queries'
import type { BackupS3Settings, BackupSettings } from '../../lib/types'
import { bytes, dateTime } from '../../lib/format'
import {
  Badge, Button, Callout, Dialog, Field, Input, NoMatches, Pagination, PasswordInput, SearchInput, Segmented, Skeleton, ToggleRow,
  matchesSearch, usePagination, useToast,
} from '../../components/ui'
import { fieldErrors, toastUnlessFields } from '../certificates/common'
import type { RemoteBackup, S3TestResult } from '../docker/ops'

type Provider = 'aws' | 'r2' | 'b2' | 'other'

const PROVIDERS: { value: Provider; label: string }[] = [
  { value: 'aws', label: 'AWS S3' },
  { value: 'r2', label: 'Cloudflare R2' },
  { value: 'b2', label: 'Backblaze B2' },
  { value: 'other', label: 'Other' },
]

const PRESET: Record<Provider, { region: string; pathStyle: boolean; endpoint: string; regionHint: string; help: string }> = {
  aws: {
    region: 'eu-central-1',
    pathStyle: false,
    endpoint: '',
    regionHint: 'Where the bucket lives, e.g. eu-central-1',
    help: 'Use an IAM user that can only reach this bucket: s3:PutObject, s3:GetObject, s3:DeleteObject and s3:ListBucket.',
  },
  r2: {
    region: 'auto',
    pathStyle: true,
    endpoint: 'https://<account-id>.r2.cloudflarestorage.com',
    regionHint: 'Always auto for R2',
    help: 'Create an R2 API token with Object Read & Write for this bucket. The endpoint is shown on the bucket’s settings page.',
  },
  b2: {
    region: 'us-west-004',
    pathStyle: false,
    endpoint: 'https://s3.us-west-004.backblazeb2.com',
    regionHint: 'The part after s3. in the endpoint',
    help: 'Create an application key with read and write access to this bucket. The endpoint is shown on the bucket page.',
  },
  other: {
    region: 'us-east-1',
    pathStyle: true,
    endpoint: 'https://minio.example.lan:9000',
    regionHint: 'us-east-1 works for most self-hosted storage',
    help: 'Any storage with an S3 API: MinIO, Wasabi, Garage, Ceph, Hetzner, Scaleway…',
  },
}

function detectProvider(s: BackupS3Settings): Provider {
  if (!s.endpoint) return 'aws'
  if (s.endpoint.includes('r2.cloudflarestorage.com')) return 'r2'
  if (s.endpoint.includes('backblazeb2.com')) return 'b2'
  return 'other'
}

export function s3Location(s: { bucket?: string; prefix?: string; endpoint?: string }): string {
  let host = 'AWS'
  if (s.endpoint) {
    try {
      host = new URL(s.endpoint).host
    } catch {
      host = s.endpoint
    }
  }
  return `s3://${s.bucket ?? ''}/${s.prefix ?? ''} · ${host}`
}

export function S3SetupDialog({ open, onClose, settings }: { open: boolean; onClose: () => void; settings: BackupSettings }) {
  const toast = useToast()
  const save = useSaveSettings('backup')
  const [provider, setProvider] = useState<Provider>('aws')
  const [draft, setDraft] = useState<BackupS3Settings>(settings.s3)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [testing, setTesting] = useState(false)
  const [result, setResult] = useState<S3TestResult | null>(null)

  useEffect(() => {
    if (!open) return
    const s3 = settings.s3
    const p = detectProvider(s3)
    setProvider(p)
    setDraft({ ...s3, region: s3.region || PRESET[p].region, secretAccessKey: '' })
    setErrors({})
    setResult(null)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  const set = (patch: Partial<BackupS3Settings>) => {
    setDraft((d) => ({ ...d, ...patch }))
    setResult(null)
    const changed = Object.keys(patch).map((k) => `s3.${k}`)
    setErrors((e) => Object.fromEntries(Object.entries(e).filter(([k]) => !changed.includes(k))))
  }
  const pick = (p: Provider) => {
    setProvider(p)
    const keepEndpoint = p !== 'aws' && detectProvider(draft) === p
    set({ endpoint: keepEndpoint ? draft.endpoint : '', region: PRESET[p].region, pathStyle: PRESET[p].pathStyle })
  }
  const err = (f: string) => errors[`s3.${f}`]

  const test = async () => {
    setTesting(true)
    setErrors({})
    setResult(null)
    try {
      setResult(await api.post<S3TestResult>('/api/backups/s3/test', draft))
    } catch (e) {
      setErrors(fieldErrors(e))
      toastUnlessFields(toast, e, 'Test failed')
    } finally {
      setTesting(false)
    }
  }

  const submit = async () => {
    setErrors({})
    try {
      await save.mutateAsync({ ...settings, s3: { ...draft, enabled: true } })
      toast.success('S3 destination saved', 'New backups are copied to the bucket.')
      onClose()
    } catch (e) {
      setErrors(fieldErrors(e))
      toastUnlessFields(toast, e, 'Could not save the S3 destination')
    }
  }

  const preset = PRESET[provider]
  const canSubmit = !!draft.bucket && !!draft.accessKeyId && (!!draft.secretAccessKey || settings.s3.secretSet)

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Copy backups to S3"
      description="Every backup is still written to this machine, then copied to your bucket. Retention and deletes apply to both copies. Archives stay encrypted with your passphrase."
      width={600}
      footer={
        <>
          <Button variant="ghost" loading={testing} disabled={!canSubmit} onClick={test} style={{ marginRight: 'auto' }}>Test connection</Button>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" loading={save.isPending} disabled={!canSubmit} onClick={submit}>Save and turn on</Button>
        </>
      }
    >
      <div className="col gap-14">
        <div className="col gap-8">
          <Segmented value={provider} onChange={pick} options={PROVIDERS} />
          <div className="small faint">{preset.help}</div>
        </div>
        {provider !== 'aws' && (
          <Field label="Endpoint" error={err('endpoint')}>
            <Input mono autoFocus value={draft.endpoint} placeholder={preset.endpoint} invalid={!!err('endpoint')} onChange={(e) => set({ endpoint: e.target.value })} />
          </Field>
        )}
        <div className="grid-2">
          <Field label="Bucket" error={err('bucket')}>
            <Input mono autoFocus={provider === 'aws'} value={draft.bucket} placeholder="relay-backups" invalid={!!err('bucket')} onChange={(e) => set({ bucket: e.target.value })} />
          </Field>
          <Field label="Region" hint={preset.regionHint} error={err('region')}>
            <Input mono value={draft.region} placeholder={preset.region} invalid={!!err('region')} onChange={(e) => set({ region: e.target.value })} />
          </Field>
        </div>
        <Field label="Folder in the bucket" hint="Optional, e.g. relay/ to share a bucket with other backups" error={err('prefix')}>
          <Input mono value={draft.prefix} placeholder="relay/" invalid={!!err('prefix')} onChange={(e) => set({ prefix: e.target.value })} />
        </Field>
        <div className="grid-2">
          <Field label="Access key ID" error={err('accessKeyId')}>
            <Input mono autoComplete="off" value={draft.accessKeyId} invalid={!!err('accessKeyId')} onChange={(e) => set({ accessKeyId: e.target.value })} />
          </Field>
          <Field label="Secret access key" hint={settings.s3.secretSet ? 'Saved. Leave empty to keep it' : undefined} error={err('secretAccessKey')}>
            <PasswordInput
              mono
              autoComplete="new-password"
              value={draft.secretAccessKey ?? ''}
              placeholder={settings.s3.secretSet ? '••••••••' : ''}
              invalid={!!err('secretAccessKey')}
              onChange={(e) => set({ secretAccessKey: e.target.value })}
            />
          </Field>
        </div>
        {provider === 'other' && (
          <ToggleRow
            title="Path-style URLs"
            description="endpoint/bucket/file instead of bucket.endpoint/file. Needed for MinIO and most self-hosted storage."
            checked={draft.pathStyle}
            onChange={(pathStyle) => set({ pathStyle })}
          />
        )}
        {result && (
          <Callout tone={result.ok ? (result.canList ? 'ok' : 'warn') : 'danger'} title={result.ok ? 'Connection works' : 'Connection failed'}>
            {result.message}
          </Callout>
        )}
      </div>
    </Dialog>
  )
}

const BUCKET_PAGE = 8

export function BucketDialog({ open, onClose, onRestore, location }: {
  open: boolean
  onClose: () => void
  onRestore: (b: RemoteBackup) => void
  location: string
}) {
  const [search, setSearch] = useState('')
  const listRef = useRef<HTMLDivElement>(null)
  const remote = useQuery({
    queryKey: ['ops', 'backups', 'remote'],
    queryFn: () => api.get<{ items: RemoteBackup[] }>('/api/backups/remote'),
    enabled: open,
    staleTime: 0,
    retry: false,
  })
  const items = (remote.data?.items ?? []).filter((b) => matchesSearch(search, b.name, dateTime(b.lastModified)))
  const pg = usePagination(items, BUCKET_PAGE, [search])

  useEffect(() => {
    if (open) setSearch('')
  }, [open])

  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Backups in the bucket"
      description={location}
      width={640}
      footer={
        <>
          <Button variant="ghost" icon="reload" loading={remote.isFetching} onClick={() => remote.refetch()} style={{ marginRight: 'auto' }}>Refresh</Button>
          <Button onClick={onClose}>Close</Button>
        </>
      }
    >
      <div className="col gap-12">
        {remote.isError ? (
          <Callout tone="danger" title="Couldn’t list the bucket">{remote.error instanceof Error ? remote.error.message : 'Unknown error'}</Callout>
        ) : remote.isLoading ? (
          <Skeleton height={160} />
        ) : (remote.data?.items.length ?? 0) === 0 ? (
          <div className="small muted">No Relay backups in this bucket folder yet.</div>
        ) : (
          <>
            <SearchInput value={search} onChange={setSearch} placeholder="Search file or date" label="Search bucket" />
            <div ref={listRef} className="col" style={{ border: '1px solid var(--hairline-soft)', borderRadius: 8 }}>
              {items.length === 0 && <NoMatches what="backups" onClear={() => setSearch('')} />}
              {pg.rows.map((b, i) => (
                <div
                  key={b.key}
                  className="row gap-12"
                  style={{ padding: '10px 14px', borderTop: i === 0 ? undefined : '1px solid var(--hairline-soft)' }}
                >
                  <div className="grow col" style={{ minWidth: 0 }}>
                    <span className="mono truncate" title={b.key}>{b.name}</span>
                    <span className="small faint">{dateTime(b.lastModified)} · {bytes(b.size)}</span>
                  </div>
                  {b.backupId && <Badge title="This backup is also in the snapshot list">Also local</Badge>}
                  <Button size="sm" variant="ghost" style={{ color: 'var(--ink)' }} onClick={() => onRestore(b)}>Restore</Button>
                </div>
              ))}
            </div>
            <Pagination page={pg.page} pageSize={pg.pageSize} total={pg.total} onPage={pg.setPage} label="backups" />
          </>
        )}
      </div>
    </Dialog>
  )
}
