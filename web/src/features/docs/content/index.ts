import type { DocSection } from '../types'
import { startSections } from './start'
import { setupSections } from './setup'
import { proxySections } from './proxy'
import { tlsSections } from './tls'
import { lbSections } from './lb'
import { dockerSections } from './docker'
import { opsSections } from './ops'
import { adminSections } from './admin'
import { referenceSections } from './reference'

const [introduction, ...restStart] = startSections

/** Every article in reading order. */
export const DOC_SECTIONS: DocSection[] = [
  introduction,
  ...setupSections,
  ...restStart,
  ...proxySections,
  ...tlsSections,
  ...lbSections,
  ...dockerSections,
  ...opsSections,
  ...adminSections,
  ...referenceSections,
]

export const DOC_GROUPS: string[] = [...new Set(DOC_SECTIONS.map((s) => s.group))]
