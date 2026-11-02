<script setup lang="ts">
import { reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, errorMessage, safeMessage } from '../api'
import { byteLength, mappingInput, validateMapping } from '../validation'
import type { MappingInput, MappingRecord, ProxyRecord } from '../types'

const props = defineProps<{ proxies: ProxyRecord[] }>()
const emit = defineEmits<{ saved: [] }>()
const visible = ref(false)
const saving = ref(false)
const error = ref('')
const editId = ref<string>()
const cidrText = ref('127.0.0.1/32')
const draft = reactive<MappingInput>(defaults())

/** 生成保守的新建配置，不填充实际目标。
 * @returns 默认启用的映射草稿。
 */
function defaults(): MappingInput {
  return { name: '', listenHost: '127.0.0.1', listenPort: 3306, targetHost: '', targetPort: 3306,
    proxyId: '', enabled: true, maxConnections: 32, allowedCIDRs: ['127.0.0.1/32'] }
}

/** 创建独立编辑快照，后续轮询不修改它。
 * @param record 可选已有映射。
 * @returns 无返回值。
 */
function open(record?: MappingRecord): void {
  editId.value = record?.id
  Object.assign(draft, record ? mappingInput(record) : defaults())
  cidrText.value = draft.allowedCIDRs.join('\n')
  error.value = ''
  visible.value = true
}

/** 校验、确认并保存映射；编辑会提示已有连接受影响。
 * @returns 保存完成后的 Promise。
 */
async function save(): Promise<void> {
  if (saving.value) return
  const input: MappingInput = {
    ...draft, enabled: editId.value ? draft.enabled : true,
    name: draft.name.trim(), listenHost: draft.listenHost.trim(), targetHost: draft.targetHost.trim(),
    allowedCIDRs: cidrText.value.split(/[\n,]+/).map(value => value.trim()).filter(Boolean),
  }
  error.value = validateMapping(input)
  if (!error.value && !props.proxies.some(proxy => proxy.id === input.proxyId)) error.value = '所选代理已不存在，请重新选择。'
  if (error.value) return
  saving.value = true
  try {
    if (editId.value) {
      try {
        await ElMessageBox.confirm('保存将重新应用此映射配置，该映射已有连接可能中断。其他映射和容器不重启。', '保存映射', {
          type: 'warning', confirmButtonText: '确认保存', cancelButtonText: '取消',
        })
      } catch { return }
    }
    const saved = await api.saveMapping(input, editId.value)
    emit('saved')
    if (saved.status === 'error') {
      // 保存成功不等于监听成功；保留弹窗及错误，允许用户修正后重试。
      editId.value = saved.id
      error.value = `配置已保存，但监听启动失败：${safeMessage(saved.lastError || '请检查监听地址和端口占用。')}`
      return
    }
    visible.value = false
    ElMessage.success(editId.value ? '映射已保存' : '映射已创建并启用')
  } catch (failure) { error.value = errorMessage(failure) }
  finally { saving.value = false }
}

defineExpose({ open })
</script>

<template>
  <el-dialog v-model="visible" :title="editId ? '编辑端口映射' : '新增端口映射'" width="680px" :close-on-click-modal="false" :close-on-press-escape="!saving" :show-close="!saving">
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <el-form label-position="top" :disabled="saving" @submit.prevent="save">
      <el-form-item label="映射名称" required><el-input v-model="draft.name" maxlength="100" placeholder="为映射命名" /><span class="field-help">{{ byteLength(draft.name) }} / 100 UTF-8 字节</span></el-form-item>
      <div class="form-grid host-port">
        <el-form-item label="监听地址" required><el-input v-model="draft.listenHost" placeholder="本机监听 IP" /></el-form-item>
        <el-form-item label="监听端口" required><el-input-number v-model="draft.listenPort" :min="1" :max="65535" controls-position="right" /></el-form-item>
      </div>
      <p class="field-help compact-help">Docker bridge 模式需监听 0.0.0.0；未发布端口将在上方显示待应用，确认应用后才可从 LAN 访问。不能绑定宿主机 LAN IP。</p>
      <el-alert v-if="draft.listenHost.trim() === '0.0.0.0'" title="将监听所有 IPv4 接口，请仅允许可信来源网段。" type="warning" :closable="false" show-icon class="setup-alert" />
      <div class="form-grid host-port">
        <el-form-item label="目标主机" required><el-input v-model="draft.targetHost" placeholder="由 SOCKS5 代理访问的主机名或 IP" /></el-form-item>
        <el-form-item label="目标端口" required><el-input-number v-model="draft.targetPort" :min="1" :max="65535" controls-position="right" /></el-form-item>
      </div>
      <div class="form-grid host-port">
        <el-form-item label="SOCKS5 代理" required>
          <el-select v-model="draft.proxyId" placeholder="选择代理" :disabled="!proxies.length">
            <el-option v-for="proxy in proxies" :key="proxy.id" :label="proxy.name" :value="proxy.id" />
          </el-select>
        </el-form-item>
        <el-form-item label="最大连接数" required><el-input-number v-model="draft.maxConnections" :min="1" :max="4096" controls-position="right" /></el-form-item>
      </div>
      <el-form-item label="允许来源 CIDR" required>
        <el-input v-model="cidrText" type="textarea" :rows="3" placeholder="每行一条 IPv4 CIDR，最多 64 条，不允许 /0" />
        <span class="field-help">填写实际允许访问的网段；127.0.0.1/32 只允许本机。</span>
      </el-form-item>
      <div class="muted">{{ !editId ? '创建后自动启用，请确认监听地址和允许来源。' : draft.enabled ? '当前已启用，保存会重新应用配置。' : '当前未启用，保存后可从列表手动启用。' }}</div>
    </el-form>
    <template #footer>
      <el-button :disabled="saving" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="saving" :disabled="!proxies.length" @click="save">保存映射</el-button>
    </template>
  </el-dialog>
</template>
