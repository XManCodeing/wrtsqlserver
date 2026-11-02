import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'
import { ref, computed, watch, nextTick, effectScope } from 'vue'

/** 在内存中编译模块，注入依赖，测试不访问真实 Redis。@param path 源文件路径。@param dependencies 注入依赖。@returns 模块导出。 */
function loadModule(path, dependencies) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8')
  const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS } })
  const exports = {}
  new Function('require', 'exports', outputText)(name => dependencies[name], exports)
  return exports
}

const { api, setCsrfToken } = loadModule('../src/api.ts', {})
const calls = []
globalThis.window = { setTimeout, clearTimeout, dispatchEvent() {} }
globalThis.fetch = async (path, options) => {
  calls.push({ path, options })
  return { status: 200, ok: true, text: async () => JSON.stringify({ accepted: true }) }
}
setCsrfToken('test-csrf')
await api.redisKeys('db1', '18446744073709551615', 'user:&*')
const query = new URL(calls.at(-1).path, 'http://localhost')
assert.equal(query.pathname, '/api/redis/databases/db1/keys')
assert.equal(query.searchParams.get('cursor'), '18446744073709551615')
assert.equal(query.searchParams.get('pattern'), 'user:&*')
assert.equal(calls.at(-1).options.method, 'GET')
await api.redisValue('db1', '')
assert.equal(new URL(calls.at(-1).path, 'http://localhost').searchParams.get('keyToken'), '')
await api.clearRedis('db1', '清空 db1', 'redis:6379')
assert.equal(calls.at(-1).options.method, 'POST')
assert.equal(calls.at(-1).options.headers['X-CSRF-Token'], 'test-csrf')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), { confirmation: '清空 db1', address: 'redis:6379' })
let clearAttempts = 0
globalThis.fetch = async () => { clearAttempts++; throw new DOMException('timeout', 'AbortError') }
await assert.rejects(api.clearRedis('db1', '清空 db1', 'redis:6379'), /执行结果不确定/)
assert.equal(clearAttempts, 1, '清空超时不能自动重试')
globalThis.fetch = async () => { throw new TypeError('network failure') }
await assert.rejects(api.clearRedis('db1', '清空 db1', 'redis:6379'), /不要直接重复清空/)

const pendingScans = []
const pendingValues = []
const scanCalls = []
const cleanup = []
const mockApi = {
  redisKeys(database, cursor, pattern) {
    scanCalls.push({ database, cursor, pattern })
    return new Promise((resolve, reject) => pendingScans.push({ resolve, reject }))
  },
  redisValue(database, keyToken) {
    return new Promise((resolve, reject) => pendingValues.push({ database, keyToken, resolve, reject }))
  },
}
const { useRedisKeys } = loadModule('../src/composables/useRedisKeys.ts', {
  vue: { ref, computed, watch, onUnmounted: callback => cleanup.push(callback) },
  '../api': { api: mockApi, errorMessage: error => error.message },
})
const scope = effectScope()
const database = ref('db0')
const enabled = ref(true)
const state = scope.run(() => useRedisKeys(() => database.value, () => enabled.value))
/** 推进Vue侦听器和Promise任务。@returns 微任务完成。 */
async function tick() { await nextTick(); await Promise.resolve(); await nextTick() }
assert.equal(pendingScans.length, 1)
database.value = 'db1'
await tick()
pendingScans[0].resolve({ keys: [{ key: 'old', keyToken: 'b2xk' }], nextCursor: '0' })
await tick()
assert.equal(state.keys.value.length, 0, '切库后的旧扫描响应必须丢弃')
const batch = Array.from({ length: 12 }, (_, index) => ({ key: `key${index}`, keyToken: `token${index}` }))
batch[0] = { key: 'binary', keyToken: '__proto__' }
pendingScans[1].resolve({ keys: [...batch, batch[1]], nextCursor: '99' })
await tick()
assert.equal(state.keys.value.length, 12, '批内去重但不截断SCAN返回')
assert.equal(state.rows.value.length, 10)
assert.equal(pendingValues.length, 3, '只允许三个并发预览')
assert.equal(state.values.value.__proto__, undefined, '令牌不能命中Object原型')
state.page.value = 2
await tick()
assert.equal(pendingValues.length, 5, '新页仅请求两条')
pendingValues[0].resolve({ keyToken: '__proto__', value: 'stale' })
pendingValues[3].resolve({ keyToken: 'token10', value: 'current' })
pendingValues[4].reject(new Error('preview failed'))
await tick()
assert.equal(state.values.value.__proto__, undefined, '换页后旧预览不能回写')
assert.equal(state.values.value.token10.value, 'current')
assert.equal(state.failures.value.token11, 'preview failed')
const nextBatch = state.scan(false)
assert.equal(scanCalls.at(-1).cursor, '99')
pendingScans.at(-1).resolve({ keys: [], nextCursor: '101' })
await nextBatch; await tick()
assert.equal(state.nextCursor.value, '101', '空批次不代表扫描结束')
assert.equal(state.keys.value.length, 0)
state.pattern.value = 'new:*'
const failedScan = state.scan()
pendingScans.at(-1).reject(new Error('scan failed'))
await failedScan; await tick()
assert.equal(state.nextCursor.value, '0', '新筛选失败不能复用旧筛选游标')
assert.equal(state.scanned.value, false)
state.pattern.value = '中'.repeat(86)
await state.scan()
assert.match(state.error.value, /256/)
enabled.value = false
await tick()
assert.equal(state.keys.value.length, 0)
assert.equal(state.error.value, '')
for (const callback of cleanup) callback()
scope.stop()
console.log('Redis keys: API encoding, CSRF, concurrency, pagination, empty batches and stale-response protection passed')
