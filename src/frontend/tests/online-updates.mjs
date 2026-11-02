import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'
import { computed, ref, watch, nextTick } from 'vue'

/** 在内存编译纯模块，不启动服务。@param path 相对模块路径。@returns 模块导出。 */
async function loadModule(path) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } })
  return import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
}
const { api, setCsrfToken } = await loadModule('../src/api.ts')
const { onlinePhaseLabel, onlineOperationActive, shouldPollOnline, onlineDisconnectedMessage, onlineDownloadPercent, canCheckOnOpen } = await loadModule('../src/composables/onlineUpdateState.ts')
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
await api.downloadOnlineUpdate('1.2.3', 'sha256:abc')
assert.equal(calls.at(-1).path, '/api/online-updates/download')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), { version: '1.2.3', digest: 'sha256:abc', confirmation: '更新 1.2.3' })
assert.equal(calls.at(-1).options.headers['X-CSRF-Token'], 'test-token')
await api.activateOnlineUpdate('1.2.3', 'sha256:abc')
assert.equal(calls.at(-1).path, '/api/online-updates/activate')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), { version: '1.2.3', digest: 'sha256:abc', confirmation: '更新 1.2.3' })
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
  for (const operation of [
    () => api.applyOnlineUpdate('1.2.3', 'sha256:abc', '更新 1.2.3'),
    () => api.downloadOnlineUpdate('1.2.3', 'sha256:abc'),
    () => api.activateOnlineUpdate('1.2.3', 'sha256:abc'),
  ]) {
    const previous = calls.length
    await assert.rejects(operation(), /尚未确认.*不要重复提交/)
    assert.equal(calls.length, previous + 1, 'must not retry POST')
  }
}
failure = null
response.status = 401
await assert.rejects(api.onlineUpdates(), /登录已过期/)
assert.equal(expired, true)
const stages = JSON.parse(readFileSync(new URL('./online-update-stages.json', import.meta.url), 'utf8'))
const labels = { idle: '待检测', checking: '正在检测', available: '发现新版本', downloading: '正在下载镜像', applying: '正在重启并检查健康状态', succeeded: '更新成功', rolled_back: '已回退', failed: '更新失败', manual_recovery: '需要人工恢复' }
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
const ready = { ...response.payload, state: 'downloaded', prepared: { version: '1.2.3' }, enabled: true }
assert.equal(onlinePhaseLabel(ready), '已下载，待重启')
assert.equal(onlineOperationActive(ready), false)
assert.equal(canCheckOnOpen(ready), false, 'opening must preserve a prepared update')
assert.equal(canCheckOnOpen({ ...ready, prepared: null, state: 'idle' }), true)
assert.equal(canCheckOnOpen({ ...ready, prepared: null, state: 'downloading' }), false)
assert.equal(canCheckOnOpen({ ...ready, prepared: null, state: 'manual_recovery' }), false)
for (const invalid of [undefined, null, NaN, Infinity, -1, 101]) {
  assert.equal(onlineDownloadPercent({ ...ready, downloadPercent: invalid }), null)
}
assert.equal(onlineDownloadPercent({ ...ready, downloadPercent: 42.9 }), 42)
assert.equal(onlineDownloadPercent({ ...ready, downloadPercent: 0 }), 0)
const component = readFileSync(new URL('../src/components/OnlineUpdates.vue', import.meta.url), 'utf8')
assert.doesNotMatch(component, /<el-input|openConfirmation|v-model="confirmation"/)
assert.match(component, /稍后重启/)
assert.match(component, /api\.activateOnlineUpdate/)
assert.doesNotMatch(component, /api\.applyOnlineUpdate/)
// 用真实 Vue 响应式运行组件逻辑，验证自动检测只在打开后发生以及每次动作只发一次。
const reactiveStatus = ref({ ...ready, state: 'idle', prepared: null, latest: { version: '1.2.3', digest: 'sha256:abc' }, stagedUpdates: true, canApply: true })
const submitted = []
let refreshes = 0
const fakeApi = {
  checkOnlineUpdate: async () => { submitted.push('check'); return reactiveStatus.value },
  downloadOnlineUpdate: async (version, digest) => { submitted.push(['download', version, digest]); return reactiveStatus.value },
  activateOnlineUpdate: async (version, digest) => { submitted.push(['activate', version, digest]); return reactiveStatus.value },
  rollbackOnlineUpdate: async () => reactiveStatus.value,
}
globalThis.__updateComponentHarness = {
  computed, ref, watch, onMounted: () => {}, api: fakeApi, safeMessage: value => value,
  canCheckOnOpen, onlineDownloadPercent, onlineOperationActive, onlinePhaseLabel, onlineTransferSize: () => '',
  useOnlineUpdates: () => ({ status: reactiveStatus, loading: ref(false), busy: ref(false), error: ref(''), blocked: ref(false),
    refresh: async () => { refreshes++ }, submit: async operation => { await operation() }, invalidateSource: () => {} }),
}
const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)[1]
const compiled = ts.transpileModule(script, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText.replace(/^import .*;\n/gm, '')
const componentModule = `const {computed,ref,watch,onMounted,api,safeMessage,canCheckOnOpen,onlineDownloadPercent,onlineOperationActive,onlinePhaseLabel,onlineTransferSize,useOnlineUpdates}=globalThis.__updateComponentHarness;\n${compiled}\nexport {openUpdates,opened,download,restart,sourceSaved};`
const actions = await import(`data:text/javascript;base64,${Buffer.from(componentModule).toString('base64')}`)
assert.equal(submitted.length, 0)
actions.openUpdates()
await nextTick()
assert.deepEqual(submitted, ['check'])
reactiveStatus.value = { ...reactiveStatus.value, state: 'available' }
await nextTick()
assert.deepEqual(submitted, ['check'], 'polling must not trigger another check')
await actions.download()
assert.deepEqual(submitted.at(-1), ['download', '1.2.3', 'sha256:abc'])
reactiveStatus.value = { ...reactiveStatus.value, state: 'downloaded', prepared: { version: '1.2.2', digest: 'sha256:pinned' } }
actions.opened.value = false
await nextTick()
actions.openUpdates()
await nextTick()
assert.equal(submitted.filter(value => value === 'check').length, 1, 'reopen preserves prepared version')
await actions.restart()
assert.deepEqual(submitted.at(-1), ['activate', '1.2.2', 'sha256:pinned'], 'restart never follows mutable latest')
reactiveStatus.value = { ...reactiveStatus.value, stagedUpdates: false, prepared: null }
const beforeLegacy = submitted.length
await actions.download()
assert.equal(submitted.length, beforeLegacy, 'legacy host must never silently fall back to download-and-apply')
actions.opened.value = false
await nextTick()
console.log('Online updates: staged download/activate, real progress, open detection, CSRF, no typed confirmation and no retries passed')
