import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

/** 在内存编译纯模块，不启动服务。@param path 相对模块路径。@returns 模块导出。 */
async function loadModule(path) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } })
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
}
const { api, setCsrfToken } = await loadModule('../src/api.ts')
const { onlinePhaseLabel, onlineOperationActive, shouldPollOnline, onlineDisconnectedMessage } = await loadModule('../src/composables/onlineUpdateState.ts')
const calls = []
let expired = false
let failure = null
let response = { status: 202, payload: { enabled: true, currentVersion: '1.0.0', latest: null, state: 'checking', phase: 'check', message: '', operationId: 'op-1', updatedAt: '2026-11-02T03:00:00Z', canApply: false, canRollback: false } }
globalThis.window = { setTimeout, clearTimeout, dispatchEvent(event) { expired = event.type === 'session-expired' } }
globalThis.fetch = async (path, options) => {
  calls.push({ path, options })
  if (failure) throw failure
  return { status: response.status, ok: response.status >= 200 && response.status < 300, text: async () => JSON.stringify(response.payload) }
}
setCsrfToken('test-token')
assert.deepEqual(await api.checkOnlineUpdate(), response.payload)
assert.equal(calls.at(-1).path, '/api/online-updates/check')
assert.equal(calls.at(-1).options.method, 'POST')
assert.equal(calls.at(-1).options.body, undefined)
assert.equal(calls.at(-1).options.headers['X-CSRF-Token'], 'test-token')
await api.applyOnlineUpdate('v1.2.3', 'sha256:abc', '更新 v1.2.3')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), { version: 'v1.2.3', digest: 'sha256:abc', confirmation: '更新 v1.2.3' })
assert.equal(calls.at(-1).path, '/api/online-updates/apply')
await api.rollbackOnlineUpdate('回退转发程序')
assert.equal(calls.at(-1).path, '/api/online-updates/rollback')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), { confirmation: '回退转发程序' })
response.status = 200
await api.onlineUpdates()
assert.equal(calls.at(-1).path, '/api/online-updates')
assert.equal(calls.at(-1).options.method, 'GET')
assert.equal(calls.at(-1).options.headers['X-CSRF-Token'], undefined)
assert.equal(calls.at(-1).options.credentials, 'same-origin')
for (const problem of [new TypeError('offline'), new DOMException('timeout', 'AbortError')]) {
  failure = problem
  const previous = calls.length
  await assert.rejects(api.applyOnlineUpdate('1.2.3', 'sha256:abc', '更新 1.2.3'), /尚未确认.*不要重复提交/)
  assert.equal(calls.length, previous + 1, 'must not retry POST')
}
failure = null
response.status = 401
await assert.rejects(api.onlineUpdates(), /登录已过期/)
assert.equal(expired, true)
const stages = JSON.parse(readFileSync(new URL('./online-update-stages.json', import.meta.url), 'utf8'))
const labels = { idle: '待检测', checking: '正在检测', available: '发现新版本', downloading: '正在下载镜像', applying: '正在切换并检查健康状态', succeeded: '更新成功', rolled_back: '已回退', failed: '更新失败', manual_recovery: '需要人工恢复' }
for (const stage of stages) {
  response = { status: 200, payload: { ...response.payload, ...stage } }
  const actual = await api.onlineUpdates()
  assert.deepEqual(actual, response.payload)
  assert.equal(onlineOperationActive(actual), ['checking', 'downloading', 'applying'].includes(stage.state), JSON.stringify(stage))
  assert.equal(onlinePhaseLabel(actual), labels[stage.state], JSON.stringify(stage))
}
assert.equal(onlinePhaseLabel({ ...response.payload, enabled: false }), '未启用')
const applying = { ...response.payload, state: 'applying', phase: 'rollback' }
assert.match(onlineDisconnectedMessage(applying), /最终结果以恢复后的状态为准/)
assert.equal(applying.state, 'applying', 'disconnect must preserve persistent state')
assert.equal(applying.phase, 'rollback', 'disconnect must preserve detail phase')
assert.equal(shouldPollOnline(true, 'visible'), true)
assert.equal(shouldPollOnline(false, 'visible'), false)
assert.equal(shouldPollOnline(true, 'hidden'), false)
console.log('Online updates: fixed APIs, CSRF, 202, no POST retries, expiration, persistent phases and visibility polling passed')
