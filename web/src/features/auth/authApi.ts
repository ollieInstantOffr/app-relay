// Auth slice: types, queries and helpers shared by the auth screens and the
// Users / General / About settings pages.
import { useQuery, type UseQueryResult } from '@tanstack/react-query'
import { api, ApiError } from '../../lib/api'
import { useSession } from '../../lib/queries'
import type { ApiToken, Role, Session, SessionUser } from '../../lib/types'

/** GET /api/auth/session — the shared Session plus auth-slice extras. */
export interface AuthUser extends SessionUser {
  passkeyCount: number
  mustEnroll2fa: boolean
  sessionExpiresAt: string
}
export interface AuthSession extends Omit<Session, 'user'> {
  user?: AuthUser
  /** Present when authenticated. */
  setupDone?: boolean
  sessionTtlHours: number
}

export function useAuthSession(): UseQueryResult<AuthSession> {
  return useSession() as unknown as UseQueryResult<AuthSession>
}

export interface UserRow {
  id: string
  username: string
  email: string
  role: Role
  totpEnabled: boolean
  passkeys: number
  twoFactor: boolean
  disabled: boolean
  mustChangePassword: boolean
  createdAt: string
  lastActiveAt?: string
  self: boolean
}

export interface SessionRow {
  id: string
  userId: string
  username: string
  createdAt: string
  lastSeenAt: string
  expiresAt: string
  ip: string
  userAgent: string
  remember: boolean
  current: boolean
}

export interface Passkey { id: string; name: string; createdAt: string; lastUsedAt?: string }
export interface TotpEnroll { secret: string; otpauthUrl: string; qrPng: string }
export interface SetupCheck { id: string; status: 'ok' | 'warn' | 'fail' | 'unknown'; title: string; detail: string }
export interface NetworkReport {
  checks: SetupCheck[]
  lanCidr: string
  lanDetected: boolean
  publicIp: string
  adminDomain: string
  containers: number | null
  httpContainers: number | null
}
export interface FinishResult { applied: boolean; version?: number; error?: string; containers: number | null; httpContainers: number | null }
export interface About { version: string; installedAt?: string; databaseBytes: number; goVersion: string }

export const authKeys = {
  users: ['auth', 'users'] as const,
  sessions: (all: boolean) => ['auth', 'sessions', all] as const,
  passkeys: ['auth', 'passkeys'] as const,
  tokens: (surface: string) => ['tokens', surface] as const,
  about: ['auth', 'about'] as const,
  network: ['setup', 'network'] as const,
}

export function useUsers(enabled = true) {
  return useQuery({ queryKey: authKeys.users, queryFn: () => api.get<UserRow[]>('/api/users'), enabled })
}
export function useMySessions(all: boolean) {
  return useQuery({ queryKey: authKeys.sessions(all), queryFn: () => api.get<SessionRow[]>(`/api/sessions${all ? '?scope=all' : ''}`) })
}
export function usePasskeys() {
  return useQuery({ queryKey: authKeys.passkeys, queryFn: () => api.get<Passkey[]>('/api/auth/passkeys') })
}
export function useTokens(surface: 'mcp' | 'rest', enabled = true) {
  return useQuery({ queryKey: authKeys.tokens(surface), queryFn: () => api.get<ApiToken[]>(`/api/tokens?surface=${surface}`), enabled })
}
export function useAbout() {
  return useQuery({ queryKey: authKeys.about, queryFn: () => api.get<About>('/api/about') })
}

// ---------------------------------------------------------------- errors
export function fieldErrors(err: unknown): Record<string, string> {
  return err instanceof ApiError && err.fields ? err.fields : {}
}
export function errCode(err: unknown): string {
  return err instanceof ApiError ? err.code : ''
}
/** First field error, or the error message. */
export function describeError(err: unknown): string {
  const f = fieldErrors(err)
  const first = Object.values(f)[0]
  if (first) return first
  if (err instanceof DOMException && err.name === 'NotAllowedError') return 'The passkey prompt was cancelled or timed out'
  if (err instanceof Error) return err.message
  return String(err)
}

export function safeNext(next: string | null): string {
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.startsWith('/login') || next.startsWith('/setup')) return '/'
  return next
}

export const roleLabel: Record<Role, string> = { admin: 'Admin', editor: 'Editor', viewer: 'Viewer' }

// ---------------------------------------------------------------- passkeys (WebAuthn)
function b64urlToBuf(s: string): ArrayBuffer {
  const pad = s.length % 4 ? '='.repeat(4 - (s.length % 4)) : ''
  const bin = atob(s.replace(/-/g, '+').replace(/_/g, '/') + pad)
  const buf = new ArrayBuffer(bin.length)
  const view = new Uint8Array(buf)
  for (let i = 0; i < bin.length; i++) view[i] = bin.charCodeAt(i)
  return buf
}

function bufToB64url(buf: ArrayBuffer | null): string {
  if (!buf) return ''
  const bytes = new Uint8Array(buf)
  let bin = ''
  for (let i = 0; i < bytes.length; i++) bin += String.fromCharCode(bytes[i])
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

export function passkeysSupported(): boolean {
  return typeof window !== 'undefined' && window.isSecureContext && typeof window.PublicKeyCredential === 'function'
}

/* eslint-disable @typescript-eslint/no-explicit-any */
type Descriptor = { id: string; type: PublicKeyCredentialType; transports?: AuthenticatorTransport[] }

function descriptors(list: Descriptor[] | undefined): PublicKeyCredentialDescriptor[] {
  return (list ?? []).map((c) => ({ ...c, id: b64urlToBuf(c.id) }))
}

/** Registers a passkey for the signed-in user. */
export async function registerPasskey(name: string): Promise<Passkey> {
  const opts = await api.post<{ publicKey: any }>('/api/auth/passkeys/register/begin')
  const pk = opts.publicKey
  const publicKey: PublicKeyCredentialCreationOptions = {
    ...pk,
    challenge: b64urlToBuf(pk.challenge),
    user: { ...pk.user, id: b64urlToBuf(pk.user.id) },
    excludeCredentials: descriptors(pk.excludeCredentials),
  }
  const cred = (await navigator.credentials.create({ publicKey })) as PublicKeyCredential | null
  if (!cred) throw new Error('Passkey creation was cancelled')
  const res = cred.response as AuthenticatorAttestationResponse
  const body = {
    id: cred.id,
    rawId: bufToB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: bufToB64url(res.clientDataJSON),
      attestationObject: bufToB64url(res.attestationObject),
      transports: typeof res.getTransports === 'function' ? res.getTransports() : [],
    },
  }
  return api.post<Passkey>(`/api/auth/passkeys/register/finish?name=${encodeURIComponent(name)}`, body)
}

/** Signs in with a discoverable passkey; returns the new session. */
export async function passkeyLogin(remember: boolean): Promise<AuthSession> {
  const opts = await api.post<{ publicKey: any }>('/api/auth/passkeys/login/begin')
  const pk = opts.publicKey
  const publicKey: PublicKeyCredentialRequestOptions = {
    ...pk,
    challenge: b64urlToBuf(pk.challenge),
    allowCredentials: descriptors(pk.allowCredentials),
  }
  const cred = (await navigator.credentials.get({ publicKey })) as PublicKeyCredential | null
  if (!cred) throw new Error('Passkey sign-in was cancelled')
  const res = cred.response as AuthenticatorAssertionResponse
  const body = {
    id: cred.id,
    rawId: bufToB64url(cred.rawId),
    type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment ?? undefined,
    clientExtensionResults: cred.getClientExtensionResults(),
    response: {
      clientDataJSON: bufToB64url(res.clientDataJSON),
      authenticatorData: bufToB64url(res.authenticatorData),
      signature: bufToB64url(res.signature),
      userHandle: res.userHandle ? bufToB64url(res.userHandle) : undefined,
    },
  }
  return api.post<AuthSession>(`/api/auth/passkeys/login/finish${remember ? '?remember=1' : ''}`, body)
}

/** A friendly default passkey name from the user agent ("Chrome on macOS"). */
export function deviceLabel(ua: string = typeof navigator !== 'undefined' ? navigator.userAgent : ''): string {
  if (!ua) return 'Unknown device'
  const browser = /Edg\//.test(ua) ? 'Edge'
    : /OPR\//.test(ua) ? 'Opera'
    : /Firefox\//.test(ua) ? 'Firefox'
    : /Chrome\//.test(ua) ? 'Chrome'
    : /Safari\//.test(ua) ? 'Safari'
    : /curl\//i.test(ua) ? 'curl'
    : ''
  const os = /iPhone|iPad/.test(ua) ? 'iOS'
    : /Android/.test(ua) ? 'Android'
    : /Mac OS X|Macintosh/.test(ua) ? 'macOS'
    : /Windows/.test(ua) ? 'Windows'
    : /Linux/.test(ua) ? 'Linux'
    : ''
  if (browser && os) return `${browser} on ${os}`
  return browser || os || ua.slice(0, 40)
}

// ---------------------------------------------------------------- passwords
export const MIN_PASSWORD = 10

export interface Strength { score: 0 | 1 | 2 | 3 | 4; label: string; message: string }

const COMMON = ['password', 'passw0rd', 'relay', 'admin', 'qwerty', 'letmein', 'welcome', 'iloveyou', 'dragon', 'monkey', '123456', 'abc123']

/** Rough entropy estimate for the strength meter (the server enforces the minimum length). */
export function passwordStrength(pw: string): Strength {
  const chars = [...pw]
  const len = chars.length
  if (len === 0) return { score: 0, label: '', message: `At least ${MIN_PASSWORD} characters` }
  const lower = /[a-z]/.test(pw)
  const upper = /[A-Z]/.test(pw)
  const digit = /\d/.test(pw)
  const symbol = /[^A-Za-z0-9]/.test(pw)
  const pool = (lower ? 26 : 0) + (upper ? 26 : 0) + (digit ? 10 : 0) + (symbol ? 33 : 0)
  let bits = len * Math.log2(Math.max(pool, 2))
  const unique = new Set(chars).size
  if (unique <= Math.max(2, len / 4)) bits *= 0.4
  const lc = pw.toLowerCase()
  if (COMMON.some((w) => lc.includes(w))) bits -= 25
  if (/(0123|1234|2345|3456|4567|5678|6789|abcd|bcde|cdef|qwer|asdf)/i.test(pw)) bits -= 10
  let score: Strength['score'] = bits < 40 ? 1 : bits < 60 ? 2 : bits < 80 ? 3 : 4
  if (len < MIN_PASSWORD) score = 1
  const label = len < MIN_PASSWORD ? 'Too short' : ['', 'Weak', 'Fair', 'Strong', 'Very strong'][score]
  let hint = ''
  if (len < MIN_PASSWORD) hint = `${MIN_PASSWORD - len} more needed`
  else if (score < 4 && !symbol) hint = 'add a symbol for very strong'
  else if (score < 4 && !digit) hint = 'add a number for very strong'
  else if (score < 4) hint = 'make it longer for very strong'
  return { score, label, message: [label, `${len} character${len === 1 ? '' : 's'}`, hint].filter(Boolean).join(' · ') }
}

const ADJECTIVES = [
  'agile', 'alpine', 'amber', 'azure', 'bold', 'brave', 'bright', 'brisk', 'calm', 'cheery', 'clever', 'coral', 'cosmic', 'cozy',
  'crisp', 'daring', 'dapper', 'dusty', 'eager', 'fair', 'fleet', 'fresh', 'fuzzy', 'gentle', 'glad', 'golden', 'grand', 'happy',
  'hardy', 'hazel', 'humble', 'ivory', 'jade', 'jolly', 'keen', 'kind', 'lively', 'loyal', 'lucky', 'lunar', 'maple', 'mellow',
  'merry', 'mild', 'misty', 'mossy', 'neat', 'nifty', 'nimble', 'noble', 'olive', 'pearl', 'plucky', 'polar', 'proud', 'quick',
  'quiet', 'rapid', 'ready', 'rosy', 'royal', 'ruby', 'rustic', 'sandy', 'sharp', 'shiny', 'silent', 'silver', 'sleek', 'smart',
  'snappy', 'snowy', 'solar', 'solid', 'spry', 'steady', 'stormy', 'sturdy', 'sunny', 'sweet', 'swift', 'tidy', 'true', 'velvet',
  'vivid', 'warm', 'windy', 'wise', 'witty', 'young', 'zesty',
]
const NOUNS = [
  'acorn', 'anchor', 'aspen', 'badger', 'beaver', 'birch', 'bison', 'breeze', 'brook', 'canyon', 'cedar', 'cliff', 'clover', 'comet',
  'condor', 'coyote', 'crane', 'delta', 'dingo', 'dolphin', 'dune', 'eagle', 'ember', 'falcon', 'fern', 'finch', 'fjord', 'fox',
  'gecko', 'glacier', 'grove', 'harbor', 'heron', 'husky', 'ibis', 'island', 'koala', 'lagoon', 'lark', 'lemur', 'lion', 'llama',
  'lynx', 'magpie', 'marmot', 'meadow', 'mesa', 'moose', 'newt', 'ocelot', 'orbit', 'orca', 'osprey', 'otter', 'owl', 'panda',
  'parrot', 'pebble', 'pelican', 'puffin', 'quail', 'quartz', 'rabbit', 'raven', 'reef', 'ridge', 'river', 'robin', 'salmon', 'seal',
  'sparrow', 'stork', 'summit', 'swan', 'tapir', 'tiger', 'toucan', 'trout', 'tundra', 'turtle', 'valley', 'walrus', 'whale',
  'willow', 'wolf', 'wombat', 'yak', 'zebra',
]

function randomInt(n: number): number {
  const limit = Math.floor(0x100000000 / n) * n
  const buf = new Uint32Array(1)
  for (;;) {
    crypto.getRandomValues(buf)
    if (buf[0] < limit) return buf[0] % n
  }
}

/** "brisk-otter-4471" style initial password. */
export function generatePassword(): string {
  return `${ADJECTIVES[randomInt(ADJECTIVES.length)]}-${NOUNS[randomInt(NOUNS.length)]}-${String(randomInt(10000)).padStart(4, '0')}`
}
