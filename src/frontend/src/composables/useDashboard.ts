import { onUnmounted, ref } from 'vue'
import { api, errorMessage } from '../api'
import type { MappingRecord, ProxyRecord, ServiceStatus } from '../types'

/** 加载只读运行快照，使用代次标记防止注销后异步响应回写。
 * @returns 快照、加载状态与轮询生命周期方法。
 */
export function useDashboard() {
  const proxies = ref<ProxyRecord[]>([])
  const mappings = ref<MappingRecord[]>([])
  const status = ref<ServiceStatus | null>(null)
  const loading = ref(false)
  const error = ref('')
  const updatedAt = ref('')
  let timer: number | undefined
  let generation = 0
  let enabled = false
  let inFlight: Promise<void> | undefined

  /** 更新全部只读快照，不触碰弹窗草稿。
   * @param foreground 是否显示全局加载态。
   * @returns 刷新结束后的 Promise。
   */
  async function refresh(foreground = false): Promise<void> {
    if (!enabled) return
    if (inFlight) await inFlight
    if (!enabled) return
    const current = generation
    if (foreground) loading.value = true
    const operation = (async () => {
      try {
        const [nextProxies, nextMappings, nextStatus] = await Promise.all([api.proxies(), api.mappings(), api.status()])
        if (current !== generation) return
        proxies.value = nextProxies
        mappings.value = nextMappings
        status.value = nextStatus
        error.value = ''
        updatedAt.value = new Date().toLocaleTimeString('zh-CN')
      } catch (failure) {
        if (current === generation) error.value = errorMessage(failure)
      } finally {
        if (current === generation) loading.value = false
      }
    })()
    inFlight = operation
    await operation
    if (inFlight === operation) inFlight = undefined
  }

  /** 安排下一次轮询；页面隐藏时不发请求。
   * @returns 无返回值。
   */
  function schedule(): void {
    timer = window.setTimeout(async () => {
      if (!enabled) return
      if (!document.hidden) await refresh()
      if (enabled) schedule()
    }, 5000)
  }

  /** 开始读取快照并轮询。
   * @returns 无返回值。
   */
  function start(): void {
    if (enabled) return
    enabled = true
    void refresh(true)
    schedule()
  }

  /** 停止轮询并丢弃旧会话快照。
   * @returns 无返回值。
   */
  function stop(): void {
    enabled = false
    generation += 1
    window.clearTimeout(timer)
    loading.value = false
    proxies.value = []
    mappings.value = []
    status.value = null
    updatedAt.value = ''
    error.value = ''
  }
  onUnmounted(stop)
  return { proxies, mappings, status, loading, error, updatedAt, refresh, start, stop }
}
