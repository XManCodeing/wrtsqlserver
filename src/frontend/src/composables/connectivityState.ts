import type { ConnectivitySample } from '../types'

export const PROBE_INTERVAL_MS = 60_000
export const HISTORY_MINUTES = 60

export interface ConnectivityBar {
  time: number
  state: 'success' | 'failure' | 'unknown'
  sample?: ConnectivitySample
}

/** 将真实采样归入最近60个分钟格；缺失数据保持未知，同分钟取最新结果。
 * @param samples 后端探测记录。
 * @param now 服务端校准的当前毫秒时间。
 * @returns 从旧到新排列的固定长度时间格。
 */
export function connectivityBars(samples: ConnectivitySample[], now: number): ConnectivityBar[] {
  const lastMinute = Math.floor(now / PROBE_INTERVAL_MS) * PROBE_INTERVAL_MS
  const firstMinute = lastMinute - (HISTORY_MINUTES - 1) * PROBE_INTERVAL_MS
  const minutes = new Map<number, ConnectivitySample>()
  for (const sample of samples) {
    if (!Number.isFinite(sample.checkedAt) || sample.checkedAt > now || sample.checkedAt < firstMinute) continue
    const minute = Math.floor(sample.checkedAt / PROBE_INTERVAL_MS) * PROBE_INTERVAL_MS
    const previous = minutes.get(minute)
    if (!previous || previous.checkedAt <= sample.checkedAt) minutes.set(minute, sample)
  }
  return Array.from({ length: HISTORY_MINUTES }, (_, index) => {
    const time = firstMinute + index * PROBE_INTERVAL_MS
    const sample = minutes.get(time)
    return { time, sample, state: sample ? sample.ok ? 'success' : 'failure' : 'unknown' }
  })
}
