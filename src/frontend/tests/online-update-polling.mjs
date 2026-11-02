import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

/** 编译 composable 并注入轻量生命周期，不挂载真实页面或操作宿主。@param path 模块路径。@returns 转译文本。 */
function compile(path) {
  return ts.transpileModule(readFileSync(new URL(path, import.meta.url), 'utf8'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 },
  }).outputText
}
const state = await import(`data:text/javascript;base64,${Buffer.from(compile('../src/composables/onlineUpdateState.ts')).toString('base64')}`)
let active = false
let watcher
let unmount
let visibilityListener
let timer
let reads = 0
let writes = 0
let failRead = false
const snapshot = { enabled: true, currentVersion: '1.0.0', latest: null, state: 'applying', phase: 'apply', message: '', operationId: 'op-1', updatedAt: '2026-11-02T03:00:00Z', canApply: false, canRollback: false }
const fakeApi = { async onlineUpdates() { reads++; if (failRead) throw new Error('disconnected'); return { ...snapshot } } }
globalThis.__onlineHarness = {
  ...state, api: fakeApi, errorMessage: error => error.message,
  ref: value => ({ value }), computed: callback => ({ get value() { return callback() } }),
  watch: (_getter, callback) => { watcher = callback; callback() }, onUnmounted: callback => { unmount = callback },
}
globalThis.document = {
  visibilityState: 'visible',
  addEventListener: (_event, callback) => { visibilityListener = callback },
  removeEventListener: (_event, callback) => { assert.equal(callback, visibilityListener); visibilityListener = undefined },
}
globalThis.setInterval = (callback, delay) => { assert.equal(delay, 5000); timer = callback; return 1 }
globalThis.clearInterval = () => { timer = undefined }
let source = compile('../src/composables/useOnlineUpdates.ts').replace(/^import .*;\n/gm, '')
source = `const { computed, onUnmounted, ref, watch, api, errorMessage, onlineDisconnectedMessage, onlineOperationActive, shouldPollOnline } = globalThis.__onlineHarness;\n${source}`
const { useOnlineUpdates } = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)
const model = useOnlineUpdates(() => active)
assert.equal(timer, undefined)
assert.equal(reads, 0)
active = true
watcher()
await Promise.resolve()
assert.equal(reads, 1)
assert.equal(typeof timer, 'function')
assert.equal(model.status.value.state, 'applying')
failRead = true
timer()
await Promise.resolve()
assert.equal(model.status.value.state, 'applying')
assert.match(model.error.value, /最终结果/)
assert.equal(model.blocked.value, true)
document.visibilityState = 'hidden'
visibilityListener()
assert.equal(timer, undefined)
const previousReads = reads
await model.refresh()
assert.equal(reads, previousReads)
failRead = false
snapshot.state = 'succeeded'
snapshot.phase = 'complete'
document.visibilityState = 'visible'
visibilityListener()
await Promise.resolve()
assert.equal(model.status.value.state, 'succeeded')
assert.equal(model.error.value, '')
assert.equal(model.blocked.value, false)
await model.submit(async () => { writes++; throw new Error('结果尚未确认') })
assert.equal(writes, 1)
assert.equal(model.status.value.state, 'succeeded')
assert.equal(model.blocked.value, true)
await model.submit(async () => { writes++; return snapshot })
assert.equal(writes, 1, 'uncertain operation cannot repeat before status refresh')
await model.refresh()
assert.equal(model.blocked.value, false)
const stages = JSON.parse(readFileSync(new URL('./online-update-stages.json', import.meta.url), 'utf8'))
for (const stage of stages) {
  Object.assign(snapshot, stage)
  await model.refresh()
  assert.equal(model.blocked.value, ['checking', 'downloading', 'applying'].includes(stage.state), JSON.stringify(stage))
  assert.equal(model.status.value.phase, stage.phase)
}
model.status.value.latest = { version: '1.2.3', image: 'ghcr.io/example/relay', digest: 'sha256:abc', platform: 'linux/amd64' }
model.status.value.canApply = true
model.invalidateSource()
assert.equal(model.status.value.latest, null)
assert.equal(model.status.value.canApply, false)
assert.equal(model.blocked.value, true, 'source change invalidates previous update immediately')
let resolveStale
fakeApi.onlineUpdates = () => new Promise(resolve => { resolveStale = resolve })
const pending = model.refresh()
model.invalidateSource()
fakeApi.onlineUpdates = async () => ({ ...snapshot, state: 'idle', phase: 'idle', latest: null, canApply: false })
resolveStale({ ...snapshot, state: 'available', phase: 'ready', latest: { version: '9.0.0' }, canApply: true })
await pending
await Promise.resolve()
assert.equal(model.status.value.latest, null, 'old in-flight status cannot restore a stale source release')
assert.equal(model.status.value.canApply, false)
active = false
watcher()
assert.equal(timer, undefined)
unmount()
assert.equal(visibilityListener, undefined)
console.log('Online composable: five-second active/visible polling, no hidden refresh, state preservation, no automatic/repeated POST and cleanup passed')
