<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, errorMessage } from '../api'
import { byteLength, validateProxy } from '../validation'
import type { ProxyInput, ProxyRecord } from '../types'

const emit = defineEmits<{ saved: [] }>()
const visible = ref(false)
const saving = ref(false)
const error = ref('')
const editId = ref<string>()
const hadPassword = ref(false)
const draft = reactive({ name: '', host: '', port: 1080, username: '', password: '', clearPassword: false })
const title = computed(() => editId.value ? '编辑 SOCKS5 代理' : '新增 SOCKS5 代理')

/** 打开代理草稿；从不读取或回显已有密码。
 * @param record 可选已存在代理。
 * @returns 无返回值。
 */
function open(record?: ProxyRecord): void {
  editId.value = record?.id
  hadPassword.value = record?.passwordSet ?? false
  Object.assign(draft, {
    name: record?.name ?? '', host: record?.host ?? '', port: record?.port ?? 1080,
    username: record?.username ?? '', password: '', clearPassword: false,
  })
  error.value = ''
  visible.value = true
}

/** 提交校验后的代理配置。
 * @returns 保存完成后的 Promise。
 */
async function save(): Promise<void> {
  if (saving.value) return
  const input: ProxyInput = {
    name: draft.name.trim(), host: draft.host.trim(), port: draft.port, username: draft.username.trim(),
    ...(draft.clearPassword ? { clearPassword: true } : draft.password ? { password: draft.password } : {}),
  }
  error.value = validateProxy(input)
  if (error.value) return
  saving.value = true
  try {
    if (editId.value) {
      try {
        await ElMessageBox.confirm('修改代理可能影响引用它的映射连接。确定保存配置？', '保存代理', {
          type: 'warning', confirmButtonText: '保存配置', cancelButtonText: '取消',
        })
      } catch { return }
    }
    await api.saveProxy(input, editId.value)
    visible.value = false
    draft.password = ''
    ElMessage.success('代理已保存')
    emit('saved')
  } catch (failure) { error.value = errorMessage(failure) }
  finally { saving.value = false }
}

/** 关闭时移除未提交密码。
 * @returns 无返回值。
 */
function clearSensitive(): void { draft.password = '' }

defineExpose({ open })
</script>

<template>
  <el-dialog v-model="visible" :title="title" width="540px" :close-on-click-modal="false" :close-on-press-escape="!saving" :show-close="!saving" @closed="clearSensitive">
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon />
    <el-form label-position="top" :disabled="saving" @submit.prevent="save">
      <el-form-item label="代理名称" required><el-input v-model="draft.name" maxlength="100" placeholder="为代理命名" /><span class="field-help">{{ byteLength(draft.name) }} / 100 UTF-8 字节</span></el-form-item>
      <div class="form-grid host-port">
        <el-form-item label="SOCKS5 主机" required><el-input v-model="draft.host" placeholder="主机名或 IP" /></el-form-item>
        <el-form-item label="端口" required><el-input-number v-model="draft.port" :min="1" :max="65535" controls-position="right" /></el-form-item>
      </div>
      <el-form-item label="用户名（可选）"><el-input v-model="draft.username" maxlength="255" autocomplete="off" /><span class="field-help">用户名和密码分别不超过 255 UTF-8 字节。</span></el-form-item>
      <el-form-item label="密码（可选）">
        <el-input v-model="draft.password" type="password" autocomplete="new-password" maxlength="255" :disabled="draft.clearPassword" :placeholder="hadPassword ? '留空保留原有密码' : '未设置密码'" />
        <span v-if="hadPassword" class="field-help">已存储密码不会回显；留空即保留。</span>
      </el-form-item>
      <el-checkbox v-if="hadPassword" v-model="draft.clearPassword" @change="draft.password = ''">删除已存储的代理密码</el-checkbox>
      <p class="field-help">连通性测试在端口映射中手动执行，不会自动连接目标。</p>
    </el-form>
    <template #footer>
      <el-button :disabled="saving" @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="save">保存代理</el-button>
    </template>
  </el-dialog>
</template>
