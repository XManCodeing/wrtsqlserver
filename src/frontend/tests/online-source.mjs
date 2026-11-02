import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import ts from 'typescript'

/** 编译模块以便注入受控 API 与生命周期。@param path 模块路径。@returns JavaScript。 */
function compile(path) {
  return ts.transpileModule(readFileSync(new URL(path, import.meta.url), 'utf8'), { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ES2022 } }).outputText
}
/** 导入无运行时依赖模块。@param source 模块文本。@returns 模块。 */
async function load(source) { return import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`) }
const state = await load(compile('../src/composables/onlineSourceState.ts'))
const { api, setCsrfToken } = await load(compile('../src/api.ts'))
const calls = []
globalThis.window = { setTimeout, clearTimeout, dispatchEvent() {} }
globalThis.fetch = async (path, options) => {
  calls.push({ path, options })
  return { status: 200, ok: true, text: async () => JSON.stringify(state.defaultOnlineSource) }
}
setCsrfToken('csrf-source')
await api.onlineUpdateSource()
assert.equal(calls.at(-1).path, '/api/online-updates/source')
assert.equal(calls.at(-1).options.method, 'GET')
await api.saveOnlineUpdateSource(state.defaultOnlineSource)
assert.equal(calls.at(-1).options.headers['X-CSRF-Token'], 'csrf-source')
assert.equal(calls.at(-1).options.method, 'POST')
assert.deepEqual(JSON.parse(calls.at(-1).options.body), state.defaultOnlineSource)
assert.equal(state.validateOnlineSource(state.defaultOnlineSource), '')
for (const repositoryUrl of ['http://github.com/a/b', 'https://evil.invalid/a/b', 'https://github.com/a/b?token=x', 'https://github.com/a/..', 'x'.repeat(257)]) assert.notEqual(state.validateOnlineSource({ ...state.defaultOnlineSource, repositoryUrl }), '')
for (const image of ['docker.io/a/b', 'ghcr.io/a/b:latest', 'ghcr.io/A/b', 'ghcr.io/a/b@sha256:abc', 'x'.repeat(257)]) assert.notEqual(state.validateOnlineSource({ ...state.defaultOnlineSource, image }), '')
assert.throws(() => state.checkedOnlineSource(null))
let resolveRead
let rejectRead
let resolveSave
let reads = 0
let writes = 0
let changed = 0
let blocked = false
let watcher
let dispose
const fakeApi = {
  onlineUpdateSource() { reads++; return new Promise((resolve, reject) => { resolveRead = resolve; rejectRead = reject }) },
  saveOnlineUpdateSource() { writes++; return new Promise(resolve => { resolveSave = resolve }) },
}
globalThis.__sourceHarness = {
  ...state, api: fakeApi, errorMessage: error => error.message,
  ref: value => ({ value }), computed: callback => ({ get value() { return callback() } }),
  watch: (_getter, callback) => { watcher = callback; callback(true) }, onUnmounted: callback => { dispose = callback },
}
const source = `const { computed, onUnmounted, ref, watch, api, errorMessage, checkedOnlineSource, validateOnlineSource } = globalThis.__sourceHarness;\n${compile('../src/composables/useOnlineSource.ts').replace(/^import .*;\n/gm, '')}`
const { useOnlineSource } = await load(source)
const model = useOnlineSource(() => true, () => blocked, async () => { changed++ })
assert.equal(model.loading.value, true)
model.open()
await model.save()
assert.equal(model.visible.value, false)
assert.equal(writes, 0, 'cannot save defaults before successful GET')
rejectRead(new Error('来源读取失败'))
await Promise.resolve()
assert.equal(model.disabled.value, true)
assert.match(model.error.value, /读取失败/)
const retry = model.load()
resolveRead(state.defaultOnlineSource)
await retry
model.open()
assert.equal(model.visible.value, true)
model.draft.value.repositoryUrl = 'https://github.com/Another/project'
const retained = { ...model.draft.value }
watcher(true)
assert.equal(reads, 2, 'navigation and theme must not reload a loaded source')
const refresh = model.load()
resolveRead(state.defaultOnlineSource)
await refresh
assert.deepEqual(model.draft.value, retained, 'GET must not overwrite editing draft')
blocked = true
await model.save()
assert.equal(writes, 0, 'busy/manual-recovery/status failure must block saving')
blocked = false
model.draft.value.image = 'https://bad.invalid/image'
await model.save()
assert.equal(writes, 0)
assert.match(model.formError.value, /GHCR/)
model.draft.value.image = state.defaultOnlineSource.image
const saving = model.save()
assert.equal(model.saving.value, true)
await model.save()
assert.equal(writes, 1)
resolveSave({ ...model.draft.value })
await saving
assert.equal(changed, 1)
assert.equal(model.visible.value, false)
assert.equal(model.source.value.repositoryUrl, retained.repositoryUrl)
model.open()
const failedSave = model.save()
resolveSave({ unexpected: true })
await failedSave
assert.equal(model.ready.value, false)
assert.equal(model.visible.value, true)
assert.equal(changed, 1, 'invalid response cannot claim success')
assert.deepEqual(model.draft.value, retained)
dispose()
const component = readFileSync(new URL('../src/components/OnlineUpdateSource.vue', import.meta.url), 'utf8')
assert.equal(/useTheme|localStorage|matchMedia/.test(component), false)
assert.match(component, /:disabled="disabled"/)
const sourceComponent = readFileSync(new URL('../src/components/OnlineUpdateSource.vue', import.meta.url), 'utf8')
assert.match(sourceComponent, /const expanded = ref<string\[\]>\(\[\]\)/, 'source settings must start collapsed')
assert.match(sourceComponent, /<el-collapse v-model="expanded">/)
assert.match(sourceComponent, /source\.value \?\? defaultOnlineSource/, 'unavailable source displays labeled defaults without replacing saved configuration')
assert.match(sourceComponent, /尚未确认实际配置/)
assert.match(sourceComponent, /displayedSource\.repositoryUrl/)
assert.match(sourceComponent, /displayedSource\.image/)
const parent = readFileSync(new URL('../src/components/OnlineUpdates.vue', import.meta.url), 'utf8')
assert.match(parent, /sourceSaved[\s\S]*dialog\.value = null[\s\S]*target\.value = null[\s\S]*invalidateSource\(\)/)
assert.match(parent, /<el-popover[^>]*trigger="click"/)
assert.match(parent, /useOnlineUpdates\(\(\) => opened\.value\)/, 'poll only while the compact panel is open')
const app = readFileSync(new URL('../src/App.vue', import.meta.url), 'utf8')
assert.match(app, /<OnlineUpdates v-if="authenticated"/)
assert.doesNotMatch(app, /index="updates"|<UpdatesPage/)
console.log('Online source: fixed APIs, CSRF, validation, loading/failure/save guards, draft preservation and cache/confirmation invalidation passed')
