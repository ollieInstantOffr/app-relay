// Upload a custom certificate (or replace an existing custom one).
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { api } from '../../lib/api'
import { keys } from '../../lib/queries'
import type { Certificate } from '../../lib/types'
import { Button, Dialog, Field, Input, Textarea, useToast } from '../../components/ui'
import { fieldErrors, pendingToast, toastUnlessFields } from './common'
import './certs.css'

function PemField({ label, value, onChange, error, placeholder, accept }: {
  label: string; value: string; onChange: (v: string) => void; error?: string; placeholder: string; accept: string
}) {
  const ref = useRef<HTMLInputElement>(null)
  return (
    <Field label={label} error={error}>
      <div className="cs-file-drop">
        <Textarea
          mono
          invalid={!!error}
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
          spellCheck={false}
          style={{ minHeight: 120, fontSize: 11 }}
          onDragOver={(e) => e.preventDefault()}
          onDrop={async (e) => {
            const f = e.dataTransfer.files?.[0]
            if (f) {
              e.preventDefault()
              onChange(await f.text())
            }
          }}
        />
        <Button size="sm" icon="upload" className="file-btn" onClick={() => ref.current?.click()}>Choose file</Button>
        <input
          ref={ref}
          type="file"
          accept={accept}
          hidden
          onChange={async (e) => {
            const f = e.target.files?.[0]
            if (f) onChange(await f.text())
            e.target.value = ''
          }}
        />
      </div>
    </Field>
  )
}

export default function UploadCertificateDialog({ open, onClose, replace, onUploaded }: {
  open: boolean
  onClose: () => void
  replace?: Certificate
  onUploaded?: (c: Certificate) => void
}) {
  const toast = useToast()
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [cert, setCert] = useState('')
  const [key, setKey] = useState('')
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(replace?.name ?? '')
    setCert('')
    setKey('')
    setErrors({})
  }, [open, replace])

  const submit = async () => {
    const form = new FormData()
    form.set('certificate', cert)
    form.set('privateKey', key)
    if (name.trim()) form.set('name', name.trim())
    if (replace) form.set('id', replace.id)
    setBusy(true)
    setErrors({})
    try {
      const saved = await api.upload<Certificate>('/api/certificates/upload', form)
      qc.invalidateQueries({ queryKey: keys.entities('certificates') })
      pendingToast(toast, replace ? 'Certificate replaced' : 'Certificate uploaded', saved.name)
      onUploaded?.(saved)
      onClose()
    } catch (err) {
      setErrors(fieldErrors(err))
      toastUnlessFields(toast, err, 'Upload failed')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={560}
      icon="upload"
      title={replace ? `Replace ${replace.name}` : 'Upload custom certificate'}
      description="PEM certificate (leaf first, intermediates optional) and its unencrypted private key. Relay can't renew custom certificates — you'll get a reminder 14 days before expiry."
      footer={
        <>
          <Button onClick={onClose}>Cancel</Button>
          <Button variant="primary" onClick={submit} loading={busy} disabled={!cert.trim() || !key.trim()}>
            {replace ? 'Replace' : 'Upload'}
          </Button>
        </>
      }
    >
      <Field label="Name (optional)" error={errors.name} hint="Defaults to the certificate's common name">
        <Input mono value={name} placeholder="internal-ca.pem" onChange={(e) => setName(e.target.value)} />
      </Field>
      <PemField label="Certificate" value={cert} onChange={setCert} error={errors.certificate} placeholder="-----BEGIN CERTIFICATE-----" accept=".pem,.crt,.cer,.cert" />
      <PemField label="Private key" value={key} onChange={setKey} error={errors.privateKey} placeholder="-----BEGIN PRIVATE KEY-----" accept=".pem,.key" />
    </Dialog>
  )
}
