import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

/** 编译实际 TypeScript 脚本并移除模块语法，供隔离测试执行。
 * @param source TypeScript 内容。
 * @returns 可在 VM 中执行的脚本。
 */
function compile(source) {
  return ts.transpileModule(source.replace(/^import .*$/gm, '').replace(/^export /gm, ''), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText
}

const validation = readFileSync(new URL('../src/validation.ts', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../src/api.ts', import.meta.url), 'utf8')
const component = readFileSync(new URL('../src/components/MappingsPage.vue', import.meta.url), 'utf8')
const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)?.[1]
assert.ok(script, '映射页面脚本必须存在')
const calls = []
const messages = []
const events = []
let pendingResponse
let failure = false
const context = vm.createContext({
  TextEncoder, URL, URLSearchParams, AbortController, Blob, DOMException, Event,
  window: { setTimeout, clearTimeout }, console: { info() {}, warn() {} },
  ref: value => ({ value }), computed: getter => ({ get value() { return getter() } }), watch: () => {},
  defineProps: () => ({ mappings: [], proxies: [{ id: 'proxy' }], loading: false, active: false }),
  defineEmits: () => event => events.push(event), useConnectivity: () => ({}),
  ElMessage: { success: message => messages.push(['success', message]), error: message => messages.push(['error', message]) },
  ElMessageBox: { confirm: () => { throw new Error('复制不得弹出确认框') } },
  /** 捕获真实 api.saveMapping 生成的请求，并模拟异步保存。
   * @param path 请求路径。
   * @param options 请求选项。
   * @returns 模拟 HTTP 响应。
   */
  fetch: async (path, options) => {
    calls.push({ path, ...options, body: JSON.parse(options.body) })
    if (pendingResponse) await pendingResponse
    return { status: failure ? 400 : 200, ok: !failure,
      text: async () => JSON.stringify(failure ? { error: '配置保存失败' } : { id: 'new-id', status: 'stopped' }) }
  },
})
vm.runInContext(compile(validation + '\n' + apiSource + '\n' + script) +
  '\n;globalThis.model = { copyMapping, busy, operationError, mappingCopyName, mappingInput, testResults };', context)
const model = context.model
const original = { id: 'source-id', name: '数据库', listenHost: '127.0.0.1', listenPort: 13306,
  targetHost: 'db.internal', targetPort: 3306, proxyId: 'proxy', enabled: true,
  maxConnections: 32, allowedCIDRs: ['127.0.0.1/32'], status: 'running', activeConnections: 7,
  lastError: '旧诊断', history: [{ ok: true }] }
const originalSnapshot = structuredClone(original)

for (const [name, expected] of [
  ['数据库', '数据库 copy'], ['x'.repeat(100), 'x'.repeat(95) + ' copy'],
  ['x'.repeat(95), 'x'.repeat(95) + ' copy'], ['汉'.repeat(33), '汉'.repeat(31) + ' copy'],
  ['\u{20000}'.repeat(25), '\u{20000}'.repeat(23) + ' copy'],
  ['x'.repeat(94) + '汉字', 'x'.repeat(94) + ' copy'],
  ['数据库 copy', '数据库 copy copy'],
]) {
  const result = model.mappingCopyName(name)
  assert.equal(result, expected)
  assert.ok(new TextEncoder().encode(result).length <= 100)
  assert.equal(new TextDecoder('utf-8', { fatal: true }).decode(new TextEncoder().encode(result)), result)
}

let release
pendingResponse = new Promise(resolve => { release = resolve })
const saving = model.copyMapping(original)
assert.equal(model.busy.value[original.id], 'copy')
await model.copyMapping(original)
assert.equal(calls.length, 1, '请求未结束时双击只发送一次 POST')
release()
await saving
pendingResponse = undefined
assert.equal(calls[0].path, '/api/mappings')
assert.equal(calls[0].method, 'POST')
assert.deepEqual(calls[0].body, { name: '数据库 copy', listenHost: original.listenHost,
  listenPort: original.listenPort, targetHost: original.targetHost, targetPort: original.targetPort,
  proxyId: original.proxyId, enabled: false, maxConnections: original.maxConnections,
  allowedCIDRs: original.allowedCIDRs })
assert.deepEqual(original, originalSnapshot, '复制不得修改原始映射')
assert.equal(model.busy.value[original.id], undefined)
assert.deepEqual(events, ['changed'])
assert.equal(messages.at(-1)[0], 'success')
assert.equal(Object.keys(model.testResults.value).length, 0, '副本不继承测试结果')
const isolatedInput = model.mappingInput(original)
isolatedInput.allowedCIDRs.push('10.0.0.0/8')
assert.deepEqual(original.allowedCIDRs, ['127.0.0.1/32'])

for (const status of ['running', 'stopped', 'error']) {
  await model.copyMapping({ ...original, status, enabled: status !== 'stopped' })
  assert.equal(calls.at(-1).body.enabled, false, `${status} 状态的副本必须停用`)
}
failure = true
const successEvents = events.length
await model.copyMapping(original)
assert.match(model.operationError.value, /配置保存失败/)
assert.equal(messages.at(-1)[0], 'error')
assert.equal(events.length, successEvents, '失败不能通知保存成功')
assert.equal(model.busy.value[original.id], undefined, '失败必须解除 busy')
failure = false
await model.copyMapping(original)
assert.equal(model.operationError.value, '')
assert.equal(events.length, successEvents + 1)

const validCalls = calls.length
await model.copyMapping({ ...original, proxyId: '' })
assert.equal(calls.length, validCalls, '无效输入不发送请求')
assert.match(model.operationError.value, /请选择 SOCKS5 代理/)
assert.match(component, /:loading="busy\[row.id\] === 'copy'" :disabled="!!busy\[row.id\]" @click="copyMapping\(row\)"/)
assert.match(component, /暂无映射，新增后自动启用/)
console.log('mapping copy tests passed')
