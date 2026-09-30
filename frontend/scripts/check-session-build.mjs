import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const root = resolve(process.argv[2] || fileURLToPath(new URL('../../backend/internal/web/dist', import.meta.url)))
const html = readFileSync(resolve(root, 'index.html'), 'utf8')
const entry = html.match(/<script[^>]+src="([^"]+)"/)?.[1]
if (!entry?.startsWith('/assets/')) throw new Error('Session frontend has no bundled entry')
const source = readFileSync(resolve(root, '.' + entry), 'utf8')
for (const required of ['/sso/session', 'conpera-cookie-session', '/sso/start?return=', '/sso/browser.js']) {
  if (!source.includes(required)) {
    throw new Error('Session frontend is missing ' + required + '; build with VITE_CONPERA_SESSION_CENTER=true')
  }
}
console.log('Session frontend build verified: ' + entry)
