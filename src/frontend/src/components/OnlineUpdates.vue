<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { api, safeMessage } from '../api'
import { canCheckOnOpen, onlineDownloadPercent, onlineOperationActive, onlinePhaseLabel, onlineTransferSize } from '../composables/onlineUpdateState'
import { useOnlineUpdates } from '../composables/useOnlineUpdates'
import OnlineUpdateSource from './OnlineUpdateSource.vue'
import UpdatesPage from './UpdatesPage.vue'

const opened = ref(false)
const offlineOpen = ref(false)
const { status, loading, busy, error, blocked, refresh, submit, invalidateSource } = useOnlineUpdates(() => opened.value)
const initialVersion = ref('')
const sourceSaving = ref(false)
const checkOnOpen = ref(false)
const actionBlocked = computed(() => blocked.value || sourceSaving.value)
const label = computed(() => onlinePhaseLabel(status.value))
const prepared = computed(() => status.value?.prepared ?? null)
const downloading = computed(() => status.value?.state === 'downloading')
const applying = computed(() => status.value?.state === 'applying')
const percent = computed(() => onlineDownloadPercent(status.value))
const canDownload = computed(() => status.value?.stagedUpdates === true && status.value.canApply && !prepared.value)
const tagType = computed(() => {
  if (onlineOperationActive(status.value)) return 'info'
  if (status.value?.state === 'failed' || status.value?.state === 'manual_recovery') return 'danger'
  if (prepared.value || status.value?.state === 'succeeded') return 'success'
  if (status.value?.state === 'rolled_back') return 'warning'
  return 'info'
})

// 页面初始化只读取本地版本；用户打开更新弹窗后才访问更新来源。
onMounted(async () => {
  try { initialVersion.value = (await api.updates()).currentVersion } catch { initialVersion.value = '' }
})

/** 打开版本弹窗并安排一次检测；重开时先读取宿主实际状态。@returns 无。 */
function openUpdates(): void {
  checkOnOpen.value = true
  opened.value = true
}

// 先恢复下载/重启任务再决定是否检测；轮询不会反复触发检测。
watch([status, loading, busy, error, opened], () => {
  if (!opened.value) { checkOnOpen.value = false; return }
  if (!checkOnOpen.value || loading.value || busy.value || error.value || !status.value) return
  checkOnOpen.value = false
  if (canCheckOnOpen(status.value)) void submit(api.checkOnlineUpdate)
})

/** 下载当前已检测的固定镜像，不停止服务。@returns 单次提交结束。 */
async function download(): Promise<void> {
  const release = status.value?.latest
  if (actionBlocked.value || !canDownload.value || !release) return
  await submit(() => api.downloadOnlineUpdate(release.version, release.digest))
}

/** 点击重启即明确授权激活已准备版本，不要求输入确认文字。@returns 单次提交结束。 */
async function restart(): Promise<void> {
  const release = prepared.value
  if (actionBlocked.value || !release || status.value?.stagedUpdates !== true) return
  await submit(() => api.activateOnlineUpdate(release.version, release.digest))
}

/** 按钮明确回退至保留版本，兼容原有协议而不增加手动输入。@returns 单次提交结束。 */
async function rollback(): Promise<void> {
  if (actionBlocked.value || !status.value?.canRollback) return
  await submit(() => api.rollbackOnlineUpdate('回退转发程序'))
}

/** 来源保存后清除旧下载身份并重新检测，不沿用旧确认目标。@returns 刷新结束。 */
async function sourceSaved(): Promise<void> {
  invalidateSource()
  checkOnOpen.value = true
  await refresh()
}
</script>

<template>
  <el-button text size="small" aria-label="程序更新与仓库设置" @click="openUpdates">
    {{ status?.currentVersion || initialVersion ? `v${status?.currentVersion || initialVersion}` : '程序更新' }}
    <span v-if="status?.canApply || prepared" class="version-notice">{{ prepared ? '待重启' : '有更新' }}</span>
  </el-button>
  <el-dialog v-model="opened" title="版本更新" width="560px" :close-on-click-modal="false">
    <div class="update-body">
      <div class="update-heading">
        <strong>{{ prepared ? '新版本已准备就绪' : '程序更新' }}</strong>
        <el-tag :type="tagType" size="small" effect="plain">{{ label }}</el-tag>
      </div>
      <el-alert v-if="error" :title="error" type="warning" :closable="false" show-icon />
      <dl class="version-details">
        <div><dt>当前版本</dt><dd>{{ status?.currentVersion || initialVersion || '—' }}</dd></div>
        <div><dt>最新版本</dt><dd>{{ status?.latest?.version || (status?.state === 'checking' ? '正在检测…' : '尚未检测') }}</dd></div>
        <div v-if="prepared"><dt>已下载版本</dt><dd>{{ prepared.version }}</dd></div>
      </dl>
      <div v-if="downloading || applying" class="update-progress" role="status" aria-live="polite">
        <div class="update-heading"><span>{{ applying ? '正在重启并检查服务' : status?.phase === 'verify' ? '正在校验镜像' : '正在下载更新' }}</span><span>{{ downloading && percent !== null ? `${percent}%` : '处理中' }}</span></div>
        <el-progress :percentage="downloading && percent !== null ? percent : 100" :indeterminate="applying || percent === null" :show-text="false" />
        <p v-if="downloading && (status?.downloadedBytes ?? 0) > 0" class="field-help">已传输 {{ onlineTransferSize(status?.downloadedBytes) }}<template v-if="(status?.totalBytes ?? 0) > 0"> / {{ onlineTransferSize(status?.totalBytes) }}</template></p>
        <p class="field-help">{{ applying ? '网页和转发会短暂断开；恢复后可能需要重新登录，请勿重复提交。' : '下载不影响当前服务，关闭弹窗后仍会继续。' }}</p>
      </div>
      <el-alert v-else-if="prepared" title="下载完成，点击「重启并更新」后生效，也可以稍后操作。" type="success" :closable="false" show-icon />
      <p v-if="status?.message" class="field-help" role="status">{{ safeMessage(status.message) }}</p>
      <el-alert v-if="status?.enabled && !status.stagedUpdates" title="宿主更新器需升级后才能分步下载和重启，当前不会自动切换服务。" type="info" :closable="false" />
      <p v-if="prepared" class="field-help">重启会短暂中断网页与转发连接，Redis 不受影响。健康检查失败时尝试回退；原有回退窗口将被本次更新替换。</p>
      <p v-else-if="status?.canRollback && !onlineOperationActive(status)" class="field-help">回退会恢复上次事务保存的配置并中断转发；事务之后的配置更改将不再生效，新数据保留在宿主备份中。</p>
      <OnlineUpdateSource :active="opened" :blocked="blocked || !status?.enabled || status?.state === 'manual_recovery'" :on-saved="sourceSaved" @saving="sourceSaving = $event" />
      <el-button link size="small" @click="offlineOpen = true; opened = false">离线签名包暂存</el-button>
    </div>
    <template #footer>
      <div class="update-footer">
        <el-button v-if="status?.canRollback && !prepared && !onlineOperationActive(status)" :disabled="actionBlocked" @click="rollback">回退上次变更</el-button>
        <el-button v-if="error" :loading="loading" :disabled="busy" @click="refresh">刷新状态</el-button>
        <el-button v-else-if="!prepared && !onlineOperationActive(status)" :disabled="actionBlocked || !status?.enabled || status?.state === 'manual_recovery'" @click="submit(api.checkOnlineUpdate)">重新检测</el-button>
        <el-button @click="opened = false">{{ prepared ? '稍后重启' : '关闭' }}</el-button>
        <el-button v-if="prepared" type="primary" :disabled="actionBlocked || !status?.stagedUpdates" @click="restart">重启并更新</el-button>
        <el-button v-else-if="canDownload || downloading" type="primary" :loading="downloading" :disabled="actionBlocked" @click="download">{{ downloading ? '正在下载' : '下载更新' }}</el-button>
      </div>
    </template>
  </el-dialog>
  <el-dialog v-model="offlineOpen" title="离线签名包暂存" width="760px" :close-on-click-modal="false">
    <UpdatesPage :active="offlineOpen" />
  </el-dialog>
</template>

<style scoped>
.update-body { display: flex; flex-direction: column; gap: 16px; }
.update-heading, .version-details > div { display: flex; align-items: center; justify-content: space-between; gap: 12px; }
.version-details { margin: 0; font-size: 13px; }
.version-details > div + div { margin-top: 10px; }
.version-details dt { color: var(--el-text-color-secondary); }
.version-details dd { margin: 0; overflow-wrap: anywhere; }
.version-notice { margin-left: 6px; color: var(--el-color-primary); }
.update-progress { display: flex; flex-direction: column; gap: 12px; }
.update-body .field-help { margin: 0; }
.update-footer { display: flex; flex-wrap: wrap; justify-content: flex-end; gap: 8px; }
.update-footer .el-button + .el-button { margin-left: 0; }
</style>
