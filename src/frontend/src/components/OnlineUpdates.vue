<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, safeMessage } from '../api'
import { onlinePhaseLabel } from '../composables/onlineUpdateState'
import { useOnlineUpdates } from '../composables/useOnlineUpdates'
import OnlineUpdateSource from './OnlineUpdateSource.vue'
import UpdatesPage from './UpdatesPage.vue'

const opened = ref(false)
const offlineOpen = ref(false)
const { status, loading, busy, error, blocked, refresh, submit, invalidateSource } = useOnlineUpdates(() => opened.value)
const initialVersion = ref('')
// 只读取本地版本，不自动联网检测新版，也不在关闭浮层时轮询。
onMounted(async () => {
  try { initialVersion.value = (await api.updates()).currentVersion } catch { initialVersion.value = '' }
})
const sourceSaving = ref(false)
const actionBlocked = computed(() => blocked.value || sourceSaving.value)
const dialog = ref<'apply' | 'rollback' | null>(null)
const confirmation = ref('')
const target = ref<{ version: string; digest: string } | null>(null)
const expected = computed(() => dialog.value === 'apply' ? `更新 ${target.value?.version ?? ''}` : '回退转发程序')
const label = computed(() => onlinePhaseLabel(status.value))
const tagType = computed(() => {
  if (status.value?.state === 'succeeded') return 'success'
  if (status.value?.state === 'failed' || status.value?.state === 'manual_recovery') return 'danger'
  if (status.value?.state === 'rolled_back') return 'warning'
  return 'info'
})

/** 打开确认对话框并冻结已检测目标，不在主题变化时重建。@param action 更新或回退。@returns 无。 */
function openConfirmation(action: 'apply' | 'rollback'): void {
  if (actionBlocked.value) return
  if (action === 'apply' && (!status.value?.canApply || !status.value.latest)) return
  if (action === 'rollback' && !status.value?.canRollback) return
  target.value = status.value?.latest ? { ...status.value.latest } : null
  confirmation.value = ''
  dialog.value = action
}

/** 仅在文字匹配后提交一次操作，不因断联重试。@returns 提交结束。 */
async function confirm(): Promise<void> {
  if (actionBlocked.value || confirmation.value !== expected.value) return
  const action = dialog.value
  const selected = target.value
  const text = confirmation.value
  if (action === 'apply' && (!selected || !status.value?.canApply)) return
  if (action === 'rollback' && !status.value?.canRollback) return
  dialog.value = null
  if (action === 'apply' && selected) await submit(() => api.applyOnlineUpdate(selected.version, selected.digest, text))
  else if (action === 'rollback') await submit(() => api.rollbackOnlineUpdate(text))
}
/** 来源已持久化后清除旧确认与版本缓存，读取新的宿主状态。@returns 状态刷新结束。 */
async function sourceSaved(): Promise<void> {
  dialog.value = null
  target.value = null
  confirmation.value = ''
  invalidateSource()
  await refresh()
}
</script>

<template>
  <el-popover v-model:visible="opened" trigger="click" placement="bottom-end" :width="380" :persistent="true">
    <template #reference>
      <el-button text size="small" aria-label="程序更新与仓库设置">
        {{ status?.currentVersion || initialVersion ? `v${status?.currentVersion || initialVersion}` : '程序更新' }}
        <span v-if="status?.canApply" class="version-notice">有更新</span>
      </el-button>
    </template>
    <div class="update-popover">
    <div class="update-heading"><strong>程序更新</strong><el-tag :type="tagType" size="small" effect="plain">{{ label }}</el-tag></div>
    <el-alert v-if="error" :title="error" type="warning" :closable="false" show-icon />
    <dl class="version-details">
      <div><dt>当前版本</dt><dd>{{ status?.currentVersion || initialVersion || '—' }}</dd></div>
      <div><dt>最新版本</dt><dd>{{ status?.latest?.version || '尚未检测' }}</dd></div>
    </dl>
    <p class="field-help" role="status">{{ error ? '连接恢复后请刷新状态。' : status ? safeMessage(status.message) : '打开后读取更新状态。' }}</p>
    <div class="online-actions">
      <el-button size="small" :loading="busy" :disabled="actionBlocked || !status?.enabled" @click="submit(api.checkOnlineUpdate)">检测更新</el-button>
      <el-button v-if="status?.canApply" size="small" type="primary" :disabled="actionBlocked" @click="openConfirmation('apply')">下载并更新</el-button>
      <el-button v-if="status?.canRollback" size="small" :disabled="actionBlocked" @click="openConfirmation('rollback')">回退</el-button>
      <el-button link size="small" :loading="loading" :disabled="busy" @click="refresh">刷新状态</el-button>
    </div>
    <p class="field-help">更新会短暂中断转发；健康检查失败时尝试回退。Redis 不受影响。</p>
    <OnlineUpdateSource :active="opened" :blocked="blocked || !status?.enabled || status?.state === 'manual_recovery'" :on-saved="sourceSaved" @saving="sourceSaving = $event" />
    <el-button link size="small" @click="offlineOpen = true; opened = false">离线签名包暂存</el-button>
    </div>
  </el-popover>
  <el-dialog v-model="offlineOpen" title="离线签名包暂存" width="760px" :close-on-click-modal="false">
    <UpdatesPage :active="offlineOpen" />
  </el-dialog>
  <el-dialog :model-value="dialog !== null" :title="dialog === 'apply' ? '确认下载并更新' : '确认回退转发程序'" width="520px" :close-on-click-modal="false" @update:model-value="(value: boolean) => { if (!value) dialog = null }">
    <p>{{ dialog === 'apply' ? `将下载 ${target?.version}，下载完成后切换转发程序。` : '将切换至上一可用的转发程序版本。' }}</p>
    <el-alert title="切换会中断网页与转发连接、清空内存探测历史，并需要重新登录。Redis 独立运行，数据不受影响。" type="warning" :closable="false" show-icon />
    <p class="field-help">断联不代表失败。恢复后重新登录查看最终状态，不要重复提交。</p>
    <el-form label-position="top" class="online-confirm-form" @submit.prevent="confirm">
      <el-form-item :label="`请输入「${expected}」确认`"><el-input v-model="confirmation" autocomplete="off" :placeholder="expected" /></el-form-item>
    </el-form>
    <template #footer><el-button @click="dialog = null">取消</el-button><el-button type="primary" :disabled="actionBlocked || confirmation !== expected" @click="confirm">{{ dialog === 'apply' ? '下载并更新' : '确认回退' }}</el-button></template>
  </el-dialog>
</template>

<style scoped>
.update-popover { max-height: calc(100vh - 140px); overflow-y: auto; }
.update-heading, .version-details > div { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.version-details { margin: 16px 0; font-size: 13px; }
.version-details > div + div { margin-top: 10px; }
.version-details dt { color: var(--el-text-color-secondary); }
.version-details dd { margin: 0; overflow-wrap: anywhere; }
.version-notice { margin-left: 6px; color: var(--el-color-primary); }
.online-actions { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.online-actions .el-button + .el-button { margin-left: 0; }
.online-confirm-form { margin-top: 18px; }
</style>
