import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import vm from 'node:vm'
import ts from 'typescript'

const component = readFileSync(new URL('../src/components/MappingDialog.vue', import.meta.url), 'utf8')
const script = component.match(/<script setup lang="ts">([\s\S]*?)<\/script>/)?.[1]
assert.ok(script, '映射弹窗脚本必须存在')
const calls = []
const messages = []
let resultStatus = 'running'
let failure = false
const context = vm.createContext({
  ref: value => ({ value }), reactive: value => value,
  defineProps: () => ({ proxies: [{ id: 'proxy-test' }] }),
  defineEmits: () => () => {}, defineExpose: () => {},
  validateMapping: () => '', mappingInput: record => ({ ...record }),
  safeMessage: value => value, errorMessage: error => error.message,
  ElMessage: { success: message => messages.push(message) },
  ElMessageBox: { confirm: async () => {} },
  api: {
    /** 模拟保存结果，捕获启用参数与重试 ID。@param input 配置。@param id 映射 ID。@returns 保存结果。 */
    async saveMapping(input, id) {
      calls.push({ input, id })
      if (failure) throw new Error('网络失败')
      return { ...input, id: id || 'created-id', status: resultStatus, lastError: resultStatus === 'error' ? '端口占用' : '' }
    },
  },
})
const compiled = ts.transpileModule(script.replace(/^import .*$/gm, '') + '\n;(globalThis as any).model = { open, save, draft, error, visible };', {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
}).outputText
vm.runInContext(compiled, context)
const model = context.model

/** 打开有效的新建草稿。@returns 无返回值。 */
function openNew() {
  model.open()
  Object.assign(model.draft, { name: '测试映射', proxyId: 'proxy-test', listenPort: 13306 })
}

openNew()
assert.equal(model.draft.enabled, true)
await model.save()
assert.equal(calls.length, 1, '创建并启用必须通过单次保存，不追加 toggle 请求')
assert.equal(calls[0].input.enabled, true)
assert.equal(model.visible.value, false)
assert.equal(messages.at(-1), '映射已创建并启用')

model.open({ ...calls[0].input, id: 'disabled-id', enabled: false, allowedCIDRs: ['127.0.0.1/32'] })
await model.save()
assert.equal(calls.at(-1).input.enabled, false, '编辑已停用映射不能擅自启用')

openNew()
resultStatus = 'error'
const messageCount = messages.length
await model.save()
assert.equal(model.visible.value, true)
assert.match(model.error.value, /监听启动失败.*端口占用/)
assert.equal(messages.length, messageCount, '启动失败不能弹出成功提示')
resultStatus = 'running'
await model.save()
assert.equal(calls.at(-1).id, 'created-id', '监听失败后修正保存必须更新原记录，避免重复创建')

openNew()
failure = true
await model.save()
assert.equal(model.visible.value, true)
assert.equal(model.error.value, '网络失败')
assert.match(component, /创建后自动启用/)
console.log('mapping creation auto-enable tests passed')
