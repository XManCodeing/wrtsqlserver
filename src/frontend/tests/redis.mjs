import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

// 纯格式化测试不连接Redis；确保未知值与真实0严格区分。
const source = readFileSync(new URL('../src/composables/redisState.ts', import.meta.url), 'utf8')
const { outputText } = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } })
const { redisBytes, redisUptime, redisState } = await import(`data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`)
assert.equal(redisBytes(null), '—')
assert.equal(redisBytes(undefined), '—')
assert.equal(redisBytes(-1), '—')
assert.equal(redisBytes(NaN), '—')
assert.equal(redisBytes(0), '0 B')
assert.equal(redisBytes(128 * 1024 * 1024), '128.0 MiB')
assert.equal(redisUptime(null), '—')
assert.equal(redisUptime(0), '0 天 0 小时 0 分钟')
assert.equal(redisUptime(90060), '1 天 1 小时 1 分钟')
assert.equal(redisState(null).label, '状态未知')
assert.equal(redisState({ state: 'unconfigured' }).label, '未配置')
assert.equal(redisState({ state: 'online' }).type, 'success')
assert.equal(redisState({ state: 'unreachable' }).type, 'danger')
assert.equal(redisState({ state: 'error' }).label, '读取异常')
console.log('Redis state: 14 assertions passed')
