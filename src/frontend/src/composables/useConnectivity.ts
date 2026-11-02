import { onMounted, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import type { ConnectivitySample, MappingRecord, TestResult } from '../types'

/** 读取后端历史，不在浏览器重复自动探测；隐藏页面仅暂停拉取。
 * @param options 当前页面是否可见及配置变更版本。
 * @returns 历史、服务端校准时钟、加载错误和手动测试方法。
 */
export function useConnectivity(options: { active: () => boolean; revision: () => string }) {
  const probing = ref<string | null>(null)
  const histories = ref<Record<string, ConnectivitySample[]>>({})
  const now = ref(Date.now())
  const error = ref('')
  const loading = ref(true)
  let disposed = false
  let timer: number | undefined
  let inFlight: Promise<void> | undefined
  let clockOffset = 0
  let lastFetch = 0
  let revision = 0

  /** 拉取真实历史，保留失败前的记录但明确提示失败。
   * @returns 请求完成后的 Promise；不会将接口失败标成目标阻塞。
   */
  async function refresh(): Promise<void> {
    if (disposed) return
    if (inFlight) { await inFlight; return }
    const currentRevision = revision
    const operation = (async () => {
      try {
        const result = await api.connectivity()
        if (disposed || currentRevision !== revision) return
        clockOffset = result.serverTime - Date.now()
        now.value = result.serverTime
        histories.value = result.histories
        error.value = ''
      } catch (failure) {
        if (!disposed && currentRevision === revision) error.value = errorMessage(failure)
      } finally {
        if (!disposed) loading.value = false
        lastFetch = Date.now()
      }
    })()
    inFlight = operation
    await operation
    if (inFlight === operation) inFlight = undefined
  }

  /** 测试当前映射后重新读取服务器历史。
   * @param record 目标映射。
   * @returns 测试结果；已忙或已卸载时返回 undefined，接口错误交由调用方展示。
   */
  async function probe(record: MappingRecord): Promise<TestResult | undefined> {
    if (disposed || probing.value) return undefined
    probing.value = record.id
    try {
      const result = await api.testMapping(record.id)
      if (inFlight) await inFlight
      await refresh()
      return disposed ? undefined : result
    } finally { if (!disposed) probing.value = null }
  }

  /** 更新时钟并每5秒读取历史；自动探测由后端独立执行。@returns 无。 */
  function schedule(): void {
    timer = window.setTimeout(async () => {
      now.value = Date.now() + clockOffset
      if (options.active() && !document.hidden && Date.now() - lastFetch >= 5000) await refresh()
      if (!disposed) schedule()
    }, 1000)
  }

  watch(options.revision, () => {
    revision += 1
    // 配置更新时立即移除旧快照，避免目标已变但短暂显示旧历史。
    histories.value = {}
    lastFetch = 0
  })
  onMounted(() => { if (options.active()) void refresh(); schedule() })
  onUnmounted(() => { disposed = true; window.clearTimeout(timer) })
  return { histories, now, error, loading, probing, probe, refresh }
}
