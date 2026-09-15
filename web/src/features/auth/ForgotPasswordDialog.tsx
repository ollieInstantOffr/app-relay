// "Forgot your password?" (design 23): reset from the machine Relay runs on.
import { Button, CopyButton, Dialog } from '../../components/ui'
import './auth.css'

const USERNAME = /^[A-Za-z0-9][A-Za-z0-9._-]{1,31}$/

export function ForgotPasswordDialog({ open, onClose, username }: { open: boolean; onClose: () => void; username: string }) {
  const name = USERNAME.test(username.trim()) ? username.trim() : '<username>'
  const cmd = `docker exec -it relay relay users reset-password ${name}`
  return (
    <Dialog
      open={open}
      onClose={onClose}
      width={500}
      title="Forgot your password?"
      description="Relay doesn't assume you have email set up. Reset from the machine it runs on:"
      footer={<Button onClick={onClose}>Back to sign in</Button>}
    >
      <div className="token-reveal">
        <code>{cmd}</code>
        <CopyButton text={cmd} />
      </div>
      <div className="muted small" style={{ lineHeight: 1.55 }}>
        It prints a temporary password and signs that account out everywhere. Another admin can also reset it in Settings → Users &amp; access.
      </div>
    </Dialog>
  )
}
