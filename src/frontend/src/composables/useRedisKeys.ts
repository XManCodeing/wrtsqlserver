import { computed, onUnmounted, ref, watch } from 'vue'
import { api, errorMessage } from '../api'
import type { RedisKey, RedisValue } from '../types'

/** 管理单库游标扫描及当前页值预览，切库后丢弃旧响应。
 * @param database 当前库名。@param enabled 是否允许读取。@returns 列表状态及操作。
 */
export function useRedisKeys(database: () => string, enabled: () => boolean) {
  const keys = ref<RedisKey[]>([])
  const values = ref<Record<string, RedisValue>>(Object.create(null))
  const failures = ref<Record<string, string>>(Object.create(null))
  const pattern = ref('*')
  const page = ref(1)
  const nextCursor = ref('0')
  const scanned = ref(false)
  const loading = ref(false)
  const previewLoading = ref(false)
  const error = ref('')
  const rows = computed(() => keys.value.slice((page.value - 1) * 10, page.value * 10))
  let generation = 0
  let previewGeneration = 0
  let appliedPattern = '*'

  /** 失效全部在途响应并清除旧库数据。@returns 无。 */
  function reset(): void {
    generation++; previewGeneration++
    keys.value = []; values.value = Object.create(null); failures.value = Object.create(null)
    page.value = 1; nextCursor.value = '0'; scanned.value = false
    loading.value = false; previewLoading.value = false; error.value = ''
  }

  /** 扫描下一批或重新筛选；SCAN数量为提示，不丢弃返回键。@param restart 是否从头扫描。@returns 完成状态。 */
  async function scan(restart = true): Promise<void> {
    if (!enabled() || loading.value) return
    if (!restart && (!scanned.value || nextCursor.value === '0')) return
    if (new TextEncoder().encode(pattern.value || '*').length > 256) {
      error.value = '筛选条件不能超过 256 字节。'; return
    }
    const cursor = restart ? '0' : nextCursor.value
    if (restart) {
      appliedPattern = pattern.value || '*'; nextCursor.value = '0'; scanned.value = false
    }
    const requestGeneration = ++generation
    previewGeneration++
    loading.value = true; previewLoading.value = false; error.value = ''
    keys.value = []; values.value = Object.create(null); failures.value = Object.create(null); page.value = 1
    try {
      const result = await api.redisKeys(database(), cursor, appliedPattern)
      if (requestGeneration !== generation) return
      // SCAN允许重复键；当前批次去重，不声称快照一致性。
      keys.value = [...new Map(result.keys.map(key => [key.keyToken, key])).values()]
      nextCursor.value = result.nextCursor; scanned.value = true
    } catch (failure) {
      if (requestGeneration === generation) error.value = errorMessage(failure)
    } finally { if (requestGeneration === generation) loading.value = false }
  }

  /** 当前页最多三个并发请求，直接在列表显示值；失败保留逐键诊断。@returns 预览加载完成。 */
  async function loadPreviews(): Promise<void> {
    const requestGeneration = ++previewGeneration
    if (!enabled() || !rows.value.length) { previewLoading.value = false; return }
    const selectedDatabase = database()
    const pending = rows.value.filter(row => !Object.hasOwn(values.value, row.keyToken))
    previewLoading.value = pending.length > 0
    let position = 0
    /** 消费有限队列，换页后停止发送新请求。@returns 当前工作队列结束。 */
    async function worker(): Promise<void> {
      while (position < pending.length && requestGeneration === previewGeneration) {
        const row = pending[position++]!
        try {
          const result = await api.redisValue(selectedDatabase, row.keyToken)
          if (requestGeneration === previewGeneration) {
            values.value[row.keyToken] = result; delete failures.value[row.keyToken]
          }
        } catch (failure) {
          if (requestGeneration === previewGeneration) failures.value[row.keyToken] = errorMessage(failure)
        }
      }
    }
    await Promise.all(Array.from({ length: Math.min(3, pending.length) }, worker))
    if (requestGeneration === previewGeneration) previewLoading.value = false
  }

  watch(rows, () => { void loadPreviews() })
  watch([database, enabled], () => { reset(); if (enabled()) void scan() }, { immediate: true })
  onUnmounted(reset)
  return { keys, rows, values, failures, pattern, page, nextCursor, scanned, loading, previewLoading, error, scan, reset, loadPreviews }
}
