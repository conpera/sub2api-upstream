import { apiClient } from '@/api/client'

// User activity, not polling or token refresh, extends the seven-day idle lease.
export function startBrowserSessionActivity(): () => void {
  const interval = 60_000
  let lastAttempt = -Infinity
  let pending = false
  let inFlight = false
  let stopped = false
  let timer: ReturnType<typeof setTimeout> | undefined
  const active = () => !stopped && !document.hidden && document.hasFocus() && !!localStorage.getItem('auth_token')
  const cancel = () => { if (timer !== undefined) clearTimeout(timer); timer = undefined }

  function flush() {
    cancel()
    if (!active()) { pending = false; return }
    if (!pending || inFlight) return
    const wait = interval - (Date.now() - lastAttempt)
    if (wait > 0) { timer = setTimeout(flush, wait); return }
    pending = false
    inFlight = true
    lastAttempt = Date.now()
    // The normal client coordinates token rotation across tabs before retrying 401.
    void apiClient.post('/auth/activity', {}, {
      headers: { 'X-Conpera-Activity': '1' }, timeout: 10_000
    }).catch(() => {
      // Temporary network failures do not erase authentication or create a heartbeat.
    }).finally(() => { inFlight = false; if (pending && !stopped) flush() })
  }
  function note(event?: Event) {
    if (event && !event.isTrusted) return
    if (!active()) return
    // Initial visit and pageshow/focus can describe the same visit. Do not queue
    // a delayed renewal for duplicate lifecycle signals.
    if (event && ['focus', 'pageshow', 'visibilitychange'].includes(event.type) && Date.now() - lastAttempt < interval) return
    pending = true
    flush()
  }
  function visibility(event: Event) {
    if (document.hidden) { pending = false; cancel() }
    else note(event)
  }
  const inputs = ['pointerdown', 'keydown', 'wheel', 'touchstart']
  inputs.forEach(type => document.addEventListener(type, note, { passive: true }))
  document.addEventListener('visibilitychange', visibility)
  window.addEventListener('focus', note)
  window.addEventListener('pageshow', note)
  note() // A visible, focused visit is activity; an unattended open tab is not.
  return () => {
    stopped = true; pending = false; cancel()
    inputs.forEach(type => document.removeEventListener(type, note))
    document.removeEventListener('visibilitychange', visibility)
    window.removeEventListener('focus', note)
    window.removeEventListener('pageshow', note)
  }
}
