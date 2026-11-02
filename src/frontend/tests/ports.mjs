import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

/** 在 VM 内编译模块，注入依赖，避免真实宿主请求。
 * @param path 相对测试目录的源文件路径。
 * @param dependencies 模块依赖表。
 * @param globals 浏览器生命周期替身。
 * @returns 模块导出。
 */
function load(path, dependencies = {}, globals = {}) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  const compiled = ts.transpileModule(source, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS },
  }).outputText
  const exports = {}
  vm.runInNewContext(compiled, { exports, require: name => {
    assert.ok(name in dependencies, `unexpected dependency: ${name}`)
    return dependencies[name]
  }, ...globals })
  return exports
}
const state = load('../src/composables/portsState.ts')
const snapshot = { supported: true, schemaVersion: 1, revision: 'host-1', configRevision: 'config-1',
  desiredPorts: [3306, 13306], appliedPorts: [13306], actualPorts: [13306, 10080], rulesReady: true,
  state: 'idle', operationId: '', message: '', canApply: true, needsFinalize: false }
assert.equal(JSON.stringify(state.portsDiff(snapshot)), JSON.stringify({ added: [3306], removed: [10080] }))
assert.equal(state.formatPorts([13306, 3306, 3306]), '3306、13306')
assert.equal(state.formatPorts([]), '无')
assert.equal(state.validatePortsStatus(snapshot), snapshot)
for (const invalid of [null, {}, { ...snapshot, schemaVersion: 2 }, { ...snapshot, desiredPorts: [0] },
  { ...snapshot, actualPorts: [65536] }, { ...snapshot, state: 'unknown' }, { ...snapshot, canApply: 'true' }]) {
  assert.throws(() => state.validatePortsStatus(invalid))
}
for (const override of [{ supported: false }, { canApply: false }, { state: 'applying' },
  { state: 'manual_recovery' }, { revision: '' }]) assert.equal(state.portsCanApply({ ...snapshot, ...override }), false)
assert.equal(state.portsCanApply({ ...snapshot, state: 'failed' }), true)

/** 构造页面生命周期、计时器及网络替身。@returns 可观察的测试环境。 */
function harness() {
  let active = false
  let revision = 0
  let unmount
  let listener
  let timer
  let reads = 0
  let writes = 0
  let read = async () => ({ ...snapshot })
  let write = async () => undefined
  const watchers = []
  const document = { visibilityState: 'visible', addEventListener: (_event, callback) => { listener = callback },
    removeEventListener: (_event, callback) => { assert.equal(callback, listener); listener = undefined } }
  const vue = { ref: value => ({ value }), computed: getter => ({ get value() { return getter() } }),
    watch: (_getter, callback, options) => { watchers.push(callback); if (options?.immediate) callback() },
    onUnmounted: callback => { unmount = callback } }
  const api = { ports: () => { reads++; return read() }, applyPorts: input => { writes++; return write(input) } }
  const { usePorts } = load('../src/composables/usePorts.ts', {
    vue, '../api': { api, errorMessage: error => error.message }, './portsState': state,
  }, { document, setInterval: (callback, delay) => { assert.equal(delay, 5000); timer = callback; return 1 },
    clearInterval: () => { timer = undefined } })
  const model = usePorts(() => active, () => revision)
  return { model, document, get reads() { return reads }, get writes() { return writes },
    get timer() { return timer }, get listener() { return listener },
    activate: value => { active = value; watchers[1]() }, revise: () => { revision++; watchers[0]() },
    visibility: value => { document.visibilityState = value; listener() },
    setRead: callback => { read = callback }, setWrite: callback => { write = callback }, unmount: () => unmount() }
}
/** 清空确定数量的微任务，不等待真实时间。@returns 异步处理完成。 */
async function settle() { for (let index = 0; index < 8; index++) await Promise.resolve() }
const env = harness()
const model = env.model
assert.equal(env.reads, 0)
assert.equal(env.timer, undefined)
env.activate(true)
await settle()
assert.equal(env.reads, 1)
assert.equal(model.blocked.value, false)
assert.equal(typeof env.timer, 'function')
env.visibility('hidden')
assert.equal(env.timer, undefined)
await model.refresh()
assert.equal(env.reads, 1)
env.visibility('visible')
await settle()
assert.equal(env.reads, 2)
const input = { revision: 'host-1', configRevision: 'config-1', confirmation: '应用端口变更', finalizePrevious: false }
await model.apply({ ...input, confirmation: 'wrong' })
await model.apply({ ...input, revision: 'stale' })
await model.apply({ ...input, configRevision: 'stale' })
assert.equal(env.writes, 0)
env.setRead(async () => ({ ...snapshot, needsFinalize: true }))
await model.refresh()
await model.apply(input)
assert.equal(env.writes, 0, 'finalizing rollback window requires explicit consent')
env.setWrite(async submitted => {
  assert.equal(submitted.finalizePrevious, true)
  env.setRead(async () => ({ ...snapshot, state: 'applying', canApply: false, operationId: 'operation-1' }))
})
await model.apply({ ...input, finalizePrevious: true })
await settle()
assert.equal(env.writes, 1)
assert.equal(model.status.value.state, 'applying')
assert.equal(model.blocked.value, true)
await model.apply(input)
assert.equal(env.writes, 1)
env.setRead(async () => { throw new Error('disconnected') })
env.timer()
await settle()
assert.equal(model.status.value.operationId, 'operation-1')
assert.match(model.error.value, /最终结果/)
assert.equal(env.writes, 1, 'GET failure must not replay POST')
env.setRead(async () => ({ ...snapshot, actualPorts: [...snapshot.desiredPorts] }))
await model.refresh()
assert.equal(model.blocked.value, false)
env.setWrite(async () => { throw new Error('connection interrupted') })
env.setRead(async () => { throw new Error('offline') })
await model.apply(input)
await settle()
assert.equal(env.writes, 2)
assert.equal(model.blocked.value, true)
await model.apply(input)
env.timer()
await settle()
assert.equal(env.writes, 2, 'uncertain request must not replay')
env.setRead(async () => ({ ...snapshot, state: 'manual_recovery', canApply: true }))
await model.refresh()
assert.equal(model.blocked.value, true)
env.setRead(async () => ({ ...snapshot, supported: false }))
await model.refresh()
assert.equal(model.blocked.value, true)
let resolveStale
env.setRead(() => new Promise(resolve => { resolveStale = resolve }))
const pending = model.refresh()
env.revise()
env.setRead(async () => ({ ...snapshot, configRevision: 'config-2', desiredPorts: [] }))
resolveStale(snapshot)
await pending
await settle()
assert.equal(model.status.value.configRevision, 'config-2', 'mapping changes discard in-flight stale status')
assert.equal(model.status.value.desiredPorts.length, 0)
env.activate(false)
assert.equal(env.timer, undefined)
const previousReads = env.reads
env.revise()
await model.refresh()
assert.equal(env.reads, previousReads)
env.unmount()
assert.equal(env.listener, undefined)

// 在途写入阻止重复点击；组件销毁后响应不得恢复旧会话状态。
const pendingEnv = harness()
pendingEnv.activate(true)
await settle()
let resolveWrite
pendingEnv.setWrite(() => new Promise(resolve => { resolveWrite = resolve }))
const pendingWrite = pendingEnv.model.apply(input)
assert.equal(pendingEnv.model.busy.value, true)
await pendingEnv.model.apply(input)
assert.equal(pendingEnv.writes, 1)
resolveWrite()
await pendingWrite
await settle()
let resolveRead
pendingEnv.setRead(() => new Promise(resolve => { resolveRead = resolve }))
const pendingRead = pendingEnv.model.refresh()
const previousStatus = pendingEnv.model.status.value
pendingEnv.unmount()
resolveRead({ ...snapshot, revision: 'after-unmount' })
await pendingRead
assert.equal(pendingEnv.model.status.value, previousStatus)
assert.equal(pendingEnv.timer, undefined)

/** 验证统一请求层仍使用 CSRF 且只发送白名单字段。@returns 测试完成。 */
async function testRequestContract() {
  const calls = []
  const { api, setCsrfToken } = load('../src/api.ts', {}, {
    window: { setTimeout: () => 1, clearTimeout: () => undefined }, AbortController, Blob, DOMException,
    console: { info: () => undefined, warn: () => undefined },
    fetch: async (path, options) => { calls.push({ path, options }); return { ok: true, status: 202, text: async () => '{}' } },
  })
  setCsrfToken('test-csrf')
  await api.ports()
  await api.applyPorts({ ...input, desiredPorts: [9999] })
  assert.equal(calls[0].path, '/api/ports')
  assert.equal(calls[0].options.method, 'GET')
  assert.equal(calls[1].path, '/api/ports/apply')
  assert.equal(calls[1].options.headers['X-CSRF-Token'], 'test-csrf')
  assert.deepEqual(JSON.parse(calls[1].options.body), input)
}
await testRequestContract()

// 上下板块必须复用统一表格基础样式，不另设小字号或行高。
const panelSource = readFileSync(new URL('../src/components/PortsPanel.vue', import.meta.url), 'utf8')
assert.match(panelSource, /class="page-heading"/)
assert.match(panelSource, /class="panel table-panel"/)
assert.match(panelSource, /class="table-toolbar"/)
assert.doesNotMatch(panelSource, /size="small"|:deep\(|height:\s*42px/)
assert.match(panelSource, /<el-empty[^>]+:image-size="68"/)
console.log('Ports: schema validation, desired/actual diff, CSRF whitelist, visible-only polling, revision invalidation, explicit finalize, no POST replay and lifecycle cleanup passed')
