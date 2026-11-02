import { onMounted, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import type { RedisStatus } from '../types'

/** 只在激活且可见时轮询Redis状态，不让请求重叠或注销后回写。
 * @param active 当前页面可见状态 getter。
 * @returns 状态、错误、加载与手动刷新方法。
 */
export function useRedisStatus(active: () => boolean) {
  const status = ref<RedisStatus | null>(null)
  const loading = ref(false)
  const error = ref('')
  let disposed = false
  let timer: number | undefined

  /** 拉取快照，失败清除旧数据避免误显示在线。@returns 请求完成。 */
  async function refresh(): Promise<void> {
    if (disposed || loading.value) return
    loading.value = true
    try {
      const result = await api.redisStatus()
      if (!disposed) { status.value = result; error.value = '' }
    } catch (failure) {
      if (!disposed) { status.value = null; error.value = errorMessage(failure) }
    } finally { if (!disposed) loading.value = false }
  }

  /** 每5秒检查页面可见性并读取快照。@returns 无。 */
  function schedule(): void {
    timer = window.setTimeout(async () => {
      if (active() && !document.hidden) await refresh()
      if (!disposed) schedule()
    }, 5000)
  }

  /** 返回可见页面时立即刷新。@returns 无。 */
  function onVisible(): void { if (!document.hidden && active()) void refresh() }
  watch(active, visible => { if (visible) void refresh() }, { immediate: true })
  onMounted(() => { schedule(); document.addEventListener('visibilitychange', onVisible) })
  onUnmounted(() => { disposed = true; window.clearTimeout(timer); document.removeEventListener('visibilitychange', onVisible) })
  return { status, loading, error, refresh }
}
