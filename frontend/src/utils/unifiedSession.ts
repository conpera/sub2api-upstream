export const unifiedSessionEnabled = import.meta.env.VITE_CONPERA_SESSION_CENTER === 'true'
export const COOKIE_SESSION_MARKER = 'conpera-cookie-session'

export async function initializeUnifiedSession(): Promise<boolean> {
  if (!unifiedSessionEnabled) return true
  try {
    const response = await fetch('/sso/session', {
      credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(10000)
    })
    if (response.status === 401) {
      for (const key of ['auth_token', 'auth_user', 'refresh_token', 'token_expires_at']) localStorage.removeItem(key)
      location.replace('/sso/start?return=' + encodeURIComponent(location.pathname + location.search + location.hash))
      return false
    }
    if (!response.ok) throw new Error('Session service unavailable')
    const session = await response.json()
    if (!session.authenticated || !session.user?.id) throw new Error('Invalid session response')
    // Compatibility state for existing view guards; this fixed string is not a credential.
    localStorage.setItem('auth_token', COOKIE_SESSION_MARKER)
    localStorage.setItem('auth_user', JSON.stringify(session.user))
    localStorage.removeItem('refresh_token')
    localStorage.removeItem('token_expires_at')
    return true
  } catch {
    const target = document.querySelector('#app')
    if (target) {
      const message = document.createElement('p')
      message.textContent = '暂时无法验证登录，请稍后重试。'
      const retry = document.createElement('button')
      retry.textContent = '重试'
      retry.addEventListener('click', () => location.reload())
      target.replaceChildren(message, retry)
    }
    return false
  }
}

export function startUnifiedSessionActivity(): void {
  if (document.querySelector('script[src="/sso/browser.js"]')) return
  const script = document.createElement('script')
  script.src = '/sso/browser.js'
  script.defer = true
  document.head.append(script)
}

export async function logoutUnifiedSession(): Promise<void> {
  const response = await fetch('/sso/logout', {
    method: 'POST', credentials: 'same-origin', signal: AbortSignal.timeout(10000)
  })
  if (!response.ok) throw new Error('退出失败，请重试。')
}
