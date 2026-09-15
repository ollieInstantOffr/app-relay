import type { DocSection } from '../types'
import { startSections } from './start'
import { setupSections } from './setup'
import { proxySections } from './proxy'
import { edgeSections } from './edge'
import { loginSections } from './login'
import { errorPageSections } from './errorpages'
import { tlsSections } from './tls'
import { dnsSections } from './dns'
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
  ...loginSections,
  ...errorPageSections,
  ...edgeSections,
  ...tlsSections,
  ...dnsSections,
  ...lbSections,
  ...dockerSections,
  ...opsSections,
  ...adminSections,
  ...referenceSections,
]

export const DOC_GROUPS: string[] = [...new Set(DOC_SECTIONS.map((s) => s.group))]
