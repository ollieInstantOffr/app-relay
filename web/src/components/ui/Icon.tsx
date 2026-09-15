import sprite from '../../assets/icons.svg?raw'

let injected = false
function inject() {
  if (injected || typeof document === 'undefined') return
  const div = document.createElement('div')
  div.innerHTML = sprite
  div.style.display = 'none'
  document.body.prepend(div)
  injected = true
}

export type IconName =
  | 'overview' | 'hosts' | 'load-balancer' | 'certificates' | 'access' | 'streams' | 'logs' | 'history' | 'settings'
  | 'users' | 'mcp' | 'docker' | 'redirect' | 'expose' | 'terminal' | 'stats'
  | 'plus' | 'search' | 'close' | 'chevron' | 'edit' | 'trash' | 'copy' | 'external' | 'reload' | 'check' | 'warning'
  | 'info' | 'filter' | 'download' | 'upload' | 'drain' | 'power' | 'reveal' | 'token' | 'link' | 'bolt' | 'rollback'
  | 'more' | 'drag'

export function Icon({ name, size = 16, className, style }: { name: IconName; size?: number; className?: string; style?: React.CSSProperties }) {
  inject()
  return (
    <svg width={size} height={size} className={className} style={{ flex: 'none', ...style }} aria-hidden>
      <use href={`#i-${name}`} />
    </svg>
  )
}

export function LogoMark({ size = 28, healthy = false }: { size?: number; healthy?: boolean }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-label="Relay">
      <rect width="32" height="32" rx="8" fill={healthy ? '#1fa971' : '#141414'} />
      <circle cx="9.5" cy="16" r="3" fill="#fff" />
      <path d="M12.5 16h6.5" stroke="#fff" strokeWidth="2.5" strokeLinecap="round" />
      <circle cx="22.5" cy="16" r="3" fill="none" stroke="#fff" strokeWidth="2.5" />
    </svg>
  )
}

export function Wordmark({ size = 18 }: { size?: number }) {
  return (
    <div className="row gap-8" style={{ alignItems: 'center' }}>
      <LogoMark size={size + 10} />
      <span style={{ fontSize: size, fontWeight: 600, letterSpacing: '-0.03em' }}>Relay</span>
    </div>
  )
}
