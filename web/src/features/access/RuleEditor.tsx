// Draggable IP rule rows ("Evaluated top to bottom").
import { useState } from 'react'
import { rid } from '../../lib/format'
import type { IPRule } from '../../lib/types'
import { Button, Icon, IconButton, Input, cx } from '../../components/ui'
import '../certificates/certs.css'

export function ruleSummary(rules: IPRule[]) {
  const allows = rules.filter((r) => r.action === 'allow' && r.cidr !== 'all').length
  const denies = rules.filter((r) => r.action === 'deny' && r.cidr !== 'all').length
  const denyAll = rules.some((r) => r.action === 'deny' && r.cidr === 'all')
  const parts: string[] = []
  if (allows) parts.push(`Allow ${allows} ${allows === 1 ? 'CIDR' : 'CIDRs'}`)
  if (denies) parts.push(`Deny ${denies} ${denies === 1 ? 'CIDR' : 'CIDRs'}`)
  if (denyAll) parts.push('deny all')
  return parts.join(' · ')
}

export default function RuleEditor({ rules, onChange, errors, readOnly }: {
  rules: IPRule[]
  onChange: (rules: IPRule[]) => void
  errors?: Record<string, string>
  readOnly?: boolean
}) {
  const [drag, setDrag] = useState<number | null>(null)
  const [over, setOver] = useState<number | null>(null)

  const update = (i: number, patch: Partial<IPRule>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  const move = (from: number, to: number) => {
    if (from === to) return
    const next = [...rules]
    const [item] = next.splice(from, 1)
    next.splice(to > from ? to - 1 : to, 0, item)
    onChange(next)
  }
  const add = () => {
    const rule: IPRule = { id: rid(), action: 'allow', cidr: '', note: '' }
    const last = rules[rules.length - 1]
    if (last && last.action === 'deny' && last.cidr === 'all') onChange([...rules.slice(0, -1), rule, last])
    else onChange([...rules, rule])
  }
  const lastIsDenyAll = rules.length > 0 && rules[rules.length - 1].action === 'deny' && rules[rules.length - 1].cidr === 'all'
  const hasAllow = rules.some((r) => r.action === 'allow')

  if (readOnly) {
    return (
      <div className="col" style={{ gap: 0 }}>
        {rules.map((r) => (
          <div key={r.id} className="cs-rule-static">
            <span className={`cs-action ${r.action}`}>{r.action}</span>
            <span className="mono grow">{r.cidr}</span>
            <span className="micro faint">{r.note}</span>
          </div>
        ))}
        {!rules.length && <div className="small muted" style={{ padding: '12px 18px' }}>No IP rules · every client passes the IP check.</div>}
      </div>
    )
  }

  return (
    <div className="col" style={{ gap: 0 }}>
      {rules.map((r, i) => {
        const err = errors?.[`rules.${i}.cidr`] ?? errors?.[`rules.${i}.action`] ?? errors?.[`rules.${i}.note`]
        return (
          <div
            key={r.id}
            className={cx('cs-rule', drag === i && 'dragging', over === i && drag !== null && drag !== i && 'drop-target')}
            onDragOver={(e) => {
              if (drag === null) return
              e.preventDefault()
              setOver(i)
            }}
            onDrop={(e) => {
              e.preventDefault()
              if (drag !== null) move(drag, i)
              setDrag(null)
              setOver(null)
            }}
            style={{ flexWrap: 'wrap' }}
          >
            <span
              className="drag-handle"
              draggable
              onDragStart={(e) => {
                setDrag(i)
                e.dataTransfer.effectAllowed = 'move'
                e.dataTransfer.setData('text/plain', r.id)
              }}
              onDragEnd={() => {
                setDrag(null)
                setOver(null)
              }}
              title="Drag to reorder"
            >
              <Icon name="drag" size={14} />
            </span>
            <button
              type="button"
              className={`cs-action ${r.action}`}
              style={{ border: 0, cursor: 'pointer' }}
              title="Click to switch allow / deny"
              onClick={() => update(i, { action: r.action === 'allow' ? 'deny' : 'allow' })}
            >
              {r.action}
            </button>
            <Input mono inputSize="sm" invalid={!!errors?.[`rules.${i}.cidr`]} value={r.cidr} placeholder="192.168.0.0/16 · 10.0.0.5 · all" onChange={(e) => update(i, { cidr: e.target.value.trim() })} style={{ flex: 1, minWidth: 140 }} />
            <Input inputSize="sm" value={r.note} placeholder="Note" onChange={(e) => update(i, { note: e.target.value })} style={{ width: 130 }} />
            <IconButton icon="close" bare size={14} label="Remove rule" onClick={() => onChange(rules.filter((_, j) => j !== i))} />
            {err && <div className="field-error" style={{ flexBasis: '100%', paddingLeft: 90 }}>{err}</div>}
          </div>
        )
      })}
      <div
        className={cx('row', over === rules.length && 'drop-target')}
        style={{ padding: '10px 14px', gap: 8 }}
        onDragOver={(e) => {
          if (drag === null) return
          e.preventDefault()
          setOver(rules.length)
        }}
        onDrop={(e) => {
          e.preventDefault()
          if (drag !== null) move(drag, rules.length)
          setDrag(null)
          setOver(null)
        }}
      >
        <Button variant="ghost" size="sm" icon="plus" onClick={add}>Add rule</Button>
        {hasAllow && !lastIsDenyAll && (
          <>
            <span className="micro warn-text grow">Clients matching no rule are allowed.</span>
            <Button size="sm" onClick={() => onChange([...rules, { id: rid(), action: 'deny', cidr: 'all', note: 'Default' }])}>Add deny all</Button>
          </>
        )}
      </div>
    </div>
  )
}
