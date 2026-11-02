<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import type { PortsStatus } from '../composables/portsState'
import { safeMessage } from '../api'
import { usePorts } from '../composables/usePorts'
import { formatPorts, portsDiff, type PortsApplyInput } from '../composables/portsState'

const props = defineProps<{ active: boolean; revision: number }>()
const emit = defineEmits<{ status: [value: PortsStatus | null] }>()
const { status, loading, busy, error, blocked, refresh, apply } = usePorts(() => props.active, () => props.revision)
watch([status, error], () => emit('status', error.value ? null : status.value), { immediate: true })
const opened = ref(false)
const target = ref<{ revision: string; configRevision: string; needsFinalize: boolean; added: number[]; removed: number[] } | null>(null)
const difference = computed(() => portsDiff(status.value))
const stateLabels = { idle: '已就绪', applying: '正在应用', failed: '应用失败', manual_recovery: '需手动恢复' }
const stale = computed(() => !!target.value && (target.value.revision !== status.value?.revision ||
  target.value.configRevision !== status.value?.configRevision || target.value.needsFinalize !== status.value?.needsFinalize))
const confirmationBlocked = computed(() => blocked.value || stale.value)
const summary = computed(() => status.value ? [{
  desired: formatPorts(status.value.desiredPorts), applied: formatPorts(status.value.appliedPorts),
  actual: formatPorts(status.value.actualPorts), rules: status.value.rulesReady,
}] : [])

/** 冻结当前差异与版本，轮询不能悄悄改变用户授权范围。@returns 无。 */
function openConfirmation(): void {
  if (blocked.value || !status.value) return
  target.value = { revision: status.value.revision, configRevision: status.value.configRevision,
    needsFinalize: status.value.needsFinalize, added: [...difference.value.added], removed: [...difference.value.removed] }
  opened.value = true
}

/** 确认按钮授权弹窗展示的变更及回退窗口处理，保留协议字段与版本校验。@returns 请求结束。 */
async function confirm(): Promise<void> {
  if (!opened.value || confirmationBlocked.value || !target.value) return
  const input: PortsApplyInput = { revision: target.value.revision, configRevision: target.value.configRevision,
    confirmation: '应用端口变更', finalizePrevious: target.value.needsFinalize }
  opened.value = false
  await apply(input)
}
</script>

<template>
  <section class="ports-panel" aria-label="宿主端口管理">
    <header class="page-heading">
      <div><h1>宿主端口管理</h1><p>管理 Docker 发布端口与 LAN 例外，保存映射后在此应用变更。</p></div>
      <el-button type="primary" :loading="busy || status?.state === 'applying'" :disabled="blocked" @click="openConfirmation">应用端口变更</el-button>
    </header>
    <el-alert v-if="error" :title="error" type="warning" :closable="false" show-icon />
    <el-alert v-if="status && !status.supported" title="当前宿主更新器不支持端口管理，需要手动升级宿主后再应用。" type="info" :closable="false" show-icon />
    <el-alert v-else-if="status?.state === 'manual_recovery'" title="端口状态需要手动恢复，请在宿主检查后刷新；此处不能继续应用。" type="error" :closable="false" show-icon />
    <div class="panel table-panel">
      <div class="table-toolbar">
        <el-tag v-if="status" effect="plain" :type="!status.supported ? 'info' : status.state === 'failed' || status.state === 'manual_recovery' ? 'danger' : status.state === 'applying' ? 'warning' : 'info'">
          {{ status.supported ? stateLabels[status.state] : '宿主不支持' }}
        </el-tag>
        <span v-else class="muted">{{ loading ? '正在读取状态' : '状态暂不可用' }}</span>
        <div class="toolbar-count"><el-button link type="primary" :loading="loading" :disabled="busy" @click="refresh">刷新状态</el-button></div>
      </div>
      <el-table v-loading="loading && !status" :data="summary" table-layout="fixed">
        <el-table-column prop="desired" label="期望端口" min-width="170" show-overflow-tooltip />
        <el-table-column prop="applied" label="上次应用端口" min-width="150" show-overflow-tooltip />
        <el-table-column prop="actual" label="实际发布端口" min-width="170" show-overflow-tooltip />
        <el-table-column label="LAN 例外规则" width="150"><template #default="{ row }"><el-tag effect="plain" :type="row.rules ? 'success' : 'warning'">{{ row.rules ? '已就绪' : '未就绪' }}</el-tag></template></el-table-column>
        <template #empty><el-empty :description="loading ? '正在读取端口状态…' : error ? '端口状态暂不可用，请刷新重试' : '暂无端口状态'" :image-size="68" /></template>
      </el-table>
    </div>
    <div v-if="status" class="ports-diff" role="status">
      <span>待新增：<span class="endpoint">{{ formatPorts(difference.added) }}</span></span>
      <span>待移除：<span class="endpoint">{{ formatPorts(difference.removed) }}</span></span>
      <span v-if="!difference.added.length && !difference.removed.length" class="muted">发布集合与期望一致</span>
    </div>
    <p v-if="status?.message" class="field-help" role="status">{{ safeMessage(status.message) }}</p>
    <p v-if="status?.needsFinalize" class="field-help">应用本次端口变更将结束旧版本回退窗口，需在确认时明确同意。</p>
    <p class="field-help">端口由已保存映射与宿主配置计算。应用可能重建服务，短暂中断网页与转发连接；LAN 例外不改变现有访问权限。</p>
    <el-dialog v-model="opened" title="应用端口变更" width="540px" :close-on-click-modal="false">
      <el-alert title="应用将短暂中断网页与转发连接，可能需要重新登录。断联不代表失败，恢复后仅刷新状态，不要重复提交。" type="warning" :closable="false" show-icon />
      <p class="field-help">待新增：{{ formatPorts(target?.added || []) }}；待移除：{{ formatPorts(target?.removed || []) }}。</p>
      <el-alert v-if="stale" title="映射或宿主状态已变化，请取消并重新确认最新差异。" type="warning" :closable="false" />
      <p v-if="target?.needsFinalize" class="field-help">确认应用将结束旧版本回退窗口，之后无法通过本次保留记录回退到旧版本。</p>
      <template #footer><el-button @click="opened = false">取消</el-button><el-button type="primary" :disabled="confirmationBlocked" @click="confirm">确认应用</el-button></template>
    </el-dialog>
  </section>
</template>

<style scoped>
.ports-panel { margin-bottom: 32px; }
.ports-panel > .el-alert { margin-bottom: 20px; }
.ports-diff { display: flex; flex-wrap: wrap; gap: 8px 20px; margin-top: 10px; font-size: 12px; overflow-wrap: anywhere; }
</style>
