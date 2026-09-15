import type { ComponentType } from 'react'
import type { IconName } from '../../components/ui'

export interface DocSection {
  id: string
  group: string
  title: string
  icon: IconName
  /** One sentence shown under the title and in search results. */
  summary: string
  /** Extra search terms. */
  keywords?: string
  /** Shortcuts into the app shown under the title. */
  app?: { to: string; label: string }[]
  Body: ComponentType
}
