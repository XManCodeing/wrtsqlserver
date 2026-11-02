<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { api, errorMessage } from '../api'
import type { UpdateStatus } from '../types'

const props = defineProps<{ active: boolean }>()
const status = ref<UpdateStatus | null>(null)
const loading = ref(false)
const busy = ref(false)
const error = ref('')
const file = ref<File | null>(null)
const input = ref<HTMLInputElement>()
let disposed = false
const rows = computed(() => status.value?.package ? [status.value.package] : [])
const limit = computed(() => Math.floor((status.value?.maxUploadBytes ?? 0) / 1024 / 1024))

/** 刷新能力与包摘要；失败保留上次结果并禁用写操作。@returns 请求完成。 */
async function refresh(): Promise<void> {
  if (loading.value || busy.value) return
  loading.value = true
  try {
    const result = await api.updates()
    if (!disposed) { status.value = result; error.value = '' }
  } catch (failure) { if (!disposed) error.value = errorMessage(failure) }
  finally { if (!disposed) loading.value = false }
}

/** 校验选中文件的大小和扩展名，服务端仍执行完整签名验证。@param event 选择事件。@returns 无。 */
function selectFile(event: Event): void {
  const candidate = (event.target as HTMLInputElement).files?.[0]
  file.value = null
  if (!candidate) return
  if (!candidate.name.toLowerCase().endsWith('.tar') || !candidate.size || candidate.size > (status.value?.maxUploadBytes ?? 0)) {
    ElMessage.error(`请选择非空的 .tar 更新包，大小不超过 ${limit.value} MB。`)
    if (input.value) input.value.value = ''
    return
  }
  file.value = candidate
}

/** 上传并等待校验，不调用任何执行更新接口。@returns 操作结束。 */
async function upload(): Promise<void> {
  if (!file.value || busy.value || !status.value?.uploadEnabled || error.value) return
  if (status.value.package) {
    try { await ElMessageBox.confirm('校验成功后将替换原暂存包，不影响正在运行的服务。', '替换暂存包', { confirmButtonText: '上传并校验', cancelButtonText: '取消' }) }
    catch { return }
  }
  busy.value = true
  try {
    const result = await api.uploadUpdate(file.value)
    if (disposed) return
    status.value.package = result
    file.value = null
    if (input.value) input.value.value = ''
    ElMessage.success('更新包已校验，尚未执行更新。')
  } catch (failure) { if (!disposed) error.value = errorMessage(failure) }
  finally { if (!disposed) busy.value = false }
}

/** 确认后删除暂存包，绝不删除运行配置或镜像。@returns 操作结束。 */
async function remove(): Promise<void> {
  if (busy.value || !status.value?.package) return
  try { await ElMessageBox.confirm('仅删除此暂存更新包，当前服务和配置不变。', '删除暂存包', { confirmButtonText: '删除', cancelButtonText: '取消', type: 'warning' }) }
  catch { return }
  busy.value = true
  try {
    await api.deleteUpdate()
    if (!disposed) { status.value.package = null; ElMessage.success('暂存包已删除。') }
  } catch (failure) { if (!disposed) error.value = errorMessage(failure) }
  finally { if (!disposed) busy.value = false }
}

watch(() => props.active, active => { if (active) void refresh() }, { immediate: true })
onUnmounted(() => { disposed = true })
</script>

<template>
  <section>
    <div class="section-heading"><h2>离线暂存</h2><el-button :loading="loading" :disabled="busy" @click="refresh">刷新暂存状态</el-button></div>
    <el-alert v-if="error" :title="error" type="error" :closable="false" show-icon class="global-error" />
    <section v-loading="loading" class="panel overview-section">
      <h2>更新准备</h2>
      <el-descriptions :column="2" border>
        <el-descriptions-item label="当前版本">{{ status?.currentVersion || '—' }}</el-descriptions-item>
        <el-descriptions-item label="部署方式">独立替换转发容器</el-descriptions-item>
        <el-descriptions-item label="更新包上传"><el-tag :type="status?.uploadEnabled ? 'success' : 'info'" effect="plain">{{ status?.uploadEnabled ? '已启用' : '未启用' }}</el-tag></el-descriptions-item>
        <el-descriptions-item label="暂存包安装"><el-tag type="info" effect="plain">不支持</el-tag></el-descriptions-item>
      </el-descriptions>
      <p class="field-help">{{ status?.uploadEnabled ? '仅上传、校验与暂存签名包，不安装，不重启服务。' : status?.reason || (error ? '状态读取失败，请刷新重试。' : '正在读取更新配置。') }}</p>
    </section>
    <div class="panel table-panel">
      <div class="table-toolbar update-toolbar">
        <input ref="input" type="file" accept=".tar" class="update-file-input" aria-label="选择更新包"
          :disabled="busy || loading || !status?.uploadEnabled || !!error" @change="selectFile" />
        <el-button :disabled="busy || loading || !status?.uploadEnabled || !!error" @click="input?.click()">选择更新包</el-button>
        <span class="muted update-filename" :title="file?.name">{{ file?.name || '未选择文件' }}</span>
        <el-button type="primary" :loading="busy" :disabled="!file || loading || !status?.uploadEnabled || !!error" @click="upload">{{ busy ? '处理中' : '上传并校验' }}</el-button>
      </div>
      <el-table :data="rows" row-key="sha256">
        <el-table-column prop="version" label="版本" width="130" />
        <el-table-column prop="platform" label="平台" width="140" />
        <el-table-column prop="imageTag" label="镜像" min-width="190" show-overflow-tooltip />
        <el-table-column label="大小" width="110"><template #default="{ row }">{{ (row.size / 1024 / 1024).toFixed(1) }} MB</template></el-table-column>
        <el-table-column label="状态" width="120"><template #default><el-tag type="success" effect="plain">已校验</el-tag></template></el-table-column>
        <el-table-column label="操作" width="130"><template #default><el-button link type="danger" :disabled="busy || loading || !!error" @click="remove">删除暂存包</el-button></template></el-table-column>
        <template #empty><el-empty description="暂无已校验的更新包" :image-size="68" /></template>
      </el-table>
    </div>
    <p class="field-help">仅接受签名的 linux/amd64 更新包{{ limit ? `，最大 ${limit} MB` : '' }}。校验不会安装或重启服务；此暂存包不用于上方在线更新。</p>
  </section>
</template>

<style scoped>
.update-file-input { display: none; }
.update-toolbar { flex-wrap: wrap; }
.update-filename { flex: 1; min-width: 100px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; }
</style>
