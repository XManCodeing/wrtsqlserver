import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// 在内存中编译纯状态函数，不生成文件，也不连接真实代理或数据库。
const source = readFileSync(new URL('../src/composables/connectivityState.ts', import.meta.url), 'utf8')
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } })
const { connectivityBars, PROBE_INTERVAL_MS } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
const now = Date.UTC(2026, 10, 2, 10, 0, 30)
const minute = Math.floor(now / PROBE_INTERVAL_MS) * PROBE_INTERVAL_MS
const empty = connectivityBars([], now)
assert.equal(empty.length, 60)
assert.ok(empty.every(bar => bar.state === 'unknown'))
assert.equal(empty[0].time, minute - 59 * PROBE_INTERVAL_MS)
assert.equal(empty[59].time, minute)
const bars = connectivityBars([
  { checkedAt: minute + 2000, ok: false, latencyMs: 10000, error: 'timeout' },
  { checkedAt: minute + 1000, ok: true, latencyMs: 60 },
  { checkedAt: minute - 60000, ok: true, latencyMs: 100 },
], now)
assert.equal(bars[59].state, 'failure')
assert.equal(bars[59].sample.latencyMs, 10000)
assert.equal(bars[58].state, 'success')
assert.equal(bars[57].state, 'unknown')
const boundary = connectivityBars([
  { checkedAt: minute - 60 * PROBE_INTERVAL_MS, ok: true, latencyMs: 1 },
  { checkedAt: minute - 59 * PROBE_INTERVAL_MS, ok: false, latencyMs: 1 },
  { checkedAt: now + 1, ok: true, latencyMs: 1 },
  { checkedAt: NaN, ok: true, latencyMs: 1 },
], now)
assert.equal(boundary[0].state, 'failure')
assert.equal(boundary.filter(bar => bar.sample).length, 1)
const advanced = connectivityBars(boundary.flatMap(bar => bar.sample ? [bar.sample] : []), now + PROBE_INTERVAL_MS)
assert.ok(advanced.every(bar => bar.state === 'unknown'))
const staged = connectivityBars([
  { checkedAt: minute, ok: false, latencyMs: 500, proxy: { ok: true, latencyMs: 80 } },
  { checkedAt: minute - PROBE_INTERVAL_MS, ok: false, latencyMs: 100, proxy: { ok: false, latencyMs: 100 } },
], now)
assert.equal(staged[59].sample.proxy.ok, true)
assert.equal(staged[59].state, 'failure')
assert.equal(staged[58].sample.proxy.ok, false)
assert.equal(bars[59].sample.proxy, undefined)
console.log('Connectivity history: 15 assertions passed')
