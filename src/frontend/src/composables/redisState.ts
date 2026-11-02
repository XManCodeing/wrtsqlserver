import type { RedisStatus } from '../types'

/** 格式化可缺失字节数。@param value 字节数或未知。@returns 可读容量。 */
export function redisBytes(value: number | null | undefined): string {
  if (value == null || !Number.isFinite(value) || value < 0) return '—'
  if (value < 1024) return `${value} B`
  if (value < 1024 ** 2) return `${(value / 1024).toFixed(1)} KiB`
  return `${(value / 1024 ** 2).toFixed(1)} MiB`
}

/** 格式化运行秒数。@param value 运行秒数或未知。@returns 天时分。 */
export function redisUptime(value: number | null | undefined): string {
  if (value == null || !Number.isFinite(value) || value < 0) return '—'
  return `${Math.floor(value / 86400)} 天 ${Math.floor(value % 86400 / 3600)} 小时 ${Math.floor(value % 3600 / 60)} 分钟`
}

/** 将真实服务状态转换为标签，未知或请求失败不得展示在线。
 * @param status 后端状态快照。
 * @returns 标签及语义颜色。
 */
export function redisState(status: RedisStatus | null) {
  if (!status) return { label: '状态未知', type: 'info' as const }
  const labels = { unconfigured: '未配置', online: '在线', unreachable: '无法连接', error: '读取异常' }
  return { label: labels[status.state], type: status.state === 'online' ? 'success' as const : status.state === 'unconfigured' ? 'info' as const : 'danger' as const }
}
