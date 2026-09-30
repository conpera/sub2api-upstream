import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

describe('unified browser session', () => {
  beforeEach(() => {
    vi.resetModules()
    vi.stubEnv('VITE_CONPERA_SESSION_CENTER', 'true')
    localStorage.clear()
    document.body.innerHTML = '<div id="app"></div>'
  })
  afterEach(() => { vi.unstubAllGlobals(); vi.unstubAllEnvs() })

  it('replaces legacy credentials with a nonsecret view marker', async () => {
    localStorage.setItem('auth_token', 'old-native-jwt')
    localStorage.setItem('refresh_token', 'old-native-refresh')
    localStorage.setItem('token_expires_at', '99999999')
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({
      authenticated: true, user: { id: 17, role: 'admin' }
    }), { status: 200 })))
    const mod = await import('../unifiedSession')
    expect(await mod.initializeUnifiedSession()).toBe(true)
    expect(localStorage.getItem('auth_token')).toBe('conpera-cookie-session')
    expect(localStorage.getItem('refresh_token')).toBeNull()
    expect(localStorage.getItem('token_expires_at')).toBeNull()
  })

  it('redirects expired sessions through SSO with the original local destination', async () => {
    const replace = vi.fn()
    vi.stubGlobal('location', { pathname: '/admin/accounts', search: '?page=2', hash: '', replace })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{}', { status: 401 })))
    const mod = await import('../unifiedSession')
    expect(await mod.initializeUnifiedSession()).toBe(false)
    expect(replace).toHaveBeenCalledWith('/sso/start?return=%2Fadmin%2Faccounts%3Fpage%3D2')
  })

  it('keeps identity during an outage and provides retry instead of redirecting to login', async () => {
    localStorage.setItem('auth_user', '{"id":17}')
    const replace = vi.fn()
    vi.stubGlobal('location', { replace })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{}', { status: 503 })))
    const mod = await import('../unifiedSession')
    expect(await mod.initializeUnifiedSession()).toBe(false)
    expect(localStorage.getItem('auth_user')).toBe('{"id":17}')
    expect(replace).not.toHaveBeenCalled()
    expect(document.querySelector('#app button')?.textContent).toBe('重试')
  })

  it('does not claim logout succeeded when the authority is unavailable', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{}', { status: 503 })))
    const mod = await import('../unifiedSession')
    await expect(mod.logoutUnifiedSession()).rejects.toThrow('退出失败')
  })
})
