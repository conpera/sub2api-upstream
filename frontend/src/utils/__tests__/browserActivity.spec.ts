import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { startBrowserSessionActivity } from '../browserActivity'
import { apiClient } from '@/api/client'

vi.mock('@/api/client', () => ({ apiClient: { post: vi.fn().mockResolvedValue({}) } }))
let stop: () => void
let hidden = false
let focus = true
let listeners: Map<string, EventListener>

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-09-27T00:00:00Z'))
  vi.mocked(apiClient.post).mockReset().mockResolvedValue({})
  localStorage.clear()
  localStorage.setItem('auth_token', 'synthetic')
  hidden = false; focus = true
  vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden)
  vi.spyOn(document, 'hasFocus').mockImplementation(() => focus)
  listeners = new Map()
  const original = document.addEventListener.bind(document)
  vi.spyOn(document, 'addEventListener').mockImplementation((type, listener, options) => {
    if (typeof listener === 'function') listeners.set(type, listener as EventListener)
    original(type, listener, options)
  })
})
afterEach(() => { stop?.(); vi.restoreAllMocks(); vi.useRealTimers() })
const input = (type: string, trusted = true) => listeners.get(type)?.({ isTrusted: trusted } as Event)

describe('browser activity lease', () => {
  it('renews on visit and trusted input, never on an idle timer', async () => {
    stop = startBrowserSessionActivity()
    await vi.advanceTimersByTimeAsync(10 * 60_000)
    expect(apiClient.post).toHaveBeenCalledTimes(1)
    input('keydown', false)
    expect(apiClient.post).toHaveBeenCalledTimes(1)
    input('keydown')
    await vi.advanceTimersByTimeAsync(0)
    expect(apiClient.post).toHaveBeenCalledTimes(2)
    expect(apiClient.post).toHaveBeenLastCalledWith('/auth/activity', {}, expect.objectContaining({ headers: { 'X-Conpera-Activity': '1' } }))
  })
  it('coalesces input and discards pending activity when hidden or logged out', async () => {
    stop = startBrowserSessionActivity()
    await vi.advanceTimersByTimeAsync(1000)
    input('wheel'); input('pointerdown')
    await vi.advanceTimersByTimeAsync(59_000)
    expect(apiClient.post).toHaveBeenCalledTimes(2)
    input('keydown'); hidden = true; input('visibilitychange')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(apiClient.post).toHaveBeenCalledTimes(2)
    hidden = false; focus = false; input('keydown')
    expect(apiClient.post).toHaveBeenCalledTimes(2)
    focus = true; localStorage.clear(); input('keydown')
    await vi.advanceTimersByTimeAsync(60_000)
    expect(apiClient.post).toHaveBeenCalledTimes(2)
  })
  it('retains login through temporary failures and tries again on later activity', async () => {
    vi.mocked(apiClient.post).mockRejectedValueOnce(new Error('offline'))
    stop = startBrowserSessionActivity()
    await vi.advanceTimersByTimeAsync(120_000)
    expect(localStorage.getItem('auth_token')).toBe('synthetic')
    expect(apiClient.post).toHaveBeenCalledTimes(1)
    input('touchstart')
    await vi.advanceTimersByTimeAsync(0)
    expect(apiClient.post).toHaveBeenCalledTimes(2)
  })
})
