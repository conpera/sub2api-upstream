import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { resolve } from 'node:path'

const cwd = fileURLToPath(new URL('..', import.meta.url))
const env = { ...process.env, VITE_CONPERA_SESSION_CENTER: 'true' }
for (const [tool, args] of [
  ['vitest', ['run', 'src/i18n/__tests__/localeKeyCompleteness.spec.ts']],
  ['vue-tsc', ['-b']],
  ['vite', ['build']]
]) {
  const result = spawnSync(resolve(cwd, 'node_modules/.bin', tool), args, { cwd, env, stdio: 'inherit' })
  if (result.error) throw result.error
  if (result.status !== 0) process.exit(result.status ?? 1)
}
await import('./check-session-build.mjs')
