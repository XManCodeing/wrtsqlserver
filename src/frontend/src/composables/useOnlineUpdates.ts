import { computed, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import { onlineDisconnectedMessage, onlineOperationActive, shouldPollOnline, type OnlineUpdateStatus } from './onlineUpdateState'

/** 管理有界轮询和一次性操作提交，不自动重试 POST。@param active 页面激活读取器。@returns 状态及显式操作。 */
export function useOnlineUpdates(active: () => boolean) {
  const status = ref<OnlineUpdateStatus | null>(null)
  const loading = ref(false)
  const busy = ref(false)
  const error = ref('')
  const uncertain = ref(false)
  let disposed = false
  let revision = 0
  let timer: ReturnType<typeof setInterval> | undefined
  const blocked = computed(() => loading.value || busy.value || !!error.value || uncertain.value || onlineOperationActive(status.value))

  /** 获取持久状态；断联保留旧状态而非推断更新失败。@returns 请求结束。 */
  async function refresh(): Promise<void> {
    if (disposed || loading.value || busy.value || !shouldPollOnline(active(), document.visibilityState)) return
    loading.value = true
    const requestedRevision = revision
    try {
      const result = await api.onlineUpdates()
      if (!disposed && requestedRevision === revision) { status.value = result; error.value = ''; uncertain.value = false }
    } catch {
      if (!disposed && requestedRevision === revision) error.value = onlineDisconnectedMessage(status.value)
    } finally {
      if (!disposed) {
        loading.value = false
        if (requestedRevision !== revision) void refresh()
      }
    }
  }

  /** 提交单次显式操作；错误后的结果必须由 GET 核实。@param operation 单次 API 调用。@returns 请求结束。 */
  async function submit(operation: () => Promise<OnlineUpdateStatus>): Promise<void> {
    if (blocked.value || !status.value?.enabled) return
    const publishRevision = revision
    const canPublish = () => !disposed && publishRevision === revision && active()
    busy.value = true
    try {
      const result = await operation()
      if (canPublish()) { status.value = result; error.value = '' }
    } catch (failure) {
      if (canPublish()) { uncertain.value = true; error.value = errorMessage(failure) }
    } finally { if (!disposed) { busy.value = false; if (publishRevision !== revision) void refresh() } }
  }

  /** 来源变更立即清除旧版本能力，下一次 GET 才恢复按钮。@returns 无。 */
  function invalidateSource(): void {
    revision++
    if (status.value) status.value = { ...status.value, latest: null, prepared: null, downloadPercent: null, canApply: false }
    uncertain.value = true
  }

  /** 按导航与文档可见性启停轮询，刷新互斥避免慢请求重叠。@returns 无。 */
  function synchronize(): void {
    revision++
    if (timer) clearInterval(timer)
    timer = undefined
    if (!disposed && shouldPollOnline(active(), document.visibilityState)) {
      void refresh()
      timer = setInterval(() => { void refresh() }, 1500)
    }
  }
  document.addEventListener('visibilitychange', synchronize)
  watch(active, synchronize, { immediate: true })
  onUnmounted(() => {
    disposed = true
    if (timer) clearInterval(timer)
    document.removeEventListener('visibilitychange', synchronize)
  })
  return { status, loading, busy, error, blocked, refresh, submit, invalidateSource }
}
