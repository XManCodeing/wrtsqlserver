import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// 在内存中编译 API 模块，用 mock fetch 检查上传协议，不访问本机或真实路由。
const source = readFileSync(new URL('../src/api.ts', import.meta.url), 'utf8')
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } })
const { api, setCsrfToken } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
const calls = []
let response = { status: 200, payload: {} }
let expired = false
const timers = []
globalThis.window = {
  setTimeout(callback, delay) { timers.push(delay); return setTimeout(callback, delay) },
  clearTimeout,
  dispatchEvent(event) { expired = event.type === 'session-expired' },
}
globalThis.fetch = async (path, options) => {
  calls.push({ path, options })
  return { status: response.status, ok: response.status >= 200 && response.status < 300,
    text: async () => response.status === 204 ? '' : JSON.stringify(response.payload) }
}
setCsrfToken('test-csrf')
const file = new File(['test archive bytes'], 'release.tar')
const summary = { version: '1.1.0', platform: 'linux/amd64', imageTag: 'local/db-relay:1.1.0', sha256: 'test-hash', size: file.size, uploadedAt: new Date().toISOString() }
response.payload = summary
assert.deepEqual(await api.uploadUpdate(file), summary)
assert.equal(calls[0].path, '/api/updates/package')
assert.equal(calls[0].options.body, file)
assert.equal(calls[0].options.headers['Content-Type'], 'application/octet-stream')
assert.equal(calls[0].options.headers['X-CSRF-Token'], 'test-csrf')
assert.equal(calls[0].options.credentials, 'same-origin')
assert.equal(timers[0], 300000)
await api.updates()
assert.equal(calls[1].options.method, 'GET')
assert.equal(calls[1].options.headers['X-CSRF-Token'], undefined)
response.status = 204
assert.equal(await api.deleteUpdate(), undefined)
assert.equal(calls[2].options.method, 'DELETE')
response.status = 200
response.payload = { configured: false, address: '', state: 'unconfigured', checkedAt: null, latencyMs: null, error: '', metrics: null, databases: [] }
assert.equal((await api.redisStatus()).state, 'unconfigured')
assert.equal(calls.at(-1).path, '/api/redis/status')
assert.equal(calls.at(-1).options.method, 'GET')
assert.equal(calls.at(-1).options.body, undefined)
response = { status: 400, payload: { error: 'invalid signature' } }
await assert.rejects(api.uploadUpdate(file), /invalid signature/)
response = { status: 401, payload: { error: 'expired' } }
await assert.rejects(api.updates(), /登录已过期/)
assert.equal(expired, true)
console.log('Update API: binary payload, CSRF, timeout, delete, errors and expiration passed')
